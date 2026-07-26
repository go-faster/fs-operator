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
its HTTPS metrics service, plus the operator's own. The metrics service and an
optional operator `ServiceMonitor` ship with the
[chart](../install/helm.md).

Controller-runtime describes how the *reconciler* is behaving. These describe
the clusters it is reconciling:

| Metric | Labels | Meaning |
|---|---|---|
| `fsoperator_cluster_ready` | `namespace`, `cluster` | 1 when the cluster can serve writes, 0 otherwise — the `Ready` condition, alertable. |
| `fsoperator_cluster_nodes` | `namespace`, `cluster`, `state` | Node counts by `declared` (the topology asks for), `ready` (pod is up) and `registered` (present in the cluster's own etcd topology). |
| `fsoperator_update_phase` | `namespace`, `cluster`, `phase` | 1 for the phase a rolling change is in, 0 for the others. |
| `fsoperator_update_duration_seconds` | `namespace`, `cluster` | Histogram of *completed* rolling changes. |
| `fsoperator_reconcile_errors_total` | `controller` | Passes that ended in an error. |

Two things worth knowing about them:

- **Every series carries `namespace`.** One operator serves the whole cluster,
  so two namespaces may hold an `FSCluster` of the same name; a `cluster`-only
  label would merge them silently.
- **`update_phase` publishes 0 for the phases a cluster is not in**, rather
  than omitting them. An absent series and a false one look the same to an
  alert that has never seen the cluster before.
- **A deleted cluster stops reporting**, immediately. A gauge that outlives its
  object would report `ready=0` forever on a name nothing will ever reconcile
  again — indistinguishable from an outage that never resolves.

### Grafana dashboard

The chart ships a dashboard for these, as a ConfigMap the Grafana sidecar
discovers by label:

```sh
helm upgrade --install fs-operator oci://ghcr.io/go-faster/charts/fs-operator \
  --set prometheus.enabled=true \
  --set grafanaDashboard.enabled=true
```

The sidecar's label is configurable, since deployments differ on it —
kube-prometheus-stack watches `grafana_dashboard: "1"`, which is the default
here. Set `grafanaDashboard.labels` for anything else, and
`grafanaDashboard.namespace` when the sidecar only watches its own namespace.

It has three rows: cluster state (readiness, node counts, the ready-vs-declared
gap), rolling changes (the phase panel and the completed-duration quantiles),
and the operator itself (reconcile errors and latency). The raw JSON is
`dist/chart/dashboards/fs-operator.json` if you would rather import it by hand.

A rolling change that is *stuck* never reaches `update_duration_seconds`,
which only observes completed ones. Alert on `update_phase` staying at 1
instead:

```promql
# A rolling change in the same phase for over an hour.
max by (namespace, cluster, phase) (fsoperator_update_phase) == 1
```

## Suggested alerts

- `Ready=False` for more than a few minutes — the cluster is below write
  quorum.
- `Converged=False` (reason `ConvergenceTimeout`) — a rollout is stuck; check
  the fs pods.
- `SchemaCurrent=False` (reason `MigrationPending`) under the Manual policy —
  a migration is waiting for you.
- A rising fs repair-queue metric — nodes are failing or disks filling.
- `fsoperator_cluster_ready == 0` — the same signal as `Ready=False`, without
  needing to read conditions.
- `fsoperator_cluster_nodes{state="ready"} < fsoperator_cluster_nodes{state="declared"}`
  for long — a node is not coming back.
- `fsoperator_update_phase{phase="Draining"} == 1` for hours — a decommission
  cannot move a node's data off. See [scaling.md](scaling.md).
