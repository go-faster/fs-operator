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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// TestNewNetworkPolicy pins the restriction SPEC §9 promises: the peer and
// admin ports reach only cluster pods and the operator; S3 and metrics stay
// open.
func TestNewNetworkPolicy(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()

	policy := NewNetworkPolicy(cluster, "fs-operator-system")

	ports := func(rule networkingv1.NetworkPolicyIngressRule) map[int32]bool {
		set := map[int32]bool{}
		for _, p := range rule.Ports {
			set[p.Port.IntVal] = true
		}

		return set
	}

	if len(policy.Spec.Ingress) != 2 {
		t.Fatalf("%d ingress rules, want 2 (open + restricted)", len(policy.Spec.Ingress))
	}

	open := ports(policy.Spec.Ingress[0])
	if !open[S3Port] || !open[MetricsPort] {
		t.Errorf("open rule ports = %v, want S3 (%d) and metrics (%d)", open, S3Port, MetricsPort)
	}

	// pprof is open on purpose, not by omission. It is unauthenticated and a
	// heap profile carries whatever is in memory, so the grant is asserted
	// here: if someone restricts it later that is a decision, and this test is
	// where they have to make it rather than a behaviour that drifts.
	if !open[PprofPort] {
		t.Errorf("open rule ports = %v, want pprof (%d) reachable from any pod", open, PprofPort)
	}

	if open[PeerPort] || open[AdminPort] {
		t.Error("the peer or admin port is open to everyone")
	}

	restricted := policy.Spec.Ingress[1]
	rp := ports(restricted)
	if !rp[PeerPort] || !rp[AdminPort] {
		t.Errorf("restricted rule ports = %v, want peer (%d) and admin (%d)", rp, PeerPort, AdminPort)
	}

	// Two sources: the cluster's own pods and the operator's namespace.
	if len(restricted.From) != 2 {
		t.Fatalf("%d restricted sources, want cluster pods + operator namespace", len(restricted.From))
	}

	if restricted.From[0].PodSelector == nil ||
		restricted.From[0].PodSelector.MatchLabels[LabelCluster] != cluster.Name {
		t.Error("the restricted rule does not allow the cluster's own pods")
	}

	if restricted.From[1].NamespaceSelector == nil ||
		restricted.From[1].NamespaceSelector.MatchLabels[namespaceNameLabel] != "fs-operator-system" {
		t.Error("the restricted rule does not allow the operator namespace")
	}
}

// TestNewNetworkPolicyWithoutOperatorNamespace covers the degraded case: with
// no operator namespace known, only the cluster's own pods are allowed.
func TestNewNetworkPolicyWithoutOperatorNamespace(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()

	policy := NewNetworkPolicy(cluster, "")

	if from := policy.Spec.Ingress[1].From; len(from) != 1 || from[0].PodSelector == nil {
		t.Errorf("restricted sources = %v, want just the cluster's pods", from)
	}
}

// TestReconcileNetworkPolicyToggle covers applying and removing the policy as
// spec.networkPolicy flips.
func TestReconcileNetworkPolicyToggle(t *testing.T) {
	r, _ := reconciler(t)
	r.OperatorNamespace = "fs-operator-system"

	key := createCluster(t, r, "netpol", func(c *fsv1alpha1.FSCluster) {
		c.Spec.NetworkPolicy = true
	})

	reconcile(t, r, key)

	var policy networkingv1.NetworkPolicy
	get(t, r, key.Namespace, key.Name, &policy)

	if len(policy.OwnerReferences) != 1 {
		t.Error("the NetworkPolicy is not owned by the cluster")
	}

	// Turn it off: the policy is removed.
	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)
	cluster.Spec.NetworkPolicy = false

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("disable the network policy: %v", err)
	}

	reconcile(t, r, key)

	err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, &networkingv1.NetworkPolicy{})
	if err == nil {
		t.Error("the NetworkPolicy survived being disabled")
	}
}
