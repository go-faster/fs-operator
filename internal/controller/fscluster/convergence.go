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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// convergence is what the admin API reports about whether the cluster has
// settled after a membership or configuration change. The rollout will not
// replace a second node until the cluster has reconverged, and the Converged
// condition reports it (SPEC §8.2).
type convergence struct {
	// known is false when no node could be queried — a fresh cluster with no
	// serving nodes, or one whose admin API is unreachable. The rollout treats
	// unknown as not-yet-converged and holds rather than guessing.
	known bool

	// converged is true when the repair queue is empty and no rebalance is
	// running, so placement has settled.
	converged bool

	// repairQueue is the pending async remainder work summed across nodes;
	// rebalanceRunning reports whether a rebalance runner holds the election.
	repairQueue      int
	rebalanceRunning bool

	// nodesReporting and nodesNotReporting split the registered topology by
	// whether the node answered the live-state fetch. repairQueue only counts
	// the reporting ones, so a zero with nodes silent means "nobody said
	// otherwise", not "nothing left to do".
	//
	// The rollout gate tolerates silence — a cluster being upgraded from a
	// pre-v0.9.0 image has no node serving live state, and gating on a
	// complete view would stall it for good. The drain gate (SPEC §8.4) will
	// not: deleting a node on the strength of a partial view is the one
	// mistake a decommission cannot take back.
	nodesReporting, nodesNotReporting int

	// nodes is the per-node view, keyed by fs node ID: disks with their
	// capacity and placement weight. The drain gate reads occupancy from here
	// (SPEC §8.4).
	nodes map[string]fsclient.ClusterNode

	// skew is the placement skew (max minus min disk fullness) — informational,
	// surfaced in events, not a hard gate.
	skew float64

	// schemaVersion is what the cluster has agreed on in etcd; binarySchema is
	// what the deployed image implements. binarySchema > schemaVersion means a
	// migration is pending (SPEC §8.2 Migrating).
	schemaVersion, binarySchema int
}

// allReporting is true when every registered node answered the live-state
// fetch, so repairQueue is a complete count and each node's occupancy is
// current.
//
// The decommission gate requires it: deleting a node on the strength of a
// partial view is the one mistake it cannot take back. The rollout gate
// deliberately does not, or a cluster being upgraded from a pre-v0.9.0 image —
// where no node serves live state — could never proceed.
func (c convergence) allReporting() bool {
	return c.nodesNotReporting == 0 && c.nodesReporting > 0
}

// gatherConvergence reads the cluster's reconvergence state from the admin
// API. It runs before the rollout, which gates on the result, and leaves the
// convergence unknown (so the rollout holds) when no node can be reached.
func (r *Reconciler) gatherConvergence(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	log := logf.FromContext(ctx)

	serving := servingNodes(p)
	if len(serving) == 0 {
		// Nothing is up to ask; convergence is not yet meaningful.
		return pipeline.Continue()
	}

	token, err := r.adminToken(ctx, p)
	if err != nil {
		// The secrets step handles a missing token; here, just defer.
		log.V(1).Info("Admin token unavailable; convergence unknown", "error", err)

		return pipeline.Continue()
	}

	status, ok := r.clusterStatus(ctx, p, serving, token)
	if !ok {
		// No node answered; leave convergence unknown so the rollout holds.
		return pipeline.Continue()
	}

	repairQueue := r.repairQueueDepth(ctx, p, status, serving, token)

	nodes := make(map[string]fsclient.ClusterNode, len(status.Nodes))
	for _, node := range status.Nodes {
		nodes[node.ID] = node
	}

	p.convergence = convergence{
		known:             true,
		repairQueue:       repairQueue,
		rebalanceRunning:  status.RebalanceRunning,
		nodesReporting:    status.NodesReporting,
		nodesNotReporting: status.NodesNotReporting,
		nodes:             nodes,
		skew:              status.PlacementSkew,
		schemaVersion:     status.SchemaVersion,
		binarySchema:      status.BinarySchema,
		// Converged when the repair queue has drained and no rebalance is
		// moving data — placement has settled (SPEC §8.2, §11.2).
		converged: repairQueue == 0 && !status.RebalanceRunning,
	}

	return pipeline.Continue()
}

// clusterStatus reads the cluster-wide status from the first serving node that
// answers.
func (r *Reconciler) clusterStatus(ctx context.Context, p *pass, serving []Node, token string) (fsclient.ClusterStatus, bool) {
	log := logf.FromContext(ctx)

	for _, node := range serving {
		client, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, node.Name), token)
		if err != nil {
			continue
		}

		status, err := client.ClusterStatus(ctx)
		if err != nil {
			log.V(1).Info("Node cluster status unreachable", "node", node.Name, "error", err)

			continue
		}

		if status.Disabled {
			continue
		}

		return status, true
	}

	return fsclient.ClusterStatus{}, false
}

// repairQueueDepth reports the cluster's pending async remainder work.
//
// Since fs v0.9.0 the cluster status carries the sum itself, over the nodes
// that answered the live-state fetch, and that is what the operator reads.
// Nodes running an older binary do not serve live state at all, so a cluster
// still on a pre-v0.9.0 image reports no depth there — and a zero meaning
// "nobody answered" must not be read as "the queue is empty". When not one
// node reports, fall back to fanning out over the per-node rebalance endpoint,
// which every supported fs release serves (SPEC §11.2).
func (r *Reconciler) repairQueueDepth(
	ctx context.Context, p *pass, status fsclient.ClusterStatus, serving []Node, token string,
) int {
	log := logf.FromContext(ctx)

	if status.NodesReporting > 0 {
		return status.RepairQueueDepth
	}

	var total int

	for _, node := range serving {
		client, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, node.Name), token)
		if err != nil {
			continue
		}

		rb, err := client.Rebalance(ctx)
		if err != nil {
			log.V(1).Info("Node rebalance status unreachable", "node", node.Name, "error", err)

			continue
		}

		total += rb.RepairQueueDepth
	}

	return total
}

// servingNodes is the declared nodes whose pod is up and current.
func servingNodes(p *pass) []Node {
	serving := make([]Node, 0, len(p.nodes))

	for _, node := range p.nodes {
		if set, ok := p.live[node.Name]; ok && nodeServing(set) {
			serving = append(serving, node)
		}
	}

	return serving
}
