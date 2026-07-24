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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-faster/errors"
)

// record is the state a test pipeline carries: the names of the steps that
// ran, in order.
type record struct {
	ran []string
}

// step builds a step that records itself and returns a fixed outcome.
func step(name string, outcome func() (Outcome, error)) Step[*record] {
	return Step[*record]{
		Name: name,
		Run: func(_ context.Context, state *record) (Outcome, error) {
			state.ran = append(state.ran, name)

			return outcome()
		},
	}
}

// alwaysRun marks a step as one that runs even after the pass stopped.
func alwaysRun(s Step[*record]) Step[*record] {
	s.AlwaysRun = true

	return s
}

func TestPipelineRunsEveryStep(t *testing.T) {
	state := &record{}

	result, err := Pipeline[*record]{
		step("secrets", Continue),
		step("services", Continue),
		step("status", Continue),
	}.Run(t.Context(), state)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, want := strings.Join(state.ran, ","), "secrets,services,status"; got != want {
		t.Errorf("ran %q, want %q", got, want)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("requeue after %s, want no requeue", result.RequeueAfter)
	}
}

// TestPipelineBlockSkipsMutations is the property the design rests on: a
// blocked pass stops changing the world but still reports why.
func TestPipelineBlockSkipsMutations(t *testing.T) {
	state := &record{}

	result, err := Pipeline[*record]{
		step("validate", func() (Outcome, error) { return Block("SchemeTopologyMismatch") }),
		step("secrets", Continue),
		alwaysRun(step("status", Continue)),
	}.Run(t.Context(), state)
	if err != nil {
		t.Fatalf("a block is not a failure, got: %v", err)
	}

	if got, want := strings.Join(state.ran, ","), "validate,status"; got != want {
		t.Errorf("ran %q, want %q", got, want)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("requeue after %s, want the block to rest until something changes", result.RequeueAfter)
	}
}

func TestPipelineFailureStopsMutationsAndSurfaces(t *testing.T) {
	state := &record{}
	first := errors.New("apply config secret")

	_, err := Pipeline[*record]{
		step("secrets", func() (Outcome, error) { return Outcome{}, first }),
		step("nodes", func() (Outcome, error) { return Outcome{}, errors.New("second") }),
		alwaysRun(step("status", Continue)),
	}.Run(t.Context(), state)

	if !errors.Is(err, first) {
		t.Errorf("returned %v, want the first failure %v", err, first)
	}

	if got, want := strings.Join(state.ran, ","), "secrets,status"; got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
}

// TestPipelineFailingAlwaysRunStepSurfaces covers the case where the only
// failure comes from a step that runs after the pass stopped.
func TestPipelineFailingAlwaysRunStepSurfaces(t *testing.T) {
	failure := errors.New("update status")

	_, err := Pipeline[*record]{
		step("validate", func() (Outcome, error) { return Block("SpecInvalid") }),
		alwaysRun(step("status", func() (Outcome, error) { return Outcome{}, failure })),
	}.Run(t.Context(), &record{})

	if !errors.Is(err, failure) {
		t.Errorf("returned %v, want %v", err, failure)
	}
}

func TestPipelineRequeuesAtTheEarliestRequest(t *testing.T) {
	result, err := Pipeline[*record]{
		step("nodes", func() (Outcome, error) { return RequeueAfter(time.Minute, "pod not ready") }),
		step("health", func() (Outcome, error) { return RequeueAfter(10*time.Second, "polling") }),
		step("status", Continue),
	}.Run(t.Context(), &record{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, want := result.RequeueAfter, 10*time.Second; got != want {
		t.Errorf("requeue after %s, want the earliest request %s", got, want)
	}
}

// TestPipelineDoesNotRequeueOnFailure keeps the retry schedule in one place:
// controller-runtime backs off on the error itself.
func TestPipelineDoesNotRequeueOnFailure(t *testing.T) {
	result, err := Pipeline[*record]{
		step("nodes", func() (Outcome, error) { return RequeueAfter(time.Minute, "waiting") }),
		alwaysRun(step("status", func() (Outcome, error) { return Outcome{}, errors.New("boom") })),
	}.Run(t.Context(), &record{})

	if err == nil {
		t.Fatal("run succeeded, want the failure")
	}

	if result.RequeueAfter != 0 {
		t.Errorf("requeue after %s, want the error to drive the retry", result.RequeueAfter)
	}
}
