# Peer connections

[`PeerConnection`](../crd-reference.md#peerconnection-separate-crd-namespaced--one-end-of-a-peer)
and [`PeerLink`](../crd-reference.md#peerlink-separate-crd-cluster-scoped--credentials-for-a-pair-through-the-managing-cluster)
implement stage 5 of the [secure-networking design](secure-networking.md) for
[operator#472](https://github.com/oramasearch/orama-operator/issues/472), decision D6: another
cluster, or a customer's network, reaches **named services** here, and the reverse, through
[zetta-tunnel](https://github.com/oramasearch/zetta-tunnel)'s peer gateway at each end
(`docs/peer.md`). No pod, Service or VPC address is routed between the two; every AKS VNet is
`10.224.0.0/12`, so routing could not work anyway.

```text
workload -> peer-<import> Service -> gateway import listener --mTLS 1.3--> other gateway -> named export target
```

## The kinds

| Kind | Where | Says |
|---|---|---|
| `PeerConnection` | The namespace of one end (a Lake workspace, or an Orama stack's namespace) | This end's peer id, the other end's address, the services it exports (by Lake service/app id or by Kubernetes Service name) and the named services it imports |
| `PeerLink` | The managing cluster, cluster-scoped | The two `PeerConnection`s of a pair (cluster, namespace, name). It mints their trust root and certificates and writes each end's credentials **on that end's cluster** |

The provider is zetta-tunnel, Orama's own component, not a vendor, so there is no provider kind
(principle 10 splits a kind when a vendor provides it). A second provider would be added as a
concrete kind behind `PeerConnection`.

## Where peers come from

- **Lake** (decision D1): Lake's `network_peers` rows (migration `0084_network`) become Lake-controlled
  `PeerConnection/peer-<row id>` in the workspace namespace, after `network_guard` exists. The row id
  is the peer id, so **both ends' rows must carry the same id** (the second end is created with the
  first's id): zetta-tunnel refuses a request naming another peer. `exports` and `imports` are decoded
  strictly (unknown fields, a non-UUID target or a numeric-address name fail the row). Status goes
  back to `network_peer_status` (`Pending | Ready | Failed | Deleted`, with the generation) only on a
  change. `desired = 'absent'` removes the peer, then `Deleted` is written and the resource deleted.
- **Orama's own stacks** (first user: Orama Cloud v1's US readers following the EU writer,
  [#396](https://github.com/oramasearch/orama-operator/issues/396), after
  [#458](https://github.com/oramasearch/orama-operator/issues/458)): a `PeerConnection` with
  `kubernetesService` exports, written by the operator for the stack or committed under
  `config/clusters/`. Wiring Cloud v1 is #396's work, not this one.

## One end

The `PeerConnection` controller renders, in the end's namespace:

| Object | What |
|---|---|
| Service `<name>-gateway` | `LoadBalancer`, TCP 7446 (zetta-tunnel's mutual-TLS listener), `externalTrafficPolicy: Local`. Made first, because the other end needs this address before either gateway can start. Its address is `status.address`. |
| ConfigMap `<name>-gateway` | `peer.json`, zetta-tunnel's exact projected peer: `id`, `workspace` (the namespace), `remote`, `imports` (`name`, `port`, `listen`), `exports` (`name`, `port`, `target`). Mounted without `subPath`; the gateway re-reads it every two seconds. |
| Deployment `<name>-gateway` | Two replicas of `zetta-tunnel peer`, restricted Pod Security, non-root, read-only root, `--peer-name` the other end's identity. A credential change restarts it. |
| Service `peer-<import name, dots as dashes>` per import | ClusterIP on the import's port, to the gateway's listener for it (allocated from 20000 in the imports' sorted order). Workloads use `peer-<name>.<namespace>.svc:<port>`; the status message lists them. A Service of that name owned by something else fails the peer. |

An export's name is its `name`, or for a Lake export the target Kubernetes Service's name (the
`-rw` Service for Postgres). Its target is that Service's cluster DNS name and port.

Policies, all in the namespace's `NetworkSandbox` (the Lake workspace renderer keeps them), named
`peer-<name>-…`:

| Allowance | Lets |
|---|---|
| `gateway-ingress` | Anyone reach the gateway on 7446 only. The gateway admits only a certificate from the pair's root with the other end's exact identity. |
| `gateway-egress` | The gateway reach only: the other end's address and port (a `/32` when it is an address; public addresses only, never private, metadata, node or pod ranges, when it is a name), cluster DNS, and the exported targets' pods on their pod ports. |
| `export-<n>` | Each exported target admit only the gateway, on only its pod port. |
| `imports-ingress` / `imports-egress` | This namespace's workloads reach the gateway on only the import listener ports. |

Workloads in other namespaces cannot use an import, and nothing in the namespace can reach the
gateway's mutual-TLS or probe ports. Who inside the namespace may use an import is not narrowed
further yet: Lake's row names no consumer.

## Credentials, through the managing cluster

Each gateway's credentials Secret (`<name>-credentials`, or `spec.credentialsSecretName`) holds
`tls.crt`, `tls.key`, `ca.crt`, `peer-name` (the other end's identity) and, once known, `remote`
(the other end's address). **Only a `PeerLink` writes it.** Without it the end stays `Pending`
("waiting for credentials Secret …, written by a PeerLink on the managing cluster"). Missing, deleting
or incomplete Secrets, invalid certificates/keys and empty peer identities withdraw any existing
gateway, import Services, projection and allowances before waiting for replacement credentials.
Credential checks run before sandbox and export resolution, so missing prerequisites cannot hide
revocation. A credential read error also triggers withdrawal and is retried.

A `PeerLink` on the managing cluster:

1. mints the pair's CA once (ECDSA P-256, ten years) in Secret `peerlink-<name>` in the operator's
   namespace, and one leaf per end with exactly one DNS name, `<end>.<peer id>.peer.orama.internal`,
   for client and server authentication (90 days, reissued with fewer than 30 left);
2. reads each end's `PeerConnection.status.address` on its cluster;
3. writes each end's credentials Secret on that end's cluster (through the `KubernetesCluster`'s
   kubeconfig; an empty `clusterRef` is the managing cluster itself), with the other end's identity
   and address. A `PeerConnection` with no `spec.remote` uses that address.

Deleting the `PeerLink` deletes both Secrets. A Secret of the same name that the `PeerLink` did not
write is never overwritten. Each end watches Secret changes and withdraws its gateway when its
credentials disappear, even while its `PeerConnection` remains `desired: present`.

## Revoking a peer

`desired: absent` (or deleting the `PeerConnection`): the import Services and every allowance are
withdrawn first, then the projection, the gateway and its load balancer, as zetta-tunnel's contract
orders it; then `Deleted`. Gateway deletion uses foreground garbage collection: the operator waits
for the old Deployment and its dependent ReplicaSets and pods to disappear before completing
withdrawal or starting a replacement. While termination is in progress, credential withdrawal
reports `Pending` with a message that the old gateway pods are still terminating. Cleanup failures
are retried, and a failed Service or policy cleanup still attempts gateway deletion.

Changing the credential Secret name, replacing the Secret, or changing its certificate, private
key, trust root or expected peer identity follows the same withdrawal path. Rotation interrupts
existing sessions and waits for termination before creating a gateway with fresh TLS contexts;
it does not overlap old and new gateways through a rolling update. Rotation keeps the public
Service so the pair retains its addresses. The first reconciliation after
upgrading from the earlier fingerprint format also replaces each existing gateway once. Changes
to `remote` still flow through the peer document. This contract applies to operator-managed
gateways; hot reloading credentials in a standalone zetta-tunnel process is outside this controller.

Removing the gateway closes every session. At the other end, its gateway
can no longer reach this one, so its imports fail too; revoke both rows (or delete the `PeerLink` as
well) to remove both ends.

## Records

The gateways run with `--audit -`: one `peer_decision` JSON line per allow or deny, with the peer id,
direction, service, port and source address. Delivery into the workspace Zetta is
[zetta#1281](https://github.com/oramasearch/zetta/issues/1281).

## Verify

Native tests (`go test ./internal/controller/ -run 'Peer'`):

- `TestPeerConnectionRehearsalBetweenTwoClusters`: two fake clusters and a managing cluster. Without a
  `PeerLink` nothing runs; the `PeerLink` gives each end a certificate the other verifies and the
  other's exact identity and address; neither gateway starts before it knows the other end; the
  documents are zetta-tunnel's exact shape and complementary; only the gateway reaches only the
  writer's pod port at the exporting end, and the importing end has one Service and one listener;
  revoking removes the import Service, every policy, the projection and the gateway; deleting the
  `PeerLink` takes the credentials back from both clusters.
- `TestLakePeerRowBecomesAPeerConnectionAndAbsentEndsIt`, `TestLakePeerRowsAreValidatedAndReadOnlyAfterTheGuard`.
- `TestPeerCRDsRefuseMalformedResources` (envtest).
- Opt-in, against local Postgres: `LAKE_PEERS_TEST_POSTGRES=1 go test ./internal/controller/ -run
  PeersPgxStore` creates the 0084 peer tables itself in a rolled-back scratch schema.

Live acceptance, **rehearsed on two nonprod clusters first** (the issue's order): only one nonprod
cluster exists today (`orama-nonprod-cluster-01`), so the rehearsal needs a second nonprod cluster
or two namespaces whose `PeerConnection`s reach each other through their load balancers on the one
cluster. Then: only the named services are reachable across, and setting the peer absent ends it.
