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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterReference points at an FSCluster in the same namespace (the
// namespace is the tenancy boundary; cross-namespace references are not
// supported).
type ClusterReference struct {
	// name of the FSCluster.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`
}

// FSBucketSpec defines the desired state of an FSBucket: an S3 bucket in a
// referenced FSCluster.
type FSBucketSpec struct {
	// clusterRef is the FSCluster this bucket lives in. Immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is immutable"
	// +required
	ClusterRef ClusterReference `json:"clusterRef"`

	// bucketName is the S3 bucket name; defaults to metadata.name.
	// Immutable.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bucketName is immutable"
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// scheme overrides the cluster's default replication scheme for this
	// bucket's objects: "rf2.5", "rf3" or "ec:k,m" (e.g. "ec:4,2"). Empty
	// applies the cluster default. Changing it affects new writes cluster-wide
	// within seconds; existing objects convert through repair/rebalance.
	// +kubebuilder:validation:Pattern=`^(rf2\.5|rf3|ec:[0-9]+,[0-9]+)?$`
	// +optional
	Scheme string `json:"scheme,omitempty"`

	// reclaimPolicy controls what happens to the bucket when this resource
	// is deleted. Retain leaves the bucket and its data; Delete removes the
	// bucket, which succeeds only once it is empty (the controller retries
	// and reports Ready=False/BucketNotEmpty until then).
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

// FSBucketStatus defines the observed state of an FSBucket.
type FSBucketStatus struct {
	// observedGeneration is the last spec generation the controller acted
	// on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// scheme is the bucket's effective replication scheme.
	// +optional
	Scheme string `json:"scheme,omitempty"`

	// conditions represent the current state of the FSBucket (Ready).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FSBucket is the Schema for the fsbuckets API.
type FSBucket struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of FSBucket
	// +required
	Spec FSBucketSpec `json:"spec"`

	// status defines the observed state of FSBucket
	// +optional
	Status FSBucketStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FSBucketList contains a list of FSBucket
type FSBucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FSBucket `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FSBucket{}, &FSBucketList{})
		return nil
	})
}
