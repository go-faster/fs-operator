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

// Package v1alpha1 holds the admission webhooks for the v1alpha1 API.
package v1alpha1

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/validation"
)

// SetupFSClusterWebhookWithManager registers the FSCluster validating webhook.
func SetupFSClusterWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy(mgr, &fsv1alpha1.FSCluster{}).
		WithValidator(&FSClusterValidator{}).
		Complete()
}

// The webhook rejects a bad spec at apply time, so `kubectl apply` says what is
// wrong instead of the object being stored and the operator refusing it later
// (SPEC §5.1, §16). failurePolicy is Fail: these checks are what stands between
// a typo and a cluster that cannot host its own data, and admitting specs
// unchecked because the webhook is down is the wrong trade.
//
// +kubebuilder:webhook:path=/validate-fs-go-faster-org-v1alpha1-fscluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=fs.go-faster.org,resources=fsclusters,verbs=create;update,versions=v1alpha1,name=vfscluster.kb.io,admissionReviewVersions=v1

// FSClusterValidator validates FSCluster specs at admission.
//
// It holds no client and reads nothing: every check is a pure function of the
// spec, and on update of the spec it replaces. That is what makes it safe on
// the admission path, where calling the API server is a deadlock waiting to
// happen — and it is why the controller can run exactly the same checks
// without a second implementation of them.
type FSClusterValidator struct{}

var _ admission.Validator[*fsv1alpha1.FSCluster] = &FSClusterValidator{}

// groupKind is what a rejection is reported against.
var groupKind = schema.GroupKind{Group: fsv1alpha1.GroupVersion.Group, Kind: "FSCluster"}

// ValidateCreate checks a new cluster.
func (v *FSClusterValidator) ValidateCreate(
	ctx context.Context, cluster *fsv1alpha1.FSCluster,
) (admission.Warnings, error) {
	logf.FromContext(ctx).V(1).Info("Validating FSCluster create", "name", cluster.Name)

	spec := defaulted(cluster)

	return validation.ClusterWarnings(spec), reject(cluster.Name, validation.Cluster(spec))
}

// ValidateUpdate checks a change, including what it is changing from.
func (v *FSClusterValidator) ValidateUpdate(
	ctx context.Context, before, updated *fsv1alpha1.FSCluster,
) (admission.Warnings, error) {
	logf.FromContext(ctx).V(1).Info("Validating FSCluster update", "name", updated.Name)

	spec := defaulted(updated)

	return validation.ClusterWarnings(spec),
		reject(updated.Name, validation.ClusterUpdate(defaulted(before), spec))
}

// ValidateDelete admits every delete. Deletion is choreographed by the
// finalizer, not gated here: a cluster the user wants gone should go, and
// whether its etcd keys go with it is spec.etcd.cleanupOnDelete's business
// (SPEC §8.6).
func (v *FSClusterValidator) ValidateDelete(
	context.Context, *fsv1alpha1.FSCluster,
) (admission.Warnings, error) {
	return nil, nil
}

// defaulted is the spec as it will actually run.
//
// The checks have to see the values the controller renders from, not the ones
// the user typed. spec.scheme is the one that matters here: it is defaulted in
// Go rather than by the CRD, so a spec that omits it would otherwise be
// validated with an empty scheme, fail to parse, and be rejected for a problem
// the user does not have.
func defaulted(cluster *fsv1alpha1.FSCluster) *fsv1alpha1.FSClusterSpec {
	spec := cluster.Spec.DeepCopy()
	spec.WithDefaults()

	return spec
}

// reject turns a validation failure into the API error a user sees, keeping
// the reason in the message so the string they read at apply time is the same
// one the condition and the event would carry.
func reject(name string, failure *validation.Failure) error {
	if failure == nil {
		return nil
	}

	return apierrors.NewInvalid(groupKind, name, field.ErrorList{
		field.Invalid(field.NewPath("spec"), "", failure.Reason+": "+failure.Message),
	})
}
