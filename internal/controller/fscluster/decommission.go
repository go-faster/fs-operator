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
	"strings"

	"github.com/go-faster/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
)

// Event reasons a decommission reports. Like the rollout's, they are API
// surface: an operator watching events keys off them.
const (
	eventNodeDraining = "NodeDraining"
	eventNodeRemoved  = "NodeRemoved"
)

// decommission is the nodes the spec no longer declares, and the StatefulSets
// they run on today (SPEC §8.4).
//
// They all leave at once. The layout step applies a layout without them, fs
// moves their data to the nodes that remain while the old layout version keeps
// serving reads, and only when that transition is complete are they deleted.
// Removing one at a time would move data more than once for nothing: the
// transition already guarantees nothing acknowledged is lost on the way.
type decommission struct {
	// sets is every removed node's StatefulSet as it runs, keyed by name —
	// the base its kept StatefulSet is built from, so removing a node never
	// quietly reshapes where it runs.
	sets map[string]*appsv1.StatefulSet
}

// active reports whether any node is being removed.
func (d decommission) active() bool {
	return len(d.sets) > 0
}

// names lists the nodes being removed, sorted.
func (d decommission) names() []string {
	names := make([]string, 0, len(d.sets))
	for name := range d.sets {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// planDecommission finds the nodes the spec no longer declares, so that the
// render keeps them running and the layout step leaves them out.
//
// It runs before render because a node being removed has to stay in the pass:
// it keeps its StatefulSet and its config while its data moves, and the health
// and rollout gates have to count it. Dropping it from the pass the moment the
// spec stopped naming it is what would let the operator delete a node still
// holding data.
func (r *Reconciler) planDecommission(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	existing, err := r.nodeSets(ctx, p.cluster)
	if err != nil {
		return pipeline.Outcome{}, err
	}

	declared := make(map[string]bool, len(p.nodes))
	for _, node := range p.nodes {
		declared[node.Name] = true
	}

	for i := range existing {
		set := &existing[i]
		if declared[set.Name] {
			continue
		}

		if p.decommission.sets == nil {
			p.decommission.sets = map[string]*appsv1.StatefulSet{}
		}

		p.decommission.sets[set.Name] = set
		p.nodes = append(p.nodes, nodeFromSet(set))
	}

	if !p.decommission.active() {
		return pipeline.Continue()
	}

	// Announce the removal once, when it starts, rather than on every pass:
	// the waiting is reported by the Draining phase and its condition.
	if update := p.object.Status.Update; update == nil || update.Phase != fsv1alpha1.UpdatePhaseDraining {
		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventNodeDraining,
			"Removing node(s) %s: leaving the layout, kept running until their data has moved",
			strings.Join(p.decommission.names(), ", "))
	}

	return pipeline.Continue()
}

// nodeFromSet recovers the identity of a node the spec no longer declares.
//
// Only what the node's configuration needs is recovered — its fs node ID and
// its rack. Where the node runs (zone, selectors, affinity, its volume) is
// deliberately not: that is taken from the StatefulSet as it exists, because a
// node being removed must keep running exactly where it already is.
func nodeFromSet(set *appsv1.StatefulSet) Node {
	return Node{Name: set.Name, Rack: set.Labels[LabelRack]}
}

// keptStatefulSet is a removed node's StatefulSet as it should run while its
// data moves: the live one, unchanged. It is a copy of what is running rather
// than a fresh build, so removing a node changes nothing about where it runs or
// what it mounts.
func keptStatefulSet(live *appsv1.StatefulSet) (*appsv1.StatefulSet, error) {
	set := live.DeepCopy()

	// A typed read strips the kind, which server-side apply requires.
	set.TypeMeta = metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: KindStatefulSet}

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

// reconcileDecommission deletes removed nodes once the cluster no longer needs
// them.
//
// The gates (SPEC §8.4): the layout the cluster runs must not give the node a
// share, and no older version that did may still be retained — a retained
// version is one fs still reads from, and its replicas include the node. Until
// both hold it waits. A removal that stalls is an inconvenience; one that
// deletes a node still holding the only copy of something is not, so every
// unknown resolves to "wait".
func (r *Reconciler) reconcileDecommission(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	if !p.decommission.active() {
		return pipeline.Continue()
	}

	names := p.decommission.names()
	layout := p.convergence.layout

	switch {
	case !p.convergence.known || layout == nil:
		return r.holdDrain(p, names, "no node answered, so whether their data has moved is unknown")
	case layout.Transitioning():
		return r.holdDrain(p, names, fmt.Sprintf(
			"layout version %d is moving their data; versions %v are still retained",
			layout.Version, layout.Retained))
	}

	for _, name := range names {
		if _, ok := layout.Member(name); ok {
			return r.holdDrain(p, names, fmt.Sprintf(
				"node %q is still in layout version %d; it leaves with the next layout change",
				name, layout.Version))
		}
	}

	for _, name := range names {
		if err := r.removeNode(ctx, p, p.decommission.sets[name]); err != nil {
			return pipeline.Outcome{}, err
		}

		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventNodeRemoved,
			"Removed node %q: layout version %d holds its data elsewhere", name, layout.Version)
	}

	return pipeline.Continue()
}

// holdDrain keeps removed nodes running and says what they are waiting for.
// Like the rollout's hold it never forces anything: past the convergence
// timeout it says so loudly and keeps waiting.
func (r *Reconciler) holdDrain(p *pass, names []string, reason string) (pipeline.Outcome, error) {
	p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
		fsv1alpha1.ReasonDraining, reason)

	return r.hold(p, fsv1alpha1.UpdatePhaseDraining, nodeSubject(strings.Join(names, ", ")), reason)
}

// removeNode deletes a removed node's StatefulSet and its configuration.
//
// The StatefulSet's claim retention policy carries the spec's
// storage.reclaimPolicy, so its volume is kept or deleted by that policy
// rather than by anything here (SPEC §8.4 step 3).
func (r *Reconciler) removeNode(ctx context.Context, p *pass, set *appsv1.StatefulSet) error {
	if err := r.Delete(ctx, set); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "delete statefulset %q", set.Name)
	}

	config := &corev1.Secret{}
	config.Name = ConfigSecretName(set.Name)
	config.Namespace = p.cluster.Namespace

	if err := r.Delete(ctx, config); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "delete config secret %q", config.Name)
	}

	return nil
}
