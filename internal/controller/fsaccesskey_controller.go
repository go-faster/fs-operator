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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sync"
	"time"

	"github.com/go-faster/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/fscluster"
	"github.com/go-faster/fs-operator/internal/fsclient"
	"github.com/go-faster/fs-operator/internal/keygen"
	"github.com/go-faster/fs-operator/internal/metrics"
)

// accessKeyFinalizer keeps the FSAccessKey around until the controller has
// removed its credential from the cluster's etcd key store, revoking it
// cluster-wide (fs §6.8).
const accessKeyFinalizer = "fs.go-faster.org/accesskey"

// credentialHashAnnotation fingerprints the credential material last pushed to
// the cluster. The admin API omits secrets from listings, so this is how the
// controller detects an imported Secret rotating (same access key, new secret)
// and re-creates the key with the new material.
const credentialHashAnnotation = "fs.go-faster.org/credential-hash"

// requeueAfterPending is how soon to re-check an FSAccessKey that is waiting on
// the cluster to become reachable.
const requeueAfterPending = 10 * time.Second

// FSAccessKeyReconciler reconciles an FSAccessKey: it resolves the credential
// (minting a generated one once, or reading an imported Secret) and reconciles
// it into the cluster's etcd-backed key store through the admin API — created,
// re-created on grant or material drift, and deleted on removal (fs §6.8).
type FSAccessKeyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Admin builds an admin client for a node's endpoint and bearer token; nil
	// uses a pooled default. Tests inject a fake.
	Admin func(baseURL, token string) (fsclient.Interface, error)

	poolOnce sync.Once
	pool     *fsclient.Pool
}

// adminClient builds an admin client, defaulting to a shared connection pool.
func (r *FSAccessKeyReconciler) adminClient(baseURL, token string) (fsclient.Interface, error) {
	if r.Admin != nil {
		return r.Admin(baseURL, token)
	}

	r.poolOnce.Do(func() { r.pool = fsclient.NewPool() })

	return r.pool.Client(baseURL, token)
}

// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsaccesskeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsaccesskeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsaccesskeys/finalizers,verbs=update
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile resolves an FSAccessKey's credential and reconciles it into the
// cluster's key store.
func (r *FSAccessKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if err != nil {
		metrics.RecordReconcileError("fsaccesskey")
	}

	return result, err
}

// reconcile is the pass itself; Reconcile wraps it to count failures.
func (r *FSAccessKeyReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	key := &fsv1alpha1.FSAccessKey{}
	if err := r.Get(ctx, req.NamespacedName, key); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !key.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, key)
	}

	if !controllerutil.ContainsFinalizer(key, accessKeyFinalizer) {
		controllerutil.AddFinalizer(key, accessKeyFinalizer)
		if err := r.Update(ctx, key); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "add finalizer")
		}
	}

	// Resolve the credential material (mint or import). A resolution problem is
	// a Ready condition, not a reconcile error: the fix is the user's.
	access, secret, cond := r.resolveCredential(ctx, key)
	if cond != nil {
		return r.report(ctx, key, "", *cond)
	}

	cond, requeue := r.ensureInCluster(ctx, key, access, secret)

	result, err := r.report(ctx, key, access, *cond)
	if err == nil && requeue && result.RequeueAfter == 0 {
		result.RequeueAfter = requeueAfterPending
	}

	log.V(1).Info("Reconciled FSAccessKey", "accessKey", access, "ready", cond.status)

	return result, err
}

