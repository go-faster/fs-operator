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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// TestNewPodMonitor pins the PodMonitor shape: it selects the cluster's pods
// and scrapes their named metrics port.
func TestNewPodMonitor(t *testing.T) {
	cluster := testCluster()
	monitor := NewPodMonitor(cluster)

	if monitor.GetKind() != "PodMonitor" || monitor.GetAPIVersion() != "monitoring.coreos.com/v1" {
		t.Errorf("gvk = %s/%s, want monitoring.coreos.com/v1 PodMonitor", monitor.GetAPIVersion(), monitor.GetKind())
	}

	if monitor.GetName() != cluster.Name || monitor.GetNamespace() != cluster.Namespace {
		t.Errorf("name/ns = %s/%s, want %s/%s", monitor.GetNamespace(), monitor.GetName(), cluster.Namespace, cluster.Name)
	}

	selector, _, _ := unstructured.NestedStringMap(monitor.Object, "spec", "selector", "matchLabels")
	if selector[LabelCluster] != cluster.Name {
		t.Errorf("selector = %v, want it to match the cluster's pods", selector)
	}

	endpoints, _, _ := unstructured.NestedSlice(monitor.Object, "spec", "podMetricsEndpoints")
	if len(endpoints) != 1 {
		t.Fatalf("%d metrics endpoints, want 1", len(endpoints))
	}

	endpoint, _ := endpoints[0].(map[string]any)
	if endpoint["port"] != PortNameMetrics {
		t.Errorf("scrape port = %v, want the named metrics port %q", endpoint["port"], PortNameMetrics)
	}
}

// TestReconcilePodMonitorDegradesGracefully covers the common case: the
// Prometheus-operator CRDs are not installed, so requesting a PodMonitor is a
// warning, not a failure, and nothing is created.
func TestReconcilePodMonitorDegradesGracefully(t *testing.T) {
	r, recorder := reconciler(t)
	key := createCluster(t, r, "podmonitor", func(c *fsv1alpha1.FSCluster) {
		c.Spec.Observability.PodMonitor = true
	})

	// The pass must not fail just because monitoring.coreos.com is absent.
	reconcile(t, r, key)

	// A warning event explains why no PodMonitor appeared.
	warned := false
	for len(recorder.Events) > 0 {
		if ev := <-recorder.Events; contains(ev, eventPodMonitorUnavailable) {
			warned = true
		}
	}

	if !warned {
		t.Error("no warning event for a PodMonitor requested without its CRDs")
	}
}

// contains reports whether s contains sub.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
