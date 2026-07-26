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
	"sync"
	"time"

	"github.com/go-faster/errors"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/etcdstore"
	"github.com/go-faster/fs-operator/internal/fsclient"
	"github.com/go-faster/fs-operator/internal/metrics"
	"github.com/go-faster/fs-operator/internal/validation"
)

// resyncInterval refreshes the health-derived part of the status even when no
// watch fires: a pod that stops being ready is an event, but a cluster slowly
// falling behind is not.
const resyncInterval = 5 * time.Minute

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

	// Admin builds an admin API client for a node's endpoint and bearer token.
	// Nil uses a pooled default that reuses connections across reconciles;
	// tests inject a fake to stand in for unreachable pods.
	Admin func(baseURL, token string) (fsclient.Interface, error)

	// OperatorNamespace is where the operator runs, so a cluster's opt-in
	// NetworkPolicy can allow the admin and peer ports from it (SPEC §9).
	OperatorNamespace string

	// FSImageRegistry, when set, replaces the registry host of every fs node
	// image the operator deploys, so an air-gapped install pulls from a
	// private mirror without every FSCluster having to spell out the registry.
	// Empty keeps each cluster's own image.repository.
	FSImageRegistry string

	// EtcdPurge deletes a cluster's keys from etcd when it is deleted with
	// etcd.cleanupOnDelete (SPEC §8.6). Nil talks to etcd directly; tests
	// inject a fake to stand in for a control plane they do not run.
	EtcdPurge func(ctx context.Context, cfg etcdstore.Config, prefix string) (int64, error)

	poolOnce sync.Once
	pool     *fsclient.Pool
}

// adminClient builds an admin client for a node, defaulting to the shared
// connection pool.
func (r *Reconciler) adminClient(baseURL, token string) (fsclient.Interface, error) {
	if r.Admin != nil {
		return r.Admin(baseURL, token)
	}

	r.poolOnce.Do(func() { r.pool = fsclient.NewPool() })

	return r.pool.Client(baseURL, token)
}

// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fs.go-faster.org,resources=fsclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=podmonitors,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings one FSCluster's resources in line with its spec.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	object := &fsv1alpha1.FSCluster{}
	if err := r.Get(ctx, req.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !object.DeletionTimestamp.IsZero() {
		// Stop reporting on it now rather than when the finalizer releases: a
		// gauge that outlives its cluster reports ready=0 forever, on a name
		// nothing will reconcile again, which reads exactly like an outage that
		// never resolves (SPEC §10).
		metrics.Forget(object.Namespace, object.Name)

		// Everything the operator creates carries an owner reference, so
		// garbage collection takes it down, and each node's claim retention
		// policy decides the fate of its data. Only etcd state outlives that,
		// which is what the finalizer is for (SPEC §8.6).
		return r.finalize(ctx, object)
	}

	if err := r.reconcileFinalizer(ctx, object); err != nil {
		return ctrl.Result{}, err
	}

	result, err := r.pipeline().Run(ctx, newPass(object, r.FSImageRegistry))
	if result.RequeueAfter == 0 && err == nil {
		result.RequeueAfter = resyncInterval
	}

	return result, err
}

