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
	"slices"
	"strings"

	"github.com/go-faster/errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	etcdTLSVolume    = "etcd-tls"

	// stateVolumeName is the claim holding the writable storage root: what fs
	// keeps beside the disks that mount below it. The API package owns the
	// name because a disk may not take it.
	stateVolumeName = fsv1alpha1.StateVolumeName
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
	envEtcdUsername   = "FS_ETCD_USERNAME"
	envEtcdPassword   = "FS_ETCD_PASSWORD"
	envMetricsAddr    = "METRICS_ADDR"
	envPprofAddr      = "PPROF_ADDR"
	envLogLevel       = "OTEL_LOG_LEVEL"
	envTracesExporter = "OTEL_TRACES_EXPORTER"
	envLogsExporter   = "OTEL_LOGS_EXPORTER"
	envMetricsExport  = "OTEL_METRICS_EXPORTER"
	envOTLPEndpoint   = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTLPProtocol   = "OTEL_EXPORTER_OTLP_PROTOCOL"

	// Per-signal transports. The SDK reads envOTLPProtocol first and only
	// falls through to these when it is empty (autometer, autotracer,
	// autologs), so the two are rendered as alternatives, never together.
	envOTLPTracesProto  = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envOTLPLogsProto    = "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"
	envOTLPMetricsProto = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"

	// Per-signal destinations. These the exporters resolve themselves, the
	// way the OpenTelemetry specification says: a signal's own endpoint wins
	// over the shared one, so both may be rendered together.
	envOTLPTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envOTLPLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	envOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envResourceAttrs       = "OTEL_RESOURCE_ATTRIBUTES"
)

