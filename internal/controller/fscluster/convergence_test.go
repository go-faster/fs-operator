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
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// TestRolloutGatesOnConvergence is the core of SPEC §8.2: a rollout will not
// replace a second node's failure domain until the cluster has reconverged.
// Here every pod is ready, but a layout change is still moving data, so the
// rollout holds until it finishes.
func TestRolloutGatesOnConvergence(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "converge-gate", 3)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)

	fake.startTransition()

	before := templateRevisions(t, r, key, nodes)

	// An image bump wants to roll every node.
	cluster.Spec.Image.Tag = "v0.99.0-test"

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("bump the image: %v", err)
	}

	reconcile(t, r, key)

	if changedKeys(before, templateRevisions(t, r, key, nodes)) {
		t.Error("a node was rolled while a layout change was still moving data")
	}

	c := condition(t, r, key, fsv1alpha1.ConditionConverged)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonLayoutTransition {
		t.Errorf("Converged = %v, want False/%s", c, fsv1alpha1.ReasonLayoutTransition)
	}

	fake.finishTransition()
	reconcile(t, r, key)

	rolled := changedNodes(before, templateRevisions(t, r, key, nodes))
	if len(rolled) != 1 {
		t.Fatalf("%d nodes rolled once converged, want exactly 1", len(rolled))
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionConverged); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Converged = %v, want True once the transition finished", c)
	}
}

// TestConvergedTracksTheNodes covers what Converged needs beyond a settled
// layout: every node up in the cluster's own view, and on the current version.
func TestConvergedTracksTheNodes(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "converge-nodes", 3)

	fake.setUp(false, "converge-nodes-2")
	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionConverged)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "converge-nodes-2") {
		t.Errorf("Converged = %v, want False naming the node that is down", c)
	}

	fake.setUp(true, "converge-nodes-2")
	fake.behind["converge-nodes-1"] = true
	reconcile(t, r, key)

	c = condition(t, r, key, fsv1alpha1.ConditionConverged)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "converge-nodes-1") {
		t.Errorf("Converged = %v, want False naming the node on an older layout", c)
	}

	delete(fake.behind, "converge-nodes-1")
	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionConverged); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Converged = %v, want True", c)
	}

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	if cluster.Status.UpNodes != 3 {
		t.Errorf("status.upNodes = %d, want 3", cluster.Status.UpNodes)
	}
}

// TestFirstLayoutWaitsForEveryNode: before the first layout no node is Ready —
// readiness is a storage probe, and storage needs a layout — so the operator
// gates on the cluster's own view instead: every declared node up.
func TestFirstLayoutWaitsForEveryNode(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "first-layout", nil)

	reconcile(t, r, key)
	fake.setUp(true, "first-layout-0", "first-layout-1")
	reconcile(t, r, key)

	if got := len(fake.appliedLayouts()); got != 0 {
		t.Fatalf("%d layouts applied with a node not up", got)
	}

	c := condition(t, r, key, fsv1alpha1.ConditionClusterSizeAligned)
	if c == nil || c.Reason != fsv1alpha1.ReasonLayoutPending || !strings.Contains(c.Message, "first-layout-2") {
		t.Errorf("ClusterSizeAligned = %v, want LayoutPending naming the missing node", c)
	}

	fake.setUp(true, "first-layout-2")
	reconcile(t, r, key)

	applied := fake.appliedLayouts()
	if len(applied) != 1 {
		t.Fatalf("%d layouts applied, want 1", len(applied))
	}

	want := []fsclient.Role{
		{ID: "first-layout-0", Capacity: testCapacity},
		{ID: "first-layout-1", Capacity: testCapacity},
		{ID: "first-layout-2", Capacity: testCapacity},
	}
	if !slices.Equal(applied[0].Members, want) || !slices.Equal(applied[0].Widths, []int{3}) {
		t.Errorf("layout = %+v, want every node at its volume's capacity, width 3", applied[0])
	}

	// Applying the same layout again would move nothing and bump the version.
	reconcile(t, r, key)

	if got := len(fake.appliedLayouts()); got != 1 {
		t.Errorf("%d layouts applied for an unchanged spec, want 1", got)
	}
}

// TestScaleUpJoinsTheLayout: a new node gets a share once it is up.
func TestScaleUpJoinsTheLayout(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "join", 3)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Topology.Nodes = new(int32(4))

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("grow the topology: %v", err)
	}

	reconcile(t, r, key)

	if got := len(fake.appliedLayouts()); got != 1 {
		t.Fatalf("%d layouts applied before the new node was up", got)
	}

	settleAll(t, r, key, fake)
	reconcile(t, r, key)

	applied := fake.appliedLayouts()
	if len(applied) != 2 || len(applied[1].Members) != 4 {
		t.Fatalf("layouts = %+v, want a second one with four members", applied)
	}
}

// TestLayoutFollowsTheWidths: a width for an erasure-coded bucket is a layout
// change like any other.
func TestLayoutFollowsTheWidths(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := laidOut(t, r, fake, "widths", 6)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Layout.Widths = []int32{6, 3}

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("add a width: %v", err)
	}

	reconcile(t, r, key)

	applied := fake.appliedLayouts()
	if len(applied) != 2 || !slices.Equal(applied[1].Widths, []int{3, 6}) {
		t.Fatalf("layouts = %+v, want a second one spread for widths [3 6]", applied)
	}
}

// TestLayoutRejectedIsReported: when fs refuses the roles, the reason reaches
// the object and an event rather than only the operator's log.
func TestLayoutRejectedIsReported(t *testing.T) {
	r, recorder, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "rejected", nil)

	reconcile(t, r, key)
	settleAll(t, r, key, fake)

	fake.rejectLayout = true
	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionClusterSizeAligned)
	if c == nil || c.Reason != fsv1alpha1.ReasonLayoutRejected {
		t.Errorf("ClusterSizeAligned = %v, want %s", c, fsv1alpha1.ReasonLayoutRejected)
	}

	found := false

	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, fsv1alpha1.ReasonLayoutRejected) {
			found = true
		}
	}

	if !found {
		t.Error("the refusal was not reported as an event")
	}
}
