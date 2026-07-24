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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// TestRollOrderInterleavesRacks pins the ordering rule that keeps a rollout
// from taking two nodes of one failure domain in a row.
func TestRollOrderInterleavesRacks(t *testing.T) {
	nodes := []Node{
		{Name: "prod-a-0", Rack: "a"},
		{Name: "prod-a-1", Rack: "a"},
		{Name: "prod-b-0", Rack: "b"},
		{Name: "prod-b-1", Rack: "b"},
		{Name: "prod-c-0", Rack: "c"},
	}

	stale := make([]*appsv1.StatefulSet, 0, len(nodes))
	for _, node := range nodes {
		stale = append(stale, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: node.Name}})
	}

	ordered := rollOrder(stale, nodes)

	order := make([]string, 0, len(ordered))
	for _, set := range ordered {
		order = append(order, set.Name)
	}

	want := "prod-a-0,prod-b-0,prod-c-0,prod-a-1,prod-b-1"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("roll order = %q, want %q", got, want)
	}
}

// TestRollOrderKeepsFlatOrder covers the flat topology, where every node is its
// own domain and the declared order is already the safe one.
func TestRollOrderKeepsFlatOrder(t *testing.T) {
	nodes := []Node{{Name: node0}, {Name: node1}, {Name: node2}}

	stale := []*appsv1.StatefulSet{
		{ObjectMeta: metav1.ObjectMeta{Name: node0}},
		{ObjectMeta: metav1.ObjectMeta{Name: node2}},
	}

	ordered := rollOrder(stale, nodes)
	if len(ordered) != 2 || ordered[0].Name != node0 || ordered[1].Name != node2 {
		t.Errorf("roll order = %v, want the declared order", ordered)
	}
}

// TestRolloutReplacesOneNodeAtATime is the upgrade contract of fs, encoded: a
// changed pod template reaches one node, and the next node waits until that
// one is serving again.
func TestRolloutReplacesOneNodeAtATime(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "rollout", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)
	for _, node := range nodes {
		serving(t, r, key, node)
	}

	before := templateRevisions(t, r, key, nodes)

	// An image bump changes every node's pod template without touching its
	// configuration — exactly the case a config fingerprint alone would miss.
	get(t, r, key.Namespace, key.Name, &cluster)
	cluster.Spec.Image.Tag = "v0.5.1"

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("bump the image: %v", err)
	}

	reconcile(t, r, key)

	rolled := changedNodes(before, templateRevisions(t, r, key, nodes))
	if len(rolled) != 1 {
		t.Fatalf("%d nodes were replaced in one pass, want exactly 1", len(rolled))
	}

	get(t, r, key.Namespace, key.Name, &cluster)

	update := cluster.Status.Update
	if update == nil || update.Phase != fsv1alpha1.UpdatePhaseRollingNodes || update.Node != rolled[0] {
		t.Fatalf("status.update = %v, want node %q rolling", update, rolled[0])
	}

	// While the replaced node is not serving, no other node may be touched.
	notServing(t, r, key, rolled[0])
	reconcile(t, r, key)

	if got := changedNodes(before, templateRevisions(t, r, key, nodes)); len(got) != 1 {
		t.Fatalf("%d nodes replaced while one was down, want the rollout to hold at 1", len(got))
	}

	get(t, r, key.Namespace, key.Name, &cluster)

	if c := condition(t, r, key, fsv1alpha1.ConditionConfigurationInSync); c == nil ||
		c.Status != metav1.ConditionFalse {
		t.Errorf("ConfigurationInSync = %v, want False mid-rollout", c)
	}

	// The node comes back; the next one goes.
	serving(t, r, key, node(nodes, rolled[0]))
	reconcile(t, r, key)

	if got := changedNodes(before, templateRevisions(t, r, key, nodes)); len(got) != 2 {
		t.Fatalf("%d nodes replaced, want the rollout to move on to the second", len(got))
	}
}

// TestRolloutCreatesNewNodesAtOnce covers the other half: joining is additive,
// so scale-up is not serialized behind a rollout gate.
func TestRolloutCreatesNewNodesAtOnce(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "rollout-scale", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	// The existing nodes stay down: creating new ones does not wait on them.
	larger := int32(6)
	cluster.Spec.Topology.Nodes = &larger

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("grow the topology: %v", err)
	}

	reconcile(t, r, key)

	if got, want := len(statefulSets(t, r, key)), 6; got != want {
		t.Errorf("%d node statefulsets, want all %d created at once", got, want)
	}
}

// templateRevisions reads each node's stamped pod-template fingerprint.
func templateRevisions(t *testing.T, r *Reconciler, key types.NamespacedName, nodes []Node) map[string]string {
	t.Helper()

	revisions := make(map[string]string, len(nodes))

	for _, n := range nodes {
		var set appsv1.StatefulSet
		get(t, r, key.Namespace, n.Name, &set)

		revisions[n.Name] = set.Annotations[AnnotationTemplateRevision]
	}

	return revisions
}

// changedNodes names the nodes whose pod template changed between two reads.
func changedNodes(before, after map[string]string) []string {
	var changed []string

	for name, revision := range after {
		if before[name] != revision {
			changed = append(changed, name)
		}
	}

	return changed
}

// notServing reports a node's pod as gone, the way a StatefulSet controller
// does while it replaces one.
func notServing(t *testing.T, r *Reconciler, key types.NamespacedName, name string) {
	t.Helper()

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, name, &set)

	set.Status.ObservedGeneration = set.Generation
	set.Status.ReadyReplicas = 0
	set.Status.UpdatedReplicas = 0
	set.Status.AvailableReplicas = 0

	if err := r.Status().Update(t.Context(), &set); err != nil {
		t.Fatalf("mark node %q down: %v", name, err)
	}
}

// node finds a node by name.
func node(nodes []Node, name string) Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}

	return Node{}
}
