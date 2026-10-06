# fs-operator

Kubernetes operator for [go-faster/fs](https://github.com/go-faster/fs) —
an S3-compatible object store whose nodes share a layout spreading every
partition over failure domains, with metadata replicated at quorum and
object data replicated (`rf3`) or erasure coded (`ec:k,m`) per bucket.
Pinned to fs `v0.14.0`.

The operator manages the full lifecycle of fs clusters through three
namespaced custom resources:

| Kind | Purpose |
|---|---|
| `FSCluster` | A whole fs cluster: nodes, racks and zones, storage, layout, auth, exposure, telemetry. |
| `FSBucket` | An S3 bucket in a referenced cluster. |
| `FSAccessKey` | One S3 credential with bucket grants, generated or imported. |

It provisions per-node StatefulSets with one PVC-backed data volume each, maps
fs zones and racks (failure domains) onto Kubernetes zones, owns the cluster
layout, and encodes fs's operational contracts as controller logic: rolling
updates one node at a time gated on cluster reconvergence, new nodes joining
the layout once they are up, and removed nodes kept running until the layout
transition has moved their data.

See [SPEC.md](SPEC.md) for the full design and [docs/](docs/overview.md) for
installation and guides.

## Quick start

```sh
# Install the operator (CRDs included).
helm install fs-operator oci://ghcr.io/go-faster/charts/fs-operator \
  --namespace fs-operator-system --create-namespace

# Create a single-node dev cluster (one pod, one volume).
kubectl apply -f examples/00-single-node.yaml
```

A one-node cluster has no peers and no replication: one copy of every
object, development only, and it cannot be grown into a cluster in place.
The [examples/](examples/) gallery goes from there to a zonal production
shape.

## Development

Standard kubebuilder workflow:

```sh
make manifests generate   # regenerate CRDs and deepcopy after API changes
make helm-sync-crds       # keep the owned chart's CRDs in lockstep
make test                 # unit + envtest
make lint                 # golangci-lint
```

The Helm chart in `dist/chart` is committed and hand-owned; its CRD
templates and the manager RBAC are synced from `config/` by `hack/sync-chart.sh` and CI
fails on drift.

## License

Apache-2.0. Copyright 2026.
