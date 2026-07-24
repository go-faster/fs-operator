# Upgrades

The operator rolls image and configuration changes across an `FSCluster` one
node at a time, gated on the cluster reconverging between nodes — go-faster/fs's
upgrade contract, encoded so you never take down a second failure domain while
the cluster is still repairing.

You change the spec; the operator does the choreography. There is no manual pod
deletion.

## Changing the image

Bump `spec.image.tag` to a new **pinned** fs release (never a floating tag —
upgrades are deliberate):

```sh
kubectl patch fscluster prod --type merge -p '{"spec":{"image":{"tag":"v0.6.1"}}}'
```

The operator then, for one node at a time:

1. **Preflight** — waits until every pod is Ready and the cluster is
   converged (repair queue empty, no rebalance running).
2. **Replace** — updates that node's StatefulSet; the StatefulSet controller
   replaces the pod. Racks are interleaved, so two nodes of one rack are never
   adjacent in the order.
3. **Gate** — waits for the new pod to become Ready, then for the cluster to
   reconverge, before moving to the next node — up to
   `spec.updatePolicy.convergenceTimeout` (default `30m`).

Progress shows on `status.update` (`phase`, the `node` being replaced) and the
conditions below. If the gate does not open within `convergenceTimeout`, the
rollout **halts** — it never touches a second node while the cluster is
unconverged — reports it (a `RolloutStuck` event, `Converged=False`), and
resumes automatically once the cluster reconverges.

### Watching a rollout

```sh
kubectl get fscluster prod -w
kubectl get events --field-selector involvedObject.name=prod --sort-by=.lastTimestamp
```

## Configuration changes

A configuration change is applied one of two ways, depending on what changed:

- **Hot reload — no restart.** Changes to credentials, grants, anonymously
  readable buckets or the TLS certificate are applied by bumping the node's
  config Secret and calling the admin reload endpoint. The operator embeds a
  revision marker in each config and reads it back (`config_revision`) to
  confirm every node has applied the change before `ConfigurationInSync` flips
  True. Kubelet Secret propagation lags (~1m), so this can take a minute.
- **Rolling restart.** Any other configuration change (the replication
  `scheme`, disk `weight`, rack membership, tuning that fs reads only at
  startup) needs a process restart, and rolls the cluster exactly as an image
  change does.

You do not choose which path applies — the operator decides from the diff.

## Schema migrations

A new fs release may implement a newer on-disk **schema** than the cluster has
recorded in etcd. fs's contract is that a schema migration runs only **after
every node is on the new binary** (an old binary refuses to join a
schema-migrated cluster).

So after an image rollout finishes and the cluster reconverges, the operator
compares the binary's schema to the cluster's:

- **`spec.updatePolicy.schemaMigration: Auto`** (default) — runs
  `fs cluster migrate` as a Job (`<cluster>-migrate-<n>`). The migration is
  etcd-elected and resumable, so the Job is safe to re-run. `status.update.phase`
  is `Migrating` while it runs; `SchemaCurrent` flips True only once the
  cluster schema actually catches up.
- **`spec.updatePolicy.schemaMigration: Manual`** — the operator only surfaces
  the pending migration (`SchemaCurrent=False`, reason `MigrationPending`). Run
  it yourself when ready:

  ```sh
  kubectl exec prod-0-0 -- fs cluster migrate --config /etc/fs/config.yaml
  ```

`status.schemaVersion` reports both versions:

```yaml
status:
  schemaVersion:
    cluster: 4   # recorded in etcd
    binary: 5    # implemented by the deployed image
```

## Rollback

Rollback is the same machinery in reverse: revert `spec.image.tag`, and the
operator rolls the cluster back node by node.

**One rule from fs:** you cannot roll a binary back **past a schema
migration**. A schema migration is a one-way step; an old binary refuses to
join a cluster whose schema it does not implement, and its pod will CrashLoop
with that explanation. Plan schema-bumping upgrades as commitments. See the
go-faster/fs `UPGRADE.md` for the schema compatibility rules.

## Relevant conditions

| Condition | During an upgrade |
|---|---|
| `Converged` | `False` while the repair queue is draining or a rebalance runs; the rollout gates on it. |
| `ConfigurationInSync` | `False` until every node has applied the target configuration revision. |
| `NodesHealthy` | `False` while a replaced pod is not yet Ready. |
| `SchemaCurrent` | `False` while a migration is pending or running. |
| `Ready` | Stays `True` as long as a write quorum of failure domains is serving — a correct one-at-a-time rollout does not drop it. |

See [monitoring.md](monitoring.md) for the full condition and event reference.
