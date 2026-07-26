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

	// TotalBytes and FreeBytes sum the capacity every disk reports.
	TotalBytes, FreeBytes int64

	// RepairQueueDepth is pending async replication/repair work summed over
	// the nodes that answered — the cluster-wide signal fs v0.9.0 added, which
	// replaces fanning out over each node's rebalance endpoint (SPEC §11.2).
	//
	// It counts only reporting nodes, so read it together with
	// NodesNotReporting: a zero depth with nodes silent is "nobody said
	// otherwise", not "nothing left to do".
	RepairQueueDepth int

	// NodesReporting and NodesNotReporting split the topology by whether the
	// node answered the live-state request. A node is silent when it is
	// unreachable or runs a binary older than the peer status endpoint, so a
	// rolling upgrade degrades to "not reporting" rather than to an error.
	NodesReporting, NodesNotReporting int

	// Nodes is the per-node view: the registered topology, each node's disks
	// and, where the node answered, its live runtime state.
	Nodes []ClusterNode
}

// AllNodesReporting is true when every node in the topology answered the
// live-state request. The gates that must not mistake silence for quiescence —
// convergence, and the drain check that decides a node may be deleted — require
// it before trusting RepairQueueDepth or a node's live state.
func (s ClusterStatus) AllNodesReporting() bool {
	return s.NodesNotReporting == 0 && s.NodesReporting > 0
}

// Node returns the status of one node by its fs node ID.
func (s ClusterStatus) Node(id string) (ClusterNode, bool) {
	for _, node := range s.Nodes {
		if node.ID == id {
			return node, true
		}
	}

	return ClusterNode{}, false
}

// ClusterNode is one registered node as the control plane sees it.
type ClusterNode struct {
	// ID is the fs node ID, which the operator derives from the node's name
	// (SPEC §4.2), and Rack its failure-domain label.
	ID, Rack string

	// Disks is what the node registered, with the capacity it last republished.
	Disks []ClusterDisk

	// Live is the node's own runtime state, nil when it did not answer.
	Live *NodeLive

	// LiveError says why Live is missing: unreachable, or a binary that does
	// not serve live state.
	LiveError string
}

// Empty reports whether every one of the node's disks is known to hold no
// data — the gate a decommission opens on, before deleting the node and its
// volumes (SPEC §8.4).
//
// A node with no disks reported, or any disk whose occupancy is unknown, is not
// empty: the question is whether anything might still be there, and silence is
// not an answer.
func (n ClusterNode) Empty() bool {
	if len(n.Disks) == 0 {
		return false
	}

	for _, disk := range n.Disks {
		if !disk.Empty() {
			return false
		}
	}

	return true
}

// UsedBytes sums the used capacity of the node's disks, and reports whether
// every disk answered with capacity. Bytes are the human-readable progress of a
// drain, not its completion test — they come from statfs, so they include
// filesystem overhead and never reach zero on an emptied disk. Empty() is what
// says the data is gone.
func (n ClusterNode) UsedBytes() (int64, bool) {
	var (
		used  int64
		known = len(n.Disks) > 0
	)

	for _, disk := range n.Disks {
		if !disk.CapacityKnown {
			known = false
			continue
		}

		used += disk.UsedBytes()
	}

	return used, known
}

// ClusterDisk is one disk of one node.
type ClusterDisk struct {
	// ID is the disk ID within its node.
	ID string

	// Weight is the placement weight the node registered. Not positive means
	// the disk is out of placement — how a drain is expressed (see
	// fscluster.DrainWeight).
	Weight float64

	// TotalBytes, FreeBytes and Fullness (the used fraction, 0..1) are the
	// node's last statfs report; CapacityKnown is false when it has not
	// reported any, and the three are then meaningless.
	TotalBytes, FreeBytes int64
	Fullness              float64
	CapacityKnown         bool

	// HasData reports whether the disk still holds any fragment, and
	// OccupancyKnown whether the node answered at all. This is occupancy, not
	// placement: Drained() says the disk takes no new data, Empty() says its
	// data has finished moving off. A decommission needs both, in that order.
	HasData        bool
	OccupancyKnown bool

	// DataError is why the node could not probe the disk, when it could not.
	DataError string
}

