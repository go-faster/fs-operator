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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Static defaults. Where a value also carries a +kubebuilder:default marker
// the two must agree — the marker keeps the CRD schema honest, WithDefaults
// keeps controller logic independent of which path an object arrived
// through.
const (
	// DefaultImageRepository is the default fs image repository.
	DefaultImageRepository = "ghcr.io/go-faster/fs"

	// DefaultImageTag is the pinned fs release this operator version is
	// validated against; used when spec.image.tag is empty.
	DefaultImageTag = "v0.13.0"

	// DefaultScheme is the default replication scheme.
	DefaultScheme = "rf2.5"

	// DefaultStateSize is each node's state volume: the storage root, whose
	// size is the object index living on it. A few hundred bytes per object
	// puts tens of millions of them inside this, and a node past that is one
	// whose operator is already sizing volumes deliberately.
	DefaultStateSize = "10Gi"

	// DefaultS3Port is the default S3 service port.
	DefaultS3Port int32 = 8080

	// DefaultManagedEtcdRepository and DefaultManagedEtcdTag pin the etcd the
	// operator runs in managed (development) mode.
	DefaultManagedEtcdRepository = "quay.io/coreos/etcd"
	DefaultManagedEtcdTag        = "v3.5.17"

	// DefaultManagedEtcdReplicas is a single member: the managed mode is for
	// development, where one is enough and three is a choice.
	DefaultManagedEtcdReplicas int32 = 1

	// DefaultManagedEtcdSize is each member's volume. etcd holds only
	// control-plane state, so this is room for history and compaction rather
	// than for data.
	DefaultManagedEtcdSize = "2Gi"

	// EtcdClientPort and EtcdPeerPort are etcd's well-known ports.
	EtcdClientPort int32 = 2379
	EtcdPeerPort   int32 = 2380

	// DefaultConvergenceTimeout bounds the per-node reconvergence wait
	// during rolling changes.
	DefaultConvergenceTimeout = 30 * time.Minute

	// DefaultLogLevel is the default fs log level.
	DefaultLogLevel = "info"

	// DefaultOTLPProtocol is the default OTLP transport.
	DefaultOTLPProtocol = "grpc"
)

// ApplyRegistry rewrites the registry host of an image repository to registry,
// for air-gapped or mirrored deployments where every image must resolve from a
// private mirror. It replaces the leading host segment — the part before the
// first "/", when it looks like a registry host (it contains a "." or ":") —
// with registry; when the repository carries no host segment it prepends
// registry. An empty registry (the default) leaves the repository untouched.
//
//	ApplyRegistry("ghcr.io/go-faster/fs", "registry.internal")
//	  -> "registry.internal/go-faster/fs"
//
// The chart's fs-operator.applyRegistry template mirrors this logic; keep the
// two in step.
func ApplyRegistry(repository, registry string) string {
	if registry == "" || repository == "" {
		return repository
	}

	registry = strings.TrimSuffix(registry, "/")

	if host, rest, ok := strings.Cut(repository, "/"); ok && strings.ContainsAny(host, ".:") {
		return registry + "/" + rest
	}

	return registry + "/" + repository
}

// EtcdPrefix returns the effective etcd prefix for a cluster in a namespace:
// the spec value, or the default /fs/<namespace>/<name>.
func (s *FSClusterSpec) EtcdPrefix(namespace, name string) string {
	if s.Etcd.Prefix != "" {
		return s.Etcd.Prefix
	}

	return fmt.Sprintf("/fs/%s/%s", namespace, name)
}

// WithDefaults fills zero values with static defaults, mirroring the CRD
// schema defaults so controller logic never depends on the admission path.
func (s *FSClusterSpec) WithDefaults() {
	if s.Image.Repository == "" {
		s.Image.Repository = DefaultImageRepository
	}

	if s.Image.Tag == "" {
		s.Image.Tag = DefaultImageTag
	}

	if s.Image.PullPolicy == "" {
		s.Image.PullPolicy = corev1.PullIfNotPresent
	}

	if s.Scheme == "" {
		s.Scheme = DefaultScheme
	}

	if s.Topology.PodAntiAffinity == "" {
		s.Topology.PodAntiAffinity = AntiAffinityRequired
	}

	if s.Storage.ReclaimPolicy == "" {
		s.Storage.ReclaimPolicy = ReclaimRetain
	}

	if s.Storage.State.Size == nil {
		size := resource.MustParse(DefaultStateSize)
		s.Storage.State.Size = &size
	}

	if s.S3.Service.Type == "" {
		s.S3.Service.Type = corev1.ServiceTypeClusterIP
	}

	s.Etcd.withDefaults()

	if s.S3.Service.Port == 0 {
		s.S3.Service.Port = DefaultS3Port
	}

	if s.UpdatePolicy.ConvergenceTimeout == nil {
		s.UpdatePolicy.ConvergenceTimeout = &metav1.Duration{Duration: DefaultConvergenceTimeout}
	}

	if s.UpdatePolicy.SchemaMigration == "" {
		s.UpdatePolicy.SchemaMigration = SchemaMigrationAuto
	}

	if s.Observability.LogLevel == "" {
		s.Observability.LogLevel = DefaultLogLevel
	}

	if s.Observability.OTLP.Protocol == "" {
		s.Observability.OTLP.Protocol = DefaultOTLPProtocol
	}

	if s.Observability.Pprof == nil {
		s.Observability.Pprof = ptr.To(true)
	}
}

