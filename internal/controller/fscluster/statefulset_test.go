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
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// testRevision stands in for a rendered configuration's fingerprint.
const testRevision = "cfg-0123456789ab"

// nodeStatefulSet builds the first node's StatefulSet of a defaulted cluster.
func nodeStatefulSet(t *testing.T, cluster *fsv1alpha1.FSCluster) *appsv1.StatefulSet {
	t.Helper()

	cluster.Spec.WithDefaults()

	return NewStatefulSet(cluster, Nodes(cluster)[0], testRevision)
}

func TestNewStatefulSet(t *testing.T) {
	cluster := testCluster()
	set := nodeStatefulSet(t, cluster)

	if got, want := set.Name, node0; got != want {
		t.Errorf("name = %q, want the node's own name %q", got, want)
	}

	if set.Spec.Replicas == nil || *set.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want exactly one pod per node", set.Spec.Replicas)
	}

	if got, want := set.Spec.ServiceName, PeersServiceName(cluster.Name); got != want {
		t.Errorf("serviceName = %q, want the headless %q", got, want)
	}

	if got, want := set.Spec.Template.Annotations[AnnotationRestartRevision], testRevision; got != want {
		t.Errorf("restart revision annotation = %q, want %q", got, want)
	}

	container := set.Spec.Template.Spec.Containers[0]

	if got, want := container.Image, "ghcr.io/go-faster/fs:"+fsv1alpha1.DefaultImageTag; got != want {
		t.Errorf("image = %q, want %q", got, want)
	}

	if got, want := strings.Join(container.Args, " "), "s3 --config "+ConfigPath; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	if container.ReadinessProbe.HTTPGet.Path != readyPath {
		t.Errorf("readiness probes %q, want %q", container.ReadinessProbe.HTTPGet.Path, readyPath)
	}

	if container.LivenessProbe.HTTPGet.Path != healthPath {
		t.Errorf("liveness probes %q, want %q", container.LivenessProbe.HTTPGet.Path, healthPath)
	}

	if container.StartupProbe == nil {
		t.Error("no startup probe; a node scanning large disks would be killed by liveness")
	}
}