// ensureInCluster creates or updates the credential in the cluster's key store
// via the admin API. A grant change or an imported-Secret rotation re-creates
// it (delete then create, since the admin API has no in-place update).
func (r *FSAccessKeyReconciler) ensureInCluster(ctx context.Context, key *fsv1alpha1.FSAccessKey, access, secret string) (*readyCondition, bool) {
	admin, cond, requeue := r.clusterAdmin(ctx, key)
	if cond != nil {
		return cond, requeue
	}

	keys, err := admin.ListAccessKeys(ctx)
	if err != nil {
		return falseCondition(fsv1alpha1.ReasonConfigReloadPending, "cluster not reachable yet"), true
	}

	desired := grantsFor(key)
	materialHash := credentialHash(access, secret)

	var existing *fsclient.AccessKey

	for i := range keys {
		if keys[i].AccessKey == access {
			existing = &keys[i]

			break
		}
	}

	switch {
	case existing == nil:
		if err := admin.CreateAccessKey(ctx, access, secret, desired); err != nil {
			return falseCondition(fsv1alpha1.ReasonConfigReloadPending, errors.Wrap(err, "create key").Error()), true
		}

		r.event(key, corev1.EventTypeNormal, "KeyCreated", "created credential "+access)
	case !grantsEqual(existing.Grants, desired) || key.Annotations[credentialHashAnnotation] != materialHash:
		// Grants changed, or an imported Secret rotated: re-create with the
		// current material and grants.
		if err := admin.DeleteAccessKey(ctx, access); err != nil {
			return falseCondition(fsv1alpha1.ReasonConfigReloadPending, errors.Wrap(err, "replace key").Error()), true
		}

		if err := admin.CreateAccessKey(ctx, access, secret, desired); err != nil {
			return falseCondition(fsv1alpha1.ReasonConfigReloadPending, errors.Wrap(err, "recreate key").Error()), true
		}

		r.event(key, corev1.EventTypeNormal, "KeyUpdated", "updated credential "+access)
	}

	// Record the applied material fingerprint so a later rotation is detected.
	if err := r.stampCredentialHash(ctx, key, materialHash); err != nil {
		return falseCondition(fsv1alpha1.ReasonReconcileError, err.Error()), false
	}

	return trueCondition(fsv1alpha1.ReasonKeyAccepted, "credential accepted by the cluster"), false
}

// resolveCredential returns the access/secret halves of the key, or a Ready
// condition explaining why it cannot.
func (r *FSAccessKeyReconciler) resolveCredential(ctx context.Context, key *fsv1alpha1.FSAccessKey) (access, secret string, cond *readyCondition) {
	if ref := key.Spec.ExistingSecretRef; ref != nil && ref.Name != "" {
		return r.readImported(ctx, key, ref.Name)
	}

	return r.ensureGenerated(ctx, key)
}

// readImported reads and validates a user-managed credential Secret.
func (r *FSAccessKeyReconciler) readImported(ctx context.Context, key *fsv1alpha1.FSAccessKey, name string) (string, string, *readyCondition) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", falseCondition(fsv1alpha1.ReasonSecretNotFound, "imported Secret "+name+" not found")
		}

		return "", "", falseCondition(fsv1alpha1.ReasonSecretInvalid, err.Error())
	}

	access := string(secret.Data[fscluster.AccessKeyKey])
	secretKey := string(secret.Data[fscluster.SecretKeyKey])

	if access == "" || secretKey == "" {
		return "", "", falseCondition(fsv1alpha1.ReasonSecretInvalid,
			"imported Secret "+name+" needs non-empty "+fscluster.AccessKeyKey+" and "+fscluster.SecretKeyKey)
	}

	if len(secretKey) < fscluster.MinSecretKeyLength {
		return "", "", falseCondition(fsv1alpha1.ReasonWeakSecretKey, "secret-key must be at least 16 characters")
	}

	return access, secretKey, nil
}

