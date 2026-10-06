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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// FSClusterSpec defines the desired state of a go-faster/fs cluster: a set of
// storage nodes spread over failure domains (racks), sharing a layout that
// assigns every partition of the data to nodes.
// +kubebuilder:validation:XValidation:rule="has(self.clusterSecretRef) == has(oldSelf.clusterSecretRef)",message="clusterSecretRef is immutable: it cannot be added or removed"
type FSClusterSpec struct {
	// image is the fs container image to run on every node. Defaults to the
	// pinned fs release this operator version is validated against.
	// +kubebuilder:default={}
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// topology declares the cluster's nodes and failure domains.
	// +required
	Topology TopologySpec `json:"topology"`

	// storage sizes each node's data volume.
	// +required
	Storage StorageSpec `json:"storage"`

	// layout tunes the cluster layout the operator applies. Ignored by a
	// single-node cluster, which has no layout to apply.
	// +optional
	Layout LayoutSpec `json:"layout,omitempty"`

	// clusterSecretRef references a Secret with key "secret" holding the
	// shared cluster secret (HMAC peer auth, min 16 characters). Generated
	// if omitted. Immutable: fs has no secret rotation; mixed secrets
	// partition the cluster.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterSecretRef is immutable"
	// +optional
	ClusterSecretRef *corev1.LocalObjectReference `json:"clusterSecretRef,omitempty"`

	// auth configures S3 authentication.
	// +optional
	Auth AuthSpec `json:"auth,omitempty"`

	// s3 configures how the S3 endpoint is exposed.
	// +optional
	S3 S3Spec `json:"s3,omitempty"`

	// updatePolicy tunes rolling changes.
	// +optional
	UpdatePolicy UpdatePolicySpec `json:"updatePolicy,omitempty"`

	// observability configures telemetry of the fs pods.
	// +optional
	Observability ObservabilitySpec `json:"observability,omitempty"`

	// networkPolicy, when true, creates a NetworkPolicy restricting the peer
	// (7080) and admin (8090) ports to cluster pods and the operator. S3
	// stays unrestricted.
	// +optional
	NetworkPolicy bool `json:"networkPolicy,omitempty"`

	// podTemplate carries pod-level knobs applied uniformly to every node's
	// StatefulSet.
	// +optional
	PodTemplate PodTemplate `json:"podTemplate,omitempty"`
}

// LayoutSpec tunes the layout: which partition of the data each node holds.
type LayoutSpec struct {
	// widths are the slot counts the layout spreads over distinct failure
	// domains: 3 for replicated data (rf3), and K+M for every erasure scheme
	// a bucket uses, so that an FSBucket with scheme "ec:4,2" needs 6 here.
	// Each width needs at least that many nodes. Defaults to [3]. Adding a
	// width moves data; removing one that a bucket still uses is refused by
	// fs.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=64
	// +optional
	Widths []int32 `json:"widths,omitempty"`
}

// ImageSpec identifies the fs container image.
type ImageSpec struct {
	// repository is the image repository.
	// +kubebuilder:default="ghcr.io/go-faster/fs"
	// +optional
	Repository string `json:"repository,omitempty"`

	// tag is the image tag. Defaults to the pinned fs release this operator
	// version is validated against — always set a pinned version, never a
	// floating tag: cluster upgrades are deliberate, one-node-at-a-time
	// operations.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default="v0.14.1"
	// +optional
	Tag string `json:"tag,omitempty"`

	// digest pins the image by content instead of by tag, as
	// "sha256:<hex>". When set it wins over tag, and the nodes run
	// repository@digest — the reference a mirror cannot silently change
	// under a cluster. A digest already written into repository is honoured
	// too, which is how the chart pins the operator's own image.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-fA-F0-9]{32,128}$`
	// +optional
	Digest string `json:"digest,omitempty"`

	// pullPolicy is the image pull policy.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`

	// pullSecrets are image pull secrets for the fs pods.
	// +optional
	PullSecrets []corev1.LocalObjectReference `json:"pullSecrets,omitempty"`
}

