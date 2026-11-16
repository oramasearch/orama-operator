# Zetta cluster mTLS — the shared join token

> Source: [`internal/controller/oramazettacluster_controller.go`](../internal/controller/oramazettacluster_controller.go)
> (`reconcileZettaClusterToken`, `reconcileZettaConnectionRoot`, `mutateZettaStatefulSet`). Tracks
> [operator#92](https://github.com/oramasearch/orama-operator/issues/92) and
> [operator#1166](https://github.com/oramasearch/orama-operator/issues/1166).

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

2. **Give it to every node.** Every pod (node-0 included) gets the token from
   `<name>-zetta-cluster/token`: as the file below, and, for an image not known to read that file, as
   `ZETTA_CLUSTER_TOKEN` through a `secretKeyRef`. The engine maps it to its `--join-token` (its
   `src/main.cpp` provisioning env), so all nodes derive the same CA and form **one** cluster.

3. **Mount it as a file too** ([zetta#770](https://github.com/oramasearch/zetta/issues/770)). The same
   Secret key is a read-only Secret volume `cluster-token`, mounted at `/run/secrets/zetta-cluster`
   (file `token`, mode `0440`: root and the pod's `fsGroup` 10001, the zetta user, no one else), and
   `ZETTA_CLUSTER_TOKEN_FILE=/run/secrets/zetta-cluster/token` points the engine at it. An environment
   variable is readable through `/proc/<pid>/environ` and is inherited by every child process the node
   starts; a file is not. A Zetta image that includes zetta#770 reads the file first, refuses to start if
   the file and `ZETTA_CLUSTER_TOKEN` disagree, and removes `ZETTA_CLUSTER_TOKEN` from its environment
   once read.

   **`ZETTA_CLUSTER_TOKEN` is set only where it is needed**
   ([operator#721](https://github.com/oramasearch/orama-operator/issues/721)). The operator reads each
   image's own `zetta --help` (`status.imageFeatures`, the same probe as the egress file). An image whose
   usage lists `ZETTA_CLUSTER_TOKEN_FILE` (zetta v0.5.0 on) gets the file only, so the token is never in
   its environment. Any other answer keeps the variable: an image that predates zetta#770 ignores the
   file and would otherwise mint its own token and split the cluster. That covers an older usage, a probe
   that failed, an answer for another image, or no answer yet. A new image keeps the variable until its
   own answer arrives. Moving a cluster to an image that reads the file rolls its StatefulSet once, without
   the variable. An image that still gets it renders the same pod template as before, so it does not
   roll. Both names are operator-owned: `spec.env` cannot set either.

4. **Keep saved connector credentials off the data volume**
   ([operator#1166](https://github.com/oramasearch/orama-operator/issues/1166),
   [zetta#1314](https://github.com/oramasearch/zetta/issues/1314)). Zetta used to seal saved connector
   credentials under a key derived from the join token it keeps in `/data/security/`, so a copy of the
   data volume opened every stored source credential. `reconcileZettaConnectionRoot` mints one more
   Secret per cluster, **`<name>-zetta-connection-root`** (key `root`, 32 random bytes as 64 hex, minted
   once and never written again while it holds anything), owned by the `OramaZettaCluster`. Every node
   mounts it read-only at `/run/secrets/zetta-connection-root/root` (mode `0440`, outside `/data`), and
   `ZETTA_CONNECTION_ROOT_SECRET_FILE` points the engine at it. The engine refuses a file inside
   `--data-dir`, one writable by group or others, and a secret shorter than 32 characters; this mount
   meets all three. The variable is operator-owned: `spec.env` cannot set it.
   - **Every node of the cluster mounts the same Secret**, because each node opens what another sealed.
     A slice of the same global cluster (`spec.clusterName`) in another Kubernetes cluster mints its own,
     so a connection saved through one slice opens only on that slice's nodes.
   - **Roll order.** An image without zetta#1314 ignores the variable and keeps its old sealing. An image
     with it that starts without the file refuses `create_connection` and shows saved connections as
     `unreadable` (everything else keeps working). So this operator change rolls **before** that image.
     When the file is first present, the engine keeps the join tokens as retired secrets and re-seals
     existing rows under the new one at that start; roll every member together, because a member still
     on the old image cannot open rows a new member re-sealed.
   - **Rotation** is by hand and the engine's own: put the new secret on the first line of `root`, keep
     the old one on the line below, and restart every node (each re-seals at its next start). Drop the
     old line once every node has restarted. The operator never replaces a value that is there.
   - **Never delete the Secret** while the data volume is kept: a new value orphans every sealed
     credential. It is deleted with its `OramaZettaCluster`, like the join token.

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

## Identity root and enrollment CA (operator#1174)

[operator#1174](https://github.com/oramasearch/orama-operator/issues/1174), for zetta
[#1512](https://github.com/oramasearch/zetta/pull/1512) ([zetta#1342](https://github.com/oramasearch/zetta/issues/1342),
[#1360](https://github.com/oramasearch/zetta/issues/1360), [#1319](https://github.com/oramasearch/zetta/issues/1319),
[#1320](https://github.com/oramasearch/zetta/issues/1320)) and [#1514](https://github.com/oramasearch/zetta/pull/1514)
([zetta#1513](https://github.com/oramasearch/zetta/issues/1513)). The code is
`internal/controller/zetta_identity.go`. Each setting follows what the image's own `zetta --help` lists
(`status.imageFeatures`, Job `<name>-zetta-features`, which now runs for **every** cluster, once per
image), never a flag.

| The image's usage lists | `status.imageFeatures` | What the operator does |
|---|---|---|
| `ZETTA_IDENTITY_MIN_EPOCH` (zetta PR #1512, the release with `rotate_identity_root`) | `identityRootRotation` | carries out a raised `spec.identity.adminTokenGeneration` |
| `ZETTA_CLUSTER_CA` (zetta PR #1514) | `clusterCA` | gives the nodes the enrollment CA's public key, once known |
| `ZETTA_CLUSTER_CA_SEED_FILE` (zetta PR #1514) | `clusterCASeedFile` | mints a CA seed for a new cluster and mounts it |

### Rotating the admin token

The identity root is derived from `ZETTA_ADMIN_TOKEN` and moves by succession: `rotate_identity_root`
must run on a node that still holds the current root **before** any pod starts with a new token, or
every pod comes back verify-only. So the token in Secret `<name>-zetta-admin` is never edited by hand.
Raise **`spec.identity.adminTokenGeneration`** (1 is the token minted with the cluster; it cannot be
lowered) and the operator:

1. waits until the image lists `ZETTA_IDENTITY_MIN_EPOCH`, the cluster is one slice (no
   `spec.clusterName` naming another, no `spec.externalPeers`: other slices hold tokens this operator
   does not), and every node is ready on the current template;
2. mints the new token into the Secret under key `next` (with `next-generation`). The pods read key
   `token`, so nothing starts with it yet;
3. calls `POST /v1/rotate_identity_root {new_admin_token}` on **node 0**
   (`<sts>-0.<headless>:4800`) with the current token as bearer. `revoke_tokens` is never sent: the
   old token keeps working for every other holder of the Secret (lake's data-plane variables,
   workspace credentials) until they restart. An answer that the new token "derives the current root"
   is a call that already went through, so a retry after a crash is safe;
4. moves `next` into `token`, records `generation`, and stamps `orama.cloud/zetta-admin-generation` on
   the pod template, so the StatefulSet rolls the new token. Each pod follows the succession and issues
   identity again.

Condition **`IdentityRootRotated`** (present only with `spec.identity.adminTokenGeneration`) is
`Current`, `Rotated`, or `False` with `ImageNotSupported`, `MultiSliceNotSupported`, `WaitingForNodes` or
`RotationFailed` (zetta's reason, never a token). `status.identity.adminTokenGeneration` is the
generation in the Secret. A rotation after a compromise, which revokes every token
(`revoke_tokens: true`), is not offered: every holder of the old token would have to restart first.

### The enrollment CA

After zetta PR #1514 a node that is not enrolled joins only through a member the enrollment CA signed.
Joiners need **`ZETTA_CLUSTER_CA`**, the CA's public key (64 hex; public, like
`ZETTA_IDENTITY_ROOT`, so a literal variable). In an operator-run cluster every pod gets the join token
from outside, so no pod creates the CA: some must hold its seed. Secret **`<name>-zetta-cluster-ca`**,
owned by the `OramaZettaCluster`, records it once:

- **A new cluster** (no StatefulSet yet) whose image lists `ZETTA_CLUSTER_CA_SEED_FILE` gets a seed:
  32 random bytes as 64 lowercase hex, key `seed`, minted once, and its Ed25519 public key, key
  `public`. Every node of the slice mounts the seed read-only at `/run/secrets/zetta-cluster-ca/seed`
  (mode `0440`, root and fsGroup 10001; never a variable) named by `ZETTA_CLUSTER_CA_SEED_FILE`, holds
  the CA, and signs enrollments. **A new cluster's StatefulSet waits for its image's `--help` answer**,
  so it is never created without the seed it needs. Never roll such a cluster back to an image before
  zetta PR #1514: that image generates its own CA, and the seed then no longer matches.
- **A cluster that already ran zetta PR #1512** holds a CA its nodes generated (`cluster-ca.json` in each
  data dir). A seed would differ from it and stop the node, so none is minted. Once node 0 runs an image
  that lists `ZETTA_CLUSTER_CA`, the operator reads the key from node 0's printed join command
  (`--cluster-ca <key>`) and records it under `public`; the nodes then get `ZETTA_CLUSTER_CA` (one
  roll), so a replaced or added node enrolls.
- **An ephemeral (burst) slice** never holds the CA: it gets `ZETTA_CLUSTER_CA` of a non-ephemeral
  slice of the same cluster (`spec.clusterName`) in its namespace, and enrolls through those nodes. A
  slice in another Kubernetes cluster is not looked up (the same limit as the connection root secret).
- `status.identity.clusterCA` and `clusterCASource` (`Operator`, `Members` or `Sibling`) say what the
  nodes are given. `ZETTA_CLUSTER_CA` and `ZETTA_CLUSTER_CA_SEED_FILE` are operator-owned: `spec.env`
  cannot set them.
- **Order** (zetta's): roll zetta PR #1514 only after the PR #1512 release has run on every pod of the
  cluster, so each pod has enrolled. This operator rolls before either.

### Identity epoch floor: not set

`ZETTA_IDENTITY_MIN_EPOCH` ([zetta#1360](https://github.com/oramasearch/zetta/issues/1360)) is optional,
and the operator does not set it. Decided under Steve's 2026-11-14 delegation, because the safe option
is to keep today's behaviour: a recorded floor makes a node honor no account, role or token until it
holds an epoch at least that high, so a cluster rebuilt from nothing, restored from an older backup, or a
one-node workspace Zetta that lost its volume would refuse every token, the operator's included, until
the floor was cleared by hand. Without the variable, zetta's behaviour is unchanged. `spec.env` can still
set it on a cluster that wants the floor.

## Related

- [`crd-reference.md`](crd-reference.md) — the `OramaZettaCluster` field reference.
- [`zetta-datastore.md`](zetta-datastore.md) — Zetta as the operator's datastore (peer/reachability model).
