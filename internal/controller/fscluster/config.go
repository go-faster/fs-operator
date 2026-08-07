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

// Container port names. Services target ports by name, so these are part of
// the objects the operator applies and may not change casually.
const (
	PortNameS3      = "http"
	PortNamePeer    = "peer"
	PortNameAdmin   = "admin"
	PortNameMetrics = "metrics"
	PortNamePprof   = "pprof"
)

// Paths inside the fs container.
const (
	// StorageRoot is fs's storage root. In cluster mode object data lives on
	// the disks below it and the root itself holds node-local state fs can
	// rebuild — the object index of fs v0.13.0 — so it is backed by an
	// emptyDir rather than a claim (see volumes in statefulset.go).
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

	// EtcdTLSDir is where the etcd trust material is mounted. Separate from
	// TLSDir: that one holds the certificate fs *serves*, this one the
	// material it verifies etcd with, and a Secret rotated for one reason
	// should not disturb the other.
	EtcdTLSDir = ConfigDir + "/etcd-tls"

	// Keys of the etcd TLS Secret. ca.crt is the bundle etcd is verified
	// against; tls.crt/tls.key are this client's certificate for mutual TLS,
	// which is the layout of a kubernetes.io/tls Secret with a CA added.
	EtcdCAKey   = "ca.crt"
	EtcdCertKey = "tls.crt"
	EtcdKeyKey  = "tls.key"

	// Keys of the etcd auth Secret.
	EtcdUsernameKey = "username"
	EtcdPasswordKey = "password"

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
	// Drained holds the names of nodes being decommissioned: their disks are
	// rendered at DrainWeight (SPEC §8.4).
	Drained map[string]bool

	// RetainDisks are, per node, disks the spec no longer declares that the
	// node's StatefulSet still carries. They stay mounted and configured until
	// the disk is empty, because fs cannot move data off a volume the pod does
	// not have (SPEC §8.5).
	RetainDisks map[string][]string

	// EtcdClientCert says the referenced etcd TLS Secret carries a client
	// certificate, so the config should name it. Only an API read knows, and
	// naming a file that is not mounted fails the node at startup (§11.4).
	EtcdClientCert bool
}

// RenderedConfig is a node's rendered config.yaml and its revision — the
// opaque marker embedded in the file that fs echoes via the admin API's
// config_revision. The operator reads that value back to confirm the node has
// loaded this config, which is how a hot reload is verified (SPEC §8.3).
type RenderedConfig struct {
	Data     []byte
	Revision string
}

// RenderNodeConfig renders one node's config.yaml and its revision.
//
// The cluster's spec must already be defaulted (FSClusterSpec.WithDefaults):
// the renderer reads spec values as given and never re-applies defaults, so
// that what a node runs is exactly what the object says.
//
// The revision is the fingerprint of the config *without* the marker, so
// embedding the marker cannot change it (no circular hash). The header comment
// is excluded from the fingerprint; the identity it names already lives in the
// config body.
func RenderNodeConfig(cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions) (RenderedConfig, error) {
	cfg, err := nodeConfig(cluster, node, opts)
	if err != nil {
		return RenderedConfig{}, err
	}

	base, err := fsconfig.Marshal(cfg)
	if err != nil {
		return RenderedConfig{}, errors.Wrapf(err, "marshal config of node %q", node.Name)
	}

	cfg.Revision = Revision(base)

	data, err := fsconfig.Marshal(cfg)
	if err != nil {
		return RenderedConfig{}, errors.Wrapf(err, "marshal config of node %q", node.Name)
	}

	return RenderedConfig{
		Data:     append(configHeader(cluster, node), data...),
		Revision: cfg.Revision,
	}, nil
}

