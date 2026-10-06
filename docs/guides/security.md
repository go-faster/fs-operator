# Security

## Secrets

Every secret the operator generates is 32 bytes from `crypto/rand`, encoded
URL-safe so it survives being pasted into a header, a URL or a config file.

| Secret | Key(s) | Purpose |
|---|---|---|
| `<cluster>-cluster-secret` | `secret` | HMAC authentication between nodes |
| `<cluster>-admin-token` | `token` | Bearer token for the admin API |
| `<cluster>-root-credentials` | `access-key`, `secret-key` | S3 admin on every bucket |

**A generated Secret is created once and never rewritten.** Rotating the
cluster secret partitions the cluster, and rotating a credential breaks
whoever holds it, so neither happens behind your back. If you delete one, the
operator mints a new value — which is a rotation you have chosen, with the
same consequences.

You can supply any of them instead, through `spec.clusterSecretRef` and
`spec.auth.rootCredentialsSecretRef`. A supplied secret is validated rather
than trusted: an S3 secret key shorter than fs's 16-character minimum is
refused with `WeakSecretKey` on the key's status instead of being pushed to
the cluster.

`clusterSecretRef` is **immutable**. fs has no secret rotation, and a cluster
running two different secrets is a cluster split in half.

### Where secrets are not

Nothing secret is written to a ConfigMap, an annotation, an inline env value,
or a log line. Node configuration is rendered into a **Secret**, not a
ConfigMap. The cluster's own secrets do not go into it even so — they reach fs
as environment variables through `valueFrom.secretKeyRef`:

```
FS_CLUSTER_SECRET   FS_ADMIN_TOKEN
FS_ROOT_ACCESS_KEY  FS_ROOT_SECRET_KEY
```

The [FSAccessKey](buckets-and-keys.md) credentials are the exception: they are
rendered into every node's config Secret, because that is how every node
accepts them and a hot reload applies a change everywhere at once. They are
kept out of the restart fingerprint, so adding, rotating or revoking one never
restarts a node. Anyone who can read Secrets in the namespace can read them —
the same people who can read the credential Secrets themselves.

## Ports

| Port | Listener | Exposure |
|---|---|---|
| 8080 | S3 (also `/health`, `/ready`) | the service; open |
| 9464 | Prometheus metrics | open, so it can be scraped |
| 7080 | peer replication | cluster-internal |
| 8090 | admin API | cluster-internal |
| 9010 | pprof | open, deliberately — see below; `observability.pprof: false` removes it |

**Peer traffic on 7080 is HMAC-authenticated but not encrypted.** Authenticated
means a node cannot be impersonated without the cluster secret; it does not
mean the object bytes on the wire are private. If your threat model includes
someone reading pod-to-pod traffic, that is a job for the cluster's own
transport layer (a service mesh, encrypted CNI), not for fs.

The admin API on 8090 requires the bearer token on every request, and the
token Secret never leaves the namespace.

### pprof

**pprof listens on 9010, on all interfaces, with no authentication, and the
network policy deliberately leaves it open.** Any pod in the cluster can pull
a profile.

That is a real grant, so it is worth being precise about what it gives away. A
heap profile is a slice of the process's memory: object bytes in flight, and —
because they are read from the environment at startup and held — the cluster
secret, the admin token and the root credentials. Goroutine and CPU profiles
are cheaper but still let an unauthenticated caller make a node do work.

**Treat every pod in the cluster as able to read fs's memory.** If your
namespace runs untrusted or multi-tenant workloads, that is the wrong trade.
Turn the listener off:

```yaml
spec:
  observability:
    pprof: false
```

The nodes then run without `PPROF_ADDR`, so the SDK serves no profiler at all,
and the container port and the NetworkPolicy rule go with it — closing the
port rather than restricting who may reach it. Keeping it but limiting the
callers is the other option, and that one is still a line in
`NewNetworkPolicy`: move 9010 from the open ingress rule to the restricted one
beside the peer and admin ports.

It defaults to on because reaching a struggling node's profiler without first
arranging network access is worth more, in the environments this is built for,
than the exposure costs.

### Network policy

`spec.networkPolicy: true` restricts 7080 and 8090 to the cluster's own pods
and the operator's namespace. S3, metrics and (unless disabled) pprof stay
reachable — a
NetworkPolicy ingress list is an allow-list, so anything meant to stay open is
named explicitly rather than left out.

It is off by default because a policy that silently breaks scraping is worse
than no policy. Turn it on in production: it is what stops arbitrary pods from
replicating peer traffic or reaching the admin API.

## Pod hardening

fs pods run unprivileged and hold nothing they do not need:

- non-root (uid/gid 1000), with `fsGroup` so a fresh volume is writable
- read-only root filesystem
- all capabilities dropped, no privilege escalation
- `seccompProfile: RuntimeDefault`
- **no service-account token mounted** — fs never talks to the Kubernetes API,
  so a mounted token would only be something to steal

The operator's own pod is hardened the same way, minus the volume-related
parts it has no use for: `manager.podSecurityContext` sets `runAsNonRoot` and
`RuntimeDefault`, and `manager.securityContext` drops all capabilities, forbids
privilege escalation and mounts the root filesystem read-only. Both are chart
values, so you can tighten them further (a specific `runAsUser`, for instance)
without forking the templates.

## S3

`spec.s3.tls.secretName` terminates TLS in fs itself from a `kubernetes.io/tls`
Secret. Certificate renewals hot-reload, so a cert-manager rotation does not
restart anything. Empty serves plaintext — and fs then refuses SSE-C
(customer-provided keys) requests, as S3 does, since the key would cross the
network in the clear.

`spec.auth.publicReadBuckets` lists buckets readable **anonymously**. Writes
still need a credential, but reads need nothing at all — so the contents are
public to anyone who can reach the endpoint.

Per-tenant credentials are [FSAccessKey](buckets-and-keys.md), which is the
right unit for anything that is not an administrator.

## Operator RBAC

The operator is cluster-scoped and holds:

| Group | Resources |
|---|---|
| core | Secrets, Services, PersistentVolumeClaims, Pods, Events |
| `apps` | StatefulSets |
| `policy` | PodDisruptionBudgets |
| `networking.k8s.io` | NetworkPolicies |
| `monitoring.coreos.com` | PodMonitors |
| `fs.go-faster.org` | the three custom resources, their status and finalizers |

The operator talks to fs only through each node's admin API, with the
cluster's admin token: to read and apply the layout, list the keys each node
accepts, set bucket schemes and trigger reloads.

Two absences are deliberate. There is **no `configmaps` permission at all** —
everything the operator renders is a Secret, so it never needed one. And there
is no `pods/exec`, which was only ever the SIGHUP config-reload fallback that
fs's own reload endpoint replaced.

Pods carry `delete` so the operator can replace one its StatefulSet re-adopted
without restamping; see [storage.md](storage.md).

## Admission webhook

Cross-field validation runs at admission, so an impossible spec is rejected by
`kubectl apply` rather than accepted and then reported as a condition. It needs
a certificate the API server trusts — `webhook.enabled=true` with
`certManager.enabled=true` in the chart wires it up.

It is a correctness control, not a security boundary: with
`failurePolicy: Fail` a broken webhook stops FSCluster writes, which is why the
chart waits for it to actually serve before reporting ready.

## Related

- Credentials as a tenancy unit: [buckets-and-keys.md](buckets-and-keys.md)
- Conditions and event reasons, including the refusals above:
  [monitoring.md](monitoring.md)
- Every field named here: [configuration.md](configuration.md)
