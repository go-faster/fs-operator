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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/fsconfig"
)

// Container ports. They are fixed: the Services map the user-facing port onto
// them, so nothing about the pod's own listeners depends on the spec.
const (
	// S3Port is the S3 listener, which also serves /health and /ready.
	S3Port int32 = 8080

	// PeerPort is the cluster (peer replication) listener.
	PeerPort int32 = 7080

	// AdminPort is the admin API listener.
	AdminPort int32 = 8090

	// MetricsPort is the Prometheus exporter.
	MetricsPort int32 = 9464

	// PprofPort is the pprof listener.
	PprofPort int32 = 9010
)

// Paths inside the fs container.
const (
	// StorageRoot is fs's storage root. In cluster mode object data lives on
	// the disks below it and the root itself only holds node-local state, so
	// it is backed by an emptyDir rather than a claim.
	StorageRoot = "/var/lib/fs"

	// DisksDir holds one mounted claim per disk: <DisksDir>/<disk name>.
	DisksDir = StorageRoot + "/disks"

	// ConfigDir is where the node's config Secret is mounted.
	ConfigDir = "/etc/fs"

	// ConfigFileName is the key in the config Secret and the file name below
	// ConfigDir.
	ConfigFileName = "config.yaml"

	// ConfigPath is the config file fs is started with.
	ConfigPath = ConfigDir + "/" + ConfigFileName

	// TLSDir is where the S3 serving certificate Secret is mounted. The keys
	// are the kubernetes.io/tls ones.
	TLSDir = ConfigDir + "/tls"

	// TLSCertPath and TLSKeyPath are the mounted certificate and key.
	TLSCertPath = TLSDir + "/tls.crt"
	TLSKeyPath  = TLSDir + "/tls.key"
)

// Server timeouts rendered into every node's config. They follow the values
// go-faster/fs recommends for production (config.production.yaml) rather than
// the tighter library defaults, which are tuned for single-node development:
// S3 clients push large objects over slow links.
const (
	serverReadTimeout  = 60 * time.Second
	serverWriteTimeout = 120 * time.Second
	serverIdleTimeout  = 300 * time.Second
)

// healthPath is fs's liveness endpoint; readiness is served at /ready next to
// it and is not configurable.
const healthPath = "/health"

// DefaultDiskWeight is the placement weight of a disk whose spec leaves the
// weight unset.
const DefaultDiskWeight = 1.0

// DrainWeight takes a disk out of placement so the auto-rebalancer moves its
// data elsewhere (SPEC §8.4).
//
// It is negative on purpose. fs's config layer reads weight 0 as "unset" and
// substitutes 1 (cmd/fs/cluster.go), while placement skips every disk whose
// weight is not positive (internal/cluster/placement). A negative weight is
// therefore the only way to express a drained disk through the config file;
// fs §11.6 (a persisted drain flag) would replace this with an API call.
const DrainWeight = -1.0

// debugLogLevel is the only log level at which the operator turns on fs's
// per-request logging: on a busy S3 endpoint one line per request is a cost,
// not an insight.
const debugLogLevel = "debug"

// RenderOptions carries the inputs a rendered config needs beyond the
// FSCluster itself.
type RenderOptions struct {
	// Keys are the declarative S3 credentials merged from the cluster's
	// FSAccessKeys. They are rendered into every node's config, so that a
	// credential is cluster-wide and survives restarts (SPEC §7).
	Keys []fsconfig.Key

	// Drained holds the names of nodes being decommissioned: their disks are
	// rendered at DrainWeight (SPEC §8.4).
	Drained map[string]bool
}

// RenderNodeConfig renders one node's config.yaml.
//
// The cluster's spec must already be defaulted (FSClusterSpec.WithDefaults):
// the renderer reads spec values as given and never re-applies defaults, so
// that what a node runs is exactly what the object says.
func RenderNodeConfig(cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions) ([]byte, error) {
	cfg, err := nodeConfig(cluster, node, opts)
	if err != nil {
		return nil, err
	}

	data, err := fsconfig.Marshal(cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "marshal config of node %q", node.Name)
	}

	return append(configHeader(cluster, node), data...), nil
}

