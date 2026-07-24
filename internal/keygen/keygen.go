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

// Package keygen mints the secret material the operator generates: the shared
// cluster secret, admin bearer tokens and S3 credentials.
//
// Everything here comes from crypto/rand and is generated once. The operator
// creates a Secret when it is missing and never rewrites one that exists —
// rotating a peer secret partitions a cluster, and rotating a credential
// breaks whoever holds it (SPEC §8.1, §9).
package keygen

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"

	"github.com/go-faster/errors"
)

// tokenBytes is the entropy behind every generated secret. 256 bits is far
// above fs's 16-character minimum for the cluster secret and leaves no reason
// to think about brute force again.
const tokenBytes = 32

// accessKeyBytes is the entropy behind the non-secret half of a credential.
// It only identifies the key, so it stays short enough to read in a log line.
const accessKeyBytes = 9

// accessKeyPrefix marks a credential as one the operator minted, the way S3
// keys elsewhere carry a type prefix.
const accessKeyPrefix = "AK"

// Token returns a random secret: a cluster secret, an admin bearer token or
// the secret half of an S3 credential. It is URL-safe, so it survives every
// place a credential is pasted.
func Token() (string, error) {
	buf := make([]byte, tokenBytes)

	if _, err := rand.Read(buf); err != nil {
		return "", errors.Wrap(err, "read random")
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AccessKey returns the non-secret half of an S3 credential: printable,
// alphanumeric and safe in a URL, a header or a log line.
func AccessKey() (string, error) {
	buf := make([]byte, accessKeyBytes)

	if _, err := rand.Read(buf); err != nil {
		return "", errors.Wrap(err, "read random")
	}

	return accessKeyPrefix + hex.EncodeToString(buf), nil
}
