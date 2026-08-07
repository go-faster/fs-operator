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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-faster/errors"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
)

// reconcileNetworkPolicy applies the cluster's NetworkPolicy when opted in and
// removes it when opted out, so toggling spec.networkPolicy takes effect (SPEC
// §9).
func (r *Reconciler) reconcileNetworkPolicy(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	policy := NewNetworkPolicy(p.cluster, r.OperatorNamespace)

	if !p.cluster.Spec.NetworkPolicy {
		if err := r.deleteIfExists(ctx, policy); err != nil {
			return pipeline.Outcome{}, err
		}

		return pipeline.Continue()
	}

	if err := r.apply(ctx, p.cluster, policy); err != nil {
		return pipeline.Outcome{}, err
	}

	return pipeline.Continue()
}

// deleteIfExists deletes an object, treating an already-absent one as success.
func (r *Reconciler) deleteIfExists(ctx context.Context, object client.Object) error {
	if err := r.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "delete %s %q", object.GetObjectKind().GroupVersionKind().Kind, object.GetName())
	}

	return nil
}

// namespaceNameLabel is the label the API server stamps on every namespace with
// its own name, so a NetworkPolicy can select the operator's namespace without
// the operator labelling it.
const namespaceNameLabel = "kubernetes.io/metadata.name"

// NewNetworkPolicy builds the opt-in NetworkPolicy for a cluster (SPEC §9).
//
// fs's peer traffic (7080) is HMAC-authenticated but not encrypted, and the
// admin listener (8090) manages credentials, so both are restricted to the
// cluster's own pods and the operator's namespace. S3, metrics and pprof stay
// open — S3 is the service, metrics must be scrapeable, and pprof is reachable
// from any pod by design (see below) — which, because a NetworkPolicy ingress
// list is an allow-list, must be stated explicitly or they would be denied.
//
// pprof (9010) is deliberately on the open rule rather than the restricted
// one. It is unauthenticated, and a heap profile contains whatever is in the
// process's memory, so this is a real grant and not an oversight: it is here
// because reaching a struggling node's profiler without first arranging
// network access is worth more than the exposure costs in the environments
// this runs in. Moving it to internalFrom is the one-line change if that
// trade stops holding; docs/guides/security.md states the consequence.
func NewNetworkPolicy(cluster *fsv1alpha1.FSCluster, operatorNamespace string) *networkingv1.NetworkPolicy {
	clusterPods := metav1.LabelSelector{MatchLabels: SelectorLabels(cluster.Name)}

	// Peers and the operator may reach the internal ports.
	internalFrom := []networkingv1.NetworkPolicyPeer{
		{PodSelector: &clusterPods},
	}

	if operatorNamespace != "" {
		internalFrom = append(internalFrom, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{namespaceNameLabel: operatorNamespace},
			},
		})
	}

	// The open ports. pprof is only among them when the node serves it: an
	// allow-list naming a port nothing listens on grants access to nothing,
	// but it also tells the next reader the endpoint is there.
	open := []networkingv1.NetworkPolicyPort{
		policyPort(cluster.Spec.S3.Service.Port),
	}

	if MetricsScraped(cluster) {
		open = append(open, policyPort(MetricsPort))
	}

	if pprofEnabled(cluster) {
		open = append(open, policyPort(PprofPort))
	}

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    ObjectLabels(cluster.Name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: clusterPods,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// S3, metrics and (when served) pprof: unrestricted.
					Ports: open,
				},
				{
					// Peer replication and the admin API: cluster + operator.
					From:  internalFrom,
					Ports: []networkingv1.NetworkPolicyPort{policyPort(PeerPort), policyPort(AdminPort)},
				},
			},
		},
	}
}

// policyPort is a TCP port for a NetworkPolicy rule.
func policyPort(port int32) networkingv1.NetworkPolicyPort {
	proto := corev1.ProtocolTCP
	value := intstr.FromInt32(port)

	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &value}
}
