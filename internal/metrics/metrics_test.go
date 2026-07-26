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

package metrics_test

import (
	"slices"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/metrics"
)

// testNamespace is the namespace every fixture cluster lives in.
const testNamespace = "prod"

// sample reads one metric's samples as label-set -> value, so a test can assert
// on the series a scrape would actually see.
func sample(t *testing.T, name string) map[string]float64 {
	t.Helper()

	families, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	out := map[string]float64{}

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, metric := range family.GetMetric() {
			labels := make([]string, 0, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}

			out[strings.Join(labels, ",")] = value(metric)
		}
	}

	return out
}

// key builds the lookup key sample() uses: labels sorted by name, the order a
// Prometheus gather returns them in.
func key(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}

	slices.Sort(names)

	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+labels[name])
	}

	return strings.Join(pairs, ",")
}

// clusterKey is the label set of a cluster-scoped series, plus any extras.
func clusterKey(name string, extra ...string) string {
	labels := map[string]string{"namespace": testNamespace, "cluster": name}
	for i := 0; i+1 < len(extra); i += 2 {
		labels[extra[i]] = extra[i+1]
	}

	return key(labels)
}

func value(m *dto.Metric) float64 {
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}

	return m.GetCounter().GetValue()
}

func cluster(name string) *fsv1alpha1.FSCluster {
	return &fsv1alpha1.FSCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
	}
}

// TestRecordClusterPublishesStatus covers the series an alert rule reads.
func TestRecordClusterPublishesStatus(t *testing.T) {
	c := cluster("alpha")
	c.Status.Nodes = 4
	c.Status.ReadyNodes = 3
	c.Status.RegisteredNodes = 3
	c.Status.Conditions = []metav1.Condition{
		{Type: fsv1alpha1.ConditionReady, Status: metav1.ConditionTrue},
	}
	c.Status.Update = &fsv1alpha1.UpdateStatus{Phase: fsv1alpha1.UpdatePhaseDraining}

	metrics.RecordCluster(c)
	t.Cleanup(func() { metrics.Forget(testNamespace, "alpha") })

	ready := sample(t, "fsoperator_cluster_ready")
	if got := ready[clusterKey("alpha")]; got != 1 {
		t.Errorf("cluster_ready = %v, want 1", got)
	}

	nodes := sample(t, "fsoperator_cluster_nodes")
	for state, want := range map[string]float64{
		metrics.NodeStateDeclared:   4,
		metrics.NodeStateReady:      3,
		metrics.NodeStateRegistered: 3,
	} {
		if got := nodes[clusterKey("alpha", "state", state)]; got != want {
			t.Errorf("cluster_nodes{state=%q} = %v, want %v", state, got, want)
		}
	}

	// The phase the cluster is in reads 1; the others are published as 0 rather
	// than left absent, so an alert does not have to tell "false" from "this
	// cluster has never been seen".
	phases := sample(t, "fsoperator_update_phase")
	if got := phases[clusterKey("alpha", "phase", "Draining")]; got != 1 {
		t.Errorf("update_phase{Draining} = %v, want 1", got)
	}

	if got, ok := phases[clusterKey("alpha", "phase", "RollingNodes")]; !ok || got != 0 {
		t.Errorf("update_phase{RollingNodes} = %v (present %v), want 0 present", got, ok)
	}
}

// TestRecordClusterUnready covers a cluster that is not serving, which is the
// state the ready gauge exists for.
func TestRecordClusterUnready(t *testing.T) {
	c := cluster("beta")
	c.Status.Conditions = []metav1.Condition{
		{Type: fsv1alpha1.ConditionReady, Status: metav1.ConditionFalse},
	}

	metrics.RecordCluster(c)
	t.Cleanup(func() { metrics.Forget(testNamespace, "beta") })

	if got := sample(t, "fsoperator_cluster_ready")[clusterKey("beta")]; got != 0 {
		t.Errorf("cluster_ready = %v, want 0", got)
	}

	// A cluster with no rolling change in flight is in no phase at all.
	phases := sample(t, "fsoperator_update_phase")
	for _, phase := range []string{"Preflight", "RollingNodes", "Draining", "Migrating"} {
		if got := phases[clusterKey("beta", "phase", phase)]; got != 0 {
			t.Errorf("update_phase{%s} = %v, want 0 with no update in flight", phase, got)
		}
	}
}

// TestForgetDropsTheSeries is the one that matters for a long-running operator:
// a deleted cluster must stop reporting. A ready=0 gauge on a name nothing will
// reconcile again is indistinguishable from a real outage, and never resolves.
func TestForgetDropsTheSeries(t *testing.T) {
	c := cluster("gamma")
	c.Status.Nodes = 3

	metrics.RecordCluster(c)

	if _, ok := sample(t, "fsoperator_cluster_ready")[clusterKey("gamma")]; !ok {
		t.Fatal("the cluster was not published in the first place")
	}

	metrics.Forget(testNamespace, "gamma")

	for _, name := range []string{
		"fsoperator_cluster_ready",
		"fsoperator_cluster_nodes",
		"fsoperator_update_phase",
	} {
		for labels := range sample(t, name) {
			if strings.Contains(labels, "cluster=gamma") {
				t.Errorf("%s still reports the deleted cluster: %s", name, labels)
			}
		}
	}
}

// TestForgetLeavesOtherClusters covers the label matching: forgetting one
// cluster must not take its namespace's others with it.
func TestForgetLeavesOtherClusters(t *testing.T) {
	metrics.RecordCluster(cluster("keep"))
	metrics.RecordCluster(cluster("drop"))

	t.Cleanup(func() { metrics.Forget(testNamespace, "keep") })

	metrics.Forget(testNamespace, "drop")

	ready := sample(t, "fsoperator_cluster_ready")
	if _, ok := ready[clusterKey("keep")]; !ok {
		t.Error("forgetting one cluster dropped another in the same namespace")
	}

	if _, ok := ready[clusterKey("drop")]; ok {
		t.Error("the forgotten cluster is still reported")
	}
}

func TestRecordReconcileError(t *testing.T) {
	before := sample(t, "fsoperator_reconcile_errors_total")["controller=fsbucket"]

	metrics.RecordReconcileError("fsbucket")

	if got := sample(t, "fsoperator_reconcile_errors_total")["controller=fsbucket"]; got != before+1 {
		t.Errorf("reconcile_errors_total = %v, want %v", got, before+1)
	}
}
