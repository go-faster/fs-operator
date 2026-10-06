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

// ConditionType is the type of a status condition. The condition and reason
// vocabulary below is documented API surface: dashboards and automations may
// key off it.
type ConditionType = string

// ConditionReason explains a condition's status.
type ConditionReason = string

// Condition types shared by all resources.
const (
	// ConditionSpecValid indicates the resource passes controller-side
	// cross-field validation (checks CEL cannot express).
	ConditionSpecValid ConditionType = "SpecValid"

	// ConditionReconcileSucceeded indicates the last reconcile pass
	// completed without error.
	ConditionReconcileSucceeded ConditionType = "ReconcileSucceeded"

	// ConditionReady indicates the resource is fully functional: an
	// FSCluster serves S3 at write quorum; an FSBucket exists; an
	// FSAccessKey is accepted by every node.
	ConditionReady ConditionType = "Ready"
)

// Reasons shared by all resources.
const (
	ReasonSpecValid         ConditionReason = "SpecValid"
	ReasonSpecInvalid       ConditionReason = "SpecInvalid"
	ReasonReconcileFinished ConditionReason = "ReconcileFinished"
	ReasonReconcileError    ConditionReason = "ReconcileError"
)

// FSCluster condition types.
const (
	// ConditionNodesHealthy indicates every node pod is Ready and up in the
	// cluster's own view.
	ConditionNodesHealthy ConditionType = "NodesHealthy"

	// ConditionClusterSizeAligned indicates the actual node set and the
	// layout's membership match the declared topology (False while scaling
	// or removing nodes).
	ConditionClusterSizeAligned ConditionType = "ClusterSizeAligned"

	// ConditionConfigurationInSync indicates every node runs the desired
	// configuration revision (restart or verified hot reload).
	ConditionConfigurationInSync ConditionType = "ConfigurationInSync"

	// ConditionConverged indicates every node is up on the current layout
	// and no layout change is in transition; rolling changes gate on it
	// between nodes.
	ConditionConverged ConditionType = "Converged"
)

// FSCluster condition reasons.
const (
	ReasonLayoutTopologyMismatch ConditionReason = "LayoutTopologyMismatch"
	ReasonUnsupportedTopology    ConditionReason = "UnsupportedTopology"
	ReasonStorageShrinkForbidden ConditionReason = "StorageShrinkForbidden"
	ReasonStorageExpanding       ConditionReason = "StorageExpanding"
	ReasonAllNodesReady          ConditionReason = "AllNodesReady"
	ReasonNodesNotReady          ConditionReason = "NodesNotReady"
	ReasonQuorumAvailable        ConditionReason = "QuorumAvailable"
	ReasonQuorumUnavailable      ConditionReason = "QuorumUnavailable"
	ReasonUpToDate               ConditionReason = "UpToDate"
	ReasonScalingUp              ConditionReason = "ScalingUp"
	ReasonDraining               ConditionReason = "Draining"
	ReasonRollingNodes           ConditionReason = "RollingNodes"
	ReasonConfigReloadPending    ConditionReason = "ConfigReloadPending"
	ReasonConverged              ConditionReason = "Converged"
	ReasonConvergenceTimeout     ConditionReason = "ConvergenceTimeout"

	// ReasonLayoutPending means the layout does not yet describe the
	// declared nodes: the operator is waiting for them to be reachable
	// before it applies one that does.
	ReasonLayoutPending ConditionReason = "LayoutPending"

	// ReasonLayoutRejected means fs refused the layout the spec asks for,
	// e.g. a width wider than the nodes that can hold it.
	ReasonLayoutRejected ConditionReason = "LayoutRejected"

	// ReasonLayoutTransition means a layout change is moving data: older
	// versions are retained until every node has synced the new one.
	ReasonLayoutTransition ConditionReason = "LayoutTransition"
)

// FSBucket condition reasons.
const (
	ReasonBucketReady     ConditionReason = "BucketReady"
	ReasonBucketNotEmpty  ConditionReason = "BucketNotEmpty"
	ReasonClusterNotFound ConditionReason = "ClusterNotFound"
	ReasonClusterNotReady ConditionReason = "ClusterNotReady"
	ReasonSchemeRejected  ConditionReason = "SchemeRejected"
	ReasonBucketError     ConditionReason = "BucketError"
)

// FSAccessKey condition reasons.
const (
	ReasonKeyAccepted    ConditionReason = "KeyAccepted"
	ReasonWeakSecretKey  ConditionReason = "WeakSecretKey"
	ReasonSecretNotFound ConditionReason = "SecretNotFound"
	ReasonSecretInvalid  ConditionReason = "SecretInvalid"
)
