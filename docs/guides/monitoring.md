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
| `SpecValid` | The spec passed cross-field validation. | The spec is coherent and applied. |
| `ReconcileSucceeded` | The last reconcile pass completed without error. | The pass finished cleanly. |
| `Ready` | The cluster serves S3 at write quorum. | A write quorum of failure domains is serving. |
| `NodesHealthy` | Every node's pod is up and current. | All nodes Ready. |
| `ClusterSizeAligned` | The running node set matches the topology. | Every declared node exists and runs its pod template. |
| `ConfigurationInSync` | Every node has applied the desired configuration. | Each node reports the target `config_revision`. |
| `Converged` | The cluster has settled. | Repair queue empty and no rebalance running. |
| `SchemaCurrent` | The cluster schema matches the deployed binary. | `schemaVersion.cluster == schemaVersion.binary`. |

### Condition reasons

The `reason` names the specific cause; these are stable.

With the [admission webhook](../install/helm.md#admission-webhook) enabled,
most of the `SpecValid` reasons below are reported by `kubectl apply` instead
— the object is never stored, so there is no condition to read. They still
appear here for clusters stored before the webhook was on, or while it is
off: the controller runs the same checks either way.

| Reason | On | Meaning |
|---|---|---|
| `SpecValid` / `SpecInvalid` | `SpecValid` | Spec passed / an unparseable value (e.g. bad scheme). |
| `SchemeTopologyMismatch` | `SpecValid` | The scheme needs more failure domains than the topology has. |
| `UnsupportedTopology` | `SpecValid` | Node count outside the 3–16 envelope; a [single-node](scaling.md#single-node) cluster declaring more than one disk, renaming it, or being grown into a cluster. Also the reason on the warning event a 1–2 node development cluster carries. |
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
| `DiskDraining` | Normal | A disk the spec dropped is being taken out of placement, or restored because it was declared again. See [storage.md](storage.md#removing-a-disk). |
| `DiskRemoved` | Normal | A drained disk holds no data; it is being removed from a node. |
| `ManagedEtcdUnsupported` | Warning | The cluster runs the operator-managed development etcd. Permanent, and repeated every reconcile. See [configuration.md](configuration.md#managed--development-only). |
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

Traces and logs go to a collector when one is configured:

```yaml
spec:
  observability:
    otlp:
      endpoint: http://otel-collector.observability:4317
      protocol: grpc   # or http/protobuf
```

Without an endpoint both exporters are explicitly `none`. That matters: fs runs
on [go-faster/sdk](https://github.com/go-faster/sdk), whose traces, logs **and**
metrics all default to OTLP at `localhost:4318`, so an unset exporter is a
failed upload logged every interval rather than silence.

### Per-signal exporters

Each signal picks its own exporter and, if it needs one, its own transport:

```yaml
spec:
  observability:
    otlp:
      endpoint: http://otel-collector.observability:4317
      protocol: grpc            # OTEL_EXPORTER_OTLP_PROTOCOL
    traces:
      exporter: otlp            # OTEL_TRACES_EXPORTER: otlp | none
    logs:
      exporter: none            # OTEL_LOGS_EXPORTER
    metrics:
      exporter: otlp            # OTEL_METRICS_EXPORTER: prometheus | otlp | none
      protocol: http/protobuf   # OTEL_EXPORTER_OTLP_METRICS_PROTOCOL
```

| Field | Default | Notes |
|---|---|---|
| `traces.exporter` | `otlp` with an endpoint, else `none` | |
| `logs.exporter` | `otlp` with an endpoint, else `none` | fs always logs to stdout; this ships a copy |
| `metrics.exporter` | `prometheus` | Kubernetes collects by scraping |
| `*.protocol` | `otlp.protocol` | per-signal override |

Two combinations are refused at apply time rather than left to fail quietly:
an `otlp` exporter with no `otlp.endpoint`, and `podMonitor: true` with a
`metrics.exporter` other than `prometheus` — nothing would be listening on the
port it scrapes.

`metrics.exporter` also decides the plumbing around the port: choose `otlp` or
`none` and the node stops advertising the metrics container port, and the
NetworkPolicy stops opening it.

**A transport subtlety the operator handles for you.** The SDK reads
`OTEL_EXPORTER_OTLP_PROTOCOL` *first* and consults the per-signal variables
only when it is unset — the reverse of the OpenTelemetry specification. So the
operator renders one or the other, never both: the shared variable when every
signal agrees, and only per-signal variables as soon as one of them differs.

### Configuring the rest of the SDK

```yaml
spec:
  observability:
    logLevel: info            # OTEL_LOG_LEVEL
    pprof: true               # PPROF_ADDR on :9010; false removes the
                              # listener, its container port and its
                              # NetworkPolicy rule
    resourceAttributes:       # added to OTEL_RESOURCE_ATTRIBUTES
      deployment.environment: staging
```

`resourceAttributes` are **added** to the ones the operator derives —
`fs.cluster`, `k8s.namespace.name`, `fs.node`, `fs.rack` — which is why they
have their own field: setting `OTEL_RESOURCE_ATTRIBUTES` directly replaces
those, and the dashboards and alerts here read them.

pprof is on by default, which is not the SDK's own default. A node worth
profiling is usually a node already misbehaving, and that is a bad moment to
discover the listener needs turning on and the pods restarting. It is
unauthenticated and a heap profile carries whatever is in memory — see
[security.md](security.md) for the exposure and how to close it.

Everything else in the SDK's
[environment reference](https://github.com/go-faster/sdk#reference) — Pyroscope,
propagators, export intervals, per-signal protocols, pprof routes — goes
through `podTemplate.extraEnv`, which the operator applies last, so it wins
over what the operator sets:

```yaml
spec:
  podTemplate:
    extraEnv:
      - name: PYROSCOPE_ENABLE
        value: "true"
      - name: PYROSCOPE_URL
        value: http://pyroscope.observability:4040
      - name: PYROSCOPE_PASSWORD
        valueFrom:
          secretKeyRef:
            name: pyroscope-auth
            key: password
      - name: OTEL_METRIC_EXPORT_INTERVAL
        value: "30000"
```

Overriding an operator-set variable this way is supported but on you: pointing
`METRICS_ADDR` elsewhere, for instance, leaves the `PodMonitor` scraping a port
nothing serves.

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
