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

package fsclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-faster/fs/adminapi"

	"github.com/go-faster/fs-operator/internal/fsclient"
)

// The tests run the operator's client against the real ogen server for the fs
// admin API, so the wire format — the same code fs serves — is exercised end
// to end. A bearer guard in front asserts the client authenticates.

const (
	testToken   = "admin-token-abc"
	testVersion = "v0.6.0"
)

// stubHandler serves canned admin responses. It embeds UnimplementedHandler so
// only the operations the operator uses need to be defined.
type stubHandler struct {
	adminapi.UnimplementedHandler

	info      *adminapi.InstanceInfo
	reload    *adminapi.ReloadResult
	cluster   *adminapi.ClusterStatus
	rebalance *adminapi.RebalanceStatus

	reloadCalls int
}

func (h *stubHandler) GetInfo(context.Context) (*adminapi.InstanceInfo, error) {
	return h.info, nil
}

func (h *stubHandler) ReloadConfig(context.Context) (*adminapi.ReloadResult, error) {
	h.reloadCalls++

	return h.reload, nil
}

func (h *stubHandler) GetClusterStatus(context.Context) (*adminapi.ClusterStatus, error) {
	return h.cluster, nil
}

func (h *stubHandler) GetRebalanceStatus(context.Context) (*adminapi.RebalanceStatus, error) {
	return h.rebalance, nil
}

// newServer serves handler behind a bearer guard, returning the base URL and
// how many requests arrived without the expected token.
func newServer(t *testing.T, handler adminapi.Handler) (string, *int) {
	t.Helper()

	ogen, err := adminapi.NewServer(handler)
	if err != nil {
		t.Fatalf("build admin server: %v", err)
	}

	unauthorized := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			unauthorized++

			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		ogen.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, &unauthorized
}

func TestClientInfo(t *testing.T) {
	handler := &stubHandler{info: &adminapi.InstanceInfo{
		Version:        testVersion,
		Commit:         "abcdef0",
		ConfigRevision: adminapi.NewOptString("cfg-112233445566"),
	}}
	url, unauthorized := newServer(t, handler)

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	info, err := client.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}

	if info.ConfigRevision != "cfg-112233445566" {
		t.Errorf("config revision = %q, want the marker the node loaded", info.ConfigRevision)
	}

	if info.Version != testVersion {
		t.Errorf("version = %q, want v0.6.0", info.Version)
	}

	if *unauthorized != 0 {
		t.Errorf("%d requests arrived without the bearer token", *unauthorized)
	}
}

// TestClientInfoNoRevision covers a node whose config sets no revision: the
// optional field maps to the empty string, not a leaked ogen Opt.
func TestClientInfoNoRevision(t *testing.T) {
	url, _ := newServer(t, &stubHandler{info: &adminapi.InstanceInfo{Version: testVersion}})

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	info, err := client.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}

	if info.ConfigRevision != "" {
		t.Errorf("config revision = %q, want empty", info.ConfigRevision)
	}
}

func TestClientReload(t *testing.T) {
	handler := &stubHandler{reload: &adminapi.ReloadResult{
		Reloaded:       []string{"credentials", "tls"},
		ConfigRevision: adminapi.NewOptString("cfg-778899aabbcc"),
	}}
	url, _ := newServer(t, handler)

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	res, err := client.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if handler.reloadCalls != 1 {
		t.Errorf("reload called %d times, want 1", handler.reloadCalls)
	}

	if res.ConfigRevision != "cfg-778899aabbcc" {
		t.Errorf("revision = %q, want the post-reload marker", res.ConfigRevision)
	}

	if len(res.Reloaded) != 2 {
		t.Errorf("reloaded = %v, want credentials + tls", res.Reloaded)
	}
}

