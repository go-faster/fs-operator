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

	"github.com/google/go-cmp/cmp"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

func TestNodes(t *testing.T) {
	flat := int32(3)

	for _, tc := range []struct {
		name     string
		topology fsv1alpha1.TopologySpec
		want     []Node
	}{
		{
			name:     "flat",
			topology: fsv1alpha1.TopologySpec{Nodes: &flat},
			want: []Node{
				{Name: node0, Index: 0},
				{Name: node1, Index: 1},
				{Name: node2, Index: 2},
			},
		},
		{
			name: "racks",
			topology: fsv1alpha1.TopologySpec{
				Racks: []fsv1alpha1.RackSpec{
					{Name: "a", Nodes: 2, Zone: zoneA},
					{Name: "b", Nodes: 1, NodeSelector: map[string]string{rackKey: "b"}},
				},
			},
			want: []Node{
				{Name: "prod-a-0", Rack: "a", Index: 0, Zone: zoneA},
				{Name: "prod-a-1", Rack: "a", Index: 1, Zone: zoneA},
				{Name: "prod-b-0", Rack: "b", Index: 0, NodeSelector: map[string]string{rackKey: "b"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testCluster()
			cluster.Spec.Topology = tc.topology

			if diff := cmp.Diff(tc.want, Nodes(cluster)); diff != "" {
				t.Errorf("Nodes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNodesMatchesSpecNames keeps the two expansions — the API helper used for
// validation and the controller's node set — from drifting apart.
func TestNodesMatchesSpecNames(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.Topology = fsv1alpha1.TopologySpec{
		Racks: []fsv1alpha1.RackSpec{{Name: "a", Nodes: 2}, {Name: "b", Nodes: 3}},
	}

	nodes := Nodes(cluster)

	got := make([]string, 0, len(nodes))
	for _, node := range nodes {
		got = append(got, node.Name)
	}

	if diff := cmp.Diff(cluster.Spec.NodeNames(cluster.Name), got); diff != "" {
		t.Errorf("node names (-spec +controller):\n%s", diff)
	}
}
