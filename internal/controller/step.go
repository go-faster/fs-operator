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
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// A reconcile pass is a sequence of steps over shared state: each one moves
// the world a little closer to the spec, and each one is idempotent, so a pass
// may stop anywhere and be replayed from the top.
//
// Two things make the sequence more than a list of function calls. A step can
// declare the pass *blocked* — the cluster is not in a state where the
// remaining work is safe or meaningful — which skips the rest without turning
// into an error; and a step can be marked always-run, so that the status of a
// blocked pass still gets written and says why.

// Outcome is what a step reports back to the pipeline.
type Outcome struct {
	// Blocked stops the pass: the steps after this one are skipped unless
	// they are marked AlwaysRun. It is not a failure — a spec the controller
	// refuses to apply, or a gate that is not open yet, is a legitimate
	// resting state.
	Blocked bool

	// Reason explains a block or a wait in the log. It is free text; what the
	// user sees are conditions and events, which the status step writes.
	Reason string

	// RequeueAfter asks for another pass later, for the progress no watch
	// will report — a gate that opens with time rather than with an event.
	// The pipeline requeues at the earliest time any step asked for.
	RequeueAfter time.Duration
}

// Continue is the outcome of a step that did its work and has nothing to say.
func Continue() (Outcome, error) {
	return Outcome{}, nil
}

// RequeueAfter is the outcome of a step waiting on something no watch reports.
func RequeueAfter(after time.Duration, reason string) (Outcome, error) {
	return Outcome{RequeueAfter: after, Reason: reason}, nil
}

// Block is the outcome of a step that will not let the pass go on.
func Block(reason string) (Outcome, error) {
	return Outcome{Blocked: true, Reason: reason}, nil
}

// FailureRecorder is implemented by pass state that wants to see the failure
// that stopped the pass. A status step that reports "reconcile failed" should
// say what failed, and only the pipeline knows.
type FailureRecorder interface {
	RecordFailure(err error)
}

// Step is one stage of a reconcile pass over T, the state the pass carries.
type Step[T any] struct {
	// Name identifies the step in logs.
	Name string

	// AlwaysRun marks a step that runs even after the pass is blocked or a
	// step failed: the status refreshers, which have to report what happened.
	AlwaysRun bool

	// Run does the step's work.
	Run func(ctx context.Context, state T) (Outcome, error)
}

// Pipeline is an ordered sequence of steps.
type Pipeline[T any] []Step[T]

// Run executes the pipeline and reports what the caller should do next.
//
// A failing step ends the mutating part of the pass — there is no point
// applying a StatefulSet whose config Secret could not be written — but the
// always-run steps still execute, so the object says what went wrong. The
// first failure is what is returned; a later one is logged.
func (p Pipeline[T]) Run(ctx context.Context, state T) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var (
		result  ctrl.Result
		stopped bool
		failure error
	)

	for _, step := range p {
		if stopped && !step.AlwaysRun {
			continue
		}

		outcome, err := step.Run(ctx, state)

		switch {
		case err != nil:
			if failure == nil {
				failure = err

				if recorder, ok := any(state).(FailureRecorder); ok {
					recorder.RecordFailure(err)
				}
			} else {
				log.Error(err, "Step failed after an earlier failure", "step", step.Name)
			}

			stopped = true
		case outcome.Blocked:
			log.Info("Reconcile blocked", "step", step.Name, "reason", outcome.Reason)

			stopped = true
		case outcome.Reason != "":
			log.V(1).Info("Step waiting", "step", step.Name, "reason", outcome.Reason)
		}

		result.RequeueAfter = soonest(result.RequeueAfter, outcome.RequeueAfter)
	}

	if failure != nil {
		// Requeueing is the error's job: controller-runtime backs off on it,
		// and a fixed delay next to that only muddies the retry schedule.
		return ctrl.Result{}, failure
	}

	return result, nil
}

// soonest picks the earlier of two requeue delays, treating zero as "no
// request" rather than "immediately".
func soonest(current, next time.Duration) time.Duration {
	if next <= 0 {
		return current
	}

	if current <= 0 || next < current {
		return next
	}

	return current
}
