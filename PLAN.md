# PLAN — current work plan

Working plan for finishing **P1 (core)** of [SPEC.md](SPEC.md) §16. Updated
as steps land; later phases (P2–P4) stay in the spec until they are next.

## Status

Done:

- API types (`FSCluster`, `FSBucket`, `FSAccessKey`) with CEL validation,
  conditions and defaults (SPEC §5–7)
- Owned Helm chart in `dist/chart` + CRD sync (`hack/sync-chart-crds.sh`,
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

## Remaining P1 — build order

1. **Resource builders** — generated Secrets (cluster secret, admin token,
   root credentials), per-node config Secrets, per-node single-pod
   StatefulSets (probes, env, disk PVCs, rack affinity + anti-affinity),
   peers (headless) + client Services, PDB maxUnavailable=1 (SPEC §8.1).
2. **Step-pipeline reconciler** — `internal/controller/step.go`, then the
   FSCluster controller wiring builders through server-side apply:
   conditions (SpecValid, NodesHealthy, ClusterSizeAligned,
   ConfigurationInSync, Ready, …), status revisions, controller-side
   cross-field validation (scheme vs failure domains), scale-up path,
   scale-down refusal (`ScaleDownRequiresDrain`) (SPEC §8).
3. **Basic rolling logic** — sequential per-node StatefulSet updates gated
   on pod-ready. Repair-queue/convergence gates and hot reload arrive in
   P2 with the upstream fs endpoints (SPEC §8.2, §11).
4. **Tests + first release** — envtest coverage for 1–3 (idempotency,
   ownership/GC, refusal paths); kind e2e: operator via `dist/chart`, a
   minimal 3-pod etcd, 3-node FSCluster from `examples/01-minimal.yaml`,
   S3 smoke against real fs v0.5.0. Then tag `v0.1.0` to exercise the
   release pipeline end to end.

## Parallel track — upstream go-faster/fs (unblocks P2)

In `/src/faster/fs`, per SPEC §11 (sequencing: 1, 2, 7 first):

- `POST /api/v1/reload` — admin reload endpoint (SIGHUP equivalent)
- `GET /api/v1/cluster/status` — schema versions, config revision,
  occupancy, convergence, repair queue
- Export the admin API client as a public package

## Definition of done for P1

`kubectl apply -f examples/01-minimal.yaml` on a kind cluster with etcd
produces a running 3-node fs cluster serving S3; `make test`, `make lint`,
`helm lint` clean; e2e green in CI; `v0.1.0` released.
