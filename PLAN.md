# PLAN — current work plan

P1–P3 of [SPEC.md](SPEC.md) §16 are complete, released through `v0.5.0`.
This file tracks **P4 (lifecycle)** and the upstream fs changes it is gated
on; the finished phases are kept below as the record of what shipped.

Landed since `v0.5.0`, not yet released: the FSCluster deletion finalizer and
etcd cleanup (issue #4, below) and image pinning by digest.

## P1 — done (shipped in v0.1.0)

- API types (`FSCluster`, `FSBucket`, `FSAccessKey`) with CEL validation,
  conditions and defaults (SPEC §5–7)
- Owned Helm chart in `dist/chart` + CRD & RBAC sync (`hack/sync-chart.sh`,
  `make helm-sync-crds`)
- Docs skeleton, examples 01–03, valid samples
- Release CI: image → `ghcr.io/go-faster/fs-operator`, chart →
  `oci://ghcr.io/go-faster/charts/fs-operator`, keyed by `v*` tags
- Default fs image pinned to `v0.5.0`; dependency advisories cleared
- **Config renderer** — `internal/fsconfig` (schema mirror + fs's startup
  checks) and `internal/controller/fscluster` (node expansion, names,
  per-node `config.yaml` renderer, configuration revision). Golden tests
  per topology shape; every rendered config is validated the way fs
  validates it at startup
- **Resource builders** — generated Secrets (`internal/keygen` mints them),
  per-node config Secrets, per-node single-pod StatefulSets (probes, env,
  disk PVCs, rack affinity + anti-affinity, unprivileged pods), peers
  (headless) + client Services, PDB maxUnavailable=1 (SPEC §8.1)
- **Step pipeline + FSCluster controller** — `internal/controller/step.go`
  and `internal/controller/fscluster`: server-side apply of every resource,
  create-once secret material, cross-field validation (scheme vs failure
  domains, node envelope, scale-down refusal, missing referenced Secrets),
  status revisions and conditions (SpecValid, ReconcileSucceeded,
  NodesHealthy, ClusterSizeAligned, ConfigurationInSync, Ready by write
  quorum). envtest coverage incl. the CRD's own validation rules
- **Rolling updates** — per-node pod-template fingerprint on the
  StatefulSet, one node replaced per pass, racks interleaved, gated on the
  node's own StatefulSet reporting its pod up and current; new nodes are
  created all at once (joining is additive). Repair-queue/convergence
  gates and hot reload arrive in P2 with the upstream fs endpoints
  (SPEC §8.2, §11)

- **kind e2e** — `test/e2e`: operator installed from `dist/chart`, a 3-pod
  etcd, the `examples/01-minimal.yaml` cluster, S3 bucket + put/get through
  the generated root credentials against real fs v0.5.0, and the
  scale-down refusal. Caught a real bug: the owned chart's manager RBAC had
  drifted from `config/rbac`, so the operator started but its `Owns()`
  informers could not list StatefulSets/Secrets/Services and it silently
  never reconciled. `hack/sync-chart.sh` now syncs the manager role too,
  and the sync + fs-pin checks run on every PR (were release-only)
- **fs version command** — `make fs-version FS_VERSION=vX.Y.Z` (and
  `hack/set-fs-version.sh`) rewrites the pin everywhere it is spelled out —
  API default, CRD, chart, examples, e2e, docs — then regenerates;
  `make check-fs-version` fails on drift

- **First release** — `v0.1.0` tagged and published: multi-arch operator
  image (`linux/amd64` + `linux/arm64`) to
  `ghcr.io/go-faster/fs-operator:v0.1.0` and the chart to
  `oci://ghcr.io/go-faster/charts/fs-operator` (chart `0.1.0`, appVersion
  `v0.1.0`). All CI green on the released commit: Release, Tests, Lint,
  Test Chart, E2E

**P1 (core) is complete.** Next focus is P2 (day-2), which is gated on the
upstream fs endpoints below.

## Upstream go-faster/fs — done, shipped in fs v0.6.0

Per SPEC §11 (items 1, 2, 7 gate P2). All merged (fs PRs #90/#91) and
released as **fs v0.6.0**, which the operator is now pinned to:

- ✅ `POST /api/v1/reload` — reloads credentials/TLS like SIGHUP, returns
  what it reloaded + the config revision now in effect
- ✅ Config revision echo — a top-level `revision` config marker fs reports
  via `GET /api/v1/info` (`config_revision`) and the reload result (§8.3)
- ✅ `GET /api/v1/cluster/status` — schema versions, per-node/-disk capacity,
  placement skew, rebalance state. Aggregate repair-queue depth and per-node
  object count still deferred upstream; interim is the per-node rebalance
  endpoint's `repair_queue_depth`
- ✅ Importable admin client — `github.com/go-faster/fs/adminapi`
- ✅ Headless `fs admin` service — a control-plane-only admin the operator
  can run as its own Deployment + Service for cluster status / rebalance
  control (§11, §4.2). Reload + credential ops stay per-node

Observed during e2e (fs-side, not operator): in cluster mode a **multipart**
part upload returns 500 (`mc pipe` reproduces it) while a single-shot PUT/GET
round-trips fine. The operator e2e uses a single PUT, so it is unaffected;
worth filing upstream.

## P2 (day-2) — build order

Now unblocked by fs v0.6.0. Sequenced so each piece builds on the last:

1. ✅ **fs admin client** — `internal/fsclient`: a thin wrapper over
   `github.com/go-faster/fs/adminapi` (info + config-revision, reload,
   cluster status, rebalance) returning plain structs, with a connection
   pool keyed by endpoint + token; `fscluster.AdminURL` dials the per-pod
   admin over the headless Service (SPEC §4.2, §12).
2. ✅ **Hot reload with revision verification** — the config carries an
   opaque `revision` marker fs echoes; `RestartRevision` excludes the
   hot-reloadable auth section (and the marker), so a credentials/public-
   read/TLS-only change bumps the config Secret and reloads in place instead
   of rolling. The reload step POSTs `/api/v1/reload` per serving node and
   polls `config_revision` until every node reports the target;
   `ConfigurationInSync` reflects what nodes have actually applied (SPEC
   §8.3).
3. ✅ **Convergence-gated rollouts** — a convergence step reads
   `cluster/status` (rebalance running?) and the per-node rebalance endpoint
   (repair-queue depth, summed); the rollout will not replace another node
   until the queue is empty and no rebalance is moving data, and holds
   past `convergenceTimeout`. `Converged` condition (SPEC §8.2).
4. ✅ **Schema migration Job** — the convergence step also reads schema
   versions; once every node is config-current and converged and the binary
   schema is ahead, the Auto policy runs `fs cluster migrate` as a
   create-once Job keyed by target schema. `SchemaCurrent` condition, the
   `Migrating` phase, and `status.schemaVersion`; Manual only surfaces the
   pending migration (SPEC §8.2).
5. **Day-2 extras** — in progress:
   - ✅ **NetworkPolicy** (§9) — opt-in `spec.networkPolicy`, restricts
     peer/admin ports to cluster pods + operator namespace (POD_NAMESPACE)
   - ✅ **PVC expansion** (§8.5) — grow disk PVCs + orphan-recreate the
     StatefulSet (one node at a time, gated); disk-shrink refused
   - ✅ **PodMonitor** — optional `spec.observability.podMonitor`, created
     when `monitoring.coreos.com` is discoverable; unstructured, no
     prometheus-operator dependency
   - ✅ **Docs guides** — upgrades, scaling, monitoring written from the code
     (§13)
   - ✅ Release **v0.2.0**

**P2 (day-2) is complete and released as `v0.2.0` (2026-07-24).** Shipped:
fsclient, hot reload with revision verification, convergence-gated rollouts,
schema migration Job, NetworkPolicy, PVC expansion, PodMonitor, day-2 guides.
Multi-arch operator image `ghcr.io/go-faster/fs-operator:v0.2.0`
(`linux/amd64` + `linux/arm64`) and chart
`oci://ghcr.io/go-faster/charts/fs-operator` (chart `0.2.0`, appVersion
`v0.2.0`). All CI green on the released commit `004c0e6`; Release workflow
published both artifacts.

## P3 (tenancy) — complete, released as `v0.3.0` (2026-07-24)

Delivered SPEC §6/§7 tenancy on top of fs **v0.7.0** (which added the §11.3
bucket-scheme admin endpoints — go-faster/fs PR #95):

- ✅ **FSAccessKey controller** — generated credential minted once into an
  owned Secret, or imported via `existingSecretRef` (weak-key refusal, external
  rotation propagated by a hot reload); Ready verified via the admin API's
  `listAccessKeys`. Stamps a credential fingerprint that drives the cluster.
- ✅ **fscluster render merge** — every FSAccessKey of a cluster rendered into
  each node's config as a declarative `auth.keys` entry, applied and verified by
  the existing hot-reload machinery; the cluster watches FSAccessKeys.
- ✅ **FSBucket controller** — S3 `CreateBucket` via minio over the client
  Service + root credentials, per-bucket `scheme` override through the new admin
  endpoint (`status.scheme` = effective), reclaim policy (Retain / Delete with
  non-empty refusal). `FSBucket.spec.scheme` added.
- ✅ **fsclient** — `ListAccessKeys`, `Get/SetBucketScheme` with typed errors
  (`ErrSchemeRejected`, `ErrBucketNotFound`).
- ✅ **Refactor** — step-pipeline framework moved to
  `internal/controller/pipeline` to break the import cycle the tenancy
  controllers would create.
- ✅ **Examples 04–07** and the buckets-and-keys guide; RBAC/CRDs/chart synced.
- ✅ **e2e** — kind bucket + generated access-key round-trip (rf3 override,
  put/get with the minted credential).
- ✅ Release **v0.3.0**.

**P3 (tenancy) is complete and released as `v0.3.0` (2026-07-24).** Multi-arch
image `ghcr.io/go-faster/fs-operator:v0.3.0` and chart
`oci://ghcr.io/go-faster/charts/fs-operator` (chart `0.3.0`, appVersion
`v0.3.0`); operator pinned to fs `v0.7.0`.

## Post-P3: cluster-wide runtime credentials — released as `v0.4.0` (2026-07-24)

Incorporated fs **v0.8.0** (PR #96, DESIGN §6.8): the runtime key store is now
cluster-wide in etcd, sealed by the cluster secret and hot-reloaded on every
node. This removed the reason v0.1–0.3 rendered credentials into config
(node-local runtime keys), so the credential path was reworked:

- ✅ Config renders `auth.source: etcd` and no keys/public-read; the root
  credential seeds etcd via the `FS_ROOT_*` env on first boot.
- ✅ **FSAccessKey controller** now manages credentials through the admin API
  (`CreateAccessKey`/`DeleteAccessKey`), re-creating on grant change or
  imported-Secret rotation (material fingerprint) and revoking on delete.
- ✅ **Public-read** (`spec.auth.publicReadBuckets`) reconciled via
  `GET`/`PUT /api/v1/public-read-buckets`; the FSAccessKey render-merge, its
  watch and RBAC removed.
- ✅ `fsclient` gained `CreateAccessKey`/`DeleteAccessKey`/`Get`+`SetPublicReadBuckets`
  and grants on `ListAccessKeys`. Golden configs, SPEC §7, the guide, RBAC and
  tests updated; e2e green (etcd-backed key round-trip).
- ✅ Release **v0.4.0**; operator pinned to fs `v0.8.0`. All CI green on
  `00add8b`.

## Post-P3: deletion and re-creation (issue #4)

`spec.etcd.cleanupOnDelete` was declared in the CRD but nothing read it, and
the SPEC §8.6 finalizer did not exist — so a cluster deleted and re-created
under the same name adopted the previous incarnation's etcd keys, whose sealed
credentials it could no longer open. It went `Ready` while every FSBucket hung
on `cluster S3 not reachable yet`.

- ✅ **Finalizer** `fs.go-faster.org/cluster`, carried only by clusters with
  `etcd.cleanupOnDelete`, so an ordinary delete is never held up by an
  unreachable etcd. On delete it stops the node StatefulSets and the migration
  Job, waits for their pods (a running node re-registers itself), deletes every
  key under `<prefix>/` — the trailing separator keeps `app` from taking
  `app-staging`'s keys — then releases. A failed purge retries rather than
  leaking the keys.
- ✅ **`internal/etcdstore`** — the operator's own etcd client, refusing an
  empty or root prefix.
- ✅ **Adopted-prefix detection** — the root credential is checked against the
  cluster's key store; a mismatch is `Ready=False/RootCredentialUnregistered`
  naming the prefix and the fix, instead of a `warn` line in a node's log. An
  empty key store is treated as unknown (a starting cluster looks like that).
- ✅ **`docs/guides/deletion.md`**, monitoring catalogue, SPEC §8.6.

## P4 (lifecycle) — in progress

SPEC §16: decommission/drain, disk add/remove, etcd TLS, managed dev-grade
etcd, admission webhook, Grafana dashboards, CRD-compat CI gate.

Three of these were gated on upstream fs. Two upstream releases were cut for
this phase — v0.9.0 (live node state) and v0.10.0 (per-disk occupancy, fs PR
#102, written here) — which between them settled the §11.2 gap entirely:

- ✅ **Aggregate repair-queue depth + per-node live state** (fs #97) — the
  §11.2 gap the convergence gate was working around. `GET
  /api/v1/cluster/status` now carries `repair_queue_depth`,
  `nodes_reporting`/`nodes_not_reporting` and a per-node `live` block, and
  a node that does not answer carries `live_error` rather than zeroed
  counters. Also lands `GET/POST /api/v1/cluster/migrate`, and fixes the
  cluster-mode multipart 500 this repo recorded during P2 e2e.
- ✅ **Per-disk occupancy** (§11.2 remainder, fs v0.10.0) — `has_data` per
  disk in `GET /api/v1/cluster/status`: false once the disk holds no
  fragments. A boolean rather than the object count originally sketched —
  fs keeps no index, so counting means walking the tree, while the status
  path is contracted cheap; the boolean is exact *and* constant-time on a
  drained disk, the case a drain polls hardest. `data_error` carries a disk
  the node could not probe, and both absences mean unknown, never drained.
- ❌ **etcd client TLS/auth** (§11.4) — `EtcdConfig` carries only
  `endpoints`/`prefix`/`ttl`; both `clientv3.New` call sites pass no TLS and
  no credentials. Blocks `etcd.external.tlsSecretName`.
- ❌ **Hot drain / weight override** (§11.6) — no admin endpoint, no CLI, no
  etcd key. Weight reaches etcd only inside the node's own registration
  record, republished every 30s, so an external writer is overwritten.
  Draining still means: render a negative weight, restart the node.

Upstream bug found on the way, **documented** in fs v0.10.0 and still to be
fixed with §11.6: fs substitutes `1` for a disk weight of `0` (`weight == 0`
reads as "unset"), while four places in its own tree — the config struct
comment, `config.yaml`, the admin OpenAPI description and SIZING.md —
documented `0` as draining. So the documented drain was unreachable, and a
user who wrote `weight: 0` got **full** weight. All four now say a negative
weight drains, which is why `DrainWeight = -1.0`; making `0` itself work
needs a pointer field to tell an absent key from an explicit zero.

### Build order

1. ✅ **Pin fs v0.9.0** — `make fs-version`, and surface the new status in
   `internal/fsclient`: aggregate repair queue, the reporting split,
   per-node disks with capacity and weight, `Drained()`/`UsedBytes()`.
2. ✅ **Convergence reads the aggregate** — with the per-node fan-out kept as
   the fallback, because a cluster on a pre-v0.9.0 image serves no live
   state and its zeroed aggregate would otherwise read as an empty queue
   mid-upgrade. `NodesReporting` tells "nobody answered" from "nothing to
   do".
3. ✅ **Per-disk occupancy upstream** — fs PR #102, released as v0.10.0, and
   pinned here. Closes the §11.2 remainder so the drain gate is exact rather
   than inferred from bytes.
4. ✅ **Decommission** (§8.4) — drain-then-remove, one node at a time, highest
   index first within the affected rack. The node stays in the pass while it
   drains (so the health, rollout and PDB gates keep counting it) and keeps
   the StatefulSet it is running, restamped onto the drained config rather
   than rebuilt from a spec that no longer describes where it runs. Every
   gate resolves unknown to *wait*, so a pre-v0.10.0 cluster drains and then
   holds rather than deleting on a signal that is not there.
5. **Disk add** (§8.5), and removal once §11.6 lands.
6. ✅ **Admission webhook** — cross-field checks move to `internal/validation`
   and run in two places: the validating webhook at apply time (so `kubectl
   apply` reports the problem and nothing is stored), and the controller
   before it acts, because a webhook can be disabled, unreachable, or absent
   when an object was stored. One implementation, two callers. Checks that
   read cluster state (a referenced Secret, a live StatefulSet's disks) stay
   controller-only: the admission path must not call the API server. Opt-in
   (`webhook.enabled` + a certificate), `failurePolicy: Fail`. The scheme
   parser moved to `internal/scheme` so the shared validation had somewhere
   neutral to live.
7. ✅ **Operator metrics** (§10) **+ Grafana dashboard** — `internal/metrics`
   publishes the five `fsoperator_*` series §10 has declared since P1 and
   none of which existed. Every series carries `namespace` as well as
   `cluster` (one operator, many namespaces, same cluster name);
   `update_phase` publishes 0 for the phases a cluster is not in rather than
   omitting them; and a deleted cluster is forgotten immediately, since a
   gauge that outlives its object reports `ready=0` forever on a name nothing
   will reconcile again. The chart ships a dashboard as a sidecar-discovered
   ConfigMap (`grafanaDashboard.enabled`).
8. ✅ **CRD-compat CI gate** — `hack/check-crd-compat.py` diffs the generated
   CRDs against the last release tag and fails on anything that breaks
   objects already stored (removed field or version, changed type, newly
   required field, dropped enum value, tightened bound/pattern, new CEL
   rule, changed default). Additive changes pass. One exemption:
   `spec.image.tag`'s default, which is meant to move every release.
9. ✅ **Managed dev-grade etcd** — `spec.etcd.managed`: a StatefulSet with a
   static bootstrap list, one PVC per member, and nothing else. Development
   only and built to stay that way (SPEC §2): `replicas` 1 or 3 and
   immutable, `PodManagementPolicy: Parallel` (OrderedReady deadlocks a fresh
   three-member etcd), volumes reclaimed on delete by default, a warning
   event every reconcile, no finalizer. The etcd objects use a distinct app
   name so they match none of the selectors the client Service, PDB,
   NetworkPolicy and PodMonitor use — a test guards it, having caught exactly
   that.
10. **etcd TLS** — still blocked on fs §11.4: `EtcdConfig` upstream carries
    only endpoints/prefix/ttl and neither `clientv3.New` call site passes TLS
    or credentials. Needs an upstream PR like #102.

## Definition of done for P1 (met — v0.1.0)

`kubectl apply -f examples/01-minimal.yaml` on a kind cluster with etcd
produces a running 3-node fs cluster serving S3; `make test`, `make lint`,
`helm lint` clean; e2e green in CI; `v0.1.0` released.