// Empty reports whether the disk is known to hold no data — the signal that a
// drain has finished and the volume can be deleted (fs v0.10.0 `has_data`).
//
// It is false when the answer is unknown: a node that did not report, a disk
// the node could not read, or a cluster running a binary that predates the
// field. That is the safe direction — a decommission that holds is recoverable,
// one that deletes a disk still holding the only copy of something is not.
func (d ClusterDisk) Empty() bool {
	return d.OccupancyKnown && !d.HasData
}

// UsedBytes is the disk's used capacity, zero when it reported none.
func (d ClusterDisk) UsedBytes() int64 {
	if !d.CapacityKnown {
		return 0
	}

	return d.TotalBytes - d.FreeBytes
}

// Drained reports whether the disk is out of placement.
func (d ClusterDisk) Drained() bool {
	return d.Weight <= 0
}

// NodeLive is a node's own runtime state, fetched over the peer transport and
// folded into the cluster status. It is process-local, so it keeps answering
// during an etcd outage.
type NodeLive struct {
	// Version is the node's binary version and SchemaVersion the schema that
	// binary implements — a half-upgraded cluster shows mixed values.
	Version       string
	SchemaVersion int

	// RepairQueueDepth is the objects with pending async replication or repair
	// work on this node.
	RepairQueueDepth int

	// RebalanceState is the node's rebalance runner state, and RebalanceError
	// why it failed, if it did.
	RebalanceState string
	RebalanceError string
}

// Rebalance is a node's rebalance runner snapshot (GET
// /api/v1/cluster/rebalance). RepairQueueDepth is the per-node async remainder
// backlog the operator sums as the interim repair-queue signal until fs
// exposes an aggregate (SPEC §11.2).
type Rebalance struct {
	State            string
	RepairQueueDepth int
}

// Grant authorises a key for buckets matching Bucket (a glob) up to Permission
// ("read", "write" or "admin").
type Grant struct {
	Bucket     string
	Permission string
}

// AccessKey is one credential the cluster accepts (GET /api/v1/access-keys),
// secret omitted. The operator lists these to reconcile the declarative
// FSAccessKeys against the cluster's etcd-backed key store (SPEC §7).
type AccessKey struct {
	AccessKey string
	Grants    []Grant
}

// ErrKeyExists is returned by CreateAccessKey when the access key already
// exists (HTTP 409).
var ErrKeyExists = errors.New("access key already exists")

// BucketScheme is a bucket's effective replication scheme and whether it
// overrides the cluster default (GET/PUT /api/v1/buckets/{bucket}/scheme).
type BucketScheme struct {
	// Scheme is the effective scheme: the override when set, else the default.
	Scheme string
	// Override is the explicit override; empty when following the default.
	Override string
	// ClusterDefault is the scheme applied to buckets without an override.
	ClusterDefault string
	// IsDefault reports whether the bucket follows the cluster default.
	IsDefault bool
}

// DiskWeightOverride is one disk's placement-weight override as the cluster
// stores it (fs §11.6).
type DiskWeightOverride struct {
	Node, Disk string
	Weight     float64
	Reason     string
}

// Drained reports whether the override takes the disk out of placement.
func (o DiskWeightOverride) Drained() bool { return o.Weight <= 0 }

// ListDiskWeights returns every placement-weight override the cluster holds.
func (c *Client) ListDiskWeights(ctx context.Context) ([]DiskWeightOverride, error) {
	list, err := c.api.ListDiskWeights(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list disk weights")
	}

	out := make([]DiskWeightOverride, 0, len(list.Overrides))
	for _, o := range list.Overrides {
		out = append(out, DiskWeightOverride{
			Node: o.Node, Disk: o.Disk, Weight: o.Weight, Reason: o.Reason.Or(""),
		})
	}

	return out, nil
}

// SetDiskWeight overrides one disk's placement weight until it is cleared.
//
// A weight that is not positive drains the disk: no new data is placed on it
// and the rebalancer moves what it holds elsewhere. The override outlives the
// node restarting, which is what makes it usable as a decommission step.
func (c *Client) SetDiskWeight(ctx context.Context, node, disk string, weight float64, reason string) error {
	req := &adminapi.SetDiskWeightRequest{Weight: weight}
	if reason != "" {
		req.Reason = adminapi.NewOptString(reason)
	}

	if _, err := c.api.SetDiskWeight(ctx, req, adminapi.SetDiskWeightParams{
		Node: node, Disk: disk,
	}); err != nil {
		return errors.Wrapf(err, "set weight of disk %q on node %q", disk, node)
	}

	return nil
}

