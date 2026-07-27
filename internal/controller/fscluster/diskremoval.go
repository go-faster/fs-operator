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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// Event reasons a disk removal reports.
const (
	eventDiskDraining = "DiskDraining"
	eventDiskRemoved  = "DiskRemoved"
)

// drainReason is recorded with every override the operator sets, so an
// operator who finds a drained disk can tell who drained it and why.
const drainReason = "removed from FSCluster.spec.storage.disks"

// reconcileDiskRemoval takes a disk the spec no longer declares out of the
// cluster (SPEC §8.5).
//
// Unlike a node decommission, a disk is removed from *every* node at once:
// spec.storage.disks describes each node's storage identically, so dropping an
// entry drains that disk cluster-wide. Draining them together is also the
// cheaper choice — one placement change and one rebalance, rather than one per
// node.
//
// The removal itself is still one node at a time, because it recreates the
// node's StatefulSet, and that is the same contract as any other storage
// surgery.
func (r *Reconciler) reconcileDiskRemoval(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	removed := p.removedDisks()
	if len(removed) == 0 {
		return r.clearStaleDrains(ctx, p)
	}

	// Refusing here rather than draining first: a cluster with no disks holds
	// nothing, and there would be nowhere for the data to go.
	if len(p.cluster.Spec.Storage.Disks) == 0 {
		return r.refuse(p, fsv1alpha1.ReasonSpecInvalid,
			"the spec removes every disk; a cluster needs at least one")
	}

	disk := removed[0]

	// Take it out of placement everywhere, then wait for the rebalancer.
	drained, err := r.drainDisk(ctx, p, disk)
	if err != nil {
		return pipeline.Outcome{}, err
	}

	if !drained {
		return r.holdDiskRemoval(p, disk,
			fmt.Sprintf("draining disk %q out of placement on every node", disk))
	}

	if !p.convergence.known {
		return r.holdDiskRemoval(p, disk, fmt.Sprintf(
			"disk %q is drained, but no node answered: its occupancy is unknown", disk))
	}

	if !p.convergence.converged {
		return r.holdDiskRemoval(p, disk, fmt.Sprintf(
			"waiting for the cluster to move disk %q's data off (repair queue / rebalance)", disk))
	}

	// The same rule the node decommission holds to: a silent node makes the
	// view partial, and a partial view is not evidence a disk is empty.
	if !p.convergence.allReporting() {
		return r.holdDiskRemoval(p, disk, fmt.Sprintf(
			"%d node(s) are not reporting, so disk %q's occupancy cannot be confirmed",
			p.convergence.nodesNotReporting, disk))
	}

	if holding := p.nodesHoldingDisk(disk); len(holding) > 0 {
		return r.holdDiskRemoval(p, disk, fmt.Sprintf(
			"disk %q still holds data on node(s) %v", disk, holding))
	}

	return r.removeDisk(ctx, p, disk)
}

// removedDisks names the disks being removed, in a stable order. They are
// recorded before the render, which keeps them mounted until they are empty.
func (p *pass) removedDisks() []string {
	seen := make(map[string]bool)

	var removed []string

	for _, disks := range p.retainedDisks {
		for _, disk := range disks {
			if seen[disk] {
				continue
			}

			seen[disk] = true

			removed = append(removed, disk)
		}
	}

	slices.Sort(removed)

	return removed
}

// nodesHoldingDisk names the nodes whose copy of the disk is not known to be
// empty. Unknown counts as holding: silence is not evidence.
func (p *pass) nodesHoldingDisk(disk string) []string {
	var holding []string

	for _, node := range p.nodes {
		reported, ok := p.convergence.nodes[node.Name]
		if !ok {
			holding = append(holding, node.Name)

			continue
		}

		for _, reportedDisk := range reported.Disks {
			if reportedDisk.ID == disk && !reportedDisk.Empty() {
				holding = append(holding, node.Name)

				break
			}
		}
	}

	slices.Sort(holding)

	return holding
}

