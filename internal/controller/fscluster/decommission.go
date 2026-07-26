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
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-faster/errors"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// Event reasons a decommission reports. Like the rollout's, they are API
// surface: an operator watching events keys off them.
const (
	eventNodeDraining = "NodeDraining"
	eventNodeDrained  = "NodeDrained"
	eventNodeRemoved  = "NodeRemoved"
)

// decommission is the node the spec no longer declares that is being removed
// now, and what still stands between it and deletion (SPEC §8.4).
//
// Exactly one node is decommissioned at a time: removing a node takes a failure
// domain out of the cluster for good, and doing two at once can leave
// erasure-coded objects unrecoverable. Nodes leave highest-index-first within
// the affected rack, which is the reverse of the order they were created in.
type decommission struct {
	// node is the one being removed now, and set its StatefulSet as it runs
	// today — the base the drained one is built from, so taking a node away
	// never quietly reshapes where it runs.
	node Node
	set  *appsv1.StatefulSet

	// queued is every node the spec no longer declares, in removal order. The
	// first is node; the rest wait their turn.
	queued []string
}

// active reports whether a node is being decommissioned.
func (d decommission) active() bool {
	return d.set != nil
}

// draining reports whether name is the node being decommissioned.
func (d decommission) draining(name string) bool {
	return d.active() && d.node.Name == name
}

// planDecommission finds the nodes the spec no longer declares and picks the
// one to remove now, so the render step can take it out of placement.
//
// It runs before render because a decommissioning node has to stay in the pass:
// it keeps its StatefulSet and its config while it drains, and the health and
// rollout gates have to count it. Dropping it from the pass the moment the spec
// stopped naming it is what would let the operator delete a node still holding
// data.
func (r *Reconciler) planDecommission(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	existing, err := r.nodeSets(ctx, p.cluster)
	if err != nil {
		return pipeline.Outcome{}, err
	}

	declared := make(map[string]bool, len(p.nodes))
	for _, node := range p.nodes {
		declared[node.Name] = true
	}

	var removed []*appsv1.StatefulSet

	for i := range existing {
		if !declared[existing[i].Name] {
			removed = append(removed, &existing[i])
		}
	}

	if len(removed) == 0 {
		return pipeline.Continue()
	}

	slices.SortFunc(removed, removalOrder)

	queued := make([]string, 0, len(removed))
	for _, set := range removed {
		queued = append(queued, set.Name)
	}

	target := removed[0]

	p.decommission = decommission{
		node:   nodeFromSet(target),
		set:    target,
		queued: queued,
	}

	// The node stays part of the cluster until it is empty, so every step that
	// walks the node set — health, the rollout's one-at-a-time gate, the
	// disruption budget — keeps counting it.
	p.nodes = append(p.nodes, p.decommission.node)

	// Announce the decommission once, when it starts, rather than on every
	// pass: the waiting is reported by the Draining phase and its condition.
	if update := p.object.Status.Update; update == nil ||
		update.Phase != fsv1alpha1.UpdatePhaseDraining || update.Node != target.Name {
		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventNodeDraining,
			"Decommissioning node %q: draining it out of placement before removal (%d node(s) queued)",
			target.Name, len(queued))
	}

	return pipeline.Continue()
}

// removalOrder sorts nodes into the order they are decommissioned: rack by
// rack, and within a rack the highest index first, undoing the order they were
// created in.
func removalOrder(a, b *appsv1.StatefulSet) int {
	if rack := strings.Compare(a.Labels[LabelRack], b.Labels[LabelRack]); rack != 0 {
		return rack
	}

	ai, aok := nodeIndex(a.Name)
	bi, bok := nodeIndex(b.Name)

	if aok && bok && ai != bi {
		// Descending: the last node added is the first to leave.
		return bi - ai
	}

	return strings.Compare(b.Name, a.Name)
}

