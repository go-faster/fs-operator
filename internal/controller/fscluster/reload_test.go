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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// TestReconcileHotReload is the §8.3 fast path end to end: a change touching
// only hot-reloadable material updates the config Secret and is applied by a
// reload — no pod restart — and ConfigurationInSync flips True only once every
// node reports the target config revision.
func TestReconcileHotReload(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "hot-reload", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)

	// Bring every node up and have it report the configuration it was given,
	// so the cluster starts in sync.
	for _, node := range nodes {
		serving(t, r, key, node)
		admin.setApplied(nodeAdminURL(key, node), configRevision(t, r, key, node))
	}

	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionConfigurationInSync); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("ConfigurationInSync = %v, want True once every node reports its config", c)
	}

	templatesBefore := templateRevisions(t, r, key, nodes)

	// A public-read bucket is hot-reloadable (it lives in the auth section), so
	// it must not roll the pods.
	get(t, r, key.Namespace, key.Name, &cluster)
	cluster.Spec.Auth.PublicReadBuckets = []string{publicBucket}

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("add a public-read bucket: %v", err)
	}

	// Propagation lag: the config Secret is bumped, but the kubelet has not yet
	// surfaced it on the nodes' volumes, so a reload re-reads the old config.
	reconcile(t, r, key)

	if templatesNow := templateRevisions(t, r, key, nodes); changedKeys(templatesBefore, templatesNow) {
		t.Error("a hot-reloadable change rolled the pods; it should reload in place")
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionConfigurationInSync); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ConfigurationInSync = %v, want False while the reload has not landed", c)
	}

	for _, node := range nodes {
		if admin.reloadCount(nodeAdminURL(key, node)) == 0 {
			t.Errorf("node %q was never asked to reload", node.Name)
		}
	}

	// The kubelet propagates the new Secret; the next reload picks it up.
	for _, node := range nodes {
		admin.setMounted(nodeAdminURL(key, node), configRevision(t, r, key, node))
	}

	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionConfigurationInSync); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("ConfigurationInSync = %v, want True once every node reloaded to the target", c)
	}

	// The whole change happened without a restart.
	if changedKeys(templatesBefore, templateRevisions(t, r, key, nodes)) {
		t.Error("the pods were rolled after all; a hot change must not restart them")
	}
}

// TestReconcileHotReloadWaitsForNode covers an unreachable node: the reload
// step requeues and does not call the cluster in sync while a node cannot be
// verified.
func TestReconcileHotReloadWaitsForNode(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "hot-reload-wait", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)
	for _, node := range nodes {
		serving(t, r, key, node)
		admin.setApplied(nodeAdminURL(key, node), configRevision(t, r, key, node))
	}

	// One node's admin API is unreachable.
	admin.setUnreachable(nodeAdminURL(key, nodes[0]), true)

	result := reconcile(t, r, key)

	if result.RequeueAfter == 0 {
		t.Error("a node that cannot be verified should requeue the pass")
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionConfigurationInSync); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ConfigurationInSync = %v, want False while a node is unverifiable", c)
	}
}

// nodeAdminURL is the admin endpoint the operator dials for a node.
func nodeAdminURL(key types.NamespacedName, node Node) string {
	return AdminURL(key.Name, key.Namespace, node.Name)
}

// configRevision reads a node's desired config revision from its config Secret.
func configRevision(t *testing.T, r *Reconciler, key types.NamespacedName, node Node) string {
	t.Helper()

	var secret corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(node.Name), &secret)

	rev := secret.Annotations[AnnotationConfigRevision]
	if rev == "" {
		t.Fatalf("node %q config Secret has no revision annotation", node.Name)
	}

	return rev
}

// changedKeys reports whether any node's value differs between two snapshots.
func changedKeys(before, after map[string]string) bool {
	for name, v := range after {
		if before[name] != v {
			return true
		}
	}

	return false
}
