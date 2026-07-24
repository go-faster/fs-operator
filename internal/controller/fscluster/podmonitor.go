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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-faster/fs-operator/internal/controller/pipeline"
)

// podMonitorGVK is the Prometheus-operator resource that scrapes pods. The
// operator addresses it unstructured so it does not depend on the
// prometheus-operator Go module, and creates one only when the CRD is present.
var podMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "PodMonitor",
}

// eventPodMonitorUnavailable warns that a PodMonitor was requested without the
// Prometheus-operator CRDs installed.
const eventPodMonitorUnavailable = "PodMonitorUnavailable"

// reconcilePodMonitor creates or removes the cluster's PodMonitor per
// spec.observability.podMonitor, when the monitoring.coreos.com API is present.
func (r *Reconciler) reconcilePodMonitor(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	if !r.podMonitorAvailable() {
		if p.cluster.Spec.Observability.PodMonitor {
			r.Recorder.Event(p.object, "Warning", eventPodMonitorUnavailable,
				"observability.podMonitor is set but the monitoring.coreos.com CRDs are not installed")
		}

		return pipeline.Continue()
	}

	monitor := NewPodMonitor(p.cluster)

	if !p.cluster.Spec.Observability.PodMonitor {
		if err := r.deleteIfExists(ctx, monitor); err != nil {
			return pipeline.Outcome{}, err
		}

		return pipeline.Continue()
	}

	if err := r.apply(ctx, p.cluster, monitor); err != nil {
		return pipeline.Outcome{}, err
	}

	return pipeline.Continue()
}

// podMonitorAvailable reports whether the PodMonitor kind is served by the API,
// so the operator does not try to create a resource the cluster cannot store.
func (r *Reconciler) podMonitorAvailable() bool {
	_, err := r.RESTMapper().RESTMapping(podMonitorGVK.GroupKind(), podMonitorGVK.Version)
	if err != nil {
		logf.Log.V(1).Info("PodMonitor API not available", "error", err)

		return false
	}

	return true
}

// NewPodMonitor builds the PodMonitor that scrapes the cluster's pods on their
// metrics port. It is unstructured so the operator carries no dependency on the
// Prometheus-operator API types.
func NewPodMonitor(cluster clusterMeta) *unstructured.Unstructured {
	monitor := &unstructured.Unstructured{}
	monitor.SetGroupVersionKind(podMonitorGVK)
	monitor.SetName(cluster.GetName())
	monitor.SetNamespace(cluster.GetNamespace())
	monitor.SetLabels(ObjectLabels(cluster.GetName()))

	monitor.Object["spec"] = map[string]any{
		"selector": map[string]any{
			"matchLabels": toAnyMap(SelectorLabels(cluster.GetName())),
		},
		"podMetricsEndpoints": []any{
			map[string]any{
				"port": PortNameMetrics,
				"path": "/metrics",
			},
		},
	}

	return monitor
}

// clusterMeta is the slice of an FSCluster the PodMonitor builder needs, so the
// builder can be exercised without a full object.
type clusterMeta interface {
	GetName() string
	GetNamespace() string
}

// toAnyMap converts a string map to the map[string]any unstructured wants.
func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}
