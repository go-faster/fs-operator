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
	"sync"

	"github.com/go-faster/errors"
	"github.com/minio/minio-go/v7"

	"github.com/go-faster/fs-operator/internal/controller/fscluster"
	"github.com/go-faster/fs-operator/internal/fsclient"
)

// fakeAdmin is a shared in-memory admin API for the controller tests. It tracks
// the access keys the cluster accepts and the per-bucket scheme overrides.
type fakeAdmin struct {
	mu         sync.Mutex
	accessKeys map[string]bool
	schemes    map[string]string
	// rejectScheme, when set, makes SetBucketScheme reject that scheme value.
	rejectScheme string
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{accessKeys: map[string]bool{}, schemes: map[string]string{}}
}

// client returns an fsclient.Interface backed by this admin, ignoring the
// endpoint and token (the tests drive one logical cluster).
func (f *fakeAdmin) client(_, _ string) (fsclient.Interface, error) {
	return &fakeAdminClient{admin: f}, nil
}

func (f *fakeAdmin) addKey(access string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.accessKeys[access] = true
}

type fakeAdminClient struct{ admin *fakeAdmin }

func (c *fakeAdminClient) Info(context.Context) (fsclient.Info, error) {
	return fsclient.Info{}, nil
}

func (c *fakeAdminClient) Reload(context.Context) (fsclient.ReloadResult, error) {
	return fsclient.ReloadResult{}, nil
}

func (c *fakeAdminClient) ClusterStatus(context.Context) (fsclient.ClusterStatus, error) {
	return fsclient.ClusterStatus{}, nil
}

func (c *fakeAdminClient) Rebalance(context.Context) (fsclient.Rebalance, error) {
	return fsclient.Rebalance{}, nil
}

func (c *fakeAdminClient) ListAccessKeys(context.Context) ([]fsclient.AccessKey, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	keys := make([]fsclient.AccessKey, 0, len(f.accessKeys))
	for k := range f.accessKeys {
		keys = append(keys, fsclient.AccessKey{AccessKey: k})
	}

	return keys, nil
}

func (c *fakeAdminClient) GetBucketScheme(_ context.Context, bucket string) (fsclient.BucketScheme, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	return schemeResult(f.schemes[bucket]), nil
}

func (c *fakeAdminClient) SetBucketScheme(_ context.Context, bucket, scheme string) (fsclient.BucketScheme, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if scheme != "" && scheme == f.rejectScheme {
		return fsclient.BucketScheme{}, errors.Wrap(fsclient.ErrSchemeRejected, "topology cannot host scheme "+scheme)
	}

	f.schemes[bucket] = scheme

	return schemeResult(scheme), nil
}

func schemeResult(override string) fsclient.BucketScheme {
	if override == "" {
		return fsclient.BucketScheme{Scheme: fscluster.SchemeRF25, ClusterDefault: fscluster.SchemeRF25, IsDefault: true}
	}

	return fsclient.BucketScheme{Scheme: override, Override: override, ClusterDefault: fscluster.SchemeRF25}
}

// fakeS3 is an in-memory S3 backend for the bucket controller tests.
type fakeS3 struct {
	mu       sync.Mutex
	buckets  map[string]bool
	nonEmpty map[string]bool
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