// nodeIndex is the ordinal a node's name ends in, which is how the topology
// numbers nodes within a rack.
func nodeIndex(name string) (int, bool) {
	cut := strings.LastIndex(name, "-")
	if cut < 0 {
		return 0, false
	}

	index, err := strconv.Atoi(name[cut+1:])
	if err != nil {
		return 0, false
	}

	return index, true
}

// nodeFromSet recovers the identity of a node the spec no longer declares.
//
// Only what the node's *configuration* needs is recovered — its fs node ID and
// its rack. Where the node runs (zone, selectors, affinity, its volumes) is
// deliberately not: that is taken from the StatefulSet as it exists, because a
// node being removed must keep running exactly where it already is. Rebuilding
// its placement from a spec that no longer describes it is how a drain would
// move the pod away from its own data.
func nodeFromSet(set *appsv1.StatefulSet) Node {
	node := Node{Name: set.Name, Rack: set.Labels[LabelRack]}
	if index, ok := nodeIndex(set.Name); ok {
		node.Index = index
	}

	return node
}

// drainedStatefulSet is the decommissioning node's StatefulSet as it should run
// while it drains: the live one, restamped so the pod restarts onto the drained
// configuration.
//
// It is a copy of what is running rather than a fresh build, so a decommission
// changes exactly one thing — the configuration the node comes back with — and
// nothing about where it runs or what it mounts.
func drainedStatefulSet(live *appsv1.StatefulSet, restartRevision string) (*appsv1.StatefulSet, error) {
	set := live.DeepCopy()

	set.Spec.Template.Annotations = withAnnotation(
		set.Spec.Template.Annotations, AnnotationRestartRevision, restartRevision)

	// A typed read strips the kind, which server-side apply requires.
	set.TypeMeta = metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "StatefulSet"}

	// Apply owns these; a resourceVersion carried over from a read would make
	// the apply a conflict, and the managed fields are the server's to keep.
	set.ResourceVersion = ""
	set.ManagedFields = nil
	set.Status = appsv1.StatefulSetStatus{}

	if err := stampTemplateRevision(set); err != nil {
		return nil, err
	}

	return set, nil
}

// withAnnotation sets one annotation, allocating the map when there is none.
func withAnnotation(annotations map[string]string, key, value string) map[string]string {
	if annotations == nil {
		return map[string]string{key: value}
	}

	annotations[key] = value

	return annotations
}

// reconcileDecommission removes a decommissioning node once its data is gone.
//
// The gates, in the order they must hold (SPEC §8.4): the node has to be
// running the drained configuration (until it restarts, its disks are still
// taking writes), every node has to be serving and the cluster reconverged (the
// rebalancer is what moves the data, and it cannot finish while the cluster is
// unsettled), the whole cluster has to be reporting, and every one of the
// node's disks has to report itself empty.
//
// Until all of that holds it waits. A decommission that stalls is an
// inconvenience; one that deletes a node still holding the only copy of
// something is not, so every unknown resolves to "wait".
func (r *Reconciler) reconcileDecommission(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	if !p.decommission.active() {
		return pipeline.Continue()
	}

	name := p.decommission.node.Name

	live, running := p.live[name]
	if !running {
		// Already gone: a previous pass removed it.
		return pipeline.Continue()
	}

	// Still coming back on the drained configuration. The rollout step is what
	// restarts it; here we only wait for it to land.
	//
	// Compared against the *desired* set, never against the one the drain
	// started from — that one carries the revision the node is already running,
	// so it would match itself and wave the node straight through to removal
	// while its disks were still taking writes.
	desired, ok := p.desiredSet(name)
	if !ok {
		return pipeline.Continue()
	}

	if live.Annotations[AnnotationTemplateRevision] != desired.Annotations[AnnotationTemplateRevision] ||
		!nodeServing(live) {
		return r.holdDrain(p, fmt.Sprintf(
			"node %q is restarting onto the drained configuration", name))
	}

	if !p.convergence.known {
		return r.holdDrain(p, fmt.Sprintf(
			"node %q is drained, but no node answered: occupancy is unknown", name))
	}

	if !p.convergence.converged {
		return r.holdDrain(p, fmt.Sprintf(
			"waiting for the cluster to move node %q's data off (repair queue / rebalance)", name))
	}

	// A silent node makes the view partial, and a partial view is not evidence
	// that a disk is empty.
	if !p.convergence.allReporting() {
		return r.holdDrain(p, fmt.Sprintf(
			"%d node(s) are not reporting, so node %q's occupancy cannot be confirmed",
			p.convergence.nodesNotReporting, name))
	}

	reported, ok := p.convergence.nodes[name]
	if !ok {
		return r.holdDrain(p, fmt.Sprintf(
			"node %q is not in the cluster's own view yet", name))
	}

	if !reported.Empty() {
		return r.holdDrain(p, drainProgress(name, reported))
	}

	r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventNodeDrained,
		"Node %q holds no data; removing it", name)

	if err := r.removeNode(ctx, p, live); err != nil {
		return pipeline.Outcome{}, err
	}

	r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventNodeRemoved,
		"Removed node %q; %d node(s) still to decommission", name, len(p.decommission.queued)-1)

	// The next node's drain starts on the next pass, once this removal has
	// settled and the cluster has reconverged without it.
	return pipeline.RequeueAfter(pollInterval, "node removed; waiting for the cluster to reconverge")
}

