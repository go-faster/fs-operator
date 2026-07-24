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
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

func TestNewNodeConfigSecret(t *testing.T) {
	cluster := testCluster()
	node := Nodes(cluster)[0]
	config := RenderedConfig{Data: []byte("server:\n  addr: :8080\n"), Revision: "cfg-abcdef012345"}

	secret := NewNodeConfigSecret(cluster, node, config)

	if got, want := secret.Name, node.Name+"-config"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}

	if got, want := secret.Namespace, cluster.Namespace; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}

	if got, want := string(secret.Data[ConfigFileName]), string(config.Data); got != want {
		t.Errorf("config = %q, want %q", got, want)
	}

	// The annotation is the config's own revision marker, so it matches what
	// the node reports back after a reload (SPEC §8.3).
	if got, want := secret.Annotations[AnnotationConfigRevision], config.Revision; got != want {
		t.Errorf("config revision = %q, want %q", got, want)
	}

	if got, want := secret.Labels[LabelNode], node.Name; got != want {
		t.Errorf("node label = %q, want %q", got, want)
	}
}

// TestGeneratedSecrets checks the shape of the minted Secrets. Their values are
// random by design, so what is worth pinning is that every consumer finds the
// key it reads and that no two Secrets share material.
func TestGeneratedSecrets(t *testing.T) {
	cluster := testCluster()

	clusterSecret, err := NewClusterSecret(cluster)
	if err != nil {
		t.Fatalf("cluster secret: %v", err)
	}

	adminToken, err := NewAdminTokenSecret(cluster)
	if err != nil {
		t.Fatalf("admin token: %v", err)
	}

	root, err := NewRootCredentialsSecret(cluster)
	if err != nil {
		t.Fatalf("root credentials: %v", err)
	}

	for _, tc := range []struct {
		secret *corev1.Secret
		name   string
		keys   []string
	}{
		{secret: clusterSecret, name: "prod-cluster-secret", keys: []string{ClusterSecretKey}},
		{secret: adminToken, name: "prod-admin-token", keys: []string{AdminTokenKey}},
		{secret: root, name: "prod-root-credentials", keys: []string{AccessKeyKey, SecretKeyKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.secret.Name != tc.name {
				t.Errorf("name = %q, want %q", tc.secret.Name, tc.name)
			}

			if got, want := len(tc.secret.StringData), len(tc.keys); got != want {
				t.Errorf("holds %d keys, want %d", got, want)
			}

			for _, key := range tc.keys {
				if tc.secret.StringData[key] == "" {
					t.Errorf("key %q is empty", key)
				}
			}

			if tc.secret.Labels[LabelManagedBy] != OperatorName {
				t.Error("secret is not labelled as managed by the operator")
			}
		})
	}

	if clusterSecret.StringData[ClusterSecretKey] == adminToken.StringData[AdminTokenKey] {
		t.Error("the cluster secret and the admin token are the same value")
	}

	// Minting is per call: the reconciler must create these once and never
	// apply them again, or every pass would hand the cluster a new secret.
	again, err := NewClusterSecret(cluster)
	if err != nil {
		t.Fatalf("cluster secret: %v", err)
	}

	if again.StringData[ClusterSecretKey] == clusterSecret.StringData[ClusterSecretKey] {
		t.Error("two mints produced the same cluster secret")
	}
}

func TestSecretSources(t *testing.T) {
	cluster := testCluster()

	if got, want := ClusterSecretSource(cluster), "prod-cluster-secret"; got != want {
		t.Errorf("cluster secret source = %q, want the generated %q", got, want)
	}

	if got, want := RootCredentialsSource(cluster), "prod-root-credentials"; got != want {
		t.Errorf("root credentials source = %q, want the generated %q", got, want)
	}

	cluster.Spec.ClusterSecretRef = &corev1.LocalObjectReference{Name: "byo-cluster-secret"}
	cluster.Spec.Auth.RootCredentialsSecretRef = &corev1.LocalObjectReference{Name: "byo-root"}

	if got, want := ClusterSecretSource(cluster), "byo-cluster-secret"; got != want {
		t.Errorf("cluster secret source = %q, want the referenced %q", got, want)
	}

	if got, want := RootCredentialsSource(cluster), "byo-root"; got != want {
		t.Errorf("root credentials source = %q, want the referenced %q", got, want)
	}
}

