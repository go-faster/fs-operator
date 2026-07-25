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

// Package etcdstore is the operator's direct access to the control plane a
// cluster registers in. fs owns everything under a cluster's etcd prefix while
// it runs; the operator only reaches in when the cluster is gone and its keys
// have to go with it (SPEC §8.6).
package etcdstore

import (
	"context"
	"strings"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// DialTimeout bounds the initial connection to etcd; RequestTimeout bounds one
// call. Deletion happens on a finalizer, which retries, so neither has to be
// generous — an unreachable etcd should report quickly rather than hold a
// reconcile worker.
const (
	DialTimeout    = 10 * time.Second
	RequestTimeout = 15 * time.Second
)

// Config is how to reach a cluster's etcd. It carries only what
// FSCluster.spec.etcd.external offers today; client TLS and authentication
// arrive with fs §11.4.
type Config struct {
	// Endpoints are the etcd client URLs.
	Endpoints []string
}

// DeletePrefix removes every key of one cluster and reports how many it
// deleted.
//
// The range is prefix + "/", never prefix itself, and that separator is the
// whole point: fs writes each key as "<prefix>/nodes/…", so a plain prefix
// range over "/fs/ns/app" would also sweep up "/fs/ns/app-staging", a
// different cluster that merely shares a string prefix. Deleting a
// neighbour's control plane is precisely the accident this package exists to
// avoid.
func DeletePrefix(ctx context.Context, cfg Config, prefix string) (int64, error) {
	if len(cfg.Endpoints) == 0 {
		return 0, errors.New("no etcd endpoints")
	}

	if strings.Trim(prefix, "/") == "" {
		// An empty or root prefix would range over the whole keyspace.
		return 0, errors.Errorf("refusing to delete etcd prefix %q", prefix)
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		DialTimeout: DialTimeout,
		Context:     ctx,
	})
	if err != nil {
		return 0, errors.Wrap(err, "connect to etcd")
	}

	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	res, err := client.Delete(ctx, KeyRange(prefix), clientv3.WithPrefix())
	if err != nil {
		return 0, errors.Wrap(err, "delete prefix")
	}

	return res.Deleted, nil
}

// KeyRange is the prefix every key of a cluster starts with: the configured
// prefix with exactly one trailing separator.
func KeyRange(prefix string) string {
	return strings.TrimSuffix(prefix, "/") + "/"
}
