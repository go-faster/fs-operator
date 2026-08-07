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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// twoDisks gives a cluster d0 and d1.
func twoDisks(c *fsv1alpha1.FSCluster) {
	c.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
		{Name: "d0", Size: resource.MustParse("10Gi")},
		{Name: "d1", Size: resource.MustParse("10Gi")},
	}
}

// removedDisk is the disk every case in this file drops.
const removedDisk = "d1"

// dropDisk removes removedDisk from the cluster's spec.
func dropDisk(t *testing.T, r *Reconciler, key types.NamespacedName) {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Storage.Disks = slices.DeleteFunc(cluster.Spec.Storage.Disks,
		func(d fsv1alpha1.DiskSpec) bool { return d.Name == removedDisk })

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("drop disk %q: %v", removedDisk, err)
	}
}

// liveWithDisks reports every node as carrying the given disks, each holding
// data unless named in empty, and each at the given placement weight.
func liveWithDisks(names []string, disks []string, weights map[string]float64, empty map[string]bool) []fsclient.ClusterNode {
	nodes := make([]fsclient.ClusterNode, 0, len(names))

	for _, name := range names {
		node := fsclient.ClusterNode{ID: name, Live: &fsclient.NodeLive{}}

		for _, disk := range disks {
			weight, ok := weights[disk]
			if !ok {
				weight = 1
			}

			node.Disks = append(node.Disks, fsclient.ClusterDisk{
				ID:             disk,
				Weight:         weight,
				CapacityKnown:  true,
				TotalBytes:     100,
				FreeBytes:      60,
				OccupancyKnown: true,
				HasData:        !empty[disk],
			})
		}

		nodes = append(nodes, node)
	}

	return nodes
}

// collectGarbage completes every terminating StatefulSet's deletion, standing
// in for the garbage collector envtest does not run. An orphan delete adds a
// finalizer that only the GC controller clears, so without this a node the
// operator removed a disk from is never actually recreated.
func collectGarbage(t *testing.T, r *Reconciler, key types.NamespacedName) {
	t.Helper()

	for _, set := range statefulSets(t, r, key) {
		if set.DeletionTimestamp.IsZero() {
			continue
		}

		finishGarbageCollection(t, r, key.Namespace, set.Name)
	}
}

// claimNames are a node's declared disks.
func claimNames(set appsv1.StatefulSet) []string {
	names := make([]string, 0, len(set.Spec.VolumeClaimTemplates))

	for _, claim := range set.Spec.VolumeClaimTemplates {
		// The state claim is on every node always; these tests are about the
		// disks coming and going around it.
		if claim.Name == fsv1alpha1.StateVolumeName {
			continue
		}

		names = append(names, claim.Name)
	}

	slices.Sort(names)

	return names
}

// TestDiskRemovalDrainsBeforeRemoving is SPEC §8.5: a disk the spec stops
// declaring is taken out of placement on every node and removed only once fs
// reports it holds nothing.
func TestDiskRemovalDrainsBeforeRemoving(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "disk-remove", twoDisks)

	reconcile(t, r, key)

	names := settleAll(t, r, key, fake)
	fake.setLive(0, liveWithDisks(names, []string{"d0", "d1"}, nil, nil)...)

	dropDisk(t, r, key)
	reconcile(t, r, key)

	// Still there: it holds data, and dropping it now would drop the data.
	var set appsv1.StatefulSet
	get(t, r, key.Namespace, key.Name+"-0", &set)

	if !slices.Contains(claimNames(set), "d1") {
		t.Fatal("the disk was removed while it still held data")
	}

	if phase := updatePhase(t, r, key); phase != fsv1alpha1.UpdatePhaseDraining {
		t.Errorf("update phase = %q, want %q", phase, fsv1alpha1.UpdatePhaseDraining)
	}

	// A disk is drained out of every node at once, so it is not attributable
	// to one of them. Reporting "d1" in the node field — which is what shipped
	// first — tells anyone reading the status that a node called d1 is being
	// rolled.
	var draining fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &draining)

	if got := draining.Status.Update.Disk; got != removedDisk {
		t.Errorf("status.update.disk = %q, want %q", got, removedDisk)
	}

	if got := draining.Status.Update.Node; got != "" {
		t.Errorf("status.update.node = %q, want it empty: no single node is being rolled", got)
	}

	// The operator should have drained it out of placement on every node.
	overrides := fake.overrides()
	if len(overrides) != len(names) {
		t.Fatalf("%d disks drained, want one per node (%d): %+v", len(overrides), len(names), overrides)
	}

	for _, override := range overrides {
		if override.Disk != "d1" || !override.Drained() {
			t.Errorf("override = %+v, want d1 drained", override)
		}
	}

	// Drained but still holding: the rebalancer has not finished.
	fake.setLive(0, liveWithDisks(names, []string{"d0", "d1"},
		map[string]float64{"d1": 0}, nil)...)
	reconcile(t, r, key)

	get(t, r, key.Namespace, key.Name+"-0", &set)

	if !slices.Contains(claimNames(set), "d1") {
		t.Fatal("the disk was removed while it still held data")
	}

	// Empty at last. The removal is an orphan-recreate, so it takes a pass to
	// delete the StatefulSet and another to rebuild it without the disk.
	fake.setLive(0, liveWithDisks(names, []string{"d0", "d1"},
		map[string]float64{"d1": 0}, map[string]bool{"d1": true})...)

	// Each node is rebuilt without the disk in turn: orphan-delete, then a
	// pass that recreates it from the spec alone.
	for range 8 {
		reconcile(t, r, key)
		collectGarbage(t, r, key)
		settleAll(t, r, key, fake)
	}

	get(t, r, key.Namespace, key.Name+"-0", &set)

	if got := claimNames(set); !slices.Equal(got, []string{"d0"}) {
		t.Errorf("node disks = %v, want only d0", got)
	}
}

