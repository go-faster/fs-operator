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

package fscluster

import "testing"

func TestParseScheme(t *testing.T) {
	for _, tc := range []struct {
		scheme  string
		domains int
		quorum  int
	}{
		// Two full replicas are acknowledged synchronously; the third target
		// — parity or the third replica — follows behind the async queue.
		{scheme: SchemeRF25, domains: 3, quorum: 2},
		{scheme: SchemeRF3, domains: 3, quorum: 2},
		// Erasure coding has no async path to complete a shard set, so every
		// shard is placed and acknowledged synchronously.
		{scheme: schemeEC, domains: 6, quorum: 6},
		{scheme: "ec:8,3", domains: 11, quorum: 11},
	} {
		t.Run(tc.scheme, func(t *testing.T) {
			scheme, err := ParseScheme(tc.scheme)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if got := scheme.MinDomains(); got != tc.domains {
				t.Errorf("MinDomains = %d, want %d", got, tc.domains)
			}

			if got := scheme.WriteQuorumDomains(); got != tc.quorum {
				t.Errorf("WriteQuorumDomains = %d, want %d", got, tc.quorum)
			}
		})
	}
}

func TestParseSchemeRejects(t *testing.T) {
	for _, scheme := range []string{"", "rf4", "rf2", "ec", "ec:", "ec:4", "ec:4,", "ec:0,2", "ec:4,0", "ec:x,2"} {
		t.Run(scheme, func(t *testing.T) {
			if _, err := ParseScheme(scheme); err == nil {
				t.Error("parse succeeded, want an error")
			}
		})
	}
}
