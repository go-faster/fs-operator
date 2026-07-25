# fs-operator

fs-operator is a Kubernetes operator that deploys and operates clustered
[go-faster/fs](https://github.com/go-faster/fs) — an S3-compatible object
store with quorum replication (`rf2.5`, `rf3`, `ec:k,m`), failure-domain-aware
placement, automatic rebalancing and scrub/repair.

The operator manages the full cluster lifecycle: provisioning (per-node
StatefulSets, disks, services, secrets), topology (racks mapped to zones),
safe rolling upgrades (one node at a time, gated on cluster reconvergence),
schema migrations, scaling, and declarative buckets and S3 credentials.

## Custom resources

| Kind | Purpose |
|---|---|
| `FSCluster` | A whole fs cluster: nodes, racks, disks, etcd, auth, exposure, tuning. |
| `FSBucket` | An S3 bucket in a referenced cluster. |
| `FSAccessKey` | One S3 credential with bucket grants, generated or imported. |

All three are namespaced; the namespace is the tenancy boundary.

## Installation

- [Helm](install/helm.md) — the primary method
- [kubectl / kustomize](install/kubectl.md)

## Guides

- [Configuration](guides/configuration.md) — every spec section
- [Scaling](guides/scaling.md) — scale-up, decommission, envelope limits
- [Upgrades](guides/upgrades.md) — rolling updates, schema migration, rollback
- [Storage](guides/storage.md) — disks, weights, expansion, reclaim policy
- [Deletion](guides/deletion.md) — reclaim policy, etcd cleanup, re-creating a cluster
- [Buckets and access keys](guides/buckets-and-keys.md)
- [Monitoring](guides/monitoring.md) — metrics, conditions, events
- [Security](guides/security.md) — secrets, network policy, peer traffic

## Reference

- [API reference](reference/api.md) — generated from the Go types
- [Examples](../examples/) — a numbered gallery from minimal dev cluster to
  full production shape
