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

	"github.com/minio/minio-go/v7"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

var _ = Describe("FSBucket Controller", func() {
	const namespace = "default"

	ctx := context.Background()

	It("creates the S3 bucket, applies the scheme override and becomes Ready", func() {
		makeCluster(ctx, "bkt-cluster")

		s3 := newFakeS3()
		admin := newFakeAdmin()
		r := &FSBucketReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			S3: s3.factory(), Admin: admin.client,
		}

		bucket := &fsv1alpha1.FSBucket{
			ObjectMeta: metav1.ObjectMeta{Name: "media", Namespace: namespace},
			Spec: fsv1alpha1.FSBucketSpec{
				ClusterRef: fsv1alpha1.ClusterReference{Name: "bkt-cluster"},
				Scheme:     "ec:4,2",
			},
		}
		Expect(k8sClient.Create(ctx, bucket)).To(Succeed())

		key := types.NamespacedName{Name: "media", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(s3.buckets["media"]).To(BeTrue(), "bucket should have been created")

		Expect(k8sClient.Get(ctx, key, bucket)).To(Succeed())
		Expect(bucket.Status.Scheme).To(Equal("ec:4,2"))
		ready := meta.FindStatusCondition(bucket.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonBucketReady))
	})

	It("creates the bucket on a single-node cluster, which has no schemes", func() {
		makeSingleNodeCluster(ctx, "solo-cluster")

		s3 := newFakeS3()
		admin := newFakeAdmin()
		r := &FSBucketReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			S3: s3.factory(), Admin: admin.client,
		}

		bucket := &fsv1alpha1.FSBucket{
			ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: namespace},
			Spec:       fsv1alpha1.FSBucketSpec{ClusterRef: fsv1alpha1.ClusterReference{Name: "solo-cluster"}},
		}
		Expect(k8sClient.Create(ctx, bucket)).To(Succeed())

		key := types.NamespacedName{Name: "solo", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(s3.buckets["solo"]).To(BeTrue(), "bucket should have been created")

		Expect(k8sClient.Get(ctx, key, bucket)).To(Succeed())
		ready := meta.FindStatusCondition(bucket.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(bucket.Status.Scheme).To(BeEmpty())
	})

	It("refuses a per-bucket scheme on a single-node cluster", func() {
		makeSingleNodeCluster(ctx, "solo-scheme-cluster")

		s3 := newFakeS3()
		admin := newFakeAdmin()
		r := &FSBucketReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			S3: s3.factory(), Admin: admin.client,
		}

		bucket := &fsv1alpha1.FSBucket{
			ObjectMeta: metav1.ObjectMeta{Name: "solo-ec", Namespace: namespace},
			Spec: fsv1alpha1.FSBucketSpec{
				ClusterRef: fsv1alpha1.ClusterReference{Name: "solo-scheme-cluster"},
				Scheme:     "rf3",
			},
		}
		Expect(k8sClient.Create(ctx, bucket)).To(Succeed())

		key := types.NamespacedName{Name: "solo-ec", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, bucket)).To(Succeed())
		ready := meta.FindStatusCondition(bucket.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonSchemeRejected))
	})

	It("reports SchemeRejected for a scheme the cluster cannot host", func() {
		makeCluster(ctx, "reject-cluster")

		s3 := newFakeS3()
		admin := newFakeAdmin()
		admin.rejectScheme = "ec:10,4"
		r := &FSBucketReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			S3: s3.factory(), Admin: admin.client,
		}

		bucket := &fsv1alpha1.FSBucket{
			ObjectMeta: metav1.ObjectMeta{Name: "wide", Namespace: namespace},
			Spec: fsv1alpha1.FSBucketSpec{
				ClusterRef: fsv1alpha1.ClusterReference{Name: "reject-cluster"},
				Scheme:     "ec:10,4",
			},
		}
		Expect(k8sClient.Create(ctx, bucket)).To(Succeed())

		key := types.NamespacedName{Name: "wide", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, bucket)).To(Succeed())
		ready := meta.FindStatusCondition(bucket.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonSchemeRejected))
	})

	// A cluster that adopted a populated etcd prefix (SPEC §8.6) serves S3 and
	// rejects the operator's root credential, so every bucket of it fails here.
	// The condition has to carry that error: reported as a reachability problem
	// it sends the reader to the network, and the cause is in the key store.
	It("reports what the S3 call actually failed with", func() {
		makeCluster(ctx, "unauth-cluster")

		s3 := newFakeS3()
		s3.existsErr = minio.ErrorResponse{
			Code:    "InvalidAccessKeyId",
			Message: "The AWS access key Id you provided does not exist in our records.",
		}
		admin := newFakeAdmin()
		r := &FSBucketReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			S3: s3.factory(), Admin: admin.client,
		}

		bucket := &fsv1alpha1.FSBucket{
			ObjectMeta: metav1.ObjectMeta{Name: "locked", Namespace: namespace},
			Spec: fsv1alpha1.FSBucketSpec{
				ClusterRef: fsv1alpha1.ClusterReference{Name: "unauth-cluster"},
			},
		}
		Expect(k8sClient.Create(ctx, bucket)).To(Succeed())

		key := types.NamespacedName{Name: "locked", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, bucket)).To(Succeed())
		ready := meta.FindStatusCondition(bucket.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonClusterNotReady))
		Expect(ready.Message).To(ContainSubstring("does not exist in our records"))
	})

	It("refuses to delete a non-empty bucket under reclaimPolicy Delete", func() {
		makeCluster(ctx, "del-cluster")

		s3 := newFakeS3()
		s3.buckets["logs"] = true
		s3.nonEmpty["logs"] = true
		admin := newFakeAdmin()
		r := &FSBucketReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			S3: s3.factory(), Admin: admin.client,
		}

		bucket := &fsv1alpha1.FSBucket{
			ObjectMeta: metav1.ObjectMeta{Name: "logs", Namespace: namespace},
			Spec: fsv1alpha1.FSBucketSpec{
				ClusterRef:    fsv1alpha1.ClusterReference{Name: "del-cluster"},
				ReclaimPolicy: fsv1alpha1.ReclaimDelete,
			},
		}
		Expect(k8sClient.Create(ctx, bucket)).To(Succeed())

		key := types.NamespacedName{Name: "logs", Namespace: namespace}
		// First reconcile establishes the finalizer and readiness.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Delete(ctx, bucket)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// The finalizer holds and the bucket is still there.
		Expect(k8sClient.Get(ctx, key, bucket)).To(Succeed())
		Expect(s3.buckets["logs"]).To(BeTrue())
		ready := meta.FindStatusCondition(bucket.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonBucketNotEmpty))

		// Once emptied, the next reconcile deletes it and releases the finalizer.
		s3.nonEmpty["logs"] = false
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(s3.buckets).NotTo(HaveKey("logs"))
		Expect(k8sClient.Get(ctx, key, bucket)).To(MatchError(ContainSubstring("not found")))
	})
})
