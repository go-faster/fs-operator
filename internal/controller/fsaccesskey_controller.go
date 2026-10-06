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
	"strings"
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

// accessKeyFinalizer keeps the FSAccessKey around until no node accepts its
// credential any more: deleting the object is revoking the key.
const accessKeyFinalizer = "fs.go-faster.org/accesskey"

// credentialHashAnnotation fingerprints the resolved credential material.
// Bumping it on a change (notably an imported Secret rotating: same access
// key, new secret) is an update to the FSAccessKey, which the FSCluster
// watches — so the cluster re-renders and reloads even though the
// FSAccessKey spec did not change.
const credentialHashAnnotation = "fs.go-faster.org/credential-hash"

// requeueAfterPending is how soon to re-check an FSAccessKey that is waiting on
// the cluster to become reachable.
const requeueAfterPending = 10 * time.Second

// FSAccessKeyReconciler reconciles an FSAccessKey: it resolves the credential
// (minting a generated one once, or reading an imported Secret), keeps a
// fingerprint that drives the cluster's config, and reports whether every node
// accepts the key (SPEC §7). The FSCluster controller is what renders the key
// into every node's config.
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

// Reconcile resolves an FSAccessKey's credential and reports whether the
// cluster accepts it.
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

	// Record the material fingerprint: a change to it is an update to this
	// object, which the FSCluster watches, so an imported Secret rotating
	// re-renders and reloads the cluster even though no spec changed.
	if err := r.stampCredentialHash(ctx, key, credentialHash(access, secret)); err != nil {
		return ctrl.Result{}, err
	}

	cond, requeue := r.verifyAccepted(ctx, key, access)

	result, err := r.report(ctx, key, access, *cond)
	if err == nil && requeue && result.RequeueAfter == 0 {
		result.RequeueAfter = requeueAfterPending
	}

	log.V(1).Info("Reconciled FSAccessKey", "accessKey", access, "ready", cond.status)

	return result, err
}

// verifyAccepted confirms that every node of the cluster accepts the key. The
// FSCluster controller renders the key into each node's config and reloads it
// (SPEC §7); this only reads the result back. Until every node lists the key it
// is not Ready and the reconcile requeues.
func (r *FSAccessKeyReconciler) verifyAccepted(ctx context.Context, key *fsv1alpha1.FSAccessKey, access string) (*readyCondition, bool) {
	listings, cond, requeue := r.nodeKeys(ctx, key)
	if cond != nil {
		return cond, requeue
	}

	var missing []string

	for node, keys := range listings {
		switch {
		case keys == nil:
			missing = append(missing, node+" (unreachable)")
		case !slices.Contains(keys, access):
			missing = append(missing, node)
		}
	}

	if len(missing) > 0 {
		slices.Sort(missing)

		return falseCondition(fsv1alpha1.ReasonConfigReloadPending,
			"waiting for node(s) to apply the credential: "+strings.Join(missing, ", ")), true
	}

	return trueCondition(fsv1alpha1.ReasonKeyAccepted, "credential accepted by every node"), false
}

// nodeKeys lists the access keys every node of the key's cluster accepts,
// keyed by node; a node that did not answer maps to nil.
func (r *FSAccessKeyReconciler) nodeKeys(
	ctx context.Context, key *fsv1alpha1.FSAccessKey,
) (map[string][]string, *readyCondition, bool) {
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

	listings := make(map[string][]string, len(nodes))

	for _, node := range nodes {
		listings[node.Name] = nil

		admin, err := r.adminClient(fscluster.AdminURL(cluster.Name, cluster.Namespace, node.Name), token)
		if err != nil {
			return nil, falseCondition(fsv1alpha1.ReasonClusterNotReady, err.Error()), true
		}

		keys, err := admin.AccessKeys(ctx)
		if err != nil {
			continue
		}

		if keys == nil {
			keys = []string{}
		}

		listings[node.Name] = keys
	}

	return listings, nil, false
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

// finalize waits for the key to be revoked: the FSCluster re-renders without
// it (the render skips keys being deleted) and reloads every node. The
// finalizer goes once no node that answers still accepts the key, which lets
// the owned credential Secret be garbage-collected. A node that does not answer
// is not waited for: it reads the re-rendered config when it next starts. A
// cluster that is already gone cannot hold the credential.
func (r *FSAccessKeyReconciler) finalize(ctx context.Context, key *fsv1alpha1.FSAccessKey) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(key, accessKeyFinalizer) {
		return ctrl.Result{}, nil
	}

	if access := key.Status.AccessKey; access != "" {
		listings, cond, _ := r.nodeKeys(ctx, key)

		switch {
		case cond != nil && cond.reason == fsv1alpha1.ReasonClusterNotFound:
			// The cluster is gone; nothing to revoke.
		case cond != nil:
			return ctrl.Result{RequeueAfter: requeueAfterPending}, nil
		default:
			for _, keys := range listings {
				if slices.Contains(keys, access) {
					return ctrl.Result{RequeueAfter: requeueAfterPending}, nil
				}
			}

			r.event(key, corev1.EventTypeNormal, "KeyDeleted", "revoked credential "+access)
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

// credentialHash fingerprints the credential material, so a change to it —
// an imported Secret rotating — is visible as an annotation change.
func credentialHash(access, secret string) string {
	sum := sha256.Sum256([]byte(access + "\x00" + secret))

	return hex.EncodeToString(sum[:8])
}
