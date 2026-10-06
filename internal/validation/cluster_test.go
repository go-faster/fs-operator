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

package validation_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/validation"
)

// spec is a valid flat topology, defaulted the way the API server and the
// controller both see it.
func spec(mutate func(*fsv1alpha1.FSClusterSpec)) *fsv1alpha1.FSClusterSpec {
	nodes := int32(3)
	s := &fsv1alpha1.FSClusterSpec{
		Topology: fsv1alpha1.TopologySpec{Nodes: &nodes},
		Storage:  fsv1alpha1.StorageSpec{Size: resource.MustParse("10Gi")},
	}

	if mutate != nil {
		mutate(s)
	}

	s.WithDefaults()

	return s
}

func nodes(n int32) func(*fsv1alpha1.FSClusterSpec) {
	return func(s *fsv1alpha1.FSClusterSpec) { s.Topology.Nodes = &n }
}

func TestClusterAcceptsAValidSpec(t *testing.T) {
	if failure := validation.Cluster(spec(nil)); failure != nil {
		t.Fatalf("a valid spec was refused: %v", failure)
	}
}

func TestClusterRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fsv1alpha1.FSClusterSpec)
		reason fsv1alpha1.ConditionReason
	}{
		{
			// Three copies on distinct nodes; two nodes cannot hold them.
			name:   "two nodes",
			mutate: nodes(2),
			reason: fsv1alpha1.ReasonUnsupportedTopology,
		},
		{
			name: "two nodes across racks",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				s.Topology = fsv1alpha1.TopologySpec{Racks: []fsv1alpha1.RackSpec{
					{Name: "a", Nodes: 1},
					{Name: "b", Nodes: 1},
				}}
			},
			reason: fsv1alpha1.ReasonUnsupportedTopology,
		},
		{
			// An ec:4,2 bucket needs its six shards on six nodes.
			name: "a width wider than the cluster",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				s.Topology.Nodes = new(int32(5))
				s.Layout.Widths = []int32{3, 6}
			},
			reason: fsv1alpha1.ReasonLayoutTopologyMismatch,
		},
		{
			// An OTLP exporter with no endpoint ships to localhost:4318 and
			// logs the failure every interval.
			name: "otlp exporter without a destination",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				s.Observability.Logs.Exporter = fsv1alpha1.ExporterOTLP
			},
			reason: fsv1alpha1.ReasonSpecInvalid,
		},
		{
			// fs reads the shared transport first, so the per-signal one
			// would be configuration that does nothing.
			name: "a per-signal protocol under a shared one",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				s.Observability.OTLP.Endpoint = "http://collector.observability:4317"
				s.Observability.OTLP.Protocol = "grpc"
				s.Observability.Traces.Protocol = "http/protobuf"
			},
			reason: fsv1alpha1.ReasonSpecInvalid,
		},
		{
			// Only the Prometheus exporter serves the port the PodMonitor
			// scrapes; the pair would be a dashboard with no data.
			name: "podMonitor scraping an exporter that does not listen",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				s.Observability.PodMonitor = true
				s.Observability.Metrics.Exporter = fsv1alpha1.ExporterNone
			},
			reason: fsv1alpha1.ReasonSpecInvalid,
		},
		{
			name:   "no storage",
			mutate: func(s *fsv1alpha1.FSClusterSpec) { s.Storage.Size = resource.Quantity{} },
			reason: fsv1alpha1.ReasonSpecInvalid,
		},
		{
			name:   "more nodes than fs supports",
			mutate: nodes(validation.MaxNodes + 1),
			reason: fsv1alpha1.ReasonUnsupportedTopology,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := validation.Cluster(spec(tc.mutate))
			if failure == nil {
				t.Fatal("the spec was admitted")
			}

			if failure.Reason != tc.reason {
				t.Errorf("reason = %q, want %q (%s)", failure.Reason, tc.reason, failure.Message)
			}
		})
	}
}

// TestClusterAcceptsAWidthItHasNodesFor covers the erasure-coded cluster: six
// nodes host an ec:4,2 bucket's width.
func TestClusterAcceptsAWidthItHasNodesFor(t *testing.T) {
	s := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Topology.Nodes = new(int32(6))
		s.Layout.Widths = []int32{3, 6}
	})

	if failure := validation.Cluster(s); failure != nil {
		t.Fatalf("a six-node cluster with width 6 was refused: %v", failure)
	}
}