// TestNewPassImageRegistry pins the air-gapped override: when the operator runs
// with an FSImageRegistry, newPass rewrites the fs node image's registry host
// so every node pulls from the private mirror, and leaves it alone otherwise.
func TestNewPassImageRegistry(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		want     string
	}{
		{
			name:     "no override keeps the default registry",
			registry: "",
			want:     "ghcr.io/go-faster/fs:" + fsv1alpha1.DefaultImageTag,
		},
		{
			name:     "override rewrites the registry host",
			registry: "registry.internal",
			want:     "registry.internal/go-faster/fs:" + fsv1alpha1.DefaultImageTag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPass(testCluster(), tt.registry)

			if got := Image(p.cluster); got != tt.want {
				t.Errorf("image = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestImageDigestPinning covers pinning a node image by content rather than by
// tag, which is what makes a mirrored registry trustworthy: a tag can be
// repointed under a running cluster, a digest cannot.
func TestImageDigestPinning(t *testing.T) {
	const digest = "sha256:" +
		"3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea"

	tests := []struct {
		name     string
		registry string
		mutate   func(*fsv1alpha1.FSCluster)
		want     string
	}{
		{
			name:   "digest wins over the tag",
			mutate: func(c *fsv1alpha1.FSCluster) { c.Spec.Image.Digest = digest },
			want:   "ghcr.io/go-faster/fs@" + digest,
		},
		{
			name:     "mirrored digest keeps the content pin",
			registry: "registry.internal",
			mutate:   func(c *fsv1alpha1.FSCluster) { c.Spec.Image.Digest = digest },
			want:     "registry.internal/go-faster/fs@" + digest,
		},
		{
			// The spelling the chart uses for the operator's own image, and
			// the one ApplyRegistry preserves.
			name: "digest written into the repository is honoured",
			mutate: func(c *fsv1alpha1.FSCluster) {
				c.Spec.Image.Repository = "ghcr.io/go-faster/fs@" + digest
			},
			want: "ghcr.io/go-faster/fs@" + digest,
		},
		{
			name:   "no digest still runs the tag",
			mutate: func(*fsv1alpha1.FSCluster) {},
			want:   "ghcr.io/go-faster/fs:" + fsv1alpha1.DefaultImageTag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := testCluster()
			tt.mutate(cluster)

			p := newPass(cluster, tt.registry)

			if got := Image(p.cluster); got != tt.want {
				t.Errorf("image = %q, want %q", got, tt.want)
			}

			// A digest has to reach the pod, not just the helper.
			set := NewStatefulSet(p.cluster, p.nodes[0], "rev")
			if got := set.Spec.Template.Spec.Containers[0].Image; got != tt.want {
				t.Errorf("container image = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStatefulSetIsUnprivileged pins the hardening SPEC §9 promises.
func TestStatefulSetIsUnprivileged(t *testing.T) {
	set := nodeStatefulSet(t, testCluster())

	pod := set.Spec.Template.Spec.SecurityContext
	if pod == nil || pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot {
		t.Fatal("pods may run as root")
	}

	if pod.FSGroup == nil {
		t.Error("no fsGroup; a non-root process could not write to a fresh volume")
	}

	if pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp is not the runtime default")
	}

	if set.Spec.Template.Spec.AutomountServiceAccountToken == nil ||
		*set.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Error("a service account token is mounted; fs never talks to the API server")
	}

	container := set.Spec.Template.Spec.Containers[0].SecurityContext
	if container == nil || container.ReadOnlyRootFilesystem == nil || !*container.ReadOnlyRootFilesystem {
		t.Error("the root filesystem is writable")
	}

	if container.AllowPrivilegeEscalation == nil || *container.AllowPrivilegeEscalation {
		t.Error("privilege escalation is allowed")
	}

	if len(container.Capabilities.Drop) != 1 || container.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities dropped = %v, want ALL", container.Capabilities.Drop)
	}
}

// TestStatefulSetSecretMaterialComesFromSecrets is the environment half of the
// rule the config renderer keeps: nothing secret is ever an inline value.
func TestStatefulSetSecretMaterialComesFromSecrets(t *testing.T) {
	cluster := testCluster()
	set := nodeStatefulSet(t, cluster)

	want := map[string]struct{ secret, key string }{
		envClusterSecret: {ClusterSecretName(cluster.Name), ClusterSecretKey},
		envAdminToken:    {AdminTokenSecretName(cluster.Name), AdminTokenKey},
		envRootAccessKey: {RootCredentialsSecretName(cluster.Name), AccessKeyKey},
		envRootSecretKey: {RootCredentialsSecretName(cluster.Name), SecretKeyKey},
	}

	for _, env := range set.Spec.Template.Spec.Containers[0].Env {
		source, ok := want[env.Name]
		if !ok {
			continue
		}

		delete(want, env.Name)

		if env.Value != "" {
			t.Errorf("%s carries an inline value", env.Name)
		}

		ref := env.ValueFrom.SecretKeyRef
		if ref.Name != source.secret || ref.Key != source.key {
			t.Errorf("%s reads %s/%s, want %s/%s", env.Name, ref.Name, ref.Key, source.secret, source.key)
		}
	}

	for name := range want {
		t.Errorf("%s is not in the environment", name)
	}
}

func TestStatefulSetTelemetryEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		want     map[string]string
	}{
		{
			// Without a collector the SDK must be told not to export, or every
			// interval logs a failed connection to localhost.
			name: "no collector",
			want: map[string]string{
				envTracesExporter: fsv1alpha1.ExporterNone,
				envMetricsExport:  fsv1alpha1.ExporterPrometheus,
				envMetricsAddr:    ":9464",
				envPprofAddr:      ":9010",
			},
		},
		{
			// An endpoint and no transport: the SDK's default (grpc) applies,
			// and leaving the variable out is what lets a signal name its own.
			name:     "otlp collector",
			endpoint: "http://collector:4317",
			want: map[string]string{
				envTracesExporter: fsv1alpha1.ExporterOTLP,
				envOTLPEndpoint:   "http://collector:4317",
				envMetricsExport:  fsv1alpha1.ExporterPrometheus,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testCluster()
			cluster.Spec.Observability.OTLP.Endpoint = tc.endpoint

			env := environment(nodeStatefulSet(t, cluster))

			for name, want := range tc.want {
				if got := env[name]; got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}

			if !strings.Contains(env[envResourceAttrs], "fs.node="+node0) {
				t.Errorf("resource attributes %q do not identify the node", env[envResourceAttrs])
			}
		})
	}
}

// TestStatefulSetExtraEnvWins keeps the escape hatch open: a user setting a
// variable the operator also sets must win, since the kubelet keeps the last
// occurrence.
func TestStatefulSetExtraEnvWins(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.PodTemplate.ExtraEnv = []corev1.EnvVar{{Name: envMetricsExport, Value: fsv1alpha1.ExporterOTLP}}

	env := environment(nodeStatefulSet(t, cluster))
	if got, want := env[envMetricsExport], fsv1alpha1.ExporterOTLP; got != want {
		t.Errorf("%s = %q, want the user's %q", envMetricsExport, got, want)
	}
}

func TestStatefulSetStorage(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.Storage.Disks = []fsv1alpha1.DiskSpec{
		{Name: "d0", Size: resource.MustParse("200Gi"), StorageClass: storageClass},
		{Name: "d1", Size: resource.MustParse("1Ti")},
	}

	set := nodeStatefulSet(t, cluster)

	// One per disk, plus the state volume every node carries.
	if got, want := len(set.Spec.VolumeClaimTemplates), 3; got != want {
		t.Fatalf("%d claim templates, want %d", got, want)
	}

	first := claimNamed(t, set, "d0")
	if first.Spec.StorageClassName == nil || *first.Spec.StorageClassName != storageClass {
		t.Errorf("storage class = %v, want fast-nvme", first.Spec.StorageClassName)
	}

	if got := first.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "200Gi" {
		t.Errorf("size = %s, want 200Gi", got.String())
	}

	// A disk without a class must not pin one, or the cluster default is lost.
	if claimNamed(t, set, "d1").Spec.StorageClassName != nil {
		t.Error("a disk without a storage class pinned one anyway")
	}

	mounts := set.Spec.Template.Spec.Containers[0].VolumeMounts
	for _, disk := range []string{"d0", "d1"} {
		if !slices.ContainsFunc(mounts, func(m corev1.VolumeMount) bool {
			return m.Name == disk && m.MountPath == DiskPath(disk)
		}) {
			t.Errorf("disk %q is not mounted at %q", disk, DiskPath(disk))
		}
	}

	if got, want := set.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted,
		appsv1.RetainPersistentVolumeClaimRetentionPolicyType; got != want {
		t.Errorf("retention on delete = %q, want %q", got, want)
	}

	cluster.Spec.Storage.ReclaimPolicy = fsv1alpha1.ReclaimDelete

	if got, want := nodeStatefulSet(t, cluster).Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted,
		appsv1.DeletePersistentVolumeClaimRetentionPolicyType; got != want {
		t.Errorf("retention on delete = %q, want %q", got, want)
	}
}

func TestStatefulSetTLS(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.S3.TLS.SecretName = tlsSecret

	set := nodeStatefulSet(t, cluster)
	container := set.Spec.Template.Spec.Containers[0]

	if !slices.ContainsFunc(set.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == tlsVolumeName && v.Secret.SecretName == tlsSecret
	}) {
		t.Error("the certificate Secret is not mounted")
	}

	if !slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == tlsVolumeName && m.MountPath == TLSDir
	}) {
		t.Errorf("the certificate is not mounted at %q, where the rendered config looks for it", TLSDir)
	}

	// Probing HTTP against a TLS listener fails, so the scheme has to follow.
	if got, want := container.ReadinessProbe.HTTPGet.Scheme, corev1.URISchemeHTTPS; got != want {
		t.Errorf("probe scheme = %q, want %q", got, want)
	}
}

func TestStatefulSetScheduling(t *testing.T) {
	racks := fsv1alpha1.TopologySpec{
		Racks: []fsv1alpha1.RackSpec{{
			Name:         "a",
			Nodes:        1,
			Zone:         zoneA,
			NodeSelector: map[string]string{"disk": "nvme"},
		}},
	}

	t.Run("rack pins zone and selector", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Topology = racks
		cluster.Spec.PodTemplate.NodeSelector = map[string]string{"pool": teamValue}

		pod := nodeStatefulSet(t, cluster).Spec.Template.Spec

		if got, want := pod.NodeSelector["pool"], teamValue; got != want {
			t.Errorf("cluster-wide selector = %q, want %q", got, want)
		}

		if got, want := pod.NodeSelector["disk"], "nvme"; got != want {
			t.Errorf("rack selector = %q, want %q", got, want)
		}

		term := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]
		if got, want := term.MatchExpressions[0].Values[0], zoneA; got != want {
			t.Errorf("zone = %q, want %q", got, want)
		}
	})

	t.Run("anti-affinity modes", func(t *testing.T) {
		for _, tc := range []struct {
			mode      fsv1alpha1.AntiAffinityMode
			required  bool
			preferred bool
		}{
			{mode: fsv1alpha1.AntiAffinityRequired, required: true},
			{mode: fsv1alpha1.AntiAffinityPreferred, preferred: true},
			{mode: fsv1alpha1.AntiAffinityNone},
		} {
			t.Run(string(tc.mode), func(t *testing.T) {
				cluster := testCluster()
				cluster.Spec.Topology.PodAntiAffinity = tc.mode

				affinity := nodeStatefulSet(t, cluster).Spec.Template.Spec.Affinity

				var anti *corev1.PodAntiAffinity
				if affinity != nil {
					anti = affinity.PodAntiAffinity
				}

				if !tc.required && !tc.preferred {
					if anti != nil {
						t.Error("pods are spread even though spreading is off")
					}

					return
				}

				if anti == nil {
					t.Fatal("pods are not spread over Kubernetes nodes")
				}

				if got := len(anti.RequiredDuringSchedulingIgnoredDuringExecution) == 1; got != tc.required {
					t.Errorf("required spreading = %v, want %v", got, tc.required)
				}

				if got := len(anti.PreferredDuringSchedulingIgnoredDuringExecution) == 1; got != tc.preferred {
					t.Errorf("preferred spreading = %v, want %v", got, tc.preferred)
				}
			})
		}
	})
}