// RenderNodeConfigs renders every node's config.yaml, keyed by node name.
func RenderNodeConfigs(cluster *fsv1alpha1.FSCluster, nodes []Node, opts RenderOptions) (map[string][]byte, error) {
	configs := make(map[string][]byte, len(nodes))

	for _, node := range nodes {
		data, err := RenderNodeConfig(cluster, node, opts)
		if err != nil {
			return nil, err
		}

		configs[node.Name] = data
	}

	return configs, nil
}

// configHeader labels the rendered file for whoever reads the Secret or execs
// into a pod. It is part of the config bytes, so it also participates in the
// configuration revision — which is correct: a node moving between racks must
// look like a configuration change.
func configHeader(cluster *fsv1alpha1.FSCluster, node Node) []byte {
	return fmt.Appendf(nil,
		"# Rendered by fs-operator for node %s of FSCluster %s/%s. Do not edit.\n",
		node.Name, cluster.Namespace, cluster.Name)
}

// nodeConfig assembles one node's configuration from the cluster spec.
func nodeConfig(cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions) (fsconfig.Config, error) {
	if cluster.Name == "" || cluster.Namespace == "" {
		return fsconfig.Config{}, errors.New("cluster name and namespace are required")
	}

	if node.Name == "" {
		return fsconfig.Config{}, errors.New("node name is required")
	}

	spec := &cluster.Spec

	return fsconfig.Config{
		Server: serverConfig(spec),
		Storage: fsconfig.Storage{
			Root: StorageRoot,
			Type: fsconfig.StorageTypeCluster,
		},
		Auth: authConfig(spec, opts.Keys),
		Admin: fsconfig.Admin{
			Enabled: true,
			Addr:    listenAddr(AdminPort),
		},
		Cluster:       clusterConfig(cluster, node, opts.Drained[node.Name]),
		Integrity:     integrityConfig(spec),
		Observability: observabilityConfig(cluster),
	}, nil
}

// serverConfig renders the S3 listener, terminating TLS in fs itself when the
// spec points at a certificate.
func serverConfig(spec *fsv1alpha1.FSClusterSpec) fsconfig.Server {
	server := fsconfig.Server{
		Addr:         listenAddr(S3Port),
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
		IdleTimeout:  serverIdleTimeout,
		HealthPath:   healthPath,
	}

	if spec.S3.TLS.SecretName != "" {
		server.TLS = fsconfig.TLS{
			CertFile: TLSCertPath,
			KeyFile:  TLSKeyPath,
		}
	}

	return server
}

// authConfig renders the declarative credentials. Keys are sorted so that the
// same set always renders the same bytes — the configuration revision must not
// change because a map was iterated in a different order.
func authConfig(spec *fsv1alpha1.FSClusterSpec, keys []fsconfig.Key) fsconfig.Auth {
	sorted := slices.Clone(keys)
	slices.SortFunc(sorted, func(a, b fsconfig.Key) int {
		return strings.Compare(a.AccessKey, b.AccessKey)
	})

	return fsconfig.Auth{
		Keys:              sorted,
		PublicReadBuckets: spec.Auth.PublicReadBuckets,
	}
}

// clusterConfig renders the node's identity, disks and control plane. The
// shared cluster secret is deliberately absent: it is injected through
// FS_CLUSTER_SECRET so it never lands in a rendered file.
func clusterConfig(cluster *fsv1alpha1.FSCluster, node Node, drained bool) fsconfig.Cluster {
	spec := &cluster.Spec

	return fsconfig.Cluster{
		NodeID:        node.Name,
		Rack:          node.Rack,
		Addr:          listenAddr(PeerPort),
		AdvertiseAddr: AdvertiseAddr(cluster.Name, cluster.Namespace, node.Name),
		Scheme:        spec.Scheme,
		Disks:         diskConfigs(spec, drained),
		Etcd: fsconfig.Etcd{
			Endpoints: spec.Etcd.External.Endpoints,
			Prefix:    spec.EtcdPrefix(cluster.Namespace, cluster.Name),
			TTL:       duration(spec.Etcd.TTL),
		},
		Rebalance: fsconfig.Rebalance{
			AutoDisabled:  spec.Rebalance.AutoDisabled,
			Settle:        duration(spec.Rebalance.Settle),
			Cooldown:      duration(spec.Rebalance.Cooldown),
			FullWatermark: quantity(spec.Rebalance.FullWatermark),
		},
	}
}