func TestClientClusterStatus(t *testing.T) {
	handler := &stubHandler{cluster: &adminapi.ClusterStatus{
		State:               adminapi.ClusterStateOk,
		SchemaVersion:       4,
		BinarySchemaVersion: 5,
		NodeCount:           6,
		DiskCount:           6,
		PlacementSkew:       0.02,
		RebalanceRunning:    true,
	}}
	url, _ := newServer(t, handler)

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	status, err := client.ClusterStatus(context.Background())
	if err != nil {
		t.Fatalf("cluster status: %v", err)
	}

	if status.Disabled {
		t.Error("status reports disabled for a cluster-mode node")
	}

	// A binary schema ahead of the cluster's is the migration-pending signal.
	if status.SchemaVersion != 4 || status.BinarySchema != 5 {
		t.Errorf("schema = cluster %d/binary %d, want 4/5", status.SchemaVersion, status.BinarySchema)
	}

	if status.NodeCount != 6 || !status.RebalanceRunning {
		t.Errorf("status = %+v, want 6 nodes with a rebalance running", status)
	}
}

// TestClientClusterStatusNodes covers the per-node view fs v0.9.0 added: the
// aggregate repair queue, the reporting split, per-disk capacity and the live
// state of a node that answered — plus a node that did not, which must stay
// distinguishable from an idle one.
func TestClientClusterStatusNodes(t *testing.T) {
	handler := &stubHandler{cluster: &adminapi.ClusterStatus{
		State:             adminapi.ClusterStateOk,
		NodeCount:         2,
		DiskCount:         3,
		TotalBytes:        300,
		FreeBytes:         180,
		RepairQueueDepth:  7,
		NodesReporting:    1,
		NodesNotReporting: 1,
		Nodes: []adminapi.ClusterNode{
			{
				ID:   "fs-0",
				Rack: adminapi.NewOptString("a"),
				Disks: []adminapi.ClusterDisk{
					{
						ID:         "d0",
						Weight:     1,
						TotalBytes: adminapi.NewOptInt64(100),
						FreeBytes:  adminapi.NewOptInt64(40),
						Fullness:   adminapi.NewOptFloat64(0.6),
					},
					{
						// Drained, and still holding data: the state a
						// decommission passes through (SPEC §8.4).
						ID:         "d1",
						Weight:     -1,
						TotalBytes: adminapi.NewOptInt64(100),
						FreeBytes:  adminapi.NewOptInt64(90),
						Fullness:   adminapi.NewOptFloat64(0.1),
					},
				},
				Live: adminapi.NewOptClusterNodeLive(adminapi.ClusterNodeLive{
					Version:          adminapi.NewOptString(testVersion),
					SchemaVersion:    adminapi.NewOptInt(5),
					RepairQueueDepth: 7,
					RebalanceState:   adminapi.RebalanceStateRunning,
				}),
			},
			{
				// Silent: unreachable, or a binary predating the peer status
				// endpoint. It reports no capacity either.
				ID:        "fs-1",
				Disks:     []adminapi.ClusterDisk{{ID: "d0", Weight: 1}},
				LiveError: adminapi.NewOptString("peer does not serve live state"),
			},
		},
	}}
	url, _ := newServer(t, handler)

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	status, err := client.ClusterStatus(context.Background())
	if err != nil {
		t.Fatalf("cluster status: %v", err)
	}

	if status.RepairQueueDepth != 7 {
		t.Errorf("aggregate repair queue = %d, want 7", status.RepairQueueDepth)
	}

	// One node silent means the aggregate is a partial count, and every gate
	// that must not mistake silence for quiescence has to see that.
	if status.AllNodesReporting() {
		t.Error("AllNodesReporting is true with a node not reporting")
	}

	reporting, ok := status.Node("fs-0")
	if !ok {
		t.Fatal("fs-0 missing from the per-node view")
	}

	if reporting.Rack != "a" {
		t.Errorf("fs-0 rack = %q, want %q", reporting.Rack, "a")
	}

	// 60 used on d0 plus 10 on d1: the only occupancy signal fs exposes.
	used, known := reporting.UsedBytes()
	if !known || used != 70 {
		t.Errorf("fs-0 used = %d (known %v), want 70 (known)", used, known)
	}

	if reporting.Live == nil {
		t.Fatal("fs-0 answered, so it should carry live state")
	}

	if reporting.Live.RebalanceState != string(adminapi.RebalanceStateRunning) {
		t.Errorf("fs-0 rebalance state = %q, want running", reporting.Live.RebalanceState)
	}

	if reporting.Disks[0].Drained() || !reporting.Disks[1].Drained() {
		t.Errorf("fs-0 disks = %+v, want only d1 drained", reporting.Disks)
	}

	silent, ok := status.Node("fs-1")
	if !ok {
		t.Fatal("fs-1 missing from the per-node view")
	}

	if silent.Live != nil {
		t.Error("fs-1 did not answer, so it must carry no live state")
	}

	if silent.LiveError == "" {
		t.Error("a node that did not answer must say why")
	}

	// A disk reporting no capacity must not read as an empty disk: that is
	// exactly the reading that would delete a node still holding data.
	if used, known := silent.UsedBytes(); known {
		t.Errorf("fs-1 reports no capacity, got used = %d known", used)
	}
}

