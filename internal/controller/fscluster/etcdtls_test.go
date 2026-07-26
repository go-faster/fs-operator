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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// etcdSecret creates a Secret in the cluster's namespace.
func etcdSecretFixture(t *testing.T, r *Reconciler, key types.NamespacedName, name string, data map[string][]byte) {
	t.Helper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace},
		Data:       data,
	}

	if err := r.Create(t.Context(), secret); err != nil {
		t.Fatalf("create secret %q: %v", name, err)
	}
}

// withEtcdTLS points a cluster at an etcd TLS Secret.
func withEtcdTLS(secretName string) func(*fsv1alpha1.FSCluster) {
	return func(c *fsv1alpha1.FSCluster) {
		c.Spec.Etcd.External.TLS.SecretName = secretName
	}
}

// TestEtcdTLSRendersAndMounts covers the CA-only case: the config names the
// mounted CA and nothing else, because fs refuses a cert_file without its key
// and pointing at a file that is not there fails the node at startup.
func TestEtcdTLSRendersAndMounts(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "etcd-tls", withEtcdTLS("etcd-trust"))

	etcdSecretFixture(t, r, key, "etcd-trust", map[string][]byte{EtcdCAKey: []byte("ca-pem")})

	reconcile(t, r, key)

	config := nodeConfigYAML(t, r, key, key.Name+"-0")
	if !strings.Contains(config, EtcdTLSDir+"/"+EtcdCAKey) {
		t.Errorf("the config does not name the mounted CA:\n%s", config)
	}

	if strings.Contains(config, EtcdCertKey) {
		t.Errorf("a CA-only Secret must not render a client certificate:\n%s", config)
	}

	// The file has to actually be there.
	var set appsv1.StatefulSet
	get(t, r, key.Namespace, key.Name+"-0", &set)

	if !mountsPath(set, EtcdTLSDir) {
		t.Error("the etcd TLS Secret is not mounted, so the config names a file that does not exist")
	}
}

// TestEtcdMutualTLSRendersBothFiles: with a client certificate in the Secret,
// the config names it too.
func TestEtcdMutualTLSRendersBothFiles(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "etcd-mtls", withEtcdTLS("etcd-mutual"))

	etcdSecretFixture(t, r, key, "etcd-mutual", map[string][]byte{
		EtcdCAKey:   []byte("ca-pem"),
		EtcdCertKey: []byte("cert-pem"),
		EtcdKeyKey:  []byte("key-pem"),
	})

	reconcile(t, r, key)

	config := nodeConfigYAML(t, r, key, key.Name+"-0")
	for _, want := range []string{EtcdCAKey, EtcdCertKey, EtcdKeyKey} {
		if !strings.Contains(config, EtcdTLSDir+"/"+want) {
			t.Errorf("the config does not name %q:\n%s", want, config)
		}
	}
}

// TestEtcdTLSRefusesBadSecret covers what a user has to be told rather than
// left to discover as a node that will not start.
func TestEtcdTLSRefusesBadSecret(t *testing.T) {
	for name, tc := range map[string]struct {
		cluster string
		data    map[string][]byte
		create  bool
		reason  fsv1alpha1.ConditionReason
	}{
		"missing secret": {
			cluster: "etcd-tls-missing",
			reason:  fsv1alpha1.ReasonSecretNotFound,
		},
		"no ca": {
			cluster: "etcd-tls-no-ca",
			create:  true,
			data:    map[string][]byte{EtcdCertKey: []byte("cert"), EtcdKeyKey: []byte("key")},
			reason:  fsv1alpha1.ReasonSecretInvalid,
		},
		"certificate without key": {
			cluster: "etcd-tls-half",
			create:  true,
			data:    map[string][]byte{EtcdCAKey: []byte("ca"), EtcdCertKey: []byte("cert")},
			reason:  fsv1alpha1.ReasonSecretInvalid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := reconciler(t)
			key := createCluster(t, r, tc.cluster, withEtcdTLS("etcd-trust"))

			if tc.create {
				etcdSecretFixture(t, r, key, "etcd-trust", tc.data)
			}

			reconcile(t, r, key)

			c := condition(t, r, key, fsv1alpha1.ConditionSpecValid)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tc.reason {
				t.Fatalf("SpecValid = %v, want False/%s", c, tc.reason)
			}

			// Refused means nothing was built from it.
			if len(statefulSets(t, r, key)) != 0 {
				t.Error("nodes were created from a spec whose etcd Secret is unusable")
			}
		})
	}
}

