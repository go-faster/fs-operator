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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// TestRolloutGatesOnConvergence is the core of SPEC §8.2: a rollout will not
// replace a second node's failure domain until the cluster has reconverged.
// Here every pod is ready, but a rebalance is moving data, so the rollout holds
// until it finishes.
func TestRolloutGatesOnConvergence(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "converge-gate", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)
	for _, node := range nodes {
		serving(t, r, key, node)
		admin.setApplied(nodeAdminURL(key, node), configRevision(t, r, key, node))
	}

	// A rebalance is in flight: the cluster is not converged.
	admin.setRebalanceRunning(true)

	before := templateRevisions(t, r, key, nodes)

	// An image bump wants to roll every node.
	get(t, r, key.Namespace, key.Name, &cluster)
	cluster.Spec.Image.Tag = "v0.6.1"

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("bump the image: %v", err)
	}

	reconcile(t, r, key)

	// No node may be touched while the cluster is unconverged.
	if changedKeys(before, templateRevisions(t, r, key, nodes)) {
		t.Error("a node was rolled while the cluster was still rebalancing")
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionConverged); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("Converged = %v, want False while a rebalance runs", c)
	}

	// The rebalance finishes; now the rollout may proceed — one node.
	admin.setRebalanceRunning(false)

	reconcile(t, r, key)

	rolled := changed(before, templateRevisions(t, r, key, nodes))
	if len(rolled) != 1 {
		t.Fatalf("%d nodes rolled once converged, want exactly 1", len(rolled))
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionConverged); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Converged = %v, want True once the rebalance finished", c)
	}
}

// TestConvergedReflectsRepairQueue covers the Converged condition tracking the
// repair-queue backlog even when nothing is rolling.
func TestConvergedReflectsRepairQueue(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "converge-repair", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)
	for _, node := range nodes {
		serving(t, r, key, node)
		admin.setApplied(nodeAdminURL(key, node), configRevision(t, r, key, node))
	}

	admin.setRepairQueue(nodeAdminURL(key, nodes[0]), 7)

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionConverged)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonRepairQueueBacklog {
		t.Fatalf("Converged = %v, want False/%s with a repair backlog", c, fsv1alpha1.ReasonRepairQueueBacklog)
	}

	admin.setRepairQueue(nodeAdminURL(key, nodes[0]), 0)

	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionConverged); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Converged = %v, want True once the repair queue drained", c)
	}
}

// TestConvergedReadsAggregateRepairQueue covers the v0.9.0 path: the cluster
// status carries the repair queue summed over the nodes that reported, so the
// operator reads that instead of fanning out over the per-node rebalance
// endpoint — which it must keep doing for a cluster whose binaries predate it
// (TestConvergedReflectsRepairQueue covers that fallback).
func TestConvergedReadsAggregateRepairQueue(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "converge-aggregate", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)
	for _, node := range nodes {
		serving(t, r, key, node)
		admin.setApplied(nodeAdminURL(key, node), configRevision(t, r, key, node))
	}

	// The aggregate says there is work left; every per-node rebalance endpoint
	// says zero. Reading the aggregate is what keeps the rollout gated.
	admin.setLive(0, liveNode(nodes[0].Name, 1, 40, 100, 4))

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionConverged)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonRepairQueueBacklog {
		t.Fatalf("Converged = %v, want False/%s from the aggregate depth", c, fsv1alpha1.ReasonRepairQueueBacklog)
	}

	admin.setLive(0, liveNode(nodes[0].Name, 1, 40, 100, 0))

	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionConverged); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Converged = %v, want True once the aggregate queue drained", c)
	}
}

// changed names the nodes whose value differs between two snapshots.
func changed(before, after map[string]string) []string {
	var names []string

	for name, v := range after {
		if before[name] != v {
			names = append(names, name)
		}
	}

	return names
}
