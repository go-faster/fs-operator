# fs-operator — Kubernetes operator for go-faster/fs clusters

Status: runs **fs v0.14** — one storage engine and a Garage-style layout over
peer nodes. [PLAN.md](PLAN.md) tracks what is next; this document is the
design of record.

This document is the specification for `fs-operator`: a Kubernetes operator
(kubebuilder v4) that provisions and operates
[go-faster/fs](https://github.com/go-faster/fs) deployments — an S3-compatible
object store whose nodes share a versioned **layout** assigning every
partition of the data to nodes spread over failure domains (zones, then
racks). Metadata is replicated to three nodes at quorum; object data is
replicated (`rf3`) or erasure coded (`ec:k,m`) per bucket.

The operator exists because a clustered fs deployment is not "a StatefulSet
with N replicas": it has per-node identity, failure-domain (zone/rack)
assignment, a layout that has to be applied when nodes join or leave, a
strict *one-node-at-a-time with reconvergence* upgrade contract, and nodes
that must keep running after they leave until their data has moved. The Helm
chart in `go-faster/fs` (`helm/go-faster-fs`) can only template the static
shape; the operator owns the day-2 choreography.

---

## 1. Goals

- **Provision** a complete fs cluster from one custom resource: per-node
  StatefulSets each with one PVC-backed data volume, headless + client
  Services, rendered per-node config, generated secrets (cluster secret,
  admin token, root S3 credentials), PDB, and the cluster layout.
- **Topology-aware**: map fs *zones and racks* (failure domains) onto
  Kubernetes zones or arbitrary node sets; keep the failure model honest with
  required pod anti-affinity.
- **Safe day-2 operations**, encoding fs's operational contracts as
  controller logic:
  - rolling image/config updates one node at a time, gated on cluster
    reconvergence between nodes;
  - scale-up (new nodes join the layout once they are up) and scale-down
    (removed nodes leave the layout and keep running until the layout
    transition has moved their data);
  - hot reload (credentials, TLS certs) without restarts, with per-node
    verification that the reload actually applied.
- **Declarative tenancy primitives**: buckets (`FSBucket`) and S3 credentials
  (`FSAccessKey`) as CRs, reconciled against the cluster's config, admin and
  S3 APIs.
- **Own the Helm chart from day one**: the operator's chart lives in
  `dist/chart`, is committed and hand-maintained (scaffolded once by the
  kubebuilder `helm/v2-alpha` plugin, then owned), with CRDs synced from
  `config/crd` by a hack script so the chart can never drift.
- **Documentation as a deliverable** (§13): task-oriented guides, a numbered
  example gallery, and an API reference generated from the Go types.

## 2. Non-goals (v1alpha1)

- **No autoscaling.** Cluster membership changes are deliberate,
  capacity-planned operations. No HPA, ever, for the data plane.
- **No Ingress/HTTPRoute/Gateway management** for S3 traffic. The operator
  exposes a Service; routing is composed by the user.
- **No backup/restore or cross-cluster replication.** Durability is the
  cluster's replication scheme; disaster recovery is out of scope for now.
- **No certificate issuance.** TLS material comes from Secrets (cert-manager
  or hand-made); the operator mounts and hot-reloads it.
- **No backward compatibility before v1.** The API and what the operator
  renders follow fs as it is; a release that changes either breaks objects
  and clusters built by an earlier one, and there is no migration path.
- **No automatic release of a dead node.** A node that is gone for good can
  hold a layout transition open forever; releasing it (`fs layout skip
  <id>`) gives up whatever only it held, which is a human decision. The
  operator reports the stuck transition and never skips on its own.
- **No cluster-secret rotation.** Peer HMAC auth uses a single shared secret;
  mixed secrets partition the cluster. Rotation is a documented manual
  procedure until fs grows dual-secret support.

---

## 3. Background: what fs cluster mode requires from an orchestrator

Facts about go-faster/fs v0.14 that drive the design (source: `cmd/fs`,
`engine`, `internal/cluster/{layout,peer}`, `docs/DEPLOYMENT.md`):

- Every node runs the same `fs s3` binary with a YAML config. The config is
  **strict**: a key fs does not know stops the node at startup. Cluster mode
  is a `cluster:` section: `node_id`, peer listener `addr` /
  `advertise_addr` (default `:7080`), `peers` to join through (one that
  answers is enough; gossip learns the rest) and the shared `secret` (HMAC
  peer auth, min 16 chars).
- Per-instance identity is injectable via env — `FS_CLUSTER_NODE_ID`,
  `FS_CLUSTER_ADVERTISE_ADDR`, `FS_CLUSTER_SECRET`; the admin token and the
  root credential come from `FS_ADMIN_TOKEN` and `FS_ROOT_ACCESS_KEY` /
  `FS_ROOT_SECRET_KEY`.
- Everything a node stores — metadata, blocks, the adopted layout, runtime
  state — lives under one `storage.root`. There are no disks or weights.
- **The layout** is cluster state applied through the admin API
  (`POST /api/v1/cluster/layout`) and spread by gossip. It takes every
  member's role — `id`, `zone`, `rack`, `capacity` in bytes — and the
  `widths` to spread for: 3 for replicated data, `k+m` for each erasure
  scheme a bucket uses. It assigns 256 partitions to node slots spread over
  zones first, then racks, balanced by capacity. fs refuses a layout with
  fewer members holding capacity than the widest width.
- **A layout change is a transition.** The previous versions stay
  *retained* — writes go to every retained version's replicas, reads to the
  oldest's — until every node holding data has synced the new one; then they
  retire. `GET /api/v1/cluster/layout` reports `retained_versions`, empty when
  nothing is moving. A removed node must keep running until then. A dead node
  blocks retirement until it is released with `fs layout skip <id>`
  (`POST /api/v1/cluster/nodes/{id}/skip`).
- **No layout, no service.** Until the first layout is applied a node
  serves no S3 request, and `/ready` (a `ListBuckets` probe) answers 503.
  Gossip works without a layout: `GET /api/v1/cluster/nodes` lists every
  peer, up or not, with the layout version it last reported.
- **Single-node mode**: a node with no `cluster:` section is a one-node
  layout of its own — no peers, no layout to apply, no replication.
- Health endpoints: `/health` (liveness) and `/ready` (readiness) on the S3
  listener.
- Admin API (separate listener, bearer token): `/api/v1/info` (incl. the
  applied config `revision`), `/api/v1/reload`, `/api/v1/access-keys`,
  `/api/v1/cluster/{layout,nodes}`, `/api/v1/buckets/{bucket}/scheme`.
- Keys created through `POST /api/v1/access-keys` are stored **per node**,
  under that node's root. Keys in the config file are what every node
  accepts.
- `SIGHUP` or `POST /api/v1/reload` hot-reloads credentials, grants,
  public-read buckets and the TLS certificate — nothing else. All other
  config changes need a process restart. `cluster.peers` is read only at
  startup.
- **Upgrade contract**: one node at a time; wait for the cluster to
  reconverge before touching the next node. All nodes of a cluster run the
  same release.
- Observability: OTEL SDK via standard env vars (traces/metrics/logs),
  Prometheus metrics exporter, pprof via `PPROF_ADDR`; cluster metrics
  include `fs.cluster.layout.retained` and `.synced`.

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
                                 │
                                 ▼ admin API
                          cluster layout
```

### 4.1 One StatefulSet per node

The operator manages **one single-pod StatefulSet per fs node**, not one
StatefulSet with N replicas. This is the load-bearing decision; it buys:

- **Per-node configuration.** Each node gets its own rendered `config.yaml`
  (Secret) carrying its identity and peers.
- **Exact rollout control.** A rolling change is "update node i's
  StatefulSet, let the StatefulSet controller replace the pod, gate on
  reconvergence, proceed to node i+1" — the native workload machinery does
  pod replacement; the operator only sequences. No `OnDelete` + manual pod
  deletion choreography.
- **Per-node storage surgery.** PVC expansion is a per-node orphan-recreate
  touching exactly one node at a time (§8.5).
- **Independent scheduling and removal.** Rack→zone pinning is per-node
  nodeAffinity; scale-down removes specific named nodes, and a removed node
  keeps exactly the StatefulSet it runs until its data has moved (§8.4).

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
| Node name / fs `node_id` / layout member | `<cluster>-<rack>-<n>` (flat: `<cluster>-<n>`) |
| StatefulSet (per node) | `<node>` → pod `<node>-0` |
| Data volume (per node) | claim template `data` → PVC `data-<node>-0` |
| Advertise address | `<node>-0.<cluster>-peers.<ns>.svc:7080` |
| Headless service | `<cluster>-peers` (publishNotReadyAddresses) |
| Client service | `<cluster>` (S3 port) |
| Per-node config Secret | `<node>-config` |
| Cluster secret / admin token / root creds | `<cluster>-{cluster-secret,admin-token,root-credentials}` |

- The operator talks to fs over two client channels: the **admin API**
  (bearer token, per-pod DNS via the headless service) for the layout, the
  gossip view, reload and access-key verification; and the **S3 API** (root
  credentials, client Service) for bucket CRUD. Admin clients are pooled and
  keyed by endpoint and token.
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
    # against (currently v0.14.1). Always a pinned version, never a floating
    # tag — cluster upgrades are deliberate, one-node-at-a-time operations.
    tag: v0.14.1
    # digest: sha256:...   # wins over tag
    pullPolicy: IfNotPresent
    pullSecrets: []

  topology:
    # Exactly one of `nodes` (flat) or `racks` (failure domains).
    #
    # Flat: N nodes, each its own failure domain. 1, or 3–16; two is refused.
    # nodes: 3
    #
    # Racks: each node joins the layout with the rack's name as its fs rack
    # and the rack's zone as its fs zone; partitions spread over zones first,
    # then racks.
    racks:
      - name: a
        nodes: 2
        # Sugar for nodeAffinity on topology.kubernetes.io/zone, and the fs
        # zone of the rack's nodes.
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
    # One PVC per node, mounted at the storage root /var/lib/fs. Its size is
    # also the node's capacity in the layout. May only grow.
    size: 200Gi
    storageClass: fast-nvme       # optional; cluster default otherwise
    # PVC handling when nodes are removed / the cluster is deleted.
    reclaimPolicy: Retain         # Retain | Delete

  layout:
    # Slot counts the layout spreads over distinct failure domains: 3 for
    # rf3, k+m for every erasure scheme a bucket uses. Each width needs at
    # least that many nodes.
    widths: [3, 6]                # default [3]

  # Secret with key `secret` (min 16 chars). Generated if omitted. Immutable
  # (no rotation in v1alpha1 — see non-goals).
  clusterSecretRef: null

  auth:
    # Secret with keys `access-key` / `secret-key`, granted admin on all
    # buckets (FS_ROOT_ACCESS_KEY / FS_ROOT_SECRET_KEY). Generated if
    # omitted.
    rootCredentialsSecretRef: null
    # Buckets readable anonymously. Rendered into every node's config and
    # hot-reloaded.
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

  updatePolicy:
    # Gate between node restarts during rolling changes: every pod ready,
    # every node up on the current layout and no layout change in
    # transition, up to convergenceTimeout, before touching the next node.
    convergenceTimeout: 30m

  observability:
    # go-faster/sdk env (github.com/go-faster/sdk#reference).
    otlp:
      endpoint: ""
      protocol: grpc            # the transport a signal uses unless it
                                # names its own
    # Every exporter is named rather than defaulted: the SDK sends all three
    # signals to OTLP at localhost:4318 otherwise. Traces follow the endpoint
    # (otlp when there is one, none when not), metrics are scraped, and logs
    # are none until asked for — fs writes them to stdout already.
    # endpoint/protocol override otlp.* for one signal.
    traces:  {exporter: "", endpoint: "", protocol: ""}   # otlp | none
    logs:    {exporter: "", endpoint: "", protocol: ""}   # otlp | none
    metrics: {exporter: "", endpoint: "", protocol: ""}   # prometheus | otlp | none
    logLevel: info
    # PPROF_ADDR on :9010. false drops the listener, its container port and
    # its NetworkPolicy rule.
    pprof: true
    # Added to the operator's OTEL_RESOURCE_ATTRIBUTES, not substituted for
    # them. Anything else the SDK reads goes through podTemplate.extraEnv,
    # which is applied last and wins.
    resourceAttributes: {}
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

- `topology`: exactly one of `nodes`/`racks` (CEL). The total is 1 or 3–16:
  two nodes cannot hold three copies of the metadata, so `nodes: 2` is
  refused by CEL and any two-node total by the cross-field check
  (`UnsupportedTopology`). **1 node selects single-node mode** (§5.2). Rack
  names are DNS-label and immutable per entry; removing a rack or lowering a
  node count removes nodes (§8.4).
- `layout.widths`: 1–8 distinct values in 1–64 (CEL), defaulted to `[3]`.
  The widest must not exceed the node count (`LayoutTopologyMismatch`); a
  width wider than the topology's failure domains is admitted with a warning
  — fs then puts two slots of some partitions in one domain. Ignored by a
  single node.
- `storage.size`: must be positive (`SpecInvalid`) and may only grow
  (`StorageShrinkForbidden`, checked on update).
- `observability`: an `otlp` exporter needs a destination — its own
  `endpoint` or the shared `otlp.endpoint`; a per-signal `protocol` cannot be
  combined with `otlp.protocol` (the SDK reads the shared one first); and
  `podMonitor` needs `metrics.exporter: prometheus`. These are pairs the API
  accepts field by field and none reads as wrong on its own, which is why
  they are checked together rather than discovered in an empty dashboard.

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
  operator refuses to build. A refused spec sets `SpecValid=False` with the
  reason above and mutates nothing.

  Checks that need to read cluster state (a referenced Secret existing, a
  live PVC's size) stay in the controller only: the admission path must not
  call the API server, and a Secret created after the cluster is a
  legitimate order of operations.
- Immutable (CEL): `clusterSecretRef`, rack `name`s.
- Defaults are applied by the controller through a single `WithDefaults()`
  method on the spec type (unit-testable); static defaults also carry
  `+kubebuilder:default` markers so `kubectl explain` and the CRD schema tell
  the truth. The webhook validates a defaulted copy and never writes the
  defaults back.

### 5.2 Single-node mode

`topology.nodes: 1` is not a small cluster: the node runs with no `cluster:`
section, so it is a one-node layout of its own — no peers, no layout to
apply, no replication, no failure tolerance.

What the operator does differently for it:

- **Validation** (§5.1): `layout.widths` is not checked against the node
  count. Crossing the line in either direction — 1 ↔ N nodes — is refused on
  update (`UnsupportedTopology`): a single node's data is not part of any
  layout a new cluster could adopt. A permanent warning says the node's loss
  is the data's loss.
- **Rendering**: the config carries no `cluster:` section. The pod, its
  claim and its mounts are the cluster-mode ones.
- **Skipped steps**: the layout step does nothing, and convergence is
  reported as converged rather than unknown — unknown is what holds a
  rollout.
- **Readiness**: no quorum, so `Ready` follows the one node.
- **`FSBucket`**: `spec.scheme` is refused (`SchemeRejected`): there is no
  layout to spread a code over. Everything else about buckets and access
  keys is unchanged.

### 5.3 Status

```yaml
status:
  observedGeneration: 7
  nodes: 6                 # nodes in the pass (declared + being removed)
  readyNodes: 6
  upNodes: 6               # nodes the cluster's gossip view reports up
  configurationRevision: cfg-6b9f7c   # hash of desired rendered configs
  statefulSetRevision: sts-4c11ab     # hash of desired pod templates
  currentRevision: cfg-6b9f7c  # revision every node last converged to
  updateRevision: cfg-6b9f7c   # revision being rolled out
  layout:
    version: 4             # the layout version the cluster runs
    members: 6             # nodes the layout gives data to
    widths: [3, 6]
    retainedVersions: []   # older versions still in transition
  update:                  # present while a rolling change is in flight
    phase: RollingNodes    # Preflight | RollingNodes | Draining
    node: prod-b-1         # node being replaced, or nodes being removed
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
| `Ready` | The cluster serves S3 at write quorum: a layout exists and the failure domains with a node down are at most one (given three or more domains; none otherwise). Reasons `QuorumAvailable`, `QuorumUnavailable`, `LayoutPending`. |
| `NodesHealthy` | Every node pod is Ready. |
| `ClusterSizeAligned` | Actual node set and layout match the topology. False while scaling (`ScalingUp`), rolling (`RollingNodes`), resizing (`StorageExpanding`), waiting to apply a layout (`LayoutPending`), refused by fs (`LayoutRejected`) or removing nodes (`Draining`). |
| `ConfigurationInSync` | Every node runs the desired configuration revision (hot reload verified per node). |
| `Converged` | A layout exists, no older version is retained, and every node is up on the current version (gates rollouts). Reasons `Converged`, `LayoutTransition`, `LayoutPending`, `ConvergenceTimeout`. |

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
  # rf3 | ec:k,m. An erasure scheme needs width k+m in the cluster's
  # spec.layout.widths. Empty leaves the bucket's scheme alone.
  scheme: ""
  reclaimPolicy: Retain       # Retain | Delete
status:
  conditions: [ ... Ready ... ]
  scheme: rf3                 # the scheme in effect
```

Reconcile: ensure the bucket exists (S3 `CreateBucket` with root credentials
via the client Service); set the scheme through the admin API (§11.3) when
one is given, otherwise read the one in effect; add a finalizer. A scheme
change applies to data written from then on; existing data keeps the scheme
it was written with. fs refuses a coded scheme whose width the layout is not
spread for, which surfaces as `Ready=False`, reason `SchemeRejected`. On
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
  #    e.g. minted by Vault / ExternalSecrets. The operator watches it, so
  #    external rotation propagates. secret-key must be ≥16 chars (refused
  #    otherwise: Ready=False/WeakSecretKey). No operator-owned Secret is
  #    created in this mode.
  # existingSecretRef:
  #   name: vault-minted-s3-creds
  grants:
    - bucket: "media-*"     # glob, matches fs grant semantics
      permission: write     # read | write | admin
status:
  conditions: [ ... Ready ... ]
  accessKey: AKprod4f2…     # non-secret half, for reference
```

Design decision: credentials are **rendered into every node's config** and
hot-reloaded. fs v0.14 stores keys created through the admin API per node,
under that node's root, so a key created that way would work only on the node
that received the call; keys in the config are accepted by every node, a new
node starts with them, and a reload applies a change everywhere at once.

The two controllers split the work:

- The **FSAccessKey controller** resolves the credential (generate once into
  an owned Secret, or read `existingSecretRef` — it watches referenced
  Secrets and maps them back to their FSAccessKeys) and stamps a fingerprint
  of the material on the key (`fs.go-faster.org/credential-hash`). It then
  lists the access keys on every node's admin API: `Ready=True`
  (`KeyAccepted`) once every node lists the key, `Ready=False`
  (`ConfigReloadPending`) naming the nodes that do not yet. On deletion its
  finalizer holds until no node that answers still lists the key — deleting
  the object is revoking the key; a node that does not answer reads the
  re-rendered config when it next starts.
- The **FSCluster controller** watches FSAccessKeys and renders every key of
  the cluster — skipping those being deleted, without a readable Secret, or
  with a secret shorter than 16 characters — into `auth.keys` of every
  node's config, sorted by access key, then reloads and verifies (§8.3). A
  fingerprint change is an update to the FSAccessKey, which is how an
  imported Secret's rotation reaches the cluster.

The public-read list (`spec.auth.publicReadBuckets`) is rendered the same
way. The root credential stays in the environment.

---

## 8. FSCluster reconciliation

The reconciler is a sequential **step pipeline**; each step returns
continue / requeue-after / blocked (blocked skips the remaining mutating
steps, while status-refreshing steps marked *always-run* still execute).
Steps: validate → decommission planning → render → observe (always) →
secrets → services → configs → convergence → storage → nodes (the rolling
state machine, §8.2) → layout (§8.4) → drain (§8.4) → reload (§8.3) → PDB →
NetworkPolicy → PodMonitor → status (always). Every pass is idempotent and
each step is unit-testable in isolation. An FSCluster has no finalizer:
everything it owns is garbage-collected (§8.6).

### 8.1 Resource graph

Rendered per reconcile, compared semantically, applied with server-side
apply (field manager `fs-operator`):

1. **Secrets** — cluster secret / admin token / root credentials: generated
   once if no `*Ref` is given (crypto/rand, 32 bytes), never regenerated.
2. **Per-node config Secret** — full `config.yaml` (it embeds credential
   material, so a Secret, not a ConfigMap):
   - `server`: addr `:8080`, health `/health`, timeouts; `tls` pointing at
     the mounted certificate when `s3.tls.secretName` is set;
   - `storage`: root `/var/lib/fs`;
   - `auth`: the cluster's FSAccessKeys (§7) and `publicReadBuckets`;
   - `admin`: enabled, `addr: :8090` (pod network; bearer token via env);
   - `cluster` (omitted on a single node): `node_id: <node>`, `addr: :7080`,
     `advertise_addr: <node>-0.<cluster>-peers…:7080`, and `peers`: every
     other declared node's advertise address;
   - `observability` switches, and a `revision` marker (§8.3).
   The cluster secret is env-injected (`FS_CLUSTER_SECRET`), never written
   into the file.
3. **Per-node StatefulSet** — one replica, `serviceName: <cluster>-peers`,
   `persistentVolumeClaimRetentionPolicy` from `storage.reclaimPolicy`, one
   volumeClaimTemplate `data` sized `storage.size`. Pod template:
   - env: `FS_CLUSTER_SECRET` / `FS_ADMIN_TOKEN` / root creds via
     secretKeyRef, OTEL env, `PPROF_ADDR`;
   - ports: http 8080, peer 7080, admin 8090, metrics 9464, pprof 9010;
   - probes: liveness `/health`, readiness `/ready`, generous startup probe;
   - volumes: config Secret at `/etc/fs`, TLS Secret when set, and the data
     PVC at the storage root `/var/lib/fs`, writable — the container
     filesystem is read-only and fs writes everything below the root;
   - securityContext: runAsNonRoot 1000, readOnlyRootFilesystem, seccomp
     RuntimeDefault, drop ALL;
   - `fs.go-faster.org/restart-revision` pod annotation: the fingerprint of
     the *restart-requiring* part of the config. It excludes what a reload
     re-reads (`auth`), what fs reads only at startup (`cluster.peers`) and
     the revision marker, so a credential change or a scale change never
     rolls the cluster (§8.2/§8.3). The full config revision rides on the
     config Secret as `fs.go-faster.org/config-revision`;
   - per-rack nodeAffinity (zone/nodeSelector) + anti-affinity across the
     cluster's pods per `topology.podAntiAffinity`.
4. **Services** — `<cluster>-peers` headless (`publishNotReadyAddresses:
   true`, because no node is Ready before the first layout and peers must
   still resolve each other; 7080/8090/9464) and `<cluster>` client (S3
   port).
5. **PodDisruptionBudget** — `maxUnavailable: 1` over all cluster pods.
   Voluntary evictions can never take two failure domains down;
   non-negotiable, always created.
6. **NetworkPolicy** — optional (`spec.networkPolicy`, §9).
7. **PodMonitor** — optional, created only if the `monitoring.coreos.com`
   API group is discoverable.
8. **Layout** — not a Kubernetes object but cluster state the operator owns,
   applied through the admin API (§8.4).

### 8.2 Rolling changes (image, restart-required config)

Two desired-state revisions are computed each pass: the **configuration
revision** (hash of rendered configs) and the **pod-template revision**.
A node needs a *restart* when its StatefulSet template is stale (which
includes a change to the restart revision); it needs a *reload* (§8.3)
otherwise.

The **convergence** read each pass, from the first node whose admin API
answers (serving nodes first, then the rest — before the first layout no
node is Ready): the node's layout and its gossip view. The cluster is
*converged* when a layout exists, no older version is retained, and every
node of the pass is up in the view and on the current layout version.
Unknown — no node answered — is not converged.

State machine, persisted in `status.update`:

```
Idle
 └─ stale pod template detected
Preflight        all pods Ready ∧ Converged — else hold
RollingNodes     for one node at a time (racks round-robin, so two nodes of
                 one domain are never adjacent in the order):
                   apply node's StatefulSet →
                   StatefulSet controller replaces the pod →
                   wait every pod Ready → wait Converged → next node
                 gate timeout (updatePolicy.convergenceTimeout) ⇒
                 Converged=False/ConvergenceTimeout + event; HALT — never
                 touch a second node while the cluster is unconverged;
                 resumes automatically when the gate passes
Idle             currentRevision = updateRevision
```

New nodes are not part of a rollout: their StatefulSets are created at once
(they join through the layout step, §8.4). Rollback = the user reverting
`spec`; the same machinery rolls back node-by-node.

### 8.3 Hot config changes

If a node's pod template is current and only its config changed —
credentials and grants, public-read buckets, the peer list, the TLS
certificate's content — the operator updates the config Secret and calls the
admin reload endpoint on that node instead of restarting it.

Verification is per node and revision-based: the rendered config embeds its
own revision (`revision:`), and the node reports the revision it has
applied (`GET /api/v1/info`, and the reload result). Kubelet Secret
propagation can lag (~1m), so reload is retried until the node reports the
target revision; `ConfigurationInSync` flips True when every node does. A
node being replaced is left to the rollout: its new pod loads the new config.

### 8.4 Scale-up, layout and removal

The operator owns the layout. Its desired form is every declared node with
`zone` = its rack's zone, `rack` = its rack's name (both empty in the flat
topology) and `capacity` = `storage.size` in bytes, spread for
`layout.widths`. The layout step (cluster mode only) applies it when it
differs from the layout the cluster runs, and only when:

- the convergence is known (some node answered);
- every declared node has a StatefulSet (until then the status reports
  `ScalingUp`);
- no earlier change is still in transition;
- every declared node is up in the cluster's gossip view — no slot is handed
  to a node nobody can reach. Before the first layout this is the only
  signal: no pod is Ready yet.

Otherwise it holds with `ClusterSizeAligned=False/LayoutPending` naming what
it waits for. A layout fs refuses (`ErrLayoutRejected`, HTTP 400) is reported
as `ClusterSizeAligned=False/LayoutRejected` plus a Warning event. An
applied layout is reported by a `LayoutApplied` event. The same step carries
growth of `storage.size` (the node's capacity) and a new width into the
layout.

- **Scale-up** (`nodes` increased or a rack added): the new nodes'
  StatefulSets are created at once; once they are up in the gossip view the
  next layout gives them a share, and fs moves data to them.
- **Scale-down** (`nodes` decreased or a rack removed): every node the spec
  no longer declares is removed in one change.
  1. **Plan**: the nodes whose StatefulSets exist but are not declared stay
     in the pass — counted by the health, rollout and disruption-budget
     gates — and keep the StatefulSet they already run, unchanged. A removed
     node is never rebuilt from a spec that no longer describes it, which
     would risk moving the pod away from its own data. A `NodeDraining` event
     announces the removal once.
  2. **Leave the layout**: the layout step applies a layout without them;
     fs's transition moves their data while the old version keeps serving
     reads.
  3. **Wait**: hold (`update.phase: Draining`,
     `ClusterSizeAligned=False/Draining`) while no node answers, while any
     removed node is still a layout member, or while any older version is
     retained — a retained version is one fs still reads from, and its
     replicas include the removed nodes.
  4. **Remove**: delete each node's StatefulSet and config Secret; its PVC
     follows `storage.reclaimPolicy` through the StatefulSet's claim
     retention policy. A `NodeRemoved` event per node.

  All of them leave together: the transition already keeps every
  acknowledged write, so removing them one at a time would only move data
  more than once. Every gate resolves unknown to *wait*: a stalled removal
  is recoverable, a node deleted while fs still read from it is not. If a
  node dies for good during a transition, the transition does not complete
  until a human releases the node with `fs layout skip <id>` (§2); the
  status and `fsoperator_cluster_layout_retained_versions` show it stuck.

  The total may never drop below three nodes (or the widest width); such a
  spec is refused outright, and nothing leaves on the way to it.

### 8.5 Storage changes

- **Size increase**: per node, one at a time and only while every node is
  serving and the cluster converged — patch the PVC (requires
  `allowVolumeExpansion` on the StorageClass), then orphan-recreate that
  node's StatefulSet (delete leaving the pod orphaned, re-apply with the new
  volumeClaimTemplate) so a future pod replacement claims the right size.
  The orphaned pod keeps the previous revision hash after re-adoption, so
  the operator then replaces it (one node at a time, every other node
  serving); otherwise the node would read as not serving for good. The
  layout step raises the nodes' capacity in the layout.
- **Shrink** is refused: Kubernetes cannot shrink a PVC
  (`StorageShrinkForbidden`, at admission and in the controller, which also
  compares each live claim template).

### 8.6 Deletion

Owned resources carry ownerRefs, so garbage collection takes them down, and
PVCs follow `storage.reclaimPolicy` through each StatefulSet's claim
retention policy. Nothing the operator creates lives outside that graph, so
an FSCluster carries no finalizer.

### 8.7 Failure handling

- A pod failing mid-rollout: the state machine keeps waiting on its gates
  and surfaces `Converged=False` + events after `convergenceTimeout` — no
  automatic destructive remediation (fs anti-entropy repairs data; the
  operator never touches a second node while the cluster is unconverged).
- Involuntary node loss: Kubernetes reschedules the pod (same PVC, volume
  topology permitting). The operator does not force-delete pods stuck on
  dead nodes in v1alpha1; auto-remediation needs fencing and is future work.
- A node gone for good holds a layout transition open; releasing it is
  manual (§2, §8.4).
- Requeue is watch-driven plus a slow resync (5m) refreshing health-derived
  status (layout, gossip view) from the admin API.

---

## 9. Security

- All generated secrets are 32-byte crypto/rand values; nothing secret is
  ever placed in ConfigMaps, annotations, inline env values (secrets come
  via `valueFrom.secretKeyRef`) or logs. FSAccessKey credentials are in each
  node's config Secret, which is where fs reads them.
- fs peer traffic (7080) is HMAC-authenticated but **not encrypted**; docs
  say so plainly, and `spec.networkPolicy: true` restricts 7080/8090 to
  cluster pods + the operator namespace.
- The admin listener requires the bearer token; the token Secret never
  leaves the namespace.
- RBAC (operator): apps/StatefulSets, core Secrets/Services/Pods
  (`delete` on pods only to replace an orphan-adopted pod, §8.5)/PVCs,
  policy/PDB, NetworkPolicies, monitoring PodMonitors (optional), the three
  CRs + status + finalizers.
- fs pods: non-root (uid 1000), read-only rootfs, no capabilities, seccomp
  RuntimeDefault.

## 10. Observability

- Operator metrics (controller-runtime plus), in `internal/metrics`:
  `fsoperator_cluster_ready{namespace,cluster}`,
  `fsoperator_cluster_nodes{namespace,cluster,state}` (`declared`, `ready`,
  `up`), `fsoperator_cluster_layout_retained_versions{namespace,cluster}`,
  `fsoperator_update_phase{namespace,cluster,phase}`,
  `fsoperator_update_duration_seconds{namespace,cluster}`,
  `fsoperator_reconcile_errors_total{controller}`.

  `layout_retained_versions` is the number that goes wrong: it is non-zero
  while a layout change moves data and stays non-zero when one is stuck —
  a removed node kept running, or a dead node waiting to be released. The
  gap between `ready` and `up` is a pod Ready that its peers cannot reach.

  Every series carries `namespace` as well as `cluster`: the operator is
  cluster-wide, so two namespaces may hold an FSCluster of the same name and
  a `cluster`-only label would silently merge them.

  `update_phase` publishes 0 for the phases a cluster is *not* in rather than
  omitting them — an absent series and a false one read the same to an alert
  that has never seen the cluster. And every cluster-keyed series is dropped
  when its FSCluster is deleted: a gauge that outlives its object reports
  `ready=0` forever, on a name nothing will reconcile again, which is
  indistinguishable from an outage that never resolves.
- Events on every transition: rollout started/gated/halted/finished
  (`NodeRolling`, `RolloutWaiting`, `RolloutStuck`, `RolloutComplete`,
  `NodesCreating`), layout applied/refused (`LayoutApplied`,
  `LayoutRejected`), removal (`NodeDraining`, `NodeRemoved`), storage
  (`StorageExpanding`), reload (`ConfigReloaded`, `ReloadFailed`), refused
  spec changes. Event reasons are part of the documented API surface (§13).
- fs pods get their go-faster/sdk environment from `observability`: log
  level, the shared OTLP destination, a per-signal exporter, endpoint and
  transport for traces/logs/metrics, the pprof listener, and resource
  attributes merged over the operator's own. Every exporter is named
  explicitly, since the SDK defaults all three to OTLP at localhost. The rest
  of the SDK's variables are `podTemplate.extraEnv`, applied last so an
  override wins.

---

## 11. go-faster/fs surfaces the operator depends on

The operator uses only fs's documented admin API and config file; nothing is
read from a node's disk or logs.

1. **Reload and config revision** — `POST /api/v1/reload` (credentials,
   grants, public-read buckets, TLS) and the top-level `revision` config
   marker fs echoes via `GET /api/v1/info` (`config_revision`) and the
   reload result. Drives §8.3.
2. **Layout and gossip view** — `GET`/`POST /api/v1/cluster/layout` (roles,
   widths, `retained_versions`; 404 before the first layout, 400 for a
   layout fs cannot build) and `GET /api/v1/cluster/nodes` (each peer's ID,
   whether it is up, the layout version it last reported). Drives §8.2 and
   §8.4.
3. **Bucket scheme via admin API** — `GET`/`PUT
   /api/v1/buckets/{bucket}/scheme` (`rf3` or `ec:K,M`; 400 when the layout
   is not spread for the width or on a single node, 404 for a missing
   bucket). Drives `FSBucket.spec.scheme`.
4. **Access-key listing** — `GET /api/v1/access-keys` on each node,
   config-defined and runtime keys alike. Drives FSAccessKey readiness and
   revocation (§7).
5. **Importable admin client** — `github.com/go-faster/fs/adminapi`, which
   `internal/fsclient` wraps so the reconcilers never handle generated
   optional types.

Known gap: keys created through the admin API are stored per node in fs
v0.14, which is why FSAccessKeys are rendered into the config (§7) rather
than created through the API. A cluster-wide runtime key store upstream
would let the operator stop carrying credentials in config Secrets.

---

## 12. Repository layout and tooling

Scaffold: kubebuilder v4, domain `go-faster.org`, repo
`github.com/go-faster/fs-operator`, project `fs-operator`.

```
api/v1alpha1/            fscluster_types.go, fsbucket_types.go,
                         fsaccesskey_types.go, conditions.go, defaults.go,
                         groupversion, deepcopy
internal/controller/     fsbucket and fsaccesskey controllers;
                         pipeline/ (the step pipeline); fscluster/
                         (controller, rolling state machine, layout,
                         removal, resource builders, config renderer, names)
internal/validation/     cross-field checks shared by webhook and controller
internal/webhook/        the validating admission webhook
internal/fsconfig/       mirror of the fs config file schema (upstream
                         cmd/fs/config.go), decoded as strictly as fs does
internal/fsclient/       admin API client wrapper and its connection pool
internal/keygen/         generated secret material
internal/metrics/        operator metrics
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
(generated API reference via crd-ref-docs) and
`fs-version`/`check-fs-version` (the pinned fs release, everywhere it is
spelled out).

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
                         layout, auth, S3/TLS, pod template
    scaling.md           scale-up, removal, layout transitions, limits
    upgrades.md          rolling updates, rollback rules
    storage.md           the data volume, expansion, reclaim policy
    buckets-and-keys.md  FSBucket / FSAccessKey
    deletion.md          what goes with a deleted cluster
    monitoring.md        metrics, conditions and events reference
    security.md          secrets, network policy, peer-traffic caveats
  reference/
    api.md               GENERATED from api/v1alpha1 (crd-ref-docs);
                         CI fails when stale
examples/                numbered gallery from a single node to the
                         production shape (zonal racks, erasure coding,
                         TLS, buckets and keys, telemetry)
```

Every example is exercised in e2e (applied, or at minimum server-side
dry-run validated), and every FSCluster in the gallery is decoded strictly
and run through `internal/validation` in the unit tests, so the gallery
cannot rot. Condition types, condition reasons and event reasons are
documented in `monitoring.md` as API surface.

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

- **Unit**: config renderer golden tests (spec → per-node config.yaml, each
  validated and decoded as strictly as fs decodes it); the fsconfig mirror's
  round trip and strictness; the admin client against the real ogen server
  for fs's admin API; resource builders; cross-field validation, including
  every FSCluster in the examples gallery.
- **envtest**: controller behavior against a real API server, with fs's
  admin API faked as a small in-memory cluster (layout, transitions, gossip
  view, per-node config revisions): secret generation idempotency,
  ownership/GC, per-node STS fan-out, the first layout waiting for every
  node, scale-up joining the layout, removal held until the transition
  finishes, rollouts gated on convergence, storage growth and the
  shrink refusal, quorum readiness, status conditions, the CRD's own
  validation rules, and the single-node pass (§5.2). FSAccessKey readiness
  and revocation are tested per node.
- **e2e (kind)**: 1 control-plane + 3 workers with zone topology labels;
  deploy the operator via `dist/chart`, then real fs pods: FSCluster → S3
  smoke (bucket, put/get via minio-go) → FSBucket + FSAccessKey round-trip →
  image bump → observe strictly-one-at-a-time roll with convergence gates →
  scale up → remove nodes → delete cluster, assert cleanup; and the
  single-node cluster (§5.2). Examples gallery validated in the same run.
  E2E specs are labeled per area so a single scenario can run in isolation.
- **Chaos (later)**: kill a pod mid-rollout, assert the operator halts.

## 16. Phasing

| Phase | Scope |
|---|---|
| **P1 — core** | FSCluster CRD + controller: provisioning (secrets, per-node configs + StatefulSets, services, PDB), conditions/status, flat + racks topology, owned Helm chart + CRD sync, docs skeleton + examples, envtest + kind e2e. |
| **P2 — day-2** | Convergence-gated rollouts, hot reload with revision verification, scale-up, PVC expansion, PodMonitor, NetworkPolicy, docs guides complete. |
| **P3 — tenancy** | FSBucket (+ scheme via fs §11.3), FSAccessKey via config rendering + verified reload, more examples. |
| **P4 — lifecycle** | Scale-down, admission webhook (cross-field validation shared with the controller), Grafana dashboards. |
| **fs v0.14** | The move from etcd/disks/rebalancing to the layout: one data volume per node, the operator-owned layout, removal by layout transition, config-rendered credentials verified per node. |

## 17. Resolved decisions

1. **The operator follows fs's model, not a compatibility layer**
   (resolved 2026-10-06). When fs removes something, the operator removes
   every field and step built on it rather than translating it, and
   v1alpha1 changes in place (§2).
2. **FSAccessKey: generated by default, plus `existingSecretRef` import**
   (resolved 2026-07-24). Imported credentials come from a user-managed
   Secret (Vault/ExternalSecrets-friendly), are min-length validated, and
   hot-reload on rotation. See §7.
3. **Racks are explicit spec, never discovered** (resolved 2026-07-24).
   Rack membership is declared in `topology.racks[]` and pinned with
   nodeAffinity; it is never derived from where a pod happens to be
   scheduled. Failure-domain identity must be stable across rescheduling —
   discovery would make the failure model advisory.
4. **One cluster-wide operator instance** (resolved 2026-07-24). The
   documented deployment is a single installation watching all namespaces;
   tenancy comes from the namespaced CRs and RBAC on them. The chart keeps
   a `watchNamespaces` value as an escape hatch, documented with the CRD
   version-skew caveat of running multiple instances.
5. **Uniform `podTemplate` and storage** (resolved 2026-07-24). One pod
   template and one volume size for the whole cluster; racks carry only
   scheduling (zone/nodeSelector). Per-rack overrides remain a compatible
   future extension if a real deployment demands them.
6. **The admin listener stays operator-internal** (resolved 2026-07-24).
   No dedicated admin Service, ever: the admin API answers per node (its
   config revision, its key set, its gossip view), so a load-balanced
   endpoint would answer from a different node per request; and it manages
   credentials, so it gets no stable routable exposure. It is reachable
   only per pod through the headless peers Service (bearer token required;
   NetworkPolicy-restrictable via `spec.networkPolicy`); humans use
   `kubectl port-forward` to a specific pod plus the
   `<cluster>-admin-token` Secret.
