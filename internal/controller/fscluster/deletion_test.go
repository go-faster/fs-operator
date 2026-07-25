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
	"sync"
	"testing"

	"github.com/go-faster/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/etcdstore"
)

// fakePurge records the etcd cleanups the operator drives, standing in for a
// control plane the controller tests do not run.
type fakePurge struct {
	mu sync.Mutex

	// calls is every (endpoints, prefix) the operator asked to delete.
	calls []purgeCall

	// err, when set, fails every purge — a control plane that cannot be
	// reached.
	err error
}

type purgeCall struct {
	endpoints []string
	prefix    string
}

func (f *fakePurge) purge(_ context.Context, cfg etcdstore.Config, prefix string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, purgeCall{endpoints: cfg.Endpoints, prefix: prefix})

	if f.err != nil {
		return 0, f.err
	}

	return 7, nil
}

func (f *fakePurge) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

// cleanupCluster is a cluster that asks for its etcd keys to be deleted with it.
func cleanupCluster(c *fsv1alpha1.FSCluster) {
	c.Spec.Etcd.CleanupOnDelete = true
}

// reconcilerWithPurge is a reconciler whose etcd cleanup is recorded.
func reconcilerWithPurge(t *testing.T) (*Reconciler, *record.FakeRecorder, *fakePurge) {
	t.Helper()

	r, recorder := reconciler(t)
	purge := &fakePurge{}
	r.EtcdPurge = purge.purge

	return r, recorder, purge
}

// deleteCluster asks the API server to delete a cluster; a finalizer keeps the
// object around, which is what the finalize path then works on.
func deleteCluster(t *testing.T, r *Reconciler, key types.NamespacedName) {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	if err := r.Delete(t.Context(), &cluster); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}
}

// gone reports whether the cluster object is finally away.
func gone(t *testing.T, r *Reconciler, key types.NamespacedName) bool {
	t.Helper()

	var cluster fsv1alpha1.FSCluster

	err := r.Get(t.Context(), key, &cluster)
	if err == nil {
		return false
	}

	if !apierrors.IsNotFound(err) {
		t.Fatalf("get cluster: %v", err)
	}

	return true
}

// TestFinalizerTracksCleanupPolicy keeps the finalizer on exactly the clusters
// that asked for cleanup: an ordinary cluster's deletion can never be held up
// by an etcd the operator cannot reach.
func TestFinalizerTracksCleanupPolicy(t *testing.T) {
	r, _, _ := reconcilerWithPurge(t)
	key := createCluster(t, r, "finalizer-policy", nil)

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	if controllerutil.ContainsFinalizer(&cluster, ClusterFinalizer) {
		t.Fatalf("cluster carries %q without etcd.cleanupOnDelete", ClusterFinalizer)
	}

	cluster.Spec.Etcd.CleanupOnDelete = true
	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("opt into cleanup: %v", err)
	}

	reconcile(t, r, key)
	get(t, r, key.Namespace, key.Name, &cluster)

	if !controllerutil.ContainsFinalizer(&cluster, ClusterFinalizer) {
		t.Fatalf("cluster does not carry %q with etcd.cleanupOnDelete", ClusterFinalizer)
	}

	cluster.Spec.Etcd.CleanupOnDelete = false
	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("opt out of cleanup: %v", err)
	}

	reconcile(t, r, key)
	get(t, r, key.Namespace, key.Name, &cluster)

	if controllerutil.ContainsFinalizer(&cluster, ClusterFinalizer) {
		t.Errorf("cluster still carries %q after opting out", ClusterFinalizer)
	}
}

