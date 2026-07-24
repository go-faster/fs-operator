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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	DefaultImageTag = "v0.8.0"

	// DefaultScheme is the default replication scheme.
	DefaultScheme = "rf2.5"

	// DefaultS3Port is the default S3 service port.
	DefaultS3Port int32 = 8080

	// DefaultConvergenceTimeout bounds the per-node reconvergence wait
	// during rolling changes.
	DefaultConvergenceTimeout = 30 * time.Minute

	// DefaultLogLevel is the default fs log level.
	DefaultLogLevel = "info"

	// DefaultOTLPProtocol is the default OTLP transport.
	DefaultOTLPProtocol = "grpc"
)

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

	if s.S3.Service.Type == "" {
		s.S3.Service.Type = corev1.ServiceTypeClusterIP
	}

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
