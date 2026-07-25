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
a registry that needs credentials, and `manager.image.pullPolicy` /
`spec.image.pullPolicy` (default `IfNotPresent`) to keep a node from reaching
for a registry it cannot see.

### The two images to mirror

| Image | Pulled by | Overridden with |
|---|---|---|
| `ghcr.io/go-faster/fs-operator` | the operator Deployment | `manager.image.*` |
| `ghcr.io/go-faster/fs` | every `FSCluster`'s node pods | `spec.image.*` per cluster |

That is the whole list — the operator has no sidecars, no init containers and
no cert-manager or other in-cluster dependency to mirror.

### Pinning by digest

A tag can be repointed in a mirror under a running cluster; a digest cannot.
Both images can be pinned by content:

```yaml
# The operator image, at install time.
manager:
  image:
    repository: ghcr.io/go-faster/fs-operator
    digest: sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
```

```yaml
# The fs node image, per cluster.
spec:
  image:
    repository: ghcr.io/go-faster/fs
    digest: sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
```

A digest wins over `tag`, and `global.imageRegistry` still rewrites the
registry host, so a mirrored digest stays pinned. Writing the digest straight
into `repository` (`ghcr.io/go-faster/fs@sha256:…`) works too.

Changing a cluster's digest is an ordinary image change: it rolls the cluster
one node at a time under the usual convergence gates ([upgrades](../guides/upgrades.md)).

<!-- TODO(P2): values reference, watchNamespaces. -->