// ensureGenerated mints the credential once into an owned Secret and reads it
// back. The Secret is never rewritten, so the credential is stable.
func (r *FSAccessKeyReconciler) ensureGenerated(ctx context.Context, key *fsv1alpha1.FSAccessKey) (string, string, *readyCondition) {
	name := fscluster.AccessKeySecretName(key)

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: name}, secret)
	switch {
	case err == nil:
		access := string(secret.Data[fscluster.AccessKeyKey])
		secretKey := string(secret.Data[fscluster.SecretKeyKey])
		if access == "" || secretKey == "" {
			return "", "", falseCondition(fsv1alpha1.ReasonSecretInvalid,
				"credential Secret "+name+" is missing its key material")
		}

		return access, secretKey, nil
	case !apierrors.IsNotFound(err):
		return "", "", falseCondition(fsv1alpha1.ReasonSecretInvalid, err.Error())
	}

	// The credential's endpoint needs the cluster; resolve it first so a
	// generated Secret is always self-contained.
	cluster := &fsv1alpha1.FSCluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Spec.ClusterRef.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", falseCondition(fsv1alpha1.ReasonClusterNotFound,
				"FSCluster "+key.Spec.ClusterRef.Name+" not found")
		}

		return "", "", falseCondition(fsv1alpha1.ReasonReconcileError, err.Error())
	}

	access, err := keygen.AccessKey()
	if err != nil {
		return "", "", falseCondition(fsv1alpha1.ReasonReconcileError, err.Error())
	}

	secretKey, err := keygen.Token()
	if err != nil {
		return "", "", falseCondition(fsv1alpha1.ReasonReconcileError, err.Error())
	}

	endpoint := fscluster.S3Endpoint(cluster.Name, cluster.Namespace, fscluster.S3Port,
		cluster.Spec.S3.TLS.SecretName != "")

	owned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace},
		StringData: map[string]string{
			fscluster.AccessKeyKey: access,
			fscluster.SecretKeyKey: secretKey,
			fscluster.EndpointKey:  endpoint,
		},
	}

	if err := controllerutil.SetControllerReference(key, owned, r.Scheme); err != nil {
		return "", "", falseCondition(fsv1alpha1.ReasonReconcileError, err.Error())
	}

	if err := r.Create(ctx, owned); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", "", falseCondition(fsv1alpha1.ReasonReconcileError, errors.Wrap(err, "create credential secret").Error())
	}

	return access, secretKey, nil
}

// clusterAdmin resolves an admin client to a node of the key's cluster.
func (r *FSAccessKeyReconciler) clusterAdmin(ctx context.Context, key *fsv1alpha1.FSAccessKey) (fsclient.Interface, *readyCondition, bool) {
	cluster := &fsv1alpha1.FSCluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Spec.ClusterRef.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, falseCondition(fsv1alpha1.ReasonClusterNotFound,
				"FSCluster "+key.Spec.ClusterRef.Name+" not found"), true
		}

		return nil, falseCondition(fsv1alpha1.ReasonReconcileError, err.Error()), true
	}

	nodes := fscluster.Nodes(cluster)
	if len(nodes) == 0 {
		return nil, falseCondition(fsv1alpha1.ReasonClusterNotReady, "cluster has no nodes yet"), true
	}

	token, err := r.adminToken(ctx, cluster)
	if err != nil {
		return nil, falseCondition(fsv1alpha1.ReasonClusterNotReady, "admin token unavailable"), true
	}

	admin, err := r.adminClient(fscluster.AdminURL(cluster.Name, cluster.Namespace, nodes[0].Name), token)
	if err != nil {
		return nil, falseCondition(fsv1alpha1.ReasonClusterNotReady, err.Error()), true
	}

	return admin, nil, false
}

// adminToken reads the cluster's admin bearer token from its Secret.
func (r *FSAccessKeyReconciler) adminToken(ctx context.Context, cluster *fsv1alpha1.FSCluster) (string, error) {
	secret := &corev1.Secret{}
	name := fscluster.AdminTokenSecretName(cluster.Name)
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
		return "", err
	}

	token := string(secret.Data[fscluster.AdminTokenKey])
	if token == "" {
		return "", errors.Errorf("admin token secret %q has no %q", name, fscluster.AdminTokenKey)
	}

	return token, nil
}

// stampCredentialHash records the applied material fingerprint on the key.
func (r *FSAccessKeyReconciler) stampCredentialHash(ctx context.Context, key *fsv1alpha1.FSAccessKey, want string) error {
	if key.Annotations[credentialHashAnnotation] == want {
		return nil
	}

	patch := client.MergeFrom(key.DeepCopy())
	if key.Annotations == nil {
		key.Annotations = map[string]string{}
	}

	key.Annotations[credentialHashAnnotation] = want

	if err := r.Patch(ctx, key, patch); err != nil {
		return errors.Wrap(err, "stamp credential hash")
	}

	return nil
}