// NewStatefulSet builds the single-pod StatefulSet running one fs node.
//
// One StatefulSet per node is the load-bearing decision of the design (SPEC
// §4.1): it is what makes per-node configuration, per-node storage and exact
// rollout control possible. The StatefulSet controller replaces the pod; this
// operator only decides when.
func NewStatefulSet(cluster *fsv1alpha1.FSCluster, node Node, restartRevision string, retain ...string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: KindStatefulSet},
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
			Template:                             podTemplate(cluster, node, restartRevision, retain),
			VolumeClaimTemplates:                 volumeClaimTemplates(cluster, node, retain),
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
func podTemplate(cluster *fsv1alpha1.FSCluster, node Node, restartRevision string, retain []string) corev1.PodTemplateSpec {
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
			Containers:                    []corev1.Container{fsContainer(cluster, node, retain)},
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
func fsContainer(cluster *fsv1alpha1.FSCluster, node Node, retain []string) corev1.Container {
	spec := &cluster.Spec

	return corev1.Container{
		Name:            ContainerName,
		Image:           Image(cluster),
		ImagePullPolicy: spec.Image.PullPolicy,
		Args:            []string{"s3", flagConfig, ConfigPath},
		Ports:           containerPorts(cluster),
		Env:             env(cluster, node),
		VolumeMounts:    volumeMounts(cluster, retain),
		Resources:       spec.PodTemplate.Resources,
		StartupProbe:    probe(cluster, healthPath, startupPeriodSeconds, startupFailureThreshold),
		LivenessProbe:   probe(cluster, healthPath, livenessPeriodSeconds, livenessFailureThreshold),
		ReadinessProbe:  probe(cluster, readyPath, readinessPeriodSeconds, readinessFailureThreshold),
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
//
// A digest wins over the tag: pinning by content is what makes an air-gapped
// mirror trustworthy, since a tag can be repointed under a running cluster
// while a digest cannot. Both spellings work — a separate spec.image.digest,
// and a digest written straight into the repository, which is the form
// ApplyRegistry preserves and the chart's managerImage helper uses for the
// operator's own image.
func Image(cluster *fsv1alpha1.FSCluster) string {
	image := cluster.Spec.Image

	if strings.Contains(image.Repository, "@") {
		return image.Repository
	}

	if image.Digest != "" {
		return image.Repository + "@" + image.Digest
	}

	return image.Repository + ":" + image.Tag
}

// containerPorts are the node's listeners. pprof is there only when the SDK
// was given an address to serve it on.
func containerPorts(cluster *fsv1alpha1.FSCluster) []corev1.ContainerPort {
	ports := []corev1.ContainerPort{
		containerPort(PortNameS3, S3Port),
		containerPort(PortNamePeer, PeerPort),
		containerPort(PortNameAdmin, AdminPort),
	}

	// Only the Prometheus exporter serves this port; pushed or disabled
	// metrics leave nothing behind it.
	if MetricsScraped(cluster) {
		ports = append(ports, containerPort(PortNameMetrics, MetricsPort))
	}

	if pprofEnabled(cluster) {
		ports = append(ports, containerPort(PortNamePprof, PprofPort))
	}

	return ports
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

		{Name: envLogLevel, Value: spec.Observability.LogLevel},
		{Name: envResourceAttrs, Value: resourceAttributes(cluster, node)},
	}

	vars = append(vars, telemetryEnv(&spec.Observability)...)

	// etcd credentials go through the environment, never the rendered config:
	// a config Secret is readable by anything that can read Secrets in the
	// namespace, and a password in it would also be written into every
	// config-revision fingerprint (SPEC §9, fs §11.4).
	if external := spec.Etcd.External; external != nil && external.AuthSecretRef != nil {
		vars = append(vars,
			secretEnv(envEtcdUsername, external.AuthSecretRef.Name, EtcdUsernameKey),
			secretEnv(envEtcdPassword, external.AuthSecretRef.Name, EtcdPasswordKey),
		)
	}

	// The SDK serves pprof only when given an address, and the port and the
	// NetworkPolicy rule follow this same switch.
	if pprofEnabled(cluster) {
		vars = append(vars, corev1.EnvVar{Name: envPprofAddr, Value: listenAddr(PprofPort)})
	}

	// User variables come last so that a deliberate override wins: with
	// duplicate names the kubelet keeps the last one.
	return append(vars, spec.PodTemplate.ExtraEnv...)
}

// telemetryEnv renders the exporter selection of every signal.
//
// Every exporter is named rather than left to the SDK, whose defaults are
// "otlp" for all three: on a cluster with no collector that is three uploads
// to localhost:4318 failing every interval, and on one *with* a collector it
// would push metrics that Kubernetes is already scraping.
func telemetryEnv(spec *fsv1alpha1.ObservabilitySpec) []corev1.EnvVar {
	vars := []corev1.EnvVar{
		{Name: envTracesExporter, Value: spec.Traces.Exporter},
		{Name: envLogsExporter, Value: spec.Logs.Exporter},
		{Name: envMetricsExport, Value: spec.Metrics.Exporter},
	}

	// The Prometheus exporter serves the node's metrics port; the SDK binds
	// it only when told to, and defaults it to localhost.
	if spec.Metrics.Exporter == fsv1alpha1.ExporterPrometheus {
		vars = append(vars, corev1.EnvVar{Name: envMetricsAddr, Value: listenAddr(MetricsPort)})
	}

	// The shared destination and transport are rendered as given: whatever
	// observability.otlp holds is what OTEL_EXPORTER_OTLP_ENDPOINT and
	// OTEL_EXPORTER_OTLP_PROTOCOL say. A per-signal protocol beside a shared
	// one would be inert — the SDK reads the shared variable first — so that
	// pair is refused at admission instead of being resolved here (SPEC
	// §5.1).
	if spec.OTLP.Endpoint != "" {
		vars = append(vars, corev1.EnvVar{Name: envOTLPEndpoint, Value: spec.OTLP.Endpoint})
	}

	if spec.OTLP.Protocol != "" {
		vars = append(vars, corev1.EnvVar{Name: envOTLPProtocol, Value: spec.OTLP.Protocol})
	}

	for _, v := range []corev1.EnvVar{
		signalEndpoint(envOTLPTracesEndpoint, spec.Traces.Exporter, spec.Traces.Endpoint),
		signalEndpoint(envOTLPLogsEndpoint, spec.Logs.Exporter, spec.Logs.Endpoint),
		signalEndpoint(envOTLPMetricsEndpoint, spec.Metrics.Exporter, spec.Metrics.Endpoint),
		signalProtocol(envOTLPTracesProto, spec.Traces.Exporter, spec.Traces.Protocol),
		signalProtocol(envOTLPLogsProto, spec.Logs.Exporter, spec.Logs.Protocol),
		signalProtocol(envOTLPMetricsProto, spec.Metrics.Exporter, spec.Metrics.Protocol),
	} {
		if v.Name != "" {
			vars = append(vars, v)
		}
	}

	return vars
}

// signalEndpoint is one signal's destination variable, or the zero value when
// the signal has none of its own to declare.
func signalEndpoint(name, exporter, endpoint string) corev1.EnvVar {
	if endpoint == "" || exporter != fsv1alpha1.ExporterOTLP {
		return corev1.EnvVar{}
	}

	return corev1.EnvVar{Name: name, Value: endpoint}
}

// signalProtocol is one signal's transport variable, or the zero value when
// the signal has none of its own to declare.
func signalProtocol(name, exporter, protocol string) corev1.EnvVar {
	if protocol == "" || exporter != fsv1alpha1.ExporterOTLP {
		return corev1.EnvVar{}
	}

	return corev1.EnvVar{Name: name, Value: protocol}
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

	// The user's own attributes, in a stable order so an unchanged spec
	// renders an unchanged pod template. They come last, which is also how
	// the SDK resolves a repeated key.
	extra := cluster.Spec.Observability.ResourceAttributes
	for _, key := range slices.Sorted(maps.Keys(extra)) {
		attributes = append(attributes, key+"="+extra[key])
	}

	return strings.Join(attributes, ",")
}

// MetricsScraped reports whether the nodes serve Prometheus metrics on the
// metrics port — which decides the container port, the PodMonitor and the
// NetworkPolicy rule alike.
func MetricsScraped(cluster *fsv1alpha1.FSCluster) bool {
	exporter := cluster.Spec.Observability.Metrics.Exporter

	return exporter == "" || exporter == fsv1alpha1.ExporterPrometheus
}

// pprofEnabled reports whether this cluster serves pprof. Defaulting happens
// on the spec, but a builder called with a spec that never went through it
// should still produce the documented default rather than a node with no
// profiling and no explanation.
func pprofEnabled(cluster *fsv1alpha1.FSCluster) bool {
	return cluster.Spec.Observability.Pprof == nil || *cluster.Spec.Observability.Pprof
}

// volumes mounts the node's configuration and, when fs terminates TLS, the
// certificate. Everything the node writes — its disks and its storage root —
// comes from claim templates, not from here.
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

	if external := cluster.Spec.Etcd.External; external != nil && external.TLS.SecretName != "" {
		vols = append(vols, corev1.Volume{
			Name: etcdTLSVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  external.TLS.SecretName,
					DefaultMode: ptr.To(secretFileMode),
				},
			},
		})
	}

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

