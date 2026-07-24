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
	"slices"
	"time"

	"github.com/go-faster/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller"
)

// resyncInterval refreshes the health-derived part of the status even when no
// watch fires: a pod that stops being ready is an event, but a cluster slowly
// falling behind is not.
const resyncInterval = 5 * time.Minute

// maxNodes is the largest cluster fs supports. The CRD bounds each rack, but
// only the controller sees the total.
const maxNodes = 16

// devClusterNodes is the smallest cluster that can host a replicated scheme;
// below it a cluster is a development toy and says so.
const devClusterNodes = 3

// Reconciler reconciles an FSCluster into the resources of a running fs
// cluster.
type Reconciler struct {
	client.Client

	// Scheme is the runtime scheme, used to stamp ownership on managed
	// objects.
	Scheme *runtime.Scheme

	// Recorder reports the decisions a user cannot read off the status:
	// refusals, warnings and the steps of a rollout.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings one FSCluster's resources in line with its spec.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	object := &fsv1alpha1.FSCluster{}
	if err := r.Get(ctx, req.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !object.DeletionTimestamp.IsZero() {
		// Everything the operator creates carries an owner reference, so
		// garbage collection takes it down, and each node's claim retention
		// policy decides the fate of its data. Only etcd state outlives this,
		// which is what the deletion work of SPEC §8.6 is about.
		return ctrl.Result{}, nil
	}

	result, err := r.pipeline().Run(ctx, newPass(object))
	if result.RequeueAfter == 0 && err == nil {
		result.RequeueAfter = resyncInterval
	}

	return result, err
}

// pipeline is a reconcile pass: resources first, in the order they depend on
// each other, and the status last so that it describes what actually
// happened — including a pass that was refused or failed (SPEC §8).
func (r *Reconciler) pipeline() controller.Pipeline[*pass] {
	return controller.Pipeline[*pass]{
		{Name: "validate", Run: r.validate},
		{Name: "secrets", Run: r.reconcileSecrets},
		{Name: "services", Run: r.reconcileServices},
		{Name: "configs", Run: r.reconcileNodeConfigs},
		{Name: "nodes", Run: r.reconcileNodes},
		{Name: "budget", Run: r.reconcileBudget},
		{Name: "status", AlwaysRun: true, Run: r.reconcileStatus},
	}
}

// pass is the state one reconcile pass carries between its steps.
type pass struct {
	// object is the FSCluster as stored; status is written back to it.
	object *fsv1alpha1.FSCluster

	// cluster is a defaulted copy: everything is rendered from it, so no
	// builder has to know which defaults the admission path applied.
	cluster *fsv1alpha1.FSCluster

	// nodes is the topology expanded into the cluster's node set.
	nodes []Node

	// configs holds each node's rendered configuration once the config step
	// has run, and restarts the fingerprint of its restart-requiring part.
	configs  map[string][]byte
	restarts map[string]string

	// templateRevision fingerprints the desired pod templates.
	templateRevision string

	// conditions accumulate through the pass and are written by the status
	// step, so that a refusal and its explanation reach the object together.
	conditions []metav1.Condition

	// failure is the error that stopped the pass, if any.
	failure error
}

// newPass prepares the state of a reconcile pass.
func newPass(object *fsv1alpha1.FSCluster) *pass {
	cluster := object.DeepCopy()
	cluster.Spec.WithDefaults()

	return &pass{
		object:  object,
		cluster: cluster,
		nodes:   Nodes(cluster),
	}
}

// RecordFailure lets the status step report the error that stopped the pass
// instead of guessing at one.
func (p *pass) RecordFailure(err error) {
	if p.failure == nil {
		p.failure = err
	}
}

// setCondition queues a condition for the status step.
func (p *pass) setCondition(conditionType string, status metav1.ConditionStatus, reason, message string) {
	p.conditions = append(p.conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: p.object.Generation,
	})
}

// validate performs the cross-field checks CEL cannot express, and refuses the
// spec rather than half-applying it (SPEC §5.1). In v1alpha1 this lives in the
// controller; §16 moves it to an admission webhook, where a user would find
// out at apply time instead.
func (r *Reconciler) validate(ctx context.Context, p *pass) (controller.Outcome, error) {
	spec := &p.cluster.Spec

	scheme, err := ParseScheme(spec.Scheme)
	if err != nil {
		return r.refuse(p, fsv1alpha1.ReasonSpecInvalid, err.Error())
	}

	if domains := int(spec.FailureDomains()); domains < scheme.MinDomains() {
		return r.refuse(p, fsv1alpha1.ReasonSchemeTopologyMismatch, fmt.Sprintf(
			"scheme %q places copies on %d distinct failure domains, the topology provides %d",
			spec.Scheme, scheme.MinDomains(), domains))
	}

	if total := int(spec.TotalNodes()); total > maxNodes {
		return r.refuse(p, fsv1alpha1.ReasonUnsupportedTopology, fmt.Sprintf(
			"the topology declares %d nodes; fs supports at most %d", total, maxNodes))
	}

	if total := int(spec.TotalNodes()); total < devClusterNodes {
		r.Recorder.Eventf(p.object, corev1.EventTypeWarning, fsv1alpha1.ReasonUnsupportedTopology,
			"Cluster has %d nodes, below the supported minimum of %d: suitable for development only",
			total, devClusterNodes)
	}

	removed, err := r.removedNodes(ctx, p)
	if err != nil {
		return controller.Outcome{}, err
	}

	if len(removed) > 0 {
		return r.refuse(p, fsv1alpha1.ReasonScaleDownRequiresDrain, fmt.Sprintf(
			"the spec removes node(s) %v; draining is not observable yet, so the cluster is left as it is",
			removed))
	}

	p.setCondition(fsv1alpha1.ConditionSpecValid, metav1.ConditionTrue, fsv1alpha1.ReasonSpecValid,
		"Spec passes cross-field validation")

	return controller.Continue()
}

