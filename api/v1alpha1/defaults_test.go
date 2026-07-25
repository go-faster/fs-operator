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

// The image and registry the table repeats: naming them keeps a change to
// either in one place.
const (
	fsImage  = DefaultImageRepository
	mirror   = "registry.internal"
	mirrored = mirror + "/go-faster/fs"
)

func TestApplyRegistry(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		registry   string
		want       string
	}{
		{
			name:       "empty registry is a no-op",
			repository: fsImage,
			registry:   "",
			want:       fsImage,
		},
		{
			name:       "empty repository is a no-op",
			repository: "",
			registry:   mirror,
			want:       "",
		},
		{
			name:       "replaces the host of the default fs image",
			repository: fsImage,
			registry:   mirror,
			want:       mirrored,
		},
		{
			name:       "replaces the host of the operator image",
			repository: fsImage + "-operator",
			registry:   mirror,
			want:       mirrored + "-operator",
		},
		{
			name:       "trailing slash on the registry is trimmed",
			repository: fsImage,
			registry:   mirror + "/",
			want:       mirrored,
		},
		{
			name:       "host with a port is treated as a registry host",
			repository: "example.com:5000/team/fs",
			registry:   mirror,
			want:       mirror + "/team/fs",
		},
		{
			name:       "digest pin is preserved after replacement",
			repository: fsImage + "@sha256:abc123",
			registry:   mirror,
			want:       mirrored + "@sha256:abc123",
		},
		{
			name:       "repository without a host segment is prepended",
			repository: "go-faster/fs",
			registry:   mirror,
			want:       mirrored,
		},
		{
			name:       "single-segment repository is prepended",
			repository: "fs",
			registry:   mirror,
			want:       mirror + "/fs",
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
