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

	reconcileKey := func(r *FSAccessKeyReconciler, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())
	}

	readyOf := func(name string) *metav1.Condition {
		ak := &fsv1alpha1.FSAccessKey{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ak)).To(Succeed())

		return meta.FindStatusCondition(ak.Status.Conditions, fsv1alpha1.ConditionReady)
	}

	It("mints a credential and is Ready once every node accepts it", func() {
		makeCluster(ctx, "ak-cluster")

		admin := newFakeAdmin()
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		Expect(k8sClient.Create(ctx, &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "app-writer", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef: fsv1alpha1.ClusterReference{Name: "ak-cluster"},
				Grants:     grants,
			},
		})).To(Succeed())

		reconcileKey(r, "app-writer")

		// The owned credential Secret exists with minted material.
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "app-writer-credentials", Namespace: namespace}, secret)).To(Succeed())
		access := string(secret.Data[fscluster.AccessKeyKey])
		Expect(access).To(HavePrefix("AK"))
		Expect(len(secret.Data[fscluster.SecretKeyKey])).To(BeNumerically(">=", fscluster.MinSecretKeyLength))
		Expect(secret.Data[fscluster.EndpointKey]).NotTo(BeEmpty())

		// The FSCluster controller renders it into every node; until the
		// nodes have reloaded, the key is not Ready.
		Expect(readyOf("app-writer").Reason).To(Equal(fsv1alpha1.ReasonConfigReloadPending))

		// One node of three is not enough: a client hitting the other two
		// would be refused.
		admin.accept(access, fscluster.AdminURL("ak-cluster", namespace, "ak-cluster-0"))
		reconcileKey(r, "app-writer")
		ready := readyOf("app-writer")
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(ContainSubstring("ak-cluster-1"))

		admin.accept(access)
		reconcileKey(r, "app-writer")
		ready = readyOf("app-writer")
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(fsv1alpha1.ReasonKeyAccepted))
	})

	It("holds the deletion until no node accepts the credential", func() {
		makeCluster(ctx, "revoke-cluster")

		admin := newFakeAdmin()
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		ak := &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "temp-key", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef: fsv1alpha1.ClusterReference{Name: "revoke-cluster"},
				Grants:     grants,
			},
		}
		Expect(k8sClient.Create(ctx, ak)).To(Succeed())

		key := types.NamespacedName{Name: "temp-key", Namespace: namespace}
		reconcileKey(r, "temp-key")

		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed())
		access := ak.Status.AccessKey
		admin.accept(access)

		Expect(k8sClient.Delete(ctx, ak)).To(Succeed())
		reconcileKey(r, "temp-key")
		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed(), "deleted while every node still accepts the key")

		// The FSCluster re-renders without it and reloads.
		admin.revoke(access)
		reconcileKey(r, "temp-key")
		Expect(k8sClient.Get(ctx, key, ak)).To(MatchError(ContainSubstring("not found")))
	})

	It("moves the credential fingerprint when an imported Secret rotates", func() {
		makeCluster(ctx, "rotate-cluster")
		makeSecret(ctx, "rotating-creds", map[string]string{
			fscluster.AccessKeyKey: "AKROTATE",
			fscluster.SecretKeyKey: "the-first-secret-key-value",
		})

		admin := newFakeAdmin()
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		ak := &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "rotating", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef:        fsv1alpha1.ClusterReference{Name: "rotate-cluster"},
				ExistingSecretRef: &corev1.LocalObjectReference{Name: "rotating-creds"},
				Grants:            grants,
			},
		}
		Expect(k8sClient.Create(ctx, ak)).To(Succeed())

		key := types.NamespacedName{Name: "rotating", Namespace: namespace}
		reconcileKey(r, "rotating")
		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed())
		before := ak.Annotations[credentialHashAnnotation]
		Expect(before).NotTo(BeEmpty())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rotating-creds", Namespace: namespace}, secret)).To(Succeed())
		secret.Data[fscluster.SecretKeyKey] = []byte("the-second-secret-key-value")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		// The FSCluster watches FSAccessKeys, not their Secrets: the new
		// fingerprint is what makes it re-render and reload.
		reconcileKey(r, "rotating")
		Expect(k8sClient.Get(ctx, key, ak)).To(Succeed())
		Expect(ak.Annotations[credentialHashAnnotation]).NotTo(Equal(before))
	})

	It("refuses an imported credential with a too-short secret key", func() {
		makeCluster(ctx, "weak-cluster")
		makeSecret(ctx, "weak-creds", map[string]string{
			fscluster.AccessKeyKey: "AKUSER",
			fscluster.SecretKeyKey: "tooshort", // < 16 chars
		})

		admin := newFakeAdmin()
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		Expect(k8sClient.Create(ctx, &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "imported-weak", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef:        fsv1alpha1.ClusterReference{Name: "weak-cluster"},
				ExistingSecretRef: &corev1.LocalObjectReference{Name: "weak-creds"},
				Grants:            grants,
			},
		})).To(Succeed())

		reconcileKey(r, "imported-weak")

		ready := readyOf("imported-weak")
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
		admin.accept("AKIMPORTED")
		r := &FSAccessKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Admin: admin.client}

		Expect(k8sClient.Create(ctx, &fsv1alpha1.FSAccessKey{
			ObjectMeta: metav1.ObjectMeta{Name: "imported-ok", Namespace: namespace},
			Spec: fsv1alpha1.FSAccessKeySpec{
				ClusterRef:        fsv1alpha1.ClusterReference{Name: "imp-cluster"},
				ExistingSecretRef: &corev1.LocalObjectReference{Name: "vault-creds"},
				Grants:            grants,
			},
		})).To(Succeed())

		reconcileKey(r, "imported-ok")

		ak := &fsv1alpha1.FSAccessKey{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "imported-ok", Namespace: namespace}, ak)).To(Succeed())
		Expect(ak.Status.AccessKey).To(Equal("AKIMPORTED"))
		Expect(readyOf("imported-ok").Status).To(Equal(metav1.ConditionTrue))
	})
})