// drainDisk sets a zero placement weight for the disk on every node, and
// reports whether every node's copy is already out of placement.
//
// The override is control-plane state that outlives a node restart (fs §11.6),
// which is what lets this be an API call rather than a config change and a
// roll — a disk being removed from every node at once would otherwise mean
// rolling the whole cluster before any data could move.
func (r *Reconciler) drainDisk(ctx context.Context, p *pass, disk string) (bool, error) {
	admin, ok := r.clusterAdmin(ctx, p)
	if !ok {
		// Nothing is up to tell; the caller holds.
		return false, nil
	}

	drained := true

	for _, node := range p.nodes {
		reported, ok := p.convergence.nodes[node.Name]
		if !ok {
			drained = false

			continue
		}

		for _, reportedDisk := range reported.Disks {
			if reportedDisk.ID != disk {
				continue
			}

			if reportedDisk.Drained() {
				break
			}

			drained = false

			if err := admin.SetDiskWeight(ctx, node.Name, disk, 0, drainReason); err != nil {
				return false, err
			}

			r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventDiskDraining,
				"Draining disk %q on node %q out of placement before removing it", disk, node.Name)
		}
	}

	return drained, nil
}

// removeDisk drops the disk from one node's StatefulSet, one node per pass.
//
// The claim templates are immutable, so this is the same orphan-recreate the
// storage step uses to grow a disk: delete the StatefulSet leaving the pod
// running, and let the next pass rebuild it without the disk. The node's PVC
// for it then follows storage.reclaimPolicy.
func (r *Reconciler) removeDisk(ctx context.Context, p *pass, disk string) (pipeline.Outcome, error) {
	for i, node := range p.nodes {
		if !slices.Contains(p.retainedDisks[node.Name], disk) {
			continue
		}

		if waiting := p.health.notReady; len(waiting) > 0 {
			return pipeline.RequeueAfter(pollInterval,
				fmt.Sprintf("waiting for node(s) %v before removing disk %q", waiting, disk))
		}

		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventDiskRemoved,
			"Disk %q holds no data; removing it from node %q", disk, node.Name)

		// Orphan-delete the StatefulSet, leaving the pod running. The next
		// pass finds no live set for this node, so nothing is retained for it
		// and it is rebuilt from the spec alone — without the disk.
		return r.expandNode(ctx, p, node, p.desired[i])
	}

	// Every node is rebuilt without it: the override has nothing left to
	// describe, and leaving it would drain a disk of the same name if one were
	// ever added back.
	if err := r.clearDrain(ctx, p, disk); err != nil {
		return pipeline.Outcome{}, err
	}

	return pipeline.Continue()
}

// clearStaleDrains removes overrides the operator set for disks that are back
// in the spec — an interrupted removal the user changed their mind about.
func (r *Reconciler) clearStaleDrains(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	admin, ok := r.clusterAdmin(ctx, p)
	if !ok {
		return pipeline.Continue()
	}

	overrides, err := admin.ListDiskWeights(ctx)
	if err != nil {
		// Not worth failing a pass over: the override is inert while the disk
		// is declared, and the next pass tries again.
		return pipeline.Continue()
	}

	declared := make(map[string]bool, len(p.cluster.Spec.Storage.Disks))
	for _, disk := range p.cluster.Spec.Storage.Disks {
		declared[disk.Name] = true
	}

	for _, override := range overrides {
		if override.Reason != drainReason || !declared[override.Disk] {
			continue
		}

		if err := admin.ClearDiskWeight(ctx, override.Node, override.Disk); err != nil {
			return pipeline.Outcome{}, err
		}

		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventDiskDraining,
			"Disk %q is declared again; restoring its placement weight on node %q",
			override.Disk, override.Node)
	}

	return pipeline.Continue()
}

// clearDrain removes the operator's overrides for one removed disk.
func (r *Reconciler) clearDrain(ctx context.Context, p *pass, disk string) error {
	admin, ok := r.clusterAdmin(ctx, p)
	if !ok {
		return nil
	}

	for _, node := range p.nodes {
		if err := admin.ClearDiskWeight(ctx, node.Name, disk); err != nil {
			return err
		}
	}

	return nil
}

// clusterAdmin is an admin client for any serving node. Disk weights are
// cluster-wide state, so any node can write them.
func (r *Reconciler) clusterAdmin(ctx context.Context, p *pass) (fsclient.Interface, bool) {
	serving := servingNodes(p)
	if len(serving) == 0 {
		return nil, false
	}

	token, err := r.adminToken(ctx, p)
	if err != nil {
		return nil, false
	}

	for _, node := range serving {
		client, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, node.Name), token)
		if err != nil {
			continue
		}

		return client, true
	}

	return nil, false
}

// holdDiskRemoval keeps a removal where it is and says what it is waiting for.
func (r *Reconciler) holdDiskRemoval(p *pass, disk, reason string) (pipeline.Outcome, error) {
	p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
		fsv1alpha1.ReasonDraining, reason)

	return r.hold(p, fsv1alpha1.UpdatePhaseDraining, diskSubject(disk), reason)
}
