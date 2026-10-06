# PLAN — current work plan

The operator runs **fs v0.14** (SPEC.md describes the model): one data volume
per node, an operator-owned layout, removal by layout transition, and
credentials rendered into every node's config. What earlier releases shipped
is the git history.

## Next

- **Cluster-wide runtime keys upstream.** fs v0.14 stores admin-created keys
  per node (go-faster/fs#341), which is why credentials live in config
  Secrets (SPEC §7, §11). A replicated key store in fs would let FSAccessKeys
  go through the admin API instead.
- **Stuck transitions.** A dead node holds a layout transition open until a
  human runs `fs layout skip`. The operator reports the wait; naming the
  node that has not synced would make the fix obvious.
