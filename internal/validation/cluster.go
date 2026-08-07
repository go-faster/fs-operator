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

	"k8s.io/apimachinery/pkg/api/resource"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/scheme"
)

// MaxNodes is the largest cluster fs supports. The CRD bounds each rack, but
// only a cross-field check sees the total.
const MaxNodes = 16

// DevClusterNodes is the smallest cluster that can host a replicated scheme.
// Below it a cluster is a development toy — allowed, but it says so.
const DevClusterNodes = 3

// SingleNodeWarning is what a one-node cluster is told. It is not the "below
// the minimum" caveat a two-node cluster gets: a single node is not a small
// cluster, it is fs's non-clustered backend, with a different set of things it
// cannot do.
const SingleNodeWarning = "cluster has a single node (development only): it runs fs's " +
	"filesystem backend — one disk, no replication, no repair, no failure tolerance — so losing " +
	"the node loses the data. It cannot be grown into a cluster in place: declare 3 or more " +
	"nodes from the start for anything you keep"

// ManagedEtcdWarning is what every cluster running the operator-managed etcd
// is told, at apply time and again on the object. It is a permanent property
// of that mode, not a caveat that gets softened later (SPEC §2).
const ManagedEtcdWarning = "etcd is operator-managed (development only): no backups, " +
	"no defrag automation, no member replacement. Losing its volume loses the cluster's " +
	"control plane and the credentials sealed in it. Use etcd.external in production"

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
	parsed, err := scheme.Parse(spec.Scheme)
	if err != nil {
		return &Failure{Reason: fsv1alpha1.ReasonSpecInvalid, Message: err.Error()}
	}

	if spec.SingleNode() {
		return singleNode(spec)
	}

	if domains := int(spec.FailureDomains()); domains < parsed.Copies() {
		return &Failure{
			Reason: fsv1alpha1.ReasonSchemeTopologyMismatch,
			Message: fmt.Sprintf(
				"scheme %q places copies on %d distinct failure domains, the topology provides %d",
				spec.Scheme, parsed.Copies(), domains),
		}
	}

	// CEL enforces this too, but a spec stored before the rule existed, or
	// admitted while the webhook was off, would otherwise be rendered with no
	// etcd endpoints at all — a config the nodes accept and cannot use.
	if (spec.Etcd.External == nil) == (spec.Etcd.Managed == nil) {
		return &Failure{
			Reason:  fsv1alpha1.ReasonSpecInvalid,
			Message: "exactly one of etcd.external or etcd.managed must be set",
		}
	}

	if external := spec.Etcd.External; external != nil && len(external.Endpoints) == 0 {
		return &Failure{
			Reason:  fsv1alpha1.ReasonSpecInvalid,
			Message: "etcd.external.endpoints must not be empty",
		}
	}

	if failure := observability(&spec.Observability); failure != nil {
		return failure
	}

	for _, disk := range spec.Storage.Disks {
		if disk.Name == fsv1alpha1.StateVolumeName {
			return &Failure{
				Reason: fsv1alpha1.ReasonSpecInvalid,
				Message: fmt.Sprintf(
					"disk name %q is reserved for the node's state volume (the storage root); rename the disk",
					fsv1alpha1.StateVolumeName),
			}
		}
	}

	if total := int(spec.TotalNodes()); total > MaxNodes {
		return &Failure{
			Reason: fsv1alpha1.ReasonUnsupportedTopology,
			Message: fmt.Sprintf(
				"the topology declares %d nodes; fs supports at most %d", total, MaxNodes),
		}
	}

	return nil
}

// singleNode checks the shape a one-node cluster has to have.
//
// It is not a small cluster with the rules relaxed: fs cannot place any scheme
// on one node — every scheme needs three distinct disks or k+m of them, and a
// bucket record needs three — so the node runs the non-clustered filesystem
// backend instead. That backend stores everything under one root and has no
// control plane, which is exactly the two things checked here. The scheme is
// left alone: it is the default on a field the user may never have touched, and
// nothing reads it in this mode.
func singleNode(spec *fsv1alpha1.FSClusterSpec) *Failure {
	if disks := len(spec.Storage.Disks); disks != 1 {
		return &Failure{
			Reason: fsv1alpha1.ReasonUnsupportedTopology,
			Message: fmt.Sprintf(
				"a single-node cluster stores objects under one root and declares exactly 1 disk, got %d",
				disks),
		}
	}

	if spec.Etcd.External != nil || spec.Etcd.Managed != nil {
		return &Failure{
			Reason: fsv1alpha1.ReasonSpecInvalid,
			Message: "a single-node cluster has no control plane to register in; leave etcd unset " +
				"(a clustered topology of 3 or more nodes is what needs it)",
		}
	}

	return nil
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
	var warnings []string

	switch total := int(spec.TotalNodes()); {
	case total == 1:
		warnings = append(warnings, SingleNodeWarning)
	case total < DevClusterNodes:
		warnings = append(warnings, fmt.Sprintf(
			"cluster has %d nodes, below the supported minimum of %d: suitable for development only",
			total, DevClusterNodes))
	}

	if spec.Etcd.Managed != nil {
		warnings = append(warnings, ManagedEtcdWarning)
	}

	return warnings
}