// RenderNodeConfigs renders every node's config.yaml, keyed by node name.
func RenderNodeConfigs(cluster *fsv1alpha1.FSCluster, nodes []Node, opts RenderOptions) (map[string]RenderedConfig, error) {
	configs := make(map[string]RenderedConfig, len(nodes))

	for _, node := range nodes {
		rendered, err := RenderNodeConfig(cluster, node, opts)
		if err != nil {
			return nil, err
		}

		configs[node.Name] = rendered
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

	cfg := fsconfig.Config{
		Server: serverConfig(spec),
		Storage: fsconfig.Storage{
			Root: StorageRoot,
			Type: fsconfig.StorageTypeCluster,
		},
		Auth: authConfig(spec),
		Admin: fsconfig.Admin{
			Enabled: true,
			Addr:    listenAddr(AdminPort),
		},
		Cluster:       clusterConfig(cluster, node, opts, opts.Drained[node.Name]),
		Integrity:     integrityConfig(spec),
		Observability: observabilityConfig(cluster),
	}

	if spec.SingleNode() {
		singleNodeConfig(spec, &cfg)
	}

	return cfg, nil
}

// singleNodeConfig turns a node's config into fs's non-clustered form: the
// filesystem backend rooted at the node's one disk, no cluster section, and no
// etcd to hold credentials or a public-read list — so both of those are
// rendered into the file instead, where fs hot-reloads them (SPEC §5.2).
//
// The disk is mounted where it is in cluster mode, so nothing about the pod or
// its claims changes with the backend.
func singleNodeConfig(spec *fsv1alpha1.FSClusterSpec, cfg *fsconfig.Config) {
	cfg.Storage.Root = DiskPath(spec.Storage.Disks[0].Name)
	cfg.Storage.Type = fsconfig.StorageTypeFilesystem
	cfg.Cluster = fsconfig.Cluster{}
	cfg.Auth = fsconfig.Auth{
		Source:            fsconfig.AuthSourceFile,
		PublicReadBuckets: spec.Auth.PublicReadBuckets,
	}
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

// authConfig selects the cluster-wide etcd credential store (fs §6.8): runtime
// credentials live in etcd, sealed by the cluster secret and hot-reloaded on
// every node. The operator manages them through the admin API — the FSAccessKey
// controller for keys, the public-read step for anonymous buckets — so no keys
// or public-read list are rendered into the config. The root credential reaches
// the cluster through the FS_ROOT_* env, which seeds etcd on first boot.
func authConfig(_ *fsv1alpha1.FSClusterSpec) fsconfig.Auth {
	return fsconfig.Auth{Source: fsconfig.AuthSourceEtcd}
}

// etcdTLSConfig renders the client TLS block for reaching etcd.
//
// The file paths are where the referenced Secret is mounted; only the keys the
// Secret actually carries are named, because fs requires cert_file and
// key_file together and pointing at a file that is not there fails the node at
// startup. serverName and insecureSkipVerify apply with or without a Secret,
// so an https endpoint verified against the system roots can still be reached
// through an address its certificate does not name.
func etcdTLSConfig(spec *fsv1alpha1.FSClusterSpec, opts RenderOptions) fsconfig.EtcdTLS {
	external := spec.Etcd.External
	if external == nil {
		// The managed development etcd is plaintext in-cluster (SPEC §2).
		return fsconfig.EtcdTLS{}
	}

	tls := fsconfig.EtcdTLS{
		ServerName:         external.TLS.ServerName,
		InsecureSkipVerify: external.TLS.InsecureSkipVerify,
	}

	if external.TLS.SecretName != "" {
		tls.CAFile = EtcdTLSDir + "/" + EtcdCAKey

		if opts.EtcdClientCert {
			tls.CertFile = EtcdTLSDir + "/" + EtcdCertKey
			tls.KeyFile = EtcdTLSDir + "/" + EtcdKeyKey
		}
	}

	return tls
}

// clusterConfig renders the node's identity, disks and control plane. The
// shared cluster secret is deliberately absent: it is injected through
// FS_CLUSTER_SECRET so it never lands in a rendered file.
func clusterConfig(cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions, drained bool) fsconfig.Cluster {
	spec := &cluster.Spec

	return fsconfig.Cluster{
		NodeID:        node.Name,
		Rack:          node.Rack,
		Addr:          listenAddr(PeerPort),
		AdvertiseAddr: AdvertiseAddr(cluster.Name, cluster.Namespace, node.Name),
		Scheme:        spec.Scheme,
		Disks:         diskConfigs(spec, opts.RetainDisks[node.Name], drained),
		Etcd: fsconfig.Etcd{
			Endpoints: EtcdEndpoints(cluster),
			Prefix:    spec.EtcdPrefix(cluster.Namespace, cluster.Name),
			TTL:       duration(spec.Etcd.TTL),
			TLS:       etcdTLSConfig(spec, opts),
			// Auth is deliberately absent: the credentials reach fs through
			// FS_ETCD_USERNAME / FS_ETCD_PASSWORD, so an etcd password is
			// never written into a rendered config (SPEC §9).
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
func diskConfigs(spec *fsv1alpha1.FSClusterSpec, retained []string, drained bool) []fsconfig.Disk {
	disks := make([]fsconfig.Disk, 0, len(spec.Storage.Disks)+len(retained))

	for _, disk := range spec.Storage.Disks {
		disks = append(disks, fsconfig.Disk{
			ID:     disk.Name,
			Path:   DiskPath(disk.Name),
			Weight: diskWeight(disk, drained),
		})
	}

	// A disk being removed stays in the config until its data has moved off.
	// Dropping it here would be worse than dropping the mount: fs would not
	// know the disk exists, so it could neither serve what is on it nor move
	// it — the data would simply be stranded on a volume nobody reads.
	//
	// It is registered at DrainWeight so it takes no new data even before the
	// control-plane override lands (SPEC §8.5).
	for _, name := range retained {
		disks = append(disks, fsconfig.Disk{
			ID:     name,
			Path:   DiskPath(name),
			Weight: DrainWeight,
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

// Revision prefixes distinguish a configuration revision from a pod-template
// revision at a glance in status and events.
const (
	revisionPrefix = "cfg-"
	templatePrefix = "sts-"
)

// revisionDigits is how much of the digest a revision carries: enough to make
// a collision between the handful of revisions a cluster sees implausible,
// short enough to read in a status field.
const revisionDigits = 12

// ConfigRevision is the fingerprint of a set of rendered configs
// (status.configurationRevision). It is stable across reconciles and changes
// whenever any node's configuration does, which is what the rolling and
// hot-reload machinery keys off (SPEC §8.2, §8.3).
func ConfigRevision(configs map[string]RenderedConfig) string {
	digest := sha256.New()

	for _, name := range slices.Sorted(maps.Keys(configs)) {
		data := configs[name].Data
		// Length-prefix both halves so that no pair of distinct config sets
		// can hash alike by shifting bytes between node name and content.
		// hash.Hash never fails, hence the discarded errors.
		_, _ = fmt.Fprintf(digest, "%d:%s%d:", len(name), name, len(data))
		_, _ = digest.Write(data)
	}

	return format(revisionPrefix, digest.Sum(nil))
}

// Revision is the fingerprint of one node's rendered configuration.
func Revision(config []byte) string {
	digest := sha256.Sum256(config)

	return format(revisionPrefix, digest[:])
}

// RestartRevision fingerprints the configuration a node can only pick up by
// restarting — everything except the credentials and certificate fs reloads on
// SIGHUP. It rides on the pod template, so a change to it replaces the pod,
// while a change to the rest is a reload (SPEC §8.2, §8.3).
func RestartRevision(cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions) (string, error) {
	cfg, err := nodeConfig(cluster, node, opts)
	if err != nil {
		return "", err
	}

	// Everything fs re-reads on SIGHUP: the credentials, their grants and the
	// anonymously readable buckets. The certificate is reloaded from the same
	// paths, which is why the paths themselves stay in the fingerprint —
	// turning TLS on or off does need a restart.
	cfg.Auth = fsconfig.Auth{}

	// The revision marker moves with every config change, hot-reloadable ones
	// included; excluding it keeps a credential-only change off the restart
	// path (SPEC §8.3).
	cfg.Revision = ""

	data, err := fsconfig.Marshal(cfg)
	if err != nil {
		return "", errors.Wrapf(err, "marshal config of node %q", node.Name)
	}

	return Revision(data), nil
}

// format renders a digest as a revision.
func format(prefix string, digest []byte) string {
	return prefix + hex.EncodeToString(digest)[:revisionDigits]
}
