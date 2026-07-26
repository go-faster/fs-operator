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

// Package metrics is the operator's own Prometheus metrics (SPEC §10).
//
// controller-runtime already exports how the reconciler behaves — queue depth,
// reconcile latency, error counts per controller. What it cannot say is
// anything about the clusters being reconciled: whether one is serving, how
// many of its nodes are up, whether a rolling change is in flight and for how
// long. That is what these add, and it is what an alert rule wants.
//
// Every cluster-keyed metric is deleted when its FSCluster goes (see Forget).
// A gauge that outlives its object is worse than no gauge: it reports a cluster
// that no longer exists as permanently unready, forever.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// Label names shared by the cluster-scoped metrics. Named so the gauges and
// Forget's matcher can never drift apart.
const (
	labelNamespace = "namespace"
	labelCluster   = "cluster"
	labelState     = "state"
	labelPhase     = "phase"
)

// Node states reported by ClusterNodes. They are cumulative counts of the same
// node set, not a partition: every ready node is also a declared one.
const (
	// NodeStateDeclared is how many nodes the topology asks for.
	NodeStateDeclared = "declared"
	// NodeStateReady is how many node pods are Ready.
	NodeStateReady = "ready"
	// NodeStateRegistered is how many nodes the cluster's own control plane
	// (etcd) has in its topology — a node whose pod is up but which has not
	// registered is the interesting gap between this and ready.
	NodeStateRegistered = "registered"
)

var (
	// clusterReady is 1 when the cluster can serve writes, 0 otherwise. The
	// single most useful series here: it is the Ready condition, alertable.
	clusterReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fsoperator_cluster_ready",
		Help: "Whether the fs cluster is Ready (1) or not (0).",
	}, []string{labelNamespace, labelCluster})

	// clusterNodes counts the cluster's nodes by state.
	clusterNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fsoperator_cluster_nodes",
		Help: "Number of fs cluster nodes by state (declared, ready, registered).",
	}, []string{labelNamespace, labelCluster, labelState})

	// updatePhase is 1 for the phase a rolling change is in and 0 for the
	// others, so `max by (phase)` reads as the current phase and a phase stuck
	// at 1 for too long is a rollout that is not progressing.
	updatePhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fsoperator_update_phase",
		Help: "Whether the cluster is in this rolling-change phase (1) or not (0).",
	}, []string{labelNamespace, labelCluster, labelPhase})

	// updateDuration observes how long a completed rolling change took. It is
	// recorded when a change finishes, so a rollout that is stuck never shows
	// up here — that is what updatePhase is for.
	updateDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "fsoperator_update_duration_seconds",
		Help: "How long a completed rolling change took, from its first phase to done.",
		// A node replacement is minutes and a full rollout of 16 nodes with
		// reconvergence between each can be hours, so the range is wide.
		Buckets: prometheus.ExponentialBuckets(30, 2, 10),
	}, []string{labelNamespace, labelCluster})

	// reconcileErrors counts passes that failed, per controller. Distinct from
	// controller-runtime's own error counter, which also counts requeues.
	reconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fsoperator_reconcile_errors_total",
		Help: "Reconcile passes that ended in an error, by controller.",
	}, []string{"controller"})
)

// allPhases is every phase updatePhase reports, so the ones a cluster is not in
// are published as 0 rather than left absent. An absent series and a false one
// look the same to an alert that has never seen the cluster before.
var allPhases = []fsv1alpha1.UpdatePhase{
	fsv1alpha1.UpdatePhasePreflight,
	fsv1alpha1.UpdatePhaseRollingNodes,
	fsv1alpha1.UpdatePhaseDraining,
	fsv1alpha1.UpdatePhaseMigrating,
}

func init() {
	metrics.Registry.MustRegister(
		clusterReady, clusterNodes, updatePhase, updateDuration, reconcileErrors,
	)
}

// RecordCluster publishes one cluster's status. It is called at the end of
// every pass, so the series track the status the operator just wrote.
func RecordCluster(cluster *fsv1alpha1.FSCluster) {
	namespace, name := cluster.Namespace, cluster.Name
	status := &cluster.Status

	clusterReady.WithLabelValues(namespace, name).Set(boolValue(ready(cluster)))

	clusterNodes.WithLabelValues(namespace, name, NodeStateDeclared).Set(float64(status.Nodes))
	clusterNodes.WithLabelValues(namespace, name, NodeStateReady).Set(float64(status.ReadyNodes))
	clusterNodes.WithLabelValues(namespace, name, NodeStateRegistered).Set(float64(status.RegisteredNodes))

	current := fsv1alpha1.UpdatePhase("")
	if status.Update != nil {
		current = status.Update.Phase
	}

	for _, phase := range allPhases {
		updatePhase.WithLabelValues(namespace, name, string(phase)).Set(boolValue(phase == current))
	}
}

// RecordUpdateFinished observes a rolling change that has just completed.
func RecordUpdateFinished(cluster *fsv1alpha1.FSCluster, startedAt time.Time) {
	updateDuration.WithLabelValues(cluster.Namespace, cluster.Name).
		Observe(time.Since(startedAt).Seconds())
}

// RecordReconcileError counts a pass that ended in an error.
func RecordReconcileError(controller string) {
	reconcileErrors.WithLabelValues(controller).Inc()
}

// Forget drops every series for a cluster that no longer exists.
//
// Without it a deleted cluster keeps reporting — ready=0 forever, on a name
// nothing will ever reconcile again — which is indistinguishable from a real
// outage and never resolves.
func Forget(namespace, name string) {
	labels := prometheus.Labels{labelNamespace: namespace, labelCluster: name}

	clusterReady.DeletePartialMatch(labels)
	clusterNodes.DeletePartialMatch(labels)
	updatePhase.DeletePartialMatch(labels)
	updateDuration.DeletePartialMatch(labels)
}

// ready reports whether the cluster's Ready condition is true.
func ready(cluster *fsv1alpha1.FSCluster) bool {
	for _, condition := range cluster.Status.Conditions {
		if condition.Type == fsv1alpha1.ConditionReady {
			return condition.Status == "True"
		}
	}

	return false
}

// boolValue is a gauge's 1/0.
func boolValue(b bool) float64 {
	if b {
		return 1
	}

	return 0
}
