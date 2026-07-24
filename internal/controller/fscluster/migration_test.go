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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// steady brings a cluster to a fully rolled-out, converged, config-current
// state — the precondition for a schema migration — and returns its nodes.
func steady(t *testing.T, r *Reconciler, admin *fakeAdmin, key types.NamespacedName) []Node {
	t.Helper()

	reconcile(t, r, key)

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	nodes := Nodes(&cluster)
	for _, node := range nodes {
		serving(t, r, key, node)
		admin.setApplied(nodeAdminURL(key, node), configRevision(t, r, key, node))
	}

	return nodes
}

// TestSchemaMigrationAuto covers the Auto policy end to end: a binary ahead of
// the cluster schema, once every node runs it and the cluster is converged,
// runs a migration Job; SchemaCurrent flips True only when the cluster schema
// catches up.
func TestSchemaMigrationAuto(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "migrate-auto", nil)

	steady(t, r, admin, key)

	// The deployed binary implements schema 5; the cluster is on 4.
	admin.setSchema(4, 5)

	reconcile(t, r, key)

	// The migration Job exists and the state is surfaced.
	jobName := MigrationJobName(key.Name, 5)

	var job batchv1.Job
	get(t, r, key.Namespace, jobName, &job)

	container := job.Spec.Template.Spec.Containers[0]
	if got, want := container.Args, []string{"cluster", migrateName, flagConfig, ConfigPath}; !equalStrings(got, want) {
		t.Errorf("job args = %v, want %v", got, want)
	}

	if c := condition(t, r, key, fsv1alpha1.ConditionSchemaCurrent); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("SchemaCurrent = %v, want False while migrating", c)
	}

	var cluster fsv1alpha1.FSCluster
	get(t, r, key.Namespace, key.Name, &cluster)

	if cluster.Status.SchemaVersion == nil || cluster.Status.SchemaVersion.Binary != 5 {
		t.Errorf("status.schemaVersion = %v, want binary 5", cluster.Status.SchemaVersion)
	}

	if cluster.Status.Update == nil || cluster.Status.Update.Phase != fsv1alpha1.UpdatePhaseMigrating {
		t.Errorf("update phase = %v, want Migrating", cluster.Status.Update)
	}

	// The Job completes and the cluster schema advances to 5.
	completeJob(t, r, &job)
	admin.setSchema(5, 5)

	reconcile(t, r, key)

	if c := condition(t, r, key, fsv1alpha1.ConditionSchemaCurrent); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("SchemaCurrent = %v, want True once the cluster reached the binary schema", c)
	}
}

// TestSchemaMigrationManual covers the Manual policy: a pending migration is
// surfaced but no Job is created.
func TestSchemaMigrationManual(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "migrate-manual", func(c *fsv1alpha1.FSCluster) {
		c.Spec.UpdatePolicy.SchemaMigration = fsv1alpha1.SchemaMigrationManual
	})

	steady(t, r, admin, key)
	admin.setSchema(4, 5)

	reconcile(t, r, key)

	c := condition(t, r, key, fsv1alpha1.ConditionSchemaCurrent)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != fsv1alpha1.ReasonMigrationPending {
		t.Fatalf("SchemaCurrent = %v, want False/%s", c, fsv1alpha1.ReasonMigrationPending)
	}

	var job batchv1.Job
	err := r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: MigrationJobName(key.Name, 5)}, &job)
	if err == nil {
		t.Error("a migration Job was created under the Manual policy")
	}
}

// TestSchemaMigrationWaitsForRollout keeps the migration from running before
// every node is on the new binary: a node not yet current blocks it.
func TestSchemaMigrationWaitsForRollout(t *testing.T) {
	r, _, admin := reconcilerWithAdmin(t)
	key := createCluster(t, r, "migrate-wait", nil)

	nodes := steady(t, r, admin, key)

	// One node has not applied the desired config yet (still rolling).
	admin.setApplied(nodeAdminURL(key, nodes[0]), "cfg-stale00000")
	admin.setSchema(4, 5)

	reconcile(t, r, key)

	if err := r.Get(t.Context(), types.NamespacedName{
		Namespace: key.Namespace, Name: MigrationJobName(key.Name, 5),
	}, &batchv1.Job{}); err == nil {
		t.Error("migration started before every node ran the new binary")
	}

	c := condition(t, r, key, fsv1alpha1.ConditionSchemaCurrent)
	if c == nil || c.Reason != fsv1alpha1.ReasonMigrationPending {
		t.Errorf("SchemaCurrent reason = %v, want %s", c, fsv1alpha1.ReasonMigrationPending)
	}
}

// completeJob marks a Job as succeeded, standing in for the Job controller.
func completeJob(t *testing.T, r *Reconciler, job *batchv1.Job) {
	t.Helper()

	now := metav1.Now()
	job.Status.Succeeded = 1
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	if err := r.Status().Update(t.Context(), job); err != nil {
		t.Fatalf("complete migration job: %v", err)
	}
}

// equalStrings compares two string slices.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
