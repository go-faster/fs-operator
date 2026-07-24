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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller"
)

// pollInterval is how often a rollout looks again while it waits for a node.
// Pod readiness arrives as a watch event, so this is only a safety net for the
// transitions no watch reports.
const pollInterval = 15 * time.Second

// Event reasons a rollout reports. They are API surface: an operator watching
// events keys off them.
const (
	eventNodeRolling   = "NodeRolling"
	eventRolloutWait   = "RolloutWaiting"
	eventRolloutStuck  = "RolloutStuck"
	eventRolloutDone   = "RolloutComplete"
	eventNodesCreating = "NodesCreating"
)

// reconcileNodes brings the node StatefulSets to the desired state, one at a
// time where that matters.
//
// The two halves are not symmetric, and that is the whole point. Adding nodes
// is additive: they join, the rebalancer converges, and several may come up at
// once. Replacing an existing node's pod takes a failure domain out of the
// cluster, so it happens strictly one node at a time, and only while every
// other node is serving — fs's upgrade contract, encoded (SPEC §8.2).
func (r *Reconciler) reconcileNodes(ctx context.Context, p *pass) (controller.Outcome, error) {
	created, stale, err := r.classify(ctx, p)
	if err != nil {
		return controller.Outcome{}, err
	}

	if created > 0 {
		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventNodesCreating,
			"Creating %d node(s); they join the cluster and the rebalancer converges", created)
	}

	return r.roll(ctx, p, stale)
}

// classify applies the nodes that do not exist yet and returns the existing
// ones whose pod template has changed, in the order they should be rolled.
func (r *Reconciler) classify(ctx context.Context, p *pass) (int, []*appsv1.StatefulSet, error) {
	var (
		created int
		stale   []*appsv1.StatefulSet
	)

	for i, set := range p.desired {
		live, running := p.live[set.Name]

		switch {
		case !running:
			if err := r.apply(ctx, p.cluster, set); err != nil {
				return 0, nil, err
			}

			created++
		case live.Annotations[AnnotationTemplateRevision] != set.Annotations[AnnotationTemplateRevision]:
			stale = append(stale, p.desired[i])
		}
	}

	return created, rollOrder(stale, p.nodes), nil
}

// roll replaces at most one node per pass, and only while the rest of the
// cluster is serving.
func (r *Reconciler) roll(ctx context.Context, p *pass, stale []*appsv1.StatefulSet) (controller.Outcome, error) {
	if len(stale) == 0 {
		if p.object.Status.Update != nil {
			r.Recorder.Event(p.object, corev1.EventTypeNormal, eventRolloutDone,
				"Every node runs the desired configuration")
		}

		return controller.Continue()
	}

	// The phase a hold reports: a rollout that has already touched a node is
	// still RollingNodes; one that has not is in Preflight.
	phase, holdNode := fsv1alpha1.UpdatePhasePreflight, ""
	if update := p.object.Status.Update; update != nil && update.Phase == fsv1alpha1.UpdatePhaseRollingNodes {
		phase, holdNode = update.Phase, update.Node
	}

	// A node whose pod is not ready is a failure domain already missing.
	// Taking a second one down is what the upgrade contract forbids.
	if waiting := p.health.notReady; len(waiting) > 0 {
		return r.hold(p, phase, holdNode, fmt.Sprintf(
			"waiting for node(s) %v to become ready before replacing another", waiting))
	}

	// Every pod is up, but the cluster must also have reconverged — the repair
	// queue drained and no rebalance moving data — before another node's
	// failure domain is taken down (SPEC §8.2). Unknown convergence (no node
	// answered) holds too, rather than proceed blind.
	if !p.convergence.known || !p.convergence.converged {
		return r.hold(p, phase, holdNode,
			"waiting for the cluster to reconverge (repair queue / rebalance) before replacing another node")
	}

	next := stale[0]

	if err := r.apply(ctx, p.cluster, next); err != nil {
		return controller.Outcome{}, err
	}

	r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventNodeRolling,
		"Replacing node %q; %d node(s) still to roll after it", next.Name, len(stale)-1)

	p.update = &fsv1alpha1.UpdateStatus{
		Phase:     fsv1alpha1.UpdatePhaseRollingNodes,
		Node:      next.Name,
		StartedAt: ptrTime(metav1.Now()),
	}

	return controller.RequeueAfter(pollInterval, "node replacement in flight")
}

// hold keeps a rollout where it is and says what it is waiting for.
//
// It never gives up and never forces anything: past the convergence timeout it
// reports the wait as stuck, loudly, and keeps waiting — the cluster resumes on
// its own the moment the gate opens (SPEC §8.2).
func (r *Reconciler) hold(p *pass, phase fsv1alpha1.UpdatePhase, node, reason string) (controller.Outcome, error) {
	started := metav1.Now()
	if update := p.object.Status.Update; update != nil && update.Phase == phase && update.Node == node {
		if update.StartedAt != nil {
			started = *update.StartedAt
		}
	}

	p.update = &fsv1alpha1.UpdateStatus{Phase: phase, Node: node, StartedAt: ptrTime(started)}

	timeout := p.cluster.Spec.UpdatePolicy.ConvergenceTimeout
	if timeout != nil && time.Since(started.Time) > timeout.Duration {
		p.setCondition(fsv1alpha1.ConditionConverged, metav1.ConditionFalse,
			fsv1alpha1.ReasonConvergenceTimeout,
			fmt.Sprintf("Rollout has been %s for over %s: %s", phase, timeout.Duration, reason))
		r.Recorder.Eventf(p.object, corev1.EventTypeWarning, eventRolloutStuck,
			"Rollout halted for over %s: %s", timeout.Duration, reason)

		return controller.RequeueAfter(pollInterval, reason)
	}

	r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventRolloutWait, "Rollout %s: %s", phase, reason)

	return controller.RequeueAfter(pollInterval, reason)
}

// rollOrder interleaves the racks of the nodes to roll, so that two nodes of
// one failure domain are never replaced back to back. Within a rack the
// declared order is kept, which makes a rollout reproducible.
func rollOrder(stale []*appsv1.StatefulSet, nodes []Node) []*appsv1.StatefulSet {
	if len(stale) < 2 {
		return stale
	}

	rackOf := make(map[string]string, len(nodes))
	for _, node := range nodes {
		rackOf[node.Name] = domainOf(node)
	}

	var (
		racks  []string
		queues = make(map[string][]*appsv1.StatefulSet)
	)

	for _, set := range stale {
		rack := rackOf[set.Name]

		if _, seen := queues[rack]; !seen {
			racks = append(racks, rack)
		}

		queues[rack] = append(queues[rack], set)
	}

	ordered := make([]*appsv1.StatefulSet, 0, len(stale))

	for len(ordered) < len(stale) {
		for _, rack := range racks {
			queue := queues[rack]
			if len(queue) == 0 {
				continue
			}

			ordered = append(ordered, queue[0])
			queues[rack] = queue[1:]
		}
	}

	return ordered
}

// stampTemplateRevision records the fingerprint of a node's desired pod
// template on its StatefulSet, which is how the next pass tells a node that
// needs replacing from one that is already current.
func stampTemplateRevision(set *appsv1.StatefulSet) error {
	revision, err := TemplateRevision(set)
	if err != nil {
		return err
	}

	if set.Annotations == nil {
		set.Annotations = map[string]string{}
	}

	set.Annotations[AnnotationTemplateRevision] = revision

	return nil
}

// ptrTime is metav1.Time as the API's optional timestamp.
func ptrTime(t metav1.Time) *metav1.Time {
	return &t
}
