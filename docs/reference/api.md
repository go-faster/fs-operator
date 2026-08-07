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






#### DiskSpec



DiskSpec is one per-node disk.



_Appears in:_
- [StorageSpec](#storagespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name identifies the disk within each node (the fs disk id). Immutable. |  | MaxLength: 15 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#quantity-resource-api)_ | size is the capacity requested for each node's PVC of this disk. It<br />may only grow, and growing it requires the StorageClass to allow<br />volume expansion. The growth check is the controller's: expressing it<br />in CEL costs more than the API server's validation budget allows for a<br />list of this size. |  | Required: \{\} <br /> |
| `storageClass` _string_ | storageClass selects the StorageClass for this disk's PVCs; empty uses<br />the cluster default. |  | Optional: \{\} <br /> |
| `weight` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#quantity-resource-api)_ | weight is the disk's relative capacity weight for placement (fs<br />semantics; default 1). Expressed as a quantity so fractional weights<br />like "0.5" are possible. Changing weights rolls the cluster. |  | Optional: \{\} <br /> |


#### EndpointsStatus



EndpointsStatus lists the cluster's client endpoints.



_Appears in:_
- [FSClusterStatus](#fsclusterstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `s3` _string_ | s3 is the in-cluster S3 endpoint URL. |  | Optional: \{\} <br /> |


#### EtcdSpec



EtcdSpec configures the etcd control plane connection.

CEL only rules out having both: a clustered topology needs exactly one, a
single-node cluster needs neither, and which of those applies is a
cross-field question CEL cannot see from here. internal/validation decides
it, at admission and again in the controller.

The prefix rule spells out the absent case: a rule that reads self.prefix
directly fails evaluation — and rejects every update, including status
writes — while the field is unset, and setting it later would move the
cluster's keys.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `external` _[ExternalEtcdSpec](#externaletcdspec)_ | external points the cluster at user-operated etcd endpoints. This is<br />the production mode, and the only supported one. |  | Optional: \{\} <br /> |
| `managed` _[ManagedEtcdSpec](#managedetcdspec)_ | managed asks the operator to run a minimal etcd for this cluster.<br />For development and demos only, permanently. It has no backups, no<br />defrag automation, no member replacement and no restore path: losing<br />its volume loses the cluster's control plane, and with it the sealed<br />credentials and the topology. The operator says so on every cluster<br />that uses it (Ready condition message plus an event) — that is not a<br />warning that will be softened once it is "good enough", because etcd<br />lifecycle management is its own discipline and this is not it.<br />Production clusters use external (SPEC §2). |  | Optional: \{\} <br /> |
| `prefix` _string_ | prefix namespaces this cluster's keys in etcd. Defaults to<br />/fs/<namespace>/<name>. Immutable. |  | MaxLength: 250 <br />Optional: \{\} <br /> |
| `ttl` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | ttl is the node registration lease: how long a dead node lingers in<br />the topology. Zero defers to the fs default (10s); minimum 1s. |  | Optional: \{\} <br /> |
| `cleanupOnDelete` _boolean_ | cleanupOnDelete deletes the cluster's keys under prefix when the<br />FSCluster is deleted. Off by default: with a shared etcd, prefer<br />leaving state over destroying a neighbour's. |  | Optional: \{\} <br /> |


#### EtcdTLSSpec



EtcdTLSSpec is the client TLS material for reaching etcd.



_Appears in:_
- [ExternalEtcdSpec](#externaletcdspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretName` _string_ | secretName names a Secret with the trust material. Key "ca.crt" is the<br />bundle etcd's certificate is verified against; "tls.crt" and "tls.key",<br />when present, are this client's certificate for mutual TLS — a<br />kubernetes.io/tls Secret with a ca.crt added serves both.<br />Setting it turns TLS on. Omit it against an https endpoint to verify<br />with the system roots instead. |  | Optional: \{\} <br /> |
| `serverName` _string_ | serverName overrides the name verified against etcd's certificate, for<br />reaching it through an address the certificate does not name. |  | Optional: \{\} <br /> |
| `insecureSkipVerify` _boolean_ | insecureSkipVerify disables verification of etcd's certificate. It<br />makes TLS decorative — anything on the path can impersonate the<br />cluster's control plane — and exists for development against<br />self-signed certificates only. |  | Optional: \{\} <br /> |


#### ExternalEtcdSpec



ExternalEtcdSpec is a user-operated etcd.



_Appears in:_
- [EtcdSpec](#etcdspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoints` _string array_ | endpoints are the etcd client URLs. An "https://" endpoint is served<br />over TLS; fs enables it from the scheme alone, so an https endpoint<br />works without tls below (verifying against the system roots). |  | MinItems: 1 <br />items:MinLength: 1 <br />Required: \{\} <br /> |
| `tls` _[EtcdTLSSpec](#etcdtlsspec)_ | tls secures the connection to etcd. etcd holds the node registry and<br />the cluster's credential store, sealed with the cluster secret, so<br />anything that can write to it can reshape the cluster. |  | Optional: \{\} <br /> |
| `authSecretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | authSecretRef references a Secret with keys "username" and "password"<br />for etcd role-based authentication. Both keys are required. The<br />credentials reach the nodes as environment variables, never through<br />the rendered configuration, so they are not written to a config file. |  | Optional: \{\} <br /> |


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
| `scheme` _string_ | scheme overrides the cluster's default replication scheme for this<br />bucket's objects: "rf2.5", "rf3" or "ec:k,m" (e.g. "ec:4,2"). Empty<br />applies the cluster default. Changing it affects new writes cluster-wide<br />within seconds; existing objects convert through repair/rebalance. |  | Pattern: `^(rf2\.5\|rf3\|ec:[0-9]+,[0-9]+)?$` <br />Optional: \{\} <br /> |
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
storage nodes spread over failure domains (racks), replicating objects at
write quorum with an etcd control plane.



_Appears in:_
- [FSCluster](#fscluster)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _[ImageSpec](#imagespec)_ | image is the fs container image to run on every node. Defaults to the<br />pinned fs release this operator version is validated against. | \{  \} | Optional: \{\} <br /> |
| `scheme` _string_ | scheme is the default replication scheme for all buckets: "rf2.5"<br />(2 replicas + half parity), "rf3" (3 replicas) or "ec:k,m"<br />(Reed-Solomon). Changeable at runtime: new writes use it immediately<br />and existing objects converge via repair/rebalance. The controller<br />refuses a scheme the topology cannot host (distinct failure domains<br />below the scheme requirement). Ignored by a single-node cluster,<br />which stores objects on one disk and replicates nothing. | rf2.5 | Pattern: `^(rf2\.5\|rf3\|ec:[1-9][0-9]*,[1-9][0-9]*)$` <br />Optional: \{\} <br /> |
| `topology` _[TopologySpec](#topologyspec)_ | topology declares the cluster's nodes and failure domains. |  | Required: \{\} <br /> |
| `storage` _[StorageSpec](#storagespec)_ | storage declares each node's disks. Every disk becomes one<br />PersistentVolumeClaim per node. |  | Required: \{\} <br /> |
| `etcd` _[EtcdSpec](#etcdspec)_ | etcd configures the control plane the cluster registers in. Required<br />for a clustered topology; a single-node cluster runs fs's<br />non-clustered filesystem backend, which has no control plane, and<br />must leave it unset. |  | Optional: \{\} <br /> |
| `clusterSecretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | clusterSecretRef references a Secret with key "secret" holding the<br />shared cluster secret (HMAC peer auth, min 16 characters). Generated<br />if omitted. Immutable: fs has no secret rotation; mixed secrets<br />partition the cluster. |  | Optional: \{\} <br /> |
| `auth` _[AuthSpec](#authspec)_ | auth configures S3 authentication. |  | Optional: \{\} <br /> |
| `s3` _[S3Spec](#s3spec)_ | s3 configures how the S3 endpoint is exposed. |  | Optional: \{\} <br /> |
| `rebalance` _[RebalanceSpec](#rebalancespec)_ | rebalance tunes the automatic rebalancer. Zero values defer to the fs<br />defaults. |  | Optional: \{\} <br /> |
| `integrity` _[IntegritySpec](#integrityspec)_ | integrity configures object integrity checking (scrub). |  | Optional: \{\} <br /> |
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
| `registeredNodes` _integer_ | registeredNodes is the number of nodes present in the etcd topology. |  | Optional: \{\} <br /> |
| `configurationRevision` _string_ | configurationRevision is the hash of the desired rendered configs. |  | Optional: \{\} <br /> |
| `statefulSetRevision` _string_ | statefulSetRevision is the hash of the desired pod templates. |  | Optional: \{\} <br /> |
| `currentRevision` _string_ | currentRevision is the revision every node has converged to. |  | Optional: \{\} <br /> |
| `updateRevision` _string_ | updateRevision is the revision being rolled out. |  | Optional: \{\} <br /> |
| `schemaVersion` _[SchemaVersionStatus](#schemaversionstatus)_ | schemaVersion reports the fs schema versions in play. |  | Optional: \{\} <br /> |
| `rebalance` _[RebalanceStatus](#rebalancestatus)_ | rebalance summarizes rebalance/repair across nodes. |  | Optional: \{\} <br /> |
| `update` _[UpdateStatus](#updatestatus)_ | update is present while a rolling change is in flight. |  | Optional: \{\} <br /> |
| `endpoints` _[EndpointsStatus](#endpointsstatus)_ | endpoints are the cluster's client endpoints. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | conditions represent the current state of the FSCluster. See the<br />documented condition types (SpecValid, ReconcileSucceeded, Ready,<br />NodesHealthy, ClusterSizeAligned, ConfigurationInSync, Converged,<br />SchemaCurrent). |  | Optional: \{\} <br /> |


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
| `tag` _string_ | tag is the image tag. Defaults to the pinned fs release this operator<br />version is validated against — always set a pinned version, never a<br />floating tag: cluster upgrades are deliberate, one-node-at-a-time<br />operations. | v0.12.0 | MinLength: 1 <br />Optional: \{\} <br /> |
| `digest` _string_ | digest pins the image by content instead of by tag, as<br />"sha256:<hex>". When set it wins over tag, and the nodes run<br />repository@digest — the reference a mirror cannot silently change<br />under a cluster. A digest already written into repository is honoured<br />too, which is how the chart pins the operator's own image. |  | Pattern: `^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-fA-F0-9]\{32,128\}$` <br />Optional: \{\} <br /> |
| `pullPolicy` _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#pullpolicy-v1-core)_ | pullPolicy is the image pull policy. | IfNotPresent | Enum: [Always IfNotPresent Never] <br />Optional: \{\} <br /> |
| `pullSecrets` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core) array_ | pullSecrets are image pull secrets for the fs pods. |  | Optional: \{\} <br /> |


#### IntegritySpec



IntegritySpec configures object integrity checking.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `verifyOnRead` _boolean_ | verifyOnRead recomputes and checks each object's checksum before<br />serving it (costs a full extra read per GET). |  | Optional: \{\} <br /> |
| `scrubInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | scrubInterval, if positive, runs a background scrubber walking all<br />objects on this cadence. Zero disables it. |  | Optional: \{\} <br /> |
| `scrubQuarantine` _boolean_ | scrubQuarantine moves corrupt objects aside instead of only reporting<br />them. |  | Optional: \{\} <br /> |


#### ManagedEtcdImageSpec



ManagedEtcdImageSpec pins the managed etcd image.



_Appears in:_
- [ManagedEtcdSpec](#managedetcdspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `repository` _string_ | repository is the etcd image repository. | quay.io/coreos/etcd | Optional: \{\} <br /> |
| `tag` _string_ | tag is the etcd image tag. | v3.5.17 | MinLength: 1 <br />Optional: \{\} <br /> |
| `pullPolicy` _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#pullpolicy-v1-core)_ | pullPolicy is the image pull policy. | IfNotPresent | Enum: [Always IfNotPresent Never] <br />Optional: \{\} <br /> |


#### ManagedEtcdSpec



ManagedEtcdSpec is the operator-run development etcd (SPEC §2). Everything
here is deliberately small: it exists so `kubectl apply` of an example
produces a working cluster, not so anyone runs it in production.



_Appears in:_
- [EtcdSpec](#etcdspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | replicas is the etcd member count: 1 for a laptop, 3 for a demo that<br />should survive a node restart. Even counts cannot form a quorum and are<br />refused. Immutable, because this etcd has no member-replacement path:<br />growing it would need a join dance the operator does not implement. |  | Enum: [1 3] <br />Optional: \{\} <br /> |
| `image` _[ManagedEtcdImageSpec](#managedetcdimagespec)_ | image is the etcd image. Defaults to the release this operator is<br />tested against. |  | Optional: \{\} <br /> |
| `storage` _[ManagedEtcdStorageSpec](#managedetcdstoragespec)_ | storage sizes each member's volume. etcd holds only control-plane<br />state — the node registry, cursors, the sealed key store — so this is<br />kilobytes of data with room for history and compaction. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#resourcerequirements-v1-core)_ | resources are the etcd container's resource requirements. |  | Optional: \{\} <br /> |


#### ManagedEtcdStorageSpec



ManagedEtcdStorageSpec sizes the managed etcd's volumes.



_Appears in:_
- [ManagedEtcdSpec](#managedetcdspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#quantity-resource-api)_ | size is the capacity requested for each member's PVC. |  | Optional: \{\} <br /> |
| `storageClass` _string_ | storageClass selects the StorageClass; empty uses the cluster default. |  | Optional: \{\} <br /> |
| `reclaimPolicy` _[ReclaimPolicy](#reclaimpolicy)_ | reclaimPolicy decides what happens to the members' volumes when the<br />cluster is deleted. Delete by default, unlike the data disks: this etcd<br />is a development convenience, and leaving its volumes behind means a<br />re-created cluster adopts a key store whose sealed credentials it can no<br />longer open (SPEC §8.6). | Delete | Enum: [Retain Delete] <br />Optional: \{\} <br /> |


#### OTLPSpec



OTLPSpec is the OTLP exporter destination.



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | endpoint is the OTLP endpoint URL; empty disables the OTLP exporters. |  | Optional: \{\} <br /> |
| `protocol` _string_ | protocol is the OTLP transport. | grpc | Enum: [grpc http/protobuf] <br />Optional: \{\} <br /> |


#### ObservabilitySpec



ObservabilitySpec configures telemetry of the fs pods.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `otlp` _[OTLPSpec](#otlpspec)_ | otlp configures the OpenTelemetry exporter env of the fs pods. |  | Optional: \{\} <br /> |
| `logLevel` _string_ | logLevel sets the fs log level. | info | Enum: [debug info warn error] <br />Optional: \{\} <br /> |
| `podMonitor` _boolean_ | podMonitor creates a PodMonitor for the fs pods' Prometheus metrics<br />(requires the monitoring.coreos.com API group). |  | Optional: \{\} <br /> |


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
| `zone` _string_ | zone pins the rack's nodes to a topology.kubernetes.io/zone value<br />(sugar for nodeSelector). |  | Optional: \{\} <br /> |
| `nodeSelector` _object (keys:string, values:string)_ | nodeSelector pins the rack's nodes to matching Kubernetes nodes.<br />Merged over zone. |  | Optional: \{\} <br /> |


#### RebalanceSpec



RebalanceSpec tunes the automatic rebalancer; zero values defer to fs
defaults.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `autoDisabled` _boolean_ | autoDisabled turns automatic rebalancing off; relocation then happens<br />only via periodic scrubs and manual runs. |  | Optional: \{\} <br /> |
| `settle` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | settle is how long the membership must be stable before data moves<br />(fs default 1m). |  | Optional: \{\} <br /> |
| `cooldown` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | cooldown is the minimum gap between a node's automatic trigger<br />attempts (fs default 15m). |  | Optional: \{\} <br /> |
| `fullWatermark` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#quantity-resource-api)_ | fullWatermark is the disk-fullness fraction (0,1] beyond which a node<br />warns that the disk should be drained (fs default 0.9). |  | Optional: \{\} <br /> |


#### RebalanceStatus



RebalanceStatus summarizes rebalance/repair across nodes.



_Appears in:_
- [FSClusterStatus](#fsclusterstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `state` _string_ | state is the worst rebalance state across nodes. |  | Optional: \{\} <br /> |
| `repairQueueDepth` _integer_ | repairQueueDepth is the summed pending repair tasks across nodes. |  | Optional: \{\} <br /> |


#### ReclaimPolicy

_Underlying type:_ _string_

ReclaimPolicy controls the fate of data-bearing resources on removal.



_Appears in:_
- [FSBucketSpec](#fsbucketspec)
- [ManagedEtcdStorageSpec](#managedetcdstoragespec)
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


#### SchemaMigrationPolicy

_Underlying type:_ _string_

SchemaMigrationPolicy selects who triggers fs schema migrations.



_Appears in:_
- [UpdatePolicySpec](#updatepolicyspec)

| Field | Description |
| --- | --- |
| `Auto` |  |
| `Manual` |  |


#### SchemaVersionStatus



SchemaVersionStatus reports the fs schema versions in play.



_Appears in:_
- [FSClusterStatus](#fsclusterstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cluster` _integer_ | cluster is the schema version recorded in etcd. |  | Optional: \{\} <br /> |
| `binary` _integer_ | binary is the schema version the deployed image implements. |  | Optional: \{\} <br /> |


#### StorageSpec



StorageSpec declares the per-node disks and how their claims are reclaimed.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `disks` _[DiskSpec](#diskspec) array_ | disks are this cluster's per-node storage devices: every entry becomes<br />one PersistentVolumeClaim on every node, mounted at<br />/var/lib/fs/disks/<name>. A single-node cluster declares exactly one:<br />fs's filesystem backend stores everything under a single root.<br />Entries may be added, and removed. Removing one is a decommission, not<br />a delete: the disk is drained out of placement on every node, and its<br />volumes go only once fs reports it holds nothing (SPEC §8.5). Sizes may<br />only grow. A disk is identified by its name, so renaming one reads as<br />removing a disk and adding an empty one — which is a slow, safe, and<br />almost certainly unintended way to spend a rebalance. |  | MaxItems: 32 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `reclaimPolicy` _[ReclaimPolicy](#reclaimpolicy)_ | reclaimPolicy controls what happens to a node's PVCs when the node is<br />removed or the cluster is deleted. | Retain | Enum: [Retain Delete] <br />Optional: \{\} <br /> |


#### TopologySpec



TopologySpec declares the cluster's nodes and failure domains. Exactly one
of nodes or racks must be set.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodes` _integer_ | nodes is the flat topology: N nodes, each its own failure domain.<br />One node is a development install: that node runs fs's single-node<br />filesystem backend — one disk, no etcd, no replication, no failure<br />tolerance — instead of joining a cluster. |  | Maximum: 16 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `racks` _[RackSpec](#rackspec) array_ | racks are explicit failure domains; placement spreads object copies<br />across racks first. Rack membership is declared here and pinned with<br />node affinity — it is never derived from where a pod happens to be<br />scheduled. |  | MaxItems: 16 <br />MinItems: 1 <br />Optional: \{\} <br /> |
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
| `Migrating` |  |
| `Draining` | UpdatePhaseDraining is a node being decommissioned: taken out of<br />placement and waiting for the cluster to move its data off, before it is<br />removed (SPEC §8.4).<br /> |


#### UpdatePolicySpec



UpdatePolicySpec tunes rolling changes.



_Appears in:_
- [FSClusterSpec](#fsclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `convergenceTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | convergenceTimeout bounds how long the operator waits, between node<br />restarts, for the cluster to reconverge (pod ready, node registered,<br />repair queue drained). On timeout the rollout halts — it never<br />proceeds to another node while the cluster is unconverged — and<br />resumes automatically when the gate passes. | 30m | Optional: \{\} <br /> |
| `schemaMigration` _[SchemaMigrationPolicy](#schemamigrationpolicy)_ | schemaMigration selects whether the operator runs `fs cluster migrate`<br />automatically after a successful full rollout (Auto) or only surfaces<br />the pending migration via the SchemaCurrent condition (Manual). | Auto | Enum: [Auto Manual] <br />Optional: \{\} <br /> |


#### UpdateStatus



UpdateStatus describes the rolling change in flight.



_Appears in:_
- [FSClusterStatus](#fsclusterstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[UpdatePhase](#updatephase)_ | phase is the state-machine phase. |  | Enum: [Preflight RollingNodes Migrating Draining] <br />Optional: \{\} <br /> |
| `node` _string_ | node is the node currently being replaced or decommissioned. |  | Optional: \{\} <br /> |
| `disk` _string_ | disk is the disk being drained out of the cluster, when the change in<br />flight is a disk removal. A disk is removed from every node at once, so<br />it is not attributable to one node the way a rolling change is. |  | Optional: \{\} <br /> |
| `startedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | startedAt is when the rolling change started. |  | Optional: \{\} <br /> |


