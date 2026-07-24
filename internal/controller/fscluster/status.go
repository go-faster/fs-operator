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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller"
)

// health is what one pass observed of the running cluster: pod state from
// Kubernetes (filled by observe) and applied configuration from the admin API
// (filled by the reload step).
type health struct {
	// ready is the number of nodes whose pod is up and current.
	ready int32

	// readyDomains is how many distinct failure domains have a ready node.
	// It, not the node count, is what decides whether writes can be
	// acknowledged.
	readyDomains int

	// current is the number of ready nodes already running the desired
	// StatefulSet (pod template).
	current int32

	// configCurrent is the number of nodes the admin API confirms are running
	// the desired configuration revision. It drives ConfigurationInSync; a
	// mid-roll or not-yet-reloaded node is not counted until it reports the
	// target (SPEC §8.3). Set by the reload step, so it is meaningful only
	// once that step has run.
	configCurrent int32

	// notReady names the declared nodes that are not serving, in declaration
	// order. A rollout will not touch a second node while it is non-empty.
	notReady []string
}

// observe reads the cluster's own StatefulSets and matches them against the
// node set. It runs even when the pass was refused: a spec the operator will
// not apply says nothing about the cluster that is already running.
func (r *Reconciler) observe(ctx context.Context, p *pass) (controller.Outcome, error) {
	sets, err := r.nodeSets(ctx, p.cluster)
	if err != nil {
		return controller.Outcome{}, err
	}

	p.live = make(map[string]*appsv1.StatefulSet, len(sets))
	for i := range sets {
		p.live[sets[i].Name] = &sets[i]
	}

	desired := make(map[string]string, len(p.desired))
	for _, set := range p.desired {
		desired[set.Name] = set.Annotations[AnnotationTemplateRevision]
	}

	domains := make(map[string]bool)

	for _, node := range p.nodes {
		set, running := p.live[node.Name]
		if !running || !nodeServing(set) {
			p.health.notReady = append(p.health.notReady, node.Name)

			continue
		}

		p.health.ready++
		domains[domainOf(node)] = true

		if revision, ok := desired[node.Name]; ok && set.Annotations[AnnotationTemplateRevision] == revision {
			p.health.current++
		}
	}

	p.health.readyDomains = len(domains)

	return controller.Continue()
}

// nodeServing reports whether a node's single pod is up and is the one the
// current template describes.
//
// It reads the StatefulSet's own status rather than the pod's: the workload
// controller already tracks which revision a pod belongs to, and trusting it
// keeps the operator from racing the machinery it delegates pod replacement
// to. A stale observedGeneration means the answer is about the previous
// template, so it does not count.
func nodeServing(set *appsv1.StatefulSet) bool {
	return set.Status.ObservedGeneration >= set.Generation &&
		set.Status.ReadyReplicas == 1 &&
		set.Status.UpdatedReplicas == 1
}

// domainOf is the failure domain a node belongs to. In the flat topology every
// node is its own domain, which is what fs does with an empty rack.
func domainOf(node Node) string {
	if node.Rack == "" {
		return node.Name
	}

	return node.Rack
}

// reconcileStatus writes what the pass observed and decided.
//
// It is the always-run step: a pass that refused a spec or failed halfway
// still has to say so, and this is where that reaches the object.
func (r *Reconciler) reconcileStatus(ctx context.Context, p *pass) (controller.Outcome, error) {
	base := p.object.DeepCopy()
	status := &p.object.Status

	status.ObservedGeneration = p.object.Generation
	status.Nodes = int32(len(p.nodes)) //nolint:gosec // the topology is bounded at 16 nodes
	status.ReadyNodes = p.health.ready
	status.Update = p.update
	status.SchemaVersion = p.schema
	status.Endpoints = &fsv1alpha1.EndpointsStatus{
		S3: S3Endpoint(p.cluster.Name, p.cluster.Namespace,
			p.cluster.Spec.S3.Service.Port, p.cluster.Spec.S3.TLS.SecretName != ""),
	}

	if p.configs != nil {
		status.ConfigurationRevision = ConfigRevision(p.configs)
		status.StatefulSetRevision = p.templateRevision
		status.UpdateRevision = status.ConfigurationRevision

		if p.health.current == status.Nodes {
			status.CurrentRevision = status.ConfigurationRevision
		}
	}

	r.summarize(p)

	for _, condition := range p.conditions {
		meta.SetStatusCondition(&status.Conditions, condition)
	}

	if err := r.Status().Patch(ctx, p.object, client.MergeFrom(base)); err != nil {
		return controller.Outcome{}, errors.Wrap(err, "update status")
	}

	return controller.Continue()
}

