# Monitoring

An `FSCluster`'s state is exposed three ways: **conditions and status** on the
object (for humans and automation), **events** (for the story of what the
operator did), and **Prometheus metrics** (for dashboards and alerts). The
condition, reason and event vocabulary below is API surface — automations may
key off it.

## Status conditions

Read them with:

```sh
kubectl get fscluster prod -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

| Type | Meaning | `True` when |
|---|---|---|
| `SpecValid` | The spec passed controller-side cross-field validation. | The spec is coherent and applied. |
| `ReconcileSucceeded` | The last reconcile pass completed without error. | The pass finished cleanly. |
| `Ready` | The cluster serves S3 at write quorum. | A write quorum of failure domains is serving. |
| `NodesHealthy` | Every node's pod is up and current. | All nodes Ready. |
| `ClusterSizeAligned` | The running node set matches the topology. | Every declared node exists and runs its pod template. |
| `ConfigurationInSync` | Every node has applied the desired configuration. | Each node reports the target `config_revision`. |
| `Converged` | The cluster has settled. | Repair queue empty and no rebalance running. |
| `SchemaCurrent` | The cluster schema matches the deployed binary. | `schemaVersion.cluster == schemaVersion.binary`. |

### Condition reasons

The `reason` names the specific cause; these are stable.

| Reason | On | Meaning |
|---|---|---|
| `SpecValid` / `SpecInvalid` | `SpecValid` | Spec passed / an unparseable value (e.g. bad scheme). |
| `SchemeTopologyMismatch` | `SpecValid` | The scheme needs more failure domains than the topology has. |
| `UnsupportedTopology` | `SpecValid` | Node count outside the 3–16 envelope. |
| `DiskShrinkForbidden` | `SpecValid` | A disk would shrink; disks may only grow. |
| `SecretNotFound` / `SecretInvalid` | `SpecValid` | A referenced Secret is missing or lacks the expected keys. |
| `ReconcileFinished` / `ReconcileError` | `ReconcileSucceeded` | The pass finished / the pass failed (message carries the error). |
| `QuorumAvailable` / `QuorumUnavailable` | `Ready` | Enough / not enough failure domains are serving for a write. |
| `RootCredentialUnregistered` | `Ready` | Quorum is serving, but the cluster's key store does not hold the root credential — the cluster started on an etcd prefix left behind by a previous incarnation. See [deletion.md](deletion.md). |
| `AllNodesReady` / `NodesNotReady` | `NodesHealthy` | All / not all node pods are Ready. |
| `UpToDate` | `ClusterSizeAligned`, `ConfigurationInSync` | Everything matches the spec. |
| `ScalingUp` | `ClusterSizeAligned` | New nodes are joining. |
| `Draining` | `ClusterSizeAligned` | A node is being decommissioned; the message says what the drain is still waiting for. See [scaling.md](scaling.md). |
| `StorageExpanding` | `ClusterSizeAligned` | A node's storage is being grown. |
| `RollingNodes` | `ClusterSizeAligned` | A pod-template rollout is in flight. |
| `ConfigReloadPending` | `ConfigurationInSync` | Some node has not applied the target configuration yet. |
| `Converged` | `Converged` | Repair queue empty, placement settled. |
| `RepairQueueBacklog` | `Converged` | Repair tasks are pending. |
| `Rebalancing` | `Converged` | A rebalance is moving data. |
| `ConvergenceTimeout` | `Converged` | A rollout has been stuck past `convergenceTimeout`. |
| `MigrationPending` / `MigrationRunning` | `SchemaCurrent` | A schema migration is waiting to run / running. |

## Events

Events narrate the day-2 machinery. `kubectl describe fscluster prod`, or:

```sh
kubectl get events --field-selector involvedObject.name=prod --sort-by=.lastTimestamp
```

| Reason | Type | Emitted when |
|---|---|---|
| `NodesCreating` | Normal | New nodes are being created (scale-up). |
| `NodeRolling` | Normal | A node is being replaced during a rollout. |
| `RolloutWaiting` | Normal | The rollout is holding for a pod or reconvergence. |
| `RolloutStuck` | Warning | The rollout has held past `convergenceTimeout`. |
| `RolloutComplete` | Normal | Every node runs the desired configuration. |
| `ConfigReloaded` | Normal | A node hot-reloaded to a new configuration. |
| `ReloadFailed` | Warning | A node's reload failed. |
| `StorageExpanding` | Normal | A node's PVCs are being grown and its StatefulSet recreated. |
| `NodeDraining` | Normal | A decommission started: the node is being taken out of placement. |
| `NodeDrained` | Normal | A decommissioning node reports no data left; it is being removed. |
| `NodeRemoved` | Normal | A decommissioned node's StatefulSet and config were deleted. |
| `MigrationFailed` | Warning | The schema migration Job failed. |
| `SchemeTopologyMismatch`, `UnsupportedTopology`, … | Warning | A spec was refused (same names as the condition reasons). |
| `PodMonitorUnavailable` | Warning | `observability.podMonitor` is set but the Prometheus-operator CRDs are absent. |
| `RootCredentialUnregistered` | Warning | The cluster's key store does not hold its root credential. |
| `EtcdCleanup` | Normal | A deleted cluster's nodes are being stopped before its etcd keys go. |
| `EtcdCleanupComplete` | Normal | A deleted cluster's etcd keys were removed. |
| `EtcdCleanupFailed` | Warning | The etcd cleanup failed; the cluster is held until it succeeds. |

## Metrics

### fs cluster metrics

Each fs pod exports Prometheus metrics (disk fullness, placement skew, repair
queue, rebalance/scrub counters, request metrics) on port `9464`. To scrape
them with the Prometheus operator, set:

```yaml
spec:
  observability:
    podMonitor: true
```

The operator creates a `PodMonitor` selecting the cluster's pods — but only when
the `monitoring.coreos.com` CRDs are installed. Without them, the field is a
warning (`PodMonitorUnavailable` event), not an error.

Traces and OTLP metrics go to a collector when one is configured:

```yaml
spec:
  observability:
    otlp:
      endpoint: http://otel-collector.observability:4317
      protocol: grpc   # or http/protobuf
```

Without an OTLP endpoint the SDK does not export (no failed-export noise);
Prometheus scraping still works.

### Operator metrics

The controller-manager serves controller-runtime's standard metrics
(reconcile counts and latencies, work-queue depth, per-controller errors) on
its HTTPS metrics service — enough to alert on reconcile errors or a backed-up
queue. The metrics service and an optional operator `ServiceMonitor` ship with
the [chart](../install/helm.md).

## Suggested alerts

- `Ready=False` for more than a few minutes — the cluster is below write
  quorum.
- `Converged=False` (reason `ConvergenceTimeout`) — a rollout is stuck; check
  the fs pods.
- `SchemaCurrent=False` (reason `MigrationPending`) under the Manual policy —
  a migration is waiting for you.
- A rising fs repair-queue metric — nodes are failing or disks filling.
