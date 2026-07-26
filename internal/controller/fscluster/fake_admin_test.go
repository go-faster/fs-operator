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
	"context"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs-operator/internal/fsclient"
	"github.com/go-faster/fs-operator/internal/scheme"
)

// fakeAdmin is an in-memory fs admin API for the controller tests, keyed by a
// node's admin base URL. It stands in for pods envtest does not run: a test
// sets what each node reports, and asserts on the reloads the operator drives.
type fakeAdmin struct {
	mu sync.Mutex

	// applied is the config revision a node's Info reports — the config it has
	// loaded. Empty until a test sets it.
	applied map[string]string

	// mounted is the config revision a Reload picks up (the config the kubelet
	// has propagated to the node's volume). Empty means the reload changes
	// nothing, modelling propagation lag.
	mounted map[string]string

	// reloads counts Reload calls per node; unreachable makes a node's admin
	// API error, modelling a node that is not up.
	reloads     map[string]int
	unreachable map[string]bool

	// rebalanceRunning and repairQueue drive the cluster's convergence: the
	// cluster is converged only with no rebalance running and an empty queue.
	// repairQueue is the pre-v0.9.0 shape — per node, over the rebalance
	// endpoint the operator falls back to when nothing serves live state.
	rebalanceRunning bool
	repairQueue      map[string]int

	// live is the per-node view a v0.9.0 cluster folds into its status, and
	// notReporting how many registered nodes did not answer. Leaving live
	// empty models a cluster whose binaries predate the peer status endpoint:
	// the status carries no aggregate, and the operator fans out instead.
	live         []fsclient.ClusterNode
	notReporting int

	// schemaVersion is what the cluster reports as agreed in etcd; binarySchema
	// is what the deployed binary implements. binarySchema > schemaVersion is a
	// pending migration.
	schemaVersion, binarySchema int

	// accessKeys are the access-key IDs every node's ListAccessKeys reports.
	accessKeys []string

	// publicRead is the cluster-wide public-read bucket list the admin serves.
	publicRead []string
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{
		applied:     map[string]string{},
		mounted:     map[string]string{},
		reloads:     map[string]int{},
		unreachable: map[string]bool{},
		repairQueue: map[string]int{},
	}
}

// client is the factory installed as Reconciler.Admin.
func (f *fakeAdmin) client(baseURL, _ string) (fsclient.Interface, error) {
	return &fakeClient{admin: f, url: baseURL}, nil
}

// setApplied makes a node report url's config revision as already loaded.
func (f *fakeAdmin) setApplied(url, revision string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.applied[url] = revision
}

// setUnreachable toggles whether url's admin API errors.
func (f *fakeAdmin) setUnreachable(url string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.unreachable[url] = down
}

// setRebalanceRunning toggles whether a rebalance is moving data cluster-wide.
func (f *fakeAdmin) setRebalanceRunning(running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.rebalanceRunning = running
}

// setRepairQueue sets a node's pending repair-queue depth.
func (f *fakeAdmin) setRepairQueue(url string, depth int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.repairQueue[url] = depth
}

// setLive makes the cluster report the v0.9.0 per-node view: these nodes
// answered the live-state fetch, and notReporting more did not.
func (f *fakeAdmin) setLive(notReporting int, nodes ...fsclient.ClusterNode) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.live, f.notReporting = nodes, notReporting
}

// liveNode builds one reporting node holding used bytes of data on a single
// disk, at the given placement weight.
func liveNode(id string, weight float64, used, total int64, repairQueue int) fsclient.ClusterNode {
	return fsclient.ClusterNode{
		ID: id,
		Disks: []fsclient.ClusterDisk{{
			ID:            "disk-0",
			Weight:        weight,
			CapacityKnown: true,
			TotalBytes:    total,
			FreeBytes:     total - used,
		}},
		Live: &fsclient.NodeLive{RepairQueueDepth: repairQueue},
	}
}

// setAccessKeys replaces what every node's ListAccessKeys reports — the
// cluster-wide key store as it stands in etcd.
func (f *fakeAdmin) setAccessKeys(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.accessKeys = keys
}

// setSchema sets the cluster-recorded and binary schema versions.
//
//nolint:unparam // binary is a meaningful axis even if the current tests all use 5.
func (f *fakeAdmin) setSchema(cluster, binary int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.schemaVersion, f.binarySchema = cluster, binary
}

