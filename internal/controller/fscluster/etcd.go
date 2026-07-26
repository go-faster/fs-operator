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
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/controller/pipeline"
	"github.com/go-faster/fs-operator/internal/validation"
)

// eventManagedEtcd warns, on every cluster that uses it, that the control
// plane is the development one. It is an event rather than a log line because
// the person who needs to know is looking at the cluster, not at the operator.
const eventManagedEtcd = "ManagedEtcdUnsupported"

// etcdLabels identify the managed etcd's objects.
//
// The app name is deliberately NOT AppName. SelectorLabels — which the client
// Service, the peers Service, the disruption budget, the NetworkPolicy and the
// PodMonitor all select on — is {LabelName: AppName, LabelCluster: cluster},
// and an etcd pod carrying those would be served S3 traffic by the client
// Service and counted against the fs nodes' disruption budget. A different app
// name is what keeps every one of those selectors matching exactly what it
// always matched.
func etcdLabels(cluster string) map[string]string {
	return map[string]string{
		LabelName:      AppName + "-etcd",
		LabelCluster:   cluster,
		LabelManagedBy: OperatorName,
		LabelComponent: ComponentEtcd,
	}
}

// reconcileEtcd runs the cluster's own etcd when it asked for one.
//
// It is deliberately the least sophisticated thing that works: a StatefulSet
// with a static bootstrap list, one PVC per member, no backups and no
// membership management. SPEC §2 makes that permanent — this exists so an
// example applies cleanly on a laptop, and hardening it into a production
// offering is a different project.
func (r *Reconciler) reconcileEtcd(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	if !p.cluster.Spec.ManagedEtcd() {
		return pipeline.Continue()
	}

	if err := r.apply(ctx, p.cluster, NewEtcdService(p.cluster)); err != nil {
		return pipeline.Outcome{}, err
	}

	if err := r.apply(ctx, p.cluster, NewEtcdStatefulSet(p.cluster)); err != nil {
		return pipeline.Outcome{}, err
	}

	// Said on every pass that finds it, because the cost of this being a
	// surprise later is the whole cluster.
	r.Recorder.Event(p.object, corev1.EventTypeWarning, eventManagedEtcd, validation.ManagedEtcdWarning)

	return pipeline.Continue()
}

