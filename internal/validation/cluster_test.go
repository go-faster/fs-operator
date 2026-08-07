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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/scheme"
	"github.com/go-faster/fs-operator/internal/validation"
)

// spec is a valid flat topology, defaulted the way the API server and the
// controller both see it.
func spec(mutate func(*fsv1alpha1.FSClusterSpec)) *fsv1alpha1.FSClusterSpec {
	nodes := int32(3)
	s := &fsv1alpha1.FSClusterSpec{
		Scheme:   scheme.RF3,
		Topology: fsv1alpha1.TopologySpec{Nodes: &nodes},
		Etcd: fsv1alpha1.EtcdSpec{
			External: &fsv1alpha1.ExternalEtcdSpec{
				Endpoints: []string{"http://etcd.default.svc:2379"},
			},
		},
	}

	if mutate != nil {
		mutate(s)
	}

	s.WithDefaults()

	return s
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
			name:   "unparseable scheme",
			mutate: func(s *fsv1alpha1.FSClusterSpec) { s.Scheme = "rf9" },
			reason: fsv1alpha1.ReasonSpecInvalid,
		},
		{
			// rf3 places on three domains; two nodes cannot host it.
			name: "topology too small for the scheme",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				nodes := int32(2)
				s.Topology.Nodes = &nodes
			},
			reason: fsv1alpha1.ReasonSchemeTopologyMismatch,
		},
		{
			name: "erasure coding needs more domains than the topology has",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				s.Scheme = "ec:4,2"
				nodes := int32(5)
				s.Topology.Nodes = &nodes
			},
			reason: fsv1alpha1.ReasonSchemeTopologyMismatch,
		},
		{
			// The single node stores everything under one root; a second disk
			// would be a volume nothing ever reads.
			name: "single node with more than one disk",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				nodes := int32(1)
				s.Topology.Nodes = &nodes
				s.Storage = disks(map[string]string{"d0": diskSize, "d1": diskSize})
				s.Etcd = fsv1alpha1.EtcdSpec{}
			},
			reason: fsv1alpha1.ReasonUnsupportedTopology,
		},
		{
			// There is no cluster to register in, so etcd would be a control
			// plane the node never dials.
			name: "single node declaring etcd",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				nodes := int32(1)
				s.Topology.Nodes = &nodes
				s.Storage = disks(map[string]string{"d0": diskSize})
			},
			reason: fsv1alpha1.ReasonSpecInvalid,
		},
		{
			name: "more nodes than fs supports",
			mutate: func(s *fsv1alpha1.FSClusterSpec) {
				nodes := int32(validation.MaxNodes + 1)
				s.Topology.Nodes = &nodes
			},
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

// singleNode is the development shape: one node, one disk, no control plane.
func singleNode(s *fsv1alpha1.FSClusterSpec) {
	nodes := int32(1)
	s.Topology.Nodes = &nodes
	s.Storage = disks(map[string]string{"d0": diskSize})
	s.Etcd = fsv1alpha1.EtcdSpec{}
}

// TestClusterAcceptsSingleNode covers the development shape: fs's filesystem
// backend, which needs neither the failure domains the scheme asks for nor the
// etcd a cluster registers in.
func TestClusterAcceptsSingleNode(t *testing.T) {
	dev := spec(singleNode)

	if failure := validation.Cluster(dev); failure != nil {
		t.Fatalf("a single-node dev cluster was refused: %v", failure)
	}

	warnings := validation.ClusterWarnings(dev)
	if len(warnings) != 1 || warnings[0] != validation.SingleNodeWarning {
		t.Errorf("warnings = %v, want the single-node warning", warnings)
	}
}

// TestClusterUpdateRefusesCrossingTheSingleNodeLine covers the two changes that
// would silently strand a single node's data: growing it into a cluster, which
// changes the storage backend, and renaming the disk its root lives on.
func TestClusterUpdateRefusesCrossingTheSingleNodeLine(t *testing.T) {
	dev := spec(singleNode)

	for _, tc := range []struct {
		name    string
		updated *fsv1alpha1.FSClusterSpec
	}{
		{
			// The user's natural patch: raise the node count and nothing else.
			// The backend switch has to be what they are told about, not the
			// etcd a clustered topology would separately need.
			name: "grown into a cluster",
			updated: spec(func(s *fsv1alpha1.FSClusterSpec) {
				s.Storage = disks(map[string]string{"d0": diskSize})
				s.Etcd = fsv1alpha1.EtcdSpec{}
			}),
		},
		{
			name: "disk renamed",
			updated: spec(func(s *fsv1alpha1.FSClusterSpec) {
				singleNode(s)
				s.Storage = disks(map[string]string{"data": diskSize})
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := validation.ClusterUpdate(dev, tc.updated)
			if failure == nil {
				t.Fatal("the update was admitted")
			}

			if failure.Reason != fsv1alpha1.ReasonUnsupportedTopology {
				t.Errorf("reason = %q, want %q (%s)",
					failure.Reason, fsv1alpha1.ReasonUnsupportedTopology, failure.Message)
			}
		})
	}
}

// TestClusterWarnsOnDevSizedCluster covers what is allowed but worth saying:
// a cluster too small to host a replicated scheme is a development toy, and a
// user should hear that at apply time rather than discover it in production.
func TestClusterWarnsOnDevSizedCluster(t *testing.T) {
	// Below three nodes, but with a scheme that does not itself refuse it.
	small := spec(func(s *fsv1alpha1.FSClusterSpec) {
		nodes := int32(2)
		s.Topology.Nodes = &nodes
		s.Scheme = "ec:1,1"
	})

	warnings := validation.ClusterWarnings(small)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the cluster being dev-sized", warnings)
	}

	if len(validation.ClusterWarnings(spec(nil))) != 0 {
		t.Error("a supported topology should warn about nothing")
	}
}

// diskSize is the size the disk fixtures start at.
const diskSize = "100Gi"

// disks builds a storage spec with the given disk sizes.
func disks(sizes map[string]string) fsv1alpha1.StorageSpec {
	storage := fsv1alpha1.StorageSpec{}

	for name, size := range sizes {
		storage.Disks = append(storage.Disks, fsv1alpha1.DiskSpec{
			Name: name,
			Size: resource.MustParse(size),
		})
	}

	return storage
}

// TestClusterUpdateRefusesDiskShrink covers the check that has to compare
// against the previous spec: a PVC cannot shrink, so a spec that asks for it
// would leave the cluster stuck rather than resized.
func TestClusterUpdateRefusesDiskShrink(t *testing.T) {
	before := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Storage = disks(map[string]string{"d0": diskSize, "d1": diskSize})
	})

	shrunk := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Storage = disks(map[string]string{"d0": diskSize, "d1": "50Gi"})
	})

	failure := validation.ClusterUpdate(before, shrunk)
	if failure == nil {
		t.Fatal("a shrinking disk was admitted")
	}

	if failure.Reason != fsv1alpha1.ReasonDiskShrinkForbidden {
		t.Errorf("reason = %q, want %q", failure.Reason, fsv1alpha1.ReasonDiskShrinkForbidden)
	}
}

