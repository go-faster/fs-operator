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

// Package validation holds the cross-field checks on an FSCluster spec that
// CEL cannot express — the ones that need to compare fields to each other, or
// a field to what it used to be.
//
// It exists as its own package because two callers need exactly the same
// answers. The admission webhook runs them at apply time, which is where a
// user should find out; the controller runs them again before it touches
// anything, because a webhook can be bypassed, disabled, or simply not have
// existed when an object was stored. Two implementations of "is this spec
// sane" would eventually disagree, and the disagreement would show up as a
// cluster the API accepted and the operator refuses to build.
package validation

import (
	"fmt"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// MaxNodes is the largest cluster fs supports. The CRD bounds each rack, but
// only a cross-field check sees the total.
const MaxNodes = 16

// MinClusterNodes is the smallest cluster that can hold replicated data: fs
// keeps three copies, each on its own node.
const MinClusterNodes = 3

// SingleNodeWarning is what a one-node cluster is told. A single node is not a
// small cluster: it has no peers, no layout and nothing to repair from.
const SingleNodeWarning = "cluster has a single node (development only): no replication, no " +
	"repair, no failure tolerance — losing the node loses the data. It cannot be grown into a " +
	"cluster in place: declare 3 or more nodes from the start for anything you keep"

// Failure is one rejected spec: the reason a caller reports, and why.
//
// The reason is the condition reason the controller sets and the event it
// records, so it stays the same string whether a user hits it at apply time or
// reads it off the object later (SPEC §13).
type Failure struct {
	Reason  fsv1alpha1.ConditionReason
	Message string
}

func (f Failure) Error() string { return f.Message }

// Cluster checks a spec on its own: everything answerable without looking at
// the cluster that is running or at what the spec used to be.
//
// It returns the first failure rather than all of them. These checks are not
// independent — a scheme that does not parse makes the domain check
// meaningless — and a user fixing one at a time gets a clearer message than a
// list where half the entries are consequences of the first.
func Cluster(spec *fsv1alpha1.FSClusterSpec) *Failure {
	total := int(spec.TotalNodes())

	switch {
	case total > MaxNodes:
		return &Failure{
			Reason: fsv1alpha1.ReasonUnsupportedTopology,
			Message: fmt.Sprintf(
				"the topology declares %d nodes; fs supports at most %d", total, MaxNodes),
		}
	case total > 1 && total < MinClusterNodes:
		return &Failure{
			Reason: fsv1alpha1.ReasonUnsupportedTopology,
			Message: fmt.Sprintf(
				"the topology declares %d nodes; a cluster keeps three copies of its data on "+
					"distinct nodes, so it has one node or at least %d", total, MinClusterNodes),
		}
	}

	// The size is also each node's capacity in the layout; a zero passes the
	// schema, which sees a quantity that is present.
	if spec.Storage.Size.Sign() <= 0 {
		return &Failure{
			Reason:  fsv1alpha1.ReasonSpecInvalid,
			Message: fmt.Sprintf("storage.size must be positive, got %s", spec.Storage.Size.String()),
		}
	}

	if widest := int(spec.MaxWidth()); total > 1 && widest > total {
		return &Failure{
			Reason: fsv1alpha1.ReasonLayoutTopologyMismatch,
			Message: fmt.Sprintf(
				"layout width %d spreads each partition over %d nodes, the topology declares %d",
				widest, widest, total),
		}
	}

	return observability(&spec.Observability)
}

// observability checks the telemetry knobs against each other: an exporter
// with nowhere to send, and a scrape target nothing serves.
//
// Both are combinations the API accepts field by field and no reader would
// call wrong, which is exactly the kind that is found later, in a dashboard
// with no data or a log line about localhost:4318.
func observability(spec *fsv1alpha1.ObservabilitySpec) *Failure {
	for signal, destination := range map[string][2]string{
		"traces":  {spec.Traces.Exporter, spec.Traces.Endpoint},
		"logs":    {spec.Logs.Exporter, spec.Logs.Endpoint},
		"metrics": {spec.Metrics.Exporter, spec.Metrics.Endpoint},
	} {
		exporter, endpoint := destination[0], destination[1]

		if exporter == fsv1alpha1.ExporterOTLP && endpoint == "" && spec.OTLP.Endpoint == "" {
			return &Failure{
				Reason: fsv1alpha1.ReasonSpecInvalid,
				Message: fmt.Sprintf(
					"observability.%s.exporter is %q with nowhere to send it: set observability.%s.endpoint or observability.otlp.endpoint",
					signal, fsv1alpha1.ExporterOTLP, signal),
			}
		}
	}

	// The SDK reads OTEL_EXPORTER_OTLP_PROTOCOL before the per-signal
	// variables and only falls through when it is unset (autometer,
	// autotracer, autologs), which is the reverse of the OpenTelemetry
	// specification. Setting both would therefore silently ignore the
	// per-signal value — a spec that reads exactly like what the user wanted
	// and is not what runs.
	if spec.OTLP.Protocol != "" {
		for signal, protocol := range map[string]string{
			"traces":  spec.Traces.Protocol,
			"logs":    spec.Logs.Protocol,
			"metrics": spec.Metrics.Protocol,
		} {
			if protocol != "" {
				return &Failure{
					Reason: fsv1alpha1.ReasonSpecInvalid,
					Message: fmt.Sprintf(
						"observability.%s.protocol cannot be combined with observability.otlp.protocol: "+
							"fs reads the shared variable first, so the per-signal one would be ignored. "+
							"Leave otlp.protocol unset (the SDK defaults to %s) to give each signal its own",
						signal, fsv1alpha1.SDKDefaultOTLPProtocol),
				}
			}
		}
	}

	if spec.PodMonitor && spec.Metrics.Exporter != "" &&
		spec.Metrics.Exporter != fsv1alpha1.ExporterPrometheus {
		return &Failure{
			Reason: fsv1alpha1.ReasonSpecInvalid,
			Message: fmt.Sprintf(
				"observability.podMonitor scrapes the metrics port, which only the %q exporter serves; metrics.exporter is %q",
				fsv1alpha1.ExporterPrometheus, spec.Metrics.Exporter),
		}
	}

	return nil
}

// ClusterWarnings are the things worth saying about a spec that is still
// allowed. A validating webhook can return these alongside an admission, so a
// user sees them at apply time instead of hunting for an event.
func ClusterWarnings(spec *fsv1alpha1.FSClusterSpec) []string {
	if spec.SingleNode() {
		return []string{SingleNodeWarning}
	}

	var warnings []string

	if domains, widest := spec.FailureDomains(), spec.MaxWidth(); widest > domains {
		warnings = append(warnings, fmt.Sprintf(
			"layout width %d is wider than the %d failure domains the topology provides: some "+
				"partitions keep two slots in one domain, so losing that domain costs both",
			widest, domains))
	}

	return warnings
}

// ClusterUpdate checks a change against what the spec used to be.
//
// The single-node boundary and storage shrink: both compare the old spec with
// the new one, which the webhook and the controller can do in Go more plainly
// than a CEL transition rule on a quantity.
func ClusterUpdate(old, updated *fsv1alpha1.FSClusterSpec) *Failure {
	// A single node has no layout and no peers; growing it means applying a
	// first layout to a node that already holds data as a one-node layout of
	// its own, and fs has no way to merge that into a new cluster.
	if old.SingleNode() != updated.SingleNode() {
		return &Failure{
			Reason: fsv1alpha1.ReasonUnsupportedTopology,
			Message: "a single-node cluster cannot become a clustered one in place, nor the " +
				"reverse. Create a new FSCluster and copy the objects over",
		}
	}

	if failure := Cluster(updated); failure != nil {
		return failure
	}

	// A PVC cannot shrink, so a spec asking for it leaves every node stuck
	// rather than resized.
	if before, after := old.Storage.Size, updated.Storage.Size; after.Cmp(before) < 0 {
		return &Failure{
			Reason: fsv1alpha1.ReasonStorageShrinkForbidden,
			Message: fmt.Sprintf(
				"storage.size would shrink (%s -> %s); it may only grow", before.String(), after.String()),
		}
	}

	return nil
}
