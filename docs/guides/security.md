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
ConfigMap, and credentials do not go into it even so — they reach fs as
environment variables through `valueFrom.secretKeyRef`:

```
FS_CLUSTER_SECRET   FS_ADMIN_TOKEN
FS_ROOT_ACCESS_KEY  FS_ROOT_SECRET_KEY
FS_ETCD_USERNAME    FS_ETCD_PASSWORD
```

That split is deliberate. The rendered config is fingerprinted into a config
revision, and a revision change rolls the cluster — so a credential living in
the config would make **rotating a password restart every node**.

## Ports

| Port | Listener | Exposure |
|---|---|---|
| 8080 | S3 (also `/health`, `/ready`) | the service; open |
| 9464 | Prometheus metrics | open, so it can be scraped |
| 7080 | peer replication | cluster-internal |
| 8090 | admin API | cluster-internal |
| 9010 | pprof | open, deliberately — see below |

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
a profile, and there is currently no switch to turn it off.

That is a real grant, so it is worth being precise about what it gives away. A
heap profile is a slice of the process's memory: object bytes in flight, and —
because they are read from the environment at startup and held — the cluster
secret, the admin token and the root credentials. Goroutine and CPU profiles
are cheaper but still let an unauthenticated caller make a node do work.

**Treat every pod in the cluster as able to read fs's memory.** If your
namespace runs untrusted or multi-tenant workloads, that is the wrong trade,
and the fix is one line in `NewNetworkPolicy` — moving port 9010 from the open
ingress rule to the restricted one, alongside the peer and admin ports. It is
open because reaching a struggling node's profiler without first arranging
network access is worth more, in the environments this is built for, than the
exposure costs.

### Network policy

`spec.networkPolicy: true` restricts 7080 and 8090 to the cluster's own pods
and the operator's namespace. S3, metrics and pprof stay reachable — a
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

## etcd

etcd holds the node registry and the cluster's credential store. **Anything
that can write to it can reshape the cluster**, so it is worth securing even
when it looks like an internal dependency.

An `https://` endpoint is served over TLS on the strength of the scheme alone,
verifying against the system roots. `spec.etcd.external.tls.secretName` adds
your own trust material — `ca.crt` to verify etcd, plus `tls.crt`/`tls.key` for
mutual TLS if you use client certificates.
`spec.etcd.external.authSecretRef` supplies `username`/`password` for etcd's
role-based authentication.

A referenced Secret that is missing or malformed **refuses the spec**. The
alternative is worse than an error: with TLS half-configured a node either
fails to start or quietly connects in the clear.

`insecureSkipVerify` makes TLS decorative — anything on the path can
impersonate the control plane. It exists for development against self-signed
certificates.

The **managed etcd** (`spec.etcd.managed`) has neither TLS nor authentication,
whether you run it at 1 replica or 3. It exists so `kubectl apply` of an
example produces a working cluster. The operator says so itself, at apply time
and again on the object:

> etcd is operator-managed (development only): no backups, no defrag
> automation, no member replacement. Losing its volume loses the cluster's
> control plane and the credentials sealed in it. Use `etcd.external` in
> production.

That last clause is the security-relevant one: the credential store lives in
etcd, sealed with the cluster secret, so the volume under a managed etcd is as
sensitive as the credentials themselves.

## S3

`spec.s3.tls.secretName` terminates TLS in fs itself from a `kubernetes.io/tls`
Secret. Certificate renewals hot-reload, so a cert-manager rotation does not
restart anything. Empty serves plaintext.

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
| `batch` | Jobs, for schema migration |
| `policy` | PodDisruptionBudgets |
| `networking.k8s.io` | NetworkPolicies |
| `monitoring.coreos.com` | PodMonitors |
| `fs.go-faster.org` | the three custom resources, their status and finalizers |

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