// TopologySpec declares the cluster's nodes and failure domains. Exactly one
// of nodes or racks must be set.
// +kubebuilder:validation:XValidation:rule="has(self.nodes) != has(self.racks)",message="exactly one of nodes or racks must be set"
type TopologySpec struct {
	// nodes is the flat topology: N nodes, each its own failure domain.
	// One node is a development install: one volume, no peers, no
	// replication and no failure tolerance. Two is refused: a replicated
	// layout needs three nodes.
	// +kubebuilder:validation:XValidation:rule="self != 2",message="a cluster has one node or at least three"
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +optional
	Nodes *int32 `json:"nodes,omitempty"`

	// racks are explicit failure domains. Every node of a rack joins the
	// layout with the rack's name as its fs rack and the rack's zone as its
	// fs zone, and the layout spreads each partition over zones first, then
	// racks. Rack membership is declared here and pinned with node affinity
	// — it is never derived from where a pod happens to be scheduled.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Racks []RackSpec `json:"racks,omitempty"`

	// podAntiAffinity spreads fs nodes over distinct Kubernetes nodes.
	// Required (the default) keeps the failure model honest; use Preferred
	// or None only for dev clusters.
	// +kubebuilder:validation:Enum=Required;Preferred;None
	// +kubebuilder:default=Required
	// +optional
	PodAntiAffinity AntiAffinityMode `json:"podAntiAffinity,omitempty"`
}

// AntiAffinityMode selects how strictly fs nodes are spread over Kubernetes
// nodes.
type AntiAffinityMode string

// Anti-affinity modes.
const (
	AntiAffinityRequired  AntiAffinityMode = "Required"
	AntiAffinityPreferred AntiAffinityMode = "Preferred"
	AntiAffinityNone      AntiAffinityMode = "None"
)

