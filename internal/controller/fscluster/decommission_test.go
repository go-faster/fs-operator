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
	"sigs.k8s.io/yaml"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// shrink drops a flat cluster to n nodes.
func shrink(t *testing.T, r *Reconciler, key types.NamespacedName, n int32) {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Topology.Nodes = &n

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("shrink the topology to %d: %v", n, err)
	}
}

// settleAll stands in for the StatefulSet controller and the nodes' admin APIs:
// every live node reports its pod up and current, and its config revision
// applied. It walks the live StatefulSets rather than the declared topology,
// because a decommissioning node is exactly the one the spec no longer names.
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

	return names
}

// drainedConfig reports whether a node's rendered config takes it out of
// placement — every disk at a weight that is not positive.
func drainedConfig(t *testing.T, r *Reconciler, key types.NamespacedName, node string) bool {
	t.Helper()

	var secret corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(node), &secret)

	var config struct {
		Cluster struct {
			Disks []struct {
				Weight float64 `yaml:"weight"`
			} `yaml:"disks"`
		} `yaml:"cluster"`
	}

	if err := yaml.Unmarshal(secret.Data[ConfigFileName], &config); err != nil {
		t.Fatalf("parse node %q config: %v", node, err)
	}

	if len(config.Cluster.Disks) == 0 {
		t.Fatalf("node %q config declares no disks", node)
	}

	for _, disk := range config.Cluster.Disks {
		if disk.Weight > 0 {
			return false
		}
	}

	return true
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

// liveCluster reports every node as holding data, except those named empty.
func liveCluster(names []string, empty map[string]bool) []fsclient.ClusterNode {
	nodes := make([]fsclient.ClusterNode, 0, len(names))

	for _, name := range names {
		nodes = append(nodes, fsclient.ClusterNode{
			ID: name,
			Disks: []fsclient.ClusterDisk{{
				ID:             "disk-0",
				Weight:         1,
				CapacityKnown:  true,
				TotalBytes:     100,
				FreeBytes:      60,
				OccupancyKnown: true,
				HasData:        !empty[name],
			}},
			Live: &fsclient.NodeLive{},
		})
	}

	return nodes
}

// TestDecommissionDrainsBeforeRemoving is SPEC §8.4 end to end: a node the spec
// stops declaring is taken out of placement, kept running while the cluster
// moves its data off, and only then deleted.
func TestDecommissionDrainsBeforeRemoving(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "decomm", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(4)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)

	names := settleAll(t, r, key, fake)
	if len(names) != 4 {
		t.Fatalf("%d nodes, want 4 before the decommission", len(names))
	}

	// The highest-index node is the one that leaves first.
	victim := "decomm-3"

	shrink(t, r, key, 3)
	reconcile(t, r, key)

	// It must still exist: the spec no longer names it, but it still holds
	// data, and dropping it here is the mistake the whole flow exists to avoid.
	var set appsv1.StatefulSet
	get(t, r, key.Namespace, victim, &set)

	// And it must have been taken out of placement.
	config := drainedConfig(t, r, key, victim)
	if !config {
		t.Error("the decommissioning node's config still places data on its disks")
	}

	if phase := updatePhase(t, r, key); phase != fsv1alpha1.UpdatePhaseDraining {
		t.Errorf("update phase = %q, want %q", phase, fsv1alpha1.UpdatePhaseDraining)
	}

	c := condition(t, r, key, fsv1alpha1.ConditionClusterSizeAligned)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonDraining {
		t.Errorf("ClusterSizeAligned = %v, want False/%s", c, fsv1alpha1.ReasonDraining)
	}

	// It restarts onto the drained config and the cluster still reports it as
	// holding data: not removable yet.
	settleAll(t, r, key, fake)
	fake.setLive(0, liveCluster(names, nil)...)

	reconcile(t, r, key)

	if err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set); err != nil {
		t.Fatalf("node %q was removed while it still held data: %v", victim, err)
	}

	// The rebalancer finishes: its disks report empty, and only now may it go.
	fake.setLive(0, liveCluster(names, map[string]bool{victim: true})...)

	reconcile(t, r, key)

	err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("node %q still exists after it drained: %v", victim, err)
	}

	// Its configuration goes with it; leaving it behind would re-seed the node
	// if the topology grew again.
	var secret corev1.Secret

	err = r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: ConfigSecretName(victim)}, &secret)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the removed node's config secret survived: %v", err)
	}

	if got := len(statefulSets(t, r, key)); got != 3 {
		t.Errorf("%d nodes left, want 3", got)
	}
}

// TestDecommissionWaitsForTheDrainedConfigToLand covers the first gate, which
// a live e2e caught being waved through: the node must actually be *running*
// the drained config before its occupancy means anything. Until it restarts its
// disks are still in placement and still taking writes, so an "empty" read from
// the pass that only just rendered the drain is not evidence of anything.
func TestDecommissionWaitsForTheDrainedConfigToLand(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "decomm-gate", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(4)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)

	victim := "decomm-gate-3"

	// Empty from the very first pass — a node that just joined and holds
	// nothing, which is exactly the case that hid the bug.
	fake.setLive(0, liveCluster(names, map[string]bool{victim: true})...)

	shrink(t, r, key, 3)
	reconcile(t, r, key)

	// The drain was rendered this pass; the node has not restarted onto it yet,
	// so it must still be here.
	var set appsv1.StatefulSet
	if err := r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set); err != nil {
		t.Fatalf("node removed in the same pass that rendered its drain: %v", err)
	}

	if phase := updatePhase(t, r, key); phase != fsv1alpha1.UpdatePhaseDraining {
		t.Errorf("update phase = %q, want %q while the drain lands", phase,
			fsv1alpha1.UpdatePhaseDraining)
	}

	// The pod comes back on the drained config, and only now may it go.
	settleAll(t, r, key, fake)
	reconcile(t, r, key)

	err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set)
	if !apierrors.IsNotFound(err) {
		t.Errorf("node still present once drained and empty: %v", err)
	}
}

