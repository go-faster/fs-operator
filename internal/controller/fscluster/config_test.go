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
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/fsconfig"
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

	storageClass = "fast-nvme"
	tlsSecret    = "prod-s3-tls"
	teamValue    = "storage"
	publicBucket = "public"
)

// testCapacity is the test clusters' volume size in bytes: what each node
// joins the layout with.
const testCapacity = 200 << 30

// testCluster is a minimal valid cluster: three flat nodes with 200Gi each.
// Cases below start from it and change one thing at a time.
func testCluster() *fsv1alpha1.FSCluster {
	nodes := int32(3)

	return &fsv1alpha1.FSCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "tenant-a"},
		Spec: fsv1alpha1.FSClusterSpec{
			Topology: fsv1alpha1.TopologySpec{Nodes: &nodes},
			Storage:  fsv1alpha1.StorageSpec{Size: resource.MustParse("200Gi")},
		},
	}
}

// testKeys is a declarative credential set as the render step collects it.
var testKeys = []fsconfig.Key{{
	AccessKey: "AKmedia",
	SecretKey: "media-secret-key-0123456789",
	Grants:    []fsconfig.Grant{{Bucket: "media-*", Permission: "write"}},
}}

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
			// Failure domains, TLS, declarative credentials, a public bucket
			// and the telemetry switches the file carries.
			name: "racks-full",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Topology = fsv1alpha1.TopologySpec{
					Racks: []fsv1alpha1.RackSpec{
						{Name: "a", Nodes: 2, Zone: zoneA},
						{Name: "b", Nodes: 2, Zone: "eu-central-1b"},
						{Name: "c", Nodes: 2, NodeSelector: map[string]string{rackKey: "c"}},
					},
				}
				c.Spec.Layout.Widths = []int32{3, 6}
				c.Spec.Storage.StorageClass = storageClass
				c.Spec.S3.TLS.SecretName = tlsSecret
				c.Spec.Auth.PublicReadBuckets = []string{publicBucket}
				c.Spec.Observability.LogLevel = "debug"
				c.Spec.Observability.OTLP.Endpoint = "http://otel-collector.observability:4317"
			},
			opts: RenderOptions{Keys: testKeys},
		},
		{
			// The single-node development shape: no cluster section at all.
			name: "single-node",
			mutate: func(c *fsv1alpha1.FSCluster) {
				nodes := int32(1)

				c.Spec.Topology = fsv1alpha1.TopologySpec{Nodes: &nodes}
				c.Spec.Auth.PublicReadBuckets = []string{publicBucket}
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

	if cfg.Storage.Root != StorageRoot {
		t.Errorf("storage.root = %q, want the data volume's mount %q", cfg.Storage.Root, StorageRoot)
	}

	if cfg.Cluster.NodeID == "" {
		// A single node: nothing below applies.
		if len(cfg.Cluster.Peers) > 0 || cfg.Cluster.AdvertiseAddr != "" {
			t.Errorf("cluster section = %+v, want it empty on a single node", cfg.Cluster)
		}

		return
	}

	if cfg.Cluster.NodeID != node.Name {
		t.Errorf("node_id = %q, want %q", cfg.Cluster.NodeID, node.Name)
	}

	if want := PodName(node.Name) + "."; !strings.HasPrefix(cfg.Cluster.AdvertiseAddr, want) {
		t.Errorf("advertise_addr = %q, want it to start with %q", cfg.Cluster.AdvertiseAddr, want)
	}

	if slices.Contains(cfg.Cluster.Peers, cfg.Cluster.AdvertiseAddr) || len(cfg.Cluster.Peers) == 0 {
		t.Errorf("peers = %v, want every other node and not this one", cfg.Cluster.Peers)
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
// in a rendered file.
func TestRenderNodeConfigNoSecretMaterial(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()

	rc, err := RenderNodeConfig(cluster, Nodes(cluster)[0], RenderOptions{Keys: testKeys})
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

	// FSAccessKey credentials are the one secret the file does carry: that is
	// how every node accepts them and a reload applies a change everywhere.
	if _, ok := section("auth")["keys"]; !ok {
		t.Error("auth.keys is missing; the cluster's FSAccessKeys are rendered into every node")
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

// TestRestartRevisionTracksConfig checks which changes replace a node and which
// a reload applies. Turning TLS on is restart-requiring: both revisions move.
// A credential, or a node added to the cluster, moves only the config
// revision — the credential is reloaded, and the peer list is read only by a
// starting node — and re-rendering an unchanged spec moves neither.
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

	keyed := RenderOptions{Keys: testKeys}
	if mustRender(t, cluster, node, keyed).Revision == baseCfg.Revision {
		t.Error("a new credential did not change the config revision")
	}

	if mustRestart(t, cluster, node, keyed) != baseRestart {
		t.Error("a new credential changed the restart revision; it is a reload, not a restart")
	}

	grown := cluster.DeepCopy()
	grown.Spec.Topology.Nodes = new(int32(4))

	if mustRender(t, grown, node, base).Revision == baseCfg.Revision {
		t.Error("adding a node did not change the peer list")
	}

	if mustRestart(t, grown, node, base) != baseRestart {
		t.Error("adding a node changed the restart revision; every node would restart for a peer list")
	}

	tls := cluster.DeepCopy()
	tls.Spec.S3.TLS.SecretName = tlsSecret

	if mustRender(t, tls, node, base).Revision == baseCfg.Revision {
		t.Error("turning TLS on did not change the config revision")
	}

	if mustRestart(t, tls, node, base) == baseRestart {
		t.Error("turning TLS on did not change the restart revision; the node would not pick it up")
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