// TestNewPeersService pins the two properties peer traffic depends on: stable
// per-pod DNS, and addresses published before a node is ready.
func TestNewPeersService(t *testing.T) {
	cluster := testCluster()
	service := NewPeersService(cluster)

	if got, want := service.Name, "prod-peers"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}

	if service.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("clusterIP = %q, want the service to be headless", service.Spec.ClusterIP)
	}

	if !service.Spec.PublishNotReadyAddresses {
		t.Error("not-ready addresses are not published; peers could not reach a starting node")
	}

	if !maps.Equal(service.Spec.Selector, SelectorLabels(cluster.Name)) {
		t.Errorf("selector = %v, want %v", service.Spec.Selector, SelectorLabels(cluster.Name))
	}

	assertPorts(t, service.Spec.Ports, map[string]int32{
		PortNamePeer:    PeerPort,
		PortNameAdmin:   AdminPort,
		PortNameMetrics: MetricsPort,
	})

	// The advertise address the renderer writes has to resolve through this
	// Service, which is what makes the two consistent by construction.
	if got := AdvertiseAddr(cluster.Name, cluster.Namespace, "prod-0"); got != "prod-0-0.prod-peers.tenant-a.svc:7080" {
		t.Errorf("advertise address %q does not resolve through the peers service", got)
	}
}

func TestNewClientService(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()
	cluster.Spec.S3.Service.Type = corev1.ServiceTypeLoadBalancer
	cluster.Spec.S3.Service.Port = 443
	cluster.Spec.S3.Service.Annotations = map[string]string{"lb/internal": "true"}

	service := NewClientService(cluster)

	if got, want := service.Name, cluster.Name; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}

	if got, want := service.Spec.Type, corev1.ServiceTypeLoadBalancer; got != want {
		t.Errorf("type = %q, want %q", got, want)
	}

	if got, want := service.Annotations["lb/internal"], "true"; got != want {
		t.Errorf("annotation = %q, want %q", got, want)
	}

	assertPorts(t, service.Spec.Ports, map[string]int32{PortNameS3: 443})

	// The published port is the user's; the container port it targets is not.
	if got, want := service.Spec.Ports[0].TargetPort, intstr.FromString(PortNameS3); got != want {
		t.Errorf("target port = %v, want the named container port %v", got, want)
	}
}

func TestNewPodDisruptionBudget(t *testing.T) {
	cluster := testCluster()
	pdb := NewPodDisruptionBudget(cluster)

	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Errorf("maxUnavailable = %v, want exactly 1", pdb.Spec.MaxUnavailable)
	}

	if pdb.Spec.MinAvailable != nil {
		t.Error("minAvailable is set; the budget must be expressed as maxUnavailable")
	}

	if !maps.Equal(pdb.Spec.Selector.MatchLabels, SelectorLabels(cluster.Name)) {
		t.Errorf("selector = %v, want it to cover every node of the cluster", pdb.Spec.Selector.MatchLabels)
	}
}

// TestLabelsAreSelectable checks the invariant every Service and the budget
// rely on: the labels they select are labels the pods actually carry.
func TestLabelsAreSelectable(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()
	cluster.Spec.Topology = fsv1alpha1.TopologySpec{
		Racks: []fsv1alpha1.RackSpec{{Name: "a", Nodes: 1}},
	}
	cluster.Spec.PodTemplate.Labels = map[string]string{
		"team": teamValue,
		// A user cannot relabel a pod out of its own selectors.
		LabelNode: "hijacked",
	}

	node := Nodes(cluster)[0]
	pod := NewStatefulSet(cluster, node, "cfg-000000000000").Spec.Template.Labels

	for _, selector := range []map[string]string{
		SelectorLabels(cluster.Name),
		NodeSelectorLabels(cluster.Name, node.Name),
	} {
		for key, want := range selector {
			if got := pod[key]; got != want {
				t.Errorf("pod label %q = %q, want %q", key, got, want)
			}
		}
	}

	if got, want := pod["team"], teamValue; got != want {
		t.Errorf("user label = %q, want %q", got, want)
	}

	if got, want := pod[LabelRack], "a"; got != want {
		t.Errorf("rack label = %q, want %q", got, want)
	}
}

// assertPorts checks that a Service publishes exactly the named ports wanted.
func assertPorts(t *testing.T, ports []corev1.ServicePort, want map[string]int32) {
	t.Helper()

	if len(ports) != len(want) {
		t.Fatalf("publishes %d ports, want %d", len(ports), len(want))
	}

	for _, port := range ports {
		expected, ok := want[port.Name]
		if !ok {
			t.Errorf("unexpected port %q", port.Name)
			continue
		}

		if port.Port != expected {
			t.Errorf("port %q = %d, want %d", port.Name, port.Port, expected)
		}

		if port.Protocol != corev1.ProtocolTCP {
			t.Errorf("port %q is %s, want TCP", port.Name, port.Protocol)
		}
	}
}
