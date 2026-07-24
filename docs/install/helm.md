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

## Air-gapped / mirrored registries

Set `global.imageRegistry` to pull every image from a private mirror. It
replaces the registry host of both the operator image and the fs node image
the operator deploys into each `FSCluster`, so no cluster has to spell out the
registry itself:

```sh
helm install fs-operator ./dist/chart \
  --namespace fs-operator-system --create-namespace \
  --set global.imageRegistry=registry.internal
```

With `registry.internal`, `ghcr.io/go-faster/fs-operator` resolves to
`registry.internal/go-faster/fs-operator` and `ghcr.io/go-faster/fs` (the fs
node image) to `registry.internal/go-faster/fs` — mirror both to that
registry, keeping the `go-faster/...` path. The chart wires the override into
the operator as `FS_IMAGE_REGISTRY`; the operator applies it to every cluster
it reconciles, overriding each `FSCluster`'s own `spec.image.repository` host.
Use `manager.imagePullSecrets` and each cluster's `spec.image.pullSecrets` for
a registry that needs credentials.

<!-- TODO(P2): values reference, watchNamespaces. -->
