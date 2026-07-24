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

package keygen

import (
	"strings"
	"testing"
)

// fsMinSecretLength is the shortest cluster secret fs accepts.
const fsMinSecretLength = 16

func TestToken(t *testing.T) {
	seen := make(map[string]bool)

	for range 100 {
		token, err := Token()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}

		if len(token) < fsMinSecretLength {
			t.Fatalf("token %q is shorter than the %d characters fs requires", token, fsMinSecretLength)
		}

		if strings.ContainsAny(token, "+/= ") {
			t.Fatalf("token %q is not URL-safe", token)
		}

		if seen[token] {
			t.Fatalf("token %q was minted twice", token)
		}

		seen[token] = true
	}
}

func TestAccessKey(t *testing.T) {
	seen := make(map[string]bool)

	for range 100 {
		key, err := AccessKey()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}

		if !strings.HasPrefix(key, accessKeyPrefix) {
			t.Fatalf("access key %q does not carry the %q prefix", key, accessKeyPrefix)
		}

		for _, r := range key {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				continue
			}

			t.Fatalf("access key %q contains %q, which is not alphanumeric", key, r)
		}

		if seen[key] {
			t.Fatalf("access key %q was minted twice", key)
		}

		seen[key] = true
	}
}