// ClearDiskWeight restores the weight a node registers from its config.
func (c *Client) ClearDiskWeight(ctx context.Context, node, disk string) error {
	if err := c.api.ClearDiskWeight(ctx, adminapi.ClearDiskWeightParams{
		Node: node, Disk: disk,
	}); err != nil {
		return errors.Wrapf(err, "clear weight of disk %q on node %q", disk, node)
	}

	return nil
}

// ErrBucketNotFound is returned by the bucket-scheme calls when the cluster has
// no such bucket (HTTP 404).
var ErrBucketNotFound = errors.New("bucket not found")

// ErrSchemeRejected is returned by SetBucketScheme when the cluster refuses the
// scheme (HTTP 400): an invalid form, or a topology that cannot host it.
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

// ClusterStatus reads the cluster-wide status the node assembles from etcd.
func (c *Client) ClusterStatus(ctx context.Context) (ClusterStatus, error) {
	status, err := c.api.GetClusterStatus(ctx)
	if err != nil {
		return ClusterStatus{}, errors.Wrap(err, "get cluster status")
	}

	return ClusterStatus{
		Disabled:          status.State == adminapi.ClusterStateDisabled,
		SchemaVersion:     status.SchemaVersion,
		BinarySchema:      status.BinarySchemaVersion,
		NodeCount:         status.NodeCount,
		DiskCount:         status.DiskCount,
		PlacementSkew:     status.PlacementSkew,
		RebalanceRunning:  status.RebalanceRunning,
		TotalBytes:        status.TotalBytes,
		FreeBytes:         status.FreeBytes,
		RepairQueueDepth:  status.RepairQueueDepth,
		NodesReporting:    status.NodesReporting,
		NodesNotReporting: status.NodesNotReporting,
		Nodes:             clusterNodesFromAPI(status.Nodes),
	}, nil
}

// clusterNodesFromAPI maps the wire types to the plain structs.
func clusterNodesFromAPI(nodes []adminapi.ClusterNode) []ClusterNode {
	if len(nodes) == 0 {
		return nil
	}

	out := make([]ClusterNode, 0, len(nodes))

	for _, node := range nodes {
		mapped := ClusterNode{
			ID:        node.ID,
			Rack:      node.Rack.Or(""),
			LiveError: node.LiveError.Or(""),
		}

		if len(node.Disks) > 0 {
			mapped.Disks = make([]ClusterDisk, 0, len(node.Disks))

			for _, disk := range node.Disks {
				total, hasTotal := disk.TotalBytes.Get()
				free, hasFree := disk.FreeBytes.Get()
				hasData, hasOccupancy := disk.HasData.Get()

				mapped.Disks = append(mapped.Disks, ClusterDisk{
					ID:     disk.ID,
					Weight: disk.Weight,
					// fs omits has_data rather than zeroing it when the node
					// did not report or could not probe the disk, and the
					// operator keeps that distinction intact: absent is
					// unknown, and unknown is not empty.
					HasData:        hasData,
					OccupancyKnown: hasOccupancy,
					DataError:      disk.DataError.Or(""),
					// Capacity is reported as a set or not at all: a disk whose
					// node has not republished statfs carries neither figure,
					// and a size without a free figure says nothing about use.
					CapacityKnown: hasTotal && hasFree,
					TotalBytes:    total,
					FreeBytes:     free,
					Fullness:      disk.Fullness.Or(0),
				})
			}
		}

		if live, ok := node.Live.Get(); ok {
			mapped.Live = &NodeLive{
				Version:          live.Version.Or(""),
				SchemaVersion:    live.SchemaVersion.Or(0),
				RepairQueueDepth: live.RepairQueueDepth,
				RebalanceState:   string(live.RebalanceState),
				RebalanceError:   live.RebalanceError.Or(""),
			}
		}

		out = append(out, mapped)
	}

	return out
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

// ListAccessKeys returns every credential the cluster accepts, secrets omitted.
func (c *Client) ListAccessKeys(ctx context.Context) ([]AccessKey, error) {
	list, err := c.api.ListAccessKeys(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list access keys")
	}

	out := make([]AccessKey, 0, len(list.Keys))
	for _, k := range list.Keys {
		out = append(out, AccessKey{AccessKey: k.AccessKey, Grants: grantsFromAPI(k.Grants)})
	}

	return out, nil
}

// CreateAccessKey adds a credential to the cluster's key store with the given
// material and grants (POST /api/v1/access-keys). With auth.source: etcd it is
// cluster-wide and hot-reloaded on every node. Returns ErrKeyExists on 409.
func (c *Client) CreateAccessKey(ctx context.Context, access, secret string, grants []Grant) error {
	_, err := c.api.CreateAccessKey(ctx, &adminapi.CreateAccessKeyRequest{
		AccessKey: adminapi.NewOptString(access),
		SecretKey: adminapi.NewOptString(secret),
		Grants:    grantsToAPI(grants),
	})
	if err != nil {
		var status *adminapi.ErrorStatusCode
		if errors.As(err, &status) && status.StatusCode == http.StatusConflict {
			return errors.Wrap(ErrKeyExists, access)
		}

		return errors.Wrap(err, "create access key")
	}

	return nil
}

// DeleteAccessKey removes a credential from the cluster's key store. A missing
// key is treated as already deleted (idempotent).
func (c *Client) DeleteAccessKey(ctx context.Context, access string) error {
	err := c.api.DeleteAccessKey(ctx, adminapi.DeleteAccessKeyParams{AccessKey: access})
	if err != nil {
		var status *adminapi.ErrorStatusCode
		if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
			return nil
		}

		return errors.Wrap(err, "delete access key")
	}

	return nil
}