// ClusterUpdate checks a change against what the spec used to be.
//
// Disk shrink and the single-node boundary, both here rather than in CEL for
// the reason SPEC §8.5 gives: comparing every disk's old and new size costs
// more than the API server's per-schema validation budget allows for a list
// this long.
func ClusterUpdate(old, updated *fsv1alpha1.FSClusterSpec) *Failure {
	// Checked before the spec is judged on its own, so that growing a
	// single-node cluster is reported as the backend switch it is rather than
	// as the missing etcd a clustered topology would also need.
	//
	// A single node runs a different storage backend, under a different root,
	// with none of its objects registered anywhere. Crossing that line in
	// place is not a scale-up or a scale-down — it is a cluster that comes
	// back empty, with the old data still on a volume nothing reads.
	if old.SingleNode() != updated.SingleNode() {
		return &Failure{
			Reason: fsv1alpha1.ReasonUnsupportedTopology,
			Message: "a single-node cluster and a clustered one store data differently; " +
				"switching between them in place would abandon the data. Create a new FSCluster " +
				"and copy the objects over",
		}
	}

	// Same reason, one level down: the single node's root is its disk's mount
	// path, so renaming the disk is a new empty volume and an orphaned old one.
	if updated.SingleNode() && len(old.Storage.Disks) == 1 && len(updated.Storage.Disks) == 1 &&
		old.Storage.Disks[0].Name != updated.Storage.Disks[0].Name {
		return &Failure{
			Reason: fsv1alpha1.ReasonUnsupportedTopology,
			Message: fmt.Sprintf(
				"a single-node cluster's disk %q holds everything it has and cannot be renamed to %q; "+
					"the node has no cluster to drain it into",
				old.Storage.Disks[0].Name, updated.Storage.Disks[0].Name),
		}
	}

	if failure := Cluster(updated); failure != nil {
		return failure
	}

	if shrunk := shrunkDisks(old, updated); len(shrunk) > 0 {
		return &Failure{
			Reason: fsv1alpha1.ReasonDiskShrinkForbidden,
			Message: fmt.Sprintf(
				"disk(s) %v would shrink; disks may only grow", shrunk),
		}
	}

	// The state volume is a PVC like any other: Kubernetes cannot shrink one,
	// so a spec asking for it leaves the node stuck rather than resized.
	if before, after := old.Storage.State.Size, updated.Storage.State.Size; before != nil &&
		after != nil && after.Cmp(*before) < 0 {
		return &Failure{
			Reason: fsv1alpha1.ReasonDiskShrinkForbidden,
			Message: fmt.Sprintf(
				"storage.state.size would shrink (%s -> %s); it may only grow",
				before.String(), after.String()),
		}
	}

	return nil
}

// shrunkDisks names the disks whose requested size went down.
//
// A disk the update does not mention is not shrinking — it is being removed,
// which is a different operation with its own rules, and not this check's to
// refuse.
func shrunkDisks(old, updated *fsv1alpha1.FSClusterSpec) []string {
	sizes := make(map[string]resource.Quantity, len(old.Storage.Disks))

	for i := range old.Storage.Disks {
		disk := &old.Storage.Disks[i]
		sizes[disk.Name] = disk.Size
	}

	var shrunk []string

	for i := range updated.Storage.Disks {
		disk := &updated.Storage.Disks[i]

		before, ok := sizes[disk.Name]
		if !ok {
			continue
		}

		if disk.Size.Cmp(before) < 0 {
			shrunk = append(shrunk, fmt.Sprintf("%s (%s -> %s)",
				disk.Name, before.String(), disk.Size.String()))
		}
	}

	return shrunk
}
