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
	"fmt"
	"sync"

	"github.com/go-faster/errors"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/fscluster"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// bucketFinalizer keeps the FSBucket until its reclaim policy has been honoured
// (Retain: nothing; Delete: the S3 bucket is gone).
const bucketFinalizer = "fs.go-faster.org/bucket"

// S3Client is the S3 surface the bucket controller needs: it is a seam so a
// test stands in for a live cluster.
type S3Client interface {
	BucketExists(ctx context.Context, bucket string) (bool, error)
	MakeBucket(ctx context.Context, bucket string) error
	RemoveBucket(ctx context.Context, bucket string) error
}

// FSBucketReconciler reconciles an FSBucket: it ensures the S3 bucket exists in
// the referenced cluster, applies the per-bucket scheme override and honours
// the reclaim policy on delete (SPEC §6).
type FSBucketReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// S3 builds an S3 client for the cluster endpoint and root credentials; nil
	// uses minio. Admin builds an admin client for the scheme calls; nil uses a
	// pool. Tests inject fakes.
	S3    func(endpoint, accessKey, secretKey string, secure bool) (S3Client, error)
	Admin func(baseURL, token string) (fsclient.Interface, error)

	poolOnce sync.Once
	pool     *fsclient.Pool
}

func (r *FSBucketReconciler) adminClient(baseURL, token string) (fsclient.Interface, error) {
	if r.Admin != nil {
		return r.Admin(baseURL, token)
	}

	r.poolOnce.Do(func() { r.pool = fsclient.NewPool() })

	return r.pool.Client(baseURL, token)
}

// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsbuckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsbuckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsbuckets/finalizers,verbs=update
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile brings one FSBucket in line with its spec.
func (r *FSBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	bucket := &fsv1alpha1.FSBucket{}
	if err := r.Get(ctx, req.NamespacedName, bucket); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	name := bucket.Spec.BucketName
	if name == "" {
		name = bucket.Name
	}

	cluster := &fsv1alpha1.FSCluster{}
	clusterErr := r.Get(ctx, types.NamespacedName{Namespace: bucket.Namespace, Name: bucket.Spec.ClusterRef.Name}, cluster)

	if !bucket.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, bucket, name, cluster, clusterErr)
	}

	if !controllerutil.ContainsFinalizer(bucket, bucketFinalizer) {
		controllerutil.AddFinalizer(bucket, bucketFinalizer)
		if err := r.Update(ctx, bucket); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "add finalizer")
		}
	}

	if clusterErr != nil {
		if apierrors.IsNotFound(clusterErr) {
			return r.report(ctx, bucket, "", falseCondition(fsv1alpha1.ReasonClusterNotFound,
				"FSCluster "+bucket.Spec.ClusterRef.Name+" not found"), true)
		}

		return ctrl.Result{}, errors.Wrap(clusterErr, "get cluster")
	}

	s3, err := r.s3For(ctx, cluster)
	if err != nil {
		return r.report(ctx, bucket, "", falseCondition(fsv1alpha1.ReasonClusterNotReady, err.Error()), true)
	}

	// Ensure the bucket exists.
	switch exists, err := s3.BucketExists(ctx, name); {
	case err != nil:
		return r.report(ctx, bucket, "", falseCondition(fsv1alpha1.ReasonClusterNotReady,
			"cluster S3 not reachable yet"), true)
	case !exists:
		if err := s3.MakeBucket(ctx, name); err != nil {
			return r.report(ctx, bucket, "", falseCondition(fsv1alpha1.ReasonClusterNotReady,
				errors.Wrap(err, "create bucket").Error()), true)
		}

		r.event(bucket, corev1.EventTypeNormal, "Created", "created S3 bucket "+name)
	}

	// Apply the declarative scheme (empty clears any override → cluster
	// default) and record the effective scheme.
	effective, cond, requeue := r.reconcileScheme(ctx, cluster, name, bucket.Spec.Scheme)
	if cond != nil {
		return r.report(ctx, bucket, effective, cond, requeue)
	}

	log.V(1).Info("Reconciled FSBucket", "bucket", name, "scheme", effective)

	return r.report(ctx, bucket, effective, trueCondition(fsv1alpha1.ReasonBucketReady, "bucket ready"), false)
}

// reconcileScheme sets the bucket's scheme override (empty clears it) and
// returns the effective scheme. A rejected scheme is a terminal Ready=False
// (the user must fix the spec); an unreachable cluster requeues.
func (r *FSBucketReconciler) reconcileScheme(ctx context.Context, cluster *fsv1alpha1.FSCluster, bucket, scheme string) (string, *readyCondition, bool) {
	admin, err := r.clusterAdmin(ctx, cluster)
	if err != nil {
		return "", falseCondition(fsv1alpha1.ReasonClusterNotReady, err.Error()), true
	}

	res, err := admin.SetBucketScheme(ctx, bucket, scheme)
	if err != nil {
		if errors.Is(err, fsclient.ErrSchemeRejected) {
			return "", falseCondition(fsv1alpha1.ReasonSchemeRejected, err.Error()), false
		}

		return "", falseCondition(fsv1alpha1.ReasonClusterNotReady,
			"cluster admin not reachable yet"), true
	}

	return res.Scheme, nil, false
}

