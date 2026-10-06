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
		Storage: Storage{Root: "/var/lib/fs"},
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
			Addr:          ":7080",
			AdvertiseAddr: "prod-a-0-0.prod-peers.tenant-a.svc:7080",
			Peers:         []string{"prod-a-0-0.prod-peers.tenant-a.svc:7080", "prod-b-0-0.prod-peers.tenant-a.svc:7080"},
		},
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

	if !strings.Contains(string(data), "write_timeout: 2m0s") {
		t.Errorf("durations are not rendered as fs writes them:\n%s", data)
	}
}

// TestUnmarshalRefusesUnknownKeys pins the decoder to fs's own strictness: a
// key fs does not know stops the node, so the mirror must not accept one
// either.
func TestUnmarshalRefusesUnknownKeys(t *testing.T) {
	if _, err := Unmarshal([]byte("storage:\n  root: /data\n  type: cluster\n")); err == nil {
		t.Error("a removed key (storage.type) was accepted")
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
		{name: "no service name", mutate: func(c *Config) { c.Observability.ServiceName = "" }},
		{name: "no advertise address", mutate: func(c *Config) { c.Cluster.AdvertiseAddr = "" }},
		{name: "peers without a node id", mutate: func(c *Config) { c.Cluster.NodeID = "" }},
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

// TestValidateAcceptsSingleNode covers the single-node development config: no
// cluster section at all, and none of the cluster checks applied to it.
func TestValidateAcceptsSingleNode(t *testing.T) {
	cfg := valid()
	cfg.Cluster = Cluster{}

	if err := cfg.Validate(); err != nil {
		t.Errorf("a single-node config was refused: %v", err)
	}
}
