# Configuration

<!-- TODO: prose sections per SPEC §13 for the field groups not covered here.
`etcd` is written; the rest are covered field-by-field in the generated API
reference and by topic in the guides linked below. -->

The `FSCluster` spec has these sections. Every field of every one is described
in the **[API reference](../reference/api.md)**, which is generated from the
types themselves and therefore cannot fall behind them.

| Section | Where it is explained |
|---|---|
| `topology` (flat nodes, racks, anti-affinity) | [scaling.md](scaling.md) |
| `storage` (disks, weights, expansion, reclaim) | [storage.md](storage.md) |
| `image`, `updatePolicy` | [upgrades.md](upgrades.md) |
| `etcd` | below |
| `clusterSecretRef`, `auth`, `s3.tls`, `networkPolicy` | [security.md](security.md) |
| `observability` | [monitoring.md](monitoring.md) |
| `scheme`, `rebalance`, `integrity`, `s3.service`, `podTemplate` | the API reference |

The [examples](../../examples/) are working shapes for the common cases. Every
one is validated against a live API server on each run — schema, CEL rules and
the admission webhook — so an example the operator would now reject cannot sit
in the gallery unnoticed.

## etcd

fs keeps its control plane in etcd: the node registry, the rebalance and
migrate cursors, the schema version, and — since fs v0.8.0 — the cluster-wide
credential store, sealed with the cluster secret. It is small (kilobytes) and
absolutely load-bearing: lose it and you lose the cluster, not just its
metadata.

Exactly one of `external` or `managed` must be set — unless the cluster is a
[single node](scaling.md#single-node), which has no control plane and must set
neither.

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

#### TLS

etcd holds the node registry and the cluster's credential store, sealed with
the cluster secret — anything that can write to it can reshape the cluster.
Secure the path to it on anything but a trusted network:

```yaml
spec:
  etcd:
    external:
      endpoints:
        - https://etcd-0.example:2379
      tls:
        # A Secret with ca.crt, and optionally tls.crt/tls.key for mutual TLS.
        secretName: etcd-client-tls
        # serverName: etcd.internal      # when the address is not on the cert
        # insecureSkipVerify: false      # development only
```

The Secret is mounted read-only into every node and the paths are rendered
into its config. `ca.crt` is required; add `tls.crt` and `tls.key` — **both or
neither** — for mutual TLS. A Secret missing `ca.crt`, or carrying one half of
the pair, is refused with `SpecValid=False` (`SecretInvalid`) rather than
producing nodes that will not start.

An `https://` endpoint enables TLS **on its own**, verifying against the system
roots, so `tls` is only needed for a private CA, a client certificate, or a
name override. That default exists because fs takes the transport from its
config and not from the URL: an https endpoint with no TLS block would
otherwise connect in the clear to a port expecting TLS.

`insecureSkipVerify` makes TLS decorative — anything on the path can
impersonate the cluster's control plane — and is for development against
self-signed certificates only.

#### Authentication

```yaml
spec:
  etcd:
    external:
      endpoints: ["https://etcd-0.example:2379"]
      # A Secret with keys "username" and "password".
      authSecretRef:
        name: etcd-credentials
```

The credentials reach the nodes as `FS_ETCD_USERNAME` / `FS_ETCD_PASSWORD`,
never through the rendered configuration. That is deliberate: a config Secret
is readable by anything that can read Secrets in the namespace, and a password
in it would also be baked into every config-revision fingerprint. Both keys are
required.

The operator uses the same Secrets for its own connection when it purges a
deleted cluster's keys (`etcd.cleanupOnDelete`), so there is one place to
configure and no second, quietly-plaintext path.

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
