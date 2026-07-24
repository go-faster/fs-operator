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
	"context"
	"fmt"

	"github.com/go-faster/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
)

// reconcileMigration runs a schema migration once every node runs a binary
// whose schema is newer than the cluster's recorded one.
//
// fs's contract: a migration runs only after all nodes upgrade (an old binary
// refuses to join a schema-migrated cluster), it is etcd-elected and resumable,
// and it is safe to re-run. So the operator waits for the rollout to finish and
// the cluster to reconverge, then — under the Auto policy — runs `fs cluster
// migrate` as a Job and surfaces the state on SchemaCurrent. Under Manual it
// only reports the pending migration (SPEC §8.2).
func (r *Reconciler) reconcileMigration(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	if !p.convergence.known {
		// Schema versions are unknown until a node answers; say nothing.
		return pipeline.Continue()
	}

	clusterSchema, binarySchema := p.convergence.schemaVersion, p.convergence.binarySchema
	p.schema = &fsv1alpha1.SchemaVersionStatus{Cluster: int64(clusterSchema), Binary: int64(binarySchema)}

	if binarySchema <= clusterSchema {
		p.setCondition(fsv1alpha1.ConditionSchemaCurrent, metav1.ConditionTrue,
			fsv1alpha1.ReasonUpToDate, "Cluster schema matches the deployed binary")

		return pipeline.Continue()
	}

	// A migration is pending. Under Manual, surface it and stop.
	if p.cluster.Spec.UpdatePolicy.SchemaMigration == fsv1alpha1.SchemaMigrationManual {
		p.setCondition(fsv1alpha1.ConditionSchemaCurrent, metav1.ConditionFalse,
			fsv1alpha1.ReasonMigrationPending,
			fmt.Sprintf("Schema %d pending; run `fs cluster migrate` (schemaMigration: Manual)", binarySchema))

		return pipeline.Continue()
	}

	// Under Auto, migrate only after every node runs the new binary and the
	// cluster has reconverged — fs runs migrations post-upgrade.
	desired := int32(len(p.nodes)) //nolint:gosec // the topology is bounded at 16 nodes
	if p.health.current != desired || p.health.configCurrent != desired || !p.convergence.converged {
		p.setCondition(fsv1alpha1.ConditionSchemaCurrent, metav1.ConditionFalse,
			fsv1alpha1.ReasonMigrationPending,
			"Waiting for every node to run the new binary and the cluster to reconverge before migrating")

		return pipeline.Continue()
	}

	return r.runMigration(ctx, p, binarySchema)
}

// runMigration ensures the migration Job exists and reports its progress.
func (r *Reconciler) runMigration(ctx context.Context, p *pass, targetSchema int) (pipeline.Outcome, error) {
	job := NewMigrationJob(p.cluster, targetSchema, p.nodes[0].Name)

	// Create-once, keyed by the target schema: a completed migration is never
	// re-run, and re-running a live one is unnecessary (it is etcd-elected).
	if err := r.createOnce(ctx, p.cluster, job); err != nil {
		return pipeline.Outcome{}, err
	}

	var live batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return pipeline.RequeueAfter(pollInterval, "migration job starting")
		}

		return pipeline.Outcome{}, errors.Wrapf(err, "get migration job %q", job.Name)
	}

	p.update = &fsv1alpha1.UpdateStatus{Phase: fsv1alpha1.UpdatePhaseMigrating, StartedAt: jobStart(&live)}

	switch {
	case live.Status.Succeeded > 0:
		// Done: the cluster schema advances in etcd, and the next pass reads
		// binary == cluster and flips SchemaCurrent True. Keep waiting for that
		// confirmation rather than assert it from the Job alone.
		p.setCondition(fsv1alpha1.ConditionSchemaCurrent, metav1.ConditionFalse,
			fsv1alpha1.ReasonMigrationRunning, "Migration complete; awaiting the updated cluster schema")

		return pipeline.RequeueAfter(pollInterval, "confirming migrated schema")
	case failedJob(&live):
		r.Recorder.Eventf(p.object, corev1.EventTypeWarning, eventMigrationFailed,
			"Schema migration Job %q failed", job.Name)
		p.setCondition(fsv1alpha1.ConditionSchemaCurrent, metav1.ConditionFalse,
			fsv1alpha1.ReasonMigrationRunning, "Migration Job failed; see its pods")

		return pipeline.RequeueAfter(pollInterval, "migration job failed")
	default:
		p.setCondition(fsv1alpha1.ConditionSchemaCurrent, metav1.ConditionFalse,
			fsv1alpha1.ReasonMigrationRunning,
			fmt.Sprintf("Migrating cluster schema to %d", targetSchema))

		return pipeline.RequeueAfter(pollInterval, "migration in progress")
	}
}

// eventMigrationFailed marks a failed schema migration; migrateName is the
// migrate subcommand and the Job's container name.
const (
	eventMigrationFailed = "MigrationFailed"
	migrateName          = "migrate"
)

// failedJob reports whether a Job has a Failed condition set true.
func failedJob(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

// jobStart is a Job's start time, for the update status.
func jobStart(job *batchv1.Job) *metav1.Time {
	if job.Status.StartTime != nil {
		return job.Status.StartTime
	}

	return ptrTime(metav1.Now())
}

// migrationBackoffLimit bounds retries of a failed migration pod. `fs cluster
// migrate` is etcd-elected and resumable, so a retry continues where it left
// off; the limit only stops a genuinely broken migration from looping forever.
const migrationBackoffLimit int32 = 6

// MigrationJobName is the Job that migrates the cluster's schema to the version
// its binary implements. It is keyed by the target schema version, so each
// migration gets its own Job and a completed one is never re-run.
func MigrationJobName(cluster string, targetSchema int) string {
	return fmt.Sprintf("%s-migrate-%d", cluster, targetSchema)
}

// NewMigrationJob builds the Job that runs `fs cluster migrate` once every node
// runs the new binary (SPEC §8.2). The migration is etcd-elected among
// whichever processes run it and resumable, so this single-pod Job is safe to
// re-run and safe to run alongside the cluster.
//
// migrate needs only the etcd endpoints (from any node's rendered config) and
// the cluster secret (injected, as for the nodes), so it reuses node 0's config
// Secret and the shared cluster-secret Secret rather than a bespoke config.
func NewMigrationJob(cluster *fsv1alpha1.FSCluster, targetSchema int, node0 string) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MigrationJobName(cluster.Name, targetSchema),
			Namespace: cluster.Namespace,
			Labels:    ObjectLabels(cluster.Name),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(migrationBackoffLimit),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: ObjectLabels(cluster.Name)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyOnFailure,
					SecurityContext:              podSecurityContext(),
					AutomountServiceAccountToken: ptr.To(false),
					ImagePullSecrets:             cluster.Spec.Image.PullSecrets,
					Containers: []corev1.Container{{
						Name:            migrateName,
						Image:           Image(cluster),
						ImagePullPolicy: cluster.Spec.Image.PullPolicy,
						Args:            []string{"cluster", migrateName, flagConfig, ConfigPath},
						Env: []corev1.EnvVar{
							secretEnv(envClusterSecret, ClusterSecretSource(cluster), ClusterSecretKey),
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      configVolumeName,
							MountPath: ConfigDir,
							ReadOnly:  true,
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capAll}},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: configVolumeName,
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName:  ConfigSecretName(node0),
								DefaultMode: ptr.To(secretFileMode),
							},
						},
					}},
				},
			},
		},
	}
}
