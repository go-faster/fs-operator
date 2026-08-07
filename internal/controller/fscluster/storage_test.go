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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// expandableClass is a StorageClass that allows volume expansion; the API
// server refuses to resize a PVC whose class does not.
const expandableClass = "expandable"

// TestReconcileDiskGrow covers SPEC §8.5: growing a disk expands the node's PVC
// and orphan-recreates its StatefulSet (which has immutable claim templates),
// one node at a time, keeping the pod.
func TestReconcileDiskGrow(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	ensureExpandableClass(t, r)

	key := createCluster(t, r, "disk-grow", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Storage.Disks[0].StorageClass = expandableClass
	})

	nodes := steady(t, r, admin, key)
	provisionPVCs(t, r, key, nodes, "200Gi")

	// Grow the disk.
	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)
	cluster.Spec.Storage.Disks[0].Size = resource.MustParse("500Gi")

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("grow the disk: %v", err)
	}

	reconcile(t, r, key)

	// One node's PVC grew and its StatefulSet was orphan-deleted.
	first := nodes[0]

	var pvc corev1.PersistentVolumeClaim
	get(t, r, key.Namespace, PVCName("d0", first.Name), &pvc)

	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("500Gi")) != 0 {
		t.Errorf("PVC size = %s, want 500Gi", got.String())
	}

	// The StatefulSet was orphan-deleted: it is terminating with the orphan
	// finalizer, which the garbage collector (absent in envtest) clears in a
	// real cluster after re-parenting the pod.
	var deleted appsv1.StatefulSet
	get(t, r, key.Namespace, first.Name, &deleted)

	if deleted.DeletionTimestamp == nil {
		t.Errorf("node %q StatefulSet was not deleted", first.Name)
	}

	// Exactly one node is touched per pass — the others are untouched (no
	// deletion timestamp).
	for _, node := range nodes[1:] {
		var set appsv1.StatefulSet
		get(t, r, key.Namespace, node.Name, &set)

		if set.DeletionTimestamp != nil {
			t.Errorf("node %q was also deleted; storage surgery must be one at a time", node.Name)
		}
	}

	// Stand in for the garbage collector, then the next pass recreates the
	// StatefulSet with the new claim template.
	finishGarbageCollection(t, r, key.Namespace, first.Name)

	reconcile(t, r, key)

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, first.Name, &set)

	claim := claimNamed(t, &set, "d0")
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("500Gi")) != 0 {
		t.Errorf("recreated StatefulSet claim size = %s, want 500Gi", got.String())
	}
}

// TestReconcileAddsTheStateVolumeToAnOlderNode covers the upgrade path of a
// cluster built before nodes had a state volume: its StatefulSets have no such
// claim template, and a claim template cannot be patched in. The node has to
// go through the same orphan-recreate as a disk being added — one node at a
// time, keeping the pod and its data — rather than the operator failing to
// apply a StatefulSet forever.
func TestReconcileAddsTheStateVolumeToAnOlderNode(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)

	key := createCluster(t, r, "state-upgrade", nil)
	nodes := steady(t, r, admin, key)
	provisionPVCs(t, r, key, nodes, "200Gi")

	// Rebuild the first node the way the previous operator version would
	// have: every claim except the state volume.
	first := nodes[0]

	var before appsv1.StatefulSet
	get(t, r, key.Namespace, first.Name, &before)

	older := before.DeepCopy()
	older.Spec.VolumeClaimTemplates = slices.DeleteFunc(older.Spec.VolumeClaimTemplates,
		func(claim corev1.PersistentVolumeClaim) bool {
			return claim.Name == fsv1alpha1.StateVolumeName
		})

	container := &older.Spec.Template.Spec.Containers[0]
	container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts,
		func(mount corev1.VolumeMount) bool {
			return mount.Name == fsv1alpha1.StateVolumeName
		})

	recreateStatefulSet(t, r, older)

	// The replacement starts with an empty status, so put the node back in the
	// serving state the storage step insists on before it touches anything.
	serving(t, r, key, first)

	reconcile(t, r, key)

	var deleted appsv1.StatefulSet
	get(t, r, key.Namespace, first.Name, &deleted)

	if deleted.DeletionTimestamp == nil {
		t.Fatalf("node %q was not orphan-deleted; its state volume can never appear", first.Name)
	}

	finishGarbageCollection(t, r, key.Namespace, first.Name)

	reconcile(t, r, key)

	var after appsv1.StatefulSet
	get(t, r, key.Namespace, first.Name, &after)

	claim := claimNamed(t, &after, fsv1alpha1.StateVolumeName)
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != fsv1alpha1.DefaultStateSize {
		t.Errorf("state claim = %s, want %s", got.String(), fsv1alpha1.DefaultStateSize)
	}
}