// TestDiskRemovalHoldsWhenNotEmpty covers the gate that protects the data: a
// disk drained out of placement is not a disk whose data has moved.
func TestDiskRemovalHoldsWhenNotEmpty(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "disk-remove-holds", twoDisks)

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)

	// Already drained, still holding data on one node.
	live := liveWithDisks(names, []string{"d0", "d1"},
		map[string]float64{"d1": 0}, map[string]bool{"d1": true})
	live[1].Disks[1].HasData = true

	fake.setLive(0, live...)

	dropDisk(t, r, key)

	for range 4 {
		reconcile(t, r, key)
		collectGarbage(t, r, key)
		settleAll(t, r, key, fake)
	}

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, key.Name+"-0", &set)

	if !slices.Contains(claimNames(set), "d1") {
		t.Error("a disk was removed while another node's copy still held data")
	}

	c := condition(t, r, key, fsv1alpha1.ConditionClusterSizeAligned)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonDraining {
		t.Errorf("ClusterSizeAligned = %v, want False/%s", c, fsv1alpha1.ReasonDraining)
	}
}

// TestDiskRemovalHoldsOnPartialView: a silent node makes the view partial, and
// a partial view is not evidence that a disk is empty anywhere.
func TestDiskRemovalHoldsOnPartialView(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "disk-remove-partial", twoDisks)

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)

	// Everything reports empty, but one node is not reporting at all.
	fake.setLive(1, liveWithDisks(names, []string{"d0", "d1"},
		map[string]float64{"d1": 0}, map[string]bool{"d1": true})...)

	dropDisk(t, r, key)

	for range 3 {
		reconcile(t, r, key)
		collectGarbage(t, r, key)
		settleAll(t, r, key, fake)
	}

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, key.Name+"-0", &set)

	if !slices.Contains(claimNames(set), "d1") {
		t.Error("a disk was removed while a node was not reporting")
	}
}

// TestDiskRemovalRestoredWhenDeclaredAgain: changing your mind mid-drain has to
// put the disk back into placement, or it stays drained forever and the cluster
// quietly runs at half its capacity.
func TestDiskRemovalRestoredWhenDeclaredAgain(t *testing.T) {
	r, _, fake := reconcilerWithAdmin(t)
	key := createCluster(t, r, "disk-remove-undo", twoDisks)

	reconcile(t, r, key)
	names := settleAll(t, r, key, fake)
	fake.setLive(0, liveWithDisks(names, []string{"d0", "d1"}, nil, nil)...)

	dropDisk(t, r, key)
	reconcile(t, r, key)

	if len(fake.overrides()) == 0 {
		t.Fatal("the disk was never drained, so there is nothing to restore")
	}

	// Put it back.
	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Storage.Disks = append(cluster.Spec.Storage.Disks,
		fsv1alpha1.DiskSpec{Name: "d1", Size: resource.MustParse("10Gi")})

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("restore the disk: %v", err)
	}

	reconcile(t, r, key)

	if got := fake.overrides(); len(got) != 0 {
		t.Errorf("overrides = %+v, want them cleared once the disk is declared again", got)
	}
}

// TestRetainedDiskStaysFullyPresent covers what a live cluster caught and no
// unit test had: a disk being removed has to keep its claim template, its
// volume mount AND its config entry, together.
//
// Retaining the claim and the config but not the mount is what shipped first,
// and fs crash-looped on it: it creates each configured disk's root at boot,
// and on a read-only root filesystem a disk it was told about but not given
// fails with "mkdir: read-only file system". The node never comes back, so the
// drain it was retained for can never finish.
func TestRetainedDiskStaysFullyPresent(t *testing.T) {
	cluster := testCluster()
	cluster.Name, cluster.Namespace = "retain", testNamespace
	cluster.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
		{Name: "d0", Size: resource.MustParse("10Gi")},
	}
	cluster.Spec.WithDefaults()

	node := Nodes(cluster)[0]
	set := NewStatefulSet(cluster, node, "rev", "d1")

	if !slices.Contains(claimNames(*set), "d1") {
		t.Error("the retained disk has no claim template, so its volume is released early")
	}

	mounted := false

	for _, mount := range set.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.Name == "d1" && mount.MountPath == DiskPath("d1") {
			mounted = true
		}
	}

	if !mounted {
		t.Error("the retained disk is not mounted; fs will be told to use a volume it does not have")
	}

	configs, err := RenderNodeConfigs(cluster, []Node{node},
		RenderOptions{RetainDisks: map[string][]string{node.Name: {"d1"}}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	rendered := string(configs[node.Name].Data)
	if !strings.Contains(rendered, DiskPath("d1")) {
		t.Errorf("the retained disk is not in the config, so fs cannot move its data off:\n%s", rendered)
	}
}
