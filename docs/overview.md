# fs-operator

fs-operator is a Kubernetes operator that deploys and operates clustered
[go-faster/fs](https://github.com/go-faster/fs) — an S3-compatible object
store that replicates (`rf3`) or erasure-codes (`ec:k,m`) data over a layout of
peer nodes spread across failure domains.

The operator manages the full cluster lifecycle: provisioning (per-node
StatefulSets, volumes, services, secrets), topology (racks mapped to zones),
the layout (nodes joining and leaving, with fs moving the data), safe rolling
upgrades (one node at a time, gated on cluster reconvergence), and declarative
buckets and S3 credentials.

## Custom resources

| Kind | Purpose |
|---|---|
| `FSCluster` | A whole fs cluster: nodes, racks, storage, layout, auth, exposure, telemetry. |
| `FSBucket` | An S3 bucket in a referenced cluster. |
| `FSAccessKey` | One S3 credential with bucket grants, generated or imported. |

All three are namespaced; the namespace is the tenancy boundary.

## Installation

- [Helm](install/helm.md) — the primary method
- [kubectl / kustomize](install/kubectl.md)

## Guides

- [Configuration](guides/configuration.md) — every spec section
- [Scaling](guides/scaling.md) — the layout, scale-up, removing nodes, envelope limits
- [Upgrades](guides/upgrades.md) — rolling updates and rollback
- [Storage](guides/storage.md) — volume size, expansion, reclaim policy
- [Deletion](guides/deletion.md) — reclaim policy, re-creating a cluster
- [Buckets and access keys](guides/buckets-and-keys.md)
- [Monitoring](guides/monitoring.md) — metrics, conditions, events
- [Security](guides/security.md) — secrets, network policy, peer traffic

## Reference

- [API reference](reference/api.md) — generated from the Go types
- [Examples](../examples/) — a numbered gallery from minimal dev cluster to
  full production shape
