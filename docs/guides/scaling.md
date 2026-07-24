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
of 1–2 nodes is admitted for development only, with a warning event.

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

Removing a node means **draining it first** — moving its data elsewhere — then
removing it. Killing a node without draining leaves the cluster to repair from
surviving copies (allowed, but degraded), and taking a second domain down
before the first has reconverged can make erasure-coded objects unrecoverable.

Draining requires per-node occupancy the operator cannot observe until the
upstream fs cluster-status endpoint reports it (go-faster/fs SPEC §11.2). Until
then, **scale-down is refused**: a spec that removes a node reports
`SpecValid=False`, reason `ScaleDownRequiresDrain`, and the running cluster is
left exactly as it was. This is intentional — the operator will not silently
delete a data-bearing node.

The full drain-then-remove flow (`spec.topology` decrease → weight-0 drain →
wait drained → remove, one node at a time, highest index first) arrives in a
later phase with the upstream observability.

## Total nodes may never drop below the scheme minimum

Even once decommission ships, a spec whose remaining node count would fall
below the scheme's domain requirement is refused outright.

## Related

- Growing a node's disks (not the node count) is [storage.md](storage.md).
- The one-at-a-time rollout machinery that scaling shares is
  [upgrades.md](upgrades.md).
- The conditions and events are catalogued in [monitoring.md](monitoring.md).