// refuse records why a spec is not applied and stops the pass. Nothing is
// mutated: a spec the operator will not apply leaves the running cluster
// exactly as it was.
func (r *Reconciler) refuse(p *pass, reason, message string) (controller.Outcome, error) {
	p.setCondition(fsv1alpha1.ConditionSpecValid, metav1.ConditionFalse, reason, message)
	r.Recorder.Event(p.object, corev1.EventTypeWarning, reason, message)

	return controller.Block(reason)
}

// removedNodes reports which of the cluster's existing nodes the spec no
// longer declares.
//
// Removing a node means draining it first — moving its data elsewhere — and
// the operator cannot yet see when a node holds nothing (fs §11.2). Deleting
// an undrained node would leave the cluster to repair from surviving copies,
// so a shrinking topology is refused rather than obeyed (SPEC §8.4).
func (r *Reconciler) removedNodes(ctx context.Context, p *pass) ([]string, error) {
	existing, err := r.nodeSets(ctx, p.cluster)
	if err != nil {
		return nil, err
	}

	desired := make(map[string]bool, len(p.nodes))
	for _, node := range p.nodes {
		desired[node.Name] = true
	}

	var removed []string

	for _, set := range existing {
		if !desired[set.Name] {
			removed = append(removed, set.Name)
		}
	}

	slices.Sort(removed)

	return removed, nil
}

// nodeSets lists the StatefulSets the operator manages for a cluster.
func (r *Reconciler) nodeSets(ctx context.Context, cluster *fsv1alpha1.FSCluster) ([]appsv1.StatefulSet, error) {
	var sets appsv1.StatefulSetList

	if err := r.List(ctx, &sets,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(SelectorLabels(cluster.Name)),
	); err != nil {
		return nil, errors.Wrap(err, "list node statefulsets")
	}

	return sets.Items, nil
}

// reconcileSecrets makes sure the cluster's secret material exists: what the
// operator generates is created once and never rewritten, and what the user
// referenced has to be there before anything is pointed at it.
func (r *Reconciler) reconcileSecrets(ctx context.Context, p *pass) (controller.Outcome, error) {
	generated := []struct {
		skip  bool
		build func(*fsv1alpha1.FSCluster) (*corev1.Secret, error)
	}{
		{skip: p.cluster.Spec.ClusterSecretRef != nil, build: NewClusterSecret},
		{skip: p.cluster.Spec.Auth.RootCredentialsSecretRef != nil, build: NewRootCredentialsSecret},
		{build: NewAdminTokenSecret},
	}

	for _, secret := range generated {
		if secret.skip {
			continue
		}

		object, err := secret.build(p.cluster)
		if err != nil {
			return controller.Outcome{}, err
		}

		if err := r.createOnce(ctx, p.cluster, object); err != nil {
			return controller.Outcome{}, err
		}
	}

	for _, ref := range []struct {
		name string
		keys []string
	}{
		{name: ClusterSecretSource(p.cluster), keys: []string{ClusterSecretKey}},
		{name: RootCredentialsSource(p.cluster), keys: []string{AccessKeyKey, SecretKeyKey}},
	} {
		if outcome, err := r.requireSecret(ctx, p, ref.name, ref.keys); err != nil || outcome.Blocked {
			return outcome, err
		}
	}

	return controller.Continue()
}

// requireSecret blocks the pass when a referenced Secret is missing or does
// not carry the keys fs will look for. Pointing a pod at a Secret that is not
// there produces a container that never starts and no explanation.
func (r *Reconciler) requireSecret(ctx context.Context, p *pass, name string, keys []string) (controller.Outcome, error) {
	var secret corev1.Secret

	err := r.Get(ctx, types.NamespacedName{Namespace: p.cluster.Namespace, Name: name}, &secret)

	switch {
	case apierrors.IsNotFound(err):
		return r.refuse(p, fsv1alpha1.ReasonSecretNotFound,
			fmt.Sprintf("Secret %q does not exist", name))
	case err != nil:
		return controller.Outcome{}, errors.Wrapf(err, "get secret %q", name)
	}

	for _, key := range keys {
		if len(secret.Data[key]) == 0 {
			return r.refuse(p, fsv1alpha1.ReasonSecretInvalid,
				fmt.Sprintf("Secret %q has no %q", name, key))
		}
	}

	return controller.Continue()
}