// RackSpec is one failure domain and its scheduling constraints.
type RackSpec struct {
	// name identifies the rack; it becomes the fs rack label and part of
	// node names. Immutable per entry (renaming a rack is a decommission
	// plus a new rack).
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// nodes is the number of fs nodes in this rack.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +required
	Nodes int32 `json:"nodes"`

	// zone pins the rack's nodes to a topology.kubernetes.io/zone value
	// (sugar for nodeSelector), and is the fs zone of the rack's nodes:
	// racks sharing a zone are one failure domain at the zone level.
	// +optional
	Zone string `json:"zone,omitempty"`

	// nodeSelector pins the rack's nodes to matching Kubernetes nodes.
	// Merged over zone.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// StorageSpec sizes each node's data volume and says how it is reclaimed.
type StorageSpec struct {
	// size is the capacity of each node's data volume, which holds
	// everything the node stores: metadata, blocks and runtime state. It is
	// also the capacity the node joins the layout with, so the layout gives
	// a node a share of the data in proportion to it. It may only grow, and
	// growing it requires the StorageClass to allow volume expansion.
	// +required
	Size resource.Quantity `json:"size"`

	// storageClass selects the StorageClass of the data volumes; empty
	// uses the cluster default. Metadata is written on every request, so
	// this is worth putting on the fastest class available.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// reclaimPolicy controls what happens to a node's volume when the node
	// is removed or the cluster is deleted.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

// ReclaimPolicy controls the fate of data-bearing resources on removal.
type ReclaimPolicy string

// Reclaim policies.
const (
	ReclaimRetain ReclaimPolicy = "Retain"
	ReclaimDelete ReclaimPolicy = "Delete"
)

// AuthSpec configures S3 authentication.
type AuthSpec struct {
	// rootCredentialsSecretRef references a Secret with keys "access-key"
	// and "secret-key", granted admin on all buckets. Generated if omitted.
	// +optional
	RootCredentialsSecretRef *corev1.LocalObjectReference `json:"rootCredentialsSecretRef,omitempty"`

	// publicReadBuckets may be read anonymously.
	// +optional
	PublicReadBuckets []string `json:"publicReadBuckets,omitempty"`
}

// S3Spec configures the S3 endpoint exposure.
type S3Spec struct {
	// service shapes the client Service in front of the S3 listeners.
	// +optional
	Service S3ServiceSpec `json:"service,omitempty"`

	// tls terminates TLS in fs itself using a kubernetes.io/tls Secret.
	// Certificate renewals hot-reload without restarts.
	// +optional
	TLS S3TLSSpec `json:"tls,omitempty"`
}

// S3ServiceSpec shapes the S3 client Service.
type S3ServiceSpec struct {
	// type is the Service type.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`

	// port is the S3 port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`

	// annotations are added to the Service (e.g. for load-balancer
	// controllers).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// S3TLSSpec enables TLS termination in fs.
type S3TLSSpec struct {
	// secretName names a kubernetes.io/tls Secret with the serving
	// certificate. Empty serves plaintext.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// UpdatePolicySpec tunes rolling changes.
type UpdatePolicySpec struct {
	// convergenceTimeout bounds how long the operator waits, between node
	// restarts, for the cluster to reconverge (pod ready, every node up and
	// on the current layout, no layout change in transition). On timeout
	// the rollout halts — it never proceeds to another node while the
	// cluster is unconverged — and resumes automatically when the gate
	// passes.
	// +kubebuilder:default="30m"
	// +optional
	ConvergenceTimeout *metav1.Duration `json:"convergenceTimeout,omitempty"`
}

// ObservabilitySpec configures telemetry of the fs pods.
type ObservabilitySpec struct {
	// otlp is the OTLP destination every signal exported over OTLP is sent
	// to, and the protocol they use unless a signal names its own.
	// +optional
	OTLP OTLPSpec `json:"otlp,omitempty"`

	// traces selects the trace exporter, its destination and its transport.
	// Defaults to "otlp" when an endpoint is set and "none" when none is —
	// the SDK's own default is "otlp" unconditionally, which without a
	// destination means an upload to localhost failing every interval.
	// +optional
	Traces SignalSpec `json:"traces,omitempty"`

	// logs selects the log exporter and its transport. Defaults to "none"
	// even with an endpoint set: fs logs to stdout, where the cluster's log
	// pipeline already collects it, so a second copy over OTLP is a choice
	// and not the obvious consequence of having a collector.
	// +optional
	Logs SignalSpec `json:"logs,omitempty"`

	// metrics selects the metric exporter and its transport. Defaults to
	// "prometheus": Kubernetes collects metrics by scraping, the operator
	// gives every node a metrics port for it, and podMonitor scrapes that
	// port. Choosing "otlp" or "none" retires the port with it.
	// +optional
	Metrics MetricsSpec `json:"metrics,omitempty"`

	// logLevel is OTEL_LOG_LEVEL, the level fs logs at. debug additionally
	// turns on per-request logging in fs itself, which on a busy endpoint is
	// a line per request.
	//
	// The enum is the set go-faster/sdk can parse (zapcore levels), and it
	// earns its keep: the SDK panics on a level it does not recognise, so a
	// typo here would otherwise be a crash loop across every node rather
	// than a rejected field. Levels above error exist because the SDK has
	// them; a cluster that logs nothing below panic is not one you can
	// operate.
	// +kubebuilder:validation:Enum=debug;info;warn;error;dpanic;panic;fatal
	// +kubebuilder:default=info
	// +optional
	LogLevel string `json:"logLevel,omitempty"`

	// podMonitor creates a PodMonitor for the fs pods' Prometheus metrics
	// (requires the monitoring.coreos.com API group).
	// +optional
	PodMonitor bool `json:"podMonitor,omitempty"`

	// pprof serves go-faster/sdk's pprof endpoints on port 9010. On by
	// default, which is not the SDK's own default: a cluster that is
	// misbehaving is when profiles are wanted, and that is a bad moment to
	// discover the listener has to be turned on and the nodes restarted.
	// Turn it off where an open profiling endpoint is not acceptable — the
	// container port and its NetworkPolicy rule go with it.
	// +kubebuilder:default=true
	// +optional
	Pprof *bool `json:"pprof,omitempty"`

	// resourceAttributes are added to OTEL_RESOURCE_ATTRIBUTES, alongside
	// the ones the operator derives (service, cluster, namespace, node,
	// rack). Setting the variable through podTemplate.extraEnv replaces
	// those instead, which is what the dashboards and the PodMonitor read.
	// +optional
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
}

// OTLPSpec is the OTLP exporter destination shared by every signal.
//
// What is not here — Pyroscope, propagators, export intervals, pprof routes,
// the OTEL_GO_X_* switches — is reachable through podTemplate.extraEnv, which
// is applied last and therefore wins over what the operator sets. See the
// SDK's reference table: https://github.com/go-faster/sdk#reference
type OTLPSpec struct {
	// endpoint is OTEL_EXPORTER_OTLP_ENDPOINT: where every signal exported
	// over OTLP is sent unless it names its own. A signal whose exporter is
	// "otlp" needs one of the two, since without a destination the SDK ships
	// to localhost:4318 and logs a failed upload every interval.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// protocol is OTEL_EXPORTER_OTLP_PROTOCOL, the transport every OTLP
	// signal uses. Empty leaves it unset, which is the SDK's own default of
	// grpc.
	//
	// It cannot be combined with a per-signal protocol, and that is fs's
	// SDK rather than a choice made here: it reads this variable first and
	// consults OTEL_EXPORTER_OTLP_<SIGNAL>_PROTOCOL only when it is unset,
	// so setting both would leave the per-signal value inert. Set this one
	// for a cluster that speaks one transport, or the per-signal ones for a
	// cluster that does not.
	// +kubebuilder:validation:Enum=grpc;http/protobuf
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// SignalSpec selects how one telemetry signal leaves the node.
type SignalSpec struct {
	// exporter is where the signal goes: "otlp" or "none".
	// +kubebuilder:validation:Enum=otlp;none
	// +optional
	Exporter string `json:"exporter,omitempty"`

	// endpoint overrides otlp.endpoint for this signal alone. Note the
	// OpenTelemetry rule the exporters implement: the shared endpoint has
	// "/v1/<signal>" appended over HTTP, a per-signal endpoint is used
	// exactly as written, path included.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// protocol overrides otlp.protocol for this signal alone, which is what
	// a collector that speaks one transport on one port needs.
	// +kubebuilder:validation:Enum=grpc;http/protobuf
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// MetricsSpec selects how metrics leave the node. It is its own type because
// metrics have an exporter the other signals do not: the scrape endpoint.
type MetricsSpec struct {
	// exporter is "prometheus" (the default: served on the node's metrics
	// port for a scraper), "otlp" (pushed to an endpoint) or "none".
	// +kubebuilder:validation:Enum=prometheus;otlp;none
	// +optional
	Exporter string `json:"exporter,omitempty"`

	// endpoint overrides otlp.endpoint for metrics alone, under the same
	// path rule as the other signals. Ignored unless the exporter is
	// "otlp".
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// protocol overrides otlp.protocol for metrics alone. Ignored unless
	// the exporter is "otlp".
	// +kubebuilder:validation:Enum=grpc;http/protobuf
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// PodTemplate carries pod-level knobs applied uniformly to every node's
// StatefulSet.
type PodTemplate struct {
	// resources are the fs container's resource requirements.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// nodeSelector applies to every node's pod (racks add their own on
	// top).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations apply to every node's pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// priorityClassName applies to every node's pod.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// annotations are added to every node's pod.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// labels are added to every node's pod.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// extraEnv appends environment variables to the fs container.
	// +optional
	ExtraEnv []corev1.EnvVar `json:"extraEnv,omitempty"`
}

// FSClusterStatus defines the observed state of an FSCluster.
type FSClusterStatus struct {
	// observedGeneration is the last spec generation the controller acted
	// on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// nodes is the desired node count.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`

	// readyNodes is the number of node pods that are Ready.
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// upNodes is the number of nodes the cluster's own view reports up:
	// reachable by their peers, which a Ready pod alone does not prove.
	// +optional
	UpNodes int32 `json:"upNodes,omitempty"`

	// configurationRevision is the hash of the desired rendered configs.
	// +optional
	ConfigurationRevision string `json:"configurationRevision,omitempty"`

	// statefulSetRevision is the hash of the desired pod templates.
	// +optional
	StatefulSetRevision string `json:"statefulSetRevision,omitempty"`

	// currentRevision is the revision every node has converged to.
	// +optional
	CurrentRevision string `json:"currentRevision,omitempty"`

	// updateRevision is the revision being rolled out.
	// +optional
	UpdateRevision string `json:"updateRevision,omitempty"`

	// layout is the cluster layout as fs reports it.
	// +optional
	Layout *LayoutStatus `json:"layout,omitempty"`

	// update is present while a rolling change is in flight.
	// +optional
	Update *UpdateStatus `json:"update,omitempty"`

	// endpoints are the cluster's client endpoints.
	// +optional
	Endpoints *EndpointsStatus `json:"endpoints,omitempty"`

	// conditions represent the current state of the FSCluster. See the
	// documented condition types (SpecValid, ReconcileSucceeded, Ready,
	// NodesHealthy, ClusterSizeAligned, ConfigurationInSync, Converged).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// LayoutStatus is the cluster layout as fs reports it.
type LayoutStatus struct {
	// version is the layout version the cluster has adopted; every applied
	// change increments it.
	// +optional
	Version int64 `json:"version,omitempty"`

	// members is the number of nodes the layout gives data to.
	// +optional
	Members int32 `json:"members,omitempty"`

	// widths are the slot counts the layout spreads for.
	// +optional
	Widths []int32 `json:"widths,omitempty"`

	// retainedVersions are older versions still in transition: data is
	// moving to where the current version puts it, and writes go to both.
	// Empty when nothing is moving.
	// +optional
	RetainedVersions []int64 `json:"retainedVersions,omitempty"`
}

// UpdatePhase is a phase of the rolling-change state machine.
type UpdatePhase string

// Update phases.
const (
	UpdatePhasePreflight    UpdatePhase = "Preflight"
	UpdatePhaseRollingNodes UpdatePhase = "RollingNodes"
	// UpdatePhaseDraining is a node being removed: out of the layout and
	// still running while the cluster moves its data elsewhere (SPEC §8.4).
	UpdatePhaseDraining UpdatePhase = "Draining"
)

// UpdateStatus describes the rolling change in flight.
type UpdateStatus struct {
	// phase is the state-machine phase.
	// +kubebuilder:validation:Enum=Preflight;RollingNodes;Draining
	// +optional
	Phase UpdatePhase `json:"phase,omitempty"`

	// node is the node currently being replaced or removed.
	// +optional
	Node string `json:"node,omitempty"`

	// startedAt is when the rolling change started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
}

// EndpointsStatus lists the cluster's client endpoints.
type EndpointsStatus struct {
	// s3 is the in-cluster S3 endpoint URL.
	// +optional
	S3 string `json:"s3,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fsc
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.nodes`
// +kubebuilder:printcolumn:name="ReadyNodes",type=integer,JSONPath=`.status.readyNodes`
// +kubebuilder:printcolumn:name="Layout",type=integer,JSONPath=`.status.layout.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FSCluster is the Schema for the fsclusters API.
type FSCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of FSCluster
	// +required
	Spec FSClusterSpec `json:"spec"`

	// status defines the observed state of FSCluster
	// +optional
	Status FSClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FSClusterList contains a list of FSCluster
type FSClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FSCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FSCluster{}, &FSClusterList{})
		return nil
	})
}
