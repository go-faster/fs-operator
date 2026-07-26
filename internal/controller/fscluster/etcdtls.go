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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/go-faster/errors"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/etcdstore"
)

// etcdSecurity is what the cluster's referenced etcd Secrets contain.
//
// It exists because the renderer cannot look: rendering is a pure function of
// the spec, and whether a Secret carries a client certificate as well as a CA
// decides which file paths the config must name. Naming a file that is not
// mounted fails the node at startup, so this is read once, before the render.
type etcdSecurity struct {
	// clientCert reports whether the TLS Secret carries tls.crt and tls.key,
	// so the config should ask fs for mutual TLS.
	clientCert bool

	// material is the same trust and credentials in memory, for the
	// operator's own etcd client — the deletion finalizer reaches etcd
	// directly and cannot use a path mounted into someone else's pod.
	material etcdstore.Security
}

// secretProblem is a referenced Secret that is missing or unusable. It carries
// the condition reason so the reconcile path can refuse the spec with it,
// while the finalizer — which has no spec to refuse — can just report it.
type secretProblem struct {
	reason  fsv1alpha1.ConditionReason
	message string
}

func (p *secretProblem) Error() string { return p.message }

// resolveEtcdSecurity reads the etcd TLS and auth Secrets a cluster
// references, before anything is rendered from them.
//
// A referenced Secret that is missing or malformed refuses the spec rather
// than producing nodes pointed at an etcd they cannot authenticate to —
// which, with TLS misconfigured, means either a node that will not start or
// one that quietly connects in the clear.
func (r *Reconciler) resolveEtcdSecurity(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	security, clientCert, err := r.readEtcdSecurity(ctx, p.cluster)
	if err != nil {
		var problem *secretProblem
		if errors.As(err, &problem) {
			return r.refuse(p, problem.reason, problem.message)
		}

		return pipeline.Outcome{}, err
	}

	p.etcd = etcdSecurity{clientCert: clientCert, material: security}

	return pipeline.Continue()
}

// etcdSecurity reads the material for the operator's own etcd client.
func (r *Reconciler) etcdSecurity(
	ctx context.Context, cluster *fsv1alpha1.FSCluster,
) (etcdstore.Security, error) {
	security, _, err := r.readEtcdSecurity(ctx, cluster)

	return security, err
}

// readEtcdSecurity resolves both referenced Secrets, reporting a secretProblem
// for anything a user has to fix.
func (r *Reconciler) readEtcdSecurity(
	ctx context.Context, cluster *fsv1alpha1.FSCluster,
) (etcdstore.Security, bool, error) {
	var (
		security   etcdstore.Security
		clientCert bool
	)

	external := cluster.Spec.Etcd.External
	if external == nil {
		// The managed development etcd is plaintext in-cluster (SPEC §2).
		return security, false, nil
	}

	security.ServerName = external.TLS.ServerName
	security.InsecureSkipVerify = external.TLS.InsecureSkipVerify

	if name := external.TLS.SecretName; name != "" {
		secret, err := r.etcdSecret(ctx, cluster.Namespace, name)
		if err != nil {
			return security, false, err
		}

		ca := secret.Data[EtcdCAKey]
		if len(ca) == 0 {
			return security, false, &secretProblem{
				reason: fsv1alpha1.ReasonSecretInvalid,
				message: fmt.Sprintf("etcd TLS Secret %q has no %q",
					name, EtcdCAKey),
			}
		}

		cert, key := secret.Data[EtcdCertKey], secret.Data[EtcdKeyKey]

		// A client certificate is a pair, and fs refuses one without the
		// other. Catching it here names the Secret; letting it through would
		// surface as a node that will not start.
		if (len(cert) == 0) != (len(key) == 0) {
			return security, false, &secretProblem{
				reason: fsv1alpha1.ReasonSecretInvalid,
				message: fmt.Sprintf("etcd TLS Secret %q needs %q and %q together for mutual TLS",
					name, EtcdCertKey, EtcdKeyKey),
			}
		}

		security.CA, security.Cert, security.Key = ca, cert, key
		clientCert = len(cert) > 0
	}

	if ref := external.AuthSecretRef; ref != nil && ref.Name != "" {
		secret, err := r.etcdSecret(ctx, cluster.Namespace, ref.Name)
		if err != nil {
			return security, clientCert, err
		}

		for _, key := range []string{EtcdUsernameKey, EtcdPasswordKey} {
			if len(secret.Data[key]) == 0 {
				return security, clientCert, &secretProblem{
					reason: fsv1alpha1.ReasonSecretInvalid,
					message: fmt.Sprintf("etcd auth Secret %q has no %q",
						ref.Name, key),
				}
			}
		}

		security.Username = string(secret.Data[EtcdUsernameKey])
		security.Password = string(secret.Data[EtcdPasswordKey])
	}

	return security, clientCert, nil
}

// etcdSecret reads one referenced Secret.
func (r *Reconciler) etcdSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	var secret corev1.Secret

	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &secretProblem{
				reason:  fsv1alpha1.ReasonSecretNotFound,
				message: fmt.Sprintf("Secret %q not found", name),
			}
		}

		return nil, errors.Wrapf(err, "get secret %q", name)
	}

	return &secret, nil
}
