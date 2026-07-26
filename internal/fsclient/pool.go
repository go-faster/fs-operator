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

package fsclient

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Interface is the admin surface the controllers depend on. It is what a
// reconcile step accepts, so tests substitute a fake without a live node.
type Interface interface {
	Info(ctx context.Context) (Info, error)
	Reload(ctx context.Context) (ReloadResult, error)
	ClusterStatus(ctx context.Context) (ClusterStatus, error)
	Rebalance(ctx context.Context) (Rebalance, error)
	ListAccessKeys(ctx context.Context) ([]AccessKey, error)
	CreateAccessKey(ctx context.Context, access, secret string, grants []Grant) error
	DeleteAccessKey(ctx context.Context, access string) error
	GetPublicReadBuckets(ctx context.Context) ([]string, error)
	SetPublicReadBuckets(ctx context.Context, buckets []string) error
	ListDiskWeights(ctx context.Context) ([]DiskWeightOverride, error)
	SetDiskWeight(ctx context.Context, node, disk string, weight float64, reason string) error
	ClearDiskWeight(ctx context.Context, node, disk string) error
	GetBucketScheme(ctx context.Context, bucket string) (BucketScheme, error)
	SetBucketScheme(ctx context.Context, bucket, scheme string) (BucketScheme, error)
}

var _ Interface = (*Client)(nil)

// Pool caches admin clients keyed by endpoint and token, so repeated
// reconciles reuse connections instead of dialing each node afresh (SPEC
// §4.2). A rotated token keys a new client, so a stale one is never reused; in
// v1alpha1 the admin token is generated once and does not rotate.
type Pool struct {
	// transport is shared by every client, so keep-alives to a node survive
	// across the per-node clients a pass builds.
	transport http.RoundTripper
	timeout   time.Duration

	mu      sync.Mutex
	clients map[clientKey]*Client
}

// clientKey identifies a cached client. The token is part of the key so a
// rotation cannot hand back a client authenticating with the old one.
type clientKey struct {
	baseURL string
	token   string
}

// PoolOption configures a Pool.
type PoolOption func(*Pool)

// WithPoolTimeout sets the per-request timeout of the pool's clients.
func WithPoolTimeout(d time.Duration) PoolOption {
	return func(p *Pool) { p.timeout = d }
}

// NewPool builds a Pool. Its clients share one HTTP transport for connection
// reuse.
func NewPool(opts ...PoolOption) *Pool {
	p := &Pool{
		transport: cloneDefaultTransport(),
		timeout:   DefaultTimeout,
		clients:   make(map[clientKey]*Client),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// Client returns the cached admin client for a node's endpoint and token,
// building it on first use.
func (p *Pool) Client(baseURL, token string) (*Client, error) {
	key := clientKey{baseURL: baseURL, token: token}

	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clients[key]; ok {
		return client, nil
	}

	client, err := New(baseURL, token, WithTransport(p.transport), WithTimeout(p.timeout))
	if err != nil {
		return nil, err
	}

	p.clients[key] = client

	return client, nil
}

// cloneDefaultTransport returns a fresh transport so the pool never mutates
// http.DefaultTransport, which the whole process shares.
func cloneDefaultTransport() http.RoundTripper {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		return dt.Clone()
	}

	return http.DefaultTransport
}
