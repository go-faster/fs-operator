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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// createCluster puts a cluster in its own namespace and returns its key.
func createCluster(t *testing.T, r *Reconciler, name string, mutate func(*fsv1alpha1.FSCluster)) types.NamespacedName {
	t.Helper()

	ctx := t.Context()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}

	if err := r.Create(ctx, namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	cluster := testCluster()
	cluster.Name = name
	cluster.Namespace = name

	if mutate != nil {
		mutate(cluster)
	}

	if err := r.Create(ctx, cluster); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	return client.ObjectKeyFromObject(cluster)
}

// reconcile runs one pass and fails the test if it errors.
func reconcile(t *testing.T, r *Reconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	return result
}

// get fetches an object into out, failing the test if it is missing.
func get(t *testing.T, r *Reconciler, namespace, name string, out client.Object) {
	t.Helper()

	if err := r.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: name}, out); err != nil {
		t.Fatalf("get %T %q: %v", out, name, err)
	}
}

// condition returns a cluster's condition of the given type.
func condition(t *testing.T, r *Reconciler, key types.NamespacedName, conditionType string) *metav1.Condition {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	return meta.FindStatusCondition(cluster.Status.Conditions, conditionType)
}

// statefulSets lists the node StatefulSets of a cluster.
func statefulSets(t *testing.T, r *Reconciler, key types.NamespacedName) []appsv1.StatefulSet {
	t.Helper()

	var sets appsv1.StatefulSetList

	if err := r.List(t.Context(), &sets,
		client.InNamespace(key.Namespace),
		client.MatchingLabels(SelectorLabels(key.Name)),
	); err != nil {
		t.Fatalf("list statefulsets: %v", err)
	}

	return sets.Items
}

// TestReconcileProvisions covers the shape of a freshly created cluster: every
// resource of SPEC §8.1, owned by the cluster and pointing at each other.
func TestReconcileProvisions(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "provision", nil)

	reconcile(t, r, key)

	sets := statefulSets(t, r, key)
	if got, want := len(sets), 3; got != want {
		t.Fatalf("%d node statefulsets, want %d", got, want)
	}

	for _, set := range sets {
		if len(set.OwnerReferences) != 1 || set.OwnerReferences[0].Name != key.Name {
			t.Errorf("node %q is not owned by the cluster", set.Name)
		}

		var secret corev1.Secret
		get(t, r, key.Namespace, ConfigSecretName(set.Name), &secret)

		if len(secret.Data[ConfigFileName]) == 0 {
			t.Errorf("node %q has an empty configuration", set.Name)
		}
	}

	for _, name := range []string{
		ClusterSecretName(key.Name),
		AdminTokenSecretName(key.Name),
		RootCredentialsSecretName(key.Name),
	} {
		get(t, r, key.Namespace, name, &corev1.Secret{})
	}

	get(t, r, key.Namespace, PeersServiceName(key.Name), &corev1.Service{})
	get(t, r, key.Namespace, ClientServiceName(key.Name), &corev1.Service{})
	get(t, r, key.Namespace, key.Name, &policyv1.PodDisruptionBudget{})

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	if got, want := cluster.Status.Nodes, int32(3); got != want {
		t.Errorf("status.nodes = %d, want %d", got, want)
	}

	if cluster.Status.ConfigurationRevision == "" || cluster.Status.StatefulSetRevision == "" {
		t.Error("status carries no revisions")
	}

	if cluster.Status.Endpoints == nil || cluster.Status.Endpoints.S3 == "" {
		t.Error("status carries no S3 endpoint")
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionSpecValid); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("SpecValid = %v, want True", c)
	}

	// Nothing runs the pods in envtest, so the cluster cannot be serving.
	if c := condition(t, r, key, fsv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %v, want False while no node is up", c)
	}
}

// TestReconcileIsIdempotent is the property every reconcile pass must have,
// and the one that matters most for generated secret material: a second pass
// must not hand the cluster a new cluster secret.
func TestReconcileIsIdempotent(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "idempotent", nil)

	reconcile(t, r, key)

	var first corev1.Secret
	get(t, r, key.Namespace, ClusterSecretName(key.Name), &first)

	var config corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(key.Name+"-0"), &config)

	reconcile(t, r, key)

	var second corev1.Secret
	get(t, r, key.Namespace, ClusterSecretName(key.Name), &second)

	if string(first.Data[ClusterSecretKey]) != string(second.Data[ClusterSecretKey]) {
		t.Error("the cluster secret was regenerated; every node would have to be restarted")
	}

	var reappliedConfig corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(key.Name+"-0"), &reappliedConfig)

	if reappliedConfig.ResourceVersion != config.ResourceVersion {
		t.Error("re-applying an unchanged configuration wrote to the API server")
	}
}

