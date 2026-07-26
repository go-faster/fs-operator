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

// Package fscluster renders and reconciles the Kubernetes resources of one
// FSCluster: the generated Secrets, one config Secret and one single-pod
// StatefulSet per fs node, the peer and client Services and the disruption
// budget (SPEC §8.1).
package fscluster

import (
	"fmt"
	"strings"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// The identity scheme of SPEC §4.2. These names are API surface in practice —
// users reference the Services and Secrets — so they are computed in one place
// and never spelled out again.

// PeersServiceName is the headless Service that gives every node's pod the
// stable DNS name peers dial.
func PeersServiceName(cluster string) string {
	return cluster + "-peers"
}

// ClientServiceName is the Service in front of the cluster's S3 listeners.
func ClientServiceName(cluster string) string {
	return cluster
}

// ConfigSecretName is the Secret holding one node's rendered config.yaml.
func ConfigSecretName(node string) string {
	return node + "-config"
}

// ClusterSecretName is the Secret holding the shared peer-auth secret.
func ClusterSecretName(cluster string) string {
	return cluster + "-cluster-secret"
}

// AdminTokenSecretName is the Secret holding the admin API bearer token.
func AdminTokenSecretName(cluster string) string {
	return cluster + "-admin-token"
}

// RootCredentialsSecretName is the Secret holding the root S3 credential.
func RootCredentialsSecretName(cluster string) string {
	return cluster + "-root-credentials"
}

// EtcdStatefulSetName is the managed etcd's StatefulSet, and EtcdServiceName
// the headless Service that gives its members stable DNS.
func EtcdStatefulSetName(cluster string) string {
	return cluster + "-etcd"
}

func EtcdServiceName(cluster string) string {
	return cluster + "-etcd"
}

// EtcdEndpoints is where this cluster's nodes reach etcd: the endpoints the
// user gave, or the managed members' stable DNS names.
//
// Every reader goes through this. Reading spec.etcd.external.endpoints
// directly is how a managed cluster would end up configured with no control
// plane at all.
func EtcdEndpoints(cluster *fsv1alpha1.FSCluster) []string {
	if external := cluster.Spec.Etcd.External; external != nil {
		return external.Endpoints
	}

	replicas := cluster.Spec.EtcdReplicas()
	endpoints := make([]string, 0, replicas)

	// Per-member DNS rather than the Service name: the etcd client balances
	// across the endpoints it is given and fails over between them, which a
	// single round-robin Service name cannot do.
	for i := range int(replicas) {
		endpoints = append(endpoints, fmt.Sprintf("http://%s.%s.%s.svc:%d",
			EtcdPodName(cluster.Name, i), EtcdServiceName(cluster.Name),
			cluster.Namespace, fsv1alpha1.EtcdClientPort))
	}

	return endpoints
}

// EtcdPodName is one managed etcd member's pod.
func EtcdPodName(cluster string, ordinal int) string {
	return fmt.Sprintf("%s-%d", EtcdStatefulSetName(cluster), ordinal)
}

// EtcdInitialCluster is the static bootstrap list every managed member is
// given: etcd needs to know its peers before it has a control plane to ask.
func EtcdInitialCluster(cluster *fsv1alpha1.FSCluster) string {
	replicas := cluster.Spec.EtcdReplicas()
	members := make([]string, 0, replicas)

	for i := range int(replicas) {
		pod := EtcdPodName(cluster.Name, i)
		members = append(members, fmt.Sprintf("%s=http://%s.%s.%s.svc:%d",
			pod, pod, EtcdServiceName(cluster.Name), cluster.Namespace, fsv1alpha1.EtcdPeerPort))
	}

	return strings.Join(members, ",")
}

// PodName is the name of a node's pod: its StatefulSet has exactly one
// replica, so the ordinal is always zero.
func PodName(node string) string {
	return node + "-0"
}

// AdvertiseAddr is the address peers dial to reach a node — its pod's stable
// DNS name through the headless Service, which publishes not-ready addresses
// so a starting node is reachable while it catches up.
func AdvertiseAddr(cluster, namespace, node string) string {
	return fmt.Sprintf("%s.%s.%s.svc:%d", PodName(node), PeersServiceName(cluster), namespace, PeerPort)
}

// AdminURL is the base URL of a node's admin listener, reached over the same
// headless Service (which publishes the admin port and not-ready addresses),
// so the operator can query a node that is starting or unhealthy. The listener
// is plaintext on the pod network; the bearer token authenticates it (SPEC
// §4.2, §9).
func AdminURL(cluster, namespace, node string) string {
	return fmt.Sprintf("http://%s.%s.%s.svc:%d", PodName(node), PeersServiceName(cluster), namespace, AdminPort)
}

// S3Endpoint is the cluster's in-cluster S3 URL (status.endpoints.s3).
func S3Endpoint(cluster, namespace string, port int32, tls bool) string {
	scheme := "http"
	if tls {
		scheme = "https"
	}

	return fmt.Sprintf("%s://%s.%s.svc:%d", scheme, ClientServiceName(cluster), namespace, port)
}
