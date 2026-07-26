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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// The CRD's own validation is API surface: a rule that is too strict rejects a
// legitimate change, and one that is too loose lets a cluster break itself.
// Both are only observable against a real API server, which is why these live
// next to the controller tests.

func TestCRDRejectsInvalidSpecs(t *testing.T) {
	r, _ := reconciler(t)

	for _, tc := range []struct {
		name   string
		mutate func(*fsv1alpha1.FSCluster)
	}{
		{
			name: "no topology",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Topology = fsv1alpha1.TopologySpec{}
			},
		},
		{
			name: "both flat and racked",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Topology.Racks = []fsv1alpha1.RackSpec{{Name: "a", Nodes: 3}}
			},
		},
		{
			name: "unknown scheme",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Scheme = "rf4"
			},
		},
		{
			name: "no disks",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Storage.Disks = nil
			},
		},
		{
			name: "no etcd endpoints",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Etcd.External.Endpoints = nil
			},
		},
		{
			// A digest is the one field a typo silently turns into an image
			// that will never pull, so it is validated rather than trusted.
			name: "malformed image digest",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Image.Digest = "sha256:not-hex"
			},
		},
		{
			name: "image digest without an algorithm",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Image.Digest = "3f79bb7b435b05321651daefd374cdc6"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testCluster()
			cluster.Name = "invalid"
			cluster.Namespace = testNamespace
			tc.mutate(cluster)

			if err := r.Create(t.Context(), cluster); err == nil {
				t.Error("the API server accepted the spec, want it refused")
			}
		})
	}
}

func TestCRDEnforcesImmutability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*fsv1alpha1.FSCluster)
		allowed bool
	}{
		{
			// The regression that broke every update, including status
			// writes: a transition rule reading an unset field fails to
			// evaluate, and a failed evaluation rejects the request.
			name:    "a cluster without an etcd prefix can be updated",
			mutate:  func(c *fsv1alpha1.FSCluster) { c.Spec.Observability.LogLevel = "debug" },
			allowed: true,
		},
		{
			name:   "the etcd prefix cannot be set later",
			mutate: func(c *fsv1alpha1.FSCluster) { c.Spec.Etcd.Prefix = "/fs/elsewhere" },
		},
		{
			name: "the cluster secret cannot be adopted later",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.ClusterSecretRef = &corev1.LocalObjectReference{Name: "byo"}
			},
		},
		{
			// Removing a disk is a decommission the controller performs, not
			// something the API refuses (SPEC §8.5): it drains the disk out of
			// placement on every node and takes its volumes only once fs
			// reports it holds nothing.
			name: "a disk can be removed",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
					{Name: "d1", Size: resource.MustParse("1Gi")},
				}
			},
			allowed: true,
		},
		{
			name: "the last disk cannot be removed",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Storage.Disks = nil
			},
		},
		{
			name: "a disk can be added",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Storage.Disks = append(c.Spec.Storage.Disks,
					fsv1alpha1.DiskSpec{Name: "d1", Size: resource.MustParse("1Gi")})
			},
			allowed: true,
		},
		{
			name:    "the scheme can change",
			mutate:  func(c *fsv1alpha1.FSCluster) { c.Spec.Scheme = fsv1alpha1.DefaultScheme },
			allowed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := reconciler(t)
			key := createCluster(t, r, "immutable-"+sanitize(tc.name), nil)

			var cluster fsv1alpha1.FSCluster
			get(t, r, key.Namespace, key.Name, &cluster)

			tc.mutate(&cluster)

			err := r.Update(t.Context(), &cluster)

			switch {
			case tc.allowed && err != nil:
				t.Errorf("the API server refused a legitimate change: %v", err)
			case !tc.allowed && err == nil:
				t.Error("the API server accepted the change, want it refused")
			}
		})
	}
}

// sanitize turns a case name into a DNS label so it can name a namespace.
func sanitize(name string) string {
	label := make([]rune, 0, len(name))

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			label = append(label, r)
		case r == ' ':
			label = append(label, '-')
		}
	}

	return string(label)
}
