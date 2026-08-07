# Scaling

Cluster membership is a deliberate, capacity-planned operation — there is no
autoscaling for the data plane, by design. You change the topology in the spec;
the operator adds or (in a later release) drains nodes.

## The envelope

go-faster/fs supports **3–16 nodes**. The replication scheme sets a floor on
**distinct failure domains**:

| Scheme | Minimum failure domains |
|---|---|
| `rf2.5`, `rf3` | 3 |
| `ec:k,m` | k + m |

A failure domain is a rack, or — in the flat topology — a single node. The
operator refuses a spec whose topology cannot host its scheme
(`SpecValid=False`, reason `SchemeTopologyMismatch`) and one outside the 3–16
envelope (`UnsupportedTopology`) without mutating the running cluster. A cluster
of 2 nodes is admitted for development only, with a warning event.

## Single node

`topology.nodes: 1` is a different thing from a small cluster. No scheme can be
placed on one node — every one of them needs three distinct disks, or k+m — so
that node runs **fs's non-clustered filesystem backend** instead:

```yaml
spec:
  topology:
    nodes: 1
  storage:
    disks:
      - name: d0
        size: 5Gi
  # no etcd
```

- **Exactly one disk**, and it is the storage root. More is refused
  (`UnsupportedTopology`); renaming it later is refused too, because the node
  has no cluster to drain the data into.
- **No `spec.etcd`.** There is no control plane to register in, so declaring one
  is refused rather than quietly ignored.
- **`spec.scheme` is ignored**, along with rebalancing, repair, scrub-driven
  convergence and schema migration: there is one copy of every object on one
  volume. `Converged` is trivially `True`, `SchemaCurrent` is not reported, and
  `Ready` is `True` once the only node is serving.
- **Per-bucket schemes are unavailable**: an `FSBucket` with `spec.scheme` on a
  single-node cluster is `Ready=False`, reason `SchemeRejected`. Access keys and
  public-read buckets work as usual.

What you give up is everything the failure domains were for: one copy of every
object on one machine, no repair, no failure tolerance. Losing the node loses the
data. And it **cannot be grown into a cluster in place** — the two backends store
data differently, so raising `nodes` is refused; create a new `FSCluster` and copy
the objects over. [`examples/00-single-node.yaml`](../../examples/00-single-node.yaml)
is a complete one, and the operator warns about the shape at apply time and on
the object.

## Scale up

Raise `spec.topology.nodes`, or a rack's `nodes`, or add a rack:

```sh
kubectl patch fscluster prod --type merge -p '{"spec":{"topology":{"nodes":5}}}'
```

Joining is **additive**: the new nodes' Secrets and StatefulSets are created,
they register in etcd, and the auto-rebalancer moves data onto them. Several new
nodes may join at once — only removals are serialized. `ClusterSizeAligned` is
`False` (reason `ScalingUp`) until every declared node is running.

For a **racked** topology, add a rack or grow a rack's node count:

```yaml
spec:
  topology:
    racks:
      - {name: a, nodes: 2, zone: eu-central-1a}
      - {name: b, nodes: 2, zone: eu-central-1b}
      - {name: c, nodes: 2, zone: eu-central-1c}   # new rack
```

Rack names are immutable per entry, and each node is pinned to its rack's zone
or node selector — placement is never inferred from where a pod happened to
land.

## Scale down (decommission)

Lower `spec.topology.nodes`, or a rack's `nodes`, or remove a rack. The operator
decommissions the nodes it no longer finds declared — **one at a time**, highest
index first within the affected rack:

1. **Drain.** The node's config is re-rendered with every disk out of placement
   and the node is restarted onto it (the same one-at-a-time machinery as an
   upgrade). It keeps running and serving; it just stops taking new data, and
   the auto-rebalancer begins moving what it holds onto the remaining nodes.
2. **Wait until it is empty.** fs reports `has_data` per disk, and the operator
   waits for every one of the node's disks to report `false`.
3. **Remove.** Its StatefulSet and config Secret are deleted. Its PVCs follow
   `spec.storage.reclaimPolicy` (`Retain` by default) through the StatefulSet's
   claim retention policy.
4. Wait for the cluster to reconverge, then start the next node.

While this runs, `status.update.phase` is `Draining` with the node's name, and
`ClusterSizeAligned` is `False` with reason `Draining` and a message saying what
it is still waiting for — which disks still hold data, and how much.

Killing a node without draining leaves the cluster to repair from surviving
copies (allowed, but degraded), and taking a second domain down before the first
has reconverged can make erasure-coded objects unrecoverable. That is why
removals are serialized while joins are not.

### What stops a removal

A node is deleted only when *every* one of these holds. Any of them unknown
means the operator waits — indefinitely, if that is what it takes:

| Gate | Why |
|---|---|
| The node is running the drained config | Until it restarts, its disks still take writes |
| Every node is ready and the cluster is converged | The rebalancer is what moves the data, and it cannot finish while the cluster is unsettled |
| Every node is reporting | A silent node makes the cluster's view partial, and a partial view is not evidence a disk is empty |
| Every disk reports `has_data: false` | The direct answer, from the node itself |

Note the last two. fs omits `has_data` when a node did not report or could not
read a disk — absent means **unknown**, never drained. A cluster running fs
older than **v0.10.0** reports no occupancy at all, so a decommission on it will
drain the node and then wait forever rather than delete on a signal that is not
there. Upgrade the cluster first.

Occupancy is not inferred from capacity, and neither should you: `total_bytes` /
`free_bytes` come from `statfs`, so they describe the filesystem. A disk holding
no fragments still reports bytes in use. The byte figures in the drain message
are progress, not the test.

### A stalled drain

If the rebalancer cannot place the data — no room on the remaining nodes, or a
topology that cannot host the scheme without this node — the drain never
finishes and the operator keeps waiting, reporting `Converged=False` with reason
`ConvergenceTimeout` past `spec.updatePolicy.convergenceTimeout`. Nothing is
forced and nothing is deleted. Restore the node count to undo it: a node that is
declared again stops draining and returns to placement.

## Total nodes may never drop below the scheme minimum

A spec whose remaining node count would fall below the scheme's domain
requirement is refused outright (`SpecValid=False`, reason
`SchemeTopologyMismatch`) — no node is drained on the way to a cluster that
cannot host its own data. Scaling all the way down to 1 is refused for a
different reason (`UnsupportedTopology`): that is a different storage backend,
not a smaller cluster, and the data does not come with it.

## Related

- Growing a node's disks (not the node count) is [storage.md](storage.md).
- The one-at-a-time rollout machinery that scaling shares is
  [upgrades.md](upgrades.md).
- The conditions and events are catalogued in [monitoring.md](monitoring.md).
