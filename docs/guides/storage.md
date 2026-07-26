# Storage

Each entry in `spec.storage.disks` becomes one PersistentVolumeClaim on **every**
node, mounted at `/var/lib/fs/disks/<name>`. A three-node cluster with two disks
has six PVCs.

```yaml
spec:
  storage:
    disks:
      - name: d0
        size: 500Gi
        storageClass: fast
      - name: d1
        size: 500Gi
    reclaimPolicy: Retain
```

## Weights

`weight` is the disk's relative share of placement — set it proportional to
capacity when disks differ in size. Omitted means 1.

A weight that is **not positive drains the disk**: no new data is placed on it
and the auto-rebalancer moves what it holds elsewhere. Changing a weight is a
configuration change, so it rolls the cluster one node at a time.

To drain a disk *without* a roll, use fs's own control plane:
`fs cluster drain <node> <disk>`. That is what the operator does internally when
removing a disk (below), and it takes effect within seconds.

## Growing a disk

Raise `size`. The operator patches each node's PVC and then orphan-recreates the
node's StatefulSet — deleting it while leaving the pod and its data running — so
the recreated set carries the new claim template. One node at a time, and only
while the cluster is healthy and converged.

This needs `allowVolumeExpansion: true` on the StorageClass. Without it the PVC
patch is rejected and nothing else happens.

**Shrinking is refused** (`SpecValid=False`, `DiskShrinkForbidden`). Kubernetes
cannot shrink a PVC, and fs has no way to reclaim data off a smaller volume.

## Adding a disk

Append an entry. Each node gets a new PVC and is recreated with it, one node at
a time; the restarted node mounts and registers the disk, and weights drive data
onto it.

## Removing a disk

Drop the entry. This is a decommission, not a delete — the same discipline as
[removing a node](scaling.md#scale-down-decommission):

1. **Drain.** The disk is taken out of placement on *every* node at once, through
   fs's control plane rather than a config change, so no restart is needed and
   the rebalancer starts moving its data immediately.
2. **Wait until it is empty.** fs reports `has_data` per disk; the operator waits
   for every node's copy to report `false`.
3. **Remove.** Each node's StatefulSet is orphan-recreated without the disk, one
   node at a time. Its PVC then follows `reclaimPolicy`.

While this runs, `status.update.phase` is `Draining` and `ClusterSizeAligned` is
`False` with reason `Draining`, naming the disk and what it is waiting for.

**The disk stays mounted and configured throughout the drain.** That is not an
implementation detail: fs cannot move data off a volume the pod no longer has,
and dropping it from the node's config would strand the data on a disk nothing
reads. It leaves only once it is empty.

### What stops a removal

The same rule as a node decommission — every unknown resolves to *wait*:

| Gate | Why |
|---|---|
| Every node is ready and the cluster is converged | The rebalancer is what moves the data |
| Every node is reporting | A silent node makes the view partial, and a partial view is not evidence a disk is empty |
| Every node's copy reports `has_data: false` | The direct answer, from the node itself |

A cluster running fs older than **v0.10.0** reports no occupancy, so a removal
will drain and then wait indefinitely rather than delete on a signal that is not
there. Draining without a restart needs **v0.12.0**.

**Changing your mind works.** Put the entry back and the operator clears the
drain, returning the disk to its configured weight. It only clears overrides it
set itself, so a disk you drained by hand with `fs cluster drain` stays drained.

**The last disk cannot be removed** — a cluster with no disks has nowhere to put
anything, so the spec is refused outright rather than drained toward.

A disk is identified by its name, so **renaming one reads as removing a disk and
adding an empty one**: a correct, slow, and almost certainly unintended way to
spend a rebalance.

## Reclaim policy

`reclaimPolicy` decides what happens to a node's PVCs when the node or the disk
is removed, and when the cluster is deleted:

- **`Retain`** (default) keeps the volumes. Recovering data from a mistake is
  possible; cleaning up is manual.
- **`Delete`** removes them with their owner.

It is carried by each StatefulSet's claim retention policy, so it applies the
same way however the volumes come to be released.

## Related

- Growing or shrinking the *node count* is [scaling.md](scaling.md).
- The conditions and events are catalogued in [monitoring.md](monitoring.md).
- The etcd control plane, including the managed development one, is in
  [configuration.md](configuration.md#etcd).
