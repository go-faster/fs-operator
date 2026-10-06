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
	// StorageRoot is fs's storage root, where the node's data volume is
	// mounted: metadata, blocks and runtime state all live below it.
	StorageRoot = "/var/lib/fs"

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

// debugLogLevel is the only log level at which the operator turns on fs's
// per-request logging: on a busy S3 endpoint one line per request is a cost,
// not an insight. Every other level go-faster/sdk accepts — up to fatal —
// leaves it off (SPEC §5, observability.logLevel).
const debugLogLevel = "debug"

// RenderOptions carries the inputs a rendered config needs beyond the
// FSCluster itself.
type RenderOptions struct {
	// Keys are the cluster's FSAccessKey credentials, rendered into every
	// node so that each accepts them and a reload applies a change everywhere
	// (SPEC §7).
	Keys []fsconfig.Key
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
		Server:  serverConfig(spec),
		Storage: fsconfig.Storage{Root: StorageRoot},
		Auth: fsconfig.Auth{
			Keys:              opts.Keys,
			PublicReadBuckets: spec.Auth.PublicReadBuckets,
		},
		Admin: fsconfig.Admin{
			Enabled: true,
			Addr:    listenAddr(AdminPort),
		},
		Observability: observabilityConfig(cluster),
	}

	// A single node runs with no cluster section at all: no peers to find
	// and no layout to join (SPEC §5.2).
	if !spec.SingleNode() {
		cfg.Cluster = clusterConfig(cluster, node)
	}

	return cfg, nil
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

// clusterConfig renders the node's identity and the peers it joins through.
// The shared cluster secret is deliberately absent: it is injected through
// FS_CLUSTER_SECRET so it never lands in a rendered file.
//
// Every other declared node is a peer. One that answers is enough to join;
// listing them all is what lets a node restarted while some of the others
// are down still find the cluster.
func clusterConfig(cluster *fsv1alpha1.FSCluster, node Node) fsconfig.Cluster {
	var peers []string

	for _, name := range cluster.Spec.NodeNames(cluster.Name) {
		if name != node.Name {
			peers = append(peers, AdvertiseAddr(cluster.Name, cluster.Namespace, name))
		}
	}

	return fsconfig.Cluster{
		NodeID:        node.Name,
		Addr:          listenAddr(PeerPort),
		AdvertiseAddr: AdvertiseAddr(cluster.Name, cluster.Namespace, node.Name),
		Peers:         peers,
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
// restarting — everything except what a reload re-reads and what fs reads only
// once, at startup. It rides on the pod template, so a change to it replaces
// the pod, while a change to the rest is a reload (SPEC §8.2, §8.3).
func RestartRevision(cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions) (string, error) {
	cfg, err := nodeConfig(cluster, node, opts)
	if err != nil {
		return "", err
	}

	// Everything a reload re-reads: the credentials, their grants and the
	// anonymously readable buckets. The certificate is reloaded from the same
	// paths, which is why the paths themselves stay in the fingerprint —
	// turning TLS on or off does need a restart.
	cfg.Auth = fsconfig.Auth{}

	// The peers are only where a starting node looks for the cluster; once
	// it has joined, gossip keeps the membership. Scaling the cluster changes
	// every node's list, and restarting every node for it would buy nothing.
	cfg.Cluster.Peers = nil

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
