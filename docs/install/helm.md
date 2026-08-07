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
[`examples/00-single-node.yaml`](../../examples/00-single-node.yaml) for a
one-pod development install that needs no etcd, or
[`examples/01-minimal.yaml`](../../examples/01-minimal.yaml) for a three-node
one.

## Admission webhook

Off by default. With it on, an `FSCluster` whose spec cannot work is rejected
by `kubectl apply` — you find out where you made the mistake, instead of the
object being stored and the operator refusing to build it:

```console
$ kubectl apply -f cluster.yaml
The FSCluster "prod" is invalid: spec: Invalid value: "":
  SchemeTopologyMismatch: scheme "rf3" places copies on 3 distinct failure
  domains, the topology provides 2
```

It needs a serving certificate the API server trusts, which is why it is
opt-in — a chart cannot conjure one. With
[cert-manager](https://cert-manager.io) installed:

```sh
helm upgrade --install fs-operator oci://ghcr.io/go-faster/charts/fs-operator \
  --set webhook.enabled=true \
  --set certManager.enabled=true
```

The chart then issues a self-signed CA and certificate, and annotates the
webhook configuration so cert-manager injects the CA bundle.

To bring your own certificate instead, put `tls.crt`/`tls.key` in a Secret and
point the chart at it:

```sh
--set webhook.enabled=true \
--set webhook.certSecretName=my-webhook-cert \
--set webhook.caBundle=$(base64 -w0 < ca.crt)
```

**`failurePolicy` is `Fail`.** If the webhook is unreachable, applies are
rejected rather than admitted unchecked — these checks are what stands between
a typo and a cluster that cannot host its own data. `webhook.failurePolicy:
Ignore` relaxes that if you would rather trade the guarantee for availability;
the operator still refuses the spec, but only after it has been stored.

**Right after installing**, an `FSCluster` apply can be rejected with:

```
Internal error occurred: failed calling webhook "vfscluster.kb.io":
  dial tcp 10.96.0.1:443: connect: connection refused
```

The operator reports Ready only once the webhook is serving, but the Service
backend is programmed a moment later, and `failurePolicy: Fail` rejects rather
than admits in that window. It is a few seconds and it resolves itself — retry
the apply. Nothing is half-created: the object was never stored.

Everything the webhook checks, the controller checks again before it touches
anything, so turning the webhook off costs you the early error message and
nothing else. That is deliberate: a webhook can be disabled, unreachable, or
simply not have existed when an object was stored.

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

## Values reference

Defaults are what `helm install` gives you with no `--set`. `values.yaml` in
the chart carries the same information with more commentary.

### Operator deployment

| Value | Default | What it does |
|---|---|---|
| `manager.enabled` | `true` | Install the operator itself. `false` installs only the CRDs and RBAC. |
| `manager.replicas` | `1` | Replica count. More than one is safe — leader election means only one reconciles. |
| `manager.image.repository` | `ghcr.io/go-faster/fs-operator` | Operator image. |
| `manager.image.tag` | chart `appVersion` | Overrides the tag the chart would pick. |
| `manager.image.digest` | _unset_ | Pin by content. Wins over `tag`. |
| `manager.image.pullPolicy` | `IfNotPresent` | |
| `manager.imagePullSecrets` | _unset_ | Pull secrets for a private registry. |
| `manager.args` | `[--leader-elect]` | Manager flags. |
| `manager.resources` | 500m / 128Mi limits | Requests and limits. |
| `manager.podSecurityContext` | non-root, `RuntimeDefault` | Pod-level hardening. |
| `manager.securityContext` | no caps, no escalation, read-only root | Container-level hardening. |
| `manager.affinity`, `.nodeSelector`, `.tolerations` | empty | Scheduling. |
| `manager.terminationGracePeriodSeconds` | `10` | |
| `global.imageRegistry` | `""` | Rewrites the registry **host** of both the operator image and the fs node image the operator deploys, for mirrors and air-gapped installs. Empty is a no-op. |

### CRDs

| Value | Default | What it does |
|---|---|---|
| `crd.enabled` | `true` | Install the CRDs with the chart. |
| `crd.keep` | `true` | Keep them on `helm uninstall`. Deleting a CRD deletes every object of that kind — including running clusters — so this defaults to keeping them. |

### Metrics and monitoring

| Value | Default | What it does |
|---|---|---|
| `metrics.enabled` | `true` | Serve `/metrics`. |
| `metrics.port` | `8443` | |
| `metrics.secure` | `true` | HTTPS with authn/authz. `false` serves plain HTTP. |
| `prometheus.enabled` | `false` | Create a ServiceMonitor. Needs prometheus-operator. |
| `grafanaDashboard.enabled` | `false` | Publish the operator dashboard as a ConfigMap. Needs a scrape path to show anything. |
| `grafanaDashboard.labels` | `grafana_dashboard: "1"` | The label the Grafana sidecar discovers by; `kube-prometheus-stack` uses this one, others differ. |
| `grafanaDashboard.namespace` | `""` | Release namespace when empty. Set it when the sidecar only watches its own. |

### Admission webhook

| Value | Default | What it does |
|---|---|---|
| `webhook.enabled` | `false` | Reject an unworkable spec at apply time rather than storing it. Opt-in because it needs a certificate the API server trusts. |
| `webhook.failurePolicy` | `Fail` | `Ignore` admits specs unchecked when the webhook is unreachable — the controller still refuses them, but the object is stored by then. |
| `webhook.certSecretName` | `""` | Bring your own serving certificate (`tls.crt`/`tls.key`) instead of cert-manager. |
| `webhook.caBundle` | `""` | The base64 CA bundle to go with it. |
| `certManager.enabled` | `false` | Issue the webhook (and metrics) certificates with cert-manager. |

### RBAC and service account

| Value | Default | What it does |
|---|---|---|
| `serviceAccount.enabled` | `true` | Create the operator's ServiceAccount. |
| `rbac.helpers.enabled` | `false` | Install convenience admin/editor/viewer roles for the CRDs. |
| `rbac.namespaced` | `false` | Scope the operator to the release namespace: `Role`/`RoleBinding` instead of cluster-wide, and the watch scope narrowed to match. |
| `watchNamespaces` | _unset_ | Namespaces to watch. Empty watches all of them. |
| `networkPolicy.enabled` | `false` | Restrict ingress to the operator pod. Separate from an `FSCluster`'s own `spec.networkPolicy`, which is [documented with the cluster](../guides/security.md#network-policy). |

## Limiting what the operator watches

The supported deployment is **one cluster-wide installation**. Tenancy comes
from the namespaced custom resources and RBAC on them, not from running an
operator per team.

Where that is not allowed — a cluster where nobody gets a ClusterRole — there
are two ways to narrow it, and they must agree with each other:

```sh
# Everything in one namespace: namespaced RBAC, and the watch scope follows.
helm install fs-operator oci://ghcr.io/go-faster/charts/fs-operator \
  --namespace fs-system --create-namespace \
  --set rbac.namespaced=true

# Or watch a specific set, with cluster-wide RBAC.
helm install fs-operator oci://ghcr.io/go-faster/charts/fs-operator \
  --namespace fs-system --create-namespace \
  --set 'watchNamespaces={team-a,team-b}'
```

`rbac.namespaced: true` implies `watchNamespaces: [<release namespace>]`,
because the two scopes have to match: an operator holding a namespaced `Role`
while watching cluster-wide has every List and Watch refused, and the shape
that produces is the bad one — it starts, passes its health checks, reports
Ready, and reconciles nothing while logging `forbidden`. Combining
`rbac.namespaced` with a `watchNamespaces` entry outside the release namespace
is refused when the chart renders, rather than installed and discovered later.

### The caveat before you run more than one

**CRDs are cluster-scoped, so every installation in a cluster shares one set of
them.** Two installations at different chart versions share whichever CRDs were
applied last, and the older operator then sees objects with fields it does not
understand. Pin the same chart version everywhere, or run a single instance.

`crd.enabled=false` on the extra installations, with one owner applying the
CRDs, makes that ownership explicit.
