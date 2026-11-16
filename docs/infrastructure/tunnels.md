# Developer tunnels

[`Tunnel`](../crd-reference.md#tunnel-separate-crd-namespaced--one-person-one-target-for-a-time)
implements stage 4 of the [secure-networking design](secure-networking.md) for
[operator#471](https://github.com/oramasearch/orama-operator/issues/471). One `Tunnel` lets one
person reach one target (a Lake service or app, on one port) from outside the cluster, until the
grant expires or is revoked. Nothing else in the workspace is reachable through it.

`Tunnel` is an abstract kind (principle 10). It names no vendor. Two concrete kinds carry it:

| Provider | Concrete kind | When |
|---|---|---|
| Orama's own [zetta-tunnel](https://github.com/oramasearch/zetta-tunnel) | `ZettaTunnel` | The default: a `Tunnel` with no `providerRef`, or one naming a `TunnelProvider` of type `zetta` |
| Cloudflare Tunnel, **in the customer's own Cloudflare account** | `CloudflareTunnel` | A `Tunnel` naming a `TunnelProvider` of type `cloudflare` |

Cloudflare's terms forbid reselling Cloudflare Tunnel from Orama's account (research,
2026-10-14), so a `CloudflareTunnel` exists only with the customer's account and the customer's API
token, on a `TunnelProvider` in the workspace namespace. The operator never calls Cloudflare with an
Orama credential for a tunnel.

## Desired state and ownership (decision D1)

Lake owns grants in its control Postgres: `network_tunnel_grants` (migration `0084_network`) and its
public trust column `client_ca_pem` (migration `0085_network_tunnel_ca`, section 11.1 of the design).
The operator writes only `network_tunnel_grant_status`, including `endpoint` and `relay_ca_pem`.

- The intake runs in the Lake control-row pass and wakes on `NOTIFY lake_network`. It reads nothing
  until `network_guard` exists **and** both 0085 columns exist. Before that it creates nothing and
  writes no status.
- Each grant whose end is not yet recorded becomes a Lake-controlled `Tunnel/tunnel-<grant id>` in the
  workspace namespace. `revoked_at` becomes `spec.revokedAt`; Lake's `generation` is acknowledged in
  `observed_generation`.
- A grant that has already ended when the operator first sees it gets `Expired` or `Revoked` without
  anything being built.
- After an `Expired` or `Revoked` status is recorded, the grant is no longer read and its `Tunnel` is
  deleted. A hard-deleted row's `Tunnel` is deleted as an orphan.
- The workspace's provider: if the namespace holds a `TunnelProvider` named `default`, Lake-granted
  tunnels use it; otherwise they use `ZettaTunnel`. Only a `cloudflare` provider is given the
  person's e-mail address (read from Lake's `users` table), because Cloudflare Access admits people
  by e-mail. No other personal data is read.

Status is written only on a real change. `endpoint` and `relay_ca_pem` are written only while the
phase is `Ready`, which is what Lake's `issue_network_tunnel_certificate` waits for.

## ZettaTunnel

```text
developer --mTLS--> relay (zetta-tunnel ns, LoadBalancer :443) --mTLS--> endpoint (workspace) --> target Service
```

### The relay (one per cluster)

The relay runs while any `ZettaTunnel` exists, in the operator-created namespace `zetta-tunnel`
(restricted Pod Security, its own `NetworkSandbox`):

- Deployment `zetta-tunnel-relay`, two replicas spread across nodes, a PodDisruptionBudget of one,
  restricted security context, non-root `65532`, read-only root file system.
- Service `zetta-tunnel-relay`, type `LoadBalancer`, TCP 443 to the relay's 7443, with
  `externalTrafficPolicy: Local` so audit records keep the developer's source address. TLS passes
  through: the relay itself verifies the developer's certificate.
- ConfigMap `zetta-tunnel-relay-grants`: `grants.json` is the zetta-tunnel grants file
  (`{"grants":[{"id","workspace","endpoint","expires_at"}]}`) of every live grant, sorted by id;
  `client-ca.pem` is the relay's developer trust bundle, the union of the live grants'
  `client_ca_pem`. The bundle also holds one placeholder CA whose key was discarded when it was made,
  so the file is never empty and trusts nobody when there are no grants. Mounted without `subPath`,
  so the relay's two-second reload sees the kubelet's atomic update; grant changes never restart the
  relay.
- The relay's NetworkSandbox admits only TCP 7443 from anywhere, and lets the relay reach only pods
  labelled as tunnel endpoints, in namespaces labelled as workspaces, on TCP 7444, plus cluster DNS.
- When the last `ZettaTunnel` is gone the relay's Deployment, Service, grants and leaf certificates
  are deleted. The namespace, its sandbox and the CAs stay.

### Certificates

The operator holds two CAs in Secret `zetta-tunnel/zetta-tunnel-pki`, minted once (ECDSA P-256,
ten years) and never rotated by the operator:

| CA | Signs | Verified by |
|---|---|---|
| relay CA | The relay's public server certificate: IP SAN of the load balancer address (DNS SAN for a hostname), plus `zetta-tunnel-relay.zetta-tunnel.svc` | The developer's client (`--ca`). It is `relay_ca_pem` in the grant's status |
| workload CA | The relay's workload certificate (`zetta-tunnel-relay.zetta-tunnel.svc`, client authentication) and each endpoint's (`endpoint.<grant id>`, server authentication) | The relay (`--endpoint-ca`) and each endpoint (`--ca`, `--peer-name zetta-tunnel-relay.zetta-tunnel.svc`) |

Developer certificates are Lake's (its per-customer CA). The two trust domains never meet: a
developer certificate cannot reach an endpoint. Leaves last 90 days and are reissued when fewer than
30 days remain, when their names change (a new load balancer address) or when their CA changes. A
leaf change restarts the relay through a pod-template hash; zetta-tunnel loads new leaves and CA
bundles for new connections anyway.

### The endpoint (one per grant)

In the workspace namespace, all owned by the `ZettaTunnel`:

- Secret `tunnel-<grant id>-endpoint` (the endpoint certificate and the workload CA).
- Service `tunnel-<grant id>`, ClusterIP, TCP 7444.
- Deployment `tunnel-<grant id>`: `zetta-tunnel endpoint --target <service>.<ns>.svc.cluster.local:<port>`,
  restricted Pod Security, the Lake's pull secret. The target is fixed for the life of the process;
  no client byte can choose another.
- The generated `Connection/connection-tunnel-<grant id>`, source kind `tunnel`: it lets only the
  endpoint pod (`orama.cloud/tunnel-grant-id=<grant id>`) reach only the target's pods, on only the
  port the target Service sends the grant's port to. The Connection controller accepts it because its
  controller is a `ZettaTunnel` of the same Lake and grant.
- The workspace sandbox allowance `tunnel-<grant id>-relay`: only relay pods in `zetta-tunnel` reach
  only this endpoint, on TCP 7444. The Lake workspace renderer keeps it (and the Cloudflare one
  below) when it rewrites the sandbox.

The target is the Kubernetes Service of the Lake service or app that has the grant's port, chosen
by the same identity labels as `Connection`. A Postgres service has several; the read-write `-rw`
Service is chosen. A Service whose port sends to a named target port is refused, because the
generated Connection needs the number.

`Ready` requires the endpoint Deployment available, its Connection `Ready`, the relay's address
known, the relay available and the grants file carrying the grant. Then `status.endpoint` is
`<load balancer address>:443` and `status.relayCAPEM` the relay CA.

## CloudflareTunnel

With a `TunnelProvider` of type `cloudflare` (account id, zone id, a domain in that zone, and a
Secret holding the customer's API token with Cloudflare Tunnel edit, Access apps and policies edit
and DNS edit on that account and zone):

1. A remotely managed tunnel `orama-tunnel-<grant id>` in the customer's account, configured with one
   ingress rule, `tunnel-<grant id>.<domain>` to `tcp://<target>:<port>`, and a final `http_status:404`.
2. A proxied CNAME `tunnel-<grant id>.<domain>` to `<tunnel id>.cfargotunnel.com`.
3. A self-hosted Access application on that hostname with one policy: allow exactly the grant's
   person's e-mail address. Its session lasts until the grant's expiry, at most twelve hours.
4. In the workspace: the connector token Secret, a `cloudflared` Deployment (`tunnel --no-autoupdate
   run`, restricted, pinned `cloudflare/cloudflared:2026.10.0`, Apache 2.0), the generated
   `Connection` to the target, and an allowance letting only the connector reach public addresses on
   TCP 443 and TCP/UDP 7844 (Cloudflare's edge), never private, metadata, node or pod ranges.

The developer connects with `cloudflared access tcp --hostname tunnel-<grant id>.<domain> --url
localhost:<port>` and signs in to Cloudflare Access. Lake's tunnel certificate capability is for
ZettaTunnel only; a Cloudflare grant's `relay_ca_pem` stays empty. The ids of the three Cloudflare
objects are kept in `CloudflareTunnel` status, so exactly those are deleted.

## How a grant ends

| Event | What happens |
|---|---|
| `revoked_at` set (Lake increments `generation` and notifies `lake_network`) | The next pass sets `spec.revokedAt`. The `Tunnel` deletes its provider; the provider's finalizer removes the sandbox allowances first, then the endpoint (or connector), then the Connection, and for Cloudflare the Access application, DNS record and tunnel. Only then is `Revoked` reported. The relay's grants file drops the grant in the same pass. |
| `expires_at` passes | The same teardown, then `Expired`. Every controller requeues at the earliest expiry. Independently, zetta-tunnel refuses an expired grant from the `expires_at` in its file, and Lake's client certificate ends no later than `expires_at`. |
| The row is deleted | The orphan `Tunnel` is deleted; the same finalizers run. |

Removing the endpoint closes every live session at once: the relay can no longer reach it, and its
connection to the terminated endpoint ends. The relay also drops the grant from its file within its
two-second poll once the kubelet has projected the new ConfigMap (the kubelet's ConfigMap sync is
usually well under a minute); either is enough to refuse new connections.

## Sessions are recorded

The relay and every endpoint run with `--audit -`: one JSON line per `session_start`,
`session_end` and `refusal` on stdout (zetta-tunnel `docs/tunnel.md`). The relay's records carry the
verified grant id, the workspace, the developer's address, the endpoint and byte counts. Audit failure
makes zetta-tunnel unready and refuses traffic. Delivery of these records into the workspace Zetta is
[zetta#1281](https://github.com/oramasearch/zetta/issues/1281); until then they are in the pods' logs,
and the operator claims no durable delivery. For a Cloudflare tunnel, Cloudflare Access records each
sign-in in the customer's account.

## Verify

Native tests (`go test ./internal/controller/ -run 'Tunnel|Cloudflare'`):

- `TestZettaTunnelGrantReachesOnlyItsTargetAndRevocationEndsIt`: the intake, the abstract and
  concrete controllers, the relay and the Connection together. The endpoint is fixed to the `-rw`
  target, the only ingress to it is the relay on 7444, its only egress the target pods on 5432, the
  relay serves the grant and trusts its CA, Lake gets `endpoint` and `relay_ca_pem` only when Ready
  and the relay certificate verifies with them, a steady tunnel writes nothing, and revocation removes
  every policy, the endpoint and the relay before `Revoked`, keeping the CAs.
- `TestZettaTunnelExpiryRemovesTheGrantFromTheRelay`: expiry ordering and the relay's own view.
- `TestTunnelRefusesAProviderOfAnotherCustomerAndAConnectionItDoesNotOwn`: a provider of another
  customer is refused, and a forged tunnel `Connection` opens nothing.
- `TestCloudflareTunnelUsesOnlyTheCustomersAccountAndAdmitsOnlyThePerson`: against a fake Cloudflare
  API, every call uses the customer's token, Access admits exactly one e-mail address for at most the
  grant's remaining time, the tunnel routes to the one target, the connector reaches only public
  addresses, and expiry deletes every Cloudflare object.
- `TestTunnelEnvtestServesTheGrantAndRemovesEverythingWhenItEnds` (envtest, real API server):
  finalizer order and revocation with all controllers running; `TestTunnelCRDsRefuseMalformedResources`.
- Opt-in, against local Postgres: `LAKE_TUNNELS_TEST_POSTGRES=1 go test ./internal/controller/ -run
  TunnelGrantsPgxStore`. It creates the 0084 and 0085 tunnel tables itself in a rolled-back scratch
  schema, because the vendored Lake schema changes only after Lake merges those migrations.

Live acceptance on `example-nonprod` (deferred to the rollout, after Lake's 0084/0085 are
deployed there): a granted developer reaches the target with `zetta-tunnel client` and nothing else
(another port, another service, and the endpoint directly are refused); after revocation and after
expiry the client is refused; the relay's audit lines show every session.