// finalize honours the reclaim policy: Retain drops the finalizer; Delete
// removes the S3 bucket, which fails while it is non-empty (Ready=False,
// BucketNotEmpty, retried). A missing or unreachable cluster cannot hold data
// hostage, so the finalizer is released.
func (r *FSBucketReconciler) finalize(ctx context.Context, bucket *fsv1alpha1.FSBucket, name string, cluster *fsv1alpha1.FSCluster, clusterErr error) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(bucket, bucketFinalizer) {
		return ctrl.Result{}, nil
	}

	if bucket.Spec.ReclaimPolicy == fsv1alpha1.ReclaimDelete && clusterErr == nil {
		s3, err := r.s3For(ctx, cluster)
		if err != nil {
			// Cannot reach the cluster to delete; retry rather than orphan.
			return r.report(ctx, bucket, "", falseCondition(fsv1alpha1.ReasonClusterNotReady, err.Error()), true)
		}

		if err := s3.RemoveBucket(ctx, name); err != nil && !isBucketNotFound(err) {
			if isBucketNotEmpty(err) {
				r.event(bucket, corev1.EventTypeWarning, "BucketNotEmpty",
					"refusing to delete non-empty bucket "+name)

				return r.report(ctx, bucket, "", falseCondition(fsv1alpha1.ReasonBucketNotEmpty,
					"bucket "+name+" is not empty; empty it or set reclaimPolicy: Retain"), true)
			}

			return r.report(ctx, bucket, "", falseCondition(fsv1alpha1.ReasonBucketError,
				errors.Wrap(err, "delete bucket").Error()), true)
		}

		r.event(bucket, corev1.EventTypeNormal, "Deleted", "deleted S3 bucket "+name)
	}

	controllerutil.RemoveFinalizer(bucket, bucketFinalizer)
	if err := r.Update(ctx, bucket); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "remove finalizer")
	}

	return ctrl.Result{}, nil
}

// s3For builds an S3 client for the cluster's client Service, authenticated
// with its root credentials.
func (r *FSBucketReconciler) s3For(ctx context.Context, cluster *fsv1alpha1.FSCluster) (S3Client, error) {
	secret := &corev1.Secret{}
	name := fscluster.RootCredentialsSource(cluster)
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
		return nil, errors.Wrap(err, "get root credentials")
	}

	access := string(secret.Data[fscluster.AccessKeyKey])
	secretKey := string(secret.Data[fscluster.SecretKeyKey])
	if access == "" || secretKey == "" {
		return nil, errors.Errorf("root credentials secret %q is incomplete", name)
	}

	secure := cluster.Spec.S3.TLS.SecretName != ""
	endpoint := fmt.Sprintf("%s.%s.svc:%d", fscluster.ClientServiceName(cluster.Name), cluster.Namespace, fscluster.S3Port)

	if r.S3 != nil {
		return r.S3(endpoint, access, secretKey, secure)
	}

	return newMinioClient(endpoint, access, secretKey, secure)
}

// clusterAdmin builds an admin client to a node, for the scheme calls.
func (r *FSBucketReconciler) clusterAdmin(ctx context.Context, cluster *fsv1alpha1.FSCluster) (fsclient.Interface, error) {
	nodes := fscluster.Nodes(cluster)
	if len(nodes) == 0 {
		return nil, errors.New("cluster has no nodes yet")
	}

	secret := &corev1.Secret{}
	name := fscluster.AdminTokenSecretName(cluster.Name)
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
		return nil, errors.Wrap(err, "get admin token")
	}

	token := string(secret.Data[fscluster.AdminTokenKey])
	if token == "" {
		return nil, errors.Errorf("admin token secret %q has no %q", name, fscluster.AdminTokenKey)
	}

	return r.adminClient(fscluster.AdminURL(cluster.Name, cluster.Namespace, nodes[0].Name), token)
}

// report writes the bucket's status: the effective scheme, observed generation
// and Ready condition.
func (r *FSBucketReconciler) report(ctx context.Context, bucket *fsv1alpha1.FSBucket, scheme string, cond *readyCondition, requeue bool) (ctrl.Result, error) {
	patch := client.MergeFrom(bucket.DeepCopy())

	bucket.Status.ObservedGeneration = bucket.Generation
	if scheme != "" {
		bucket.Status.Scheme = scheme
	}

	meta.SetStatusCondition(&bucket.Status.Conditions, metav1.Condition{
		Type:               fsv1alpha1.ConditionReady,
		Status:             cond.status,
		Reason:             cond.reason,
		Message:            cond.message,
		ObservedGeneration: bucket.Generation,
	})

	if err := r.Status().Patch(ctx, bucket, patch); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "update status")
	}

	if requeue {
		return ctrl.Result{RequeueAfter: requeueAfterPending}, nil
	}

	return ctrl.Result{}, nil
}

func (r *FSBucketReconciler) event(bucket *fsv1alpha1.FSBucket, eventtype, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(bucket, eventtype, reason, message)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *FSBucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fsv1alpha1.FSBucket{}).
		Named("fsbucket").
		Complete(r)
}

// newMinioClient adapts minio-go to the S3Client seam.
func newMinioClient(endpoint, access, secret string, secure bool) (S3Client, error) {
	cl, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(access, secret, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, errors.Wrap(err, "build s3 client")
	}

	return &minioClient{cl: cl}, nil
}

type minioClient struct{ cl *minio.Client }

func (m *minioClient) BucketExists(ctx context.Context, bucket string) (bool, error) {
	return m.cl.BucketExists(ctx, bucket)
}

func (m *minioClient) MakeBucket(ctx context.Context, bucket string) error {
	return m.cl.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
}

func (m *minioClient) RemoveBucket(ctx context.Context, bucket string) error {
	return m.cl.RemoveBucket(ctx, bucket)
}

// isBucketNotEmpty reports whether an S3 error is BucketNotEmpty.
func isBucketNotEmpty(err error) bool {
	return minio.ToErrorResponse(err).Code == "BucketNotEmpty"
}

// isBucketNotFound reports whether an S3 error is NoSuchBucket.
func isBucketNotFound(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchBucket"
}