// NewEtcdService is the headless Service giving each etcd member stable DNS.
//
// It publishes not-ready addresses: members have to reach each other to elect
// a leader, and none of them is ready until they have.
func NewEtcdService(cluster *fsv1alpha1.FSCluster) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: KindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      EtcdServiceName(cluster.Name),
			Namespace: cluster.Namespace,
			Labels:    etcdLabels(cluster.Name),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 etcdLabels(cluster.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "client",
					Port:       fsv1alpha1.EtcdClientPort,
					TargetPort: intstr.FromInt32(fsv1alpha1.EtcdClientPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "peer",
					Port:       fsv1alpha1.EtcdPeerPort,
					TargetPort: intstr.FromInt32(fsv1alpha1.EtcdPeerPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// NewEtcdStatefulSet is the managed etcd itself.
func NewEtcdStatefulSet(cluster *fsv1alpha1.FSCluster) *appsv1.StatefulSet {
	managed := cluster.Spec.Etcd.Managed
	replicas := cluster.Spec.EtcdReplicas()
	labels := etcdLabels(cluster.Name)

	retention := appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	if managed.Storage.ReclaimPolicy == fsv1alpha1.ReclaimRetain {
		retention = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	}

	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: KindStatefulSet},
		ObjectMeta: metav1.ObjectMeta{
			Name:      EtcdStatefulSetName(cluster.Name),
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: EtcdServiceName(cluster.Name),
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			// Parallel, not the default OrderedReady: a fresh multi-member
			// etcd cannot reach quorum one pod at a time, because member 0
			// never becomes ready alone and the rest are never started.
			PodManagementPolicy: appsv1.ParallelPodManagement,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: retention,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       etcdPodSpec(cluster),
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{etcdClaim(cluster)},
		},
	}
}

// etcdPodSpec is one etcd member.
func etcdPodSpec(cluster *fsv1alpha1.FSCluster) corev1.PodSpec {
	managed := cluster.Spec.Etcd.Managed
	image := managed.Image.Repository + ":" + managed.Image.Tag

	// Bootstrapping is static: every member is told the full list up front,
	// because there is no control plane to discover it from yet. It is also
	// why replicas is immutable — changing the list would need a join, which
	// this does not implement.
	initialCluster := EtcdInitialCluster(cluster)

	return corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true),
			RunAsUser:    ptr.To(int64(1000)),
			RunAsGroup:   ptr.To(int64(1000)),
			FSGroup:      ptr.To(int64(1000)),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{{
			Name: "etcd",
			// Not run through the operator's registry override: that rewrites
			// the *fs* image for air-gapped installs. An air-gapped managed
			// etcd sets etcd.managed.image.repository, which is the honest
			// knob for an image the operator does not otherwise own.
			Image:           image,
			ImagePullPolicy: managed.Image.PullPolicy,
			Command:         []string{"/usr/local/bin/etcd"},
			Args: []string{
				"--name=$(POD_NAME)",
				"--data-dir=/var/run/etcd/default.etcd",
				fmt.Sprintf("--listen-client-urls=http://0.0.0.0:%d", fsv1alpha1.EtcdClientPort),
				fmt.Sprintf("--advertise-client-urls=http://$(POD_NAME).%s.$(POD_NAMESPACE).svc:%d",
					EtcdServiceName(cluster.Name), fsv1alpha1.EtcdClientPort),
				fmt.Sprintf("--listen-peer-urls=http://0.0.0.0:%d", fsv1alpha1.EtcdPeerPort),
				fmt.Sprintf("--initial-advertise-peer-urls=http://$(POD_NAME).%s.$(POD_NAMESPACE).svc:%d",
					EtcdServiceName(cluster.Name), fsv1alpha1.EtcdPeerPort),
				"--initial-cluster=" + initialCluster,
				"--initial-cluster-state=new",
				"--initial-cluster-token=" + EtcdStatefulSetName(cluster.Name),
				// Development sizing: keep history bounded so a long-running
				// demo cluster does not grow its volume without limit.
				"--auto-compaction-retention=1",
				"--quota-backend-bytes=1073741824",
			},
			Env: []corev1.EnvVar{
				{
					Name:      "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
				},
				{
					Name:      "POD_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
				},
			},
			Ports: []corev1.ContainerPort{
				{Name: "client", ContainerPort: fsv1alpha1.EtcdClientPort, Protocol: corev1.ProtocolTCP},
				{Name: "peer", ContainerPort: fsv1alpha1.EtcdPeerPort, Protocol: corev1.ProtocolTCP},
			},
			ReadinessProbe: etcdProbe(3, 5),
			LivenessProbe:  etcdProbe(15, 15),
			Resources:      managed.Resources,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				ReadOnlyRootFilesystem:   ptr.To(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: etcdVolumeName, MountPath: "/var/run/etcd"}},
		}},
	}
}

// etcdVolumeName is the members' data volume, and the claim template's name.
const etcdVolumeName = "data"

// etcdProbe is etcd's own health endpoint.
func etcdProbe(delay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(fsv1alpha1.EtcdClientPort),
			},
		},
		InitialDelaySeconds: delay,
		PeriodSeconds:       period,
		FailureThreshold:    3,
	}
}

// etcdClaim is one member's volume.
func etcdClaim(cluster *fsv1alpha1.FSCluster) corev1.PersistentVolumeClaim {
	storage := cluster.Spec.Etcd.Managed.Storage

	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   etcdVolumeName,
			Labels: etcdLabels(cluster.Name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *storage.Size},
			},
		},
	}

	if storage.StorageClass != "" {
		claim.Spec.StorageClassName = &storage.StorageClass
	}

	return claim
}
