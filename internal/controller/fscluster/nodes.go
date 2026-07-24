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
	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// Node is one fs node derived from the cluster topology: its identity, its
// failure domain and where it may be scheduled. Everything downstream — the
// config renderer, the resource builders and the rolling state machine — works
// from this expansion rather than re-reading the topology.
type Node struct {
	// Name is the node's fs node_id and the name of its StatefulSet.
	Name string

	// Rack is the fs failure-domain label. Empty in the flat topology, where
	// fs treats every node as its own failure domain.
	Rack string

	// Index is the node's ordinal within its rack, or within the cluster when
	// the topology is flat.
	Index int

	// Zone pins the node to a topology.kubernetes.io/zone value; empty leaves
	// zone placement to the scheduler.
	Zone string

	// NodeSelector pins the node to matching Kubernetes nodes. It is the
	// rack's selector; the pod-template selector is merged over it by the
	// StatefulSet builder.
	NodeSelector map[string]string
}

// Nodes expands the cluster's topology into its ordered node set: rack by rack
// in spec order, or <cluster>-<n> when the topology is flat.
//
// The order is stable across reconciles and is the order nodes are created in;
// the rolling state machine reorders it to interleave racks (SPEC §8.2).
func Nodes(cluster *fsv1alpha1.FSCluster) []Node {
	spec := &cluster.Spec

	if spec.Topology.Nodes != nil {
		nodes := make([]Node, 0, *spec.Topology.Nodes)

		for i := range int(*spec.Topology.Nodes) {
			nodes = append(nodes, Node{
				Name:  fsv1alpha1.NodeName(cluster.Name, "", i),
				Index: i,
			})
		}

		return nodes
	}

	nodes := make([]Node, 0, spec.TotalNodes())

	for _, rack := range spec.Topology.Racks {
		for i := range int(rack.Nodes) {
			nodes = append(nodes, Node{
				Name:         fsv1alpha1.NodeName(cluster.Name, rack.Name, i),
				Rack:         rack.Name,
				Index:        i,
				Zone:         rack.Zone,
				NodeSelector: rack.NodeSelector,
			})
		}
	}

	return nodes
}