// recreateStatefulSet replaces a StatefulSet with one whose immutable fields
// differ — what envtest cannot do with an update.
func recreateStatefulSet(t *testing.T, r *Reconciler, set *appsv1.StatefulSet) {
	t.Helper()

	if err := r.Delete(t.Context(), set); err != nil {
		t.Fatalf("delete statefulset %q: %v", set.Name, err)
	}

	var gone appsv1.StatefulSet
	if err := r.Get(t.Context(), types.NamespacedName{Namespace: set.Namespace, Name: set.Name},
		&gone); !apierrors.IsNotFound(err) {
		t.Fatalf("statefulset %q did not go away (err=%v)", set.Name, err)
	}

	fresh := set.DeepCopy()
	fresh.ObjectMeta = metav1.ObjectMeta{
		Name:            set.Name,
		Namespace:       set.Namespace,
		Labels:          set.Labels,
		Annotations:     set.Annotations,
		OwnerReferences: set.OwnerReferences,
	}
	fresh.Status = appsv1.StatefulSetStatus{}

	if err := r.Create(t.Context(), fresh); err != nil {
		t.Fatalf("recreate statefulset %q: %v", set.Name, err)
	}
}

// TestReconcileRefusesDiskShrink covers the refusal: a disk may only grow.
func TestReconcileRefusesDiskShrink(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	ensureExpandableClass(t, r)

	key := createCluster(t, r, "disk-shrink", nil)

	nodes := steady(t, r, admin, key)
	provisionPVCs(t, r, key, nodes, "200Gi")

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)
	cluster.Spec.Storage.Disks[0].Size = resource.MustParse("100Gi")

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("shrink the disk: %v", err)
	}

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionSpecValid)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonDiskShrinkForbidden {
		t.Fatalf("SpecValid = %v, want False/%s", c, fsv1alpha1.ReasonDiskShrinkForbidden)
	}

	// Nothing was deleted.
	for _, node := range nodes {
		get(t, r, key.Namespace, node.Name, &appsv1.StatefulSet{})
	}
}

// provisionPVCs creates the disk PVCs a StatefulSet controller would, at the
// given size, so the storage step has volumes to expand.
func provisionPVCs(t *testing.T, r *Reconciler, key types.NamespacedName, nodes []Node, size string) {
	t.Helper()

	for _, node := range nodes {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PVCName("d0", node.Name),
				Namespace: key.Namespace,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				// A PVC is only resizable once bound and only when its class
				// allows expansion.
				VolumeName:       "pv-" + node.Name,
				StorageClassName: ptr.To(expandableClass),
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
				},
			},
		}

		if err := r.Create(t.Context(), pvc); err != nil {
			t.Fatalf("provision pvc for %q: %v", node.Name, err)
		}

		pvc.Status.Phase = corev1.ClaimBound
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}

		if err := r.Status().Update(t.Context(), pvc); err != nil {
			t.Fatalf("bind pvc for %q: %v", node.Name, err)
		}
	}
}

// finishGarbageCollection clears the orphan finalizer on a terminating
// StatefulSet, standing in for the garbage collector envtest does not run, so
// the deletion completes.
func finishGarbageCollection(t *testing.T, r *Reconciler, namespace, name string) {
	t.Helper()

	var set appsv1.StatefulSet
	get(t, r, namespace, name, &set)

	set.Finalizers = nil
	if err := r.Update(t.Context(), &set); err != nil {
		t.Fatalf("clear finalizer on %q: %v", name, err)
	}

	if err := r.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &set); !apierrors.IsNotFound(err) {
		t.Fatalf("StatefulSet %q not removed after clearing its finalizer (err=%v)", name, err)
	}
}

// ensureExpandableClass creates the expandable StorageClass once.
func ensureExpandableClass(t *testing.T, r *Reconciler) {
	t.Helper()

	class := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: expandableClass},
		Provisioner:          "test.fs.go-faster.org",
		AllowVolumeExpansion: ptr.To(true),
	}

	if err := r.Create(t.Context(), class); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create storage class: %v", err)
	}
}

// TestStorageReplacesTheAdoptedPod covers what a live cluster found and
// envtest had been hiding.
//
// An orphan-recreate leaves the previous pod adopted by the new StatefulSet,
// and the StatefulSet controller does not restamp it: the pod keeps the old
// controller-revision-hash, so the set reports updatedReplicas 0 for a pod
// that is running and ready. nodeServing reads that as "not serving", so the
// node never counts as healthy again and every later storage change and
// rollout blocks behind it — for good, because nothing else replaces that pod.
//
// It matters for a second reason too: a running pod cannot gain a volume
// mount, so a disk added this way is never actually mounted until the pod goes.
func TestStorageReplacesTheAdoptedPod(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "adopted-pod", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)
	for _, node := range nodes {
		serving(t, r, key, node)
	}

	// The state an orphan-recreate leaves: ready, but on the previous
	// revision. Everything else in the cluster is healthy.
	stale := nodes[0]

	var set appsv1.StatefulSet
	get(t, r, key.Namespace, stale.Name, &set)

	set.Status.UpdatedReplicas = 0
	if err := r.Status().Update(t.Context(), &set); err != nil {
		t.Fatalf("mark node %q as carrying an adopted pod: %v", stale.Name, err)
	}

	// The pod the StatefulSet adopted.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: PodName(stale.Name), Namespace: key.Namespace},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "fs", Image: "ghcr.io/go-faster/fs:test"},
		}},
	}

	if err := r.Create(t.Context(), pod); err != nil {
		t.Fatalf("create the adopted pod: %v", err)
	}

	reconcile(t, r, key)

	err := r.Get(t.Context(),
		types.NamespacedName{Namespace: key.Namespace, Name: PodName(stale.Name)}, pod)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the adopted pod survived, so the node stays not-serving for good: %v", err)
	}
}
