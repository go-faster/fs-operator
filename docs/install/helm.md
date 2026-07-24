# Install with Helm

Released charts are published to an OCI registry alongside the operator
image (CRDs included, guarded by `crd.enabled` / kept on uninstall via
`crd.keep`):

```sh
helm install fs-operator oci://ghcr.io/go-faster/charts/fs-operator \
  --namespace fs-operator-system \
  --create-namespace
```

Pin a version with `--version X.Y.Z`. The chart's `appVersion` selects the
matching `ghcr.io/go-faster/fs-operator` image.

From a checkout, install the working-tree chart instead:

```sh
helm install fs-operator ./dist/chart \
  --namespace fs-operator-system \
  --create-namespace
```

Then create an [`FSCluster`](../guides/configuration.md) — see
[`examples/01-minimal.yaml`](../../examples/01-minimal.yaml) for a starting
point.

<!-- TODO(P2): values reference, watchNamespaces. -->
