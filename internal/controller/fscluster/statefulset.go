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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/go-faster/errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// The container the fs binary runs in, and where its configuration comes from.
const (
	// ContainerName is the fs container's name; probes, events and `kubectl
	// logs` all refer to it.
	ContainerName = "fs"

	// configVolumeName and tlsVolumeName mount the config and certificate
	// Secrets.
	configVolumeName = "config"
	tlsVolumeName    = "tls"
)

// The unprivileged identity fs runs as. It matches the uid the upstream image
// ships, and fsGroup is what makes freshly provisioned volumes writable.
const (
	runAsUser  int64 = 1000
	runAsGroup int64 = 1000
)

// Probe timings. Startup is generous because a node with large disks scans
// them before it serves, while liveness stays tight — a wedged node should be
// replaced, not left in the topology.
const (
	startupPeriodSeconds    int32 = 5
	startupFailureThreshold int32 = 60

	livenessPeriodSeconds    int32 = 10
	livenessTimeoutSeconds   int32 = 3
	livenessFailureThreshold int32 = 3

	readinessPeriodSeconds    int32 = 10
	readinessTimeoutSeconds   int32 = 3
	readinessFailureThreshold int32 = 3
)

// secretFileMode is the mode of a mounted Secret file: readable by the owner
// and the fsGroup the container runs under, and by nobody else.
const secretFileMode int32 = 0o440

// terminationGracePeriodSeconds gives a node time to finish in-flight requests
// and deregister from the topology instead of leaving a lease to expire.
const terminationGracePeriodSeconds int64 = 60

// hostnameTopologyKey spreads a cluster's pods over Kubernetes nodes; zone
// placement is the rack's business.
const hostnameTopologyKey = "kubernetes.io/hostname"

// zoneLabel is the well-known zone label a rack's zone pins to.
const zoneLabel = "topology.kubernetes.io/zone"

// preferredAntiAffinityWeight is the weight of the soft spreading rule; it is
// the maximum, since anything less lets a scheduler stack two nodes for
// trivial reasons.
const preferredAntiAffinityWeight int32 = 100

// Environment variables fs and its telemetry SDK read.
const (
	envClusterSecret  = "FS_CLUSTER_SECRET"
	envAdminToken     = "FS_ADMIN_TOKEN"
	envRootAccessKey  = "FS_ROOT_ACCESS_KEY"
	envRootSecretKey  = "FS_ROOT_SECRET_KEY"
	envMetricsAddr    = "METRICS_ADDR"
	envPprofAddr      = "PPROF_ADDR"
	envLogLevel       = "OTEL_LOG_LEVEL"
	envTracesExporter = "OTEL_TRACES_EXPORTER"
	envMetricsExport  = "OTEL_METRICS_EXPORTER"
	envOTLPEndpoint   = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTLPProtocol   = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envResourceAttrs  = "OTEL_RESOURCE_ATTRIBUTES"
)

// Telemetry exporter names of the OpenTelemetry SDK.
const (
	exporterOTLP       = "otlp"
	exporterPrometheus = "prometheus"
	exporterNone       = "none"
)

// NewStatefulSet builds the single-pod StatefulSet running one fs node.
//
// One StatefulSet per node is the load-bearing decision of the design (SPEC
// §4.1): it is what makes per-node configuration, per-node storage and exact
// rollout control possible. The StatefulSet controller replaces the pod; this
// operator only decides when.
func NewStatefulSet(cluster *fsv1alpha1.FSCluster, node Node, restartRevision string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: cluster.Namespace,
			Labels:    NodeObjectLabels(cluster.Name, node),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To[int32](1),
			ServiceName: PeersServiceName(cluster.Name),
			Selector: &metav1.LabelSelector{
				MatchLabels: NodeSelectorLabels(cluster.Name, node.Name),
			},
			Template:                             podTemplate(cluster, node, restartRevision),
			VolumeClaimTemplates:                 volumeClaimTemplates(cluster, node),
			PersistentVolumeClaimRetentionPolicy: claimRetentionPolicy(cluster),
		},
	}
}

