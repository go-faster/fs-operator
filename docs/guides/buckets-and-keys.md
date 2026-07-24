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
  scheme: "ec:4,2"      # optional; empty follows the cluster default
  reclaimPolicy: Retain # Retain | Delete
```

The controller creates the S3 bucket in the referenced cluster using the
cluster's root credentials over its client Service. `bucketName` defaults to
`metadata.name` and is immutable, as is `clusterRef`.

**Scheme.** `spec.scheme` overrides the cluster's default replication scheme for
this bucket's objects — `rf2.5`, `rf3` or `ec:k,m` (e.g. `ec:4,2`). Leave it
empty to follow the cluster default. Changing it affects new writes cluster-wide
within seconds; existing objects convert through repair/rebalance. A scheme the
cluster cannot host (an unparseable value, or erasure coding that needs more
nodes than the cluster has) is refused: `Ready=False`, reason `SchemeRejected`.
`status.scheme` always reports the bucket's effective scheme.

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

Credentials live in the cluster's **etcd control plane** (`auth.source: etcd`),
sealed with a key derived from the cluster secret and hot-reloaded on every node
— so a credential is cluster-wide, survives restarts and is encrypted at rest.
The operator sets an `FSAccessKey` through the admin API: creating it, and
re-creating it when its grants change or an imported Secret rotates. A key is
`Ready` (reason `KeyAccepted`) once the cluster has accepted it;
`status.accessKey` shows the non-secret half for reference.

Deleting an `FSAccessKey` revokes the credential from the cluster (and
garbage-collects a generated Secret). Public-read buckets
(`FSCluster.spec.auth.publicReadBuckets`) are managed the same way — reconciled
into etcd through the admin API, not the config file.

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