func TestClusterUpdateAllowsDiskGrowth(t *testing.T) {
	before := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Storage = disks(map[string]string{"d0": diskSize})
	})

	grown := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Storage = disks(map[string]string{"d0": "200Gi"})
	})

	if failure := validation.ClusterUpdate(before, grown); failure != nil {
		t.Fatalf("growing a disk was refused: %v", failure)
	}
}

// TestClusterUpdateIgnoresRemovedDisks covers the distinction the shrink check
// has to make: a disk the update no longer mentions is being removed, which is
// a different operation with its own rules — not a disk shrinking to zero.
func TestClusterUpdateIgnoresRemovedDisks(t *testing.T) {
	before := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Storage = disks(map[string]string{"d0": diskSize, "d1": diskSize})
	})

	fewer := spec(func(s *fsv1alpha1.FSClusterSpec) {
		s.Storage = disks(map[string]string{"d0": diskSize})
	})

	if failure := validation.ClusterUpdate(before, fewer); failure != nil {
		t.Fatalf("removing a disk was reported as a shrink: %v", failure)
	}
}

// TestClusterUpdateStillChecksTheSpecItself: an update has to pass everything a
// create would, or a cluster could be edited into a shape it could never have
// been created in.
func TestClusterUpdateStillChecksTheSpecItself(t *testing.T) {
	before := spec(nil)
	shrunkTopology := spec(func(s *fsv1alpha1.FSClusterSpec) {
		nodes := int32(2)
		s.Topology.Nodes = &nodes
	})

	failure := validation.ClusterUpdate(before, shrunkTopology)
	if failure == nil || failure.Reason != fsv1alpha1.ReasonSchemeTopologyMismatch {
		t.Fatalf("failure = %v, want %q", failure, fsv1alpha1.ReasonSchemeTopologyMismatch)
	}
}