// GetPublicReadBuckets returns the cluster-wide anonymously-readable bucket
// list.
func (c *Client) GetPublicReadBuckets(ctx context.Context) ([]string, error) {
	res, err := c.api.GetPublicReadBuckets(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "get public-read buckets")
	}

	return res.Buckets, nil
}

// SetPublicReadBuckets replaces the cluster-wide public-read list (an empty
// slice clears it).
func (c *Client) SetPublicReadBuckets(ctx context.Context, buckets []string) error {
	if buckets == nil {
		buckets = []string{}
	}

	_, err := c.api.SetPublicReadBuckets(ctx, &adminapi.SetPublicReadBucketsRequest{Buckets: buckets})
	if err != nil {
		return errors.Wrap(err, "set public-read buckets")
	}

	return nil
}

// grantsToAPI/grantsFromAPI convert between the plain and generated grant types.
func grantsToAPI(grants []Grant) []adminapi.Grant {
	out := make([]adminapi.Grant, 0, len(grants))
	for _, g := range grants {
		out = append(out, adminapi.Grant{Bucket: g.Bucket, Permission: adminapi.Permission(g.Permission)})
	}

	return out
}

func grantsFromAPI(grants []adminapi.Grant) []Grant {
	out := make([]Grant, 0, len(grants))
	for _, g := range grants {
		out = append(out, Grant{Bucket: g.Bucket, Permission: string(g.Permission)})
	}

	return out
}

// GetBucketScheme reads a bucket's effective replication scheme and override.
func (c *Client) GetBucketScheme(ctx context.Context, bucket string) (BucketScheme, error) {
	res, err := c.api.GetBucketScheme(ctx, adminapi.GetBucketSchemeParams{Bucket: bucket})
	if err != nil {
		return BucketScheme{}, mapSchemeError(err, "get bucket scheme")
	}

	return bucketSchemeFromAPI(res), nil
}

// SetBucketScheme sets a bucket's scheme override, or clears it when scheme is
// empty, and returns the effective scheme after applying (SPEC §11.3).
func (c *Client) SetBucketScheme(ctx context.Context, bucket, scheme string) (BucketScheme, error) {
	req := &adminapi.SetBucketSchemeRequest{Scheme: adminapi.NewOptString(scheme)}

	res, err := c.api.SetBucketScheme(ctx, req, adminapi.SetBucketSchemeParams{Bucket: bucket})
	if err != nil {
		return BucketScheme{}, mapSchemeError(err, "set bucket scheme")
	}

	return bucketSchemeFromAPI(res), nil
}

// bucketSchemeFromAPI maps the wire type to the plain struct.
func bucketSchemeFromAPI(s *adminapi.BucketScheme) BucketScheme {
	return BucketScheme{
		Scheme:         s.Scheme,
		Override:       s.Override.Or(""),
		ClusterDefault: s.ClusterDefault,
		IsDefault:      s.IsDefault,
	}
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
