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

package fscluster

import (
	"maps"

	"github.com/go-faster/errors"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/keygen"
)

// Labels every managed object carries (SPEC §4.2). The cluster and node labels
// are how the operator finds what it owns, and how a user greps for it.
const (
	// LabelName identifies the application; it is always AppName.
	LabelName = "app.kubernetes.io/name"

	// LabelInstance names the FSCluster the object belongs to.
	LabelInstance = "app.kubernetes.io/instance"

	// LabelManagedBy names the operator.
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// LabelCluster names the FSCluster; unlike LabelInstance it is part of
	// the (immutable) selectors, so it never changes meaning.
	LabelCluster = "fs.go-faster.org/cluster"

	// LabelNode names the fs node an object belongs to.
	LabelNode = "fs.go-faster.org/node"

	// LabelRack names the fs failure domain a node is in.
	LabelRack = "fs.go-faster.org/rack"

	// LabelComponent separates the pieces the operator runs for a cluster.
	// Node objects carry no component label, so the selectors that predate
	// this — the peers Service, the disruption budget — keep matching exactly
	// what they always did and never pick up an etcd pod.
	LabelComponent = "app.kubernetes.io/component"

	// ComponentEtcd marks the managed development etcd.
	ComponentEtcd = "etcd"

	// AppName is the application every managed pod runs.
	AppName = "fs"

	// KindService and KindStatefulSet are the TypeMeta kinds server-side apply
	// needs on every object the operator sends.
	KindService     = "Service"
	KindStatefulSet = "StatefulSet"

	// OperatorName is this operator, both as a label value and as the
	// server-side apply field manager.
	OperatorName = "fs-operator"
)

// Annotations the operator sets on the objects it manages.
const (
	// AnnotationRestartRevision fingerprints the part of a node's
	// configuration that only a restart can apply. It lives on the pod
	// template, so changing it is what makes the StatefulSet controller
	// replace the pod (SPEC §8.2); hot-reloadable material is deliberately
	// excluded, or every credential change would roll the cluster (§8.3).
	AnnotationRestartRevision = "fs.go-faster.org/restart-revision"

	// AnnotationConfigRevision fingerprints a node's full configuration. It
	// lives on the config Secret and is what reload verification compares
	// against (SPEC §8.3).
	AnnotationConfigRevision = "fs.go-faster.org/config-revision"

	// AnnotationTemplateRevision fingerprints a node's desired pod template.
	// It lives on the StatefulSet, where the next pass reads it to tell a
	// node that needs replacing from one that is already current — including
	// changes, like an image bump, that the configuration knows nothing of.
	AnnotationTemplateRevision = "fs.go-faster.org/template-revision"
)

// Keys within the Secrets the operator reads and writes. The generated and the
// user-provided Secrets share them, so a user can hand over a Secret shaped
// like the generated one.
const (
	// ClusterSecretKey holds the shared peer-auth secret.
	ClusterSecretKey = "secret"

	// AdminTokenKey holds the admin API bearer token.
	AdminTokenKey = "token"

	// AccessKeyKey and SecretKeyKey hold the two halves of an S3 credential.
	AccessKeyKey = "access-key"
	SecretKeyKey = "secret-key"
)

// SelectorLabels are the labels shared by every pod of a cluster: the label
// set the Services and the disruption budget select on. They are part of
// immutable selectors — never add to them.
func SelectorLabels(cluster string) map[string]string {
	return map[string]string{
		LabelName:    AppName,
		LabelCluster: cluster,
	}
}

// NodeSelectorLabels are the labels of exactly one node's pod: a node's
// StatefulSet selects on them, so its single pod is never confused with a
// sibling's.
func NodeSelectorLabels(cluster, node string) map[string]string {
	labels := SelectorLabels(cluster)
	labels[LabelNode] = node

	return labels
}

// ObjectLabels are the labels of a cluster-scoped managed object: the selector
// labels plus the descriptive ones.
func ObjectLabels(cluster string) map[string]string {
	labels := SelectorLabels(cluster)
	labels[LabelInstance] = cluster
	labels[LabelManagedBy] = OperatorName

	return labels
}

// NodeObjectLabels are the labels of an object belonging to one node.
func NodeObjectLabels(cluster string, node Node) map[string]string {
	labels := ObjectLabels(cluster)
	labels[LabelNode] = node.Name

	if node.Rack != "" {
		labels[LabelRack] = node.Rack
	}

	return labels
}

// ClusterSecretSource names the Secret holding the shared cluster secret: the
// one the user referenced, or the one the operator generates.
func ClusterSecretSource(cluster *fsv1alpha1.FSCluster) string {
	if ref := cluster.Spec.ClusterSecretRef; ref != nil && ref.Name != "" {
		return ref.Name
	}

	return ClusterSecretName(cluster.Name)
}

// RootCredentialsSource names the Secret holding the root S3 credential: the
// one the user referenced, or the one the operator generates.
func RootCredentialsSource(cluster *fsv1alpha1.FSCluster) string {
	if ref := cluster.Spec.Auth.RootCredentialsSecretRef; ref != nil && ref.Name != "" {
		return ref.Name
	}

	return RootCredentialsSecretName(cluster.Name)
}

