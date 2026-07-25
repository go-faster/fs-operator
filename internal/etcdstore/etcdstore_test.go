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

package etcdstore

import (
	"strings"
	"testing"
)

// TestKeyRangeIsolatesSiblingClusters is the reason this helper exists. fs
// writes every key as "<prefix>/…", so a range over the bare prefix would also
// sweep up a cluster whose prefix merely starts with the same characters —
// deleting a neighbour's control plane.
func TestKeyRangeIsolatesSiblingClusters(t *testing.T) {
	const (
		cluster = "/fs/prod/app"
		sibling = "/fs/prod/app-staging"
	)

	got := KeyRange(cluster)

	if want := cluster + "/"; got != want {
		t.Fatalf("KeyRange(%q) = %q, want %q", cluster, got, want)
	}

	if strings.HasPrefix(sibling+"/nodes/a", got) {
		t.Errorf("range %q covers sibling cluster keys under %q", got, sibling)
	}

	if !strings.HasPrefix(cluster+"/nodes/a", got) {
		t.Errorf("range %q does not cover the cluster's own keys", got)
	}
}

// TestKeyRangeNormalisesTrailingSlash keeps a prefix a user spelled with a
// trailing slash from producing "//".
func TestKeyRangeNormalisesTrailingSlash(t *testing.T) {
	if got, want := KeyRange("/fs/prod/app/"), "/fs/prod/app/"; got != want {
		t.Errorf("KeyRange = %q, want %q", got, want)
	}
}

// TestDeletePrefixRefusesUnsafeInput fails before dialling: an empty or root
// prefix would range over the whole keyspace, and no endpoints cannot be a
// silent success.
func TestDeletePrefixRefusesUnsafeInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		prefix string
	}{
		{name: "no endpoints", cfg: Config{}, prefix: "/fs/prod/app"},
		{name: "empty prefix", cfg: Config{Endpoints: []string{"http://etcd:2379"}}, prefix: ""},
		{name: "root prefix", cfg: Config{Endpoints: []string{"http://etcd:2379"}}, prefix: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted, err := DeletePrefix(t.Context(), tc.cfg, tc.prefix)
			if err == nil {
				t.Fatalf("DeletePrefix(%q) succeeded, deleting %d keys", tc.prefix, deleted)
			}
		})
	}
}