// NodeName is a node's name — its fs node_id, the name of its StatefulSet and
// a DNS label: <cluster>-<rack>-<index>, or <cluster>-<index> in the flat
// topology (empty rack).
func NodeName(cluster, rack string, index int) string {
	if rack == "" {
		return fmt.Sprintf("%s-%d", cluster, index)
	}

	return fmt.Sprintf("%s-%s-%d", cluster, rack, index)
}

// NodeNames expands the topology into the ordered node (and StatefulSet)
// names.
func (s *FSClusterSpec) NodeNames(cluster string) []string {
	names := make([]string, 0, s.TotalNodes())

	if s.Topology.Nodes != nil {
		for i := range int(*s.Topology.Nodes) {
			names = append(names, NodeName(cluster, "", i))
		}

		return names
	}

	for _, rack := range s.Topology.Racks {
		for i := range int(rack.Nodes) {
			names = append(names, NodeName(cluster, rack.Name, i))
		}
	}

	return names
}

// TotalNodes is the declared node count across the topology.
func (s *FSClusterSpec) TotalNodes() int32 {
	if s.Topology.Nodes != nil {
		return *s.Topology.Nodes
	}

	var total int32
	for _, rack := range s.Topology.Racks {
		total += rack.Nodes
	}

	return total
}

// SingleNode reports whether this is a single-node cluster: one fs node
// running the non-clustered filesystem backend, with no etcd, no peers and no
// replication (SPEC §5.2). It is the development shape, and almost every
// cluster-mode contract — placement, quorum, rebalance, schema migration —
// simply does not apply to it.
func (s *FSClusterSpec) SingleNode() bool {
	return s.TotalNodes() == 1
}

// FailureDomains is the number of distinct failure domains the topology
// provides: the rack count, or the node count for the flat topology (each
// node is its own domain).
func (s *FSClusterSpec) FailureDomains() int32 {
	if s.Topology.Nodes != nil {
		return *s.Topology.Nodes
	}

	return int32(len(s.Topology.Racks))
}

// WithDefaults fills zero values with static defaults.
func (s *FSBucketSpec) WithDefaults(name string) {
	if s.BucketName == "" {
		s.BucketName = name
	}

	if s.ReclaimPolicy == "" {
		s.ReclaimPolicy = ReclaimRetain
	}
}

// CredentialsSecretName is the effective name of the operator-owned Secret
// for a generated credential; empty when the credential is imported.
func (s *FSAccessKeySpec) CredentialsSecretName(name string) string {
	if s.ExistingSecretRef != nil {
		return ""
	}

	if s.SecretName != "" {
		return s.SecretName
	}

	return name + "-credentials"
}

// withDefaults fills in the managed etcd's optional fields. External etcd has
// nothing to default: its endpoints are the whole of it.
func (e *EtcdSpec) withDefaults() {
	if e.Managed == nil {
		return
	}

	if e.Managed.Replicas == nil {
		replicas := DefaultManagedEtcdReplicas
		e.Managed.Replicas = &replicas
	}

	if e.Managed.Image.Repository == "" {
		e.Managed.Image.Repository = DefaultManagedEtcdRepository
	}

	if e.Managed.Image.Tag == "" {
		e.Managed.Image.Tag = DefaultManagedEtcdTag
	}

	if e.Managed.Image.PullPolicy == "" {
		e.Managed.Image.PullPolicy = corev1.PullIfNotPresent
	}

	if e.Managed.Storage.Size == nil {
		size := resource.MustParse(DefaultManagedEtcdSize)
		e.Managed.Storage.Size = &size
	}

	if e.Managed.Storage.ReclaimPolicy == "" {
		e.Managed.Storage.ReclaimPolicy = ReclaimDelete
	}
}

// ManagedEtcd reports whether the operator runs this cluster's etcd.
func (s *FSClusterSpec) ManagedEtcd() bool {
	return s.Etcd.Managed != nil
}

// EtcdReplicas is the managed etcd's member count, zero when etcd is external.
func (s *FSClusterSpec) EtcdReplicas() int32 {
	if s.Etcd.Managed == nil || s.Etcd.Managed.Replicas == nil {
		return 0
	}

	return *s.Etcd.Managed.Replicas
}
