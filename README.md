# fs-operator

Kubernetes operator for clustered [go-faster/fs](https://github.com/go-faster/fs)
— an S3-compatible object store with quorum replication (`rf2.5`, `rf3`,
`ec:k,m`), failure-domain-aware placement, automatic rebalancing and
scrub/repair.

The operator manages the full lifecycle of fs clusters through three
namespaced custom resources:

| Kind | Purpose |
|---|---|
| `FSCluster` | A whole fs cluster: nodes, racks, disks, etcd, auth, exposure, tuning. |
| `FSBucket` | An S3 bucket in a referenced cluster. |
| `FSAccessKey` | One S3 credential with bucket grants, generated or imported. |

It provisions per-node StatefulSets with PVC-backed disks, maps fs racks
(failure domains) onto zones, and encodes fs's operational contracts as
controller logic: rolling updates one node at a time gated on cluster
reconvergence, explicit schema migrations, and drain-before-remove
decommissioning.

See [SPEC.md](SPEC.md) for the full design and [docs/](docs/overview.md) for
installation and guides.

## Quick start

```sh
# Install the operator (CRDs included).
helm install fs-operator oci://ghcr.io/go-faster/charts/fs-operator \
  --namespace fs-operator-system --create-namespace

# Create a 3-node dev cluster (requires a reachable etcd).
kubectl apply -f examples/01-minimal.yaml
```

The [examples/](examples/) gallery goes from a minimal dev cluster to a
zonal, multi-disk production shape.

## Development

Standard kubebuilder workflow:

```sh
make manifests generate   # regenerate CRDs and deepcopy after API changes
make helm-sync-crds       # keep the owned chart's CRDs in lockstep
make test                 # unit + envtest
make lint                 # golangci-lint
```

The Helm chart in `dist/chart` is committed and hand-owned; its CRD
templates are synced from `config/crd` by `hack/sync-chart-crds.sh` and CI
fails on drift.

## License

Apache-2.0. Copyright 2026.
