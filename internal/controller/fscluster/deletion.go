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

	"github.com/go-faster/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/etcdstore"
)

// ClusterFinalizer holds a deleted FSCluster until the etcd keys it asked to
// have cleaned up are gone (SPEC §8.6).
//
// It is carried only by clusters that opt in with etcd.cleanupOnDelete.
// Everything else the operator creates carries an owner reference and needs no
// finalizer: garbage collection takes it down, and each node's claim retention
// policy decides the fate of its data. etcd is the one thing outside that graph.
const ClusterFinalizer = "fs.go-faster.org/cluster"

// Event reasons the deletion path reports.
const (
	eventClusterPurging  = "EtcdCleanup"
	eventClusterPurged   = "EtcdCleanupComplete"
	eventClusterPurgeErr = "EtcdCleanupFailed"
)

// reconcileFinalizer keeps the finalizer in step with the cluster's cleanup
// policy: added while cleanupOnDelete is set, dropped when it is turned off.
//
// A cluster that does not opt in never carries it, so a delete of an ordinary
// cluster can never be held up by an etcd the operator cannot reach.
func (r *Reconciler) reconcileFinalizer(ctx context.Context, cluster *fsv1alpha1.FSCluster) error {
	// A managed etcd never needs the finalizer: it is owned by this cluster, so
	// garbage collection takes it and its volumes down. Holding the object open
	// to purge keys from an etcd that is being deleted underneath us would only
	// stall the delete on a control plane already on its way out.
	want := cluster.Spec.Etcd.CleanupOnDelete && !cluster.Spec.ManagedEtcd()
	if want == controllerutil.ContainsFinalizer(cluster, ClusterFinalizer) {
		return nil
	}

	if want {
		controllerutil.AddFinalizer(cluster, ClusterFinalizer)
	} else {
		controllerutil.RemoveFinalizer(cluster, ClusterFinalizer)
	}

	if err := r.Update(ctx, cluster); err != nil {
		return errors.Wrap(err, "update finalizer")
	}

	return nil
}

// finalize deletes the cluster's keys under its etcd prefix and then releases
// the object (SPEC §8.6).
//
// The order is what makes it correct. The nodes are still running at this point
// — garbage collection does not start until the object is actually gone — and a
// running node re-registers itself in etcd within its lease TTL. So the nodes
// are stopped first, and only once no pod is left do the keys go; otherwise the
// purge would race the cluster it is purging and leave half a topology behind.
//
// A purge that fails does not release the object: leaving the keys behind is
// the failure this cleanup exists to prevent, and a re-created cluster of the
// same name would adopt them (see reconcileRootCredential). The failure is
// reported as an event on every attempt, and the finalizer can always be
// removed by hand if the etcd endpoints are gone for good.
func (r *Reconciler) finalize(ctx context.Context, cluster *fsv1alpha1.FSCluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(cluster, ClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	// The policy may have been turned off after the finalizer was added, and a
	// delete is the last chance to honour the current answer.
	if cluster.Spec.Etcd.CleanupOnDelete {
		stopped, err := r.stopNodes(ctx, cluster)
		if err != nil {
			return ctrl.Result{}, err
		}

		if !stopped {
			r.Recorder.Event(cluster, corev1.EventTypeNormal, eventClusterPurging,
				"Stopping nodes before deleting the cluster's etcd keys")

			return ctrl.Result{RequeueAfter: pollInterval}, nil
		}

		if err := r.purgeEtcd(ctx, cluster); err != nil {
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, eventClusterPurgeErr,
				"Could not delete the cluster's etcd keys, retrying: %v", err)
			log.Error(err, "etcd cleanup failed")

			return ctrl.Result{RequeueAfter: pollInterval}, nil
		}
	}

	controllerutil.RemoveFinalizer(cluster, ClusterFinalizer)

	if err := r.Update(ctx, cluster); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "remove finalizer")
	}

	return ctrl.Result{}, nil
}

// stopNodes takes down everything of the cluster that talks to etcd — the node
// StatefulSets and any migration Job — and reports whether every pod has
// actually gone.
//
// Deleting the workload is not enough on its own: a node's pod outlives its
// StatefulSet for as long as its graceful shutdown takes, and it holds its etcd
// registration until it exits. A migration Job writes the schema version, and
// its pod lingers after it completes, so the Job is deleted with propagation
// rather than waited on — otherwise a finished migration would hold the cluster
// undeletable for as long as its pod is kept around.
func (r *Reconciler) stopNodes(ctx context.Context, cluster *fsv1alpha1.FSCluster) (bool, error) {
	sets, err := r.nodeSets(ctx, cluster)
	if err != nil {
		return false, err
	}

	for i := range sets {
		if err := r.deleteWorkload(ctx, &sets[i]); err != nil {
			return false, err
		}
	}

	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(SelectorLabels(cluster.Name)),
	); err != nil {
		return false, errors.Wrap(err, "list migration jobs")
	}

	for i := range jobs.Items {
		if err := r.deleteWorkload(ctx, &jobs.Items[i]); err != nil {
			return false, err
		}
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(SelectorLabels(cluster.Name)),
	); err != nil {
		return false, errors.Wrap(err, "list cluster pods")
	}

	return len(pods.Items) == 0, nil
}

// deleteWorkload deletes an object and the pods it owns, tolerating one that is
// already going or already gone.
func (r *Reconciler) deleteWorkload(ctx context.Context, object client.Object) error {
	if !object.GetDeletionTimestamp().IsZero() {
		return nil
	}

	background := metav1.DeletePropagationBackground

	err := r.Delete(ctx, object, &client.DeleteOptions{PropagationPolicy: &background})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "delete %T %q", object, object.GetName())
	}

	return nil
}

// purgeEtcd deletes everything the cluster wrote under its prefix.
func (r *Reconciler) purgeEtcd(ctx context.Context, cluster *fsv1alpha1.FSCluster) error {
	log := logf.FromContext(ctx)

	prefix := cluster.Spec.EtcdPrefix(cluster.Namespace, cluster.Name)
	cfg := etcdstore.Config{Endpoints: EtcdEndpoints(cluster)}

	deleted, err := r.purge(ctx, cfg, prefix)
	if err != nil {
		return err
	}

	log.Info("Deleted the cluster's etcd keys", "prefix", prefix, "keys", deleted)
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, eventClusterPurged,
		"Deleted %d etcd key(s) under %q", deleted, etcdstore.KeyRange(prefix))

	return nil
}

// purge is the etcd deletion, through the seam a test replaces.
func (r *Reconciler) purge(ctx context.Context, cfg etcdstore.Config, prefix string) (int64, error) {
	if r.EtcdPurge != nil {
		return r.EtcdPurge(ctx, cfg, prefix)
	}

	return etcdstore.DeletePrefix(ctx, cfg, prefix)
}
