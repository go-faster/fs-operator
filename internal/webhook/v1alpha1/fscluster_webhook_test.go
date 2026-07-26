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

package v1alpha1

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/scheme"
)

// cluster is a valid FSCluster as a user would apply it, before defaulting.
func cluster(mutate func(*fsv1alpha1.FSCluster)) *fsv1alpha1.FSCluster {
	nodes := int32(3)
	c := &fsv1alpha1.FSCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "default"},
		Spec: fsv1alpha1.FSClusterSpec{
			Scheme:   scheme.RF3,
			Topology: fsv1alpha1.TopologySpec{Nodes: &nodes},
			Etcd: fsv1alpha1.EtcdSpec{
				External: &fsv1alpha1.ExternalEtcdSpec{
					Endpoints: []string{"http://etcd.default.svc:2379"},
				},
			},
		},
	}

	if mutate != nil {
		mutate(c)
	}

	return c
}

func TestValidateCreateAdmitsAValidSpec(t *testing.T) {
	warnings, err := (&FSClusterValidator{}).ValidateCreate(t.Context(), cluster(nil))
	if err != nil {
		t.Fatalf("a valid cluster was rejected: %v", err)
	}

	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// TestValidateCreateRejectsAsInvalid checks the shape of the rejection, not
// just that there is one: a user sees this at `kubectl apply`, so it has to be
// a StatusError the API server renders as Invalid, and it has to name the
// reason the condition would have carried.
func TestValidateCreateRejectsAsInvalid(t *testing.T) {
	tooSmall := cluster(func(c *fsv1alpha1.FSCluster) {
		nodes := int32(2)
		c.Spec.Topology.Nodes = &nodes
	})

	_, err := (&FSClusterValidator{}).ValidateCreate(t.Context(), tooSmall)
	if err == nil {
		t.Fatal("a topology too small for its scheme was admitted")
	}

	if !apierrors.IsInvalid(err) {
		t.Errorf("error is %T, want an Invalid StatusError", err)
	}

	if !strings.Contains(err.Error(), fsv1alpha1.ReasonSchemeTopologyMismatch) {
		t.Errorf("error %q does not name the reason the condition would carry", err)
	}
}

// TestValidateCreateWarnsWithoutRejecting covers the dev-sized cluster: allowed,
// but the user hears about it at apply time instead of hunting for an event.
func TestValidateCreateWarnsWithoutRejecting(t *testing.T) {
	// Two nodes: enough for ec:1,1's two domains, but below the three a
	// replicated scheme needs, so it is supported-for-development only.
	small := cluster(func(c *fsv1alpha1.FSCluster) {
		nodes := int32(2)
		c.Spec.Topology.Nodes = &nodes
		c.Spec.Scheme = "ec:1,1"
	})

	warnings, err := (&FSClusterValidator{}).ValidateCreate(t.Context(), small)
	if err != nil {
		t.Fatalf("a dev-sized cluster should be admitted, got: %v", err)
	}

	if len(warnings) == 0 {
		t.Error("a dev-sized cluster should warn")
	}
}

// TestValidateCreateDefaultsBeforeChecking is the trap this would otherwise
// fall into: spec.scheme is defaulted in Go rather than by the CRD, so a spec
// that omits it reaches the webhook empty. Validating that literally would
// fail to parse "" and reject a spec whose only sin is relying on a default.
func TestValidateCreateDefaultsBeforeChecking(t *testing.T) {
	noScheme := cluster(func(c *fsv1alpha1.FSCluster) { c.Spec.Scheme = "" })

	warnings, err := (&FSClusterValidator{}).ValidateCreate(t.Context(), noScheme)
	if err != nil {
		t.Fatalf("a spec relying on the scheme default was rejected: %v", err)
	}

	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}

	// And the webhook must not have written the default back: a validating
	// webhook returns no patch, so the stored object keeps what the user wrote.
	if noScheme.Spec.Scheme != "" {
		t.Errorf("the validator mutated the object: scheme = %q", noScheme.Spec.Scheme)
	}
}

func TestValidateUpdateRefusesDiskShrink(t *testing.T) {
	before := cluster(func(c *fsv1alpha1.FSCluster) {
		c.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
			{Name: "d0", Size: resource.MustParse("100Gi")},
		}
	})

	shrunk := cluster(func(c *fsv1alpha1.FSCluster) {
		c.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
			{Name: "d0", Size: resource.MustParse("50Gi")},
		}
	})

	_, err := (&FSClusterValidator{}).ValidateUpdate(t.Context(), before, shrunk)
	if err == nil {
		t.Fatal("a shrinking disk was admitted")
	}

	if !strings.Contains(err.Error(), fsv1alpha1.ReasonDiskShrinkForbidden) {
		t.Errorf("error %q does not name the disk-shrink reason", err)
	}
}

func TestValidateUpdateAdmitsAGrowingDisk(t *testing.T) {
	before := cluster(func(c *fsv1alpha1.FSCluster) {
		c.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
			{Name: "d0", Size: resource.MustParse("100Gi")},
		}
	})

	grown := cluster(func(c *fsv1alpha1.FSCluster) {
		c.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
			{Name: "d0", Size: resource.MustParse("200Gi")},
		}
	})

	if _, err := (&FSClusterValidator{}).ValidateUpdate(t.Context(), before, grown); err != nil {
		t.Fatalf("growing a disk was rejected: %v", err)
	}
}

// TestValidateUpdateAdmitsScaleDown: shrinking a topology is a decommission,
// which the operator performs (SPEC §8.4). The webhook must not refuse it —
// only a topology below the scheme's minimum, which the spec check catches.
func TestValidateUpdateAdmitsScaleDown(t *testing.T) {
	before := cluster(func(c *fsv1alpha1.FSCluster) {
		nodes := int32(5)
		c.Spec.Topology.Nodes = &nodes
	})

	smaller := cluster(func(c *fsv1alpha1.FSCluster) {
		nodes := int32(3)
		c.Spec.Topology.Nodes = &nodes
	})

	if _, err := (&FSClusterValidator{}).ValidateUpdate(t.Context(), before, smaller); err != nil {
		t.Fatalf("a decommission was refused at admission: %v", err)
	}
}

func TestValidateDeleteAdmits(t *testing.T) {
	if _, err := (&FSClusterValidator{}).ValidateDelete(t.Context(), cluster(nil)); err != nil {
		t.Errorf("delete was refused: %v", err)
	}
}
