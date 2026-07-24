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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller"
)

// health is what one pass observed of the running cluster.
//
// In P1 it is read from Kubernetes alone. Registration in etcd, the repair
// queue and placement convergence need the cluster status endpoint fs does not
// have yet (SPEC §11.2), so the conditions that depend on them — Converged,
// SchemaCurrent — are left unset rather than guessed at.
type health struct {
	// ready is the number of node pods that are Ready.
	ready int32

	// readyDomains is how many distinct failure domains have a ready node. It,
	// not the node count, is what decides whether writes can be acknowledged.
	readyDomains int

	// current is the number of nodes whose pod is Ready and already running
	// the desired configuration.
	current int32
}

// reconcileStatus writes what the pass observed and decided.
//
// It is the always-run step: a pass that refused a spec or failed halfway
// still has to say so, and this is where that reaches the object.
func (r *Reconciler) reconcileStatus(ctx context.Context, p *pass) (controller.Outcome, error) {
	observed, err := r.observe(ctx, p)
	if err != nil {
		return controller.Outcome{}, err
	}

	base := p.object.DeepCopy()
	status := &p.object.Status

	status.ObservedGeneration = p.object.Generation
	status.Nodes = int32(len(p.nodes)) //nolint:gosec // the topology is bounded at 16 nodes
	status.ReadyNodes = observed.ready
	status.Endpoints = &fsv1alpha1.EndpointsStatus{
		S3: S3Endpoint(p.cluster.Name, p.cluster.Namespace,
			p.cluster.Spec.S3.Service.Port, p.cluster.Spec.S3.TLS.SecretName != ""),
	}

	if p.configs != nil {
		status.ConfigurationRevision = ConfigRevision(p.configs)
		status.StatefulSetRevision = p.templateRevision
		status.UpdateRevision = status.ConfigurationRevision

		if observed.current == status.Nodes {
			status.CurrentRevision = status.ConfigurationRevision
		}
	}

	r.summarize(p, observed)

	for _, condition := range p.conditions {
		meta.SetStatusCondition(&status.Conditions, condition)
	}

	if err := r.Status().Patch(ctx, p.object, client.MergeFrom(base)); err != nil {
		return controller.Outcome{}, errors.Wrap(err, "update status")
	}

	return controller.Continue()
}

// summarize turns the observation into the conditions a user reads.
func (r *Reconciler) summarize(p *pass, observed health) {
	if p.failure != nil {
		p.setCondition(fsv1alpha1.ConditionReconcileSucceeded, metav1.ConditionFalse,
			fsv1alpha1.ReasonReconcileError, p.failure.Error())
	} else {
		p.setCondition(fsv1alpha1.ConditionReconcileSucceeded, metav1.ConditionTrue,
			fsv1alpha1.ReasonReconcileFinished, "Reconcile pass completed")
	}

	// A refused spec says nothing about the running cluster: the nodes that
	// were there before are still there, and the health conditions below still
	// describe them.
	desired := p.object.Status.Nodes

	if observed.ready == desired {
		p.setCondition(fsv1alpha1.ConditionNodesHealthy, metav1.ConditionTrue,
			fsv1alpha1.ReasonAllNodesReady, fmt.Sprintf("All %d nodes are ready", desired))
	} else {
		p.setCondition(fsv1alpha1.ConditionNodesHealthy, metav1.ConditionFalse,
			fsv1alpha1.ReasonNodesNotReady,
			fmt.Sprintf("%d of %d nodes are ready", observed.ready, desired))
	}

	r.summarizeReadiness(p, observed)
	r.summarizeAlignment(p, observed)
}

// summarizeReadiness reports whether the cluster can acknowledge a write.
//
// fs acknowledges a write once its synchronous quorum is durable on distinct
// failure domains — two full replicas for the replicated schemes, all k+m
// shards for erasure coding — so the question is how many domains are serving,
// not how many pods are up.
func (r *Reconciler) summarizeReadiness(p *pass, observed health) {
	scheme, err := ParseScheme(p.cluster.Spec.Scheme)
	if err != nil {
		// An unparseable scheme was already refused by validation.
		return
	}

	quorum := scheme.WriteQuorumDomains()
	if observed.readyDomains >= quorum {
		p.setCondition(fsv1alpha1.ConditionReady, metav1.ConditionTrue,
			fsv1alpha1.ReasonQuorumAvailable,
			fmt.Sprintf("%d failure domains are serving, %d needed for a write", observed.readyDomains, quorum))

		return
	}

	p.setCondition(fsv1alpha1.ConditionReady, metav1.ConditionFalse,
		fsv1alpha1.ReasonQuorumUnavailable,
		fmt.Sprintf("%d failure domains are serving, %d needed for a write", observed.readyDomains, quorum))
}

// summarizeAlignment reports whether the running cluster is the declared one:
// the right number of nodes, each carrying the configuration it should.
func (r *Reconciler) summarizeAlignment(p *pass, observed health) {
	desired := p.object.Status.Nodes

	if observed.current == desired {
		p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionTrue,
			fsv1alpha1.ReasonUpToDate, fmt.Sprintf("All %d declared nodes are running", desired))
		p.setCondition(fsv1alpha1.ConditionConfigurationInSync, metav1.ConditionTrue,
			fsv1alpha1.ReasonUpToDate, "Every node runs the desired configuration")

		return
	}

	p.setCondition(fsv1alpha1.ConditionClusterSizeAligned, metav1.ConditionFalse,
		fsv1alpha1.ReasonScalingUp,
		fmt.Sprintf("%d of %d declared nodes are running", observed.current, desired))
	p.setCondition(fsv1alpha1.ConditionConfigurationInSync, metav1.ConditionFalse,
		fsv1alpha1.ReasonRollingNodes,
		fmt.Sprintf("%d of %d nodes run the desired configuration", observed.current, desired))
}

// observe reads the cluster's pods and matches them against the node set.
func (r *Reconciler) observe(ctx context.Context, p *pass) (health, error) {
	var pods corev1.PodList

	if err := r.List(ctx, &pods,
		client.InNamespace(p.cluster.Namespace),
		client.MatchingLabels(SelectorLabels(p.cluster.Name)),
	); err != nil {
		return health{}, errors.Wrap(err, "list node pods")
	}

	byNode := make(map[string]*corev1.Pod, len(pods.Items))

	for i, pod := range pods.Items {
		byNode[pod.Labels[LabelNode]] = &pods.Items[i]
	}

	var (
		observed health
		domains  = make(map[string]bool)
	)

	for _, node := range p.nodes {
		pod, ok := byNode[node.Name]
		if !ok || !podReady(pod) {
			continue
		}

		observed.ready++

		// In the flat topology every node is its own failure domain, which is
		// what fs does with an empty rack.
		domains[domainOf(node)] = true

		if p.restarts != nil && pod.Annotations[AnnotationRestartRevision] == p.restarts[node.Name] {
			observed.current++
		}
	}

	observed.readyDomains = len(domains)

	return observed, nil
}

// domainOf is the failure domain a node belongs to.
func domainOf(node Node) string {
	if node.Rack == "" {
		return node.Name
	}

	return node.Rack
}

// podReady reports whether a pod is serving.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
