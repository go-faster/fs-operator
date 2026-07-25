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
	"fmt"
	"slices"

	"github.com/go-faster/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/etcdstore"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// rootCredentialState is what a pass could establish about the cluster's root
// credential. Unknown is the normal answer while nothing is serving yet, and it
// is never reported as a problem: the check is only meaningful once a node can
// answer for the cluster-wide key store.
type rootCredentialState int

const (
	rootCredentialUnknown rootCredentialState = iota
	rootCredentialRegistered
	rootCredentialMissing
)

// reconcileRootCredential checks that the root credential the operator holds is
// one the cluster will actually accept.
//
// Since fs §6.8 the credential store is cluster-wide in etcd, sealed by the
// cluster secret, and it is seeded from a node's config exactly once — while
// the key namespace is empty. A cluster that starts on a prefix which already
// holds keys therefore adopts them and ignores its own root credential.
//
// The way to get there is ordinary: delete an FSCluster without
// etcd.cleanupOnDelete and apply the same manifest again. The prefix defaults
// to the namespace and name, so the new cluster lands on the old one's keys,
// whose secrets were sealed with a cluster secret that has since been
// regenerated. fs skips what it cannot unseal (one warn line per node), the
// pods pass their health probes, and the cluster looks healthy while every S3
// client the operator owns is locked out — which is what every FSBucket of that
// cluster then reports, one indirection away from the cause.
//
// So the operator asks the question directly: is our access key in the
// cluster's key store? It does not answer it by writing the credential in.
// Registering a key into a prefix whose contents the operator did not create
// would be repairing a cluster it cannot prove is its own, and the two cases
// that reach here — an adopted prefix and a root key someone deliberately
// removed — want opposite things. Reporting is unambiguous, and it names the
// one command that fixes it.
func (r *Reconciler) reconcileRootCredential(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	log := logf.FromContext(ctx)

	serving := servingNodes(p)
	if len(serving) == 0 {
		return pipeline.Continue()
	}

	access, err := r.rootAccessKey(ctx, p)
	if err != nil {
		// The secrets step has already refused a cluster whose root credential
		// is missing; anything else is transient.
		log.V(1).Info("Root credential unreadable; skipping the registration check", "error", err)

		return pipeline.Continue()
	}

	token, err := r.adminToken(ctx, p)
	if err != nil {
		return pipeline.Continue()
	}

	admin, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, serving[0].Name), token)
	if err != nil {
		return pipeline.Continue()
	}

	keys, err := admin.ListAccessKeys(ctx)
	if err != nil {
		// Unreachable says nothing about the key store.
		log.V(1).Info("Access key list unavailable; root credential unverified", "error", err)

		return pipeline.Continue()
	}

	if len(keys) == 0 {
		// An empty key store is a cluster that has not finished starting: a
		// node seeds its root credential and the etcd watch then delivers the
		// first snapshot, and between the two the list is legitimately empty.
		// The state this check is for never looks like this — an adopted prefix
		// is one that already holds someone else's keys.
		return pipeline.Continue()
	}

	registered := slices.ContainsFunc(keys, func(k fsclient.AccessKey) bool {
		return k.AccessKey == access
	})

	if registered {
		p.rootCredential = rootCredentialRegistered

		return pipeline.Continue()
	}

	p.rootCredential = rootCredentialMissing

	prefix := p.cluster.Spec.EtcdPrefix(p.cluster.Namespace, p.cluster.Name)

	r.Recorder.Eventf(p.object, corev1.EventTypeWarning, fsv1alpha1.ReasonRootCredentialUnregistered,
		"The cluster's key store does not hold the root access key %q; "+
			"etcd prefix %q holds credentials this cluster cannot unseal", access, prefix)

	return pipeline.Continue()
}

// rootAccessKey reads the access key of the cluster's root credential.
func (r *Reconciler) rootAccessKey(ctx context.Context, p *pass) (string, error) {
	var secret corev1.Secret

	name := RootCredentialsSource(p.cluster)
	if err := r.Get(ctx, types.NamespacedName{Namespace: p.cluster.Namespace, Name: name}, &secret); err != nil {
		return "", errors.Wrapf(err, "get root credentials secret %q", name)
	}

	access := string(secret.Data[AccessKeyKey])
	if access == "" {
		return "", errors.Errorf("root credentials secret %q has no %q", name, AccessKeyKey)
	}

	return access, nil
}

// summarizeRootCredential explains an unusable root credential, which is what
// makes the cluster unusable for S3 even though every pod is up.
func summarizeRootCredential(p *pass) (metav1.Condition, bool) {
	if p.rootCredential != rootCredentialMissing {
		return metav1.Condition{}, false
	}

	prefix := p.cluster.Spec.EtcdPrefix(p.cluster.Namespace, p.cluster.Name)

	return metav1.Condition{
		Type:   fsv1alpha1.ConditionReady,
		Status: metav1.ConditionFalse,
		Reason: fsv1alpha1.ReasonRootCredentialUnregistered,
		Message: fmt.Sprintf(
			"The cluster's root credential is not registered in its key store: "+
				"etcd prefix %q already held credentials when this cluster started, "+
				"and they were sealed with a different cluster secret. "+
				"Delete the stale keys (etcdctl del --prefix %s) and restart the nodes, "+
				"or set etcd.cleanupOnDelete so a deleted cluster takes its keys with it",
			prefix, etcdstore.KeyRange(prefix)),
		ObservedGeneration: p.object.Generation,
	}, true
}
