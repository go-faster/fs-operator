# Configuration

<!-- TODO: fill in the remaining sections per SPEC §13 — one per spec field
group. `etcd` is written; the rest still defer to the API reference. -->

This guide covers each `FSCluster` spec section: `image`, `scheme`, `topology`
(flat nodes vs racks, anti-affinity), `storage` (disks, weights, reclaim),
`etcd`, `clusterSecretRef`, `auth`, `s3` (service, TLS), `rebalance`,
`integrity`, `updatePolicy`, `observability`, `networkPolicy`, `podTemplate`.

For the sections not yet written, the [API reference](../reference/api.md) and
[examples](../../examples/) are the source of truth.

## etcd

fs keeps its control plane in etcd: the node registry, the rebalance and
migrate cursors, the schema version, and — since fs v0.8.0 — the cluster-wide
credential store, sealed with the cluster secret. It is small (kilobytes) and
absolutely load-bearing: lose it and you lose the cluster, not just its
metadata.

Exactly one of `external` or `managed` must be set.

### `external` — the production mode

An etcd you operate:

```yaml
spec:
  etcd:
    external:
      endpoints:
        - https://etcd-0.example:2379
        - https://etcd-1.example:2379
        - https://etcd-2.example:2379
```

Give it the same availability care as any etcd: an odd number of members
(3 is typical), fast disks, and backups. The endpoints are passed to every fs
node, and the client fails over between them.

> TLS and authentication to etcd are not configurable yet: fs's own etcd config
> carries only endpoints, prefix and TTL (SPEC §11.4). Until that lands
> upstream, etcd must be reachable without client certificates.

### `managed` — development only

The operator runs a small etcd for the cluster:

```yaml
spec:
  etcd:
    managed: {}          # one member, pinned etcd, a 2Gi volume
```

This exists so an example applies cleanly on a laptop. It is **not** a
production offering and will not become one:

- **no backups** and no restore path;
- **no defrag automation**;
- **no member replacement** — `replicas` is immutable, because bootstrapping is
  a static member list and growing it would need a join dance the operator does
  not implement;
- **no TLS**, same as external.

Its volume holds the control plane *and* the sealed credential store, so losing
it loses the cluster. Every cluster using it gets a `ManagedEtcdUnsupported`
warning event on every reconcile, and the same sentence in its status. That
warning is permanent — etcd lifecycle management is its own discipline, and
this is deliberately not it (SPEC §2).

Knobs, all optional:

```yaml
spec:
  etcd:
    managed:
      replicas: 3        # 1 (default) or 3; immutable
      image:
        repository: quay.io/coreos/etcd
        tag: v3.5.17
      storage:
        size: 2Gi
        storageClass: fast
        reclaimPolicy: Delete
      resources: {}
```

`reclaimPolicy` defaults to **`Delete`**, unlike the data disks, which default
to `Retain`. That is deliberate: a re-created cluster that adopted a stale key
store would inherit credentials sealed with a cluster secret it no longer has,
and would come up looking healthy while nothing could authenticate — see
[deletion.md](deletion.md). Keeping a dev etcd's volume across a delete buys
you that failure and nothing else.

`etcd.cleanupOnDelete` has no effect in managed mode, and the operator does not
add its finalizer: the etcd is owned by the cluster, so garbage collection takes
it and its volumes down. There is nothing left to purge, and holding the delete
open on an etcd that is going away with it would only stall.

The example is
[`examples/08-managed-etcd.yaml`](../../examples/08-managed-etcd.yaml).

### `prefix` and `ttl`

`prefix` namespaces the cluster's keys, defaulting to
`/fs/<namespace>/<name>`. It is immutable — changing it would move the cluster's
keys out from under it. `ttl` is the node registration lease: how long a dead
node lingers in the topology before its registration expires.