// pipeline is a reconcile pass: resources first, in the order they depend on
// each other, and the status last so that it describes what actually
// happened — including a pass that was refused or failed (SPEC §8).
func (r *Reconciler) pipeline() pipeline.Pipeline[*pass] {
	return pipeline.Pipeline[*pass]{
		{Name: "validate", Run: r.validate},
		{Name: "etcdsecurity", Run: r.resolveEtcdSecurity},
		{Name: "decommission", Run: r.planDecommission},
		{Name: "render", Run: r.render},
		{Name: "observe", AlwaysRun: true, Run: r.observe},
		{Name: "secrets", Run: r.reconcileSecrets},
		{Name: "services", Run: r.reconcileServices},
		{Name: "etcd", Run: r.reconcileEtcd},
		{Name: "configs", Run: r.reconcileNodeConfigs},
		{Name: "convergence", Run: r.gatherConvergence},
		{Name: "storage", Run: r.reconcileStorage},
		{Name: "nodes", Run: r.reconcileNodes},
		{Name: "drain", Run: r.reconcileDecommission},
		{Name: "reload", Run: r.reconcileReload},
		{Name: "publicread", Run: r.reconcilePublicRead},
		{Name: "rootcredential", Run: r.reconcileRootCredential},
		{Name: migrateName, Run: r.reconcileMigration},
		{Name: "budget", Run: r.reconcileBudget},
		{Name: "networkpolicy", Run: r.reconcileNetworkPolicy},
		{Name: "podmonitor", Run: r.reconcilePodMonitor},
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

	// configs holds each node's rendered configuration (bytes + revision
	// marker), and restarts the fingerprint of its restart-requiring part;
	// both are filled by the render step.
	configs  map[string]RenderedConfig
	restarts map[string]string

	// desired holds the node StatefulSets the spec asks for, in node order,
	// and templateRevision fingerprints them together for the status.
	desired          []*appsv1.StatefulSet
	templateRevision string

	// live holds the node StatefulSets that exist, and health what they say
	// about the running cluster.
	live   map[string]*appsv1.StatefulSet
	health health

	// convergence is what the admin API reports about reconvergence; the
	// rollout gates on it and the status reports Converged from it.
	convergence convergence

	// decommission is the node the spec no longer declares that is being
	// drained and removed, if any (SPEC §8.4).
	decommission decommission

	// etcd is what the cluster's referenced etcd Secrets contain, read before
	// anything is rendered from them (SPEC §11.4).
	etcd etcdSecurity

	// schema is the cluster's observed schema versions, filled by the
	// migration step for the status.
	schema *fsv1alpha1.SchemaVersionStatus

	// rootCredential is whether the cluster's key store holds the root
	// credential the operator holds — the difference between a cluster that is
	// usable and one that only looks it (SPEC §8.6).
	rootCredential rootCredentialState

	// update is the rolling change in flight, if any.
	update *fsv1alpha1.UpdateStatus

	// conditions accumulate through the pass and are written by the status
	// step, so that a refusal and its explanation reach the object together.
	conditions []metav1.Condition

	// failure is the error that stopped the pass, if any.
	failure error
}

// newPass prepares the state of a reconcile pass. imageRegistry, when set,
// rewrites the registry host of the defaulted fs image so every node pulls
// from a private mirror (air-gapped installs).
func newPass(object *fsv1alpha1.FSCluster, imageRegistry string) *pass {
	cluster := object.DeepCopy()
	cluster.Spec.WithDefaults()
	cluster.Spec.Image.Repository = fsv1alpha1.ApplyRegistry(cluster.Spec.Image.Repository, imageRegistry)

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

// desiredSet is the StatefulSet a node should be running this pass.
func (p *pass) desiredSet(name string) (*appsv1.StatefulSet, bool) {
	for _, set := range p.desired {
		if set.Name == name {
			return set, true
		}
	}

	return nil, false
}

// hasCondition reports whether a condition of the given type is already queued,
// so a later step can defer to an earlier, more specific one.
func (p *pass) hasCondition(conditionType string) bool {
	return slices.ContainsFunc(p.conditions, func(c metav1.Condition) bool {
		return c.Type == conditionType
	})
}

// validate performs the cross-field checks CEL cannot express, and refuses the
// spec rather than half-applying it (SPEC §5.1).
//
// The admission webhook runs the same checks at apply time, which is where a
// user should find out (§16). This is not a leftover: a webhook can be
// disabled, unreachable behind a Fail policy that an admin relaxed, or simply
// not have existed when the object was stored — and a spec that reaches the
// controller unchecked is one that could build a cluster unable to host its own
// data. Both call internal/validation, so the two can never disagree.
func (r *Reconciler) validate(_ context.Context, p *pass) (pipeline.Outcome, error) {
	spec := &p.cluster.Spec

	if failure := validation.Cluster(spec); failure != nil {
		return r.refuse(p, failure.Reason, failure.Message)
	}

	// Allowed, but worth saying out loud. The webhook returns these as
	// admission warnings; here they are events, for a cluster that predates the
	// webhook or was applied while it was off.
	for _, warning := range validation.ClusterWarnings(spec) {
		r.Recorder.Event(p.object, corev1.EventTypeWarning,
			fsv1alpha1.ReasonUnsupportedTopology, warning)
	}

	p.setCondition(fsv1alpha1.ConditionSpecValid, metav1.ConditionTrue, fsv1alpha1.ReasonSpecValid,
		"Spec passes cross-field validation")

	return pipeline.Continue()
}

// refuse records why a spec is not applied and stops the pass. Nothing is
// mutated: a spec the operator will not apply leaves the running cluster
// exactly as it was.
func (r *Reconciler) refuse(p *pass, reason, message string) (pipeline.Outcome, error) {
	p.setCondition(fsv1alpha1.ConditionSpecValid, metav1.ConditionFalse, reason, message)
	r.Recorder.Event(p.object, corev1.EventTypeWarning, reason, message)

	return pipeline.Block(reason)
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
func (r *Reconciler) reconcileSecrets(ctx context.Context, p *pass) (pipeline.Outcome, error) {
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
			return pipeline.Outcome{}, err
		}

		if err := r.createOnce(ctx, p.cluster, object); err != nil {
			return pipeline.Outcome{}, err
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

	return pipeline.Continue()
}

// requireSecret blocks the pass when a referenced Secret is missing or does
// not carry the keys fs will look for. Pointing a pod at a Secret that is not
// there produces a container that never starts and no explanation.
func (r *Reconciler) requireSecret(ctx context.Context, p *pass, name string, keys []string) (pipeline.Outcome, error) {
	var secret corev1.Secret

	err := r.Get(ctx, types.NamespacedName{Namespace: p.cluster.Namespace, Name: name}, &secret)

	switch {
	case apierrors.IsNotFound(err):
		return r.refuse(p, fsv1alpha1.ReasonSecretNotFound,
			fmt.Sprintf("Secret %q does not exist", name))
	case err != nil:
		return pipeline.Outcome{}, errors.Wrapf(err, "get secret %q", name)
	}

	for _, key := range keys {
		if len(secret.Data[key]) == 0 {
			return r.refuse(p, fsv1alpha1.ReasonSecretInvalid,
				fmt.Sprintf("Secret %q has no %q", name, key))
		}
	}

	return pipeline.Continue()
}

// reconcileServices applies the peer and client Services. They come before the
// pods on purpose: a node's advertise address has to resolve when it starts.
func (r *Reconciler) reconcileServices(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	for _, service := range []client.Object{
		NewPeersService(p.cluster),
		NewClientService(p.cluster),
	} {
		if err := r.apply(ctx, p.cluster, service); err != nil {
			return pipeline.Outcome{}, err
		}
	}

	return pipeline.Continue()
}

// render turns the spec into what every node should be running: its
// configuration, the fingerprint of the part only a restart can apply, and its
// StatefulSet. Nothing here touches the API server, so the steps that observe
// and the steps that write both work from the same desired state.
func (r *Reconciler) render(_ context.Context, p *pass) (pipeline.Outcome, error) {
	// Credentials are cluster-wide in etcd (fs §6.8), managed through the admin
	// API by the FSAccessKey controller and the public-read step — none are
	// rendered into the config here (SPEC §7).
	//
	// A node being decommissioned renders with every disk drained, which is
	// what makes the rebalancer move its data off (SPEC §8.4).
	opts := RenderOptions{EtcdClientCert: p.etcd.clientCert}
	if p.decommission.active() {
		opts.Drained = map[string]bool{p.decommission.node.Name: true}
	}

	configs, err := RenderNodeConfigs(p.cluster, p.nodes, opts)
	if err != nil {
		return pipeline.Outcome{}, err
	}

	restarts := make(map[string]string, len(p.nodes))
	desired := make([]*appsv1.StatefulSet, 0, len(p.nodes))

	for _, node := range p.nodes {
		revision, err := RestartRevision(p.cluster, node, opts)
		if err != nil {
			return pipeline.Outcome{}, err
		}

		restarts[node.Name] = revision

		// A decommissioning node keeps the StatefulSet it is already running,
		// restamped onto the drained configuration: the spec no longer
		// describes where it runs, so rebuilding it from the spec could move
		// the pod away from its own data.
		var set *appsv1.StatefulSet

		if p.decommission.draining(node.Name) {
			if set, err = drainedStatefulSet(p.decommission.set, revision); err != nil {
				return pipeline.Outcome{}, err
			}
		} else {
			set = NewStatefulSet(p.cluster, node, revision)
			if err := stampTemplateRevision(set); err != nil {
				return pipeline.Outcome{}, err
			}
		}

		desired = append(desired, set)
	}

	templateRevision, err := PodTemplateRevision(desired)
	if err != nil {
		return pipeline.Outcome{}, err
	}

	p.configs = configs
	p.restarts = restarts
	p.desired = desired
	p.templateRevision = templateRevision

	return pipeline.Continue()
}

// reconcileNodeConfigs applies every node's configuration.
func (r *Reconciler) reconcileNodeConfigs(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	for _, node := range p.nodes {
		if err := r.apply(ctx, p.cluster, NewNodeConfigSecret(p.cluster, node, p.configs[node.Name])); err != nil {
			return pipeline.Outcome{}, err
		}
	}

	return pipeline.Continue()
}

// reconcileBudget applies the disruption budget.
func (r *Reconciler) reconcileBudget(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	if err := r.apply(ctx, p.cluster, NewPodDisruptionBudget(p.cluster)); err != nil {
		return pipeline.Outcome{}, err
	}

	return pipeline.Continue()
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
		Owns(&batchv1.Job{}).
		Named("fscluster").
		Complete(r)
}