// TemplateRevision fingerprints one node's desired pod template.
func TemplateRevision(set *appsv1.StatefulSet) (string, error) {
	template, err := json.Marshal(set.Spec.Template)
	if err != nil {
		return "", errors.Wrapf(err, "marshal pod template of node %q", set.Name)
	}

	digest := sha256.Sum256(template)

	return format(templatePrefix, digest[:]), nil
}

// PodTemplateRevision fingerprints the desired pod templates
// (status.statefulSetRevision): what a node runs, as opposed to what it reads
// from its configuration.
func PodTemplateRevision(sets []*appsv1.StatefulSet) (string, error) {
	digest := sha256.New()

	for _, set := range sets {
		template, err := json.Marshal(set.Spec.Template)
		if err != nil {
			return "", errors.Wrapf(err, "marshal pod template of node %q", set.Name)
		}

		// hash.Hash never fails, hence the discarded errors.
		_, _ = fmt.Fprintf(digest, "%d:%s%d:", len(set.Name), set.Name, len(template))
		_, _ = digest.Write(template)
	}

	return format(templatePrefix, digest.Sum(nil)), nil
}

// podTemplate builds the pod that runs one node.
func podTemplate(cluster *fsv1alpha1.FSCluster, node Node, restartRevision string) corev1.PodTemplateSpec {
	spec := &cluster.Spec

	labels := NodeObjectLabels(cluster.Name, node)
	maps.Copy(labels, spec.PodTemplate.Labels)
	// The user may decorate the pod, but not relabel it out of its own
	// selectors.
	maps.Copy(labels, NodeSelectorLabels(cluster.Name, node.Name))

	annotations := maps.Clone(spec.PodTemplate.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[AnnotationRestartRevision] = restartRevision

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{fsContainer(cluster, node)},
			Volumes:                       volumes(cluster, node),
			ImagePullSecrets:              spec.Image.PullSecrets,
			NodeSelector:                  nodeSelector(cluster, node),
			Affinity:                      affinity(cluster, node),
			Tolerations:                   spec.PodTemplate.Tolerations,
			PriorityClassName:             spec.PodTemplate.PriorityClassName,
			SecurityContext:               podSecurityContext(),
			TerminationGracePeriodSeconds: ptr.To(terminationGracePeriodSeconds),
			// fs never talks to the Kubernetes API; a mounted token would
			// only be something to steal.
			AutomountServiceAccountToken: ptr.To(false),
		},
	}
}

// fsContainer builds the fs container itself.
func fsContainer(cluster *fsv1alpha1.FSCluster, node Node) corev1.Container {
	spec := &cluster.Spec

	return corev1.Container{
		Name:            ContainerName,
		Image:           Image(cluster),
		ImagePullPolicy: spec.Image.PullPolicy,
		Args:            []string{"s3", flagConfig, ConfigPath},
		Ports: []corev1.ContainerPort{
			containerPort(PortNameS3, S3Port),
			containerPort(PortNamePeer, PeerPort),
			containerPort(PortNameAdmin, AdminPort),
			containerPort(PortNameMetrics, MetricsPort),
			containerPort(PortNamePprof, PprofPort),
		},
		Env:            env(cluster, node),
		VolumeMounts:   volumeMounts(cluster),
		Resources:      spec.PodTemplate.Resources,
		StartupProbe:   probe(cluster, healthPath, startupPeriodSeconds, startupFailureThreshold),
		LivenessProbe:  probe(cluster, healthPath, livenessPeriodSeconds, livenessFailureThreshold),
		ReadinessProbe: probe(cluster, readyPath, readinessPeriodSeconds, readinessFailureThreshold),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capAll}},
		},
	}
}

// readyPath is fs's readiness endpoint: it probes the storage backend, so it
// reports 503 while a node cannot serve.
const readyPath = "/ready"

// flagConfig points fs at its configuration file; capAll is the capability set
// dropped from every fs container.
const flagConfig = "--config"

const capAll corev1.Capability = "ALL"

// Image is the image reference a cluster's nodes run.
func Image(cluster *fsv1alpha1.FSCluster) string {
	return cluster.Spec.Image.Repository + ":" + cluster.Spec.Image.Tag
}

// containerPort names a port so Services and probes can target it by name.
func containerPort(name string, port int32) corev1.ContainerPort {
	return corev1.ContainerPort{
		Name:          name,
		ContainerPort: port,
		Protocol:      corev1.ProtocolTCP,
	}
}

