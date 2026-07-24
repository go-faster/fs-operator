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

// Package fsconfig mirrors the go-faster/fs configuration file schema.
//
// The operator renders one config.yaml per fs node, so these types must stay
// faithful to upstream cmd/fs/config.go (fs v0.5.0): the YAML keys here are
// the keys fs parses, and every field the operator leaves unset falls back to
// the default fs itself applies. Only the subset the operator renders is
// modelled — fs owns everything else.
//
// Secret material is deliberately absent: fs reads the cluster secret from
// FS_CLUSTER_SECRET, the admin token from FS_ADMIN_TOKEN and the root
// credential from FS_ROOT_ACCESS_KEY / FS_ROOT_SECRET_KEY, and the operator
// injects all three as environment variables rather than writing them into a
// file (SPEC §8.1, §9).
package fsconfig

import (
	"bytes"
	"time"

	"github.com/go-faster/errors"
	"gopkg.in/yaml.v3"
)

// StorageTypeCluster is the replicated cluster storage backend — the only
// backend the operator deploys.
const StorageTypeCluster = "cluster"

// Config is one fs node's configuration file.
type Config struct {
	Server        Server        `yaml:"server"`
	Storage       Storage       `yaml:"storage"`
	Auth          Auth          `yaml:"auth"`
	Admin         Admin         `yaml:"admin,omitempty"`
	Cluster       Cluster       `yaml:"cluster,omitempty"`
	Integrity     Integrity     `yaml:"integrity"`
	Observability Observability `yaml:"observability"`
}

// Server is the S3 listener.
type Server struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	HealthPath   string        `yaml:"health_path"`

	// TLS serves HTTPS when both files are set; fs hot-reloads the
	// certificate, so renewals need no restart.
	TLS TLS `yaml:"tls,omitempty"`
}

// TLS is the S3 serving certificate.
type TLS struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

// Storage selects the storage backend and its root directory.
type Storage struct {
	Root string `yaml:"root"`
	Type string `yaml:"type"`

	// Fsync is the durability policy ("none", "file", "file+dir"); empty
	// keeps the fs default ("file").
	Fsync string `yaml:"fsync,omitempty"`
}

// Auth is S3 authentication: the credentials fs accepts and the buckets that
// need none.
type Auth struct {
	Keys              []Key    `yaml:"keys,omitempty"`
	PublicReadBuckets []string `yaml:"public_read_buckets,omitempty"`
}

// Key is one credential and its grants. Config-defined keys are cluster-wide
// and hot-reloadable, unlike the node-local keys the admin API creates at
// runtime — which is why declarative FSAccessKeys are rendered here (SPEC §7).
type Key struct {
	AccessKey string  `yaml:"access_key"`
	SecretKey string  `yaml:"secret_key"`
	Grants    []Grant `yaml:"grants,omitempty"`
}

// Grant authorizes a key for the buckets matching Bucket (a glob) up to
// Permission ("read", "write" or "admin").
type Grant struct {
	Bucket     string `yaml:"bucket"`
	Permission string `yaml:"permission"`
}

// Admin is the admin API listener. Its bearer token comes from the
// environment.
type Admin struct {
	Enabled bool   `yaml:"enabled,omitempty"`
	Addr    string `yaml:"addr,omitempty"`
}

// Cluster is this node's identity, disks and control plane.
type Cluster struct {
	NodeID string `yaml:"node_id"`

	// Rack is the failure-domain label; empty means the node is its own
	// failure domain.
	Rack string `yaml:"rack,omitempty"`

	// Addr is the peer listener bind address.
	Addr string `yaml:"addr,omitempty"`

	// AdvertiseAddr is the host:port peers dial to reach this node.
	AdvertiseAddr string `yaml:"advertise_addr"`

	Scheme string `yaml:"scheme,omitempty"`
	Disks  []Disk `yaml:"disks,omitempty"`
	Etcd   Etcd   `yaml:"etcd"`

	Rebalance Rebalance `yaml:"rebalance,omitempty"`
}

// Disk is one local disk exposed to the cluster.
type Disk struct {
	ID   string `yaml:"id"`
	Path string `yaml:"path"`

	// Weight is the relative capacity weight for placement. fs reads 0 as
	// "unset" and substitutes 1, so a drained disk is expressed with a
	// negative weight — see fscluster.DrainWeight.
	Weight float64 `yaml:"weight,omitempty"`
}

// Etcd is the control-plane connection.
type Etcd struct {
	Endpoints []string      `yaml:"endpoints"`
	Prefix    string        `yaml:"prefix,omitempty"`
	TTL       time.Duration `yaml:"ttl,omitempty"`
}

