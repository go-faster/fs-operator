<!--
GENERATED FILE — DO NOT EDIT.

Produced from the field comments in api/v1alpha1 by `make docs-api-ref`.
Document a field by writing its Go comment; CI fails when this is stale.
-->

# API Reference

## Packages
- [fs.go-faster.org/v1alpha1](#fsgo-fasterorgv1alpha1)


## fs.go-faster.org/v1alpha1

Package v1alpha1 contains API Schema definitions for the fs v1alpha1 API group.

### Resource Types
- [FSAccessKey](#fsaccesskey)
- [FSBucket](#fsbucket)
- [FSCluster](#fscluster)



#### AntiAffinityMode

_Underlying type:_ _string_

AntiAffinityMode selects how strictly fs nodes are spread over Kubernetes
nodes.



_Appears in:_
- [TopologySpec](#topologyspec)

| Field | Description |
| --- | --- |
| `Required` |  |
| `Preferred` |  |
| `None` |  |


#### AuthSpec



AuthSpec configures S3 authentication.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `rootCredentialsSecretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | rootCredentialsSecretRef references a Secret with keys "access-key"<br />and "secret-key", granted admin on all buckets. Generated if omitted. |  | Optional: \{\} <br /> |
| `publicReadBuckets` _string array_ | publicReadBuckets may be read anonymously. |  | Optional: \{\} <br /> |


#### ClusterReference



ClusterReference points at an FSCluster in the same namespace (the
namespace is the tenancy boundary; cross-namespace references are not
supported).



_Appears in:_
- [FSAccessKeySpec](#fsaccesskeyspec)
- [FSBucketSpec](#fsbucketspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name of the FSCluster. |  | MaxLength: 63 <br />MinLength: 1 <br />Required: \{\} <br /> |






#### EndpointsStatus



EndpointsStatus lists the cluster's client endpoints.



_Appears in:_
- [FSClusterStatus](#fsclusterstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `s3` _string_ | s3 is the in-cluster S3 endpoint URL. |  | Optional: \{\} <br /> |


#### FSAccessKey



FSAccessKey is the Schema for the fsaccesskeys API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `fs.go-faster.org/v1alpha1` | | |
| `kind` _string_ | `FSAccessKey` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[FSAccessKeySpec](#fsaccesskeyspec)_ | spec defines the desired state of FSAccessKey |  | Required: \{\} <br /> |
| `status` _[FSAccessKeyStatus](#fsaccesskeystatus)_ | status defines the observed state of FSAccessKey |  | Optional: \{\} <br /> |


#### FSAccessKeySpec



FSAccessKeySpec defines the desired state of an FSAccessKey: one S3
credential of a referenced FSCluster with its bucket grants.

The credential comes from exactly one of two sources: generated (the
default — the operator mints it once and owns the Secret named by
secretName) or imported via existingSecretRef (a user-managed Secret, e.g.
minted by an external secret manager; the operator watches it and
propagates rotation to the cluster with a hot reload).



_Appears in:_
- [FSAccessKey](#fsaccesskey)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterRef` _[ClusterReference](#clusterreference)_ | clusterRef is the FSCluster this credential belongs to. Immutable. |  | Required: \{\} <br /> |
| `secretName` _string_ | secretName names the operator-owned Secret the generated credential<br />is written to (keys: access-key, secret-key, endpoint). Defaults to<br /><metadata.name>-credentials. Immutable; not allowed together with<br />existingSecretRef. |  | MaxLength: 253 <br />Optional: \{\} <br /> |
| `existingSecretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | existingSecretRef imports a credential from a user-managed Secret<br />with keys "access-key" and "secret-key" (secret-key must be at least<br />16 characters, refused otherwise). The operator never writes to this<br />Secret; external rotation propagates to the cluster via hot reload. |  | Optional: \{\} <br /> |
| `grants` _[GrantSpec](#grantspec) array_ | grants authorize the key for buckets matching a glob, up to a<br />permission level. |  | MinItems: 1 <br />Required: \{\} <br /> |


#### FSAccessKeyStatus



FSAccessKeyStatus defines the observed state of an FSAccessKey.



_Appears in:_
- [FSAccessKey](#fsaccesskey)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | observedGeneration is the last spec generation the controller acted<br />on. |  | Optional: \{\} <br /> |
| `accessKey` _string_ | accessKey is the non-secret half of the credential, for reference. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | conditions represent the current state of the FSAccessKey (Ready). |  | Optional: \{\} <br /> |


#### FSBucket



FSBucket is the Schema for the fsbuckets API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `fs.go-faster.org/v1alpha1` | | |
| `kind` _string_ | `FSBucket` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[FSBucketSpec](#fsbucketspec)_ | spec defines the desired state of FSBucket |  | Required: \{\} <br /> |
| `status` _[FSBucketStatus](#fsbucketstatus)_ | status defines the observed state of FSBucket |  | Optional: \{\} <br /> |


#### FSBucketSpec



FSBucketSpec defines the desired state of an FSBucket: an S3 bucket in a
referenced FSCluster.



_Appears in:_
- [FSBucket](#fsbucket)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterRef` _[ClusterReference](#clusterreference)_ | clusterRef is the FSCluster this bucket lives in. Immutable. |  | Required: \{\} <br /> |
| `bucketName` _string_ | bucketName is the S3 bucket name; defaults to metadata.name.<br />Immutable. |  | MaxLength: 63 <br />Optional: \{\} <br /> |
| `scheme` _string_ | scheme is how this bucket's objects are stored: "rf3" (three<br />replicas, the default) or "ec:k,m" (erasure coded, e.g. "ec:4,2").<br />An erasure scheme needs the cluster's spec.layout.widths to include<br />k+m, and is refused on a single-node cluster. A change applies to data<br />written from then on; existing data keeps the scheme it was written<br />with. |  | Pattern: `^(rf3\|ec:[1-9][0-9]*,[1-9][0-9]*)?$` <br />Optional: \{\} <br /> |
| `reclaimPolicy` _[ReclaimPolicy](#reclaimpolicy)_ | reclaimPolicy controls what happens to the bucket when this resource<br />is deleted. Retain leaves the bucket and its data; Delete removes the<br />bucket, which succeeds only once it is empty (the controller retries<br />and reports Ready=False/BucketNotEmpty until then). | Retain | Enum: [Retain Delete] <br />Optional: \{\} <br /> |


#### FSBucketStatus



FSBucketStatus defines the observed state of an FSBucket.



_Appears in:_
- [FSBucket](#fsbucket)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | observedGeneration is the last spec generation the controller acted<br />on. |  | Optional: \{\} <br /> |
| `scheme` _string_ | scheme is the bucket's effective replication scheme. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | conditions represent the current state of the FSBucket (Ready). |  | Optional: \{\} <br /> |


#### FSCluster



FSCluster is the Schema for the fsclusters API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `fs.go-faster.org/v1alpha1` | | |
| `kind` _string_ | `FSCluster` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[FSClusterSpec](#fsclusterspec)_ | spec defines the desired state of FSCluster |  | Required: \{\} <br /> |
| `status` _[FSClusterStatus](#fsclusterstatus)_ | status defines the observed state of FSCluster |  | Optional: \{\} <br /> |


#### FSClusterSpec



FSClusterSpec defines the desired state of a go-faster/fs cluster: a set of
storage nodes spread over failure domains (racks), sharing a layout that
assigns every partition of the data to nodes.



_Appears in:_
- [FSCluster](#fscluster)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _[ImageSpec](#imagespec)_ | image is the fs container image to run on every node. Defaults to the<br />pinned fs release this operator version is validated against. | \{  \} | Optional: \{\} <br /> |
| `topology` _[TopologySpec](#topologyspec)_ | topology declares the cluster's nodes and failure domains. |  | Required: \{\} <br /> |
| `storage` _[StorageSpec](#storagespec)_ | storage sizes each node's data volume. |  | Required: \{\} <br /> |
| `layout` _[LayoutSpec](#layoutspec)_ | layout tunes the cluster layout the operator applies. Ignored by a<br />single-node cluster, which has no layout to apply. |  | Optional: \{\} <br /> |
| `clusterSecretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | clusterSecretRef references a Secret with key "secret" holding the<br />shared cluster secret (HMAC peer auth, min 16 characters). Generated<br />if omitted. Immutable: fs has no secret rotation; mixed secrets<br />partition the cluster. |  | Optional: \{\} <br /> |
| `auth` _[AuthSpec](#authspec)_ | auth configures S3 authentication. |  | Optional: \{\} <br /> |
| `s3` _[S3Spec](#s3spec)_ | s3 configures how the S3 endpoint is exposed. |  | Optional: \{\} <br /> |
| `updatePolicy` _[UpdatePolicySpec](#updatepolicyspec)_ | updatePolicy tunes rolling changes. |  | Optional: \{\} <br /> |
| `observability` _[ObservabilitySpec](#observabilityspec)_ | observability configures telemetry of the fs pods. |  | Optional: \{\} <br /> |
| `networkPolicy` _boolean_ | networkPolicy, when true, creates a NetworkPolicy restricting the peer<br />(7080) and admin (8090) ports to cluster pods and the operator. S3<br />stays unrestricted. |  | Optional: \{\} <br /> |
| `podTemplate` _[PodTemplate](#podtemplate)_ | podTemplate carries pod-level knobs applied uniformly to every node's<br />StatefulSet. |  | Optional: \{\} <br /> |


#### FSClusterStatus



FSClusterStatus defines the observed state of an FSCluster.



_Appears in:_
- [FSCluster](#fscluster)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | observedGeneration is the last spec generation the controller acted<br />on. |  | Optional: \{\} <br /> |
| `nodes` _integer_ | nodes is the desired node count. |  | Optional: \{\} <br /> |
| `readyNodes` _integer_ | readyNodes is the number of node pods that are Ready. |  | Optional: \{\} <br /> |
| `upNodes` _integer_ | upNodes is the number of nodes the cluster's own view reports up:<br />reachable by their peers, which a Ready pod alone does not prove. |  | Optional: \{\} <br /> |
| `configurationRevision` _string_ | configurationRevision is the hash of the desired rendered configs. |  | Optional: \{\} <br /> |
| `statefulSetRevision` _string_ | statefulSetRevision is the hash of the desired pod templates. |  | Optional: \{\} <br /> |
| `currentRevision` _string_ | currentRevision is the revision every node has converged to. |  | Optional: \{\} <br /> |
| `updateRevision` _string_ | updateRevision is the revision being rolled out. |  | Optional: \{\} <br /> |
| `layout` _[LayoutStatus](#layoutstatus)_ | layout is the cluster layout as fs reports it. |  | Optional: \{\} <br /> |
| `update` _[UpdateStatus](#updatestatus)_ | update is present while a rolling change is in flight. |  | Optional: \{\} <br /> |
| `endpoints` _[EndpointsStatus](#endpointsstatus)_ | endpoints are the cluster's client endpoints. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | conditions represent the current state of the FSCluster. See the<br />documented condition types (SpecValid, ReconcileSucceeded, Ready,<br />NodesHealthy, ClusterSizeAligned, ConfigurationInSync, Converged). |  | Optional: \{\} <br /> |


#### GrantSpec



GrantSpec authorizes an access key for buckets matching Bucket (a glob) up
to Permission.



_Appears in:_
- [FSAccessKeySpec](#fsaccesskeyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `bucket` _string_ | bucket is a glob matched against bucket names (fs grant semantics). |  | MinLength: 1 <br />Required: \{\} <br /> |
| `permission` _string_ | permission is the maximum permitted operation class. |  | Enum: [read write admin] <br />Required: \{\} <br /> |


#### ImageSpec



ImageSpec identifies the fs container image.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `repository` _string_ | repository is the image repository. | ghcr.io/go-faster/fs | Optional: \{\} <br /> |
| `tag` _string_ | tag is the image tag. Defaults to the pinned fs release this operator<br />version is validated against — always set a pinned version, never a<br />floating tag: cluster upgrades are deliberate, one-node-at-a-time<br />operations. | v0.15.0 | MinLength: 1 <br />Optional: \{\} <br /> |
| `digest` _string_ | digest pins the image by content instead of by tag, as<br />"sha256:<hex>". When set it wins over tag, and the nodes run<br />repository@digest — the reference a mirror cannot silently change<br />under a cluster. A digest already written into repository is honoured<br />too, which is how the chart pins the operator's own image. |  | Pattern: `^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-fA-F0-9]\{32,128\}$` <br />Optional: \{\} <br /> |
| `pullPolicy` _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#pullpolicy-v1-core)_ | pullPolicy is the image pull policy. | IfNotPresent | Enum: [Always IfNotPresent Never] <br />Optional: \{\} <br /> |
| `pullSecrets` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core) array_ | pullSecrets are image pull secrets for the fs pods. |  | Optional: \{\} <br /> |


#### LayoutSpec



LayoutSpec tunes the layout: which partition of the data each node holds.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `widths` _integer array_ | widths are the slot counts the layout spreads over distinct failure<br />domains: 3 for replicated data (rf3), and K+M for every erasure scheme<br />a bucket uses, so that an FSBucket with scheme "ec:4,2" needs 6 here.<br />Each width needs at least that many nodes. Defaults to [3]. Adding a<br />width moves data; removing one that a bucket still uses is refused by<br />fs. |  | MaxItems: 8 <br />MinItems: 1 <br />items:Maximum: 64 <br />items:Minimum: 1 <br />Optional: \{\} <br /> |


#### LayoutStatus



LayoutStatus is the cluster layout as fs reports it.



_Appears in:_
- [FSClusterStatus](#fsclusterstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `version` _integer_ | version is the layout version the cluster has adopted; every applied<br />change increments it. |  | Optional: \{\} <br /> |
| `members` _integer_ | members is the number of nodes the layout gives data to. |  | Optional: \{\} <br /> |
| `widths` _integer array_ | widths are the slot counts the layout spreads for. |  | Optional: \{\} <br /> |
| `retainedVersions` _integer array_ | retainedVersions are older versions still in transition: data is<br />moving to where the current version puts it, and writes go to both.<br />Empty when nothing is moving. |  | Optional: \{\} <br /> |


#### MetricsSpec



MetricsSpec selects how metrics leave the node. It is its own type because
metrics have an exporter the other signals do not: the scrape endpoint.



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `exporter` _string_ | exporter is "prometheus" (the default: served on the node's metrics<br />port for a scraper), "otlp" (pushed to an endpoint) or "none". |  | Enum: [prometheus otlp none] <br />Optional: \{\} <br /> |
| `endpoint` _string_ | endpoint overrides otlp.endpoint for metrics alone, under the same<br />path rule as the other signals. Ignored unless the exporter is<br />"otlp". |  | Optional: \{\} <br /> |
| `protocol` _string_ | protocol overrides otlp.protocol for metrics alone. Ignored unless<br />the exporter is "otlp". |  | Enum: [grpc http/protobuf] <br />Optional: \{\} <br /> |


#### OTLPSpec



OTLPSpec is the OTLP exporter destination shared by every signal.

What is not here — Pyroscope, propagators, export intervals, pprof routes,
the OTEL_GO_X_* switches — is reachable through podTemplate.extraEnv, which
is applied last and therefore wins over what the operator sets. See the
SDK's reference table: https://github.com/go-faster/sdk#reference



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | endpoint is OTEL_EXPORTER_OTLP_ENDPOINT: where every signal exported<br />over OTLP is sent unless it names its own. A signal whose exporter is<br />"otlp" needs one of the two, since without a destination the SDK ships<br />to localhost:4318 and logs a failed upload every interval. |  | Optional: \{\} <br /> |
| `protocol` _string_ | protocol is OTEL_EXPORTER_OTLP_PROTOCOL, the transport every OTLP<br />signal uses. Empty leaves it unset, which is the SDK's own default of<br />grpc.<br />It cannot be combined with a per-signal protocol, and that is fs's<br />SDK rather than a choice made here: it reads this variable first and<br />consults OTEL_EXPORTER_OTLP_<SIGNAL>_PROTOCOL only when it is unset,<br />so setting both would leave the per-signal value inert. Set this one<br />for a cluster that speaks one transport, or the per-signal ones for a<br />cluster that does not. |  | Enum: [grpc http/protobuf] <br />Optional: \{\} <br /> |


#### ObservabilitySpec



ObservabilitySpec configures telemetry of the fs pods.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `otlp` _[OTLPSpec](#otlpspec)_ | otlp is the OTLP destination every signal exported over OTLP is sent<br />to, and the protocol they use unless a signal names its own. |  | Optional: \{\} <br /> |
| `traces` _[SignalSpec](#signalspec)_ | traces selects the trace exporter, its destination and its transport.<br />Defaults to "otlp" when an endpoint is set and "none" when none is —<br />the SDK's own default is "otlp" unconditionally, which without a<br />destination means an upload to localhost failing every interval. |  | Optional: \{\} <br /> |
| `logs` _[SignalSpec](#signalspec)_ | logs selects the log exporter and its transport. Defaults to "none"<br />even with an endpoint set: fs logs to stdout, where the cluster's log<br />pipeline already collects it, so a second copy over OTLP is a choice<br />and not the obvious consequence of having a collector. |  | Optional: \{\} <br /> |
| `metrics` _[MetricsSpec](#metricsspec)_ | metrics selects the metric exporter and its transport. Defaults to<br />"prometheus": Kubernetes collects metrics by scraping, the operator<br />gives every node a metrics port for it, and podMonitor scrapes that<br />port. Choosing "otlp" or "none" retires the port with it. |  | Optional: \{\} <br /> |
| `logLevel` _string_ | logLevel is OTEL_LOG_LEVEL, the level fs logs at. debug additionally<br />turns on per-request logging in fs itself, which on a busy endpoint is<br />a line per request.<br />The enum is the set go-faster/sdk can parse (zapcore levels), and it<br />earns its keep: the SDK panics on a level it does not recognise, so a<br />typo here would otherwise be a crash loop across every node rather<br />than a rejected field. Levels above error exist because the SDK has<br />them; a cluster that logs nothing below panic is not one you can<br />operate. | info | Enum: [debug info warn error dpanic panic fatal] <br />Optional: \{\} <br /> |
| `podMonitor` _boolean_ | podMonitor creates a PodMonitor for the fs pods' Prometheus metrics<br />(requires the monitoring.coreos.com API group). |  | Optional: \{\} <br /> |
| `pprof` _boolean_ | pprof serves go-faster/sdk's pprof endpoints on port 9010. On by<br />default, which is not the SDK's own default: a cluster that is<br />misbehaving is when profiles are wanted, and that is a bad moment to<br />discover the listener has to be turned on and the nodes restarted.<br />Turn it off where an open profiling endpoint is not acceptable — the<br />container port and its NetworkPolicy rule go with it. | true | Optional: \{\} <br /> |
| `resourceAttributes` _object (keys:string, values:string)_ | resourceAttributes are added to OTEL_RESOURCE_ATTRIBUTES, alongside<br />the ones the operator derives (service, cluster, namespace, node,<br />rack). Setting the variable through podTemplate.extraEnv replaces<br />those instead, which is what the dashboards and the PodMonitor read. |  | Optional: \{\} <br /> |


#### PodTemplate



PodTemplate carries pod-level knobs applied uniformly to every node's
StatefulSet.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#resourcerequirements-v1-core)_ | resources are the fs container's resource requirements. |  | Optional: \{\} <br /> |
| `nodeSelector` _object (keys:string, values:string)_ | nodeSelector applies to every node's pod (racks add their own on<br />top). |  | Optional: \{\} <br /> |
| `tolerations` _[Toleration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#toleration-v1-core) array_ | tolerations apply to every node's pod. |  | Optional: \{\} <br /> |
| `priorityClassName` _string_ | priorityClassName applies to every node's pod. |  | Optional: \{\} <br /> |
| `annotations` _object (keys:string, values:string)_ | annotations are added to every node's pod. |  | Optional: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | labels are added to every node's pod. |  | Optional: \{\} <br /> |
| `extraEnv` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#envvar-v1-core) array_ | extraEnv appends environment variables to the fs container. |  | Optional: \{\} <br /> |


#### RackSpec



RackSpec is one failure domain and its scheduling constraints.



_Appears in:_
- [TopologySpec](#topologyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name identifies the rack; it becomes the fs rack label and part of<br />node names. Immutable per entry (renaming a rack is a decommission<br />plus a new rack). |  | MaxLength: 15 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `nodes` _integer_ | nodes is the number of fs nodes in this rack. |  | Maximum: 16 <br />Minimum: 1 <br />Required: \{\} <br /> |
| `zone` _string_ | zone pins the rack's nodes to a topology.kubernetes.io/zone value<br />(sugar for nodeSelector), and is the fs zone of the rack's nodes:<br />racks sharing a zone are one failure domain at the zone level. |  | Optional: \{\} <br /> |
| `nodeSelector` _object (keys:string, values:string)_ | nodeSelector pins the rack's nodes to matching Kubernetes nodes.<br />Merged over zone. |  | Optional: \{\} <br /> |


#### ReclaimPolicy

_Underlying type:_ _string_

ReclaimPolicy controls the fate of data-bearing resources on removal.



_Appears in:_
- [FSBucketSpec](#fsbucketspec)
- [StorageSpec](#storagespec)

| Field | Description |
| --- | --- |
| `Retain` |  |
| `Delete` |  |


#### S3ServiceSpec



S3ServiceSpec shapes the S3 client Service.



_Appears in:_
- [S3Spec](#s3spec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[ServiceType](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#servicetype-v1-core)_ | type is the Service type. | ClusterIP | Enum: [ClusterIP NodePort LoadBalancer] <br />Optional: \{\} <br /> |
| `port` _integer_ | port is the S3 port. | 8080 | Maximum: 65535 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `annotations` _object (keys:string, values:string)_ | annotations are added to the Service (e.g. for load-balancer<br />controllers). |  | Optional: \{\} <br /> |


#### S3Spec



S3Spec configures the S3 endpoint exposure.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `service` _[S3ServiceSpec](#s3servicespec)_ | service shapes the client Service in front of the S3 listeners. |  | Optional: \{\} <br /> |
| `tls` _[S3TLSSpec](#s3tlsspec)_ | tls terminates TLS in fs itself using a kubernetes.io/tls Secret.<br />Certificate renewals hot-reload without restarts. |  | Optional: \{\} <br /> |


#### S3TLSSpec



S3TLSSpec enables TLS termination in fs.



_Appears in:_
- [S3Spec](#s3spec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretName` _string_ | secretName names a kubernetes.io/tls Secret with the serving<br />certificate. Empty serves plaintext. |  | Optional: \{\} <br /> |


#### SignalSpec



SignalSpec selects how one telemetry signal leaves the node.



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `exporter` _string_ | exporter is where the signal goes: "otlp" or "none". |  | Enum: [otlp none] <br />Optional: \{\} <br /> |
| `endpoint` _string_ | endpoint overrides otlp.endpoint for this signal alone. Note the<br />OpenTelemetry rule the exporters implement: the shared endpoint has<br />"/v1/<signal>" appended over HTTP, a per-signal endpoint is used<br />exactly as written, path included. |  | Optional: \{\} <br /> |
| `protocol` _string_ | protocol overrides otlp.protocol for this signal alone, which is what<br />a collector that speaks one transport on one port needs. |  | Enum: [grpc http/protobuf] <br />Optional: \{\} <br /> |


#### StorageSpec



StorageSpec sizes each node's data volume and says how it is reclaimed.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#quantity-resource-api)_ | size is the capacity of each node's data volume, which holds<br />everything the node stores: metadata, blocks and runtime state. It is<br />also the capacity the node joins the layout with, so the layout gives<br />a node a share of the data in proportion to it. It may only grow, and<br />growing it requires the StorageClass to allow volume expansion. |  | Required: \{\} <br /> |
| `storageClass` _string_ | storageClass selects the StorageClass of the data volumes; empty<br />uses the cluster default. Metadata is written on every request, so<br />this is worth putting on the fastest class available. |  | Optional: \{\} <br /> |
| `reclaimPolicy` _[ReclaimPolicy](#reclaimpolicy)_ | reclaimPolicy controls what happens to a node's volume when the node<br />is removed or the cluster is deleted. | Retain | Enum: [Retain Delete] <br />Optional: \{\} <br /> |


#### TopologySpec



TopologySpec declares the cluster's nodes and failure domains. Exactly one
of nodes or racks must be set.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodes` _integer_ | nodes is the flat topology: N nodes, each its own failure domain.<br />One node is a development install: one volume, no peers, no<br />replication and no failure tolerance. Two is refused: a replicated<br />layout needs three nodes. |  | Maximum: 16 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `racks` _[RackSpec](#rackspec) array_ | racks are explicit failure domains. Every node of a rack joins the<br />layout with the rack's name as its fs rack and the rack's zone as its<br />fs zone, and the layout spreads each partition over zones first, then<br />racks. Rack membership is declared here and pinned with node affinity<br />— it is never derived from where a pod happens to be scheduled. |  | MaxItems: 16 <br />MinItems: 1 <br />Optional: \{\} <br /> |
| `podAntiAffinity` _[AntiAffinityMode](#antiaffinitymode)_ | podAntiAffinity spreads fs nodes over distinct Kubernetes nodes.<br />Required (the default) keeps the failure model honest; use Preferred<br />or None only for dev clusters. | Required | Enum: [Required Preferred None] <br />Optional: \{\} <br /> |


#### UpdatePhase

_Underlying type:_ _string_

UpdatePhase is a phase of the rolling-change state machine.



_Appears in:_
- [UpdateStatus](#updatestatus)

| Field | Description |
| --- | --- |
| `Preflight` |  |
| `RollingNodes` |  |
| `Draining` | UpdatePhaseDraining is a node being removed: out of the layout and<br />still running while the cluster moves its data elsewhere (SPEC §8.4).<br /> |


#### UpdatePolicySpec



UpdatePolicySpec tunes rolling changes.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `convergenceTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | convergenceTimeout bounds how long the operator waits, between node<br />restarts, for the cluster to reconverge (pod ready, every node up and<br />on the current layout, no layout change in transition). On timeout<br />the rollout halts — it never proceeds to another node while the<br />cluster is unconverged — and resumes automatically when the gate<br />passes. | 30m | Optional: \{\} <br /> |


#### UpdateStatus



UpdateStatus describes the rolling change in flight.



_Appears in:_
- [FSClusterStatus](#fsclusterstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[UpdatePhase](#updatephase)_ | phase is the state-machine phase. |  | Enum: [Preflight RollingNodes Draining] <br />Optional: \{\} <br /> |
| `node` _string_ | node is the node currently being replaced or removed. |  | Optional: \{\} <br /> |
| `startedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | startedAt is when the rolling change started. |  | Optional: \{\} <br /> |