// fakeClient is one node's view of the fake admin.
type fakeClient struct {
	admin *fakeAdmin
	url   string
}

func (c *fakeClient) Info(context.Context) (fsclient.Info, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.unreachable[c.url] {
		return fsclient.Info{}, errors.New("node admin unreachable")
	}

	return fsclient.Info{ConfigRevision: f.applied[c.url]}, nil
}

func (c *fakeClient) Reload(context.Context) (fsclient.ReloadResult, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.unreachable[c.url] {
		return fsclient.ReloadResult{}, errors.New("node admin unreachable")
	}

	f.reloads[c.url]++

	// A reload reads the currently mounted config; the applied revision
	// advances to it only once the kubelet has propagated the new Secret.
	if mounted, ok := f.mounted[c.url]; ok && mounted != "" {
		f.applied[c.url] = mounted
	}

	return fsclient.ReloadResult{
		ConfigRevision: f.applied[c.url],
		Reloaded:       []string{"credentials"},
	}, nil
}

func (c *fakeClient) ClusterStatus(context.Context) (fsclient.ClusterStatus, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.unreachable[c.url] {
		return fsclient.ClusterStatus{}, errors.New("node admin unreachable")
	}

	status := fsclient.ClusterStatus{
		RebalanceRunning: f.rebalanceRunning,
		SchemaVersion:    f.schemaVersion,
		BinarySchema:     f.binarySchema,
	}

	// A pre-v0.9.0 cluster reports no live state at all, and the aggregate
	// stays zero — the operator must not read that as an empty queue.
	if len(f.live) > 0 {
		status.Nodes = f.live
		status.NodesReporting = len(f.live)
		status.NodesNotReporting = f.notReporting

		for _, node := range f.live {
			if node.Live != nil {
				status.RepairQueueDepth += node.Live.RepairQueueDepth
			}
		}
	}

	return status, nil
}

func (c *fakeClient) Rebalance(context.Context) (fsclient.Rebalance, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.unreachable[c.url] {
		return fsclient.Rebalance{}, errors.New("node admin unreachable")
	}

	return fsclient.Rebalance{RepairQueueDepth: f.repairQueue[c.url]}, nil
}

func (c *fakeClient) ListAccessKeys(context.Context) ([]fsclient.AccessKey, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.unreachable[c.url] {
		return nil, errors.New("node admin unreachable")
	}

	keys := make([]fsclient.AccessKey, 0, len(f.accessKeys))
	for _, k := range f.accessKeys {
		keys = append(keys, fsclient.AccessKey{AccessKey: k})
	}

	return keys, nil
}

func (c *fakeClient) CreateAccessKey(_ context.Context, access, _ string, _ []fsclient.Grant) error {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	f.accessKeys = append(f.accessKeys, access)

	return nil
}

func (c *fakeClient) DeleteAccessKey(_ context.Context, access string) error {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	kept := f.accessKeys[:0]
	for _, k := range f.accessKeys {
		if k != access {
			kept = append(kept, k)
		}
	}

	f.accessKeys = kept

	return nil
}

func (c *fakeClient) GetPublicReadBuckets(context.Context) ([]string, error) {
	return c.admin.publicReadOf(), nil
}

// publicReadOf returns the public-read list the admin currently serves.
func (f *fakeAdmin) publicReadOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.publicRead...)
}

func (c *fakeClient) SetPublicReadBuckets(_ context.Context, buckets []string) error {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	f.publicRead = append([]string(nil), buckets...)

	return nil
}

func (c *fakeClient) GetBucketScheme(_ context.Context, _ string) (fsclient.BucketScheme, error) {
	return fsclient.BucketScheme{Scheme: scheme.RF25, ClusterDefault: scheme.RF25, IsDefault: true}, nil
}

func (c *fakeClient) SetBucketScheme(_ context.Context, _, override string) (fsclient.BucketScheme, error) {
	if override == "" {
		return fsclient.BucketScheme{Scheme: scheme.RF25, ClusterDefault: scheme.RF25, IsDefault: true}, nil
	}

	return fsclient.BucketScheme{Scheme: override, Override: override, ClusterDefault: scheme.RF25}, nil
}