// Rebalance tunes the automatic rebalancer; zero values keep the fs defaults.
type Rebalance struct {
	AutoDisabled  bool          `yaml:"auto_disabled,omitempty"`
	Settle        time.Duration `yaml:"settle,omitempty"`
	Cooldown      time.Duration `yaml:"cooldown,omitempty"`
	FullWatermark float64       `yaml:"full_watermark,omitempty"`
}

// Integrity configures object integrity checking.
type Integrity struct {
	VerifyOnRead    bool          `yaml:"verify_on_read,omitempty"`
	ScrubInterval   time.Duration `yaml:"scrub_interval,omitempty"`
	ScrubQuarantine bool          `yaml:"scrub_quarantine,omitempty"`
}

// Observability configures telemetry. Exporter destinations are environment
// driven (OTEL_*), so only the switches fs reads from the file live here.
type Observability struct {
	ServiceName          string `yaml:"service_name"`
	EnableRequestLogging bool   `yaml:"enable_request_logging"`
	EnableMetrics        bool   `yaml:"enable_metrics"`
	EnableTracing        bool   `yaml:"enable_tracing"`
}

// yamlIndent keeps rendered configs readable for anyone reading the Secret or
// exec-ing into a pod; yaml.Marshal's default of four spaces is noisy.
const yamlIndent = 2

// Marshal renders the configuration as YAML.
func Marshal(cfg Config) ([]byte, error) {
	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(yamlIndent)

	if err := enc.Encode(cfg); err != nil {
		return nil, errors.Wrap(err, "encode")
	}

	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(err, "close encoder")
	}

	return buf.Bytes(), nil
}

// Unmarshal parses a rendered configuration back. It exists for tests and for
// diffing a node's live configuration against the desired one.
func Unmarshal(data []byte) (Config, error) {
	var cfg Config

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, errors.Wrap(err, "decode")
	}

	return cfg, nil
}

// minEtcdTTL is the shortest registration lease fs accepts.
const minEtcdTTL = time.Second

// Validate applies the checks fs performs on startup (cmd/fs/config.go
// Validate and validateCluster) to the fields the operator renders, so a bad
// render fails in a unit test instead of in a CrashLoopBackOff.
//
// Values fs takes from the environment — the cluster secret and the admin
// token — are out of scope: the operator injects them as environment
// variables, so their absence from the file is expected.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return errors.New("server.addr is required")
	}

	if c.Server.ReadTimeout <= 0 || c.Server.WriteTimeout <= 0 || c.Server.IdleTimeout <= 0 {
		return errors.New("server timeouts must be positive")
	}

	if c.Storage.Root == "" {
		return errors.New("storage.root is required")
	}

	if c.Storage.Type != StorageTypeCluster {
		return errors.Errorf("storage.type must be %q, got %q", StorageTypeCluster, c.Storage.Type)
	}

	if c.Observability.ServiceName == "" {
		return errors.New("observability.service_name is required")
	}

	return c.validateCluster()
}

// validateCluster checks the cluster section.
func (c *Config) validateCluster() error {
	cc := c.Cluster

	if cc.NodeID == "" {
		return errors.New("cluster.node_id is required")
	}

	if cc.AdvertiseAddr == "" {
		return errors.New("cluster.advertise_addr is required (peers must be able to dial this node)")
	}

	if len(cc.Etcd.Endpoints) == 0 {
		return errors.New("cluster.etcd.endpoints is required")
	}

	if cc.Etcd.TTL != 0 && cc.Etcd.TTL < minEtcdTTL {
		return errors.Errorf("cluster.etcd.ttl must be at least %s", minEtcdTTL)
	}

	if cc.Rebalance.Settle < 0 || cc.Rebalance.Cooldown < 0 {
		return errors.New("cluster.rebalance.settle and .cooldown must not be negative")
	}

	if w := cc.Rebalance.FullWatermark; w < 0 || w > 1 {
		return errors.Errorf("cluster.rebalance.full_watermark must be in (0,1], got %v", w)
	}

	if c.Integrity.ScrubInterval < 0 {
		return errors.New("integrity.scrub_interval must not be negative")
	}

	return validateDisks(cc.Disks)
}

// validateDisks checks that every disk is identified, rooted and unique.
func validateDisks(disks []Disk) error {
	seen := make(map[string]struct{}, len(disks))

	for i, d := range disks {
		if d.ID == "" || d.Path == "" {
			return errors.Errorf("cluster.disks[%d]: id and path are required", i)
		}

		if _, dup := seen[d.ID]; dup {
			return errors.Errorf("cluster.disks[%d]: duplicate disk id %q", i, d.ID)
		}

		seen[d.ID] = struct{}{}
	}

	return nil
}