// TestEtcdAuthGoesThroughTheEnvironment is the one that matters for §9: an
// etcd password must not reach the rendered config, which is a Secret anything
// with namespace read access can read — and which is also fingerprinted into
// every config revision.
func TestEtcdAuthGoesThroughTheEnvironment(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "etcd-auth", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Etcd.External.AuthSecretRef = &corev1.LocalObjectReference{Name: "etcd-creds"}
	})

	etcdSecretFixture(t, r, key, "etcd-creds", map[string][]byte{
		EtcdUsernameKey: []byte("fs"),
		EtcdPasswordKey: []byte("hunter2"),
	})

	reconcile(t, r, key)

	config := nodeConfigYAML(t, r, key, key.Name+"-0")
	if strings.Contains(config, "hunter2") || strings.Contains(config, "username") {
		t.Errorf("etcd credentials were rendered into the config:\n%s", config)
	}

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, key.Name+"-0", &set)

	for _, want := range []string{"FS_ETCD_USERNAME", "FS_ETCD_PASSWORD"} {
		if !hasSecretEnv(set, want, "etcd-creds") {
			t.Errorf("%s is not sourced from the auth Secret", want)
		}
	}
}

func TestEtcdAuthRefusesIncompleteSecret(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "etcd-auth-half", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Etcd.External.AuthSecretRef = &corev1.LocalObjectReference{Name: "etcd-creds"}
	})

	etcdSecretFixture(t, r, key, "etcd-creds", map[string][]byte{EtcdUsernameKey: []byte("fs")})

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionSpecValid)
	if c == nil || c.Reason != fsv1alpha1.ReasonSecretInvalid {
		t.Fatalf("SpecValid = %v, want False/%s", c, fsv1alpha1.ReasonSecretInvalid)
	}
}

// TestEtcdPlaintextRendersNothing: the common case stays exactly as it was.
func TestEtcdPlaintextRendersNothing(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "etcd-plain", nil)

	reconcile(t, r, key)

	config := nodeConfigYAML(t, r, key, key.Name+"-0")
	if strings.Contains(config, "tls:") && strings.Contains(config, EtcdTLSDir) {
		t.Errorf("a plaintext cluster rendered etcd TLS:\n%s", config)
	}

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, key.Name+"-0", &set)

	if mountsPath(set, EtcdTLSDir) {
		t.Error("a plaintext cluster mounted an etcd TLS Secret")
	}
}

// nodeConfigYAML is a node's rendered configuration.
func nodeConfigYAML(t *testing.T, r *Reconciler, key types.NamespacedName, node string) string {
	t.Helper()

	var secret corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(node), &secret)

	return string(secret.Data[ConfigFileName])
}

// mountsPath reports whether the node container mounts path.
func mountsPath(set appsv1.StatefulSet, path string) bool {
	for _, mount := range set.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.MountPath == path {
			return true
		}
	}

	return false
}

// hasSecretEnv reports whether name is sourced from the given Secret.
func hasSecretEnv(set appsv1.StatefulSet, name, secret string) bool {
	for _, env := range set.Spec.Template.Spec.Containers[0].Env {
		if env.Name != name {
			continue
		}

		ref := env.ValueFrom
		if ref != nil && ref.SecretKeyRef != nil && ref.SecretKeyRef.Name == secret {
			return true
		}
	}

	return false
}
