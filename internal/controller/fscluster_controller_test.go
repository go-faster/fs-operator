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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// resource1Gi is a 1Gi quantity for test disks.
func resource1Gi() resource.Quantity {
	return resource.MustParse("1Gi")
}

var _ = Describe("FSCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		fscluster := &fsv1alpha1.FSCluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind FSCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, fscluster)
			if err != nil && errors.IsNotFound(err) {
				nodes := int32(3)
				resource := &fsv1alpha1.FSCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: fsv1alpha1.FSClusterSpec{
						Image: fsv1alpha1.ImageSpec{Tag: "v0.0.0-test"},
						Topology: fsv1alpha1.TopologySpec{
							Nodes: &nodes,
						},
						Storage: fsv1alpha1.StorageSpec{
							Disks: []fsv1alpha1.DiskSpec{
								{Name: "d0", Size: resource1Gi()},
							},
						},
						Etcd: fsv1alpha1.EtcdSpec{
							External: fsv1alpha1.ExternalEtcdSpec{
								Endpoints: []string{"http://etcd.test.svc:2379"},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &fsv1alpha1.FSCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance FSCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &FSClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
