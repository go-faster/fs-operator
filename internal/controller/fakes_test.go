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

package controller

import (
	"context"
	"slices"
	"sync"

	"github.com/go-faster/errors"
	"github.com/minio/minio-go/v7"

	"github.com/go-faster/fs-operator/internal/fsclient"
)

// fakeAdmin is a shared in-memory admin API for the controller tests. It models
// what each node accepts — keyed by the node's admin URL, since every node keeps
// its own key set and the FSCluster controller is what renders the same keys
// into all of them — and the per-bucket schemes.
type fakeAdmin struct {
	mu sync.Mutex

	// accepted maps an access key to the admin URLs of the nodes that accept
	// it; allNodes stands for every node.
	accepted map[string][]string
	// down holds admin URLs that do not answer.
	down []string

	schemes map[string]string
	// rejectScheme, when set, makes SetBucketScheme reject that scheme value.
	rejectScheme string
}

// allNodes is the accepted entry for a key every node accepts.
const allNodes = "*"

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{accepted: map[string][]string{}, schemes: map[string]string{}}
}

// client returns an fsclient.Interface for one node of the fake cluster.
func (f *fakeAdmin) client(baseURL, _ string) (fsclient.Interface, error) {
	return &fakeAdminClient{admin: f, url: baseURL}, nil
}

// accept makes the nodes at urls accept a key — every node when none is
// given — the way a rendered config and a reload do.
func (f *fakeAdmin) accept(access string, urls ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(urls) == 0 {
		urls = []string{allNodes}
	}

	f.accepted[access] = urls
}

// revoke makes every node drop a key.
func (f *fakeAdmin) revoke(access string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.accepted, access)
}

type fakeAdminClient struct {
	admin *fakeAdmin
	url   string
}

func (c *fakeAdminClient) Info(context.Context) (fsclient.Info, error) {
	return fsclient.Info{}, nil
}

func (c *fakeAdminClient) Reload(context.Context) (fsclient.ReloadResult, error) {
	return fsclient.ReloadResult{}, nil
}

func (c *fakeAdminClient) Layout(context.Context) (fsclient.Layout, error) {
	return fsclient.Layout{}, fsclient.ErrNoLayout
}

func (c *fakeAdminClient) ApplyLayout(context.Context, []fsclient.Role, []int) (fsclient.Layout, error) {
	return fsclient.Layout{}, errors.New("the tenancy controllers never apply a layout")
}

func (c *fakeAdminClient) Nodes(context.Context) ([]fsclient.Node, error) {
	return nil, nil
}

func (c *fakeAdminClient) AccessKeys(context.Context) ([]string, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if slices.Contains(f.down, c.url) {
		return nil, errors.New("connection refused")
	}

	var keys []string

	for access, urls := range f.accepted {
		if slices.Contains(urls, allNodes) || slices.Contains(urls, c.url) {
			keys = append(keys, access)
		}
	}

	return keys, nil
}

func (c *fakeAdminClient) GetBucketScheme(_ context.Context, bucket string) (string, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if scheme, ok := f.schemes[bucket]; ok {
		return scheme, nil
	}

	return "rf3", nil
}

func (c *fakeAdminClient) SetBucketScheme(_ context.Context, bucket, scheme string) (string, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if scheme == f.rejectScheme {
		return "", errors.Wrap(fsclient.ErrSchemeRejected, "the layout has no width for "+scheme)
	}

	f.schemes[bucket] = scheme

	return scheme, nil
}

// fakeS3 is an in-memory S3 backend for the bucket controller tests.
type fakeS3 struct {
	mu       sync.Mutex
	buckets  map[string]bool
	nonEmpty map[string]bool

	// existsErr, when set, is what BucketExists fails with — the shape of an
	// S3 endpoint that answers but refuses the operator's credential.
	existsErr error
}

func newFakeS3() *fakeS3 {
	return &fakeS3{buckets: map[string]bool{}, nonEmpty: map[string]bool{}}
}

// factory returns an S3 factory closed over this backend.
func (s *fakeS3) factory() func(endpoint, access, secret string, secure bool) (S3Client, error) {
	return func(_, _, _ string, _ bool) (S3Client, error) { return s, nil }
}

func (s *fakeS3) BucketExists(_ context.Context, bucket string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.existsErr != nil {
		return false, s.existsErr
	}

	return s.buckets[bucket], nil
}

func (s *fakeS3) MakeBucket(_ context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.buckets[bucket] = true

	return nil
}

func (s *fakeS3) RemoveBucket(_ context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nonEmpty[bucket] {
		return minio.ErrorResponse{Code: "BucketNotEmpty"}
	}

	delete(s.buckets, bucket)

	return nil
}
