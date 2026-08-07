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

package fsconfig

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// valid is a configuration of the shape the operator renders.
func valid() Config {
	return Config{
		Server: Server{
			Addr:         ":8080",
			ReadTimeout:  60 * time.Second,
			WriteTimeout: 120 * time.Second,
			IdleTimeout:  300 * time.Second,
			HealthPath:   "/health",
			TLS:          TLS{CertFile: "/etc/fs/tls/tls.crt", KeyFile: "/etc/fs/tls/tls.key"},
		},
		Storage: Storage{Root: "/var/lib/fs", Type: StorageTypeCluster},
		Auth: Auth{
			Keys: []Key{{
				AccessKey: "AKmedia",
				SecretKey: "media-secret-key-0123456789",
				Grants:    []Grant{{Bucket: "media-*", Permission: "write"}},
			}},
			PublicReadBuckets: []string{"public"},
		},
		Admin: Admin{Enabled: true, Addr: ":8090"},
		Cluster: Cluster{
			NodeID:        "prod-a-0",
			Rack:          "a",
			Addr:          ":7080",
			AdvertiseAddr: "prod-a-0-0.prod-peers.tenant-a.svc:7080",
			Scheme:        "ec:4,2",
			Disks: []Disk{
				{ID: "d0", Path: "/var/lib/fs/disks/d0", Weight: 1},
				{ID: "d1", Path: "/var/lib/fs/disks/d1", Weight: 0.5},
			},
			Etcd:      Etcd{Endpoints: []string{"http://etcd:2379"}, Prefix: "/fs/prod", TTL: 15 * time.Second},
			Rebalance: Rebalance{Settle: time.Minute, Cooldown: 15 * time.Minute, FullWatermark: 0.9},
		},
		Integrity:     Integrity{VerifyOnRead: true, ScrubInterval: 24 * time.Hour},
		Observability: Observability{ServiceName: "prod", EnableMetrics: true},
	}
}

// TestRoundTrip is what makes this mirror trustworthy: fs parses the rendered
// bytes with the same yaml decoder, so anything that survives a round trip
// here reaches fs unchanged — durations in particular, which fs writes as Go
// duration strings.
func TestRoundTrip(t *testing.T) {
	data, err := Marshal(valid())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if diff := cmp.Diff(valid(), got); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}

	if !strings.Contains(string(data), "ttl: 15s") {
		t.Errorf("durations are not rendered as fs writes them:\n%s", data)
	}
}

func TestValidate(t *testing.T) {
	base := valid()
	if err := base.Validate(); err != nil {
		t.Fatalf("a rendered configuration must validate: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "no listener", mutate: func(c *Config) { c.Server.Addr = "" }},
		{name: "no timeouts", mutate: func(c *Config) { c.Server.WriteTimeout = 0 }},
		{name: "no storage root", mutate: func(c *Config) { c.Storage.Root = "" }},
		{name: "unknown storage type", mutate: func(c *Config) { c.Storage.Type = "s3" }},
		{
			// fs refuses the cluster-wide credential store without cluster
			// storage behind it.
			name: "etcd credentials on the filesystem backend",
			mutate: func(c *Config) {
				c.Storage.Type = StorageTypeFilesystem
				c.Auth.Source = AuthSourceEtcd
			},
		},
		{name: "no service name", mutate: func(c *Config) { c.Observability.ServiceName = "" }},
		{name: "no node id", mutate: func(c *Config) { c.Cluster.NodeID = "" }},
		{name: "no advertise address", mutate: func(c *Config) { c.Cluster.AdvertiseAddr = "" }},
		{name: "no etcd", mutate: func(c *Config) { c.Cluster.Etcd.Endpoints = nil }},
		{name: "sub-second etcd ttl", mutate: func(c *Config) { c.Cluster.Etcd.TTL = time.Millisecond }},
		{name: "negative settle", mutate: func(c *Config) { c.Cluster.Rebalance.Settle = -time.Minute }},
		{name: "watermark above one", mutate: func(c *Config) { c.Cluster.Rebalance.FullWatermark = 1.5 }},
		{name: "negative scrub interval", mutate: func(c *Config) { c.Integrity.ScrubInterval = -time.Hour }},
		{name: "unidentified disk", mutate: func(c *Config) { c.Cluster.Disks[1].ID = "" }},
		{name: "rootless disk", mutate: func(c *Config) { c.Cluster.Disks[1].Path = "" }},
		{name: "duplicate disk", mutate: func(c *Config) { c.Cluster.Disks[1].ID = "d0" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)

			if err := cfg.Validate(); err == nil {
				t.Error("validation passed, want an error")
			}
		})
	}
}

// TestValidateAcceptsFilesystemBackend covers the single-node development
// config: no cluster section at all, and none of the cluster checks applied to
// it.
func TestValidateAcceptsFilesystemBackend(t *testing.T) {
	cfg := valid()
	cfg.Storage.Type = StorageTypeFilesystem
	cfg.Auth.Source = AuthSourceFile
	cfg.Cluster = Cluster{}

	if err := cfg.Validate(); err != nil {
		t.Errorf("a single-node config was refused: %v", err)
	}
}

// TestValidateAcceptsDrainedDisk pins that a drained disk — expressed with a
// negative weight, see fscluster.DrainWeight — is a valid configuration and
// not mistaken for a misconfigured one.
func TestValidateAcceptsDrainedDisk(t *testing.T) {
	cfg := valid()
	for i := range cfg.Cluster.Disks {
		cfg.Cluster.Disks[i].Weight = -1
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("a drained node must validate: %v", err)
	}
}
