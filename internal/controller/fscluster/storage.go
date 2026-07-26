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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
)

// eventStorageExpanding marks a node's storage being grown.
const eventStorageExpanding = "StorageExpanding"

// reconcileStorage brings a node's disks to the declared sizes and set (SPEC
// §8.5). A StatefulSet's volumeClaimTemplates are immutable, so growing a disk
// or adding one cannot be a plain apply: the operator expands the live PVCs and
// then orphan-deletes the StatefulSet, leaving the pod and its data running, so
// the next pass recreates it with the new templates and re-adopts the pod.
//
// A shrink is refused outright — fs has no way to reclaim data off a smaller
// volume, and Kubernetes cannot shrink a PVC.
func (r *Reconciler) reconcileStorage(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	// An orphan-recreate leaves the previous pod adopted by the new
	// StatefulSet, and finishing that is this step's business before anything
	// else (see replaceAdoptedPod).
	if outcome, done, err := r.finishAdoption(ctx, p); done || err != nil {
		return outcome, err
	}

	var drifted []int

	for i, node := range p.nodes {
		live, ok := p.live[node.Name]
		if !ok {
			// Not created yet; the nodes step creates it with the right disks.
			continue
		}

		shrunk, changed := diskDiff(live.Spec.VolumeClaimTemplates, p.desired[i].Spec.VolumeClaimTemplates)
		if len(shrunk) > 0 {
			return r.refuse(p, fsv1alpha1.ReasonDiskShrinkForbidden, fmt.Sprintf(
				"node %q disk(s) %v would shrink; disks may only grow", node.Name, shrunk))
		}

		if changed {
			drifted = append(drifted, i)
		}
	}

	if len(drifted) == 0 {
		return pipeline.Continue()
	}

	// Storage surgery replaces a node's StatefulSet, so do it one node at a
	// time and only while the cluster is healthy and converged — the same
	// contract as a rollout.
	if waiting := p.health.notReady; len(waiting) > 0 {
		return pipeline.RequeueAfter(pollInterval,
			fmt.Sprintf("waiting for node(s) %v before storage changes", waiting))
	}

	if !p.convergence.known || !p.convergence.converged {
		return pipeline.RequeueAfter(pollInterval,
			"waiting for the cluster to reconverge before storage changes")
	}

	return r.expandNode(ctx, p, p.nodes[drifted[0]], p.desired[drifted[0]])
}

// finishAdoption replaces a pod the orphan-recreate left behind, and reports
// whether it acted.
//
// The StatefulSet controller re-adopts the orphaned pod but does not restamp
// it: the pod keeps the previous revision's controller-revision-hash, so the
// set reports updatedReplicas 0 for a pod that is running and ready. The
// operator reads that as "not serving" (nodeServing), so the node never counts
// as healthy again and every later storage change and rollout blocks behind
// it — permanently, because nothing else ever replaces that pod.
//
// It also has to go for a plainer reason: a running pod cannot gain or lose a
// volume mount, so a disk added or removed only takes effect when the pod is
// replaced. SPEC §8.5 says "orphan-recreate the StatefulSet and roll the
// node"; this is the roll.
func (r *Reconciler) finishAdoption(ctx context.Context, p *pass) (pipeline.Outcome, bool, error) {
	for _, node := range p.nodes {
		live, running := p.live[node.Name]
		if !running || !adoptedStalePod(live) {
			continue
		}

		// Replacing this pod takes a serving node down, so hold to the same
		// contract as any other roll: every other node must be up first.
		if waiting := otherNodesNotReady(p, node.Name); len(waiting) > 0 {
			outcome, err := pipeline.RequeueAfter(pollInterval, fmt.Sprintf(
				"waiting for node(s) %v before replacing node %q's pod", waiting, node.Name))

			return outcome, true, err
		}

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: PodName(node.Name), Namespace: p.cluster.Namespace,
		}}

		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return pipeline.Outcome{}, true, errors.Wrapf(err, "replace pod of node %q", node.Name)
		}

		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventStorageExpanding,
			"Replacing node %q's pod so it picks up its new storage", node.Name)

		outcome, err := pipeline.RequeueAfter(pollInterval,
			fmt.Sprintf("replacing node %q's pod after recreating its StatefulSet", node.Name))

		return outcome, true, err
	}

	return pipeline.Outcome{}, false, nil
}

