# Deletion and re-creation

Deleting an `FSCluster` takes down everything the operator created for it.
Two things can outlive it, deliberately: the **PVCs** holding the data, and the
cluster's **keys in etcd**. Both are policy, and the defaults keep data.

## What happens on delete

Every resource the operator creates — Secrets, config Secrets, Services, the
StatefulSets, the PDB, the NetworkPolicy, the PodMonitor — carries an owner
reference, so Kubernetes garbage collection takes it down with the cluster. The
operator does not delete them one by one.

The PVCs follow `spec.storage.reclaimPolicy`, applied through each node's
StatefulSet claim retention policy:

| `reclaimPolicy` | On cluster delete |
|---|---|
| `Retain` (default) | PVCs are kept. A cluster re-created with the same name reuses them. |
| `Delete` | PVCs go with the cluster. |

## etcd keys

fs keeps its control plane in etcd under `spec.etcd.prefix`, which defaults to
`/fs/<namespace>/<name>`: the node registry, the agreed schema version, the
rebalance cursor and — since fs v0.8.0 — the **cluster-wide credential store**,
sealed with the cluster secret.

None of that is a Kubernetes object, so nothing garbage-collects it. It is kept
by default, because a shared etcd may hold neighbours and destroying their state
is worse than leaving yours:

```yaml
spec:
  etcd:
    external:
      endpoints: [http://etcd-0.etcd.fs-system:2379]
    cleanupOnDelete: true    # delete this cluster's keys with the cluster
```

With `cleanupOnDelete: true` the operator holds the object with the finalizer
`fs.go-faster.org/cluster` and, on delete:

1. deletes the node StatefulSets and waits for every node pod to be gone — a
   running node re-registers itself, so purging first would race the cluster it
   is purging;
2. deletes every key under `<prefix>/` — that trailing separator is what keeps
   a cluster named `app` from taking `app-staging`'s keys with it;
3. releases the object, and garbage collection takes the rest.

A cleanup that fails does **not** release the object: it retries, emitting the
`EtcdCleanupFailed` warning each pass, because leaving the keys behind is the
exact failure the cleanup exists to prevent. If the etcd endpoints are gone for
good, remove the finalizer by hand:

```sh
kubectl patch fscluster prod --type merge -p '{"metadata":{"finalizers":[]}}'
```

Clusters without `cleanupOnDelete` never carry the finalizer, so their deletion
can never be held up by an etcd the operator cannot reach.

## Re-creating a cluster with the same name

This is where the two policies meet, and it is worth being deliberate about.

A cluster's root credential is registered in etcd once — fs seeds the credential
store only while it is **empty**, then etcd is authoritative forever after. So a
cluster that starts on a prefix which already holds keys **adopts them** and
ignores its own root credential.

Delete a cluster without `cleanupOnDelete` and apply the same manifest again,
and that is precisely what happens: the prefix still holds the previous
incarnation's credentials, sealed with a cluster secret that has since been
regenerated. fs skips what it cannot unseal — one `warn` line per node — the
pods pass their probes, and nothing can authenticate.

The operator checks for this directly and reports it, rather than letting it
surface two indirections away as `FSBucket`s stuck on an unreachable cluster:

```
$ kubectl get fscluster prod -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
Ready=False  RootCredentialUnregistered
  The cluster's root credential is not registered in its key store: etcd prefix
  "/fs/fs-dev/prod" already held credentials when this cluster started, …
```

To recover, delete the stale keys and restart the nodes:

```sh
kubectl -n fs-system exec etcd-0 -- etcdctl del --prefix /fs/fs-dev/prod/
kubectl -n fs-dev delete pod -l fs.go-faster.org/cluster=prod
```

To avoid it, set `etcd.cleanupOnDelete: true` on clusters you expect to
re-create — development and CI clusters especially — so a deleted cluster takes
its keys with it.

Note that the operator only reports this state; it does not register the
credential itself. Writing a key into a prefix whose contents it did not create
would be repairing a cluster it cannot prove is its own, and a root key someone
removed deliberately wants the opposite treatment.

## Related

- Disk sizing and the reclaim policy in detail: [storage.md](storage.md).
- The conditions and events named here: [monitoring.md](monitoring.md).
- Bucket and access-key reclaim, which is per-object:
  [buckets-and-keys.md](buckets-and-keys.md).
