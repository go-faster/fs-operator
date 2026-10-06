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

	"github.com/go-faster/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// convergence is what the admin API reports about the cluster: the layout it
// runs, and which nodes are up on it. The rollout will not replace a second
// node, and no node is removed, until the cluster has reconverged; the
// Converged condition reports it (SPEC §8.2).
type convergence struct {
	// known is false when no node could be queried — a fresh cluster whose
	// pods are not up yet, or one whose admin API is unreachable. Every gate
	// treats unknown as not-yet-converged and holds rather than guessing.
	known bool

	// layout is the layout the queried node has adopted; nil before the
	// first one is applied.
	layout *fsclient.Layout

	// nodes is the queried node's gossip view, keyed by fs node ID: whether
	// each node answered the last exchange, and which layout it last
	// reported.
	nodes map[string]fsclient.Node

	// converged is true when a layout exists, no older version is still in
	// transition, and every node of the cluster is up on the current
	// version.
	converged bool

	// waiting says what converged is waiting for, in the terms a reader can
	// act on; empty when converged.
	waiting string
}

// up reports whether the cluster's own view has a node up.
func (c convergence) up(name string) bool {
	node, ok := c.nodes[name]

	return ok && node.Up
}

// gatherConvergence reads the cluster's layout and node view from the admin
// API. It runs before the rollout and the layout step, which gate on the
// result, and leaves the convergence unknown (so they hold) when no node can
// be reached.
func (r *Reconciler) gatherConvergence(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	log := logf.FromContext(ctx)

	if p.cluster.Spec.SingleNode() {
		// No layout, no peers, no transitions: the cluster endpoints are 501
		// here. Reported converged rather than unknown, because unknown is
		// what holds a rollout (SPEC §5.2).
		p.convergence = convergence{known: true, converged: true}

		return pipeline.Continue()
	}

	token, err := r.adminToken(ctx, p)
	if err != nil {
		// The secrets step handles a missing token; here, just defer.
		log.V(1).Info("Admin token unavailable; convergence unknown", "error", err)

		return pipeline.Continue()
	}

	// Serving nodes first: they are the likeliest to answer. The rest are
	// asked too, because until the first layout is applied no node is Ready
	// — readiness is a storage probe, and storage needs a layout.
	for _, node := range append(servingNodes(p), notServingNodes(p)...) {
		view, err := r.clusterView(ctx, p, node, token)
		if err != nil {
			log.V(1).Info("Node cluster view unreachable", "node", node.Name, "error", err)

			continue
		}

		p.convergence = view
		p.convergence.converged, p.convergence.waiting = converged(p, view)

		return pipeline.Continue()
	}

	return pipeline.Continue()
}

// clusterView reads one node's layout and gossip view.
func (r *Reconciler) clusterView(ctx context.Context, p *pass, node Node, token string) (convergence, error) {
	client, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, node.Name), token)
	if err != nil {
		return convergence{}, err
	}

	view := convergence{known: true, nodes: map[string]fsclient.Node{}}

	layout, err := client.Layout(ctx)

	switch {
	case errors.Is(err, fsclient.ErrNoLayout):
	case err != nil:
		return convergence{}, err
	default:
		view.layout = &layout
	}

	nodes, err := client.Nodes(ctx)
	if err != nil {
		return convergence{}, err
	}

	for _, n := range nodes {
		if n.ID != "" {
			view.nodes[n.ID] = n
		}
	}

	return view, nil
}

// converged decides whether the cluster has settled, and if not, what it is
// waiting for.
func converged(p *pass, view convergence) (bool, string) {
	if view.layout == nil {
		return false, "no layout has been applied yet"
	}

	if view.layout.Transitioning() {
		return false, fmt.Sprintf("layout version %d is moving data; versions %v are still retained",
			view.layout.Version, view.layout.Retained)
	}

	for _, node := range p.nodes {
		n, ok := view.nodes[node.Name]

		switch {
		case !ok || !n.Up:
			return false, fmt.Sprintf("node %q is not up in the cluster's view", node.Name)
		case n.LayoutVersion < view.layout.Version:
			return false, fmt.Sprintf("node %q is on layout version %d, not %d",
				node.Name, n.LayoutVersion, view.layout.Version)
		}
	}

	return true, ""
}

// servingNodes is the cluster's nodes whose pod is up and current.
func servingNodes(p *pass) []Node {
	serving := make([]Node, 0, len(p.nodes))

	for _, node := range p.nodes {
		if set, ok := p.live[node.Name]; ok && nodeServing(set) {
			serving = append(serving, node)
		}
	}

	return serving
}

// notServingNodes is the cluster's nodes whose pod exists but is not serving:
// starting, not Ready, or on an older revision.
func notServingNodes(p *pass) []Node {
	var rest []Node

	for _, node := range p.nodes {
		if set, ok := p.live[node.Name]; ok && !nodeServing(set) {
			rest = append(rest, node)
		}
	}

	return rest
}
