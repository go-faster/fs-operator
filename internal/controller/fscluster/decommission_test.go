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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// shrinkToThree drops a flat cluster to three nodes, the smallest it may have.
func shrinkToThree(t *testing.T, r *Reconciler, key types.NamespacedName) {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Topology.Nodes = new(int32(3))

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("shrink the topology to three: %v", err)
	}
}

// settleAll stands in for the StatefulSet controller and the nodes themselves:
// every live node reports its pod up and current, its config revision
// applied, and itself up in the gossip view. It walks the live StatefulSets
// rather than the declared topology, because a node being removed is exactly
// the one the spec no longer names.
func settleAll(t *testing.T, r *Reconciler, key types.NamespacedName, fake *fakeAdmin) []string {
	t.Helper()

	sets := statefulSets(t, r, key)
	names := make([]string, 0, len(sets))

	for i := range sets {
		node := Node{Name: sets[i].Name}
		serving(t, r, key, node)
		fake.setApplied(nodeAdminURL(key, node), configRevision(t, r, key, node))

		names = append(names, sets[i].Name)
	}

	fake.setUp(true, names...)

	return names
}

// updatePhase is the rolling-change phase the status reports.
func updatePhase(t *testing.T, r *Reconciler, key types.NamespacedName) fsv1alpha1.UpdatePhase {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	if cluster.Status.Update == nil {
		return ""
	}

	return cluster.Status.Update.Phase
}

// exists reports whether a node's StatefulSet is still there.
func exists(t *testing.T, r *Reconciler, key types.NamespacedName, name string) bool {
	t.Helper()

	var set appsv1.StatefulSet

	err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: name}, &set)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get statefulset %q: %v", name, err)
	}

	return err == nil
}

// laidOut creates a cluster of n flat nodes, settles them and lets the
// operator apply the first layout.
func laidOut(t *testing.T, r *Reconciler, fake *fakeAdmin, name string, n int32) types.NamespacedName {
	t.Helper()

	key := createCluster(t, r, name, func(c *fsv1alpha1.FSCluster) { c.Spec.Topology.Nodes = &n })

	reconcile(t, r, key)
	settleAll(t, r, key, fake)
	reconcile(t, r, key)

	if applied := fake.appliedLayouts(); len(applied) != 1 || len(applied[0].Members) != int(n) {
		t.Fatalf("first layout = %+v, want one with %d members", applied, n)
	}

	return key
}

func TestRemovalKeepsTheNodeUntilItsDataHasMoved(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "decomm", 4)

	victim := "decomm-3"
	fake.transition = true

	shrinkToThree(t, r, key)
	reconcile(t, r, key)

	// The layout leaves the node out, which is what moves its data.
	applied := fake.appliedLayouts()
	if len(applied) != 2 {
		t.Fatalf("%d layouts applied, want a second one without %s", len(applied), victim)
	}

	if _, ok := applied[1].Member(victim); ok {
		t.Errorf("the second layout still gives %s a share", victim)
	}

	// It must still run: the spec no longer names it, but until the
	// transition completes fs still reads from it.
	if !exists(t, r, key, victim) {
		t.Fatal("the node was removed while its data was still moving")
	}

	if phase := updatePhase(t, r, key); phase != fsv1alpha1.UpdatePhaseDraining {
		t.Errorf("update phase = %q, want %q", phase, fsv1alpha1.UpdatePhaseDraining)
	}

	c := condition(t, r, key, fsv1alpha1.ConditionClusterSizeAligned)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonDraining {
		t.Errorf("ClusterSizeAligned = %v, want False/%s", c, fsv1alpha1.ReasonDraining)
	}

	settleAll(t, r, key, fake)
	reconcile(t, r, key)

	if !exists(t, r, key, victim) {
		t.Fatal("the node was removed while the transition was still running")
	}

	// Every node has synced the new layout: only now may it go.
	fake.finishTransition()
	reconcile(t, r, key)

	if exists(t, r, key, victim) {
		t.Fatal("the node survived the end of the transition")
	}

	// Its configuration goes with it; leaving it behind would re-seed the node
	// if the topology grew again.
	var secret corev1.Secret

	err := r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: ConfigSecretName(victim)}, &secret)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the removed node's config secret survived: %v", err)
	}

	if got := len(statefulSets(t, r, key)); got != 3 {
		t.Errorf("%d nodes left, want 3", got)
	}

	if got := len(fake.appliedLayouts()); got != 2 {
		t.Errorf("%d layouts applied, want no third one once the node is gone", got)
	}
}

// TestRemovalWaitsForTheLayoutChange covers a removal whose layout cannot be
// applied yet — a declared node is down, so the operator will not hand out
// slots — and the removed node, still in the layout, must keep running.
func TestRemovalWaitsForTheLayoutChange(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "decomm-wait", 4)

	fake.setUp(false, "decomm-wait-1")

	shrinkToThree(t, r, key)
	reconcile(t, r, key)

	if got := len(fake.appliedLayouts()); got != 1 {
		t.Fatalf("%d layouts applied, want none while a declared node is down", got-1)
	}

	if !exists(t, r, key, "decomm-wait-3") {
		t.Fatal("a node still in the layout was removed")
	}
}

// TestRemovalHoldsWhenNoNodeAnswers: with no view of the layout, whether the
// data has moved is unknown, and unknown means wait.
func TestRemovalHoldsWhenNoNodeAnswers(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "decomm-dark", 4)

	for _, name := range settleAll(t, r, key, fake) {
		fake.setUnreachable(nodeAdminURL(key, Node{Name: name}), true)
	}

	shrinkToThree(t, r, key)
	reconcile(t, r, key)

	if !exists(t, r, key, "decomm-dark-3") {
		t.Fatal("a node was removed with no view of the layout")
	}
}

// TestRemovalTakesSeveralNodesInOneChange: a layout transition already keeps
// every acknowledged write, so the nodes leave together rather than one data
// move per node.
func TestRemovalTakesSeveralNodesInOneChange(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "decomm-many", 5)

	shrinkToThree(t, r, key)
	reconcile(t, r, key)

	applied := fake.appliedLayouts()
	if len(applied) != 2 || len(applied[1].Members) != 3 {
		t.Fatalf("layouts = %+v, want one change to three members", applied)
	}

	for _, name := range []string{"decomm-many-3", "decomm-many-4"} {
		if exists(t, r, key, name) {
			t.Errorf("node %q survived a completed layout change", name)
		}
	}
}

func TestRemovalRefusesBelowTheMinimum(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "decomm-min", 3)

	// The API server refuses it outright; the controller would too.
	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Topology.Nodes = new(int32(2))

	if err := r.Update(t.Context(), &cluster); err == nil {
		t.Fatal("the API server accepted a two-node cluster")
	}

	reconcile(t, r, key)

	if got := len(statefulSets(t, r, key)); got != 3 {
		t.Errorf("%d nodes, want all 3 kept while the spec is refused", got)
	}

	if got := len(fake.appliedLayouts()); got != 1 {
		t.Errorf("%d layouts applied, want none past the first", got-1)
	}
}
