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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// readyCondition is a resource's Ready condition to be written to status. The
// FSBucket and FSAccessKey controllers build one per reconcile and hand it to
// their status writer.
type readyCondition struct {
	status  metav1.ConditionStatus
	reason  string
	message string
}

// trueCondition marks the resource Ready.
func trueCondition(reason, message string) *readyCondition {
	return &readyCondition{status: metav1.ConditionTrue, reason: reason, message: message}
}

// falseCondition marks the resource not Ready, with the reason a user acts on.
func falseCondition(reason, message string) *readyCondition {
	return &readyCondition{status: metav1.ConditionFalse, reason: reason, message: message}
}
