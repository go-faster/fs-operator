# Deletion and re-creation

Deleting an `FSCluster` takes down everything the operator created for it. One
thing can outlive it, deliberately: the **PVCs** holding the data. That is
policy, and the default keeps data.

## What happens on delete

Every resource the operator creates — Secrets, config Secrets, Services, the
StatefulSets, the PDB, the NetworkPolicy, the PodMonitor — carries an owner
reference, so Kubernetes garbage collection takes it down with the cluster. The
operator does not delete them one by one, and the cluster carries no finalizer:
nothing it owns lives outside Kubernetes.

The PVCs follow `spec.storage.reclaimPolicy`, applied through each node's
StatefulSet claim retention policy:

| `reclaimPolicy` | On cluster delete |
|---|---|
| `Retain` (default) | PVCs are kept. A cluster re-created with the same name and topology reuses them. |
| `Delete` | PVCs go with the cluster. |

## Re-creating a cluster with the same name

Under `Retain`, a cluster re-created with the same name finds its nodes'
volumes again: each node comes back with the layout and data it had. The cluster
secret is generated afresh unless you referenced your own
(`spec.clusterSecretRef`), which is fine — it authenticates peer traffic, not
data at rest.

If you wanted an empty cluster, delete the PVCs (`data-<node>-0`) first, or use
`reclaimPolicy: Delete`.

## Related

- Volume sizing and the reclaim policy in detail: [storage.md](storage.md).
- The conditions and events: [monitoring.md](monitoring.md).
- Bucket and access-key reclaim, which is per-object:
  [buckets-and-keys.md](buckets-and-keys.md).
