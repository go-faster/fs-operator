/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fscluster

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/fsconfig"
)

// MinSecretKeyLength is fs's minimum S3 secret-key length. A shorter secret is
// refused rather than rendered: the FSAccessKey controller reports it on the
// key's status (ReasonWeakSecretKey), and the render skips it.
const MinSecretKeyLength = 16

// EndpointKey holds the cluster's S3 endpoint in a generated credential Secret,
// alongside AccessKeyKey and SecretKeyKey, so the Secret is enough to configure
// a client.
const EndpointKey = "endpoint"

// AccessKeySecretName is the Secret an FSAccessKey's generated credential is
// written to: the user-named one, or <name>-credentials by default.
func AccessKeySecretName(key *fsv1alpha1.FSAccessKey) string {
	if key.Spec.SecretName != "" {
		return key.Spec.SecretName
	}

	return key.Name + "-credentials"
}

// AccessKeySecretSource is the Secret the operator reads a credential from: the
// user-managed existingSecretRef when imported, otherwise the generated Secret
// the operator owns.
func AccessKeySecretSource(key *fsv1alpha1.FSAccessKey) string {
	if ref := key.Spec.ExistingSecretRef; ref != nil && ref.Name != "" {
		return ref.Name
	}

	return AccessKeySecretName(key)
}

// GrantsToConfig converts an FSAccessKey's API grants to the config form fs
// renders (the shapes match one-to-one: a bucket glob and a permission level).
func GrantsToConfig(grants []fsv1alpha1.GrantSpec) []fsconfig.Grant {
	out := make([]fsconfig.Grant, 0, len(grants))
	for _, g := range grants {
		out = append(out, fsconfig.Grant{Bucket: g.Bucket, Permission: g.Permission})
	}

	return out
}

// collectAccessKeys gathers every FSAccessKey of the cluster and resolves each
// to a config Key from its Secret. A key whose Secret is missing, incomplete or
// whose secret half is too short is skipped: the FSAccessKey controller reports
// that on the key's own status, and one bad credential must not block the whole
// cluster's config. The result is the cluster-wide declarative credential set
// rendered into every node (SPEC §7); authConfig sorts it for a stable render.
func (r *Reconciler) collectAccessKeys(ctx context.Context, cluster *fsv1alpha1.FSCluster) ([]fsconfig.Key, error) {
	var list fsv1alpha1.FSAccessKeyList
	if err := r.List(ctx, &list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}

	keys := make([]fsconfig.Key, 0, len(list.Items))

	for i := range list.Items {
		ak := &list.Items[i]

		if ak.Spec.ClusterRef.Name != cluster.Name {
			continue
		}

		if !ak.DeletionTimestamp.IsZero() {
			// Being deleted: drop it from the rendered set so the next reload
			// revokes it.
			continue
		}

		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: AccessKeySecretSource(ak)}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				// Not minted / not present yet; the FSAccessKey controller will
				// mint it and re-enqueue this cluster.
				continue
			}

			return nil, err
		}

		access := string(secret.Data[AccessKeyKey])
		secretKey := string(secret.Data[SecretKeyKey])

		if access == "" || len(secretKey) < MinSecretKeyLength {
			continue
		}

		keys = append(keys, fsconfig.Key{
			AccessKey: access,
			SecretKey: secretKey,
			Grants:    GrantsToConfig(ak.Spec.Grants),
		})
	}

	return keys, nil
}
