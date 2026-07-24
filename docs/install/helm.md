# Install with Helm

The operator ships its own chart in `dist/chart` (CRDs included, guarded by
`crd.enabled` / kept on uninstall via `crd.keep`).

```sh
helm install fs-operator ./dist/chart \
  --namespace fs-operator-system \
  --create-namespace
```

Then create an [`FSCluster`](../guides/configuration.md) — see
[`examples/01-minimal.yaml`](../../examples/01-minimal.yaml) for a starting
point.

<!-- TODO(P2): registry-hosted chart, values reference, watchNamespaces. -->
