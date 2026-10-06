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
	"slices"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs-operator/internal/fsclient"
)

// fakeAdmin is an in-memory fs admin API for the controller tests, keyed by a
// node's admin base URL. It stands in for pods envtest does not run: a test
// sets what each node reports, and asserts on the reloads and layouts the
// operator drives.
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

	// layout is the cluster's layout, nil before the first apply; applies
	// records every layout the operator applied, in order.
	layout  *fsclient.Layout
	applies []fsclient.Layout

	// transition makes every apply start a transition: the previous version
	// stays retained until a test calls finishTransition. rejectLayout makes
	// every apply fail as fs refusing the roles.
	transition   bool
	rejectLayout bool

	// up is the gossip view, by fs node ID: whether each node answered its
	// last exchange. behind holds nodes still on an older layout version.
	up     map[string]bool
	behind map[string]bool
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{
		applied:     map[string]string{},
		mounted:     map[string]string{},
		reloads:     map[string]int{},
		unreachable: map[string]bool{},
		up:          map[string]bool{},
		behind:      map[string]bool{},
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

// setUp marks nodes up (or down) in the gossip view.
func (f *fakeAdmin) setUp(up bool, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, id := range ids {
		f.up[id] = up
	}
}

// setLayout installs a layout as though applied earlier, giving each node the
// capacity of the test clusters' volumes.
func (f *fakeAdmin) setLayout(version uint64, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	layout := fsclient.Layout{Version: version, Widths: []int{3}}
	for _, id := range ids {
		layout.Members = append(layout.Members, fsclient.Role{ID: id, Capacity: testCapacity})
	}

	f.layout = &layout
}

// startTransition retains the current layout's previous version, as a layout
// change that is still moving data does.
func (f *fakeAdmin) startTransition() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.layout.Retained = []uint64{f.layout.Version - 1}
}

// finishTransition retires every retained version.
func (f *fakeAdmin) finishTransition() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.layout != nil {
		f.layout.Retained = nil
	}
}

// appliedLayouts returns the layouts the operator applied, in order.
func (f *fakeAdmin) appliedLayouts() []fsclient.Layout {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.applies)
}

// fakeClient is one node's view of the fake admin.
type fakeClient struct {
	admin *fakeAdmin
	url   string
}

// down reports whether this node's admin API errors; f.mu must be held.
func (c *fakeClient) down() error {
	if c.admin.unreachable[c.url] {
		return errors.New("node admin unreachable")
	}

	return nil
}

func (c *fakeClient) Info(context.Context) (fsclient.Info, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := c.down(); err != nil {
		return fsclient.Info{}, err
	}

	return fsclient.Info{ConfigRevision: f.applied[c.url]}, nil
}

func (c *fakeClient) Reload(context.Context) (fsclient.ReloadResult, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := c.down(); err != nil {
		return fsclient.ReloadResult{}, err
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

func (c *fakeClient) Layout(context.Context) (fsclient.Layout, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := c.down(); err != nil {
		return fsclient.Layout{}, err
	}

	if f.layout == nil {
		return fsclient.Layout{}, fsclient.ErrNoLayout
	}

	return cloneLayout(*f.layout), nil
}

func (c *fakeClient) ApplyLayout(_ context.Context, roles []fsclient.Role, widths []int) (fsclient.Layout, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := c.down(); err != nil {
		return fsclient.Layout{}, err
	}

	if f.rejectLayout {
		return fsclient.Layout{}, errors.Wrap(fsclient.ErrLayoutRejected, "fewer members with capacity than width")
	}

	next := fsclient.Layout{Version: 1, Members: slices.Clone(roles), Widths: slices.Clone(widths)}
	if f.layout != nil {
		next.Version = f.layout.Version + 1

		if f.transition {
			next.Retained = []uint64{f.layout.Version}
		}
	}

	f.layout = &next
	f.applies = append(f.applies, cloneLayout(next))

	return cloneLayout(next), nil
}

func (c *fakeClient) Nodes(context.Context) ([]fsclient.Node, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := c.down(); err != nil {
		return nil, err
	}

	var version uint64
	if f.layout != nil {
		version = f.layout.Version
	}

	nodes := make([]fsclient.Node, 0, len(f.up))
	for id, up := range f.up {
		node := fsclient.Node{ID: id, Up: up, LayoutVersion: version, SyncedVersion: version}
		if f.behind[id] && version > 0 {
			node.LayoutVersion, node.SyncedVersion = version-1, version-1
		}

		nodes = append(nodes, node)
	}

	return nodes, nil
}

func (c *fakeClient) AccessKeys(context.Context) ([]string, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := c.down(); err != nil {
		return nil, err
	}

	return nil, nil
}

func (c *fakeClient) GetBucketScheme(context.Context, string) (string, error) {
	return "rf3", nil
}

func (c *fakeClient) SetBucketScheme(_ context.Context, _, scheme string) (string, error) {
	return scheme, nil
}

func cloneLayout(l fsclient.Layout) fsclient.Layout {
	l.Members = slices.Clone(l.Members)
	l.Widths = slices.Clone(l.Widths)
	l.Retained = slices.Clone(l.Retained)

	return l
}
