# Configuration

The `FSCluster` spec has these sections. Every field of every one is described
in the **[API reference](../reference/api.md)**, which is generated from the
types themselves and therefore cannot fall behind them.

| Section | Where it is explained |
|---|---|
| `topology` (flat nodes, racks, anti-affinity), `layout` | [scaling.md](scaling.md) |
| `storage` (size, class, expansion, reclaim) | [storage.md](storage.md) |
| `image`, `updatePolicy` | [upgrades.md](upgrades.md) |
| `clusterSecretRef`, `auth`, `s3.tls`, `networkPolicy` | [security.md](security.md) |
| `observability` | [monitoring.md](monitoring.md) |
| `s3.service`, `podTemplate` | the API reference |

The [examples](../../examples/) are working shapes for the common cases. Every
one is validated against a live API server on each run — schema, CEL rules and
the admission webhook — so an example the operator would now reject cannot sit
in the gallery unnoticed.

## What the operator renders

Each node gets its own `config.yaml` in a Secret (`<node>-config`), rendered
from the spec: the S3 and admin listeners, the storage root, the node's
identity and the peers it joins through, the cluster's FSAccessKeys and
public-read buckets, and the telemetry switches. The cluster secret, the admin
token and the root credential are not in it — they reach the node as
environment variables.

fs refuses a configuration key it does not know, so the operator and the fs
image it runs have to agree on the schema: run the fs version the operator pins
(`spec.image` defaults to it), or one it documents as compatible.

A change to the credentials, the public-read buckets or the peer list is
applied without a restart — credentials and buckets by a hot reload the
operator verifies on every node, the peer list because fs reads it only when it
starts. Anything else in the file restarts the nodes one at a time
([upgrades.md](upgrades.md)).