// diskNames are the disks the spec declares.
func diskNames(cluster *fsv1alpha1.FSCluster) []string {
	names := make([]string, 0, len(cluster.Spec.Storage.Disks))
	for _, disk := range cluster.Spec.Storage.Disks {
		names = append(names, disk.Name)
	}

	return names
}

// volumeMounts mounts the configuration, the certificate and every disk.
func volumeMounts(cluster *fsv1alpha1.FSCluster, retain []string) []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, len(cluster.Spec.Storage.Disks)+4)

	// The storage root comes first, before the disks that mount below it: the
	// kubelet mounts a nested path after its parent, and a disk hidden under a
	// later root mount would be an empty directory to fs.
	//
	// A single node has no such root to mount: its storage root *is* its disk
	// (SPEC §5.2), so the index it would keep beside the disks lives on the
	// one volume it has.
	if !cluster.Spec.SingleNode() {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      stateVolumeName,
			MountPath: StorageRoot,
		})
	}

	mounts = append(mounts, corev1.VolumeMount{
		Name:      configVolumeName,
		MountPath: ConfigDir,
		ReadOnly:  true,
	})

	if external := cluster.Spec.Etcd.External; external != nil && external.TLS.SecretName != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      etcdTLSVolume,
			MountPath: EtcdTLSDir,
			ReadOnly:  true,
		})
	}

	if cluster.Spec.S3.TLS.SecretName != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      tlsVolumeName,
			MountPath: TLSDir,
			ReadOnly:  true,
		})
	}

	// A disk being removed keeps its mount for as long as it keeps its claim
	// template and its config entry. All three go together or the node will
	// not start: fs creates each configured disk's root at boot, and on a
	// read-only root filesystem a disk it was told about but not given fails
	// with "mkdir: read-only file system" — a crash loop, not a degraded node.
	for _, disk := range append(diskNames(cluster), retain...) {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      disk,
			MountPath: DiskPath(disk),
		})
	}

	return mounts
}

