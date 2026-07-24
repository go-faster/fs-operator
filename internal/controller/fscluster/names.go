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

import "fmt"

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