// drainProgress says how far a drain has got, in the terms a reader can act on.
// Bytes are progress, not the test: they come from statfs, so an emptied disk
// still reports some in use (SPEC §8.4).
func drainProgress(name string, reported fsclient.ClusterNode) string {
	var pending []string

	for _, disk := range reported.Disks {
		if disk.Empty() {
			continue
		}

		if disk.DataError != "" {
			pending = append(pending, fmt.Sprintf("%s (unreadable: %s)", disk.ID, disk.DataError))

			continue
		}

		if !disk.OccupancyKnown {
			pending = append(pending, fmt.Sprintf("%s (occupancy not reported)", disk.ID))

			continue
		}

		pending = append(pending, disk.ID)
	}

	message := fmt.Sprintf("node %q still holds data on disk(s) %v", name, pending)

	if used, known := reported.UsedBytes(); known {
		message += fmt.Sprintf(" (%s in use)", humanBytes(used))
	}

	return message
}

// humanBytes renders a byte count for an event or condition message.
func humanBytes(b int64) string {
	const unit = 1024

	if b < unit {
		return fmt.Sprintf("%dB", b)
	}

	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// holdDrain keeps a decommission where it is and says what it is waiting for.
// Like the rollout's hold it never forces anything: past the convergence
// timeout it says so loudly and keeps waiting.
func (r *Reconciler) holdDrain(p *pass, reason string) (pipeline.Outcome, error) {
	p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
		fsv1alpha1.ReasonDraining, reason)

	return r.hold(p, fsv1alpha1.UpdatePhaseDraining, p.decommission.node.Name, reason)
}

// removeNode deletes a decommissioned node's StatefulSet and its configuration.
//
// The StatefulSet's claim retention policy carries the spec's
// storage.reclaimPolicy, so its volumes are kept or deleted by that policy
// rather than by anything here (SPEC §8.4 step 3). Everything else the node
// owns is garbage-collected through its ownerRefs.
func (r *Reconciler) removeNode(ctx context.Context, p *pass, set *appsv1.StatefulSet) error {
	if err := r.Delete(ctx, set); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "delete statefulset %q", set.Name)
	}

	config := &corev1.Secret{}
	config.Name = ConfigSecretName(p.decommission.node.Name)
	config.Namespace = p.cluster.Namespace

	if err := r.Delete(ctx, config); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "delete config secret %q", config.Name)
	}

	return nil
}
