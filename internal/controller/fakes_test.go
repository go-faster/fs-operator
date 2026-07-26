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

	"github.com/go-faster/fs-operator/internal/fsclient"
	"github.com/go-faster/fs-operator/internal/scheme"
)

// fakeAdmin is a shared in-memory admin API for the controller tests. It models
// the cluster's etcd-backed key store (access key -> grants), the per-bucket
// scheme overrides and the public-read list.
type fakeAdmin struct {
	mu         sync.Mutex
	keys       map[string][]fsclient.Grant
	schemes    map[string]string
	publicRead []string
	// rejectScheme, when set, makes SetBucketScheme reject that scheme value.
	rejectScheme string
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{keys: map[string][]fsclient.Grant{}, schemes: map[string]string{}}
}

// client returns an fsclient.Interface backed by this admin, ignoring the
// endpoint and token (the tests drive one logical cluster).
func (f *fakeAdmin) client(_, _ string) (fsclient.Interface, error) {
	return &fakeAdminClient{admin: f}, nil
}

// hasKey reports whether the store holds the access key.
func (f *fakeAdmin) hasKey(access string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.keys[access]

	return ok
}

// grantsOf returns the grants recorded for an access key.
func (f *fakeAdmin) grantsOf(access string) []fsclient.Grant {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.keys[access]
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

	keys := make([]fsclient.AccessKey, 0, len(f.keys))
	for k, grants := range f.keys {
		keys = append(keys, fsclient.AccessKey{AccessKey: k, Grants: grants})
	}

	return keys, nil
}

func (c *fakeAdminClient) CreateAccessKey(_ context.Context, access, _ string, grants []fsclient.Grant) error {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.keys[access]; ok {
		return errors.Wrap(fsclient.ErrKeyExists, access)
	}

	f.keys[access] = grants

	return nil
}

func (c *fakeAdminClient) DeleteAccessKey(_ context.Context, access string) error {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.keys, access)

	return nil
}

func (c *fakeAdminClient) GetPublicReadBuckets(context.Context) ([]string, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.publicRead...), nil
}

func (c *fakeAdminClient) SetPublicReadBuckets(_ context.Context, buckets []string) error {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	f.publicRead = append([]string(nil), buckets...)

	return nil
}

func (c *fakeAdminClient) GetBucketScheme(_ context.Context, bucket string) (fsclient.BucketScheme, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	return schemeResult(f.schemes[bucket]), nil
}

func (c *fakeAdminClient) SetBucketScheme(_ context.Context, bucket, override string) (fsclient.BucketScheme, error) {
	f := c.admin

	f.mu.Lock()
	defer f.mu.Unlock()

	if override != "" && override == f.rejectScheme {
		return fsclient.BucketScheme{}, errors.Wrap(fsclient.ErrSchemeRejected, "topology cannot host scheme "+override)
	}

	f.schemes[bucket] = override

	return schemeResult(override), nil
}

func schemeResult(override string) fsclient.BucketScheme {
	if override == "" {
		return fsclient.BucketScheme{Scheme: scheme.RF25, ClusterDefault: scheme.RF25, IsDefault: true}
	}

	return fsclient.BucketScheme{Scheme: override, Override: override, ClusterDefault: scheme.RF25}
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

// Disk weight overrides are cluster state the tenancy controllers never touch.
func (c *fakeAdminClient) SetDiskWeight(context.Context, string, string, float64, string) error {
	return nil
}

func (c *fakeAdminClient) ClearDiskWeight(context.Context, string, string) error { return nil }

func (c *fakeAdminClient) ListDiskWeights(context.Context) ([]fsclient.DiskWeightOverride, error) {
	return nil, nil
}