// probe builds an HTTP probe against the S3 listener, over TLS when fs
// terminates it.
func probe(cluster *fsv1alpha1.FSCluster, path string, period, failures int32) *corev1.Probe {
	scheme := corev1.URISchemeHTTP
	if cluster.Spec.S3.TLS.SecretName != "" {
		scheme = corev1.URISchemeHTTPS
	}

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   portByName(PortNameS3),
				Scheme: scheme,
			},
		},
		PeriodSeconds:    period,
		TimeoutSeconds:   livenessTimeoutSeconds,
		FailureThreshold: failures,
	}
}

// env builds the container environment: the secret material fs reads from it,
// and the telemetry the SDK is steered by.
func env(cluster *fsv1alpha1.FSCluster, node Node) []corev1.EnvVar {
	spec := &cluster.Spec

	vars := []corev1.EnvVar{
		secretEnv(envClusterSecret, ClusterSecretSource(cluster), ClusterSecretKey),
		secretEnv(envAdminToken, AdminTokenSecretName(cluster.Name), AdminTokenKey),
		secretEnv(envRootAccessKey, RootCredentialsSource(cluster), AccessKeyKey),
		secretEnv(envRootSecretKey, RootCredentialsSource(cluster), SecretKeyKey),

		// The SDK binds its Prometheus and pprof listeners only when told to,
		// and defaults the Prometheus one to localhost.
		{Name: envMetricsAddr, Value: listenAddr(MetricsPort)},
		{Name: envPprofAddr, Value: listenAddr(PprofPort)},
		{Name: envLogLevel, Value: spec.Observability.LogLevel},

		// Metrics are always scrapeable: pull is how Kubernetes collects them,
		// and the optional PodMonitor targets this port.
		{Name: envMetricsExport, Value: exporterPrometheus},
		{Name: envResourceAttrs, Value: resourceAttributes(cluster, node)},
	}

	if endpoint := spec.Observability.OTLP.Endpoint; endpoint != "" {
		vars = append(vars,
			corev1.EnvVar{Name: envTracesExporter, Value: exporterOTLP},
			corev1.EnvVar{Name: envOTLPEndpoint, Value: endpoint},
			corev1.EnvVar{Name: envOTLPProtocol, Value: spec.Observability.OTLP.Protocol},
		)
	} else {
		// Without a destination the SDK would still build an OTLP exporter
		// and log a failed export every interval.
		vars = append(vars, corev1.EnvVar{Name: envTracesExporter, Value: exporterNone})
	}

	// User variables come last so that a deliberate override wins: with
	// duplicate names the kubelet keeps the last one.
	return append(vars, spec.PodTemplate.ExtraEnv...)
}

// secretEnv reads one key of a Secret into an environment variable, which is
// the only way secret material reaches the container.
func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		},
	}
}

// resourceAttributes identify a node's telemetry: all nodes of a cluster share
// a service name, so the cluster, node and rack are what tell them apart.
func resourceAttributes(cluster *fsv1alpha1.FSCluster, node Node) string {
	attributes := []string{
		"fs.cluster=" + cluster.Name,
		"k8s.namespace.name=" + cluster.Namespace,
		"fs.node=" + node.Name,
	}

	if node.Rack != "" {
		attributes = append(attributes, "fs.rack="+node.Rack)
	}

	return strings.Join(attributes, ",")
}

// volumes mounts the node's configuration and, when fs terminates TLS, the
// certificate. Everything else the node writes lives on its claims.
func volumes(cluster *fsv1alpha1.FSCluster, node Node) []corev1.Volume {
	vols := []corev1.Volume{{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: ConfigSecretName(node.Name),
				// Group-readable, not owner-only: a mounted Secret belongs to
				// root:fsGroup, and fs runs unprivileged.
				DefaultMode: ptr.To(secretFileMode),
			},
		},
	}}

	if name := cluster.Spec.S3.TLS.SecretName; name != "" {
		vols = append(vols, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  name,
					DefaultMode: ptr.To(secretFileMode),
				},
			},
		})
	}

	return vols
}

