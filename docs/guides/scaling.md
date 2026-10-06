# Scaling

Cluster membership is a deliberate, capacity-planned operation — there is no
autoscaling for the data plane, by design. You change the topology in the spec;
the operator changes the cluster **layout** to match, and fs moves the data.

## The layout

fs splits the data into partitions and assigns each to nodes — three of them for
replicated data and metadata, k+m for an erasure-coded bucket. That assignment
is the layout, and the operator owns it: every declared node is a member, with
its rack and its rack's zone as failure domains and `storage.size` as its
capacity. fs spreads each partition over zones first, then racks, then nodes.

`spec.layout.widths` lists the widths to spread for. It defaults to `[3]`; a
cluster with an `ec:4,2` bucket needs `[3, 6]`. Each width needs at least that
many nodes.

A layout change is a **transition**: fs moves the data to where the new version
puts it while the previous version keeps serving, and writes go to both. When
every node has synced the new version the old one is retired.
`status.layout.retainedVersions` lists the versions still in transition; it is
empty when no data is moving.

The operator applies a layout only when every declared node is **up** in the
cluster's own gossip view — so no partition is handed to a node nobody can
reach — and never while a previous change is still in transition.

## The envelope

go-faster/fs supports **1 node, or 3–16 nodes**. Two is refused by the API: a
cluster keeps three copies of its data on distinct nodes. A width wider than the
node count is refused (`SpecValid=False`, reason `LayoutTopologyMismatch`), and
one wider than the distinct failure domains is admitted with a warning — some
partitions then keep two slots in one domain, so losing that domain costs both.

## Single node

`topology.nodes: 1` is the development shape:

```yaml
spec:
  topology:
    nodes: 1
  storage:
    size: 5Gi
```

It has no peers and no layout to apply: one copy of every object on one volume,
no repair, no failure tolerance. Losing the node loses the data. `Converged` is
trivially `True`, and `Ready` is `True` once the node is serving. Per-bucket
erasure coding is unavailable: an `FSBucket` with an `ec:k,m` scheme is
`Ready=False`, reason `SchemeRejected`.

It **cannot be grown into a cluster in place** — raising `nodes` is refused;
create a new `FSCluster` and copy the objects over.
[`examples/00-single-node.yaml`](../../examples/00-single-node.yaml) is a
complete one, and the operator warns about the shape at apply time.

## Scale up

Raise `spec.topology.nodes`, or a rack's `nodes`, or add a rack:

```sh
kubectl patch fscluster prod --type merge -p '{"spec":{"topology":{"nodes":5}}}'
```

The new nodes' Secrets and StatefulSets are created, they join through their
peers, and once every one is up the operator applies a layout that includes
them; fs moves their share onto them. `ClusterSizeAligned` is `False` (reason
`ScalingUp`, then `LayoutPending`) until it has.

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

## Scale down

Lower `spec.topology.nodes`, or a rack's `nodes`, or remove a rack. Every node
the spec no longer declares leaves in **one** layout change:

1. **Leave the layout.** The operator applies a layout without them. fs starts
   moving their data to the nodes that remain; the previous version, which
   still includes them, keeps serving reads meanwhile.
2. **Keep running.** They stay exactly where they are until the transition
   completes — `status.layout.retainedVersions` empty — because until then fs
   still reads from them.
3. **Remove.** Their StatefulSets and config Secrets are deleted. Their PVCs
   follow `spec.storage.reclaimPolicy` (`Retain` by default).

While this runs, `status.update.phase` is `Draining` and `ClusterSizeAligned`
is `False` with reason `Draining` and a message saying what it waits for.
Nothing acknowledged is lost on the way: that is what the transition is for.

### A stalled transition

A transition completes when every node that holds data has synced it. A node
that is gone for good — its volume lost, its machine gone — never does, and the
transition (and any removal waiting on it) stalls, reporting `Converged=False`,
reason `LayoutTransition`, and `ConvergenceTimeout` past
`spec.updatePolicy.convergenceTimeout`. Nothing is forced and nothing is
deleted.

If the node will not come back, release it from any other node:

```sh
kubectl exec prod-1-0 -c fs -- fs layout skip prod-3
```

Whatever only that node held is given up, so use it only for a node that is not
coming back. The admin API equivalent is
`POST /api/v1/cluster/nodes/{id}/skip`.

## Related

- Growing each node's volume (not the node count) is [storage.md](storage.md).
- The one-at-a-time rollout machinery is [upgrades.md](upgrades.md).
- The conditions and events are catalogued in [monitoring.md](monitoring.md).