// TestDecommissionHoldsWhileUnconverged covers the gate that matters most: a
// node reporting empty is not enough while the cluster is still moving data,
// because the rebalancer is what moves it.
func TestDecommissionHoldsWhileUnconverged(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "decomm-unconverged", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(4)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)

	victim := "decomm-unconverged-3"

	shrink(t, r, key, 3)
	reconcile(t, r, key)
	settleAll(t, r, key, fake)

	// Empty, but a rebalance is still moving data cluster-wide.
	fake.setRebalanceRunning(true)
	fake.setLive(0, liveCluster(names, map[string]bool{victim: true})...)

	reconcile(t, r, key)

	var set appsv1.StatefulSet
	if err := r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set); err != nil {
		t.Fatalf("node removed while the cluster was still rebalancing: %v", err)
	}

	fake.setRebalanceRunning(false)
	reconcile(t, r, key)

	err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set)
	if !apierrors.IsNotFound(err) {
		t.Errorf("node still present once converged and empty: %v", err)
	}
}

// TestDecommissionHoldsOnPartialView covers the unknown cases, which must all
// resolve to waiting: a node that did not report leaves the cluster's view
// incomplete, and an incomplete view is not evidence that a disk is empty.
func TestDecommissionHoldsOnPartialView(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "decomm-partial", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(4)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)

	victim := "decomm-partial-3"

	shrink(t, r, key, 3)
	reconcile(t, r, key)
	settleAll(t, r, key, fake)

	// The victim says it is empty, but another node is silent.
	fake.setLive(1, liveCluster(names, map[string]bool{victim: true})...)

	reconcile(t, r, key)

	var set appsv1.StatefulSet
	if err := r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set); err != nil {
		t.Fatalf("node removed while a node was not reporting: %v", err)
	}

	// A disk that could not be probed is the same kind of unknown.
	partial := liveCluster(names, map[string]bool{victim: true})
	for i := range partial {
		if partial[i].ID == victim {
			partial[i].Disks[0].OccupancyKnown = false
			partial[i].Disks[0].DataError = "input/output error"
		}
	}

	fake.setLive(0, partial...)
	reconcile(t, r, key)

	if err := r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set); err != nil {
		t.Fatalf("node removed on a disk that could not be probed: %v", err)
	}
}

// TestDecommissionNeverActsOnPreV0100Cluster covers a cluster whose binaries do
// not report occupancy at all: nothing reads as empty, so the decommission
// waits forever rather than deleting on a signal that is not there.
func TestDecommissionNeverActsOnPreV0100Cluster(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "decomm-old", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(4)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)

	victim := "decomm-old-3"

	shrink(t, r, key, 3)
	reconcile(t, r, key)
	settleAll(t, r, key, fake)

	// Reporting, converged — but every disk's occupancy is absent.
	old := liveCluster(names, nil)
	for i := range old {
		old[i].Disks[0].OccupancyKnown = false
		old[i].Disks[0].HasData = false
	}

	fake.setLive(0, old...)

	reconcile(t, r, key)

	var set appsv1.StatefulSet
	if err := r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: victim}, &set); err != nil {
		t.Fatalf("node removed by a cluster that reports no occupancy: %v", err)
	}
}

// TestDecommissionRemovesOneAtATime covers the serialization: two nodes dropped
// at once are removed one after the other, highest index first, never together.
func TestDecommissionRemovesOneAtATime(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "decomm-serial", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(5)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)

	shrink(t, r, key, 3)

	// Everything is empty and converged: only the one-at-a-time rule stands
	// between the spec and both nodes disappearing.
	for range 2 {
		reconcile(t, r, key)
		settleAll(t, r, key, fake)
		fake.setLive(0, liveCluster(names, map[string]bool{
			"decomm-serial-4": true,
			"decomm-serial-3": true,
		})...)
		reconcile(t, r, key)

		remaining := statefulSets(t, r, key)
		if len(remaining) < 3 {
			t.Fatalf("%d nodes left; the removals were not serialized", len(remaining))
		}

		names = nil
		for i := range remaining {
			names = append(names, remaining[i].Name)
		}
	}

	if got := len(statefulSets(t, r, key)); got != 3 {
		t.Errorf("%d nodes after both decommissions, want 3", got)
	}
}

// TestDecommissionRefusesBelowSchemeMinimum covers what is still refused
// outright: a topology too small for its scheme is rejected, and no node is
// drained on the way to an invalid cluster.
func TestDecommissionRefusesBelowSchemeMinimum(t *testing.T) {
	r, _, _ := reconcilerWithAdmin(t)
	key := createCluster(t, r, "decomm-floor", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(4)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)

	shrink(t, r, key, 2)
	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionSpecValid)
	if c == nil || c.Status != metav1.ConditionFalse ||
		c.Reason != fsv1alpha1.ReasonSchemeTopologyMismatch {
		t.Fatalf("SpecValid = %v, want False/%s", c, fsv1alpha1.ReasonSchemeTopologyMismatch)
	}

	if got := len(statefulSets(t, r, key)); got != 4 {
		t.Errorf("%d nodes, want the original 4 left untouched", got)
	}
}
