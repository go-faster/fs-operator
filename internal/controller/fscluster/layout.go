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
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/go-faster/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// eventLayoutApplied reports a layout the operator applied. Like the
// rollout's reasons it is API surface: an operator watching events keys off it.
const eventLayoutApplied = "LayoutApplied"

// reconcileLayout keeps the cluster layout describing the declared nodes: each
// with its zone, rack and capacity, spread for the spec's widths (SPEC §8.4).
//
// The operator owns the layout. A node the spec adds joins it, a node the spec
// drops leaves it — fs then moves the data, and the removed node keeps running
// until it has (reconcileDecommission) — and a grown volume raises the node's
// share. A layout is applied only when every declared node is up in the
// cluster's own view, so no slot is handed to a node nobody can reach, and
// never while an earlier change is still moving data.
func (r *Reconciler) reconcileLayout(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	if p.cluster.Spec.SingleNode() {
		return pipeline.Continue()
	}

	roles := desiredRoles(p)
	widths := desiredWidths(&p.cluster.Spec)

	if !p.convergence.known {
		return r.holdLayout(p, "no node's admin API answered, so the layout cannot be read")
	}

	current := p.convergence.layout
	if current != nil && sameLayout(*current, roles, widths) {
		return pipeline.Continue()
	}

	if current != nil && current.Transitioning() {
		return r.holdLayout(p, fmt.Sprintf(
			"layout version %d is still moving data; the next change waits for it", current.Version))
	}

	// Nodes still being created are the ScalingUp the status reports; saying
	// the layout waits for them would only repeat it less precisely.
	if len(p.live) < len(p.nodes) {
		return pipeline.RequeueAfter(pollInterval, "waiting for every node to be created")
	}

	for _, role := range roles {
		if !p.convergence.up(role.ID) {
			return r.holdLayout(p, fmt.Sprintf(
				"node %q is not up in the cluster's view; it gets a share of the data once it is", role.ID))
		}
	}

	token, err := r.adminToken(ctx, p)
	if err != nil {
		return pipeline.Outcome{}, err
	}

	applied, err := r.applyLayout(ctx, p, roles, widths, token)
	if errors.Is(err, fsclient.ErrLayoutRejected) {
		message := fmt.Sprintf("fs refused the layout: %v", err)
		p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
			fsv1alpha1.ReasonLayoutRejected, message)
		r.Recorder.Event(p.object, corev1.EventTypeWarning, fsv1alpha1.ReasonLayoutRejected, message)

		return pipeline.RequeueAfter(pollInterval, "layout rejected")
	}

	if err != nil {
		return pipeline.Outcome{}, err
	}

	r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventLayoutApplied,
		"Applied layout version %d: %d node(s), widths %v", applied.Version, len(roles), widths)

	// The apply response is the computed layout alone: it does not carry the
	// versions the change retained, so read as it stands it would say nothing
	// is moving and let a removed node go while it still holds data. Nothing
	// after this step acts on this pass's view of the layout — removal waits
	// for a pass that reads it back (layoutApplied).
	p.layoutApplied = true
	p.convergence.layout = &applied
	p.convergence.converged = false
	p.convergence.waiting = fmt.Sprintf("layout version %d was just applied", applied.Version)

	return pipeline.RequeueAfter(pollInterval, "layout applied; waiting for the cluster to move data")
}

// applyLayout sends the layout to the first node that takes it; gossip carries
// it to the rest.
func (r *Reconciler) applyLayout(
	ctx context.Context, p *pass, roles []fsclient.Role, widths []int, token string,
) (fsclient.Layout, error) {
	var last error

	for _, node := range append(servingNodes(p), notServingNodes(p)...) {
		client, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, node.Name), token)
		if err != nil {
			return fsclient.Layout{}, err
		}

		applied, err := client.ApplyLayout(ctx, roles, widths)
		if errors.Is(err, fsclient.ErrLayoutRejected) {
			// Every node computes the same answer; asking another would only
			// be refused again.
			return fsclient.Layout{}, err
		}

		if err != nil {
			last = err

			continue
		}

		return applied, nil
	}

	if last == nil {
		last = errors.New("no node to apply the layout through")
	}

	return fsclient.Layout{}, errors.Wrap(last, "apply layout")
}

// holdLayout says why the layout does not describe the declared nodes yet and
// polls again. It does not stop the pass: credentials, reloads and the rest
// do not depend on the layout.
func (r *Reconciler) holdLayout(p *pass, reason string) (pipeline.Outcome, error) {
	p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
		fsv1alpha1.ReasonLayoutPending, reason)

	return pipeline.RequeueAfter(pollInterval, reason)
}

// desiredRoles is every declared node's role in the layout: its rack and zone
// as failure domains, and its volume as its capacity. Nodes being removed are
// absent — leaving the layout is how their data moves off.
func desiredRoles(p *pass) []fsclient.Role {
	capacity := uint64(p.cluster.Spec.Storage.Size.Value())

	roles := make([]fsclient.Role, 0, len(p.nodes))
	for _, node := range p.nodes {
		if _, removed := p.decommission.sets[node.Name]; removed {
			continue
		}

		roles = append(roles, fsclient.Role{
			ID:       node.Name,
			Zone:     node.Zone,
			Rack:     node.Rack,
			Capacity: capacity,
		})
	}

	slices.SortFunc(roles, func(a, b fsclient.Role) int { return cmp.Compare(a.ID, b.ID) })

	return roles
}

// desiredWidths is the spec's widths in the ascending order fs keeps them.
func desiredWidths(spec *fsv1alpha1.FSClusterSpec) []int {
	widths := make([]int, 0, len(spec.Layout.Widths))
	for _, w := range spec.Layout.Widths {
		widths = append(widths, int(w))
	}

	slices.Sort(widths)

	return slices.Compact(widths)
}

// sameLayout reports whether a layout already gives exactly these roles and
// widths, so applying them again would change nothing.
func sameLayout(current fsclient.Layout, roles []fsclient.Role, widths []int) bool {
	members := slices.Clone(current.Members)
	slices.SortFunc(members, func(a, b fsclient.Role) int { return cmp.Compare(a.ID, b.ID) })

	have := slices.Clone(current.Widths)
	slices.Sort(have)

	return slices.Equal(members, roles) && slices.Equal(have, widths)
}
