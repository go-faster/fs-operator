# Storage

Every node gets **one** PersistentVolumeClaim, `data`, mounted at fs's storage
root (`/var/lib/fs`). It holds everything the node keeps: the metadata tables,
the data blocks and the layout it has adopted. A three-node cluster has three
PVCs.

```yaml
spec:
  storage:
    size: 500Gi
    storageClass: fast
    reclaimPolicy: Retain
```

- **`size`** is the volume each node requests, and also the node's **capacity
  in the layout**: fs gives each node a share of the partitions in proportion
  to it. Every node of a cluster has the same size.
- **`storageClass`** selects the class; empty uses the cluster default.
  Metadata is written on every request, so this is worth putting on the fastest
  class you have.
- **`reclaimPolicy`** — see [below](#reclaim-policy).

## Growing the storage

Raise `size`. The operator patches each node's PVC and then orphan-recreates
the node's StatefulSet — deleting it while leaving the pod and its data running
— so the recreated set carries the new claim template. One node at a time, and
only while the cluster is healthy and converged. The layout follows: each
node's capacity is raised to the new size in one layout change.

This needs `allowVolumeExpansion: true` on the StorageClass. Without it the PVC
patch is rejected and nothing else happens.

**Shrinking is refused** (`SpecValid=False`, `StorageShrinkForbidden`):
Kubernetes cannot shrink a PVC. To get a smaller cluster, create one and copy
the objects over.

## Reclaim policy

`reclaimPolicy` decides what happens to a node's PVC when the node is removed,
and to every PVC when the cluster is deleted:

- **`Retain`** (default) keeps the volumes. Recovering data from a mistake is
  possible; cleaning up is manual.
- **`Delete`** removes them with their owner.

It is carried by each StatefulSet's claim retention policy, so it applies the
same way however the volumes come to be released.

## Related

- Changing the *node count* is [scaling.md](scaling.md).
- Erasure coding — storing a bucket at 1.5× instead of 3× — is a per-bucket
  scheme plus a layout width: [buckets-and-keys.md](buckets-and-keys.md#schemes).
- The conditions and events are catalogued in [monitoring.md](monitoring.md).