// TestStatefulSetMountsItsOwnConfig checks that each node mounts its own config
// Secret — the whole point of one StatefulSet per node.
func TestStatefulSetMountsItsOwnConfig(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()

	for _, node := range Nodes(cluster) {
		set := NewStatefulSet(cluster, node, testRevision)

		volume := volumeNamed(t, set, configVolumeName)
		if got, want := volume.Secret.SecretName, ConfigSecretName(node.Name); got != want {
			t.Errorf("node %s mounts %q, want %q", node.Name, got, want)
		}

		if volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode&0o040 == 0 {
			t.Errorf("node %s mounts its config unreadable by its own group", node.Name)
		}
	}
}

// TestStatefulSetMountsAWritableStorageRoot covers what a read-only container
// filesystem takes away: fs writes node-local state directly under the storage
// root — since v0.13.0 the object index — and without somewhere to put it the
// node exits at startup with "mkdir /var/lib/fs/cluster: read-only file
// system", which is a crash loop rather than a degraded node.
func TestStatefulSetMountsAWritableStorageRoot(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.WithDefaults()

	set := NewStatefulSet(cluster, Nodes(cluster)[0], testRevision)

	state := claimNamed(t, set, stateVolumeName)
	if got := state.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != fsv1alpha1.DefaultStateSize {
		t.Errorf("state claim = %s, want the default %s", got.String(), fsv1alpha1.DefaultStateSize)
	}

	mounts := set.Spec.Template.Spec.Containers[0].VolumeMounts

	index := -1

	for i, mount := range mounts {
		if mount.MountPath == StorageRoot {
			index = i

			if mount.ReadOnly {
				t.Error("the storage root is mounted read-only; fs writes under it")
			}
		}
	}

	if index == -1 {
		t.Fatalf("no mount at %s", StorageRoot)
	}

	// The disks mount below the root, and the kubelet mounts a nested path
	// after its parent: a disk hidden under a later root mount reads as empty.
	for _, mount := range mounts[:index] {
		if strings.HasPrefix(mount.MountPath, StorageRoot+"/") {
			t.Errorf("%s mounts before the storage root it lives under", mount.MountPath)
		}
	}
}

