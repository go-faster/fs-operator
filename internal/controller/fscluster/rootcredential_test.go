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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// rootAccessKeyOf reads the access key of a cluster's generated root
// credential — what the cluster's key store has to hold for anything to
// authenticate.
func rootAccessKeyOf(t *testing.T, r *Reconciler, key types.NamespacedName) string {
	t.Helper()

	var secret corev1.Secret
	get(t, r, key.Namespace, RootCredentialsSecretName(key.Name), &secret)

	access := string(secret.Data[AccessKeyKey])
	if access == "" {
		t.Fatalf("root credentials secret has no %q", AccessKeyKey)
	}

	return access
}

// quorumServing brings enough of a cluster's nodes up to acknowledge a write,
// so the Ready condition turns on everything except quorum.
func quorumServing(t *testing.T, r *Reconciler, key types.NamespacedName) {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	for _, node := range Nodes(&cluster)[:2] {
		serving(t, r, key, node)
	}

	reconcile(t, r, key)
}

// TestRootCredentialRegistered is the ordinary cluster: its own root credential
// is in the key store it seeded, and nothing is reported.
func TestRootCredentialRegistered(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "root-ok", nil)

	reconcile(t, r, key)
	admin.setAccessKeys(rootAccessKeyOf(t, r, key))
	quorumServing(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %v, want True for a cluster holding its own root credential", c)
	}
}

// TestRootCredentialUnregistered is the state a re-created cluster lands in
// without etcd cleanup: the prefix still holds the previous incarnation's keys,
// sealed with a cluster secret that has since been regenerated, so fs never
// seeds this cluster's root credential and nothing can authenticate.
//
// Every pod is up and passes its probes, which is exactly why the operator has
// to say so itself: reporting Ready here is what leaves an FSBucket hanging on
// an unreachable cluster with the cause a node's log away.
func TestRootCredentialUnregistered(t *testing.T) {
	r, recorder, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "root-stale", nil)

	reconcile(t, r, key)

	// The key store holds a credential from a previous incarnation, not this
	// cluster's root.
	admin.setAccessKeys("AKff694f15440c9917eb")
	quorumServing(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %v, want False when the root credential is not registered", c)
	}

	if c.Reason != fsv1alpha1.ReasonRootCredentialUnregistered {
		t.Errorf("Ready reason = %q, want %q", c.Reason, fsv1alpha1.ReasonRootCredentialUnregistered)
	}

	// The message has to name the prefix and the way out; it is the only place
	// the cause is visible.
	for _, want := range []string{"/fs/root-stale/root-stale", "etcdctl del --prefix", "cleanupOnDelete"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("Ready message %q does not mention %q", c.Message, want)
		}
	}

	warned := false
	for len(recorder.Events) > 0 {
		if ev := <-recorder.Events; contains(ev, fsv1alpha1.ReasonRootCredentialUnregistered) {
			warned = true
		}
	}

	if !warned {
		t.Errorf("no %s event", fsv1alpha1.ReasonRootCredentialUnregistered)
	}
}

// TestRootCredentialEmptyStoreIsUnknown keeps the check from crying wolf at a
// cluster that is still starting: a node seeds its root credential and the etcd
// watch then delivers the first snapshot, and in between the store is
// legitimately empty.
func TestRootCredentialEmptyStoreIsUnknown(t *testing.T) {
	r, _, _ := reconcilerWithAdmin(t)
	key := createCluster(t, r, "root-empty", nil)

	reconcile(t, r, key)
	quorumServing(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %v, want True: an empty key store says nothing yet", c)
	}
}