// summarize turns the observation into the conditions a user reads.
func (r *Reconciler) summarize(p *pass) {
	if p.failure != nil {
		p.setCondition(fsv1alpha1.ConditionReconcileSucceeded, metav1.ConditionFalse,
			fsv1alpha1.ReasonReconcileError, p.failure.Error())
	} else {
		p.setCondition(fsv1alpha1.ConditionReconcileSucceeded, metav1.ConditionTrue,
			fsv1alpha1.ReasonReconcileFinished, "Reconcile pass completed")
	}

	desired := p.object.Status.Nodes

	if len(p.health.notReady) == 0 {
		p.setCondition(fsv1alpha1.ConditionNodesHealthy, metav1.ConditionTrue,
			fsv1alpha1.ReasonAllNodesReady, fmt.Sprintf("All %d nodes are ready", desired))
	} else {
		p.setCondition(fsv1alpha1.ConditionNodesHealthy, metav1.ConditionFalse,
			fsv1alpha1.ReasonNodesNotReady,
			fmt.Sprintf("%d of %d nodes are ready; waiting for %v",
				p.health.ready, desired, p.health.notReady))
	}

	r.summarizeReadiness(p)
	r.summarizeAlignment(p)
	r.summarizeConvergence(p)
}

// summarizeConvergence reports the Converged condition from what the admin API
// observed, unless the rollout already set it (its convergence-timeout
// condition, raised while a roll is stuck, is more specific and wins).
func (r *Reconciler) summarizeConvergence(p *pass) {
	if p.hasCondition(fsv1alpha1.ConditionConverged) || !p.convergence.known {
		return
	}

	switch {
	case p.convergence.converged:
		p.setCondition(fsv1alpha1.ConditionConverged, metav1.ConditionTrue,
			fsv1alpha1.ReasonConverged, "Repair queue empty and placement settled")
	case p.convergence.repairQueue > 0:
		p.setCondition(fsv1alpha1.ConditionConverged, metav1.ConditionFalse,
			fsv1alpha1.ReasonRepairQueueBacklog,
			fmt.Sprintf("%d repair tasks pending across the cluster", p.convergence.repairQueue))
	default:
		p.setCondition(fsv1alpha1.ConditionConverged, metav1.ConditionFalse,
			fsv1alpha1.ReasonRebalancing, "A rebalance is moving data")
	}
}

// summarizeReadiness reports whether the cluster can acknowledge a write.
//
// fs acknowledges a write once its synchronous quorum is durable on distinct
// failure domains — two full replicas for the replicated schemes, all k+m
// shards for erasure coding — so the question is how many domains are serving,
// not how many pods are up.
func (r *Reconciler) summarizeReadiness(p *pass) {
	scheme, err := ParseScheme(p.cluster.Spec.Scheme)
	if err != nil {
		// An unparseable scheme was already refused by validation.
		return
	}

	quorum := scheme.WriteQuorumDomains()
	message := fmt.Sprintf("%d failure domains are serving, %d needed to acknowledge a write",
		p.health.readyDomains, quorum)

	if p.health.readyDomains >= quorum {
		p.setCondition(fsv1alpha1.ConditionReady, metav1.ConditionTrue,
			fsv1alpha1.ReasonQuorumAvailable, message)

		return
	}

	p.setCondition(fsv1alpha1.ConditionReady, metav1.ConditionFalse,
		fsv1alpha1.ReasonQuorumUnavailable, message)
}

// summarizeAlignment reports whether the running cluster is the declared one:
// the right number of nodes, each carrying the configuration it should.
//
// A refused spec leaves both conditions alone. They describe the cluster that
// is running, and refusing a change does not alter it.
func (r *Reconciler) summarizeAlignment(p *pass) {
	if p.desired == nil {
		return
	}

	desired := p.object.Status.Nodes

	// ClusterSizeAligned reflects the node *set*: every declared node exists
	// and its pod is the current template.
	switch {
	case int32(len(p.live)) < desired: //nolint:gosec // the topology is bounded at 16 nodes
		p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
			fsv1alpha1.ReasonScalingUp,
			fmt.Sprintf("%d of %d declared nodes exist", len(p.live), desired))
	case p.health.current == desired:
		p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionTrue,
			fsv1alpha1.ReasonUpToDate, fmt.Sprintf("All %d declared nodes are running", desired))
	default:
		p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
			fsv1alpha1.ReasonRollingNodes,
			fmt.Sprintf("%d of %d nodes run the desired pod template", p.health.current, desired))
	}

	// ConfigurationInSync reflects what each node has actually *applied*, as
	// its admin API reports it: every node must run the desired configuration
	// revision, whether it got there by a restart or a verified hot reload
	// (SPEC §8.3).
	if p.health.configCurrent == desired {
		p.setCondition(fsv1alpha1.ConditionConfigurationInSync, metav1.ConditionTrue,
			fsv1alpha1.ReasonUpToDate, "Every node runs the desired configuration")
	} else {
		p.setCondition(fsv1alpha1.ConditionConfigurationInSync, metav1.ConditionFalse,
			fsv1alpha1.ReasonConfigReloadPending,
			fmt.Sprintf("%d of %d nodes have applied the desired configuration", p.health.configCurrent, desired))
	}
}
