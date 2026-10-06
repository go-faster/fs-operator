# Upgrades

The operator rolls image and configuration changes across an `FSCluster` one
node at a time, gated on the cluster reconverging between nodes — go-faster/fs's
upgrade contract, encoded so you never take down a second failure domain while
the cluster is still moving data.

You change the spec; the operator does the choreography. There is no manual pod
deletion.

## Changing the image

Bump `spec.image.tag` to a new **pinned** fs release (never a floating tag —
upgrades are deliberate):

```sh
kubectl patch fscluster prod --type merge -p '{"spec":{"image":{"tag":"v0.14.1"}}}'
```

The operator then, for one node at a time:

1. **Preflight** — waits until every pod is Ready and the cluster is
   converged: every node up on the current layout, and no layout change in
   transition.
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

- **Hot reload — no restart.** Changes to credentials (FSAccessKeys), grants,
  anonymously readable buckets or the TLS certificate are applied by bumping the node's
  config Secret and calling the admin reload endpoint. The operator embeds a
  revision marker in each config and reads it back (`config_revision`) to
  confirm every node has applied the change before `ConfigurationInSync` flips
  True. Kubelet Secret propagation lags (~1m), so this can take a minute.
- **Rolling restart.** Any other configuration change (turning TLS on or off,
  telemetry switches, anything fs reads only at startup) needs a process
  restart, and rolls the cluster exactly as an image change does. The peer
  list is the exception: it changes whenever the cluster is scaled, and fs
  reads it only to join, so it never restarts a node.

You do not choose which path applies — the operator decides from the diff.

## Upgrading from fs v0.13 (operator v0.8)

fs v0.14 replaced its storage and cluster model — etcd, disks and weights,
rebalancing and schema migrations are gone, replaced by one storage engine and
a layout over peer nodes. It does not read data written by v0.13, and this
operator cannot run a v0.13 cluster. There is no in-place upgrade:

1. With the old operator still installed, create a **new** `FSCluster` from this
   operator's examples alongside the old one — or in another Kubernetes
   cluster if the CRDs cannot coexist — and copy the objects over with any S3
   client:

   ```sh
   mc mirror old/bucket new/bucket        # or: aws s3 sync, rclone sync
   ```

2. Delete the old `FSCluster`s, then upgrade the operator and its CRDs. The
   CRD changes are breaking: an object written for the old schema (with
   `etcd`, `disks` or `scheme`) does not apply to the new one.

## Rollback

Rollback is the same machinery in reverse: revert `spec.image.tag`, and the
operator rolls the cluster back node by node.

**One rule from fs:** every node of a cluster runs the same release, and a
release that changes its on-disk format says so in its notes — a rollback past
one is a rebuild, not a rollout. Read the fs release notes before an upgrade.

## Relevant conditions

| Condition | During an upgrade |
|---|---|
| `Converged` | `False` while a node is down or behind the current layout, or a layout change is in transition; the rollout gates on it. |
| `ConfigurationInSync` | `False` until every node has applied the target configuration revision. |
| `NodesHealthy` | `False` while a replaced pod is not yet Ready. |
| `Ready` | Stays `True` as long as all but one failure domain are serving — a correct one-at-a-time rollout does not drop it. |

See [monitoring.md](monitoring.md) for the full condition and event reference.
