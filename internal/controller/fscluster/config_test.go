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
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/fsconfig"
	"github.com/go-faster/fs-operator/internal/scheme"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// The identities the test cluster expands to, and the scheduling values its
// racks are pinned with.
const (
	node0 = "prod-0"
	node1 = "prod-1"
	node2 = "prod-2"

	zoneA   = "eu-central-1a"
	rackKey = "rack"

	// schemeEC needs six failure domains, one more than any test topology
	// provides by accident.
	schemeEC = "ec:4,2"

	storageClass = "fast-nvme"
	tlsSecret    = "prod-s3-tls"
	teamValue    = "storage"
	publicBucket = "public"
)

// testCluster is a minimal valid cluster: three flat nodes, one disk, external
// etcd. Cases below start from it and change one thing at a time.
func testCluster() *fsv1alpha1.FSCluster {
	nodes := int32(3)

	return &fsv1alpha1.FSCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "tenant-a"},
		Spec: fsv1alpha1.FSClusterSpec{
			Topology: fsv1alpha1.TopologySpec{Nodes: &nodes},
			Storage: fsv1alpha1.StorageSpec{
				Disks: []fsv1alpha1.DiskSpec{
					{Name: "d0", Size: resource.MustParse("200Gi")},
				},
			},
			Etcd: fsv1alpha1.EtcdSpec{
				External: &fsv1alpha1.ExternalEtcdSpec{
					Endpoints: []string{
						"http://etcd-0.etcd.fs-system:2379",
						"http://etcd-1.etcd.fs-system:2379",
						"http://etcd-2.etcd.fs-system:2379",
					},
				},
			},
		},
	}
}

func TestRenderNodeConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fsv1alpha1.FSCluster)
		opts   RenderOptions
	}{
		{
			// Everything a 3-node development cluster leaves at its default.
			name:   "flat-minimal",
			mutate: func(*fsv1alpha1.FSCluster) {},
		},
		{
			// Failure domains, several weighted disks, TLS, declarative
			// credentials and every tuning knob the renderer passes through.
			name: "racks-full",
			mutate: func(c *fsv1alpha1.FSCluster) {
				half := resource.MustParse("0.5")
				watermark := resource.MustParse("0.85")

				c.Spec.Scheme = schemeEC
				c.Spec.Topology = fsv1alpha1.TopologySpec{
					Racks: []fsv1alpha1.RackSpec{
						{Name: "a", Nodes: 2, Zone: zoneA},
						{Name: "b", Nodes: 2, Zone: "eu-central-1b"},
						{Name: "c", Nodes: 2, NodeSelector: map[string]string{rackKey: "c"}},
					},
				}
				c.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
					{Name: "d0", Size: resource.MustParse("1Ti"), StorageClass: storageClass},
					{Name: "d1", Size: resource.MustParse("1Ti"), Weight: &half},
				}
				c.Spec.Etcd.Prefix = "/fs/prod"
				c.Spec.Etcd.TTL = &metav1.Duration{Duration: 15 * time.Second}
				c.Spec.S3.TLS.SecretName = tlsSecret
				c.Spec.Rebalance = fsv1alpha1.RebalanceSpec{
					Settle:        &metav1.Duration{Duration: 2 * time.Minute},
					Cooldown:      &metav1.Duration{Duration: 30 * time.Minute},
					FullWatermark: &watermark,
				}
				c.Spec.Integrity = fsv1alpha1.IntegritySpec{
					VerifyOnRead:    true,
					ScrubInterval:   &metav1.Duration{Duration: 12 * time.Hour},
					ScrubQuarantine: true,
				}
				c.Spec.Observability.LogLevel = "debug"
				c.Spec.Observability.OTLP.Endpoint = "http://otel-collector.observability:4317"
			},
		},
		{
			// A node being decommissioned: its disks leave placement while
			// its peers keep theirs (SPEC §8.4).
			name:   "drained-node",
			mutate: func(*fsv1alpha1.FSCluster) {},
			opts:   RenderOptions{Drained: map[string]bool{node1: true}},
		},
		{
			// Rebalancing off and a disk the operator was asked to drain
			// directly through its weight.
			name: "disabled-rebalance",
			mutate: func(c *fsv1alpha1.FSCluster) {
				zero := resource.MustParse("0")

				c.Spec.Rebalance.AutoDisabled = true
				c.Spec.Storage.Disks = append(c.Spec.Storage.Disks,
					fsv1alpha1.DiskSpec{Name: "d1", Size: resource.MustParse("200Gi"), Weight: &zero})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testCluster()
			tc.mutate(cluster)
			cluster.Spec.WithDefaults()

			nodes := Nodes(cluster)
			if len(nodes) == 0 {
				t.Fatal("topology expanded to no nodes")
			}

			rendered := make([][]byte, 0, len(nodes))

			for _, node := range nodes {
				rc, err := RenderNodeConfig(cluster, node, tc.opts)
				if err != nil {
					t.Fatalf("render node %s: %v", node.Name, err)
				}

				assertUsable(t, rc, node)

				rendered = append(rendered, rc.Data)
			}

			// One golden per case, each node a YAML document, so a diff shows
			// what a change does to every node at once.
			assertGolden(t, tc.name+".yaml", bytes.Join(rendered, []byte("---\n")))
		})
	}
}