// volumeMounts mounts the configuration, the certificate and every disk.
func volumeMounts(cluster *fsv1alpha1.FSCluster) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{{
		Name:      configVolumeName,
		MountPath: ConfigDir,
		ReadOnly:  true,
	}}

	if cluster.Spec.S3.TLS.SecretName != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      tlsVolumeName,
			MountPath: TLSDir,
			ReadOnly:  true,
		})
	}

	for _, disk := range cluster.Spec.Storage.Disks {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      disk.Name,
			MountPath: DiskPath(disk.Name),
		})
	}

	return mounts
}

// volumeClaimTemplates turns each declared disk into one claim per node.
func volumeClaimTemplates(cluster *fsv1alpha1.FSCluster, node Node) []corev1.PersistentVolumeClaim {
	claims := make([]corev1.PersistentVolumeClaim, 0, len(cluster.Spec.Storage.Disks))

	for _, disk := range cluster.Spec.Storage.Disks {
		claim := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:   disk.Name,
				Labels: NodeObjectLabels(cluster.Name, node),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: disk.Size},
				},
			},
		}

		if disk.StorageClass != "" {
			claim.Spec.StorageClassName = ptr.To(disk.StorageClass)
		}

		claims = append(claims, claim)
	}

	return claims
}

// claimRetentionPolicy applies the spec's reclaim policy to the claims a
// StatefulSet owns. Retain — the default — keeps a removed node's data until
// someone decides otherwise.
func claimRetentionPolicy(cluster *fsv1alpha1.FSCluster) *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
	retention := appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	if cluster.Spec.Storage.ReclaimPolicy == fsv1alpha1.ReclaimDelete {
		retention = appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	}

	return &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: retention,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
}

// nodeSelector merges the cluster-wide selector with the rack's.
func nodeSelector(cluster *fsv1alpha1.FSCluster, node Node) map[string]string {
	selector := maps.Clone(cluster.Spec.PodTemplate.NodeSelector)
	if selector == nil {
		selector = map[string]string{}
	}

	maps.Copy(selector, node.NodeSelector)

	if len(selector) == 0 {
		return nil
	}

	return selector
}

// affinity pins a node to its rack's zone and spreads the cluster's pods over
// Kubernetes nodes.
func affinity(cluster *fsv1alpha1.FSCluster, node Node) *corev1.Affinity {
	affinity := &corev1.Affinity{
		NodeAffinity:    zoneAffinity(node),
		PodAntiAffinity: antiAffinity(cluster),
	}

	if affinity.NodeAffinity == nil && affinity.PodAntiAffinity == nil {
		return nil
	}

	return affinity
}

// zoneAffinity pins a rack to a zone. Rack membership is declared, never
// derived from where a pod happens to land, so the pin is required rather than
// preferred.
func zoneAffinity(node Node) *corev1.NodeAffinity {
	if node.Zone == "" {
		return nil
	}

	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      zoneLabel,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{node.Zone},
				}},
			}},
		},
	}
}

// antiAffinity spreads a cluster's nodes over Kubernetes nodes. Required — the
// default — keeps the failure model honest: two fs nodes on one machine share
// a fate the placement logic does not know about.
func antiAffinity(cluster *fsv1alpha1.FSCluster) *corev1.PodAntiAffinity {
	term := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: SelectorLabels(cluster.Name)},
		TopologyKey:   hostnameTopologyKey,
	}

	switch cluster.Spec.Topology.PodAntiAffinity {
	case fsv1alpha1.AntiAffinityPreferred:
		return &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight:          preferredAntiAffinityWeight,
				PodAffinityTerm: term,
			}},
		}
	case fsv1alpha1.AntiAffinityNone:
		return nil
	case fsv1alpha1.AntiAffinityRequired:
		fallthrough
	default:
		return &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{term},
		}
	}
}

// podSecurityContext runs fs unprivileged. fsGroup is what makes a freshly
// provisioned volume writable by a non-root process.
func podSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To(runAsUser),
		RunAsGroup:     ptr.To(runAsGroup),
		FSGroup:        ptr.To(runAsGroup),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// portByName targets a named container port.
func portByName(name string) intstr.IntOrString {
	return intstr.FromString(name)
}