// collectorEndpoint stands in for an OTLP collector in this package's tests.
const collectorEndpoint = "http://collector.observability:4317"

// protocolHTTP is the OTLP transport a signal picks when it does not want the
// shared one.
const protocolHTTP = "http/protobuf"

// TestStatefulSetSDKEnvironment covers what steers go-faster/sdk in the fs
// container: the exporters it would otherwise point at localhost, the pprof
// listener it serves only when given an address, and the resource attributes
// the operator derives.
func TestStatefulSetSDKEnvironment(t *testing.T) {
	t.Run("without a collector", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.WithDefaults()

		env := environment(nodeStatefulSet(t, cluster))

		// Both default to OTLP in the SDK, which means an upload to
		// localhost:4318 failing every interval on a cluster that never
		// asked for one.
		for _, name := range []string{envTracesExporter, envLogsExporter} {
			if got := env[name]; got != fsv1alpha1.ExporterNone {
				t.Errorf("%s = %q, want %q", name, got, fsv1alpha1.ExporterNone)
			}
		}

		if got := env[envMetricsExport]; got != fsv1alpha1.ExporterPrometheus {
			t.Errorf("%s = %q, want %q: metrics are scraped, not pushed", envMetricsExport, got, fsv1alpha1.ExporterPrometheus)
		}
	})

	t.Run("with a collector", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Observability.OTLP.Endpoint = collectorEndpoint
		cluster.Spec.WithDefaults()

		env := environment(nodeStatefulSet(t, cluster))

		if got := env[envTracesExporter]; got != fsv1alpha1.ExporterOTLP {
			t.Errorf("%s = %q, want %q", envTracesExporter, got, fsv1alpha1.ExporterOTLP)
		}

		// Logs are the exception: fs writes them to stdout, where the
		// cluster already collects them, so a second copy is asked for and
		// not inherited from an endpoint that was set for traces.
		if got := env[envLogsExporter]; got != fsv1alpha1.ExporterNone {
			t.Errorf("%s = %q, want %q by default", envLogsExporter, got, fsv1alpha1.ExporterNone)
		}

		if got := env[envOTLPEndpoint]; got != collectorEndpoint {
			t.Errorf("%s = %q, want %q", envOTLPEndpoint, got, collectorEndpoint)
		}
	})

	t.Run("a signal with its own destination", func(t *testing.T) {
		const logsEndpoint = "http://loki-gateway.observability:4318/otlp/v1/logs"

		cluster := testCluster()
		cluster.Spec.Observability.OTLP.Endpoint = collectorEndpoint
		cluster.Spec.Observability.Logs = fsv1alpha1.SignalSpec{
			Exporter: fsv1alpha1.ExporterOTLP,
			Endpoint: logsEndpoint,
			Protocol: protocolHTTP,
		}
		cluster.Spec.WithDefaults()

		env := environment(nodeStatefulSet(t, cluster))

		// The exporters resolve this themselves, and per the specification a
		// signal's own endpoint wins — so both are rendered, unlike the
		// protocol.
		if got := env[envOTLPEndpoint]; got != collectorEndpoint {
			t.Errorf("%s = %q, want the shared endpoint to stay", envOTLPEndpoint, got)
		}

		if got := env[envOTLPLogsEndpoint]; got != logsEndpoint {
			t.Errorf("%s = %q, want %q", envOTLPLogsEndpoint, got, logsEndpoint)
		}

		// Traces have no endpoint of their own to declare.
		if _, ok := env[envOTLPTracesEndpoint]; ok {
			t.Errorf("%s is set for a signal that uses the shared endpoint", envOTLPTracesEndpoint)
		}
	})

	t.Run("a signal with no shared endpoint behind it", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Observability.Traces = fsv1alpha1.SignalSpec{
			Exporter: fsv1alpha1.ExporterOTLP,
			Endpoint: collectorEndpoint,
		}
		cluster.Spec.WithDefaults()

		env := environment(nodeStatefulSet(t, cluster))

		if _, ok := env[envOTLPEndpoint]; ok {
			t.Errorf("%s is set though no shared endpoint was given", envOTLPEndpoint)
		}

		if got := env[envOTLPTracesEndpoint]; got != collectorEndpoint {
			t.Errorf("%s = %q, want %q", envOTLPTracesEndpoint, got, collectorEndpoint)
		}
	})

}

