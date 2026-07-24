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

// Package fsclient is the operator's view of a running fs cluster: a thin
// wrapper over the go-faster/fs admin API (github.com/go-faster/fs/adminapi)
// that the controllers use for the things Kubernetes cannot tell them — which
// configuration a node has applied, whether the cluster has reconverged, and
// to trigger a hot reload (SPEC §4.2, §8.2, §8.3).
//
// The wrapper deliberately returns plain structs rather than the generated
// optional types, so the reconciler never handles ogen `Opt*` values, and it
// keeps the admin API a single seam that the rest of the operator mocks.
package fsclient

import (
	"context"
	"net/http"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// DefaultTimeout bounds a single admin request when the caller's context has
// no deadline, so a wedged node cannot stall a reconcile.
const DefaultTimeout = 15 * time.Second

// Info is what a node reports about itself (GET /api/v1/info). The config
// revision is the marker the operator stamps into each rendered config and
// reads back to confirm a reload landed (SPEC §8.3).
type Info struct {
	Version, Commit string
	ConfigRevision  string
}

// ReloadResult reports what a reload applied and the config revision now in
// effect (POST /api/v1/reload).
type ReloadResult struct {
	Reloaded       []string
	ConfigRevision string
}

// ClusterStatus is the cluster-wide view any node assembles from the control
// plane (GET /api/v1/cluster/status). It is the convergence and schema signal
// the rollout gate reads (SPEC §8.2).
type ClusterStatus struct {
	// Disabled is true when the node is not in cluster mode.
	Disabled bool

	// SchemaVersion is what the cluster has agreed on in etcd; BinarySchema is
	// what the queried node's binary implements. They differ while a migration
	// is pending (SPEC §8.2 Migrating).
	SchemaVersion, BinarySchema int

	// NodeCount and DiskCount are the registered topology as etcd sees it.
	NodeCount, DiskCount int

	// PlacementSkew is max minus min disk fullness across disks reporting
	// capacity — the convergence indicator (0 ≈ converged).
	PlacementSkew float64

	// RebalanceRunning reports whether a rebalance runner holds the
	// cluster-wide election.
	RebalanceRunning bool
}

// Rebalance is a node's rebalance runner snapshot (GET
// /api/v1/cluster/rebalance). RepairQueueDepth is the per-node async remainder
// backlog the operator sums as the interim repair-queue signal until fs
// exposes an aggregate (SPEC §11.2).
type Rebalance struct {
	State            string
	RepairQueueDepth int
}

// Client talks to one fs node's admin API.
type Client struct {
	api     *adminapi.Client
	timeout time.Duration
}

// Option configures a Client.
type Option func(*config)

type config struct {
	transport http.RoundTripper
	timeout   time.Duration
}

// WithTransport sets the base round tripper, so a Pool can share one transport
// (and its keep-alives) across every node's client.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *config) { c.transport = rt }
}

// WithTimeout overrides DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// New builds a Client for the admin listener at baseURL (for example
// http://<pod>.<cluster>-peers.<ns>.svc:8090), authenticating every request
// with the bearer token.
func New(baseURL, token string, opts ...Option) (*Client, error) {
	cfg := config{transport: http.DefaultTransport, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	httpClient := &http.Client{
		Transport: &bearerTransport{token: token, base: cfg.transport},
		Timeout:   cfg.timeout,
	}

	api, err := adminapi.NewClient(baseURL, adminapi.WithClient(httpClient))
	if err != nil {
		return nil, errors.Wrap(err, "build admin client")
	}

	return &Client{api: api, timeout: cfg.timeout}, nil
}

// Info reports the node's build metadata and applied config revision.
func (c *Client) Info(ctx context.Context) (Info, error) {
	info, err := c.api.GetInfo(ctx)
	if err != nil {
		return Info{}, errors.Wrap(err, "get info")
	}

	return Info{
		Version:        info.Version,
		Commit:         info.Commit,
		ConfigRevision: info.ConfigRevision.Or(""),
	}, nil
}

// Reload re-applies the node's hot-reloadable configuration and reports the
// config revision now in effect (SPEC §8.3).
func (c *Client) Reload(ctx context.Context) (ReloadResult, error) {
	res, err := c.api.ReloadConfig(ctx)
	if err != nil {
		return ReloadResult{}, errors.Wrap(err, "reload config")
	}

	return ReloadResult{
		Reloaded:       res.Reloaded,
		ConfigRevision: res.ConfigRevision.Or(""),
	}, nil
}

// ClusterStatus reads the cluster-wide status the node assembles from etcd.
func (c *Client) ClusterStatus(ctx context.Context) (ClusterStatus, error) {
	status, err := c.api.GetClusterStatus(ctx)
	if err != nil {
		return ClusterStatus{}, errors.Wrap(err, "get cluster status")
	}

	return ClusterStatus{
		Disabled:         status.State == adminapi.ClusterStateDisabled,
		SchemaVersion:    status.SchemaVersion,
		BinarySchema:     status.BinarySchemaVersion,
		NodeCount:        status.NodeCount,
		DiskCount:        status.DiskCount,
		PlacementSkew:    status.PlacementSkew,
		RebalanceRunning: status.RebalanceRunning,
	}, nil
}

// Rebalance reads the node's rebalance runner state, including its repair-queue
// depth.
func (c *Client) Rebalance(ctx context.Context) (Rebalance, error) {
	status, err := c.api.GetRebalanceStatus(ctx)
	if err != nil {
		return Rebalance{}, errors.Wrap(err, "get rebalance status")
	}

	return Rebalance{
		State:            string(status.State),
		RepairQueueDepth: status.RepairQueueDepth,
	}, nil
}

// bearerTransport injects the admin bearer token on every request. The admin
// listener authenticates with it (SPEC §9); it never leaves the operator.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrip must not modify the caller's request.
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)

	return t.base.RoundTrip(clone)
}
