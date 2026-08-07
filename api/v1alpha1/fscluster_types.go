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
// storage nodes spread over failure domains (racks), replicating objects at
// write quorum with an etcd control plane.
// +kubebuilder:validation:XValidation:rule="has(self.clusterSecretRef) == has(oldSelf.clusterSecretRef)",message="clusterSecretRef is immutable: it cannot be added or removed"
type FSClusterSpec struct {
	// image is the fs container image to run on every node. Defaults to the
	// pinned fs release this operator version is validated against.
	// +kubebuilder:default={}
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// scheme is the default replication scheme for all buckets: "rf2.5"
	// (2 replicas + half parity), "rf3" (3 replicas) or "ec:k,m"
	// (Reed-Solomon). Changeable at runtime: new writes use it immediately
	// and existing objects converge via repair/rebalance. The controller
	// refuses a scheme the topology cannot host (distinct failure domains
	// below the scheme requirement). Ignored by a single-node cluster,
	// which stores objects on one disk and replicates nothing.
	// +kubebuilder:validation:Pattern=`^(rf2\.5|rf3|ec:[1-9][0-9]*,[1-9][0-9]*)$`
	// +kubebuilder:default="rf2.5"
	// +optional
	Scheme string `json:"scheme,omitempty"`

	// topology declares the cluster's nodes and failure domains.
	// +required
	Topology TopologySpec `json:"topology"`

	// storage declares each node's disks. Every disk becomes one
	// PersistentVolumeClaim per node.
	// +required
	Storage StorageSpec `json:"storage"`

	// etcd configures the control plane the cluster registers in. Required
	// for a clustered topology; a single-node cluster runs fs's
	// non-clustered filesystem backend, which has no control plane, and
	// must leave it unset.
	// +optional
	Etcd EtcdSpec `json:"etcd,omitempty"`

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

	// rebalance tunes the automatic rebalancer. Zero values defer to the fs
	// defaults.
	// +optional
	Rebalance RebalanceSpec `json:"rebalance,omitempty"`

	// integrity configures object integrity checking (scrub).
	// +optional
	Integrity IntegritySpec `json:"integrity,omitempty"`

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
	// +kubebuilder:default="v0.13.0"
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
	// One node is a development install: that node runs fs's single-node
	// filesystem backend — one disk, no etcd, no replication, no failure
	// tolerance — instead of joining a cluster.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +optional
	Nodes *int32 `json:"nodes,omitempty"`

	// racks are explicit failure domains; placement spreads object copies
	// across racks first. Rack membership is declared here and pinned with
	// node affinity — it is never derived from where a pod happens to be
	// scheduled.
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
	// (sugar for nodeSelector).
	// +optional
	Zone string `json:"zone,omitempty"`

	// nodeSelector pins the rack's nodes to matching Kubernetes nodes.
	// Merged over zone.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// StorageSpec declares the per-node disks and how their claims are reclaimed.
type StorageSpec struct {
	// disks are this cluster's per-node storage devices: every entry becomes
	// one PersistentVolumeClaim on every node, mounted at
	// /var/lib/fs/disks/<name>. A single-node cluster declares exactly one:
	// fs's filesystem backend stores everything under a single root.
	//
	// Entries may be added, and removed. Removing one is a decommission, not
	// a delete: the disk is drained out of placement on every node, and its
	// volumes go only once fs reports it holds nothing (SPEC §8.5). Sizes may
	// only grow. A disk is identified by its name, so renaming one reads as
	// removing a disk and adding an empty one — which is a slow, safe, and
	// almost certainly unintended way to spend a rebalance.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +required
	Disks []DiskSpec `json:"disks"`

	// state sizes the per-node volume holding fs's node-local state: the
	// object index it keeps beside the disks, at the storage root. Every
	// node gets one; it is not optional, because the container filesystem is
	// read-only and fs writes there at startup.
	// +kubebuilder:default={}
	// +optional
	State StateSpec `json:"state,omitempty"`

	// reclaimPolicy controls what happens to a node's PVCs when the node is
	// removed or the cluster is deleted. It covers the state volume too:
	// Kubernetes sets the policy per StatefulSet, not per claim.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

// StateVolumeName is the name of every node's state claim, and therefore a
// name no disk may take: two claims of one name are one PVC, and the disk
// would be the storage root.
const StateVolumeName = "state"

// StateSpec sizes a node's state volume — the storage root, where fs keeps
// what it derives from the disks rather than the objects themselves.
//
// Since fs v0.13.0 that is the pebble object index: one entry per object the
// node holds, which is what answers a listing, a usage recount or a scrub
// sweep without reading every sidecar on every disk. The index is derived and
// never authoritative — losing it costs a rebuild by walking the disks — but
// on a node holding tens of millions of objects that walk is an event, which
// is why this is a claim and not an emptyDir.
type StateSpec struct {
	// size is the capacity requested for each node's state PVC. Defaults to
	// 10Gi: the index runs to a few hundred bytes per object, so the default
	// carries a node with tens of millions of them. Like a disk it may only
	// grow, and growing it requires the StorageClass to allow expansion.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// storageClass selects the StorageClass for the state PVCs; empty uses
	// the cluster default. The index is written constantly and read on every
	// listing, so this is the one volume of a node worth putting on the
	// fastest class available.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

// ReclaimPolicy controls the fate of data-bearing resources on removal.
type ReclaimPolicy string

// Reclaim policies.
const (
	ReclaimRetain ReclaimPolicy = "Retain"
	ReclaimDelete ReclaimPolicy = "Delete"
)

// DiskSpec is one per-node disk.
type DiskSpec struct {
	// name identifies the disk within each node (the fs disk id). Immutable.
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// size is the capacity requested for each node's PVC of this disk. It
	// may only grow, and growing it requires the StorageClass to allow
	// volume expansion. The growth check is the controller's: expressing it
	// in CEL costs more than the API server's validation budget allows for a
	// list of this size.
	// +required
	Size resource.Quantity `json:"size"`

	// storageClass selects the StorageClass for this disk's PVCs; empty uses
	// the cluster default.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// weight is the disk's relative capacity weight for placement (fs
	// semantics; default 1). Expressed as a quantity so fractional weights
	// like "0.5" are possible. Changing weights rolls the cluster.
	// +optional
	Weight *resource.Quantity `json:"weight,omitempty"`
}

// EtcdSpec configures the etcd control plane connection.
//
// CEL only rules out having both: a clustered topology needs exactly one, a
// single-node cluster needs neither, and which of those applies is a
// cross-field question CEL cannot see from here. internal/validation decides
// it, at admission and again in the controller.
//
// The prefix rule spells out the absent case: a rule that reads self.prefix
// directly fails evaluation — and rejects every update, including status
// writes — while the field is unset, and setting it later would move the
// cluster's keys.
// +kubebuilder:validation:XValidation:rule="has(self.prefix) == has(oldSelf.prefix) && (!has(self.prefix) || self.prefix == oldSelf.prefix)",message="etcd.prefix is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.external) || !has(self.managed)",message="at most one of etcd.external or etcd.managed may be set"
type EtcdSpec struct {
	// external points the cluster at user-operated etcd endpoints. This is
	// the production mode, and the only supported one.
	// +optional
	External *ExternalEtcdSpec `json:"external,omitempty"`

	// managed asks the operator to run a minimal etcd for this cluster.
	//
	// For development and demos only, permanently. It has no backups, no
	// defrag automation, no member replacement and no restore path: losing
	// its volume loses the cluster's control plane, and with it the sealed
	// credentials and the topology. The operator says so on every cluster
	// that uses it (Ready condition message plus an event) — that is not a
	// warning that will be softened once it is "good enough", because etcd
	// lifecycle management is its own discipline and this is not it.
	// Production clusters use external (SPEC §2).
	// +optional
	Managed *ManagedEtcdSpec `json:"managed,omitempty"`

	// prefix namespaces this cluster's keys in etcd. Defaults to
	// /fs/<namespace>/<name>. Immutable.
	// +kubebuilder:validation:MaxLength=250
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// ttl is the node registration lease: how long a dead node lingers in
	// the topology. Zero defers to the fs default (10s); minimum 1s.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// cleanupOnDelete deletes the cluster's keys under prefix when the
	// FSCluster is deleted. Off by default: with a shared etcd, prefer
	// leaving state over destroying a neighbour's.
	// +optional
	CleanupOnDelete bool `json:"cleanupOnDelete,omitempty"`
}

// ExternalEtcdSpec is a user-operated etcd.
type ExternalEtcdSpec struct {
	// endpoints are the etcd client URLs. An "https://" endpoint is served
	// over TLS; fs enables it from the scheme alone, so an https endpoint
	// works without tls below (verifying against the system roots).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +required
	Endpoints []string `json:"endpoints"`

	// tls secures the connection to etcd. etcd holds the node registry and
	// the cluster's credential store, sealed with the cluster secret, so
	// anything that can write to it can reshape the cluster.
	// +optional
	TLS EtcdTLSSpec `json:"tls,omitempty"`

	// authSecretRef references a Secret with keys "username" and "password"
	// for etcd role-based authentication. Both keys are required. The
	// credentials reach the nodes as environment variables, never through
	// the rendered configuration, so they are not written to a config file.
	// +optional
	AuthSecretRef *corev1.LocalObjectReference `json:"authSecretRef,omitempty"`
}

// EtcdTLSSpec is the client TLS material for reaching etcd.
type EtcdTLSSpec struct {
	// secretName names a Secret with the trust material. Key "ca.crt" is the
	// bundle etcd's certificate is verified against; "tls.crt" and "tls.key",
	// when present, are this client's certificate for mutual TLS — a
	// kubernetes.io/tls Secret with a ca.crt added serves both.
	//
	// Setting it turns TLS on. Omit it against an https endpoint to verify
	// with the system roots instead.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// serverName overrides the name verified against etcd's certificate, for
	// reaching it through an address the certificate does not name.
	// +optional
	ServerName string `json:"serverName,omitempty"`

	// insecureSkipVerify disables verification of etcd's certificate. It
	// makes TLS decorative — anything on the path can impersonate the
	// cluster's control plane — and exists for development against
	// self-signed certificates only.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// ManagedEtcdSpec is the operator-run development etcd (SPEC §2). Everything
// here is deliberately small: it exists so `kubectl apply` of an example
// produces a working cluster, not so anyone runs it in production.
type ManagedEtcdSpec struct {
	// replicas is the etcd member count: 1 for a laptop, 3 for a demo that
	// should survive a node restart. Even counts cannot form a quorum and are
	// refused. Immutable, because this etcd has no member-replacement path:
	// growing it would need a join dance the operator does not implement.
	// +kubebuilder:validation:Enum=1;3
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="etcd.managed.replicas is immutable"
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// image is the etcd image. Defaults to the release this operator is
	// tested against.
	// +optional
	Image ManagedEtcdImageSpec `json:"image,omitempty"`

	// storage sizes each member's volume. etcd holds only control-plane
	// state — the node registry, cursors, the sealed key store — so this is
	// kilobytes of data with room for history and compaction.
	// +optional
	Storage ManagedEtcdStorageSpec `json:"storage,omitempty"`

	// resources are the etcd container's resource requirements.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ManagedEtcdImageSpec pins the managed etcd image.
type ManagedEtcdImageSpec struct {
	// repository is the etcd image repository.
	// +kubebuilder:default="quay.io/coreos/etcd"
	// +optional
	Repository string `json:"repository,omitempty"`

	// tag is the etcd image tag.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default="v3.5.17"
	// +optional
	Tag string `json:"tag,omitempty"`

	// pullPolicy is the image pull policy.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// ManagedEtcdStorageSpec sizes the managed etcd's volumes.
type ManagedEtcdStorageSpec struct {
	// size is the capacity requested for each member's PVC.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// storageClass selects the StorageClass; empty uses the cluster default.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// reclaimPolicy decides what happens to the members' volumes when the
	// cluster is deleted. Delete by default, unlike the data disks: this etcd
	// is a development convenience, and leaving its volumes behind means a
	// re-created cluster adopts a key store whose sealed credentials it can no
	// longer open (SPEC §8.6).
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Delete
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

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

// RebalanceSpec tunes the automatic rebalancer; zero values defer to fs
// defaults.
type RebalanceSpec struct {
	// autoDisabled turns automatic rebalancing off; relocation then happens
	// only via periodic scrubs and manual runs.
	// +optional
	AutoDisabled bool `json:"autoDisabled,omitempty"`

	// settle is how long the membership must be stable before data moves
	// (fs default 1m).
	// +optional
	Settle *metav1.Duration `json:"settle,omitempty"`

	// cooldown is the minimum gap between a node's automatic trigger
	// attempts (fs default 15m).
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`

	// fullWatermark is the disk-fullness fraction (0,1] beyond which a node
	// warns that the disk should be drained (fs default 0.9).
	// +optional
	FullWatermark *resource.Quantity `json:"fullWatermark,omitempty"`
}

// IntegritySpec configures object integrity checking.
type IntegritySpec struct {
	// verifyOnRead recomputes and checks each object's checksum before
	// serving it (costs a full extra read per GET).
	// +optional
	VerifyOnRead bool `json:"verifyOnRead,omitempty"`

	// scrubInterval, if positive, runs a background scrubber walking all
	// objects on this cadence. Zero disables it.
	// +optional
	ScrubInterval *metav1.Duration `json:"scrubInterval,omitempty"`

	// scrubQuarantine moves corrupt objects aside instead of only reporting
	// them.
	// +optional
	ScrubQuarantine bool `json:"scrubQuarantine,omitempty"`
}

// UpdatePolicySpec tunes rolling changes.
type UpdatePolicySpec struct {
	// convergenceTimeout bounds how long the operator waits, between node
	// restarts, for the cluster to reconverge (pod ready, node registered,
	// repair queue drained). On timeout the rollout halts — it never
	// proceeds to another node while the cluster is unconverged — and
	// resumes automatically when the gate passes.
	// +kubebuilder:default="30m"
	// +optional
	ConvergenceTimeout *metav1.Duration `json:"convergenceTimeout,omitempty"`

	// schemaMigration selects whether the operator runs `fs cluster migrate`
	// automatically after a successful full rollout (Auto) or only surfaces
	// the pending migration via the SchemaCurrent condition (Manual).
	// +kubebuilder:validation:Enum=Auto;Manual
	// +kubebuilder:default=Auto
	// +optional
	SchemaMigration SchemaMigrationPolicy `json:"schemaMigration,omitempty"`
}

// SchemaMigrationPolicy selects who triggers fs schema migrations.
type SchemaMigrationPolicy string

// Schema migration policies.
const (
	SchemaMigrationAuto   SchemaMigrationPolicy = "Auto"
	SchemaMigrationManual SchemaMigrationPolicy = "Manual"
)

// ObservabilitySpec configures telemetry of the fs pods.
type ObservabilitySpec struct {
	// otlp configures the OpenTelemetry exporter env of the fs pods.
	// +optional
	OTLP OTLPSpec `json:"otlp,omitempty"`

	// logLevel sets the fs log level.
	// +kubebuilder:validation:Enum=debug;info;warn;error
	// +kubebuilder:default=info
	// +optional
	LogLevel string `json:"logLevel,omitempty"`

	// podMonitor creates a PodMonitor for the fs pods' Prometheus metrics
	// (requires the monitoring.coreos.com API group).
	// +optional
	PodMonitor bool `json:"podMonitor,omitempty"`
}

// OTLPSpec is the OTLP exporter destination.
type OTLPSpec struct {
	// endpoint is the OTLP endpoint URL; empty disables the OTLP exporters.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// protocol is the OTLP transport.
	// +kubebuilder:validation:Enum=grpc;http/protobuf
	// +kubebuilder:default=grpc
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

	// registeredNodes is the number of nodes present in the etcd topology.
	// +optional
	RegisteredNodes int32 `json:"registeredNodes,omitempty"`

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

	// schemaVersion reports the fs schema versions in play.
	// +optional
	SchemaVersion *SchemaVersionStatus `json:"schemaVersion,omitempty"`

	// rebalance summarizes rebalance/repair across nodes.
	// +optional
	Rebalance *RebalanceStatus `json:"rebalance,omitempty"`

	// update is present while a rolling change is in flight.
	// +optional
	Update *UpdateStatus `json:"update,omitempty"`

	// endpoints are the cluster's client endpoints.
	// +optional
	Endpoints *EndpointsStatus `json:"endpoints,omitempty"`

	// conditions represent the current state of the FSCluster. See the
	// documented condition types (SpecValid, ReconcileSucceeded, Ready,
	// NodesHealthy, ClusterSizeAligned, ConfigurationInSync, Converged,
	// SchemaCurrent).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SchemaVersionStatus reports the fs schema versions in play.
type SchemaVersionStatus struct {
	// cluster is the schema version recorded in etcd.
	// +optional
	Cluster int64 `json:"cluster,omitempty"`

	// binary is the schema version the deployed image implements.
	// +optional
	Binary int64 `json:"binary,omitempty"`
}

// RebalanceStatus summarizes rebalance/repair across nodes.
type RebalanceStatus struct {
	// state is the worst rebalance state across nodes.
	// +optional
	State string `json:"state,omitempty"`

	// repairQueueDepth is the summed pending repair tasks across nodes.
	// +optional
	RepairQueueDepth int64 `json:"repairQueueDepth,omitempty"`
}

// UpdatePhase is a phase of the rolling-change state machine.
type UpdatePhase string

// Update phases.
const (
	UpdatePhasePreflight    UpdatePhase = "Preflight"
	UpdatePhaseRollingNodes UpdatePhase = "RollingNodes"
	UpdatePhaseMigrating    UpdatePhase = "Migrating"

	// UpdatePhaseDraining is a node being decommissioned: taken out of
	// placement and waiting for the cluster to move its data off, before it is
	// removed (SPEC §8.4).
	UpdatePhaseDraining UpdatePhase = "Draining"
)

// UpdateStatus describes the rolling change in flight.
type UpdateStatus struct {
	// phase is the state-machine phase.
	// +kubebuilder:validation:Enum=Preflight;RollingNodes;Migrating;Draining
	// +optional
	Phase UpdatePhase `json:"phase,omitempty"`

	// node is the node currently being replaced or decommissioned.
	// +optional
	Node string `json:"node,omitempty"`

	// disk is the disk being drained out of the cluster, when the change in
	// flight is a disk removal. A disk is removed from every node at once, so
	// it is not attributable to one node the way a rolling change is.
	// +optional
	Disk string `json:"disk,omitempty"`

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
// +kubebuilder:printcolumn:name="Scheme",type=string,JSONPath=`.spec.scheme`
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