// TestReconcileRefusesSchemeTopologyMismatch covers a spec the controller must
// not half-apply: erasure coding needs more failure domains than the topology
// has, so nothing is created at all.
func TestReconcileRefusesSchemeTopologyMismatch(t *testing.T) {
	r, recorder := reconciler(t)
	key := createCluster(t, r, "mismatch", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Scheme = schemeEC
	})

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionSpecValid)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("SpecValid = %v, want False", c)
	}

	if c.Reason != fsv1alpha1.ReasonSchemeTopologyMismatch {
		t.Errorf("reason = %q, want %q", c.Reason, fsv1alpha1.ReasonSchemeTopologyMismatch)
	}

	if sets := statefulSets(t, r, key); len(sets) != 0 {
		t.Errorf("%d node statefulsets were created for a refused spec", len(sets))
	}

	select {
	case event := <-recorder.Events:
		if event == "" {
			t.Error("empty event")
		}
	default:
		t.Error("the refusal was not reported as an event")
	}
}

// TestReconcileRefusesScaleDown covers SPEC §8.4: until the operator can see
// that a node holds no data, shrinking a topology is refused and the running
// nodes are left alone.
func TestReconcileRefusesScaleDown(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "scale-down", func(c *fsv1alpha1.FSCluster) {
		nodes := int32(4)
		c.Spec.Topology.Nodes = &nodes
	})

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	// Down to three, which the scheme can still host: what is refused here is
	// the removal itself, not a topology too small for the scheme.
	smaller := int32(3)
	cluster.Spec.Topology.Nodes = &smaller

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("shrink the topology: %v", err)
	}

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionSpecValid)
	if c == nil || c.Reason != fsv1alpha1.ReasonScaleDownRequiresDrain {
		t.Fatalf("SpecValid = %v, want False/%s", c, fsv1alpha1.ReasonScaleDownRequiresDrain)
	}

	if got, want := len(statefulSets(t, r, key)), 4; got != want {
		t.Errorf("%d node statefulsets, want the original %d left untouched", got, want)
	}
}

// TestReconcileScalesUp covers the additive half: new nodes may all join at
// once, since joining is what the rebalancer converges.
func TestReconcileScalesUp(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "scale-up", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	larger := int32(5)
	cluster.Spec.Topology.Nodes = &larger

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("grow the topology: %v", err)
	}

	reconcile(t, r, key)

	if got, want := len(statefulSets(t, r, key)), 5; got != want {
		t.Errorf("%d node statefulsets, want %d", got, want)
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionClusterSizeAligned); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonScalingUp {
		t.Errorf("ClusterSizeAligned = %v, want False/%s while the new nodes come up", c, fsv1alpha1.ReasonScalingUp)
	}
}

// TestReconcileRefusesMissingSecret keeps a referenced Secret from turning
// into a pod that never starts and never says why.
func TestReconcileRefusesMissingSecret(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "missing-secret", func(c *fsv1alpha1.FSCluster) {
		c.Spec.ClusterSecretRef = &corev1.LocalObjectReference{Name: "nowhere"}
	})

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionSpecValid)
	if c == nil || c.Reason != fsv1alpha1.ReasonSecretNotFound {
		t.Fatalf("SpecValid = %v, want False/%s", c, fsv1alpha1.ReasonSecretNotFound)
	}

	if sets := statefulSets(t, r, key); len(sets) != 0 {
		t.Errorf("%d node statefulsets point at a Secret that does not exist", len(sets))
	}
}

// TestReconcileReportsQuorum drives the health summary the way the world does:
// pods become ready one at a time, and the cluster starts serving when enough
// failure domains do.
func TestReconcileReportsQuorum(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "quorum", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)

	// One ready node is a single failure domain: rf2.5 acknowledges a write
	// only once two of them hold a full replica.
	serving(t, r, key, nodes[0])
	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %v, want False with one node up", c)
	}

	serving(t, r, key, nodes[1])
	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %v, want True once the write quorum is up", c)
	}

	get(t, r, key.Namespace, key.Name, &cluster)

	if got, want := cluster.Status.ReadyNodes, int32(2); got != want {
		t.Errorf("status.readyNodes = %d, want %d", got, want)
	}

	if cluster.Status.CurrentRevision != "" {
		t.Errorf("current revision = %q, want none until every node runs it", cluster.Status.CurrentRevision)
	}
}

// serving stands in for the StatefulSet controller: it reports the node's
// single pod as up and current, which is what the operator reads.
func serving(t *testing.T, r *Reconciler, key types.NamespacedName, node Node) {
	t.Helper()

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, node.Name, &set)

	set.Status.ObservedGeneration = set.Generation
	set.Status.Replicas = 1
	set.Status.ReadyReplicas = 1
	set.Status.UpdatedReplicas = 1
	set.Status.AvailableReplicas = 1

	if err := r.Status().Update(t.Context(), &set); err != nil {
		t.Fatalf("mark node %q serving: %v", node.Name, err)
	}
}

// TestReconcileIgnoresDeletedCluster keeps the pass from re-creating what
// garbage collection is taking down.
func TestReconcileIgnoresDeletedCluster(t *testing.T) {
	r, _ := reconciler(t)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "gone"},
	})
	if err != nil {
		t.Fatalf("reconcile a cluster that does not exist: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("requeue after %s, want none", result.RequeueAfter)
	}
}
