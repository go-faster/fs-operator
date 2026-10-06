# PLAN — current work plan

P1–P4 of [SPEC.md](SPEC.md) §16 shipped against fs v0.5–v0.13 and were
released through `v0.8.0`. The current work moves the operator to **fs
v0.14.0**; the record of what each earlier release shipped is the git
history and the release notes.

## Released

| Release | Scope |
|---|---|
| `v0.1.0` | P1 — provisioning, per-node configs and StatefulSets, conditions, rolling updates, owned chart, kind e2e. |
| `v0.2.0` | P2 — hot reload with revision verification, convergence-gated rollouts, PVC expansion, NetworkPolicy, PodMonitor. |
| `v0.3.0`–`v0.4.0` | P3 — FSBucket (with per-bucket scheme), FSAccessKey (generated or imported). |
| `v0.5.0`–`v0.8.0` | P4 — scale-down, admission webhook, operator metrics and dashboard, CRD-compat gate, air-gapped registry rewrite, single-node mode, SDK telemetry configuration. |

Those releases target fs v0.13 and earlier — etcd, disks and weights,
rebalancing, schema migrations — and none of that exists in fs v0.14.

## fs v0.14 — in progress

fs v0.14 replaced etcd, disks, weights, rebalancing and schema migrations
with one storage engine and a layout over peer nodes, and does not upgrade
v0.13 data or clusters in place. The operator follows it without a
compatibility layer (SPEC §17.1): a cluster built by `v0.8.0` or earlier is
recreated and its objects copied over.

Done (commit `9ec2798`, branch `fs-v0.14`):

- ✅ API: FSCluster loses `scheme`, `etcd`, `rebalance`, `integrity`,
  `updatePolicy.schemaMigration`, `storage.disks` and `storage.state`;
  `storage` is one volume per node; `layout.widths` is new; one node or at
  least three. Status gains `upNodes` and `layout`; `SchemaCurrent` and the
  etcd reasons are gone. FSBucket schemes are `rf3` or `ec:k,m`.
- ✅ The layout step (SPEC §8.4): the operator applies the layout of every
  declared node once each is up in the cluster's gossip view, never during
  a transition.
- ✅ Removal by layout transition: removed nodes leave the layout together
  and keep running until no retained version reads from them.
- ✅ Convergence and readiness from the layout and the gossip view.
- ✅ FSAccessKeys rendered into every node's config again (fs v0.14 keeps
  admin-created keys per node); Ready and revocation verified per node.
- ✅ `fsoperator_cluster_layout_retained_versions`; the `registered` node
  state became `up`.
- ✅ Gone: managed and external etcd, the FSCluster finalizer, disk drains
  and removal, schema migration Jobs, `internal/etcdstore`,
  `internal/scheme`.

Remaining before release:

- Examples, samples, guides, chart values and the kind e2e suite rewritten
  for the new API (the examples test now decodes strictly, so it fails
  until they are).
- Release `v0.9.0` pinned to fs `v0.14.0`, with upgrade notes: recreate
  clusters, and delete FSClusters before upgrading the operator — one that
  still carries the old etcd-cleanup finalizer is never released by the new
  operator.

## Next

- **Cluster-wide runtime keys upstream.** fs v0.14 stores admin-created keys
  per node, which is why credentials live in config Secrets (SPEC §7, §11).
  A replicated key store in fs would let FSAccessKeys go through the admin
  API instead.
- **Stuck transitions.** A dead node holds a layout transition open until a
  human runs `fs layout skip`. The operator reports the wait; naming the
  node that has not synced would make the fix obvious.
