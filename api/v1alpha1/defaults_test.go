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

package v1alpha1

import "testing"

func TestApplyRegistry(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		registry   string
		want       string
	}{
		{
			name:       "empty registry is a no-op",
			repository: "ghcr.io/go-faster/fs",
			registry:   "",
			want:       "ghcr.io/go-faster/fs",
		},
		{
			name:       "empty repository is a no-op",
			repository: "",
			registry:   "registry.internal",
			want:       "",
		},
		{
			name:       "replaces the host of the default fs image",
			repository: "ghcr.io/go-faster/fs",
			registry:   "registry.internal",
			want:       "registry.internal/go-faster/fs",
		},
		{
			name:       "replaces the host of the operator image",
			repository: "ghcr.io/go-faster/fs-operator",
			registry:   "registry.internal",
			want:       "registry.internal/go-faster/fs-operator",
		},
		{
			name:       "trailing slash on the registry is trimmed",
			repository: "ghcr.io/go-faster/fs",
			registry:   "registry.internal/",
			want:       "registry.internal/go-faster/fs",
		},
		{
			name:       "host with a port is treated as a registry host",
			repository: "example.com:5000/team/fs",
			registry:   "registry.internal",
			want:       "registry.internal/team/fs",
		},
		{
			name:       "digest pin is preserved after replacement",
			repository: "ghcr.io/go-faster/fs@sha256:abc123",
			registry:   "registry.internal",
			want:       "registry.internal/go-faster/fs@sha256:abc123",
		},
		{
			name:       "repository without a host segment is prepended",
			repository: "go-faster/fs",
			registry:   "registry.internal",
			want:       "registry.internal/go-faster/fs",
		},
		{
			name:       "single-segment repository is prepended",
			repository: "fs",
			registry:   "registry.internal",
			want:       "registry.internal/fs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ApplyRegistry(tt.repository, tt.registry); got != tt.want {
				t.Errorf("ApplyRegistry(%q, %q) = %q, want %q", tt.repository, tt.registry, got, tt.want)
			}
		})
	}
}
