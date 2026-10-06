# Buckets and access keys

An `FSCluster` serves S3; `FSBucket` and `FSAccessKey` are how you carve it up.
Both reference a cluster by name in the **same namespace** — the namespace is
the tenancy boundary, and cross-namespace references are not supported.

A complete example is [`examples/06-buckets-and-keys.yaml`](../../examples/06-buckets-and-keys.yaml).

## Buckets

```yaml
apiVersion: fs.go-faster.org/v1alpha1
kind: FSBucket
metadata:
  name: media
spec:
  clusterRef:
    name: fs-dev
  scheme: "ec:4,2"      # optional: rf3 | ec:k,m; empty leaves it alone
  reclaimPolicy: Retain # Retain | Delete
```

The controller creates the S3 bucket in the referenced cluster using the
cluster's root credentials over its client Service. `bucketName` defaults to
`metadata.name` and is immutable, as is `clusterRef`.

<a id="schemes"></a>**Scheme.** `spec.scheme` is how the bucket's data is
stored: `rf3` (three replicas, the default for a new bucket) or `ec:k,m`
(erasure coded, e.g. `ec:4,2`: 4 data + 2 parity shards on 6 nodes, 1.5×
instead of 3×). Leave it empty to leave the bucket's scheme as it is. A change
applies to data written from then on; existing data keeps the scheme it was
written with. Blocks under 256 KiB and small inline objects stay replicated.

An erasure scheme needs the cluster laid out for its width — `k+m` in the
`FSCluster`'s `spec.layout.widths`, with at least that many nodes:

```yaml
kind: FSCluster
spec:
  topology:
    nodes: 6
  layout:
    widths: [3, 6]
```

A scheme the cluster cannot host — no such width, or a single-node cluster —
is refused: `Ready=False`, reason `SchemeRejected`, with fs's reason in the
message. `status.scheme` reports the bucket's scheme as fs has it. See
[`examples/04-erasure-coding.yaml`](../../examples/04-erasure-coding.yaml).

**Reclaim policy.** On delete:

- `Retain` (default) drops the finalizer and leaves the bucket and its data
  untouched.
- `Delete` removes the S3 bucket. This succeeds only once the bucket is empty;
  while it still holds objects the controller keeps the finalizer, reports
  `Ready=False` / `BucketNotEmpty` and retries. There is no force-wipe in
  v1alpha1 — empty the bucket (or switch to `Retain`) to let the delete finish.

## Access keys

An `FSAccessKey` is one S3 credential with bucket grants. The credential comes
from exactly one of two sources.

### Generated (default)

```yaml
apiVersion: fs.go-faster.org/v1alpha1
kind: FSAccessKey
metadata:
  name: app-writer
spec:
  clusterRef:
    name: fs-dev
  grants:
    - bucket: "media"     # glob, fs grant semantics
      permission: write   # read | write | admin
```

The operator mints the access/secret pair once (`crypto/rand`) and writes it to
an owned Secret named `<metadata.name>-credentials`, with keys `access-key`,
`secret-key` and `endpoint`. The Secret is created once and never rewritten —
rotating a credential breaks whoever holds it — and is garbage-collected when
the `FSAccessKey` is deleted. Point your application at that Secret.

### Imported

```yaml
spec:
  clusterRef:
    name: fs-dev
  existingSecretRef:
    name: vault-minted-s3-creds   # keys: access-key, secret-key
  grants:
    - bucket: "media"
      permission: read
```

Use `existingSecretRef` when the credential is managed elsewhere — Vault,
ExternalSecrets, a sealed Secret. The operator reads the access/secret from that
user-managed Secret, renders it into the cluster and never writes back to it. It
watches the Secret, so an external **rotation propagates automatically** via a
hot reload. `secret-key` must be at least 16 characters, or the key is refused
(`Ready=False`, reason `WeakSecretKey`). `secretName` and `existingSecretRef`
are mutually exclusive.

### How keys reach the cluster

The `FSCluster` controller renders every `FSAccessKey` of the cluster, with its
grants, into **every node's** configuration and hot-reloads the nodes — no
restart. A grant change or an imported Secret's rotation is the same re-render
and reload. A node that starts later reads the same configuration, so every
node accepts the same keys.

A key is `Ready` (reason `KeyAccepted`) once **every node** lists it; until then
it is `ConfigReloadPending`, naming the nodes still missing it.
`status.accessKey` shows the non-secret half for reference.

Deleting an `FSAccessKey` revokes it: the key is dropped from the rendered
configuration and the nodes reload, and the object goes only once no node that
answers still accepts it (a node that is down reads the new configuration when
it starts). A generated Secret is then garbage-collected. Public-read buckets
(`FSCluster.spec.auth.publicReadBuckets`) are rendered and reloaded the same
way.

Keys created directly through fs's admin API (`POST /api/v1/access-keys`) are
not this: fs keeps those on the node that received the request only. Manage
credentials with `FSAccessKey`.

## Status at a glance

Both kinds carry a `Ready` condition and print columns for it:

```console
$ kubectl get fsbuckets,fsaccesskeys
NAME                          READY   CLUSTER   AGE
fsbucket.../media             True    fs-dev    2m

NAME                              READY   CLUSTER   ACCESSKEY        AGE
fsaccesskey.../app-writer         True    fs-dev    AKprod4f2…       2m
```

`kubectl describe` the resource for the reason and message when `Ready` is
`False` (`ClusterNotFound`, `ClusterNotReady`, `BucketNotEmpty`,
`SchemeRejected`, `WeakSecretKey`, `ConfigReloadPending`).
