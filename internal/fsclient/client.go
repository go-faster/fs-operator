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
// configuration a node has applied, which layout the cluster runs and whether
// it has finished moving data, and to apply a layout or a hot reload (SPEC
// §4.2, §8.2, §8.3).
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

// Role is one node's place in a layout: the failure domains it sits in and
// the capacity it holds data in proportion to.
type Role struct {
	ID, Zone, Rack string
	Capacity       uint64
}

// Layout is the cluster layout a node has adopted (GET /api/v1/cluster/layout).
type Layout struct {
	Version uint64
	Widths  []int
	Members []Role

	// Retained are older versions still in transition, oldest first: the
	// cluster is moving data to where Version puts it. Empty when nothing is
	// moving.
	Retained []uint64
}

// Transitioning reports whether a layout change is still moving data.
func (l Layout) Transitioning() bool { return len(l.Retained) > 0 }

// Member returns the role of one node, if the layout gives it one.
func (l Layout) Member(id string) (Role, bool) {
	for _, m := range l.Members {
		if m.ID == id {
			return m, true
		}
	}

	return Role{}, false
}

// Node is one node as the queried node sees it over gossip (GET
// /api/v1/cluster/nodes).
type Node struct {
	ID, Addr string
	Self, Up bool

	// LayoutVersion is the newest layout the node last reported, and
	// SyncedVersion the newest it has finished syncing.
	LayoutVersion, SyncedVersion uint64

	// Error is why the last exchange with the node failed, when it did.
	Error string
}

// ErrNoLayout is returned by Layout before any layout has been applied (HTTP
// 404): the cluster holds no data and serves no S3 request yet.
var ErrNoLayout = errors.New("no layout applied")

// ErrLayoutRejected is returned by ApplyLayout when fs cannot build a layout
// from the roles (HTTP 400), e.g. a width wider than the nodes that hold data.
var ErrLayoutRejected = errors.New("layout rejected")

// ErrBucketNotFound is returned by the bucket-scheme calls when the cluster has
// no such bucket (HTTP 404).
var ErrBucketNotFound = errors.New("bucket not found")

// ErrSchemeRejected is returned by SetBucketScheme when the cluster refuses the
// scheme (HTTP 400): an invalid form, a layout without the scheme's width, or
// a single node.
var ErrSchemeRejected = errors.New("scheme rejected")

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

// Layout reads the layout the node has adopted.
func (c *Client) Layout(ctx context.Context) (Layout, error) {
	l, err := c.api.GetLayout(ctx)
	if err != nil {
		var status *adminapi.ErrorStatusCode
		if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
			return Layout{}, ErrNoLayout
		}

		return Layout{}, errors.Wrap(err, "get layout")
	}

	return layoutFromAPI(l), nil
}

// ApplyLayout computes the next layout from the full set of roles and adopts
// it; gossip carries it to every other node. It returns the layout now in
// effect.
func (c *Client) ApplyLayout(ctx context.Context, roles []Role, widths []int) (Layout, error) {
	req := &adminapi.ApplyLayoutRequest{Widths: widths}
	for _, r := range roles {
		req.Members = append(req.Members, adminapi.LayoutRole{
			ID:       r.ID,
			Zone:     optString(r.Zone),
			Rack:     optString(r.Rack),
			Capacity: r.Capacity,
		})
	}

	change, err := c.api.ApplyLayout(ctx, req, adminapi.ApplyLayoutParams{})
	if err != nil {
		var status *adminapi.ErrorStatusCode
		if errors.As(err, &status) && status.StatusCode == http.StatusBadRequest {
			return Layout{}, errors.Wrap(ErrLayoutRejected, status.Response.ErrorMessage)
		}

		return Layout{}, errors.Wrap(err, "apply layout")
	}

	return layoutFromAPI(&change.Layout), nil
}

// Nodes lists the queried node and every peer it gossips with.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	list, err := c.api.ListClusterNodes(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list cluster nodes")
	}

	nodes := make([]Node, 0, len(list.Nodes))
	for _, n := range list.Nodes {
		nodes = append(nodes, Node{
			ID:            n.ID.Or(""),
			Addr:          n.Addr,
			Self:          n.Self,
			Up:            n.Up,
			LayoutVersion: n.LayoutVersion.Or(0),
			SyncedVersion: n.SyncedVersion.Or(0),
			Error:         n.Error.Or(""),
		})
	}

	return nodes, nil
}

func layoutFromAPI(l *adminapi.Layout) Layout {
	out := Layout{
		Version:  l.Version,
		Widths:   l.Widths,
		Retained: l.RetainedVersions,
	}
	for _, m := range l.Members {
		out.Members = append(out.Members, Role{
			ID:       m.ID,
			Zone:     m.Zone.Or(""),
			Rack:     m.Rack.Or(""),
			Capacity: m.Capacity,
		})
	}

	return out
}

// optString leaves an empty string unset rather than sending "".
func optString(s string) adminapi.OptString {
	if s == "" {
		return adminapi.OptString{}
	}

	return adminapi.NewOptString(s)
}

// AccessKeys lists the access key IDs the node accepts: config-defined and
// runtime-created alike, secrets omitted.
func (c *Client) AccessKeys(ctx context.Context) ([]string, error) {
	keys, err := c.api.ListAccessKeys(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list access keys")
	}

	ids := make([]string, 0, len(keys.Keys))
	for _, k := range keys.Keys {
		ids = append(ids, k.AccessKey)
	}

	return ids, nil
}

// GetBucketScheme reads how a bucket's new data is stored: "rf3" or "ec:K,M".
func (c *Client) GetBucketScheme(ctx context.Context, bucket string) (string, error) {
	res, err := c.api.GetBucketScheme(ctx, adminapi.GetBucketSchemeParams{Bucket: bucket})
	if err != nil {
		return "", mapSchemeError(err, "get bucket scheme")
	}

	return res.Scheme, nil
}

// SetBucketScheme sets how a bucket's data is stored from now on and returns
// the scheme in effect (SPEC §11.3).
func (c *Client) SetBucketScheme(ctx context.Context, bucket, scheme string) (string, error) {
	res, err := c.api.SetBucketScheme(ctx, &adminapi.BucketScheme{Scheme: scheme},
		adminapi.SetBucketSchemeParams{Bucket: bucket})
	if err != nil {
		return "", mapSchemeError(err, "set bucket scheme")
	}

	return res.Scheme, nil
}

// mapSchemeError translates the admin API's structured errors to the package's
// sentinels so a caller can tell a rejected scheme (permanent) or a missing
// bucket from a transient failure worth retrying.
func mapSchemeError(err error, op string) error {
	var status *adminapi.ErrorStatusCode
	if errors.As(err, &status) {
		switch status.StatusCode {
		case http.StatusBadRequest:
			return errors.Wrap(ErrSchemeRejected, status.Response.ErrorMessage)
		case http.StatusNotFound:
			return errors.Wrap(ErrBucketNotFound, status.Response.ErrorMessage)
		}
	}

	return errors.Wrap(err, op)
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