// diskConfigs renders this node's disks: one claim per spec entry, mounted
// below DisksDir under its own name.
func diskConfigs(spec *fsv1alpha1.FSClusterSpec, drained bool) []fsconfig.Disk {
	disks := make([]fsconfig.Disk, 0, len(spec.Storage.Disks))

	for _, disk := range spec.Storage.Disks {
		disks = append(disks, fsconfig.Disk{
			ID:     disk.Name,
			Path:   DiskPath(disk.Name),
			Weight: diskWeight(disk, drained),
		})
	}

	return disks
}

// DiskPath is where a disk's claim is mounted inside the container.
func DiskPath(disk string) string {
	return DisksDir + "/" + disk
}

// diskWeight resolves a disk's placement weight. A node being drained, and a
// disk the spec weights at zero or less, both render as DrainWeight — fs's
// documented "weight 0 drains the disk" semantics, expressed the way its
// config layer actually reads (see DrainWeight).
func diskWeight(disk fsv1alpha1.DiskSpec, drained bool) float64 {
	if drained {
		return DrainWeight
	}

	if disk.Weight == nil {
		return DefaultDiskWeight
	}

	if weight := disk.Weight.AsApproximateFloat64(); weight > 0 {
		return weight
	}

	return DrainWeight
}

// integrityConfig renders the scrub and read-verification passthrough.
func integrityConfig(spec *fsv1alpha1.FSClusterSpec) fsconfig.Integrity {
	return fsconfig.Integrity{
		VerifyOnRead:    spec.Integrity.VerifyOnRead,
		ScrubInterval:   duration(spec.Integrity.ScrubInterval),
		ScrubQuarantine: spec.Integrity.ScrubQuarantine,
	}
}

// observabilityConfig renders the telemetry switches fs reads from the file.
// Exporter destinations and the log level travel as OTEL environment
// variables, so they are the StatefulSet builder's business, not this file's.
func observabilityConfig(cluster *fsv1alpha1.FSCluster) fsconfig.Observability {
	obs := &cluster.Spec.Observability

	return fsconfig.Observability{
		// The cluster is the service: telemetry from all its nodes shares a
		// service name and is told apart by resource attributes.
		ServiceName:          cluster.Name,
		EnableRequestLogging: obs.LogLevel == debugLogLevel,
		EnableMetrics:        true,
		EnableTracing:        obs.OTLP.Endpoint != "",
	}
}

// listenAddr binds a port on every interface of the pod network.
func listenAddr(port int32) string {
	return fmt.Sprintf(":%d", port)
}

// duration unwraps an optional duration; the zero value leaves fs's own
// default in place.
func duration(d *metav1.Duration) time.Duration {
	if d == nil {
		return 0
	}

	return d.Duration
}

// quantity unwraps an optional quantity as a float, which is how fs expresses
// fractional tuning values.
func quantity(q *resource.Quantity) float64 {
	if q == nil {
		return 0
	}

	return q.AsApproximateFloat64()
}

// revisionPrefix distinguishes a configuration revision from the pod-template
// revision at a glance in status and events.
const revisionPrefix = "cfg-"

// revisionDigits is how much of the digest a revision carries: enough to make
// a collision between the handful of revisions a cluster sees implausible,
// short enough to read in a status field.
const revisionDigits = 12

// ConfigRevision is the fingerprint of a set of rendered configs
// (status.configurationRevision). It is stable across reconciles and changes
// whenever any node's configuration does, which is what the rolling and
// hot-reload machinery keys off (SPEC §8.2, §8.3).
func ConfigRevision(configs map[string][]byte) string {
	digest := sha256.New()

	for _, name := range slices.Sorted(maps.Keys(configs)) {
		// Length-prefix both halves so that no pair of distinct config sets
		// can hash alike by shifting bytes between node name and content.
		// hash.Hash never fails, hence the discarded errors.
		_, _ = fmt.Fprintf(digest, "%d:%s%d:", len(name), name, len(configs[name]))
		_, _ = digest.Write(configs[name])
	}

	return revisionPrefix + hex.EncodeToString(digest.Sum(nil))[:revisionDigits]
}
