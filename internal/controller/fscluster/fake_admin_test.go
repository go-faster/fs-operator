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
	rebalanceRunning bool
	repairQueue      map[string]int

	// schemaVersion is what the cluster reports as agreed in etcd; binarySchema
	// is what the deployed binary implements. binarySchema > schemaVersion is a
	// pending migration.
	schemaVersion, binarySchema int

	// accessKeys are the access-key IDs every node's ListAccessKeys reports.
	accessKeys []string
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

// setMounted makes a Reload on url adopt revision (the propagated config).
func (f *fakeAdmin) setMounted(url, revision string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mounted[url] = revision
}

// setUnreachable toggles whether url's admin API errors.
func (f *fakeAdmin) setUnreachable(url string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.unreachable[url] = down
}

// reloadCount reports how many reloads a node has seen.
func (f *fakeAdmin) reloadCount(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reloads[url]
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

	return fsclient.ClusterStatus{
		RebalanceRunning: f.rebalanceRunning,
		SchemaVersion:    f.schemaVersion,
		BinarySchema:     f.binarySchema,
	}, nil
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

func (c *fakeClient) GetBucketScheme(_ context.Context, _ string) (fsclient.BucketScheme, error) {
	return fsclient.BucketScheme{Scheme: SchemeRF25, ClusterDefault: SchemeRF25, IsDefault: true}, nil
}

func (c *fakeClient) SetBucketScheme(_ context.Context, _, scheme string) (fsclient.BucketScheme, error) {
	if scheme == "" {
		return fsclient.BucketScheme{Scheme: SchemeRF25, ClusterDefault: SchemeRF25, IsDefault: true}, nil
	}

	return fsclient.BucketScheme{Scheme: scheme, Override: scheme, ClusterDefault: SchemeRF25}, nil
}
