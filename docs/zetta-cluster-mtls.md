# Zetta cluster mTLS — the shared join token

> Source: [`internal/controller/oramazettacluster_controller.go`](../internal/controller/oramazettacluster_controller.go)
> (`reconcileZettaClusterToken`, `mutateZettaStatefulSet`). Tracks
> [operator#92](https://github.com/oramasearch/orama-operator/issues/92).

Every `OramaZettaCluster` node talks to its peers over **mutual-TLS** on the peer port (`:7777`).
There is **no plaintext-peer mode** — inter-node replication, the signal plane, and blob transfer are
always encrypted and mutually authenticated. This document explains the one piece of shared state that
makes that work: the **cluster join token**.

## The join token *is* the cluster identity

In Zetta's mTLS model the cluster's certificate authority (CA) is derived **deterministically** from a
single secret — the join token:

```
cluster CA = SHA256( token ‖ "zetta-cluster-ca-v1" )
```

(See the engine's `docs/cluster/security.md`.) A node uses that CA to issue its own node certificate and
to validate its peers'. The consequence is simple and unforgiving:

> **Only nodes that hold the *same* join token derive the same CA, so only they trust each other.**

If each node picks its own token, each derives a **different** CA and rejects every peer's certificate —
so **every node forms its own cluster-of-one**. That was the bug in
[operator#92](https://github.com/oramasearch/orama-operator/issues/92): the operator wired the
MCP and admin tokens but never a cluster join token, so a 4-node StatefulSet reported `cluster: 1 node`
on every node indefinitely.

## What the operator does

1. **Mint once.** `reconcileZettaClusterToken` provisions a Secret **`<name>-zetta-cluster`** with one
   key, `token` = **32 cryptographically-random bytes rendered as 64 lowercase hex** (`randHex(32)`).
   It is minted **once and preserved** across reconciles — **never rotated**. Rotating the token would
   re-derive the CA and orphan every existing node certificate on disk, splitting the cluster; the token
   is the cluster's durable identity, so it is created and then left alone.
   - This mirrors the `reconcileZettaAdminToken` reconciler (the admin token that authenticates the
     engine's now-mandatory auth, [zetta#799](https://github.com/oramasearch/zetta/issues/799)) — both are
     **unconditional** and minted-once. `spec.mcpTokenAuth` / `spec.requireAuth` are now deprecated no-ops.
     The cluster CA is fundamental to a multi-node cluster, so there is no "off" state and the Secret is
     never torn down.
   - It runs **before** the StatefulSet in the reconcile so the pods' `secretKeyRef` resolves on first
     apply.

2. **Inject into every node.** `mutateZettaStatefulSet` adds `ZETTA_CLUSTER_TOKEN` to **every** pod
   (node-0 included) via a `secretKeyRef` into `<name>-zetta-cluster/token`. The engine reads
   `ZETTA_CLUSTER_TOKEN` on boot and maps it to its `--join-token` (its `src/main.cpp` provisioning env),
   so all nodes derive the same CA and form **one** cluster.

3. **Mount it as a file too** ([zetta#770](https://github.com/oramasearch/zetta/issues/770)). The same
   Secret key is a read-only Secret volume `cluster-token`, mounted at `/run/secrets/zetta-cluster`
   (file `token`, mode `0440`: root and the pod's `fsGroup` 10001, the zetta user, no one else), and
   `ZETTA_CLUSTER_TOKEN_FILE=/run/secrets/zetta-cluster/token` points the engine at it. An environment
   variable is readable through `/proc/<pid>/environ` and is inherited by every child process the node
   starts; a file is not. A Zetta image that includes zetta#770 reads the file first, refuses to start if
   the file and `ZETTA_CLUSTER_TOKEN` disagree, and removes `ZETTA_CLUSTER_TOKEN` from its environment
   once read. `ZETTA_CLUSTER_TOKEN` stays set for images that predate zetta#770, which ignore the file
   variable and would otherwise mint their own token and split the cluster. Dropping it, once every image
   in use reads the file, is [operator#721](https://github.com/oramasearch/orama-operator/issues/721).
   Both names are operator-owned: `spec.env` cannot set either.

### Why env var, not a `--join-token` flag

The token is passed **only as an environment variable**, not appended to the `zetta` command line — even
though the MCP/admin tokens *are* passed as flags. Two reasons:

- **The engine reads it natively.** `ZETTA_CLUSTER_TOKEN` is part of the engine's provisioning env; the
  operator does not need to translate it into a flag.
- **Forward/backward safety.** An image that predates the feature **ignores** an unknown env var, but
  **crashes** on an unknown *flag* (the sandbox image exits with a usage error on unrecognised options).
  Keeping the token env-only means adding it can never break a node on an older image — the default path
  degrades to "no shared token" (the prior behaviour) instead of crash-looping.

The pod's entrypoint echo shows a redacted `[join-token via env ****]` marker so operators can confirm
the token is present without it ever landing in the pod log.

## Retrieving the token

```sh
kubectl get secret <name>-zetta-cluster -n <ns> -o jsonpath='{.data.token}' | base64 -d
```

Treat it like a cluster root credential: anyone with the token can mint a node certificate the cluster
will trust. It never appears in logs, the CR, or a ConfigMap — only in the Secret.

## Why there is no plaintext mode

An earlier `spec.clusterPlaintext` flag disabled mTLS entirely (`--cluster-plaintext`) as a workaround
for multi-node clusters *before* a shared token existed — peers joined unauthenticated over plaintext.
Once real mTLS clustering worked (this change), that flag was a **security hole**: unencrypted CRDT
replication + blob transfer on the wire, with no peer authentication. Per operator principle #1
(nothing that is wrong-for-production ships), it was **removed entirely** rather than left as a footgun —
there is no way to turn peer encryption off. Existing CRs that still set `clusterPlaintext` have the
unknown field pruned by the CRD schema (it is a no-op, not an error).

## Multi-node convergence is not done by this change alone

Seeding one shared token is **necessary but not sufficient** for a multi-node StatefulSet to actually
cluster. Two engine-side dependencies must also land:

- [zetta#785](https://github.com/oramasearch/zetta/issues/785) — the engine's dialer can't yet resolve
  the StatefulSet per-pod DNS names the operator seeds peers with (either the engine fix lands, or the
  operator resolves/injects IPs at boot).
- [zetta#786](https://github.com/oramasearch/zetta/issues/786) — a separate engine mTLS-sync bug blocks
  convergence even once peers can dial each other.

Until both land, a single-node cluster (`replicas: 1`) is fully functional; a `replicas > 1` cluster gets
the correct shared identity from the operator but won't converge. See also
[zetta#784](https://github.com/oramasearch/zetta/issues/784) (clusters don't form) and
[zetta#770](https://github.com/oramasearch/zetta/issues/770) (join-token env-exposure hardening).

**Cross-cluster convergence also needs the reachability-aware dialer**
([zetta#803](https://github.com/oramasearch/zetta/issues/803)): a `dial-out-only` node must advertise **no**
dialable address and peers must **never** dial it — otherwise, when two clusters share a pod CIDR (k0s + AKS
both `10.244.0.x`), the dial reaches the wrong node and membership undercounts (peripheral nodes showed
`cluster: 7 node`, not 9). Such a node is a pure initiator that syncs both ways over its one outbound link,
and full membership rides the CRDT node-list ([zetta#800](https://github.com/oramasearch/zetta/issues/800)).
The reachability model — which node advertises what — lives in [`zetta-datastore.md`](zetta-datastore.md)
("Peering across clusters"); the operator already knows each slice's `spec.reachability` and passes each
wan-accessible node its own `--advertise` address ([operator#104](https://github.com/oramasearch/orama-operator/issues/104)).

## Related

- [`crd-reference.md`](crd-reference.md) — the `OramaZettaCluster` field reference.
- [`zetta-datastore.md`](zetta-datastore.md) — Zetta as the operator's datastore (peer/reachability model).
- [`DESIGN.md`](../DESIGN.md) — decision row 22.
