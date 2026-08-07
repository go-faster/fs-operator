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

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/fscluster"
)

// testNamespace is where the tenancy controller specs create their fixtures.
const testNamespace = "default"

// makeCluster creates a minimal valid FSCluster plus the root-credential and
// admin-token Secrets the tenancy controllers read, in the test namespace.
func makeCluster(ctx context.Context, name string) {
	namespace := testNamespace
	nodes := int32(3)

	cluster := &fsv1alpha1.FSCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: fsv1alpha1.FSClusterSpec{
			Topology: fsv1alpha1.TopologySpec{
				Nodes:           &nodes,
				PodAntiAffinity: fsv1alpha1.AntiAffinityPreferred,
			},
			Storage: fsv1alpha1.StorageSpec{
				Disks: []fsv1alpha1.DiskSpec{{Name: "d0", Size: resource.MustParse("10Gi")}},
			},
			Etcd: fsv1alpha1.EtcdSpec{
				External: &fsv1alpha1.ExternalEtcdSpec{Endpoints: []string{"http://etcd.default.svc:2379"}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

	makeSecret(ctx, fscluster.RootCredentialsSecretName(name), map[string]string{
		fscluster.AccessKeyKey: "AKROOT",
		fscluster.SecretKeyKey: "root-secret-key-0123456789",
	})
	makeSecret(ctx, fscluster.AdminTokenSecretName(name), map[string]string{
		fscluster.AdminTokenKey: "admin-token-value",
	})
}

// makeSingleNodeCluster creates the development shape: one node on fs's
// filesystem backend, with no etcd and no replication.
func makeSingleNodeCluster(ctx context.Context, name string) {
	nodes := int32(1)

	cluster := &fsv1alpha1.FSCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: fsv1alpha1.FSClusterSpec{
			Topology: fsv1alpha1.TopologySpec{Nodes: &nodes},
			Storage: fsv1alpha1.StorageSpec{
				Disks: []fsv1alpha1.DiskSpec{{Name: "d0", Size: resource.MustParse("10Gi")}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

	makeSecret(ctx, fscluster.RootCredentialsSecretName(name), map[string]string{
		fscluster.AccessKeyKey: "AKROOT",
		fscluster.SecretKeyKey: "root-secret-key-0123456789",
	})
	makeSecret(ctx, fscluster.AdminTokenSecretName(name), map[string]string{
		fscluster.AdminTokenKey: "admin-token-value",
	})
}

// makeSecret creates an opaque Secret with the given string data in the test
// namespace.
func makeSecret(ctx context.Context, name string, data map[string]string) {
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		StringData: data,
	})).To(Succeed())
}