// TestClusterAcceptsASignalWithItsOwnEndpoint covers the destination a signal
// brings itself: a collector that takes logs somewhere other than the rest.
func TestClusterAcceptsASignalWithItsOwnEndpoint(t *testing.T) {
	s := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Observability.Logs = fsv1alpha1.SignalSpec{
			Exporter: fsv1alpha1.ExporterOTLP,
			Endpoint: "http://loki-gateway.observability:4318/otlp/v1/logs",
		}
	})

	if failure := validation.Cluster(s); failure != nil {
		t.Fatalf("a signal with its own endpoint was refused: %v", failure)
	}
}

// TestClusterAcceptsSingleNode covers the development shape, which has no
// layout and so no width to check: the default width 3 must not refuse it.
func TestClusterAcceptsSingleNode(t *testing.T) {
	dev := spec(nodes(1))

	if failure := validation.Cluster(dev); failure != nil {
		t.Fatalf("a single-node spec was refused: %v", failure)
	}

	warnings := validation.ClusterWarnings(dev)
	if len(warnings) != 1 || warnings[0] != validation.SingleNodeWarning {
		t.Errorf("warnings = %q, want the single-node warning", warnings)
	}
}

// TestClusterWarnsOnAWidthWiderThanTheDomains covers a layout that has to put
// two slots of one partition in one failure domain: allowed — fs places them
// on distinct nodes — but losing that domain then costs both.
func TestClusterWarnsOnAWidthWiderThanTheDomains(t *testing.T) {
	s := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Topology = fsv1alpha1.TopologySpec{Racks: []fsv1alpha1.RackSpec{
			{Name: "a", Nodes: 2, Zone: "z1"},
			{Name: "b", Nodes: 2, Zone: "z2"},
		}}
	})

	if failure := validation.Cluster(s); failure != nil {
		t.Fatalf("a four-node, two-zone cluster was refused: %v", failure)
	}

	warnings := validation.ClusterWarnings(s)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "2 failure domains") {
		t.Errorf("warnings = %q, want one about width 3 over 2 domains", warnings)
	}

	if w := validation.ClusterWarnings(spec(nil)); len(w) != 0 {
		t.Errorf("three flat nodes warned: %q", w)
	}
}

func TestClusterUpdateRefusesCrossingTheSingleNodeLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		old, new *fsv1alpha1.FSClusterSpec
	}{
		{name: "growing a single node", old: spec(nodes(1)), new: spec(nil)},
		{name: "shrinking to a single node", old: spec(nil), new: spec(nodes(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := validation.ClusterUpdate(tc.old, tc.new)
			if failure == nil || failure.Reason != fsv1alpha1.ReasonUnsupportedTopology {
				t.Errorf("failure = %v, want UnsupportedTopology", failure)
			}
		})
	}
}

func TestClusterUpdateStorage(t *testing.T) {
	size := func(q string) func(*fsv1alpha1.FSClusterSpec) {
		return func(s *fsv1alpha1.FSClusterSpec) { s.Storage.Size = resource.MustParse(q) }
	}

	if failure := validation.ClusterUpdate(spec(size("10Gi")), spec(size("20Gi"))); failure != nil {
		t.Errorf("growing storage was refused: %v", failure)
	}

	failure := validation.ClusterUpdate(spec(size("20Gi")), spec(size("10Gi")))
	if failure == nil || failure.Reason != fsv1alpha1.ReasonStorageShrinkForbidden {
		t.Errorf("shrinking storage: failure = %v, want StorageShrinkForbidden", failure)
	}
}

// TestClusterUpdateStillChecksTheSpecItself pins that an update is judged on
// its own too, not only against what it replaces.
func TestClusterUpdateStillChecksTheSpecItself(t *testing.T) {
	failure := validation.ClusterUpdate(spec(nil), spec(nodes(2)))
	if failure == nil || failure.Reason != fsv1alpha1.ReasonUnsupportedTopology {
		t.Errorf("failure = %v, want UnsupportedTopology for two nodes", failure)
	}
}