// TestFinalizeDeletesEtcdKeys is the fix for the state a re-created cluster
// used to land in: the keys of the previous incarnation are deleted with it,
// so the next cluster of the same name starts on an empty prefix (SPEC §8.6).
func TestFinalizeDeletesEtcdKeys(t *testing.T) {
	r, _, purge := reconcilerWithPurge(t)
	key := createCluster(t, r, "finalize-cleanup", cleanupCluster)

	reconcile(t, r, key)

	if got := len(statefulSets(t, r, key)); got == 0 {
		t.Fatalf("no node statefulsets to take down")
	}

	deleteCluster(t, r, key)
	reconcile(t, r, key)

	if got, want := purge.count(), 1; got != want {
		t.Fatalf("%d etcd cleanups, want %d", got, want)
	}

	call := purge.calls[0]

	if got, want := call.prefix, "/fs/finalize-cleanup/finalize-cleanup"; got != want {
		t.Errorf("purged prefix %q, want %q", got, want)
	}

	var cluster fsv1alpha1.FSCluster
	if err := r.Get(t.Context(), key, &cluster); err == nil {
		t.Error("cluster still exists after a successful cleanup")
	}

	// The nodes go before the keys: a running node re-registers itself, so a
	// purge racing the cluster it is purging leaves half a topology behind.
	if got := len(statefulSets(t, r, key)); got != 0 {
		t.Errorf("%d node statefulsets left running", got)
	}
}

// TestFinalizeWaitsForNodesToStop covers that ordering directly: while a node's
// pod is still up, nothing is deleted from etcd.
func TestFinalizeWaitsForNodesToStop(t *testing.T) {
	r, _, purge := reconcilerWithPurge(t)
	key := createCluster(t, r, "finalize-waits", cleanupCluster)

	reconcile(t, r, key)

	// envtest runs no kubelet, so the pod a node's StatefulSet would have is
	// created by hand — it is the thing holding the etcd registration.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodName(key.Name + "-a"),
			Namespace: key.Namespace,
			Labels:    SelectorLabels(key.Name),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "fs", Image: "fs:test"}},
		},
	}

	if err := r.Create(t.Context(), pod); err != nil {
		t.Fatalf("create node pod: %v", err)
	}

	deleteCluster(t, r, key)

	result := reconcile(t, r, key)

	if purge.count() != 0 {
		t.Errorf("deleted etcd keys while a node pod was still running")
	}

	if result.RequeueAfter == 0 {
		t.Errorf("no requeue while waiting for the nodes to stop")
	}

	if gone(t, r, key) {
		t.Fatalf("cluster released before its nodes stopped")
	}

	// The pod goes; the purge follows.
	if err := r.Delete(t.Context(), pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("delete node pod: %v", err)
	}

	reconcile(t, r, key)

	if got, want := purge.count(), 1; got != want {
		t.Errorf("%d etcd cleanups once the nodes stopped, want %d", got, want)
	}
}

// TestFinalizeStopsTheMigrationJob covers the other thing of a cluster that
// writes to etcd. A migration Job's pod carries the cluster labels and lingers
// after the Job completes, so finalize has to delete the Job — with
// propagation, so garbage collection takes its pod — rather than wait on a pod
// nothing will ever remove. Waiting on it instead leaves the cluster
// undeletable for as long as the finished pod is kept around.
func TestFinalizeStopsTheMigrationJob(t *testing.T) {
	r, _, purge := reconcilerWithPurge(t)
	key := createCluster(t, r, "finalize-migration", cleanupCluster)

	reconcile(t, r, key)

	job := NewMigrationJob(mustCluster(t, r, key), 5, key.Name+"-0")
	if err := r.Create(t.Context(), job); err != nil {
		t.Fatalf("create migration job: %v", err)
	}

	// The pod the Job left behind. envtest runs no garbage collector, so it is
	// removed here where a cluster would remove it once the Job is deleted.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "finalize-migration-migrate",
			Namespace: key.Namespace,
			Labels:    ObjectLabels(key.Name),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: migrateName, Image: "fs:test"}},
		},
	}

	if err := r.Create(t.Context(), pod); err != nil {
		t.Fatalf("create migration pod: %v", err)
	}

	deleteCluster(t, r, key)
	reconcile(t, r, key)

	// The Job is deleted on the first pass, so its pod can be collected.
	var live batchv1.Job

	err := r.Get(t.Context(), types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &live)
	if err == nil && live.DeletionTimestamp.IsZero() {
		t.Errorf("migration job %q left running", job.Name)
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get migration job: %v", err)
	}

	if purge.count() != 0 {
		t.Errorf("deleted etcd keys while the migration pod was still there")
	}

	if err := r.Delete(t.Context(), pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("collect migration pod: %v", err)
	}

	reconcile(t, r, key)

	if got, want := purge.count(), 1; got != want {
		t.Errorf("%d etcd cleanups once the migration pod was collected, want %d", got, want)
	}

	if !gone(t, r, key) {
		t.Errorf("cluster not released")
	}
}

