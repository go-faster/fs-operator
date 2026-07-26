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

	if domains := int(spec.FailureDomains()); domains < parsed.MinDomains() {
		return &Failure{
			Reason: fsv1alpha1.ReasonSchemeTopologyMismatch,
			Message: fmt.Sprintf(
				"scheme %q places copies on %d distinct failure domains, the topology provides %d",
				spec.Scheme, parsed.MinDomains(), domains),
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

// ClusterWarnings are the things worth saying about a spec that is still
// allowed. A validating webhook can return these alongside an admission, so a
// user sees them at apply time instead of hunting for an event.
func ClusterWarnings(spec *fsv1alpha1.FSClusterSpec) []string {
	var warnings []string

	if total := int(spec.TotalNodes()); total < DevClusterNodes {
		warnings = append(warnings, fmt.Sprintf(
			"cluster has %d nodes, below the supported minimum of %d: suitable for development only",
			total, DevClusterNodes))
	}

	return warnings
}

// ClusterUpdate checks a change against what the spec used to be.
//
// Only disk shrink for now, and it is here rather than in CEL for the reason
// SPEC §8.5 gives: comparing every disk's old and new size costs more than the
// API server's per-schema validation budget allows for a list this long.
func ClusterUpdate(old, updated *fsv1alpha1.FSClusterSpec) *Failure {
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
