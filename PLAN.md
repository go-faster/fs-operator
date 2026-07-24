# PLAN — current work plan

**P1 (core) of [SPEC.md](SPEC.md) §16 is complete and released as `v0.1.0`.**
This file now tracks the P2 day-2 work and the upstream fs endpoints it is
gated on; later phases (P3–P4) stay in the spec until they are next.

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

## Parallel track — upstream go-faster/fs (unblocks P2)

In `/src/faster/fs`, per SPEC §11 (sequencing: 1, 2, 7 first):

- `POST /api/v1/reload` — admin reload endpoint (SIGHUP equivalent)
- `GET /api/v1/cluster/status` — schema versions, config revision,
  occupancy, convergence, repair queue
- Export the admin API client as a public package

Observed during e2e (fs-side, not operator): in cluster mode a **multipart**
part upload returns 500 (`mc pipe` reproduces it) while a single-shot PUT/GET
round-trips fine. The operator e2e uses a single PUT, so it is unaffected;
worth filing upstream.

## Definition of done for P1

`kubectl apply -f examples/01-minimal.yaml` on a kind cluster with etcd
produces a running 3-node fs cluster serving S3; `make test`, `make lint`,
`helm lint` clean; e2e green in CI; `v0.1.0` released.
