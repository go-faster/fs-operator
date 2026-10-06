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
// faithful to upstream cmd/fs/config.go (fs v0.14.0): the YAML keys here are
// the keys fs parses, and every field the operator leaves unset falls back to
// the default fs itself applies. Only the subset the operator renders is
// modelled — fs owns everything else. fs refuses a key it does not know, so a
// field here that fs has dropped stops every node at startup.
//
// The cluster secret, the admin token and the root credential are absent: fs
// reads them from FS_CLUSTER_SECRET, FS_ADMIN_TOKEN and FS_ROOT_ACCESS_KEY /
// FS_ROOT_SECRET_KEY, and the operator injects them as environment variables
// (SPEC §8.1, §9). FSAccessKey credentials are rendered here: they are what
// a reload applies on every node at once (SPEC §7).
package fsconfig

import (
	"bytes"
	"time"

	"github.com/go-faster/errors"
	"gopkg.in/yaml.v3"
)

// Config is one fs node's configuration file.
type Config struct {
	Server        Server        `yaml:"server"`
	Storage       Storage       `yaml:"storage"`
	Auth          Auth          `yaml:"auth"`
	Admin         Admin         `yaml:"admin,omitempty"`
	Cluster       Cluster       `yaml:"cluster,omitempty"`
	Observability Observability `yaml:"observability"`

	// Revision is an opaque marker fs echoes back via the admin API
	// (InstanceInfo.config_revision and the reload result) and never acts on.
	// The operator sets it to a node's configuration revision and reads it
	// back to confirm the node has loaded that config — the reload
	// verification of SPEC §8.3.
	Revision string `yaml:"revision,omitempty"`
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

// Storage is the node's storage root.
type Storage struct {
	Root string `yaml:"root"`

	// Fsync is the durability policy ("file" or "none"); empty keeps the fs
	// default ("file").
	Fsync string `yaml:"fsync,omitempty"`
}

// Auth is S3 authentication: the credentials fs accepts and the buckets that
// need none. Both are hot-reloaded.
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

// Cluster is this node's identity and how it finds its peers. Setting NodeID
// is what turns cluster mode on; a single node renders none of it.
type Cluster struct {
	NodeID string `yaml:"node_id"`

	// Addr is the peer listener bind address.
	Addr string `yaml:"addr,omitempty"`

	// AdvertiseAddr is the host:port peers dial to reach this node.
	AdvertiseAddr string `yaml:"advertise_addr"`

	// Peers are host:port addresses the node joins through at startup; one
	// that answers is enough, the rest are learned by gossip.
	Peers []string `yaml:"peers,omitempty"`
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

// Unmarshal parses a rendered configuration back, refusing unknown keys the
// way fs does. It exists for tests and for diffing a node's live
// configuration against the desired one.
func Unmarshal(data []byte) (Config, error) {
	var cfg Config

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil {
		return Config{}, errors.Wrap(err, "decode")
	}

	return cfg, nil
}

// Validate applies the checks fs performs on startup (cmd/fs/config.go) to
// the fields the operator renders, so a bad render fails in a unit test
// instead of in a CrashLoopBackOff.
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

	if c.Observability.ServiceName == "" {
		return errors.New("observability.service_name is required")
	}

	if c.Cluster.NodeID == "" {
		if c.Cluster.AdvertiseAddr != "" || len(c.Cluster.Peers) > 0 {
			return errors.New("cluster settings without cluster.node_id")
		}

		return nil
	}

	if c.Cluster.AdvertiseAddr == "" {
		return errors.New("cluster.advertise_addr is required (peers must be able to dial this node)")
	}

	return nil
}
