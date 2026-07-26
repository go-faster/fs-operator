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
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// managed builds a defaulted cluster whose etcd the operator runs.
func managed(name string, replicas int32) *fsv1alpha1.FSCluster {
	nodes := int32(3)
	cluster := &fsv1alpha1.FSCluster{}
	cluster.Name = name
	cluster.Namespace = testNamespace
	cluster.Spec.Topology.Nodes = &nodes
	cluster.Spec.Etcd = fsv1alpha1.EtcdSpec{
		Managed: &fsv1alpha1.ManagedEtcdSpec{Replicas: &replicas},
	}
	cluster.Spec.WithDefaults()

	return cluster
}

// TestEtcdEndpointsResolve covers the one thing every other reader depends on:
// a managed cluster's nodes must be pointed at the members the operator runs,
// and an external one at exactly what the user wrote.
func TestEtcdEndpointsResolve(t *testing.T) {
	endpoints := EtcdEndpoints(managed("dev", 3))
	want := []string{
		"http://dev-etcd-0.dev-etcd.default.svc:2379",
		"http://dev-etcd-1.dev-etcd.default.svc:2379",
		"http://dev-etcd-2.dev-etcd.default.svc:2379",
	}

	if !slices.Equal(endpoints, want) {
		t.Errorf("managed endpoints = %v, want %v", endpoints, want)
	}

	external := &fsv1alpha1.FSCluster{}
	external.Name, external.Namespace = "prod", testNamespace
	external.Spec.Etcd.External = &fsv1alpha1.ExternalEtcdSpec{
		Endpoints: []string{"https://etcd-a:2379", "https://etcd-b:2379"},
	}

	if got := EtcdEndpoints(external); !slices.Equal(got, external.Spec.Etcd.External.Endpoints) {
		t.Errorf("external endpoints = %v, want them passed through unchanged", got)
	}
}

// TestEtcdStatefulSetBootstraps covers the parts a fresh etcd cannot come up
// without: the full static member list, and Parallel pod management — with the
// default OrderedReady a three-member cluster deadlocks, because member 0
// never becomes ready alone and the others are never started.
func TestEtcdStatefulSetBootstraps(t *testing.T) {
	set := NewEtcdStatefulSet(managed("dev", 3))

	if set.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		t.Errorf("pod management = %q, want Parallel or a fresh cluster cannot reach quorum",
			set.Spec.PodManagementPolicy)
	}

	if *set.Spec.Replicas != 3 {
		t.Errorf("replicas = %d, want 3", *set.Spec.Replicas)
	}

	args := strings.Join(set.Spec.Template.Spec.Containers[0].Args, " ")
	for _, member := range []string{"dev-etcd-0=", "dev-etcd-1=", "dev-etcd-2="} {
		if !strings.Contains(args, member) {
			t.Errorf("--initial-cluster is missing %s: %s", member, args)
		}
	}
}

// TestEtcdStatefulSetDefaultsToOneMember: managed etcd is for development, so
// the default is the smallest thing that works.
func TestEtcdStatefulSetDefaultsToOneMember(t *testing.T) {
	cluster := &fsv1alpha1.FSCluster{}
	cluster.Name, cluster.Namespace = "dev", testNamespace
	cluster.Spec.Etcd = fsv1alpha1.EtcdSpec{Managed: &fsv1alpha1.ManagedEtcdSpec{}}
	cluster.Spec.WithDefaults()

	if got := cluster.Spec.EtcdReplicas(); got != 1 {
		t.Errorf("default replicas = %d, want 1", got)
	}

	set := NewEtcdStatefulSet(cluster)
	if image := set.Spec.Template.Spec.Containers[0].Image; !strings.HasPrefix(image, "quay.io/coreos/etcd:") {
		t.Errorf("image = %q, want the pinned default etcd", image)
	}
}

// TestEtcdLabelsDoNotCollideWithNodes is the subtle one. The peers Service and
// the disruption budget select on the cluster's node labels; if an etcd pod
// matched them, the Service would publish etcd as an fs peer and the PDB would
// count it as a data node.
func TestEtcdLabelsDoNotCollideWithNodes(t *testing.T) {
	cluster := managed("dev", 1)
	etcd := etcdLabels(cluster.Name)

	if selectorMatches(NodeSelectorLabels(cluster.Name, "dev-0"), etcd) {
		t.Error("an etcd pod matches the node selector; the peers Service would publish it as an fs peer")
	}

	if selectorMatches(SelectorLabels(cluster.Name), etcd) {
		t.Error("an etcd pod matches the cluster-wide node selector; the PDB would count it as a data node")
	}
}

// selectorMatches reports whether labels satisfy every entry of selector.
func selectorMatches(selector, labels map[string]string) bool {
	for key, want := range selector {
		if labels[key] != want {
			return false
		}
	}

	return true
}

// TestReconcileManagedEtcd covers the pass end to end: the members come up
// owned by the cluster, and the nodes are configured to talk to them.
func TestReconcileManagedEtcd(t *testing.T) {
	r, recorder := reconciler(t)
	key := createCluster(t, r, "dev-etcd-cluster", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Etcd = fsv1alpha1.EtcdSpec{Managed: &fsv1alpha1.ManagedEtcdSpec{}}
	})

	reconcile(t, r, key)

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, EtcdStatefulSetName(key.Name), &set)

	if len(set.OwnerReferences) == 0 {
		t.Error("the managed etcd is not owned by its cluster, so deleting the cluster would leak it")
	}

	var service corev1.Service
	get(t, r, key.Namespace, EtcdServiceName(key.Name), &service)

	if !service.Spec.PublishNotReadyAddresses {
		t.Error("the etcd Service must publish not-ready addresses, or members cannot find each other to elect a leader")
	}

	// The nodes have to be pointed at it, which is the whole point.
	var config corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(key.Name+"-0"), &config)

	if want := EtcdStatefulSetName(key.Name); !strings.Contains(string(config.Data[ConfigFileName]), want) {
		t.Errorf("the node config does not point at the managed etcd (%q)", want)
	}

	// And the user has to be told what they signed up for.
	warned := false
	for len(recorder.Events) > 0 {
		if ev := <-recorder.Events; contains(ev, eventManagedEtcd) {
			warned = true
		}
	}

	if !warned {
		t.Errorf("no %s event; a cluster on the development etcd must say so", eventManagedEtcd)
	}
}

// TestManagedEtcdSkipsTheFinalizer covers the interaction with §8.6: the
// managed etcd is owned by the cluster, so garbage collection takes it and its
// volumes. Holding the object open to purge keys from an etcd being deleted
// underneath us would only stall the delete.
func TestManagedEtcdSkipsTheFinalizer(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "dev-etcd-final", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Etcd = fsv1alpha1.EtcdSpec{
			Managed:         &fsv1alpha1.ManagedEtcdSpec{},
			CleanupOnDelete: true,
		}
	})

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	if slices.Contains(cluster.Finalizers, ClusterFinalizer) {
		t.Error("a managed-etcd cluster carries the etcd-cleanup finalizer; its delete would wait on an etcd that is going away with it")
	}

	// It still deletes cleanly.
	if err := r.Delete(t.Context(), &cluster); err != nil {
		t.Fatalf("delete: %v", err)
	}

	reconcile(t, r, key)

	err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, &cluster)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the cluster is still present after delete: %v", err)
	}
}