// mustCluster reads a cluster, failing the test if it is not there.
func mustCluster(t *testing.T, r *Reconciler, key types.NamespacedName) *fsv1alpha1.FSCluster {
	t.Helper()

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	return &cluster
}

// TestFinalizeHoldsOnPurgeFailure keeps the object until the keys are actually
// gone. Releasing it on an unreachable etcd would leave exactly the keys this
// cleanup exists to remove, and the next cluster of the same name would adopt
// them.
func TestFinalizeHoldsOnPurgeFailure(t *testing.T) {
	r, recorder, purge := reconcilerWithPurge(t)
	purge.err = errors.New("etcd unreachable")

	key := createCluster(t, r, "finalize-holds", cleanupCluster)

	reconcile(t, r, key)
	deleteCluster(t, r, key)

	result := reconcile(t, r, key)

	if result.RequeueAfter == 0 {
		t.Errorf("no requeue after a failed cleanup")
	}

	if gone(t, r, key) {
		t.Fatalf("cluster released though its etcd keys are still there")
	}

	warned := false
	for len(recorder.Events) > 0 {
		if ev := <-recorder.Events; contains(ev, eventClusterPurgeErr) {
			warned = true
		}
	}

	if !warned {
		t.Errorf("no %s event for the failed cleanup", eventClusterPurgeErr)
	}

	// It recovers on its own once etcd answers again.
	purge.err = nil

	reconcile(t, r, key)

	if !gone(t, r, key) {
		t.Errorf("cluster not released after the cleanup succeeded")
	}
}

// TestFinalizeSkipsCleanupWhenTurnedOff honours the policy as it stands at
// deletion, not as it stood when the finalizer was added.
func TestFinalizeSkipsCleanupWhenTurnedOff(t *testing.T) {
	r, _, purge := reconcilerWithPurge(t)
	key := createCluster(t, r, "finalize-optout", cleanupCluster)

	reconcile(t, r, key)

	// Opt out without a reconcile in between, so the finalizer is still there
	// when the delete arrives.
	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	cluster.Spec.Etcd.CleanupOnDelete = false
	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("opt out of cleanup: %v", err)
	}

	deleteCluster(t, r, key)
	reconcile(t, r, key)

	if purge.count() != 0 {
		t.Errorf("deleted etcd keys for a cluster that opted out")
	}

	if !gone(t, r, key) {
		t.Errorf("cluster not released though nothing had to be cleaned up")
	}
}

// TestReconcileIgnoresDeletedClusterWithoutFinalizer keeps a delete of an
// ordinary cluster free of any work: garbage collection is already taking its
// resources down.
func TestReconcileIgnoresDeletedClusterWithoutFinalizer(t *testing.T) {
	r, _, purge := reconcilerWithPurge(t)

	cluster := testCluster()
	cluster.Name = "no-finalizer"
	cluster.Namespace = "no-finalizer"
	cluster.DeletionTimestamp = new(metav1.Time)

	if _, err := r.finalize(t.Context(), cluster); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	if purge.count() != 0 {
		t.Errorf("cleaned up etcd for a cluster carrying no finalizer")
	}
}