// assertUsable checks a rendered config the way fs would on startup, and that
// it says what the node is and carries its revision marker.
func assertUsable(t *testing.T, rc RenderedConfig, node Node) {
	t.Helper()

	cfg, err := fsconfig.Unmarshal(rc.Data)
	if err != nil {
		t.Fatalf("rendered config does not parse: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("rendered config is not valid for fs: %v", err)
	}

	if cfg.Cluster.NodeID != node.Name {
		t.Errorf("node_id = %q, want %q", cfg.Cluster.NodeID, node.Name)
	}

	if cfg.Cluster.Rack != node.Rack {
		t.Errorf("rack = %q, want %q", cfg.Cluster.Rack, node.Rack)
	}

	if want := PodName(node.Name) + "."; !strings.HasPrefix(cfg.Cluster.AdvertiseAddr, want) {
		t.Errorf("advertise_addr = %q, want it to start with %q", cfg.Cluster.AdvertiseAddr, want)
	}

	// The config carries the revision the operator reads back to verify a
	// reload, and it is the value the renderer reports (SPEC §8.3).
	if cfg.Revision == "" {
		t.Error("rendered config has no revision marker")
	}

	if cfg.Revision != rc.Revision {
		t.Errorf("embedded revision %q does not match the reported %q", cfg.Revision, rc.Revision)
	}
}

// TestRenderNodeConfigNoSecretMaterial pins the rule that keeps generated
// Secrets from leaking into places they are not expected: fs takes the peer
// secret and the admin token from the environment, so neither may ever appear
// in a rendered file. Credentials are cluster-wide in etcd (fs §6.8), so the
// config selects that source and carries no keys at all.
func TestRenderNodeConfigNoSecretMaterial(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()

	rc, err := RenderNodeConfig(cluster, Nodes(cluster)[0], RenderOptions{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(rc.Data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}

	section := func(name string) map[string]any {
		s, _ := raw[name].(map[string]any)

		return s
	}

	if _, ok := section("cluster")["secret"]; ok {
		t.Error("cluster.secret is rendered into the config; it must come from FS_CLUSTER_SECRET")
	}

	if _, ok := section("admin")["token"]; ok {
		t.Error("admin.token is rendered into the config; it must come from FS_ADMIN_TOKEN")
	}

	auth := section("auth")
	if auth["source"] != fsconfig.AuthSourceEtcd {
		t.Errorf("auth.source = %v, want %q (credentials are cluster-wide)", auth["source"], fsconfig.AuthSourceEtcd)
	}

	if _, ok := auth["keys"]; ok {
		t.Error("auth.keys is rendered into the config; credentials live in etcd, not the config file")
	}
}

func TestRenderNodeConfigRejectsIncompleteInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster func() *fsv1alpha1.FSCluster
		node    Node
	}{
		{
			name:    "no namespace",
			cluster: func() *fsv1alpha1.FSCluster { c := testCluster(); c.Namespace = ""; return c },
			node:    Node{Name: node0},
		},
		{
			name:    "no cluster name",
			cluster: func() *fsv1alpha1.FSCluster { c := testCluster(); c.Name = ""; return c },
			node:    Node{Name: node0},
		},
		{
			name:    "no node name",
			cluster: testCluster,
			node:    Node{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderNodeConfig(tc.cluster(), tc.node, RenderOptions{}); err == nil {
				t.Error("render succeeded, want an error")
			}
		})
	}
}

// TestRenderNodeConfigsIsStable guards the property the whole rolling machine
// rests on: rendering the same cluster twice produces byte-identical configs,
// so an unchanged spec never looks like a pending change.
func TestRenderNodeConfigsIsStable(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()

	nodes := Nodes(cluster)

	first, err := RenderNodeConfigs(cluster, nodes, RenderOptions{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	second, err := RenderNodeConfigs(cluster, nodes, RenderOptions{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("rendering is not stable (-first +second):\n%s", diff)
	}

	if got, want := ConfigRevision(second), ConfigRevision(first); got != want {
		t.Errorf("configuration revision changed without the configuration: %s != %s", got, want)
	}
}

func TestConfigRevision(t *testing.T) {
	// rc wraps content as a rendered config; only the bytes feed the digest.
	rc := func(s string) RenderedConfig { return RenderedConfig{Data: []byte(s)} }

	base := map[string]RenderedConfig{node0: rc("a"), node1: rc("b")}

	revision := ConfigRevision(base)

	if !strings.HasPrefix(revision, revisionPrefix) {
		t.Errorf("revision %q does not carry the %q prefix", revision, revisionPrefix)
	}

	if got, want := len(revision), len(revisionPrefix)+revisionDigits; got != want {
		t.Errorf("revision %q has length %d, want %d", revision, got, want)
	}

	for _, tc := range []struct {
		name    string
		configs map[string]RenderedConfig
	}{
		{name: "changed config", configs: map[string]RenderedConfig{node0: rc("a"), node1: rc("c")}},
		{name: "added node", configs: map[string]RenderedConfig{node0: rc("a"), node1: rc("b"), node2: rc("c")}},
		{name: "removed node", configs: map[string]RenderedConfig{node0: rc("a")}},
		{name: "renamed node", configs: map[string]RenderedConfig{node0: rc("a"), node2: rc("b")}},
		// Without length prefixes these would hash the same as base.
		{name: "shifted bytes", configs: map[string]RenderedConfig{node0: rc("ab"), node1: rc("")}},
		{name: "shifted name", configs: map[string]RenderedConfig{"prod-0a": rc(""), node1: rc("b")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConfigRevision(tc.configs); got == revision {
				t.Errorf("revision %s is unchanged by %s", got, tc.name)
			}
		})
	}
}

// TestRestartRevisionTracksConfig checks that a restart-requiring config change
// (the scheme) moves both the config revision — so the change is verifiable —
// and the restart revision — so the node is actually replaced to pick it up —
// while re-rendering an unchanged spec moves neither. Credentials and
// public-read are no longer in the config (they are cluster-wide in etcd,
// fs §6.8), so the config's remaining fields are all restart-requiring.
func TestRestartRevisionTracksConfig(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()
	node := Nodes(cluster)[0]

	base := RenderOptions{}

	baseCfg := mustRender(t, cluster, node, base)
	baseRestart := mustRestart(t, cluster, node, base)

	// Re-rendering the same spec is stable: no phantom change.
	if again := mustRender(t, cluster, node, base); again.Revision != baseCfg.Revision {
		t.Error("re-rendering an unchanged spec moved the config revision")
	}

	if again := mustRestart(t, cluster, node, base); again != baseRestart {
		t.Error("re-rendering an unchanged spec moved the restart revision")
	}

	// The scheme lives in the config and is restart-requiring: both move.
	cluster.Spec.Scheme = scheme.RF3
	schemeCfg := mustRender(t, cluster, node, base)
	schemeRestart := mustRestart(t, cluster, node, base)

	if schemeCfg.Revision == baseCfg.Revision {
		t.Error("changing the scheme did not change the config revision")
	}

	if schemeRestart == baseRestart {
		t.Error("changing the scheme did not change the restart revision; the node would not pick it up")
	}
}

func mustRender(t *testing.T, cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions) RenderedConfig {
	t.Helper()

	rc, err := RenderNodeConfig(cluster, node, opts)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	return rc
}

func mustRestart(t *testing.T, cluster *fsv1alpha1.FSCluster, node Node, opts RenderOptions) string {
	t.Helper()

	rev, err := RestartRevision(cluster, node, opts)
	if err != nil {
		t.Fatalf("restart revision: %v", err)
	}

	return rev
}

func TestDiskWeight(t *testing.T) {
	weight := func(s string) *resource.Quantity {
		q := resource.MustParse(s)
		return &q
	}

	for _, tc := range []struct {
		name    string
		disk    fsv1alpha1.DiskSpec
		drained bool
		want    float64
	}{
		{name: "unset", disk: fsv1alpha1.DiskSpec{}, want: DefaultDiskWeight},
		{name: "explicit", disk: fsv1alpha1.DiskSpec{Weight: weight("3")}, want: 3},
		{name: "fractional", disk: fsv1alpha1.DiskSpec{Weight: weight("0.5")}, want: 0.5},
		{name: "zero drains", disk: fsv1alpha1.DiskSpec{Weight: weight("0")}, want: DrainWeight},
		{name: "drained node", disk: fsv1alpha1.DiskSpec{Weight: weight("2")}, drained: true, want: DrainWeight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := diskWeight(tc.disk, tc.drained); got != tc.want {
				t.Errorf("diskWeight = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestS3Endpoint covers the one name that depends on more than its inputs'
// concatenation.
func TestS3Endpoint(t *testing.T) {
	if got, want := S3Endpoint("prod", "tenant-a", 8080, false), "http://prod.tenant-a.svc:8080"; got != want {
		t.Errorf("S3Endpoint = %q, want %q", got, want)
	}

	if got, want := S3Endpoint("prod", "tenant-a", 443, true), "https://prod.tenant-a.svc:443"; got != want {
		t.Errorf("S3Endpoint = %q, want %q", got, want)
	}
}

// TestAdminURL pins the per-node admin endpoint the operator dials: the pod's
// stable DNS name through the peers Service, on the admin port.
func TestAdminURL(t *testing.T) {
	if got, want := AdminURL("prod", "tenant-a", "prod-1"),
		"http://prod-1-0.prod-peers.tenant-a.svc:8090"; got != want {
		t.Errorf("AdminURL = %q, want %q", got, want)
	}
}

// assertGolden compares against testdata/<name>, rewriting it under -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("create testdata: %v", err)
		}

		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	want, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read golden (run `go test ./... -update` to create it): %v", err)
	}

	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("rendered config differs from %s (-golden +rendered):\n%s", path, diff)
	}
}