// volumeClaimTemplates turns each declared disk into one claim per node.
func volumeClaimTemplates(cluster *fsv1alpha1.FSCluster, node Node, retain []string) []corev1.PersistentVolumeClaim {
	claims := make([]corev1.PersistentVolumeClaim, 0, len(cluster.Spec.Storage.Disks)+1)

	// The storage root. Not a disk — no data is placed on it and fs is never
	// told about it — but a claim for the same reason a disk is one: fs writes
	// its object index there at startup, the container filesystem is read-only,
	// and an index that does not survive the pod is rebuilt by walking every
	// sidecar on every disk of the node.
	//
	// Except on a single node, whose storage root is its disk: there the index
	// already lives on a claim, and a second one would be an empty volume.
	if !cluster.Spec.SingleNode() {
		claims = append(claims, claimTemplate(cluster, node, stateVolumeName,
			*cluster.Spec.Storage.State.Size, cluster.Spec.Storage.State.StorageClass))
	}

	for _, disk := range cluster.Spec.Storage.Disks {
		claims = append(claims, claimTemplate(cluster, node, disk.Name, disk.Size, disk.StorageClass))
	}

	// A disk the spec dropped stays until its data has moved off. Its size is
	// the live claim's, which is immutable anyway; the claim template only has
	// to keep naming it so the pod keeps mounting it.
	for _, name := range retain {
		claims = append(claims, claimTemplate(cluster, node, name, retainedDiskSize(cluster), ""))
	}

	return claims
}

// claimTemplate is one of a node's claims: a disk, a retained disk, or the
// storage root.
func claimTemplate(
	cluster *fsv1alpha1.FSCluster,
	node Node,
	name string,
	size resource.Quantity,
	storageClass string,
) corev1.PersistentVolumeClaim {
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: NodeObjectLabels(cluster.Name, node),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}

	if storageClass != "" {
		claim.Spec.StorageClassName = ptr.To(storageClass)
	}

	return claim
}

// retainedDiskSize is the size a retained claim template declares. The live
// PVC's size is what actually applies — a claim template cannot resize one —
// so this only has to be a value the API accepts, and the largest declared
// disk is the one least likely to read as a shrink.
func retainedDiskSize(cluster *fsv1alpha1.FSCluster) resource.Quantity {
	var largest resource.Quantity

	for _, disk := range cluster.Spec.Storage.Disks {
		if disk.Size.Cmp(largest) > 0 {
			largest = disk.Size
		}
	}

	return largest
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
