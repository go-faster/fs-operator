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

	"github.com/go-faster/errors"
	"github.com/go-faster/fs/adminapi"
	"github.com/google/go-cmp/cmp"

	"github.com/go-faster/fs-operator/internal/fsclient"
)

// The tests run the operator's client against the real ogen server for the fs
// admin API, so the wire format — the same code fs serves — is exercised end
// to end. A bearer guard in front asserts the client authenticates.

const (
	testToken   = "admin-token-abc"
	testVersion = "v0.6.0"

	// firstNode and secondNode are the node IDs the fixtures key off.
	firstNode  = "fs-0"
	secondNode = "fs-1"
)

// stubHandler serves canned admin responses. It embeds UnimplementedHandler so
// only the operations the operator uses need to be defined.
type stubHandler struct {
	adminapi.UnimplementedHandler

	info   *adminapi.InstanceInfo
	reload *adminapi.ReloadResult
	layout *adminapi.Layout
	nodes  *adminapi.ClusterNodeList

	// fail, when set, is returned by every layout call instead of a result.
	fail *adminapi.ErrorStatusCode

	applied     *adminapi.ApplyLayoutRequest
	reloadCalls int
}

func (h *stubHandler) GetInfo(context.Context) (*adminapi.InstanceInfo, error) {
	return h.info, nil
}

func (h *stubHandler) ReloadConfig(context.Context) (*adminapi.ReloadResult, error) {
	h.reloadCalls++

	return h.reload, nil
}

func (h *stubHandler) GetLayout(context.Context) (*adminapi.Layout, error) {
	if h.fail != nil {
		return nil, h.fail
	}

	return h.layout, nil
}

func (h *stubHandler) ApplyLayout(
	_ context.Context, req *adminapi.ApplyLayoutRequest, _ adminapi.ApplyLayoutParams,
) (*adminapi.LayoutChange, error) {
	if h.fail != nil {
		return nil, h.fail
	}

	h.applied = req

	return &adminapi.LayoutChange{Layout: *h.layout, Applied: true}, nil
}

func (h *stubHandler) ListClusterNodes(context.Context) (*adminapi.ClusterNodeList, error) {
	return h.nodes, nil
}

// NewError passes a status error through as the response, which is how fs's
// own handler reports a 404 or a 400.
func (h *stubHandler) NewError(_ context.Context, err error) *adminapi.ErrorStatusCode {
	var status *adminapi.ErrorStatusCode
	if errors.As(err, &status) {
		return status
	}

	return &adminapi.ErrorStatusCode{StatusCode: http.StatusInternalServerError}
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

func TestClientLayout(t *testing.T) {
	handler := &stubHandler{layout: &adminapi.Layout{
		Version: 7,
		Widths:  []int{3, 6},
		Members: []adminapi.LayoutMember{
			{ID: firstNode, Zone: adminapi.NewOptString("z1"), Rack: adminapi.NewOptString("a"), Capacity: 100},
			{ID: secondNode, Capacity: 50},
		},
		RetainedVersions: []uint64{6},
	}}
	url, _ := newServer(t, handler)

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	layout, err := client.Layout(context.Background())
	if err != nil {
		t.Fatalf("layout: %v", err)
	}

	want := fsclient.Layout{
		Version:  7,
		Widths:   []int{3, 6},
		Members:  []fsclient.Role{{ID: firstNode, Zone: "z1", Rack: "a", Capacity: 100}, {ID: secondNode, Capacity: 50}},
		Retained: []uint64{6},
	}
	if diff := cmp.Diff(want, layout); diff != "" {
		t.Errorf("layout (-want +got):\n%s", diff)
	}

	if !layout.Transitioning() {
		t.Error("a layout with a retained version is in transition")
	}

	if role, ok := layout.Member(secondNode); !ok || role.Capacity != 50 {
		t.Errorf("member fs-1 = %+v, %v", role, ok)
	}
}

// TestClientLayoutNone pins that a cluster with no layout yet — every cluster
// before the operator's first apply — reads as ErrNoLayout, not as a failure.
func TestClientLayoutNone(t *testing.T) {
	url, _ := newServer(t, &stubHandler{fail: &adminapi.ErrorStatusCode{
		StatusCode: http.StatusNotFound,
		Response:   adminapi.Error{ErrorMessage: "no layout"},
	}})

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if _, err := client.Layout(context.Background()); !errors.Is(err, fsclient.ErrNoLayout) {
		t.Errorf("layout error = %v, want ErrNoLayout", err)
	}
}

func TestClientApplyLayout(t *testing.T) {
	handler := &stubHandler{layout: &adminapi.Layout{Version: 2, Widths: []int{3}}}
	url, _ := newServer(t, handler)

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	layout, err := client.ApplyLayout(context.Background(),
		[]fsclient.Role{{ID: firstNode, Zone: "z1", Rack: "a", Capacity: 100}, {ID: secondNode, Capacity: 50}},
		[]int{3})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if layout.Version != 2 {
		t.Errorf("version = %d, want the layout fs returned", layout.Version)
	}

	want := &adminapi.ApplyLayoutRequest{
		Members: []adminapi.LayoutRole{
			{ID: firstNode, Zone: adminapi.NewOptString("z1"), Rack: adminapi.NewOptString("a"), Capacity: 100},
			// No zone or rack is sent as absent, not as "".
			{ID: secondNode, Capacity: 50},
		},
		Widths: []int{3},
	}
	if diff := cmp.Diff(want, handler.applied); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}
}

func TestClientApplyLayoutRejected(t *testing.T) {
	url, _ := newServer(t, &stubHandler{fail: &adminapi.ErrorStatusCode{
		StatusCode: http.StatusBadRequest,
		Response:   adminapi.Error{ErrorMessage: "2 members with capacity, width 3"},
	}})

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	_, err = client.ApplyLayout(context.Background(), []fsclient.Role{{ID: firstNode, Capacity: 1}}, []int{3})
	if !errors.Is(err, fsclient.ErrLayoutRejected) {
		t.Errorf("apply error = %v, want ErrLayoutRejected", err)
	}
}

func TestClientNodes(t *testing.T) {
	url, _ := newServer(t, &stubHandler{nodes: &adminapi.ClusterNodeList{Nodes: []adminapi.ClusterNode{
		{ID: adminapi.NewOptString(firstNode), Addr: "a:7080", Self: true, Up: true,
			LayoutVersion: adminapi.NewOptUint64(3), SyncedVersion: adminapi.NewOptUint64(3)},
		{Addr: "b:7080", Error: adminapi.NewOptString("connection refused")},
	}}})

	client, err := fsclient.New(url, testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	nodes, err := client.Nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}

	want := []fsclient.Node{
		{ID: firstNode, Addr: "a:7080", Self: true, Up: true, LayoutVersion: 3, SyncedVersion: 3},
		{Addr: "b:7080", Error: "connection refused"},
	}
	if diff := cmp.Diff(want, nodes); diff != "" {
		t.Errorf("nodes (-want +got):\n%s", diff)
	}
}

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
