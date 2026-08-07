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

package scheme

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		scheme string
		copies int
		quorum int
	}{
		// Two full replicas are acknowledged synchronously; the third target
		// — parity or the third replica — follows behind the async queue.
		{scheme: RF25, copies: 3, quorum: 2},
		{scheme: RF3, copies: 3, quorum: 2},
		// Erasure coding has no async path to complete a shard set, so every
		// shard is placed and acknowledged synchronously.
		{scheme: "ec:4,2", copies: 6, quorum: 6},
		{scheme: "ec:8,3", copies: 11, quorum: 11},
	} {
		t.Run(tc.scheme, func(t *testing.T) {
			scheme, err := Parse(tc.scheme)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if got := scheme.Copies(); got != tc.copies {
				t.Errorf("Copies = %d, want %d", got, tc.copies)
			}

			if got := scheme.WriteQuorumDomains(); got != tc.quorum {
				t.Errorf("WriteQuorumDomains = %d, want %d", got, tc.quorum)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	for _, scheme := range []string{"", "rf4", "rf2", "ec", "ec:", "ec:4", "ec:4,", "ec:0,2", "ec:4,0", "ec:x,2"} {
		t.Run(scheme, func(t *testing.T) {
			if _, err := Parse(scheme); err == nil {
				t.Error("parse succeeded, want an error")
			}
		})
	}
}
