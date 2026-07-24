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

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// FSAccessKeySpec defines the desired state of an FSAccessKey: one S3
// credential of a referenced FSCluster with its bucket grants.
//
// The credential comes from exactly one of two sources: generated (the
// default — the operator mints it once and owns the Secret named by
// secretName) or imported via existingSecretRef (a user-managed Secret, e.g.
// minted by an external secret manager; the operator watches it and
// propagates rotation to the cluster with a hot reload).
// +kubebuilder:validation:XValidation:rule="!(has(self.existingSecretRef) && has(self.secretName))",message="secretName and existingSecretRef are mutually exclusive"
type FSAccessKeySpec struct {
	// clusterRef is the FSCluster this credential belongs to. Immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is immutable"
	// +required
	ClusterRef ClusterReference `json:"clusterRef"`

	// secretName names the operator-owned Secret the generated credential
	// is written to (keys: access-key, secret-key, endpoint). Defaults to
	// <metadata.name>-credentials. Immutable; not allowed together with
	// existingSecretRef.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="secretName is immutable"
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// existingSecretRef imports a credential from a user-managed Secret
	// with keys "access-key" and "secret-key" (secret-key must be at least
	// 16 characters, refused otherwise). The operator never writes to this
	// Secret; external rotation propagates to the cluster via hot reload.
	// +optional
	ExistingSecretRef *corev1.LocalObjectReference `json:"existingSecretRef,omitempty"`

	// grants authorize the key for buckets matching a glob, up to a
	// permission level.
	// +kubebuilder:validation:MinItems=1
	// +required
	Grants []GrantSpec `json:"grants"`
}

// GrantSpec authorizes an access key for buckets matching Bucket (a glob) up
// to Permission.
type GrantSpec struct {
	// bucket is a glob matched against bucket names (fs grant semantics).
	// +kubebuilder:validation:MinLength=1
	// +required
	Bucket string `json:"bucket"`

	// permission is the maximum permitted operation class.
	// +kubebuilder:validation:Enum=read;write;admin
	// +required
	Permission string `json:"permission"`
}

// FSAccessKeyStatus defines the observed state of an FSAccessKey.
type FSAccessKeyStatus struct {
	// observedGeneration is the last spec generation the controller acted
	// on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// accessKey is the non-secret half of the credential, for reference.
	// +optional
	AccessKey string `json:"accessKey,omitempty"`

	// conditions represent the current state of the FSAccessKey (Ready).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="AccessKey",type=string,JSONPath=`.status.accessKey`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FSAccessKey is the Schema for the fsaccesskeys API.
type FSAccessKey struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of FSAccessKey
	// +required
	Spec FSAccessKeySpec `json:"spec"`

	// status defines the observed state of FSAccessKey
	// +optional
	Status FSAccessKeyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FSAccessKeyList contains a list of FSAccessKey
type FSAccessKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FSAccessKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FSAccessKey{}, &FSAccessKeyList{})
		return nil
	})
}
