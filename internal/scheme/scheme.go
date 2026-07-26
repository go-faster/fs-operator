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

import (
	"strconv"
	"strings"

	"github.com/go-faster/errors"
)

// Scheme is a parsed replication scheme. It mirrors fs's own
// internal/cluster/scheme: the operator has to answer the same two questions
// fs answers — how many failure domains a scheme needs to be placeable, and
// how many have to be alive for a write to be acknowledged — before it lets a
// spec through or calls a cluster ready.
type Scheme struct {
	// EC reports whether this is an erasure-coded scheme rather than a
	// replicated one.
	EC bool

	// Data and Parity are the k and m of an erasure-coded scheme.
	Data, Parity int
}

// Scheme names in their config and CLI form.
const (
	RF25 = "rf2.5"
	RF3  = "rf3"

	// ecPrefix introduces an erasure-coded scheme: "ec:k,m".
	ecPrefix = "ec:"
)

// replicaSchemeCopies is how many targets a replicated scheme places: two full
// replicas plus either parity (rf2.5) or a third replica (rf3).
const replicaSchemeCopies = 3

// replicaSchemeWriteQuorum is how many full replicas must be durable before fs
// acknowledges a write; the third target is produced behind the async queue.
const replicaSchemeWriteQuorum = 2

// Parse reads a scheme from the form the API and the config file use:
// "rf2.5", "rf3" or "ec:k,m".
func Parse(s string) (Scheme, error) {
	switch s {
	case RF25, RF3:
		return Scheme{}, nil
	}

	spec, ok := strings.CutPrefix(s, ecPrefix)
	if !ok {
		return Scheme{}, errors.Errorf("invalid scheme %q (want rf2.5, rf3 or ec:k,m)", s)
	}

	data, parity, ok := strings.Cut(spec, ",")
	if !ok {
		return Scheme{}, errors.Errorf("invalid scheme %q (want ec:k,m)", s)
	}

	k, err := strconv.Atoi(data)
	if err != nil || k < 1 {
		return Scheme{}, errors.Errorf("invalid scheme %q: k must be a positive number", s)
	}

	m, err := strconv.Atoi(parity)
	if err != nil || m < 1 {
		return Scheme{}, errors.Errorf("invalid scheme %q: m must be a positive number", s)
	}

	return Scheme{EC: true, Data: k, Parity: m}, nil
}

// MinDomains is how many distinct failure domains the scheme places copies on,
// and therefore the smallest topology that can host it.
func (s Scheme) MinDomains() int {
	if s.EC {
		return s.Data + s.Parity
	}

	return replicaSchemeCopies
}

// WriteQuorumDomains is how many failure domains must be serving before a
// write can be acknowledged: two full replicas for the replicated schemes, all
// k+m shards for erasure coding, which has no async path to complete a shard
// set safely.
func (s Scheme) WriteQuorumDomains() int {
	if s.EC {
		return s.Data + s.Parity
	}

	return replicaSchemeWriteQuorum
}