// TestClientClusterStatusDisabled covers a node not in cluster mode.
func TestClientClusterStatusDisabled(t *testing.T) {
	url, _ := newServer(t, &stubHandler{cluster: &adminapi.ClusterStatus{State: adminapi.ClusterStateDisabled}})

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	status, err := client.ClusterStatus(context.Background())
	if err != nil {
		t.Fatalf("cluster status: %v", err)
	}

	if !status.Disabled {
		t.Error("a node not in cluster mode should report disabled")
	}
}

func TestClientRebalance(t *testing.T) {
	handler := &stubHandler{rebalance: &adminapi.RebalanceStatus{
		State:            adminapi.RebalanceStateRunning,
		RepairQueueDepth: 42,
	}}
	url, _ := newServer(t, handler)

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	rb, err := client.Rebalance(context.Background())
	if err != nil {
		t.Fatalf("rebalance: %v", err)
	}

	if rb.RepairQueueDepth != 42 {
		t.Errorf("repair queue depth = %d, want 42", rb.RepairQueueDepth)
	}

	if rb.State != "running" {
		t.Errorf("state = %q, want running", rb.State)
	}
}

// TestClientRejectsWrongToken confirms the bearer token is actually sent: a
// client with the wrong token is refused by the guard.
func TestClientRejectsWrongToken(t *testing.T) {
	url, unauthorized := newServer(t, &stubHandler{info: &adminapi.InstanceInfo{}})

	client, err := fsclient.New(url, "wrong-token")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if _, err := client.Info(context.Background()); err == nil {
		t.Error("info succeeded with the wrong token, want it refused")
	}

	if *unauthorized == 0 {
		t.Error("the guard saw no unauthorized request")
	}
}

// TestPoolCachesClients checks that the pool hands back the same client for a
// repeated endpoint+token and a fresh one when the token changes.
func TestPoolCachesClients(t *testing.T) {
	pool := fsclient.NewPool(fsclient.WithPoolTimeout(2 * time.Second))

	first, err := pool.Client("http://prod-0-0.prod-peers.ns.svc:8090", testToken)
	if err != nil {
		t.Fatalf("pool client: %v", err)
	}

	again, err := pool.Client("http://prod-0-0.prod-peers.ns.svc:8090", testToken)
	if err != nil {
		t.Fatalf("pool client: %v", err)
	}

	if first != again {
		t.Error("the pool built a new client for the same endpoint and token")
	}

	rotated, err := pool.Client("http://prod-0-0.prod-peers.ns.svc:8090", "new-token")
	if err != nil {
		t.Fatalf("pool client: %v", err)
	}

	if rotated == first {
		t.Error("the pool reused a client after the token changed")
	}
}
