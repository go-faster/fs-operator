# fs-operator — Kubernetes operator for go-faster/fs clusters

Status: **P1 shipped** as `v0.1.0` (§16) — provisioning, per-node configs and
StatefulSets, conditions/status and basic rolling updates. P2 (day-2) is next,
now unblocked by fs v0.6.0 (admin reload + config revision, cluster status,
importable admin client). [PLAN.md](PLAN.md) tracks what is done and what is
next; this document stays the design of record for all of it.

This document is the specification for `fs-operator`: a Kubernetes operator
(kubebuilder v4) that provisions and operates **multi-node, clustered**
[go-faster/fs](https://github.com/go-faster/fs) deployments — an S3-compatible
object store with quorum replication (`rf2.5`, `rf3`, `ec:k,m`), an etcd
control plane, failure-domain-aware placement, automatic rebalancing and
scrub/repair.

The operator exists because a clustered fs deployment is not "a StatefulSet
with N replicas": it has per-node identity, disks and weights, failure-domain
(rack) assignment, a strict *one-node-at-a-time with reconvergence* upgrade
contract, explicit schema migrations, and drain-before-remove
decommissioning. The existing Helm chart in `go-faster/fs`
(`helm/go-faster-fs`) can only template the static shape; the operator owns
the day-2 choreography.

---

## 1. Goals

- **Provision** a complete fs cluster from one custom resource: per-node
  StatefulSets, PVC-backed disks, headless + client Services, rendered
  per-node config, generated secrets (cluster secret, admin token, root S3
  credentials), PDB.
- **Topology-aware**: map fs *racks* (failure domains) onto Kubernetes zones
  or arbitrary node sets; keep placement guarantees honest with required pod
  anti-affinity.
- **Safe day-2 operations**, encoding `docs/UPGRADE.md` and
  `docs/FAILURE-MODEL.md` of go-faster/fs as controller logic:
  - rolling image/config updates one node at a time, gated on cluster
    reconvergence between nodes;
  - explicit schema migration (`fs cluster migrate`) after a full rollout;
  - scale-up (join + auto-rebalance) and drain-based decommission on
    scale-down;
  - hot reload (credentials, TLS certs) without restarts where fs supports
    it, with per-node verification that the reload actually applied.
- **Declarative tenancy primitives**: buckets (`FSBucket`) and S3 credentials
  (`FSAccessKey`) as CRs, reconciled against the cluster's admin/S3 APIs.
- **Own the Helm chart from day one**: the operator's chart lives in
  `dist/chart`, is committed and hand-maintained (scaffolded once by the
  kubebuilder `helm/v2-alpha` plugin, then owned), with CRDs synced from
  `config/crd` by a hack script so the chart can never drift.
- **Documentation as a deliverable** (§13): task-oriented guides, a numbered
  example gallery, and an API reference generated from the Go types.

## 2. Non-goals (v1alpha1)

- **No autoscaling.** Cluster membership changes are deliberate,
  capacity-planned operations (fs docs are explicit about this). No HPA,
  ever, for the data plane.
- **No Ingress/HTTPRoute/Gateway management** for S3 traffic. The operator
  exposes a Service; routing is composed by the user.
- **No backup/restore or cross-cluster replication.** Durability is the
  cluster's replication scheme; disaster recovery is out of scope for now.
- **No certificate issuance.** TLS material comes from Secrets (cert-manager
  or hand-made); the operator mounts and hot-reloads it.
- **No production-grade managed etcd — by design, permanently.** External
  etcd is the only supported control plane for production. A minimal managed
  mode (`etcd.managed: {}`) exists purely for dev/demo clusters: no backups,
  no defrag automation, no member replacement, and the operator says so
  loudly (warning event on every reconcile + admission warning + docs). It
  will not be hardened into a production offering; etcd lifecycle management
  is its own discipline. **Shipped in P4**: a StatefulSet with a static
  bootstrap list, one PVC per member, `replicas` 1 or 3 and immutable
  (growing it needs a join the operator does not implement), volumes
  reclaimed on delete by default — a kept dev volume only buys the
  adopted-prefix failure of §8.6. `etcd.cleanupOnDelete` is a no-op in
  managed mode and the finalizer is not added: the etcd is owned by the
  cluster, so GC takes it, and there is nothing left to purge.
- **No cluster-secret rotation.** Peer HMAC auth uses a single shared secret;
  mixed secrets partition the cluster. Rotation is a documented manual
  procedure until fs grows dual-secret support.

---

## 3. Background: what fs cluster mode requires from an orchestrator

Facts about go-faster/fs that drive the design (source: `cmd/fs/config.go`,
`clusterstore/`, `docs/{UPGRADE,FAILURE-MODEL,SIZING,DEPLOYMENT}.md`):

- Every node runs the same `fs s3` binary with a YAML config. Cluster mode is
  `storage.type: cluster` plus a `cluster:` section: `node_id`, `rack`,
  peer listener `addr`/`advertise_addr` (default `:7080`), shared `secret`
  (HMAC peer auth, min 16 chars), `scheme`, per-node `disks`
  (id/path/weight), `etcd` (endpoints/prefix/ttl) and `rebalance` tuning.
- Per-instance identity is injectable via env — `FS_CLUSTER_NODE_ID`,
  `FS_CLUSTER_ADVERTISE_ADDR`, `FS_CLUSTER_SECRET`.
- Racks are failure domains: placement spreads copies across racks first.
  Empty rack = the node is its own domain.
- Disk **weights** drive placement; weight 0 drains a disk (no new data, and
  the auto-rebalancer moves existing data off it). Weights live in the
  node's config and are registered in etcd at startup.
- Supported envelope: 3–16 nodes. `rf2.5`/`rf3` need ≥3 distinct failure
  domains; `ec:k,m` needs ≥ k+m.
- Health endpoints: `/health` (liveness) and `/ready` (readiness, probes
  storage → 200/503) on the S3 listener.
- Admin API (separate listener, bearer token, default `localhost:8090`):
  `/api/v1/info`, `/api/v1/access-keys` CRUD, `/api/v1/cluster/rebalance`
  (status incl. `repair_queue_depth`, pause/resume control).
- `SIGHUP` hot-reloads credentials/grants and the TLS certificate — nothing
  else. All other config changes need a process restart.
- **Upgrade contract**: one node at a time; wait for the cluster to
  reconverge before touching the next node (a second missing domain can make
  EC objects unrecoverable). A node never joins a cluster whose schema is
  newer than itself; schema migrations are explicit (`fs cluster migrate`,
  etcd-elected, resumable) and run only after *all* nodes run the new
  binary.
- **Decommission contract**: drain first (weight 0 → data moves off), then
  remove. Killing a node without draining leaves the cluster to repair from
  surviving copies — allowed but degraded.
- Observability: OTEL SDK via standard env vars (traces/metrics/logs),
  Prometheus metrics exporter, pprof via `PPROF_ADDR`. Rich cluster metrics
  (disk fullness, placement skew, repair queue, rebalance/scrub counters).

Some orchestration needs are **not yet satisfiable** with today's fs; §11
lists the required upstream changes and which operator feature depends on
each.

---

## 4. Architecture

One controller-manager (Deployment, leader-elected) with three controllers:

```
                      ┌────────────────────────────┐
                      │  fs-operator (Deployment)  │
                      │  FSCluster  controller     │
                      │  FSBucket   controller     │
                      │  FSAccessKey controller    │
                      └──────────┬─────────────────┘
                                 │ owns / reconciles
     ┌───────────────┬───────────┼──────────────┬───────────────┐
     ▼               ▼           ▼              ▼               ▼
 Secrets        per-node      per-node      Services         PDB
 (cluster secret, config       StatefulSet   peers (headless,  maxUnavailable=1
  admin token,    Secrets      (1 pod each)  per-pod DNS)
  root S3 creds)                             client (S3)
```

### 4.1 One StatefulSet per node

The operator manages **one single-pod StatefulSet per fs node**, not one
StatefulSet with N replicas. This is the load-bearing decision; it buys:

- **Per-node configuration.** Each node gets its own rendered `config.yaml`
  (Secret): its rack, its disks *with per-node weights*. Draining a node for
  decommission (all weights → 0) is a config change to one node — impossible
  with a shared pod template.
- **Exact rollout control.** A rolling change is "update node i's
  StatefulSet, let the StatefulSet controller replace the pod, gate on
  reconvergence, proceed to node i+1" — the native workload machinery does
  pod replacement; the operator only sequences. No `OnDelete` + manual pod
  deletion choreography.
- **Per-node storage surgery.** PVC expansion and disk-set changes are
  per-node orphan-recreate operations touching exactly one node at a time
  (§8.5).
- **Independent scheduling.** Rack→zone pinning is per-node nodeAffinity;
  scale-down removes a specific chosen node, not "the highest ordinal".

Cost: more API objects (≤16 nodes ⇒ ≤16 StatefulSets — trivial) and the
operator must aggregate readiness itself (it does anyway for convergence
gating).

All pods share one headless Service (`serviceName`) so every pod has stable
DNS for the peer advertise address.

### 4.2 Identity scheme

| Thing | Value |
|---|---|
| API group | `fs.go-faster.org/v1alpha1` |
| Kinds | `FSCluster`, `FSBucket`, `FSAccessKey` |
| Go module | `github.com/go-faster/fs-operator` |
| Node name / fs `node_id` | `<cluster>-<rack>-<n>` (flat: `<cluster>-<n>`) |
| StatefulSet (per node) | `<node>` → pod `<node>-0` |
| Advertise address | `<node>-0.<cluster>-peers.<ns>.svc:7080` |
| Headless service | `<cluster>-peers` (publishNotReadyAddresses) |
| Client service | `<cluster>` (S3 port) |
| Per-node config Secret | `<node>-config` |
| Cluster secret / admin token / root creds | `<cluster>-{cluster-secret,admin-token,root-credentials}` |

- The operator talks to fs over two client channels: the **admin API**
  (bearer token, per-pod DNS via the headless service) for health,
  convergence gating and access-key verification; and the **S3 API** (root
  credentials, client Service) for bucket CRUD. Connections are cached per
  cluster and invalidated on secret rotation or endpoint change.
- `FSBucket` and `FSAccessKey` reference an `FSCluster` in the same
  namespace (namespace = tenancy boundary; no cross-namespace refs).
- All managed resources carry `app.kubernetes.io/managed-by: fs-operator`
  and `fs.go-faster.org/cluster: <name>` labels plus an ownerReference; the
  node StatefulSets also carry `fs.go-faster.org/node: <node>` and
  `fs.go-faster.org/rack: <rack>`.

---

## 5. `FSCluster` API

Annotated example (defaults spelled out where interesting):

```yaml
apiVersion: fs.go-faster.org/v1alpha1
kind: FSCluster
metadata:
  name: prod
spec:
  image:
    repository: ghcr.io/go-faster/fs
    # Defaults to the pinned fs release this operator version is validated
    # against (currently v0.12.0). Always a pinned version, never a floating
    # tag — cluster upgrades are deliberate, one-node-at-a-time operations.
    tag: v0.12.0
    pullPolicy: IfNotPresent
    pullSecrets: []

  # Default replication scheme for all buckets: rf2.5 | rf3 | ec:k,m.
  # Changeable at runtime (affects new writes; existing objects converge via
  # repair/rebalance) — but never below what the topology can host.
  scheme: rf2.5

  topology:
    # Exactly one of `nodes` (flat) or `racks` (failure domains).
    #
    # Flat: N nodes, each its own failure domain (fs rack = "").
    # nodes: 3
    #
    # Racks: placement spreads copies across racks first.
    racks:
      - name: a
        nodes: 2
        # Sugar for nodeAffinity on topology.kubernetes.io/zone.
        zone: eu-central-1a
        # Or full scheduling control per rack:
        # nodeSelector: {...}
      - name: b
        nodes: 2
        zone: eu-central-1b
      - name: c
        nodes: 2
        zone: eu-central-1c
    # One fs node per k8s node. Required (default) keeps the failure model
    # honest; Preferred/None for dev clusters.
    podAntiAffinity: Required     # Required | Preferred | None

  storage:
    # Each disk is one PVC on every node, mounted at
    # /var/lib/fs/disks/<name> and listed in cluster.disks with its weight.
    disks:
      - name: d0
        size: 200Gi
        storageClass: fast-nvme   # optional; cluster default otherwise
        weight: 1                 # optional; relative capacity
    # PVC handling when nodes are removed / the cluster is deleted.
    reclaimPolicy: Retain         # Retain | Delete

  etcd:
    # Exactly one of `external` / `managed`. Production clusters use
    # `external` (see go-faster/fs docs/SIZING.md for etcd sizing).
    # `managed: {}` provisions a minimal dev-grade etcd next to the cluster
    # — permanently non-production (§2); arrives in a later phase (§16).
    external:
      endpoints:
        - http://etcd-0.etcd.fs-system:2379
        - http://etcd-1.etcd.fs-system:2379
        - http://etcd-2.etcd.fs-system:2379
      # TLS/auth — requires upstream fs support (§11.4); until then http only.
      # tlsSecretName: etcd-client-tls
    prefix: /fs/prod              # defaulted to /fs/<namespace>/<name>; immutable
    ttl: 10s
    # Delete this cluster's keys under `prefix` when the FSCluster is deleted.
    cleanupOnDelete: false

  # Secret with key `secret` (min 16 chars). Generated if omitted. Immutable
  # (no rotation in v1alpha1 — see non-goals).
  clusterSecretRef: null

  auth:
    # Secret with keys `access-key` / `secret-key`, granted admin on all
    # buckets (FS_ROOT_ACCESS_KEY / FS_ROOT_SECRET_KEY). Generated if
    # omitted.
    rootCredentialsSecretRef: null
    # Buckets readable anonymously.
    publicReadBuckets: []

  s3:
    service:
      type: ClusterIP             # ClusterIP | NodePort | LoadBalancer
      port: 8080
      annotations: {}
    # TLS termination in fs itself; Secret of type kubernetes.io/tls.
    # Certificate renewals hot-reload without restarts.
    tls:
      secretName: ""              # empty = plaintext

  # Passthrough tuning; defaults mirror fs defaults.
  rebalance:
    autoDisabled: false
    settle: 1m
    cooldown: 15m
    fullWatermark: 0.9
  integrity:
    verifyOnRead: false
    scrubInterval: 24h
    scrubQuarantine: false

  updatePolicy:
    # Gate between node restarts during rolling changes: wait for /ready +
    # node registered + repair queue drained + placement converged, up to
    # convergenceTimeout, before touching the next node.
    convergenceTimeout: 30m
    # Auto: run `fs cluster migrate` (Job) after a successful full rollout.
    # Manual: only surface the SchemaCurrent=False condition.
    schemaMigration: Auto         # Auto | Manual

  observability:
    # Standard OTEL env passthrough.
    otlp:
      endpoint: ""
      protocol: grpc
    logLevel: info
    # Create a PodMonitor for the fs pods' Prometheus metrics.
    podMonitor: false

  # Opt-in NetworkPolicy: peer (7080) and admin (8090) ports only from
  # cluster pods + the operator; S3 unrestricted by default.
  networkPolicy: false

  # Pod-level knobs applied to every node's StatefulSet.
  podTemplate:
    resources:
      requests: {cpu: "1", memory: 2Gi}
    nodeSelector: {}
    tolerations: []
    priorityClassName: ""
    annotations: {}
    labels: {}
    extraEnv: []
```

### 5.1 Field semantics and validation

- `topology`: exactly one of `nodes`/`racks` (CEL). Total nodes must be
  within the supported envelope (3–16); 1–2 nodes are admitted only for dev
  (with a warning event) and only when the scheme's domain requirement
  allows. Rack names are DNS-label and immutable per entry; removing a rack
  or lowering a node count is a decommission (§8.4).
- `scheme`: pattern-validated by CEL (`rf2.5|rf3|ec:<k>,<m>`); the
  controller cross-checks it against the topology (distinct failure domains
  ≥ scheme requirement: 3 for rf2.5/rf3, k+m for EC) and refuses to apply a
  violating change: `SpecValid=False`, reason `SchemeTopologyMismatch`, no
  resource mutation.

  **Where the cross-field checks run.** They live in `internal/validation`
  and are called from two places. The **validating webhook** runs them at
  admission, so `kubectl apply` reports the problem and the object is never
  stored; it is opt-in (`webhook.enabled`) because it needs a serving
  certificate the chart cannot conjure, and its `failurePolicy` is `Fail`.
  The **controller** runs them again before it touches anything — not as a
  leftover, but because a webhook can be disabled, unreachable behind a
  policy an admin relaxed, or simply not have existed when the object was
  stored. One implementation, two callers: two would eventually disagree,
  and the disagreement would surface as a spec the API accepted and the
  operator refuses to build.

  Checks that need to read cluster state (a referenced Secret existing, a
  live StatefulSet's disks) stay in the controller only: the admission path
  must not call the API server, and a Secret created after the cluster is a
  legitimate order of operations.
- Immutable (CEL `self == oldSelf`): `etcd.prefix`, `storage.disks[].name`,
  `clusterSecretRef`, rack `name`s.
- `storage.disks`: entries may be **added** (§8.5); entries may not be
  removed in v1alpha1 (needs per-disk drain observability, §11.6). `size`
  may only grow (PVC expansion, §8.5). `weight` is mutable (rolls the
  cluster, §8.2).
- Defaults are applied by the controller through a single `WithDefaults()`
  method on the spec type (unit-testable and fuzzable); static defaults also
  carry `+kubebuilder:default` markers so `kubectl explain` and the CRD
  schema tell the truth.

### 5.2 Status

```yaml
status:
  observedGeneration: 7
  nodes: 6                 # desired
  readyNodes: 6
  registeredNodes: 6       # nodes present in the etcd topology
  configurationRevision: cfg-6b9f7c   # hash of desired rendered configs
  statefulSetRevision: sts-4c11ab     # hash of desired pod templates
  currentRevision: 6b9f7c  # revision all nodes have converged to
  updateRevision: 6b9f7c   # revision being rolled out
  schemaVersion:
    cluster: 4             # etcd-recorded schema version
    binary: 4              # version the deployed image implements
  rebalance:
    state: idle            # worst state across nodes
    repairQueueDepth: 0    # summed
  update:                  # present while a rolling change is in flight
    phase: RollingNodes    # Preflight | RollingNodes | Migrating
    node: prod-b-1         # node currently being replaced
    startedAt: "..."
  endpoints:
    s3: http://prod.tenant-a.svc:8080
  conditions: [...]
```

Conditions (all standard `metav1.Condition`, with documented reasons — the
condition and event vocabulary is API surface, §13):

| Type | Meaning |
|---|---|
| `SpecValid` | Spec passes cross-field validation (§5.1; the webhook rejects most of it at apply time, this covers specs stored without it). |
| `ReconcileSucceeded` | The last reconcile pass completed without error. |
| `Ready` | The cluster serves S3 at write quorum. |
| `NodesHealthy` | Every node pod is Ready and registered in etcd. |
| `ClusterSizeAligned` | Actual node set matches the topology (False while scaling, reason `ScalingUp`/`Draining`/…). |
| `ConfigurationInSync` | Every node runs the desired configuration revision (hot reload verified per node). |
| `Converged` | Repair queue empty and placement converged (gates rollouts). |
| `SchemaCurrent` | Cluster schema version matches the binary's (False = migration pending). |

Per-node detail lives in events and metrics, not status, to keep the object
bounded.

---

## 6. `FSBucket` API

```yaml
apiVersion: fs.go-faster.org/v1alpha1
kind: FSBucket
metadata:
  name: media
spec:
  clusterRef:
    name: prod
  bucketName: media           # defaults to metadata.name; immutable
  # Per-bucket scheme override; empty = cluster default. Requires the
  # upstream admin endpoint (§11.3); ships in the phase that lands it.
  scheme: ""
  reclaimPolicy: Retain       # Retain | Delete
status:
  conditions: [ ... Ready ... ]
  scheme: rf2.5               # effective scheme
```

Reconcile: ensure the bucket exists (S3 `CreateBucket` with root credentials
via the client Service); apply the scheme override; add a finalizer. On
delete with `reclaimPolicy: Delete`, issue S3 `DeleteBucket` — which fails
while the bucket is non-empty; the controller retries with backoff and
surfaces `Ready=False`, reason `BucketNotEmpty` (no force-wipe in v1alpha1).
`Retain` drops the finalizer without touching data.

## 7. `FSAccessKey` API

```yaml
apiVersion: fs.go-faster.org/v1alpha1
kind: FSAccessKey
metadata:
  name: app-writer
spec:
  clusterRef:
    name: prod
  # Credential source — exactly one of the two modes:
  #
  # 1. Generated (default): the operator mints the credential once
  #    (crypto/rand) and writes it to this Secret (keys: access-key,
  #    secret-key, endpoint). Defaults to <metadata.name>-credentials;
  #    owned by the FSAccessKey.
  secretName: app-writer-credentials
  # 2. Imported: a user-managed Secret (keys: access-key, secret-key) —
  #    e.g. minted by Vault / ExternalSecrets. The operator watches it,
  #    renders it into the cluster config, and hot-reloads on change, so
  #    external rotation propagates. secret-key must be ≥16 chars
  #    (refused otherwise: Ready=False/WeakSecretKey). No operator-owned
  #    Secret is created in this mode.
  # existingSecretRef:
  #   name: vault-minted-s3-creds
  grants:
    - bucket: "media-*"     # glob, matches fs GrantConfig
      permission: write     # read | write | admin
status:
  conditions: [ ... Ready ... ]
  accessKey: AKprod4f2…     # non-secret half, for reference
```

Design decision: credentials live in the cluster's **etcd control plane**
(`auth.source: etcd`, fs §6.8), managed through the admin API — not rendered
into config files. Since fs v0.8.0 the runtime key store is cluster-wide:
credentials are sealed with a key derived from the cluster secret, persisted in
etcd and hot-reloaded on every node, so they are cluster-wide, survive restarts
and are encrypted at rest. (Earlier the runtime store was node-local, which is
why v0.1–0.3 rendered keys into config; the etcd store removed that reason.) The
config carries no keys — the root credential seeds etcd via the `FS_ROOT_*` env
on first boot, and etcd is authoritative thereafter.

Reconcile: resolve the credential (generate once into an owned Secret, or read
`existingSecretRef` — the controller watches referenced Secrets and maps them
back to their FSAccessKeys), then reconcile it into the cluster's key store via
the admin API: create it, re-create it on a grant change or an imported-Secret
rotation (detected by a material fingerprint), and set `Ready=True` once the
cluster accepts it. Deletion revokes the credential (finalizer →
`deleteAccessKey`). Public-read buckets are reconciled the same way, through
`GET`/`PUT /api/v1/public-read-buckets`.

---

## 8. FSCluster reconciliation

The reconciler is a sequential **step pipeline**; each step returns
continue / requeue-after / blocked (blocked skips the remaining mutating
steps, while status-refreshing steps marked *always-run* still execute).
Steps: Secrets → Services → NodeConfigs → NodeSets (the rolling state
machine, §8.2) → Migration → PDB → Status. Every pass is idempotent and each
step is unit-testable in isolation.

### 8.1 Resource graph

Rendered per reconcile, compared semantically, applied with server-side
apply (field manager `fs-operator`):

1. **Secrets** — cluster secret / admin token / root credentials: generated
   once if no `*Ref` is given (crypto/rand, 32 bytes), never regenerated.
2. **Per-node config Secret** — full `config.yaml` (it embeds credential
   material, so a Secret, not a ConfigMap):
   - `server`: addr `:8080`, health `/health`, timeouts; `tls` pointing at
     the mounted certificate when `s3.tls.secretName` is set;
   - `storage`: `type: cluster`, root `/var/lib/fs`;
   - `cluster`: `node_id: <node>`, `rack: <rack>`, `addr: :7080`,
     `advertise_addr: <node>-0.<cluster>-peers…:7080`, `scheme`, `disks`
     (one per `storage.disks` entry at `/var/lib/fs/disks/<name>`, with
     *this node's* weights — 0 while draining), `etcd`, `rebalance`;
   - `auth`: keys merged from all FSAccessKeys + `publicReadBuckets`;
   - `admin`: enabled, `addr: :8090` (pod network; bearer token via env);
   - `integrity`, `observability` passthrough.
   The cluster secret is env-injected (`FS_CLUSTER_SECRET`), never written
   into the file.
3. **Per-node StatefulSet** — one replica, `serviceName: <cluster>-peers`,
   `persistentVolumeClaimRetentionPolicy` from `storage.reclaimPolicy`,
   one volumeClaimTemplate per disk. Pod template:
   - env: `FS_CLUSTER_SECRET` / `FS_ADMIN_TOKEN` / root creds via
     secretKeyRef, OTEL env, `PPROF_ADDR`;
   - ports: http 8080, peer 7080, admin 8090, metrics 9464, pprof 9010;
   - probes: liveness `/health`, readiness `/ready`, generous startup probe;
   - volumes: config Secret at `/etc/fs`, TLS Secret when set, disk PVCs;
   - securityContext: runAsNonRoot 1000, readOnlyRootFilesystem, seccomp
     RuntimeDefault, drop ALL;
   - `fs.go-faster.org/restart-revision` pod annotation: the fingerprint of
     the *restart-requiring* part of the config (everything fs does not
     hot-reload). Changing it is what replaces the pod, so credential
     changes never roll the cluster (§8.2/§8.3). The full config revision
     rides on the config Secret as `fs.go-faster.org/config-revision`;
   - per-rack nodeAffinity (zone/nodeSelector) + anti-affinity across the
     cluster's pods per `topology.podAntiAffinity`.
4. **Services** — `<cluster>-peers` headless (`publishNotReadyAddresses:
   true`; 7080/8090/9464) and `<cluster>` client (S3 port).
5. **PodDisruptionBudget** — `maxUnavailable: 1` over all cluster pods.
   Voluntary evictions can never take two failure domains down;
   non-negotiable, always created.
6. **PodMonitor** — optional, created only if the `monitoring.coreos.com`
   API group is discoverable.

### 8.2 Rolling changes (image, restart-required config)

Two desired-state revisions are computed each pass: the **configuration
revision** (hash of rendered configs) and the **pod-template revision**.
A node needs a *restart* when its StatefulSet template is stale or its
config diff touches non-hot-reloadable fields; it needs a *reload* (§8.3)
otherwise.

State machine, persisted in `status.update`:

```
Idle
 └─ restart-requiring diff detected
Preflight        all pods Ready ∧ all nodes registered ∧ Converged
                 — else hold (Ready stays, conditions report why)
RollingNodes     for one node at a time (racks round-robin, so two nodes of
                 one rack are never adjacent in the order):
                   apply node's config Secret + StatefulSet template →
                   StatefulSet controller replaces the pod →
                   wait pod Ready → wait node registered in topology →
                   wait Converged (repair queue empty on all nodes,
                   placement convergence — §11.2) → next node
                 gate timeout (updatePolicy.convergenceTimeout) ⇒
                 Converged=False + event; HALT — never touch a second node
                 while the cluster is unconverged; resumes automatically
                 when the gate passes
Migrating        schemaVersion.binary > schemaVersion.cluster ∧ Auto ⇒
                 Job `<cluster>-migrate-<rev>` runs `fs cluster migrate`
                 (etcd-elected, resumable; safe to re-run)
Idle             currentRevision = updateRevision
```

Rollback = the user reverting `spec`; the same machinery rolls back
node-by-node. fs's schema rules protect the edges: an old binary refuses to
join a schema-migrated cluster — the operator surfaces the CrashLoop with
the upstream explanation (post-migration binary rollback is unsupported, per
fs UPGRADE.md).

### 8.3 Hot config changes

If a config diff touches only hot-reloadable material (auth keys/grants,
public-read buckets, TLS certificate), the operator updates the config
Secrets and triggers a reload on every pod instead of restarting: preferred
via the admin reload endpoint (§11.1), fallback `pods/exec` → `kill -HUP 1`.

Verification is per node and revision-based: the rendered config embeds its
own revision, and the node reports the revision it has applied (§11.2;
until then, fallback to probing observable effects — `listAccessKeys` for
credentials, the served certificate's serial for TLS). Kubelet Secret
propagation can lag (~1m), so reload is retried until the node reports the
target revision; `ConfigurationInSync` flips True when every node does.

### 8.4 Scale-up and decommission

- **Scale-up** (`nodes` increased or a rack added): create the new nodes'
  Secrets + StatefulSets; they register and the auto-rebalancer converges.
  `ClusterSizeAligned=False/ScalingUp` until registered + converged.
  Multiple new nodes may join simultaneously (join is additive; only
  removals are serialized).
- **Scale-down** (`nodes` decreased or a rack removed): decommission
  strictly one node at a time, highest node index first within the affected
  rack:
  1. **Drain**: re-render the node's config with all disk weights drained
     and roll that node (§8.2 machinery); on restart it re-registers drained
     and the auto-rebalancer moves its data off. A drained disk is rendered
     with a *negative* weight, not 0: fs's config layer reads 0 as "unset"
     and substitutes 1, while placement skips any disk whose weight is not
     positive. §11.6 replaces this with an API call.
  2. **Wait drained**: every one of the node's disks reports `has_data:
     false` in the cluster status (fs ≥ v0.10.0, §11.2). Occupancy is *not*
     inferred from capacity: `total_bytes`/`free_bytes` come from statfs, so
     they describe the filesystem and a disk holding no fragments still
     reports bytes in use.
  3. **Remove**: delete the node's StatefulSet (graceful stop deregisters
     it from etcd) and config Secret; apply `storage.reclaimPolicy` to its
     PVCs — carried by the StatefulSet's claim retention policy, so the
     volumes follow the same rule as any other deletion.
  4. Wait Converged, repeat for the next node.

  Every gate resolves unknown to *wait*, never to *proceed*: the node must be
  running the drained config (until it restarts its disks still take writes),
  the cluster must be converged and **fully reporting** (a silent node makes
  the view partial, and a partial view is not evidence a disk is empty), and
  every disk must answer. fs omits `has_data` for a node that did not report
  or a disk it could not read, so a cluster on a pre-v0.10.0 binary drains
  and then waits indefinitely rather than deleting on a signal that is not
  there. A stalled decommission is recoverable; a node deleted while it still
  held the only copy of something is not.

  The decommissioning node stays in the pass — counted by the health, rollout
  and disruption-budget gates — until it is removed, and keeps the
  StatefulSet it is already running, restamped onto the drained config. It is
  never rebuilt from a spec that no longer describes it, which would risk
  moving the pod away from its own data.

  Total nodes may never drop below the scheme's domain requirement; such a
  spec is refused outright, and no node is drained on the way to it.

### 8.5 Storage changes

- **Disk size increase**: per node — patch the PVC (requires
  `allowVolumeExpansion` on the StorageClass), then orphan-recreate that
  node's StatefulSet (delete leaving the pod orphaned, re-apply with the
  new volumeClaimTemplate) so a future pod replacement claims the right
  size. Shrink is refused by the controller, alongside the other
  cross-field checks: comparing every disk's old and new size in CEL costs
  more than the API server's per-schema validation budget allows for a list
  this long.
- **Disk added**: per node, one node at a time — orphan-recreate the
  StatefulSet with the extra volumeClaimTemplate and roll the node; the
  restarted pod mounts and registers the new disk, weights drive data onto
  it.
- **Disk removed**: a decommission, not a delete. The disk is drained out of
  placement on *every* node at once through fs's control plane (§11.6), so
  no restart is needed and the rebalancer starts immediately; the operator
  waits until every node's copy reports `has_data: false`, then
  orphan-recreates each node without it, one at a time, and its PVCs follow
  `storage.reclaimPolicy`. The gates are the node decommission's, and
  resolve unknown to *wait* for the same reasons (§8.4).

  **The disk stays mounted and in the node's config for the whole drain.**
  Dropping it from the config would leave fs unable to move the data — it
  would not know the disk exists — and dropping the volume would leave it
  nothing to read. It is registered at a drained weight and removed only
  once empty.

  Restoring the entry mid-drain clears the override. The operator clears
  only overrides it set (matched by reason), so a disk drained by hand stays
  drained. Removing the last disk is refused outright.
- **Weight change**: config change → §8.2 rolling restart. `fs cluster
  drain` (§11.6) is the hot path, and what the removal flow above uses.

### 8.6 Deletion

Owned resources carry ownerRefs, so GC takes them down, and PVCs follow
`reclaimPolicy` through each StatefulSet's claim retention policy. etcd is
the one thing outside that graph, and the finalizer
`fs.go-faster.org/cluster` is what handles it — carried **only** by
clusters with `etcd.cleanupOnDelete: true`, so an ordinary cluster's
deletion can never be held up by an etcd the operator cannot reach.

On delete, with cleanup opted in: stop the node StatefulSets and wait for
every node pod to be gone (a running node re-registers itself, so purging
first would race the cluster being purged), then delete every key under
`<etcd.prefix>/` — with the trailing separator, or a cluster named `app`
would take `app-staging`'s keys with it — then release the object. A purge
that fails retries rather than releasing: leaving the keys behind is the
failure the cleanup exists to prevent. The default leaves etcd state
untouched (shared-etcd caution).

**Adopting a prefix.** Since fs §6.8 the credential store is cluster-wide
in etcd and seeded from config only while it is empty, so a cluster
starting on a prefix that already holds keys adopts credentials sealed
with a cluster secret it no longer has: fs skips what it cannot unseal,
the pods pass their probes, and nothing can authenticate. Re-creating a
deleted cluster under the same name is the ordinary way to get there. The
operator checks its root credential against the cluster's key store and
reports `Ready=False/RootCredentialUnregistered` naming the prefix and the
way out, rather than leaving the cause a node's log away and the symptom
on every FSBucket. It reports only — registering the credential itself
would be repairing a cluster it cannot prove is its own, and a root key
removed deliberately wants the opposite. A key store that is merely
*empty* is treated as unknown: a starting cluster looks like that.

### 8.7 Failure handling

- A pod failing mid-rollout: the state machine keeps waiting on its gates
  and surfaces `Converged=False` + events after `convergenceTimeout` — no
  automatic destructive remediation (fs repair handles data; the operator
  never touches a second node while the cluster is unconverged).
- Involuntary node loss: Kubernetes reschedules the pod (same PVCs, volume
  topology permitting). The operator does not force-delete pods stuck on
  dead nodes in v1alpha1; auto-remediation needs fencing and is future
  work.
- Requeue is watch-driven plus a slow resync (~5m) refreshing
  health-derived status (registration, repair queue) from the admin API.

---

## 9. Security

- All generated secrets are 32-byte crypto/rand values; nothing secret is
  ever placed in ConfigMaps, annotations, inline env values (secrets come
  via `valueFrom.secretKeyRef`) or logs.
- fs peer traffic (7080) is HMAC-authenticated but **not encrypted**; docs
  say so plainly, and `spec.networkPolicy: true` restricts 7080/8090 to
  cluster pods + the operator namespace.
- The admin listener requires the bearer token; the token Secret never
  leaves the namespace.
- RBAC (operator): apps/StatefulSets, core Secrets/Services/ConfigMaps/Pods
  (+`pods/exec` only for the SIGHUP fallback — dropped once §11.1 lands),
  policy/PDB, batch/Jobs, monitoring PodMonitors (optional), the three CRs
  + status + finalizers.
- fs pods: non-root (uid 1000), read-only rootfs, no capabilities, seccomp
  RuntimeDefault.

## 10. Observability

- Operator metrics (controller-runtime plus), in `internal/metrics`:
  `fsoperator_cluster_ready{namespace,cluster}`,
  `fsoperator_cluster_nodes{namespace,cluster,state}` (`declared`, `ready`,
  `registered`), `fsoperator_update_phase{namespace,cluster,phase}`,
  `fsoperator_update_duration_seconds{namespace,cluster}`,
  `fsoperator_reconcile_errors_total{controller}`.

  Every series carries `namespace` as well as `cluster`: the operator is
  cluster-wide, so two namespaces may hold an FSCluster of the same name and
  a `cluster`-only label would silently merge them.

  `update_phase` publishes 0 for the phases a cluster is *not* in rather than
  omitting them — an absent series and a false one read the same to an alert
  that has never seen the cluster. And every cluster-keyed series is dropped
  when its FSCluster is deleted: a gauge that outlives its object reports
  `ready=0` forever, on a name nothing will reconcile again, which is
  indistinguishable from an outage that never resolves.
- Events on every transition: rollout started/gated/halted/finished,
  migration run, drain progress, refused spec changes, reload verified.
  Event reasons are part of the documented API surface (§13).
- fs pods get OTEL env passthrough and a metrics port;
  `observability.podMonitor: true` creates the PodMonitor. Grafana
  dashboards ship later (§16).

---

## 11. Required changes in go-faster/fs

The operator degrades gracefully where these are missing, but each unlocks a
feature. Each is small and independently useful outside Kubernetes.

Items 1, 2 and 7 are **landed upstream** (unblocking P2's core loops); the
rest remain:

1. **Admin reload endpoint** — `POST /api/v1/reload`, semantically identical
   to SIGHUP (credentials + TLS). Unblocks: hot reload (§8.3) without
   `pods/exec` RBAC. **DONE** (go-faster/fs `feat(admin): reload endpoint
   and config revision`): returns what it reloaded plus the config revision
   now in effect. A top-level `revision` config field is an opaque marker fs
   echoes via `GET /api/v1/info` (`config_revision`) and the reload result —
   the operator stamps each rendered config with its configuration revision
   and reads it back to verify a node applied it.
2. **Cluster status endpoint** — `GET /api/v1/cluster/status`: schema
   version (binary + cluster-recorded), the applied **config revision**
   (echo of a marker from the config file), topology as this node sees it,
   per-node/per-disk occupancy (bytes, object count), convergence indicator
   (misplaced-object estimate, as `fs cluster rebalance --dry-run`
   computes), repair queue depth, last scrub summary. Unblocks: the §8.2
   convergence gate, §8.3 reload verification, §8.4 drain-complete
   detection. **DONE**, in three parts:
   - fs PR #90 (passive state): schema versions, per-node/-disk capacity
     (bytes, fullness), placement skew, rebalance state.
   - fs PR #97, v0.9.0 (live state): per-node `live` — repair queue,
     rebalance runner, scrub totals — plus a cluster-wide
     `repair_queue_depth` and `nodes_reporting`/`nodes_not_reporting`. A node
     that does not answer carries `live_error` rather than zeroed counters,
     so "not reporting" stays distinguishable from "idle". This retired the
     operator's fan-out over each node's rebalance endpoint, kept now only as
     the fallback for pre-v0.9.0 binaries — they serve no live state, and
     their zeroed aggregate would otherwise read as an empty repair queue.
   - fs PR #102, v0.10.0 (occupancy): per-disk `has_data`, the drain signal
     §8.4 needs. A boolean rather than the object count first sketched here:
     fs keeps no index, so counting means walking the tree, while the status
     path is contracted to be cheap — and the boolean is exact *and*
     constant-time on a drained disk, the case a drain polls hardest.
     `data_error` carries a disk the node could not probe; both absences mean
     unknown, never drained.

   The config-revision echo ships on the per-node admin via item 1
   (`GET /api/v1/info`).
3. **Bucket scheme via admin API** — `GET/PUT /api/v1/buckets/{name}/scheme`
   (today CLI-only `fs cluster scheme`). Unblocks: `FSBucket.spec.scheme`.
4. **etcd client TLS + auth** — `cluster.etcd.tls.{ca_file,cert_file,key_file,
   server_name,insecure_skip_verify}` and `cluster.etcd.auth.{username,
   password}` (with `FS_ETCD_USERNAME`/`FS_ETCD_PASSWORD` overrides).
   **DONE** (go-faster/fs PR #106, released as v0.11.0). Both `clientv3.New`
   call sites — the data node and the CLI/headless admin — go through one
   constructor, and an `https://` endpoint enables TLS by itself: the client
   takes the transport from the config and ignores the URL scheme, so an https
   endpoint without a TLS block used to connect in the clear. Unblocked
   `etcd.external.tls.secretName` and `authSecretRef`.
5. **`FS_CLUSTER_RACK` env override** — symmetry with node_id/advertise.
   Nice-to-have (per-node configs carry the rack today).
6. **Hot drain / weight override** — persisted per-disk weight override in
   etcd, settable via CLI + admin API. **DONE** (go-faster/fs PR #110,
   released as v0.12.0): `fs cluster drain <node> <disk>` and
   `GET/PUT/DELETE /api/v1/cluster/disk-weights/{node}/{disk}`. The override
   is its own etcd key rather than part of the node's registration — a node
   republishes that record on every capacity refresh, so a weight written
   there would be undone within the interval and a drained disk would
   silently return to placement. The topology source merges the two, so a
   drain moves the topology signature and the rebalancer acts on it.
   Unblocked disk removal (§8.5). The `weight: 0` trap is fixed too: the
   config field is a pointer, so an explicit zero is a value and drains as
   documented.
7. **Public admin client** — export the ogen-generated admin API client
   as an importable package so the operator and other tooling don't
   re-generate from `_oas/admin.yml`. **DONE** (go-faster/fs
   `refactor(adminapi): export the admin API client`): moved from
   `internal/adminapi` to the importable `github.com/go-faster/fs/adminapi`.

Sequencing: 1, 2, 7 first (they gate the core loops); 3–5 next; 6 with
decommission polish.

**Dedicated admin backend (fs PR #90).** fs now ships a headless
`fs admin --config config.yaml` — a control-plane-only process (no S3 data)
that reads cluster status from etcd and drives rebalancing through the
cluster-wide election. In P2 the operator can run it as its own
Deployment + Service and target that one endpoint for cluster status and
rebalance control, instead of dialing each pod's admin listener. Per-node
operations that are inherently local — the reload endpoint (§8.3) and
runtime credential management — still go to each data node's own admin
listener; the headless admin returns 501 for them.

---

## 12. Repository layout and tooling

Scaffold (already initialized): kubebuilder v4.15, domain `go-faster.org`,
repo `github.com/go-faster/fs-operator`, project `fs-operator`, Go 1.26.

```
api/v1alpha1/            fscluster_types.go, fsbucket_types.go,
                         fsaccesskey_types.go, conditions.go, defaults.go,
                         groupversion, deepcopy
internal/controller/     step.go (pipeline), fscluster/ (controller,
                         rolling state machine, resource builders, config
                         renderer, names), fsbucket/, fsaccesskey/
internal/fsconfig/       mirror of the fs config file schema (upstream
                         cmd/fs/config.go) + the checks fs makes on startup
internal/fsclient/       thin wrappers: admin API client + minio-go S3
                         client, connection cache keyed by cluster
config/                  kustomize (crd, rbac, manager, samples, …) — the
                         authoritative manifest source
dist/chart/              the OWNED Helm chart (§14)
hack/sync-chart.sh       regenerates dist/chart CRD + manager-role
                         templates from config/ after `make manifests`
docs/, examples/         §13
test/e2e/                kind-based e2e
SPEC.md                  this document
```

Make targets: the standard kubebuilder set plus `helm-sync-crds`,
`helm-lint`, `helm-deploy`/`helm-uninstall` (dev), `docs-api-ref`
(generated API reference via crd-ref-docs), and `check-crd-compat` (CRD
backward-compatibility diff against `origin/main` in CI).

Conventions inherited from go-faster projects: `github.com/go-faster/errors`
(wrap only under non-nil checks), full-sentence comments, Conventional
Commits, `golangci-lint` clean.

## 13. Documentation

Documentation ships with the code and is part of "done" for every feature:

```
docs/
  overview.md            what it is, feature list, links
  install/               helm.md (primary), kubectl.md (kustomize)
  guides/
    configuration.md     every spec section: topology/racks, storage,
                         etcd, auth, S3/TLS, tuning, pod template
    scaling.md           scale-up, decommission, envelope limits
    upgrades.md          rolling updates, schema migration, rollback rules
    storage.md           disks, weights, expansion, reclaim policy
    buckets-and-keys.md  FSBucket / FSAccessKey
    monitoring.md        metrics, conditions and events reference
    security.md          secrets, network policy, peer-traffic caveats
  reference/
    api.md               GENERATED from api/v1alpha1 (crd-ref-docs);
                         CI fails when stale
examples/
  01-minimal.yaml            3-node flat dev cluster
  02-zonal-racks.yaml        3 racks × 2 nodes across zones
  03-multi-disk.yaml         multiple disks per node, weights
  04-erasure-coding.yaml     ec:4,2 with 6 nodes
  05-tls.yaml                S3 TLS via cert-manager Secret
  06-buckets-and-keys.yaml   FSBucket + FSAccessKey round-trip
  07-production.yaml         full production shape (resources, monitor,
                             network policy, external etcd w/ TLS)
```

Every example is exercised in e2e (applied, or at minimum server-side
dry-run validated), so the gallery cannot rot. Condition types, condition
reasons and event reasons are documented in `monitoring.md` as API surface.

## 14. Helm chart ownership

The chart is scaffolded once with the kubebuilder `helm/v2-alpha` plugin
into `dist/chart`, then committed and hand-owned: it deploys the operator
(manager Deployment, RBAC, metrics service, optional network policy /
Prometheus bits) and ships the CRDs as templates guarded by
`.Values.crd.enable`, with `helm.sh/resource-policy: keep` behind
`.Values.crd.keep` (default true). Because the CRDs and the manager's RBAC
are generated from Go types, the chart copy must never drift:
`hack/sync-chart.sh` wraps each `config/crd/bases/*.yaml` into its chart
template and rewrites the chart's manager-role from `config/rbac/role.yaml`
(a drifted manager role is not a lint failure but an operator that starts,
cannot list what it owns, and silently never reconciles); `make
helm-sync-crds` runs it after `manifests`, and CI fails on any diff.

Releases are keyed by git tag (`vX.Y.Z`): the release workflow publishes
the multi-arch operator image to `ghcr.io/go-faster/fs-operator:vX.Y.Z` and
the chart to `oci://ghcr.io/go-faster/charts/fs-operator` with chart
version `X.Y.Z` and `appVersion vX.Y.Z` (the chart's default image tag), so
a chart release always pulls its matching image.

For air-gapped installs `global.imageRegistry` replaces the registry host of
every image from a private mirror. It rewrites the manager image in the chart
and is passed to the operator as `FS_IMAGE_REGISTRY` (`--fs-image-registry`),
which rewrites the fs node image of every `FSCluster` it reconciles — so a
mirror that keeps the `go-faster/...` path needs no per-cluster
`spec.image.repository`. The rewrite is host-replacement only (the leading
registry segment), shared between the chart template and the operator's
`ApplyRegistry` so the two never diverge.

## 15. Testing

- **Unit**: config renderer golden tests (spec → per-node config.yaml);
  the rolling state machine as a table-driven pure function over fake
  cluster-health snapshots; resource builders; **fuzz** on spec
  validation/defaulting (round-trip: any accepted spec renders a valid
  config).
- **envtest**: controller behavior against a real API server — secret
  generation idempotency, ownership/GC, per-node STS fan-out, status
  conditions, refusal paths (scheme/topology mismatch, undrained
  scale-down, disk shrink). fs admin/S3 endpoints faked with httptest.
- **e2e (kind)**: 1 control-plane + 3 workers with zone topology labels;
  deploy the operator via `dist/chart`, a minimal 3-pod etcd, then: 3-node
  FSCluster → S3 smoke (bucket, put/get via minio-go) → FSBucket +
  FSAccessKey round-trip → image bump → observe strictly-one-at-a-time roll
  with convergence gates → scale 3→4 → decommission 4→3 → delete cluster,
  assert cleanup. Examples gallery validated in the same run. E2E specs are
  labeled per area so a single scenario can run in isolation.
- **Chaos (later)**: kill a pod mid-rollout, assert the operator halts.

## 16. Phasing

| Phase | Scope |
|---|---|
| **P1 — core** | FSCluster CRD + controller: provisioning (secrets, per-node configs + StatefulSets, services, PDB), conditions/status, flat + racks topology, owned Helm chart + CRD sync, docs skeleton + examples 01–03, envtest + kind e2e. Rolling updates gated on ready + repair-queue only (until fs §11.2). |
| **P2 — day-2** | Full convergence-gated rollouts + migration Job (fs §11.1/2/7), hot reload with revision verification, scale-up, PVC expansion, PodMonitor, NetworkPolicy, docs guides complete. |
| **P3 — tenancy** | FSBucket (+ scheme once fs §11.3), FSAccessKey via config rendering + verified reload, examples 04–07. |
| **P4 — lifecycle** | Decommission/scale-down with drain observability (fs §11.2/6), disk add/remove, etcd TLS (fs §11.4), managed dev-grade etcd, admission webhook (cross-field validation moves out of the controller), Grafana dashboards, CRD-compat CI gate. |

## 17. Resolved decisions

1. **Managed etcd is dev-only, permanently** (resolved 2026-07-24). The
   operator ships `etcd.managed: {}` strictly as a dev/demo convenience and
   will never harden it for production; `etcd.external` is the production
   path. See §2.
2. **FSAccessKey: generated by default, plus `existingSecretRef` import**
   (resolved 2026-07-24). Imported credentials come from a user-managed
   Secret (Vault/ExternalSecrets-friendly), are min-length validated, and
   hot-reload on rotation. See §7.
3. **Racks are explicit spec, never discovered** (resolved 2026-07-24).
   Rack membership is declared in `topology.racks[]` and pinned with
   nodeAffinity; it is never derived from where a pod happens to be
   scheduled. Failure-domain identity must be stable across rescheduling —
   discovery would make the failure model advisory. Upstream
   `FS_CLUSTER_RACK` (§11.5) drops to a pure nice-to-have.
4. **One cluster-wide operator instance** (resolved 2026-07-24). The
   documented deployment is a single installation watching all namespaces;
   tenancy comes from the namespaced CRs and RBAC on them. The chart keeps
   a `watchNamespaces` value as an escape hatch, documented with the CRD
   version-skew caveat of running multiple instances.
5. **Uniform `podTemplate` only** (resolved 2026-07-24). One pod template
   for the whole cluster; racks carry only scheduling (zone/nodeSelector)
   and disk weights absorb uneven capacity. Per-rack overrides remain a
   compatible future extension if a real deployment demands them.
6. **The admin listener stays operator-internal** (resolved 2026-07-24).
   No dedicated admin Service, ever: the admin API is per-node state
   (rebalance status, repair queue, runtime keys), so a load-balanced
   endpoint would answer from a different node per request; and it manages
   credentials, so it gets no stable routable exposure. It is reachable
   only per pod through the headless peers Service (bearer token required;
   NetworkPolicy-restrictable via `spec.networkPolicy`); humans use
   `kubectl port-forward` to a specific pod plus the
   `<cluster>-admin-token` Secret.
