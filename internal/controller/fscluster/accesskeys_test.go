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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// TestRenderMergesAccessKeys is the core of the tenancy render (SPEC §7): an
// FSAccessKey and its credential Secret must land in every node's config as a
// declarative auth key, and a key for another cluster must not.
func TestRenderMergesAccessKeys(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "akmerge", nil)
	ctx := t.Context()

	// The credential Secret the FSAccessKey controller would mint.
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-writer-credentials", Namespace: key.Namespace},
		StringData: map[string]string{
			AccessKeyKey: "AKMERGE123",
			SecretKeyKey: "merge-secret-key-0123456789",
		},
	}
	if err := r.Create(ctx, cred); err != nil {
		t.Fatalf("create credential secret: %v", err)
	}

	ak := &fsv1alpha1.FSAccessKey{
		ObjectMeta: metav1.ObjectMeta{Name: "app-writer", Namespace: key.Namespace},
		Spec: fsv1alpha1.FSAccessKeySpec{
			ClusterRef: fsv1alpha1.ClusterReference{Name: key.Name},
			Grants:     []fsv1alpha1.GrantSpec{{Bucket: "media-*", Permission: "write"}},
		},
	}
	if err := r.Create(ctx, ak); err != nil {
		t.Fatalf("create access key: %v", err)
	}

	// A key pointing at a different cluster must not leak into this render.
	other := &fsv1alpha1.FSAccessKey{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: key.Namespace},
		Spec: fsv1alpha1.FSAccessKeySpec{
			ClusterRef: fsv1alpha1.ClusterReference{Name: "another-cluster"},
			Grants:     []fsv1alpha1.GrantSpec{{Bucket: "*", Permission: "admin"}},
		},
	}
	otherCred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere-credentials", Namespace: key.Namespace},
		StringData: map[string]string{AccessKeyKey: "AKELSEWHERE", SecretKeyKey: "another-secret-key-01234567"},
	}
	if err := r.Create(ctx, otherCred); err != nil {
		t.Fatalf("create other secret: %v", err)
	}

	if err := r.Create(ctx, other); err != nil {
		t.Fatalf("create other access key: %v", err)
	}

	reconcile(t, r, key)

	var config corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(key.Name+"-0"), &config)
	rendered := string(config.Data[ConfigFileName])

	if !strings.Contains(rendered, "AKMERGE123") {
		t.Errorf("node config does not carry the access key:\n%s", rendered)
	}

	if !strings.Contains(rendered, "media-*") {
		t.Error("node config does not carry the grant pattern")
	}

	if strings.Contains(rendered, "AKELSEWHERE") {
		t.Error("node config leaked a key belonging to another cluster")
	}
}

// TestRenderSkipsWeakAccessKey is the render's defensive counterpart to the
// FSAccessKey controller's WeakSecretKey status: a credential Secret with a
// too-short secret half is left out rather than blocking the whole cluster.
func TestRenderSkipsWeakAccessKey(t *testing.T) {
	r, _ := reconciler(t)
	key := createCluster(t, r, "akweak", nil)
	ctx := t.Context()

	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "weak-credentials", Namespace: key.Namespace},
		StringData: map[string]string{AccessKeyKey: "AKWEAK", SecretKeyKey: "short"},
	}
	if err := r.Create(ctx, cred); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	ak := &fsv1alpha1.FSAccessKey{
		ObjectMeta: metav1.ObjectMeta{Name: "weak", Namespace: key.Namespace},
		Spec: fsv1alpha1.FSAccessKeySpec{
			ClusterRef: fsv1alpha1.ClusterReference{Name: key.Name},
			SecretName: "weak-credentials",
			Grants:     []fsv1alpha1.GrantSpec{{Bucket: "*", Permission: "read"}},
		},
	}
	if err := r.Create(ctx, ak); err != nil {
		t.Fatalf("create access key: %v", err)
	}

	reconcile(t, r, key)

	var config corev1.Secret
	get(t, r, key.Namespace, ConfigSecretName(key.Name+"-0"), &config)

	if strings.Contains(string(config.Data[ConfigFileName]), "AKWEAK") {
		t.Error("node config rendered a credential with a too-short secret key")
	}
}
