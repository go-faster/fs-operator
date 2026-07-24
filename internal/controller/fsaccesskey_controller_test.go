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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/fscluster"
)

var _ = Describe("FSAccessKey Controller", func() {
	const namespace = "default"

	ctx := context.Background()

	grants := []fsv1alpha1.GrantSpec{{Bucket: "media-*", Permission: "write"}}

	It("mints a generated credential and becomes Ready once the cluster accepts it", func() {
		makeCluster(ctx, "ak-cluster")

		admin := newFakeAdmin()
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		ak := &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "app-writer", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef: fsv1alpha1.ClusterReference{Name: "ak-cluster"},
				Grants:     grants,
			},
		}
		Expect(k8sClient.Create(ctx, ak)).To(Succeed())

		key := types.NamespacedName{Name: "app-writer", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// The owned credential Secret exists with minted material.
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "app-writer-credentials", Namespace: namespace}, secret)).To(Succeed())
		access := string(secret.Data[fscluster.AccessKeyKey])
		Expect(access).To(HavePrefix("AK"))
		Expect(len(secret.Data[fscluster.SecretKeyKey])).To(BeNumerically(">=", fscluster.MinSecretKeyLength))
		Expect(secret.Data[fscluster.EndpointKey]).NotTo(BeEmpty())

		// Not yet accepted by the cluster: Ready is False, pending the reload.
		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed())
		Expect(ak.Status.AccessKey).To(Equal(access))
		ready := meta.FindStatusCondition(ak.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonConfigReloadPending))

		// Once the cluster reports the key, the next reconcile flips Ready.
		admin.addKey(access)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed())
		ready = meta.FindStatusCondition(ak.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonKeyAccepted))
	})

	It("refuses an imported credential with a too-short secret key", func() {
		makeCluster(ctx, "weak-cluster")
		makeSecret(ctx, "weak-creds", map[string]string{
			fscluster.AccessKeyKey: "AKUSER",
			fscluster.SecretKeyKey: "tooshort", // < 16 chars
		})

		admin := newFakeAdmin()
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		ak := &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "imported-weak", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef:        fsv1alpha1.ClusterReference{Name: "weak-cluster"},
				ExistingSecretRef: &corev1.LocalObjectReference{Name: "weak-creds"},
				Grants:            grants,
			},
		}
		Expect(k8sClient.Create(ctx, ak)).To(Succeed())

		key := types.NamespacedName{Name: "imported-weak", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed())
		ready := meta.FindStatusCondition(ak.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonWeakSecretKey))
	})

	It("imports a valid user-managed credential", func() {
		makeCluster(ctx, "imp-cluster")
		makeSecret(ctx, "vault-creds", map[string]string{
			fscluster.AccessKeyKey: "AKIMPORTED",
			fscluster.SecretKeyKey: "a-strong-imported-secret-key",
		})

		admin := newFakeAdmin()
		admin.addKey("AKIMPORTED")
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		ak := &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "imported-ok", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef:        fsv1alpha1.ClusterReference{Name: "imp-cluster"},
				ExistingSecretRef: &corev1.LocalObjectReference{Name: "vault-creds"},
				Grants:            grants,
			},
		}
		Expect(k8sClient.Create(ctx, ak)).To(Succeed())

		key := types.NamespacedName{Name: "imported-ok", Namespace: namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed())
		Expect(ak.Status.AccessKey).To(Equal("AKIMPORTED"))
		ready := meta.FindStatusCondition(ak.Status.Conditions, fsv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	})
})
