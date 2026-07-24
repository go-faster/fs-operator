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
	"testing"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

const assetsBucket = "assets"

// TestReconcilePublicReadBuckets checks that the cluster-wide public-read list
// is reconciled through the admin API (fs §6.8): it is applied once nodes are
// serving, and edits converge — matching the etcd credential store rather than
// the config file.
func TestReconcilePublicReadBuckets(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "publicread", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Auth.PublicReadBuckets = []string{publicBucket, assetsBucket}
	})

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)
	nodes := Nodes(&cluster)

	// No node is serving yet, so the step cannot reach the cluster.
	if got := admin.publicReadOf(); len(got) != 0 {
		t.Errorf("public-read applied before any node was serving: %v", got)
	}

	// Bring the nodes up; the step pushes the desired list.
	for _, node := range nodes {
		serving(t, r, key, node)
	}

	reconcile(t, r, key)

	if got := admin.publicReadOf(); !samePublicRead(got, []string{publicBucket, assetsBucket}) {
		t.Errorf("public-read = %v, want [%s assets]", got, publicBucket)
	}

	// Removing a bucket from the spec reconciles the cluster list down.
	get(t, r, key.Namespace, key.Name, &cluster)
	cluster.Spec.Auth.PublicReadBuckets = []string{assetsBucket}

	if err := r.Update(t.Context(), &cluster); err != nil {
		t.Fatalf("update public-read list: %v", err)
	}

	reconcile(t, r, key)

	if got := admin.publicReadOf(); !samePublicRead(got, []string{assetsBucket}) {
		t.Errorf("public-read = %v, want [assets] after removal", got)
	}
}