// TestStatefulSetSDKListeners covers the rest of the SDK's environment: what
// the node says about itself, and what it listens on.
func TestStatefulSetSDKListeners(t *testing.T) {
	t.Run("log level", func(t *testing.T) {
		// Every level the CRD admits has to be one go-faster/sdk can parse:
		// it panics on anything else, so a level that passes admission and
		// kills the process is the failure this enum exists to prevent.
		for _, level := range []string{debugLogLevel, "info", "warn", "error", "dpanic", "panic", "fatal"} {
			cluster := testCluster()
			cluster.Spec.Observability.LogLevel = level
			cluster.Spec.WithDefaults()

			var parsed zapcore.Level
			if err := parsed.UnmarshalText([]byte(level)); err != nil {
				t.Errorf("the API admits %q, which the SDK cannot parse: %v", level, err)
			}

			if got := environment(nodeStatefulSet(t, cluster))[envLogLevel]; got != level {
				t.Errorf("%s = %q, want %q", envLogLevel, got, level)
			}
		}
	})

	t.Run("user resource attributes are added, not substituted", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Observability.ResourceAttributes = map[string]string{
			"deployment.environment": "staging",
			"team":                   "storage",
		}
		cluster.Spec.WithDefaults()

		got := environment(nodeStatefulSet(t, cluster))[envResourceAttrs]

		// The operator's own attributes are what the dashboards read.
		for _, want := range []string{
			"fs.cluster=" + cluster.Name,
			"k8s.namespace.name=" + cluster.Namespace,
			"deployment.environment=staging",
			"team=storage",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s = %q, want it to carry %q", envResourceAttrs, got, want)
			}
		}
	})

	t.Run("metrics pushed instead of scraped", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Observability.OTLP.Endpoint = collectorEndpoint
		cluster.Spec.Observability.Metrics.Exporter = fsv1alpha1.ExporterOTLP
		cluster.Spec.Observability.Metrics.Protocol = protocolHTTP
		cluster.Spec.WithDefaults()

		set := nodeStatefulSet(t, cluster)
		env := environment(set)

		if got := env[envMetricsExport]; got != fsv1alpha1.ExporterOTLP {
			t.Errorf("%s = %q, want %q", envMetricsExport, got, fsv1alpha1.ExporterOTLP)
		}

		// Nothing serves the scrape endpoint any more, so nothing should
		// advertise it either.
		if _, ok := env[envMetricsAddr]; ok {
			t.Errorf("%s is set for an exporter that does not listen", envMetricsAddr)
		}

		for _, port := range set.Spec.Template.Spec.Containers[0].Ports {
			if port.ContainerPort == MetricsPort {
				t.Error("the pod still declares the metrics port")
			}
		}

		if got := env[envOTLPMetricsProto]; got != protocolHTTP {
			t.Errorf("%s = %q, want %q", envOTLPMetricsProto, got, protocolHTTP)
		}

		// Nothing else named a transport, so nothing else renders one: the
		// SDK's own default applies to traces, and logs export nothing.
		for _, name := range []string{envOTLPProtocol, envOTLPTracesProto, envOTLPLogsProto} {
			if _, ok := env[name]; ok {
				t.Errorf("%s is set though nothing asked for it", name)
			}
		}
	})

	t.Run("one transport for every signal", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Observability.OTLP.Endpoint = collectorEndpoint
		cluster.Spec.Observability.OTLP.Protocol = protocolHTTP
		cluster.Spec.WithDefaults()

		env := environment(nodeStatefulSet(t, cluster))

		// Rendered as given: this is the variable a cluster with one
		// collector on one transport sets, and the operator does not
		// second-guess it.
		if got := env[envOTLPProtocol]; got != protocolHTTP {
			t.Errorf("%s = %q, want %q", envOTLPProtocol, got, protocolHTTP)
		}

		for _, name := range []string{envOTLPTracesProto, envOTLPLogsProto, envOTLPMetricsProto} {
			if _, ok := env[name]; ok {
				t.Errorf("%s is redundant when every signal shares the transport", name)
			}
		}
	})

	t.Run("no transport named at all", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Observability.OTLP.Endpoint = collectorEndpoint
		cluster.Spec.WithDefaults()

		env := environment(nodeStatefulSet(t, cluster))

		// An unset protocol stays unset, which is what leaves a per-signal
		// one able to take effect at all — and the SDK's own default (grpc)
		// is the same answer the operator used to write out.
		for _, name := range []string{envOTLPProtocol, envOTLPTracesProto, envOTLPLogsProto, envOTLPMetricsProto} {
			if _, ok := env[name]; ok {
				t.Errorf("%s is set though the spec names no transport", name)
			}
		}
	})

	t.Run("pprof off", func(t *testing.T) {
		cluster := testCluster()
		cluster.Spec.Observability.Pprof = ptr.To(false)
		cluster.Spec.WithDefaults()

		set := nodeStatefulSet(t, cluster)

		if _, ok := environment(set)[envPprofAddr]; ok {
			t.Errorf("%s is set; the SDK would serve pprof anyway", envPprofAddr)
		}

		for _, port := range set.Spec.Template.Spec.Containers[0].Ports {
			if port.ContainerPort == PprofPort {
				t.Error("the pod still declares the pprof port")
			}
		}

		policy := NewNetworkPolicy(cluster, "fs-operator-system")
		for _, rule := range policy.Spec.Ingress {
			for _, port := range rule.Ports {
				if port.Port != nil && port.Port.IntVal == PprofPort {
					t.Error("the NetworkPolicy still opens the pprof port")
				}
			}
		}
	})
}

// volumeNamed returns a pod's volume by name.
func volumeNamed(t *testing.T, set *appsv1.StatefulSet, name string) corev1.Volume {
	t.Helper()

	for _, volume := range set.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			return volume
		}
	}

	t.Fatalf("no volume named %q", name)

	return corev1.Volume{}
}

// claimNamed returns a node's claim template by name.
func claimNamed(t *testing.T, set *appsv1.StatefulSet, name string) corev1.PersistentVolumeClaim {
	t.Helper()

	for _, claim := range set.Spec.VolumeClaimTemplates {
		if claim.Name == name {
			return claim
		}
	}

	t.Fatalf("no claim template named %q", name)

	return corev1.PersistentVolumeClaim{}
}

// environment flattens a pod's container environment for lookup.
func environment(set *appsv1.StatefulSet) map[string]string {
	env := make(map[string]string)

	for _, v := range set.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}

	return env
}
