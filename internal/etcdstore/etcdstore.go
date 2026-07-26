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
	"crypto/tls"
	"crypto/x509"
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

// Config is how to reach a cluster's etcd.
type Config struct {
	// Endpoints are the etcd client URLs.
	Endpoints []string

	// Security is the TLS material and credentials, if the cluster's etcd
	// needs them.
	Security Security
}

// Security is the client TLS material and credentials for reaching etcd.
//
// The certificates are bytes rather than paths on purpose: the nodes read
// theirs from a mounted Secret, but the operator reaches etcd from its own
// process, where the only thing it has is what it read out of the API.
type Security struct {
	// CA is the PEM bundle etcd's certificate is verified against. Empty
	// verifies against the system roots.
	CA []byte

	// Cert and Key are this client's certificate for mutual TLS. Both or
	// neither.
	Cert, Key []byte

	// ServerName overrides the name verified against etcd's certificate.
	ServerName string

	// InsecureSkipVerify disables verification entirely.
	InsecureSkipVerify bool

	// Username and Password are etcd role-based credentials.
	Username, Password string
}

// enabled reports whether any TLS material was configured.
func (s Security) enabled() bool {
	return len(s.CA) > 0 || len(s.Cert) > 0 || s.ServerName != "" || s.InsecureSkipVerify
}

// tlsConfig builds the transport, or nil for a plaintext connection.
//
// An https endpoint enables TLS on its own, for the reason fs does the same:
// the etcd client takes the transport from this value and ignores the URL
// scheme, so an https endpoint with a nil config would connect in the clear.
func (c Config) tlsConfig() (*tls.Config, error) {
	if !c.Security.enabled() && !hasTLSEndpoint(c.Endpoints) {
		return nil, nil //nolint:nilnil // No TLS configured is a valid answer.
	}

	out := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         c.Security.ServerName,
		InsecureSkipVerify: c.Security.InsecureSkipVerify, //nolint:gosec // Opt-in, documented as development-only.
	}

	if len(c.Security.CA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(c.Security.CA) {
			return nil, errors.New("etcd CA bundle contains no certificates")
		}

		out.RootCAs = pool
	}

	if len(c.Security.Cert) > 0 {
		cert, err := tls.X509KeyPair(c.Security.Cert, c.Security.Key)
		if err != nil {
			return nil, errors.Wrap(err, "load etcd client certificate")
		}

		out.Certificates = []tls.Certificate{cert}
	}

	return out, nil
}

// hasTLSEndpoint reports whether any endpoint is an https URL.
func hasTLSEndpoint(endpoints []string) bool {
	for _, endpoint := range endpoints {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(endpoint)), "https://") {
			return true
		}
	}

	return false
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

	tlsCfg, err := cfg.tlsConfig()
	if err != nil {
		return 0, err
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		DialTimeout: DialTimeout,
		Context:     ctx,
		TLS:         tlsCfg,
		Username:    cfg.Security.Username,
		Password:    cfg.Security.Password,
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