// adoptedStalePod reports whether a StatefulSet's single pod is running but
// left on a previous revision — the state an orphan-recreate produces.
func adoptedStalePod(set *appsv1.StatefulSet) bool {
	return set.Status.ObservedGeneration >= set.Generation &&
		set.Status.ReadyReplicas == 1 &&
		set.Status.UpdatedReplicas == 0
}

// otherNodesNotReady names the nodes other than one that are not serving.
func otherNodesNotReady(p *pass, except string) []string {
	var waiting []string

	for _, name := range p.health.notReady {
		if name != except {
			waiting = append(waiting, name)
		}
	}

	return waiting
}

// expandNode grows one node's PVCs and orphan-deletes its StatefulSet so the
// next pass recreates it with the new claim templates.
func (r *Reconciler) expandNode(ctx context.Context, p *pass, node Node, desired *appsv1.StatefulSet) (pipeline.Outcome, error) {
	if err := r.growClaims(ctx, p, node, desired); err != nil {
		return pipeline.Outcome{}, err
	}

	// Orphan-delete: the pod and PVCs stay, so the node keeps serving; the next
	// pass sees the StatefulSet gone and recreates it, re-adopting the pod.
	orphan := metav1.DeletePropagationOrphan
	if err := r.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: node.Name, Namespace: p.cluster.Namespace,
	}}, &client.DeleteOptions{PropagationPolicy: &orphan}); err != nil && !apierrors.IsNotFound(err) {
		return pipeline.Outcome{}, errors.Wrapf(err, "orphan-delete statefulset %q", node.Name)
	}

	r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventStorageExpanding,
		"Applying storage changes to node %q (recreating its StatefulSet, then its pod)", node.Name)

	p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
		fsv1alpha1.ReasonStorageExpanding, fmt.Sprintf("Applying storage changes to node %q", node.Name))

	return pipeline.RequeueAfter(pollInterval, "recreating node with new storage")
}

// growClaims expands a node's PVCs to the declared sizes. Shrinks are already
// refused; a claim already at or above the target is left alone.
func (r *Reconciler) growClaims(ctx context.Context, p *pass, node Node, desired *appsv1.StatefulSet) error {
	for _, tmpl := range desired.Spec.VolumeClaimTemplates {
		want := tmpl.Spec.Resources.Requests[corev1.ResourceStorage]

		name := PVCName(tmpl.Name, node.Name)

		var pvc corev1.PersistentVolumeClaim
		if err := r.Get(ctx, types.NamespacedName{Namespace: p.cluster.Namespace, Name: name}, &pvc); err != nil {
			if apierrors.IsNotFound(err) {
				// A disk being added has no PVC yet; the recreated StatefulSet
				// provisions it when the pod is next created.
				continue
			}

			return errors.Wrapf(err, "get pvc %q", name)
		}

		have := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if have.Cmp(want) >= 0 {
			continue
		}

		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
		if err := r.Update(ctx, &pvc); err != nil {
			return errors.Wrapf(err, "expand pvc %q", name)
		}
	}

	return nil
}

// PVCName is the name of a disk's PersistentVolumeClaim on a node: the disk
// (claim template) name, the node's pod, ordinal 0.
func PVCName(disk, node string) string {
	return fmt.Sprintf("%s-%s", disk, PodName(node))
}

// diskDiff compares live and desired claim templates. It returns the names of
// disks that would shrink, and whether any disk grew or was added (a claim
// template change that a plain apply cannot make).
func diskDiff(live, desired []corev1.PersistentVolumeClaim) (shrunk []string, changed bool) {
	sizes := make(map[string]resource.Quantity, len(live))
	for _, claim := range live {
		sizes[claim.Name] = claim.Spec.Resources.Requests[corev1.ResourceStorage]
	}

	for _, claim := range desired {
		want := claim.Spec.Resources.Requests[corev1.ResourceStorage]

		have, ok := sizes[claim.Name]
		if !ok {
			// A disk not on the live set is an addition.
			changed = true

			continue
		}

		switch have.Cmp(want) {
		case 1:
			shrunk = append(shrunk, claim.Name)
		case -1:
			changed = true
		}
	}

	return shrunk, changed
}