// finalize revokes the key: it is deleted from the cluster's key store, then
// the finalizer is removed (which lets the owned credential Secret be
// garbage-collected). A cluster that is already gone cannot hold the credential.
func (r *FSAccessKeyReconciler) finalize(ctx context.Context, key *fsv1alpha1.FSAccessKey) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(key, accessKeyFinalizer) {
		return ctrl.Result{}, nil
	}

	if key.Status.AccessKey != "" {
		admin, cond, _ := r.clusterAdmin(ctx, key)
		switch {
		case cond != nil && cond.reason == fsv1alpha1.ReasonClusterNotFound:
			// The cluster (and its etcd) is gone; nothing to revoke.
		case cond != nil:
			// Cluster unreachable: retry rather than leave a live credential.
			return ctrl.Result{RequeueAfter: requeueAfterPending}, nil
		default:
			if err := admin.DeleteAccessKey(ctx, key.Status.AccessKey); err != nil {
				return ctrl.Result{RequeueAfter: requeueAfterPending}, nil //nolint:nilerr // retry, not fail
			}

			r.event(key, corev1.EventTypeNormal, "KeyDeleted", "revoked credential "+key.Status.AccessKey)
		}
	}

	controllerutil.RemoveFinalizer(key, accessKeyFinalizer)
	if err := r.Update(ctx, key); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "remove finalizer")
	}

	return ctrl.Result{}, nil
}

// report writes status: the access key, observed generation and Ready
// condition.
func (r *FSAccessKeyReconciler) report(ctx context.Context, key *fsv1alpha1.FSAccessKey, access string, cond readyCondition) (ctrl.Result, error) {
	patch := client.MergeFrom(key.DeepCopy())

	key.Status.ObservedGeneration = key.Generation
	if access != "" {
		key.Status.AccessKey = access
	}

	meta.SetStatusCondition(&key.Status.Conditions, metav1.Condition{
		Type:               fsv1alpha1.ConditionReady,
		Status:             cond.status,
		Reason:             cond.reason,
		Message:            cond.message,
		ObservedGeneration: key.Generation,
	})

	if err := r.Status().Patch(ctx, key, patch); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "update status")
	}

	return ctrl.Result{}, nil
}

func (r *FSAccessKeyReconciler) event(key *fsv1alpha1.FSAccessKey, eventtype, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(key, eventtype, reason, message)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *FSAccessKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fsv1alpha1.FSAccessKey{}).
		Owns(&corev1.Secret{}).
		// An imported credential Secret rotating must re-push the key.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.importedSecretToKeys)).
		Named("fsaccesskey").
		Complete(r)
}

// importedSecretToKeys maps a user-managed Secret to the FSAccessKeys that
// import it, so an external rotation re-reconciles them.
func (r *FSAccessKeyReconciler) importedSecretToKeys(ctx context.Context, obj client.Object) []ctrl.Request {
	var list fsv1alpha1.FSAccessKeyList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	var reqs []ctrl.Request

	for i := range list.Items {
		ref := list.Items[i].Spec.ExistingSecretRef
		if ref != nil && ref.Name == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace, Name: list.Items[i].Name,
			}})
		}
	}

	return reqs
}

// grantsFor converts an FSAccessKey's API grants to the admin-client form.
func grantsFor(key *fsv1alpha1.FSAccessKey) []fsclient.Grant {
	out := make([]fsclient.Grant, 0, len(key.Spec.Grants))
	for _, g := range key.Spec.Grants {
		out = append(out, fsclient.Grant{Bucket: g.Bucket, Permission: g.Permission})
	}

	return out
}

// grantsEqual reports whether two grant sets are equal, order-independent.
func grantsEqual(a, b []fsclient.Grant) bool {
	if len(a) != len(b) {
		return false
	}

	key := func(g fsclient.Grant) string { return g.Bucket + "\x00" + g.Permission }

	as := make([]string, len(a))
	for i, g := range a {
		as[i] = key(g)
	}

	bs := make([]string, len(b))
	for i, g := range b {
		bs[i] = key(g)
	}

	slices.Sort(as)
	slices.Sort(bs)

	return slices.Equal(as, bs)
}

// credentialHash fingerprints the credential material.
func credentialHash(access, secret string) string {
	sum := sha256.Sum256([]byte(access + "\x00" + secret))

	return hex.EncodeToString(sum[:8])
}