// reconcileServices applies the peer and client Services. They come before the
// pods on purpose: a node's advertise address has to resolve when it starts.
func (r *Reconciler) reconcileServices(ctx context.Context, p *pass) (controller.Outcome, error) {
	for _, service := range []client.Object{
		NewPeersService(p.cluster),
		NewClientService(p.cluster),
	} {
		if err := r.apply(ctx, p.cluster, service); err != nil {
			return controller.Outcome{}, err
		}
	}

	return controller.Continue()
}

// reconcileNodeConfigs renders and applies every node's configuration.
func (r *Reconciler) reconcileNodeConfigs(ctx context.Context, p *pass) (controller.Outcome, error) {
	// Declarative credentials merge in here once FSAccessKey lands (SPEC §7).
	opts := RenderOptions{}

	configs, err := RenderNodeConfigs(p.cluster, p.nodes, opts)
	if err != nil {
		return controller.Outcome{}, err
	}

	restarts := make(map[string]string, len(p.nodes))

	for _, node := range p.nodes {
		revision, err := RestartRevision(p.cluster, node, opts)
		if err != nil {
			return controller.Outcome{}, err
		}

		restarts[node.Name] = revision

		if err := r.apply(ctx, p.cluster, NewNodeConfigSecret(p.cluster, node, configs[node.Name])); err != nil {
			return controller.Outcome{}, err
		}
	}

	p.configs = configs
	p.restarts = restarts

	return controller.Continue()
}

// reconcileNodes applies one StatefulSet per node.
//
// Creating nodes is additive — new nodes join and the rebalancer converges, so
// they may all appear at once (SPEC §8.4). Replacing an existing node's pod is
// the sequenced part, and that is the rolling state machine's job.
func (r *Reconciler) reconcileNodes(ctx context.Context, p *pass) (controller.Outcome, error) {
	sets := make([]*appsv1.StatefulSet, 0, len(p.nodes))

	for _, node := range p.nodes {
		sets = append(sets, NewStatefulSet(p.cluster, node, p.restarts[node.Name]))
	}

	revision, err := PodTemplateRevision(sets)
	if err != nil {
		return controller.Outcome{}, err
	}

	p.templateRevision = revision

	for _, set := range sets {
		if err := r.apply(ctx, p.cluster, set); err != nil {
			return controller.Outcome{}, err
		}
	}

	return controller.Continue()
}

// reconcileBudget applies the disruption budget.
func (r *Reconciler) reconcileBudget(ctx context.Context, p *pass) (controller.Outcome, error) {
	if err := r.apply(ctx, p.cluster, NewPodDisruptionBudget(p.cluster)); err != nil {
		return controller.Outcome{}, err
	}

	return controller.Continue()
}

// apply writes an object with server-side apply, owned by this operator and by
// the cluster: the operator holds the fields it sets and leaves the rest
// alone, and garbage collection takes the object down with the cluster.
func (r *Reconciler) apply(ctx context.Context, cluster *fsv1alpha1.FSCluster, object client.Object) error {
	if err := controllerutil.SetControllerReference(cluster, object, r.Scheme); err != nil {
		return errors.Wrap(err, "set owner")
	}

	config, err := applyConfiguration(object)
	if err != nil {
		return err
	}

	if err := r.Apply(ctx, config,
		client.FieldOwner(OperatorName),
		client.ForceOwnership,
	); err != nil {
		return errors.Wrapf(err, "apply %s %q", object.GetObjectKind().GroupVersionKind().Kind, object.GetName())
	}

	return nil
}

// applyConfiguration turns a built object into what server-side apply takes.
//
// The builders produce typed objects — they are far easier to read and test
// than apply configurations — and the two fields a rendered object cannot
// mean anything about are dropped: a creation timestamp it does not have, and
// a status only the API server writes.
func applyConfiguration(object client.Object) (runtime.ApplyConfiguration, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, errors.Wrapf(err, "convert %q", object.GetName())
	}

	delete(content, "status")
	unstructured.RemoveNestedField(content, "metadata", "creationTimestamp")

	return client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: content}), nil
}

// createOnce creates an object if it is absent and leaves an existing one
// exactly as it is. It is how generated secret material is handled: applying
// it every pass would hand the cluster a new secret every pass.
func (r *Reconciler) createOnce(ctx context.Context, cluster *fsv1alpha1.FSCluster, object client.Object) error {
	if err := controllerutil.SetControllerReference(cluster, object, r.Scheme); err != nil {
		return errors.Wrap(err, "set owner")
	}

	err := r.Create(ctx, object)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrapf(err, "create %q", object.GetName())
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fsv1alpha1.FSCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Named("fscluster").
		Complete(r)
}
