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

package main

import (
	"slices"
	"testing"
)

func TestParseNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		list string
		want []string
	}{
		{name: "empty watches everything", list: "", want: nil},
		{name: "only separators watch everything", list: " , ,", want: nil},
		{name: "one", list: "fs-system", want: []string{"fs-system"}},
		{name: "several", list: "b,a", want: []string{"a", "b"}},
		{name: "spaces are trimmed", list: " a , b ", want: []string{"a", "b"}},
		{name: "repeats collapse", list: "a,b,a", want: []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNamespaces(tc.list); !slices.Equal(got, tc.want) {
				t.Errorf("parseNamespaces(%q) = %v, want %v", tc.list, got, tc.want)
			}
		})
	}
}

// TestNamespaceCacheScopesWatches is the fix for a chart value that produced a
// running, Ready operator that reconciled nothing: rbac.namespaced rendered a
// namespaced Role while the manager still opened cluster-wide watches, which
// the API server refused.
func TestNamespaceCacheScopesWatches(t *testing.T) {
	// No namespaces means every namespace — the supported deployment, and the
	// default. An empty DefaultNamespaces map would mean the opposite.
	if opts := namespaceCache(nil); opts.DefaultNamespaces != nil {
		t.Errorf("DefaultNamespaces = %v, want nil so every namespace is watched", opts.DefaultNamespaces)
	}

	opts := namespaceCache([]string{"a", "b"})

	if len(opts.DefaultNamespaces) != 2 {
		t.Fatalf("DefaultNamespaces = %v, want exactly the two namespaces given", opts.DefaultNamespaces)
	}

	for _, name := range []string{"a", "b"} {
		if _, ok := opts.DefaultNamespaces[name]; !ok {
			t.Errorf("namespace %q is not watched", name)
		}
	}
}