// NewClusterSecret builds the Secret holding a freshly minted cluster secret.
//
// The generated Secrets are create-only: the operator applies them once and
// never again, because fs has no secret rotation and a cluster whose nodes
// disagree on the peer secret partitions itself.
func NewClusterSecret(cluster *fsv1alpha1.FSCluster) (*corev1.Secret, error) {
	secret, err := keygen.Token()
	if err != nil {
		return nil, errors.Wrap(err, "mint cluster secret")
	}

	return newSecret(cluster, ClusterSecretName(cluster.Name), map[string]string{
		ClusterSecretKey: secret,
	}), nil
}

// NewAdminTokenSecret builds the Secret holding the admin API bearer token.
func NewAdminTokenSecret(cluster *fsv1alpha1.FSCluster) (*corev1.Secret, error) {
	token, err := keygen.Token()
	if err != nil {
		return nil, errors.Wrap(err, "mint admin token")
	}

	return newSecret(cluster, AdminTokenSecretName(cluster.Name), map[string]string{
		AdminTokenKey: token,
	}), nil
}

// NewRootCredentialsSecret builds the Secret holding the root S3 credential,
// which is granted admin on every bucket and is what the operator itself uses
// for bucket management.
func NewRootCredentialsSecret(cluster *fsv1alpha1.FSCluster) (*corev1.Secret, error) {
	accessKey, err := keygen.AccessKey()
	if err != nil {
		return nil, errors.Wrap(err, "mint root access key")
	}

	secretKey, err := keygen.Token()
	if err != nil {
		return nil, errors.Wrap(err, "mint root secret key")
	}

	return newSecret(cluster, RootCredentialsSecretName(cluster.Name), map[string]string{
		AccessKeyKey: accessKey,
		SecretKeyKey: secretKey,
	}), nil
}

// NewNodeConfigSecret builds the Secret holding one node's rendered
// configuration. Unlike the generated Secrets it is applied on every pass: it
// is derived from the spec, so the desired content is always known. The
// revision annotation is the marker embedded in the config, which fs echoes
// back so the operator can verify a reload landed (SPEC §8.3).
func NewNodeConfigSecret(cluster *fsv1alpha1.FSCluster, node Node, config RenderedConfig) *corev1.Secret {
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigSecretName(node.Name),
			Namespace: cluster.Namespace,
			Labels:    NodeObjectLabels(cluster.Name, node),
			Annotations: map[string]string{
				AnnotationConfigRevision: config.Revision,
			},
		},
		Data: map[string][]byte{ConfigFileName: config.Data},
	}

	return secret
}

// newSecret builds a Secret of string data owned by the cluster.
func newSecret(cluster *fsv1alpha1.FSCluster, name string, data map[string]string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels:    ObjectLabels(cluster.Name),
		},
		StringData: data,
	}
}

// NewPeersService builds the headless Service that gives every pod its stable
// DNS name.
//
// It publishes not-ready addresses on purpose: a starting node must be
// dialable by its peers before it reports ready, and the operator must reach
// the admin API of a node that is not serving yet to find out why.
func NewPeersService(cluster *fsv1alpha1.FSCluster) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: KindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      PeersServiceName(cluster.Name),
			Namespace: cluster.Namespace,
			Labels:    ObjectLabels(cluster.Name),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 SelectorLabels(cluster.Name),
			Ports: []corev1.ServicePort{
				servicePort(PortNamePeer, PeerPort),
				servicePort(PortNameAdmin, AdminPort),
				servicePort(PortNameMetrics, MetricsPort),
			},
		},
	}
}

// NewClientService builds the Service S3 clients talk to.
func NewClientService(cluster *fsv1alpha1.FSCluster) *corev1.Service {
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: KindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:        ClientServiceName(cluster.Name),
			Namespace:   cluster.Namespace,
			Labels:      ObjectLabels(cluster.Name),
			Annotations: maps.Clone(cluster.Spec.S3.Service.Annotations),
		},
		Spec: corev1.ServiceSpec{
			Type:     cluster.Spec.S3.Service.Type,
			Selector: SelectorLabels(cluster.Name),
			Ports: []corev1.ServicePort{
				servicePort(PortNameS3, cluster.Spec.S3.Service.Port),
			},
		},
	}

	return service
}

// servicePort exposes a port that targets the container port of the same
// name, so the pod's own listeners stay independent of what the Service
// publishes.
func servicePort(name string, port int32) corev1.ServicePort {
	return corev1.ServicePort{
		Name:       name,
		Port:       port,
		TargetPort: intstr.FromString(name),
		Protocol:   corev1.ProtocolTCP,
	}
}

// NewPodDisruptionBudget builds the budget that keeps voluntary disruption to
// one node at a time.
//
// It is not configurable: with copies spread across failure domains, evicting
// two nodes at once can take a second domain down, and fs's upgrade contract
// is explicit that nodes are replaced one at a time (SPEC §8.1).
func NewPodDisruptionBudget(cluster *fsv1alpha1.FSCluster) *policyv1.PodDisruptionBudget {
	maxUnavailable := intstr.FromInt32(1)

	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    ObjectLabels(cluster.Name),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: SelectorLabels(cluster.Name)},
		},
	}
}
