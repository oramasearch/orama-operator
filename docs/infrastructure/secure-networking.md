# Secure networking: design

The design for [#437](https://github.com/oramasearch/orama-operator/issues/437) (the operator's
half) and [lake#724](https://github.com/oramasearch/lake/issues/724) (Lake's half). It follows the
research in [`docs/research/secure-networking/`](../research/secure-networking/README.md).

**Status: stage 1 and the operator half of stage 2 declared connections implemented and native-tested,
2026-10-14.** `NetworkSandbox` now protects Lake instances, Lake workspaces and Orama Cloud v1
stacks (#465), selecting only owner pods in shared namespaces such as `orama-system`. Workspaces
retain namespace-wide scope. Cloud v1 writer and reader public egress excludes private and cluster
destinations after resolution (#466). The 2026-10-14 live AKS probe found metadata bypassing
NetworkPolicy exclusions; every sandbox now also renders an owner-scoped Cilium `egressDeny` for
cloud metadata and host-service addresses. The observation, mandatory addresses and rollout
verification boundary are documented in [Network sandboxes](network-sandbox.md#mandatory-cloud-metadata-deny).
Only the operator manager may write network policy or `NetworkSandbox`; tenant, Lake, app, agent
and build identities are denied (#467). The design decisions are recorded in section 9.
After Lake migration `0084_network` installs `network_guard`, one `Connection` is materialized per
desired row and supplies paired sandbox-owned policy; the blanket workspace path is retired only
after every initial row is Ready (#468). See [Declared workspace connections](connections.md).
A connection with a credential scope gets its own Postgres role with exactly those grants, written
as a workspace secret, rotated by a new generation and dropped with its sessions on revocation
(#469). See [Credentials on connections](connections.md#credentials-on-connections).
An `EgressRule` lets a workspace reach one outside destination: a range through generated policy,
a DNS name only through zetta-tunnel's egress gateway, which Cilium leaves as the source's only
path; `internetEgress` is now the operator's own rule with the same policy (#470). See
[Egress rules](egress-rules.md).
Stage 4 is implemented and native-tested (#471): Lake tunnel grants become `Tunnel`s carried by
zetta-tunnel's relay and a per-grant endpoint, or by a customer's own Cloudflare account; status returns
`endpoint` and `relay_ca_pem` (section 11.1). See [Developer tunnels](tunnels.md).

---

## 1. What is asked

Steve, 2026-10-13: *"I want to add secure networking to lake with istio in the operator ... (and
tunnels so that I can connect apps and services) ... let's make sure that our workspaces are locked
down sandboxes ... and that all network and dns connections are explicit .... app to service
connections might need credentials (roles) too ... they might also want to connect apps to external
data connections ... create the appropriate navigation sections ... (we might also need peer
connections between k8s clusters or zetta clusters or aws vpcs) ... keep every tunnel abstract"*.

Added since:

| Date | Decision (Steve) |
|---|---|
| 2026-10-13 | *"this also needs to be permissible licensed for us to resell"* |
| 2026-10-13 | *"our instinct is to implement our own c++ components"* |
| 2026-10-13 | Orama Cloud v1's engine outbound traffic is locked down with this system, not a per-stack NetworkPolicy (from the review of #409). |
| 2026-10-13 | This epic is ready before the Orama Cloud v1 cutover and the Lake release (Friday 23 October 2026, 19:00 PT); work starts once the review of the #397–#449 stack is done. |
| 2026-10-14 | Cross-cluster connections, including Cloud v1's US readers of the EU writer (#396), go through this system, never ad-hoc public load balancers (from the review of #449). |
| 2026-10-14 | Everything runs self-contained, on-prem and air-gapped; the only outside services are Clerk, Cloudflare and, later, Resend. |
| 2026-10-14 | Orama's own tunnel is a new private C++26 project, [`oramasearch/zetta-tunnel`](https://github.com/oramasearch/zetta-tunnel). |
| 2026-10-14 | It needs Lake UI changes and probably Zetta changes too. |
| 2026-10-14 | **The mesh (D7) and the ingress (D8) are zetta-tunnel too**: *"d7 and d8 are zetta-tunnel too"*. |
| 2026-10-14 | **D1–D6 as proposed** (*"yes do all d1-6"*), recorded in section 9. |

## 2. Where things stand (checked 2026-10-14)

- **CNI:** Azure CNI overlay with the AKS-managed Cilium dataplane (`cilium-distroless:v1.18.14`) and
  Cilium network policy, on every cluster. `CiliumNetworkPolicy` and
  `CiliumClusterwideNetworkPolicy` are installed.
- **ACNS is off.** Azure's Advanced Container Networking Services (`advancedNetworking.enabled:
  false` on `orama-production-cluster-01`) is what AKS requires for **FQDN (DNS name) egress
  filtering** and for Hubble flow logs. Without it, policy is by address and port only.
- **Lake workspace namespaces are locked down today**, by NetworkPolicies generated from their
  operator-owned `NetworkSandbox`
  (`lake_workspaces.go`, `lake_workspace_datastore_policies.go`, #268): `default-deny` (ingress and
  egress), `allow-workspace-internal`, `allow-dns-egress`, `allow-lake-ingress` and
  `allow-lake-egress`, `allow-internet-https` when the workspace's `internetEgress` is on (port 443
  to `0.0.0.0/0` except private, link-local, node and pod ranges), one ingress policy per datastore
  operator, one API-server egress policy per operand that needs it, and one access policy per app.
- **Before #465, nothing else was locked down.** The operator now creates a `NetworkSandbox` for
  every Lake instance and Orama Cloud v1 namespace. This implementation has native test coverage
  but has not yet been rolled out and connectivity-probed on the named production clusters.
- **In the deployed pre-stage-2 state, everything inside a workspace reaches everything**
  (`allow-workspace-internal`). The implemented Connection rollout retains that only before
  `network_guard` and while initial rows become Ready; the named nonprod probe remains pending.
- **Ingress** is ingress-nginx (retired upstream), with no TLS to the origin (#457).
- **No mesh, no tunnels, no peering.** Every AKS VNet uses `10.224.0.0/12`, so clusters cannot be
  peered by routing.

## 3. Principles

1. **Abstract kinds, concrete providers** (principle 10): a sandbox, a connection, a tunnel and a
   peer connection each have one shape; what enforces or carries them is a provider behind it.
2. **Enforce with what every cluster already runs first.** Cilium is the enforcer. The operator
   generates its policies; no second eBPF dataplane goes on AKS-managed Cilium's hooks. (Both 2026-10-14
   reports.)
3. **Orama's own components where they are the product**: the tunnel (zetta-tunnel), the credential
   broker and its protocol-aware endpoints, the peer gateway. In C++, permissively licensed, and
   able to run air-gapped.
4. **One operator, no differences; two installation paths, one result; never by hand.** Any chart
   this adds goes in `helmfile.yaml` and every `platform.yaml` together.
5. **Licences**: every component permissive and recorded where it is added. Hosted services are
   providers only where their terms allow (Cloudflare Tunnel in a customer's own account, never
   resold from ours).
6. **Where data lives**: a workspace's connections, grants, tunnel sessions and network activity are
   workspace data, in the workspace Zetta. The operator reads only Lake's Postgres control plane
   (decision 51), so what it must enforce is desired state there, as services and workspaces are
   today. The exact split is decision D1 below.

## 4. The resource kinds

Names to settle (decision D2). Each is namespaced unless said otherwise, written by Lake through the
operator (or by the operator itself for Orama's own stacks), and reconciled by the operator.

| Kind | What it says | Provider |
|---|---|---|
| `NetworkSandbox` | Owner-selected pods are default-deny in and out; only fully owned workspaces select the whole namespace. Names what the platform must still reach (its operators, the API server, DNS, Lake). One per workspace, per Lake instance and per Orama Cloud v1 stack. | Kubernetes `NetworkPolicy` and `CiliumNetworkPolicy`, generated |
| `Connection` | **One** source workload may reach **one** destination (a service, an app, a stack component) on named ports. Optionally carries a credential scope (decision D4). | Generated policy on both sides; a credential through the broker |
| `EgressRule` | A workload may reach an outside destination: CIDR and port, or a DNS name and port. | CIDR: generated policy. DNS name: zetta-tunnel's egress gateway (D3) |
| `Tunnel` | A person may reach one target from outside the cluster, for a time. | **zetta-tunnel** (`ZettaTunnel`); optionally a customer's own Cloudflare account (`CloudflareTunnel`) |

zetta-tunnel is also the mesh and the ingress (D7, D8); their kinds are designed with them.
| `PeerConnection` | Another cluster or network may reach named services here, and the reverse, through gateways, never by routing addresses. | zetta-tunnel's peer gateway (D6) |

What the existing objects become: the workspace's NetworkPolicies are generated from its
`NetworkSandbox`; an app's access policy and today's blanket `allow-workspace-internal` are replaced by
`Connection`s; `internetEgress` becomes `EgressRule`s.

## 5. What each part uses

### Sandbox and connections (enforcement)

- **Default-deny on every namespace the operator creates**, not only workspaces: every Lake
  instance namespace and every Orama Cloud v1 namespace gets its `NetworkSandbox`. Each allowance the
  stack needs today is written down explicitly (the engine's broker, the dashboard's database, the
  ingress controller, the operators, the API server, DNS).
- **Orama Cloud v1's engine**: its writer and readers may reach their own broker, DNS, and the public
  internet **except** private, link-local (including the cloud metadata address `169.254.169.254`),
  carrier-grade NAT, node and pod ranges, enforced on the resolved address. That is an address rule,
  so it needs no ACNS. It closes the gap that OramaCore's host-name deny list leaves (review of #409).
- **Inside a workspace**, `allow-workspace-internal` is replaced by declared `Connection`s.
- **Tenants cannot write policy.** Kubernetes policies are additive, so an allow written by a tenant
  would reopen anything. Only the operator manager's ClusterRole writes `NetworkPolicy`,
  `CiliumNetworkPolicy` or the secure-networking kinds. A repository-wide test reads every checked-in
  Role and ClusterRole, treats wildcard rules as grants, follows bindings, and refuses any other
  writer. Native RBAC envtest also impersonates the workspace default ServiceAccount, attempts an
  allow-all policy and proves create, update, patch and delete are denied (#467).

### DNS-name egress (decision D3)

`EgressRule` by DNS name needs one of:
- **ACNS** on every AKS cluster (paid, per node; Azure only). Cilium then enforces `toFQDNs` with a
  DNS rule. It does not help an on-prem or air-gapped install that is not AKS.
- **Our own egress gateway**: workloads with name rules route that traffic through a gateway that
  resolves and authorizes by name, and Cilium allows only the gateway. Portable to every install;
  it is a component we build and run.

### Credentials on connections (decision D4)

A `Connection` with a credential scope gets a credential issued for that one app, scoped to what it
needs (for example read-only, one schema), and revocable:
- **Native roles where the datastore has them**: Postgres roles (CloudNativePG), Kafka users and ACLs
  (Strimzi), Valkey ACL users, ClickHouse users. The operator creates the role and delivers the
  credential as a projection of a workspace secret (the two-levels-of-secrets rule). Postgres is
  implemented (#469): [Credentials on connections](connections.md#credentials-on-connections).
- **A protocol-aware endpoint only where needed.** Injecting a credential is impossible when TLS is
  end to end (Postgres, MySQL/TiDB, Kafka, Valkey and ClickHouse each negotiate their own), so a
  broker endpoint terminates and re-establishes TLS per protocol, which moves the TLS trust boundary.
  Postgres first.
- **Zetta** keeps its node-to-node mTLS end to end. Scoped access for apps to a workspace Zetta is
  Zetta work (section 7).

### Tunnels

- **zetta-tunnel** is the provider Orama owns. Its transport is decision D5.
- A developer authenticates as themselves; the tunnel reaches **one** target; Lake grants, shows,
  expires and revokes it, and audits who connected. The tunnel endpoint's path into the workspace is
  a generated `Connection`.
- **Cloudflare Tunnel only in a customer's own Cloudflare account**: Cloudflare's terms forbid
  reselling it from ours (research, 2026-10-14).

### Peer connections

- **Service-addressed gateways** at each end authenticate each other and expose only named services.
  The clusters' VNets overlap, so nothing routes pod or VPC addresses across.
- **Cloud v1's US readers of the EU writer** (#396) are the first user, after a new reader can be
  seeded from an existing one (#458).
- Built in C++ (decision D6): on Corosio and OpenSSL, as Zetta's node-to-node links are, or on
  zetta-tunnel's transport.

### A mesh (D7, decided: zetta-tunnel)

Transparent mTLS between workloads, and the identity each connection carries, is zetta-tunnel's
(Steve, 2026-10-14). It is not needed for stage 1 (sandboxes and connections are enforced by Cilium)
and comes after the tunnel itself. What it must get right, from the research:
- **It is a security boundary that must not fail open**: traffic that escapes capture escapes the
  policy. Capture, enrolment races, restarts and the proxy's own traffic need tests that prove it
  fails closed. Istio ambient's CNI plugin and ztunnel are the benchmark to meet.
- **It sits beside AKS-managed Cilium**, which stays the enforcer and cannot be reconfigured freely.
  Redirection happens inside each pod's network namespace through a platform-owned node agent; the
  workloads stay under restricted Pod Security with no `NET_ADMIN`.
- **Cilium policy stays** as defence in depth behind it.

### Ingress (D8, decided: zetta-tunnel)

zetta-tunnel replaces ingress-nginx (retired upstream) as the way traffic enters a cluster (Steve,
2026-10-14). It terminates TLS with the certificates cert-manager issues (#457) and routes each host
to its Service. Until it does, ingress-nginx stays, and #457 puts TLS on it first.

### Observability

Pixie (#456) for live tracing; network flows and policy verdicts from Hubble need ACNS on AKS, so the
portable record is our own: the tunnel, the broker endpoints and the gateways each log who connected
to what, into the workspace Zetta.

## 6. Lake's part (lake#724)

- **Capabilities first**, each also in the REST API and the MCP server: list and declare a
  workspace's connections; grant, list and revoke tunnels; declare egress; see a workspace's network
  rules and recent activity.
- **Navigation:** a network section per workspace, using the research's proposal (section F, §26) as
  the starting point: overview, connections, developer access (tunnels), private networks (peers),
  DNS and egress, policies, activity.
- **Data:** connection records, grants, tunnel sessions and activity in the workspace Zetta; the
  desired state the operator enforces in the control plane (decision D1).
- Lake names a tunnel, never a vendor.

## 7. Zetta's part (to be filed when the design is agreed)

- **Scoped access for apps** to a workspace Zetta, so a `Connection` to Zetta carries a credential
  limited to what the app needs.
- **The store** for connections, grants, tunnel sessions and network activity in each workspace.
- **Zetta-to-Zetta links between clusters**, if peering covers Zetta clusters ("peer connections
  between k8s clusters or zetta clusters").
- **Reuse:** Zetta's transport (Corosio, mTLS 1.3, join tokens) as a building block for the peer
  gateway and possibly zetta-tunnel.

## 8. Order of work

Each stage ends with its tests passing on `orama-nonprod-cluster-01`, then the production clusters,
through the operator, with nothing by hand.

1. **Sandbox everything the operator creates.** `NetworkSandbox` for every Lake instance, workspace
   and Orama Cloud v1 namespace is implemented in #465. Cloud v1 engine destination narrowing is
   implemented in #466; tenant policy RBAC is #467. Live allowed/forbidden path probes remain before rollout.
2. **Declared connections inside workspaces**, replacing `allow-workspace-internal`, with Lake's
   capabilities and pages. Credentials on connections, Postgres first.
3. **Egress by DNS name** (after D3).
4. **zetta-tunnel**: developer tunnels, with Lake's grants and audit.
5. **Peer connections**: service-addressed gateways; Cloud v1's US readers once #458 is done.
6. **zetta-tunnel as the ingress** (D8), replacing ingress-nginx, and **as the mesh** (D7).

## 9. Decisions

All decided 2026-10-14 by Steve: D1–D6 by taking the proposals (*"yes do all d1-6"*), D7 and D8
directly (*"d7 and d8 are zetta-tunnel too"*).

| # | Decision | Decided |
|---|---|---|
| D1 | Where a connection's desired state lives | **Lake's Postgres control plane** holds what the operator enforces (connections, egress rules, tunnel grants, peer connections), as for services; the operator reads it there (decision 51). **The workspace Zetta** holds the records people see: grants, tunnel sessions, network activity. |
| D2 | The kinds' names | **`NetworkSandbox`, `Connection`, `EgressRule`, `Tunnel`, `PeerConnection`**, with providers `ZettaTunnel` (and `CloudflareTunnel` in a customer's own account) |
| D3 | Egress by DNS name | **Our own egress gateway, in zetta-tunnel**: workloads with name rules reach the outside through it; it resolves and authorizes by name; Cilium allows only the gateway. Portable to every install; no ACNS needed. |
| D4 | Credentials on connections | **Native datastore roles first** (Postgres roles, Kafka users and ACLs, Valkey ACL users, ClickHouse users), delivered as projections of workspace secrets; a protocol-aware endpoint only where a role cannot express the scope. **Postgres first.** |
| D5 | zetta-tunnel's transport | **TLS 1.3 over Corosio**, as Zetta's node-to-node links (mutual TLS, OpenSSL, C++26), so the network layer and Zetta share one stack. QUIC (MsQuic, MIT) is a later option behind the same interface; kernel WireGuard is not used. |
| D6 | Peer gateways | **zetta-tunnel**, on the same transport: service-addressed gateways at each end. |
| D7 | The mesh | **zetta-tunnel** |
| D8 | Ingress after ingress-nginx | **zetta-tunnel** |

## 10. Issues

Filed 2026-10-14; the list is kept on the epics: #437 (operator, zetta-tunnel, Zetta) and
[lake#724](https://github.com/oramasearch/lake/issues/724) (Lake).

**Operator (stage)**

- https://github.com/oramasearch/orama-operator/issues/465: NetworkSandbox: default-deny for every namespace the operator creates (stage 1)
- https://github.com/oramasearch/orama-operator/issues/466: Orama Cloud v1 engine: outbound only to its broker, DNS and the public internet (stage 1, implemented)
- https://github.com/oramasearch/orama-operator/issues/467: Tenants cannot write network policy (stage 1)
- https://github.com/oramasearch/orama-operator/issues/468: Connection: declared connections between workloads, replacing allow-workspace-internal (stage 2, implemented and native-tested; nonprod probe pending)
- https://github.com/oramasearch/orama-operator/issues/469: Credentials on connections: native datastore roles, Postgres first (stage 2, Postgres implemented and tested against a real server; nonprod probe pending)
- https://github.com/oramasearch/orama-operator/issues/470: EgressRule: outbound by address and by DNS name (stage 3, implemented and tested against the zetta-tunnel binary; nonprod probe pending)
- https://github.com/oramasearch/orama-operator/issues/471: Tunnel: the kind, with ZettaTunnel and a customer's own CloudflareTunnel as providers (stage 4, implemented and native-tested; nonprod acceptance pending)
- https://github.com/oramasearch/orama-operator/issues/472: PeerConnection: named services between clusters through zetta-tunnel gateways (stage 5)
- https://github.com/oramasearch/orama-operator/issues/473: Ingress through zetta-tunnel, replacing ingress-nginx (stage 6)
- https://github.com/oramasearch/orama-operator/issues/474: Mesh: zetta-tunnel's node agent on every cluster (stage 6)

**zetta-tunnel**

- https://github.com/oramasearch/zetta-tunnel/issues/2: Transport: mutual TLS 1.3 over Corosio, shared with Zetta
- https://github.com/oramasearch/zetta-tunnel/issues/3: Developer tunnel: client, relay and in-cluster endpoint
- https://github.com/oramasearch/zetta-tunnel/issues/4: Egress gateway: outbound by DNS name
- https://github.com/oramasearch/zetta-tunnel/issues/5: Peer gateway: named services between clusters
- https://github.com/oramasearch/zetta-tunnel/issues/6: Ingress gateway, replacing ingress-nginx
- https://github.com/oramasearch/zetta-tunnel/issues/7: Mesh: per-node agent with mutual TLS between workloads, failing closed
- https://github.com/oramasearch/zetta-tunnel/issues/8: Image and release: private Docker Hub, built on the cluster

**Zetta**

- https://github.com/oramasearch/zetta/issues/1280: Scoped access for apps: a credential per connection to a workspace Zetta
- https://github.com/oramasearch/zetta/issues/1281: Store the network records of a workspace: connections, grants, tunnel sessions, activity
- https://github.com/oramasearch/zetta/issues/1282: Zetta-to-Zetta links between clusters through peer connections
- https://github.com/oramasearch/zetta/issues/1283: Share the Corosio transport with zetta-tunnel

**Lake**

- https://github.com/oramasearch/lake/issues/727: Control plane: connections, egress rules, tunnel grants and peers as desired state
- https://github.com/oramasearch/lake/issues/728: Capabilities: connections, egress, tunnels, peers and network rules in REST and MCP
- https://github.com/oramasearch/lake/issues/729: A Network section in each workspace
- https://github.com/oramasearch/lake/issues/730: Developer access: grant, expire, revoke and audit tunnels
- https://github.com/oramasearch/lake/issues/731: Scoped credentials on an app's connection

## 11. The contract between Lake and the operator

Decision D1 puts the desired state in Lake's Postgres control plane. This is the contract both sides
build against, in the pattern of `services` and `service_status`: Lake writes the desired rows; the
operator reads them, reconciles, and writes only the `*_status` rows. Lake's migration
**`0084_network`** creates the tables and the guard `network_guard`; the operator vendors the schema
(`internal/controller/assets/lake-schema.sql`) after Lake merges it, and reads nothing until the guard
exists. Desired writes `NOTIFY lake_network`.

```sql
-- One declared path: one source workload may reach one target on named ports (Connection).
CREATE TABLE IF NOT EXISTS network_connections (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id      uuid        NOT NULL,
    workspace_id     uuid        NOT NULL REFERENCES workspaces(id),
    source_kind      text        NOT NULL CHECK (source_kind IN ('app', 'service', 'agent', 'tunnel')),
    source_id        uuid        NOT NULL,
    target_kind      text        NOT NULL CHECK (target_kind IN ('service', 'app')),
    target_id        uuid        NOT NULL,
    ports            integer[]   NOT NULL DEFAULT '{}',   -- empty: the target's own service ports
    credential_scope jsonb,                               -- NULL: no credential. {"access": "read"|"write", "schema": "<name>"?}
    desired          text        NOT NULL DEFAULT 'present' CHECK (desired IN ('present', 'absent')),
    generation       bigint      NOT NULL DEFAULT 1,
    created_by       uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS network_connection_status (
    connection_id       uuid        PRIMARY KEY REFERENCES network_connections(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',  -- Pending | Ready | Failed | Deleted
    message             text        NOT NULL DEFAULT '',
    credential_secret   text        NOT NULL DEFAULT '',  -- workspace secret name holding the scoped credential, if any
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Outbound from a workload (or the whole workspace) by CIDR or by DNS name (EgressRule).
CREATE TABLE IF NOT EXISTS network_egress_rules (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id  uuid        NOT NULL,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id),
    source_kind  text        CHECK (source_kind IN ('app', 'service', 'agent')),  -- NULL: every workload in the workspace
    source_id    uuid,
    cidr         cidr,
    dns_name     text,
    ports        integer[]   NOT NULL,
    desired      text        NOT NULL DEFAULT 'present' CHECK (desired IN ('present', 'absent')),
    generation   bigint      NOT NULL DEFAULT 1,
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((cidr IS NULL) <> (dns_name IS NULL)),
    CHECK ((source_kind IS NULL) = (source_id IS NULL))
);
CREATE TABLE IF NOT EXISTS network_egress_rule_status (
    rule_id             uuid        PRIMARY KEY REFERENCES network_egress_rules(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- One person may reach one target from outside the cluster until expires_at (Tunnel).
CREATE TABLE IF NOT EXISTS network_tunnel_grants (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id  uuid        NOT NULL,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id),
    user_id      uuid        NOT NULL,
    target_kind  text        NOT NULL CHECK (target_kind IN ('service', 'app')),
    target_id    uuid        NOT NULL,
    port         integer     NOT NULL,
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    generation   bigint      NOT NULL DEFAULT 1,
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS network_tunnel_grant_status (
    grant_id            uuid        PRIMARY KEY REFERENCES network_tunnel_grants(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',  -- Pending | Ready | Expired | Revoked | Failed
    message             text        NOT NULL DEFAULT '',
    endpoint            text        NOT NULL DEFAULT '',  -- what the developer's client connects to
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Named services exchanged with another cluster or network through gateways (PeerConnection).
CREATE TABLE IF NOT EXISTS network_peers (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id  uuid        NOT NULL,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id),
    name         text        NOT NULL,
    remote       text        NOT NULL,                    -- the remote gateway's address
    exports      jsonb       NOT NULL DEFAULT '[]',       -- [{"target_kind","target_id","port"}]
    imports      jsonb       NOT NULL DEFAULT '[]',       -- [{"name","port"}]
    desired      text        NOT NULL DEFAULT 'present' CHECK (desired IN ('present', 'absent')),
    generation   bigint      NOT NULL DEFAULT 1,
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS network_peer_status (
    peer_id             uuid        PRIMARY KEY REFERENCES network_peers(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS network_guard (applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migrations (version) VALUES ('0084_network') ON CONFLICT DO NOTHING;
```

- **Ids are Lake's.** `source_id` and `target_id` are an app's, service's or agent's id in Lake's own
  tables; a tunnel source is the `network_tunnel_grants` row. The operator maps each to the
  workload's pods by the labels it already sets on them.
- **Records people see** (who changed what, tunnel sessions, egress decisions, activity) go to the
  workspace Zetta (zetta#1281), not these tables.
- **Existing behaviour on rollout**: when the guard first exists, Lake writes a `network_connections`
  row for every path an app relies on today (each app to each service in its workspace), so nothing
  that works today breaks. **Internet egress is the operator's**: `internetEgress` lives only in the
  `Lake` resource's `spec.workspaces.overrides`, so Lake writes no egress rows for it; the operator
  keeps today's port-443 rule for those workspaces as an implicit egress rule (corrected 2026-10-14,
  found by the lake#727 work).
- **Removing a row**: `network_connections`, `network_egress_rules` and `network_peers` are removed by
  setting `desired = 'absent'` (the operator deletes what it made, then writes phase `Deleted`). A
  tunnel grant has no `desired` column: it ends by `revoked_at` being set or `expires_at` passing.

### 11.1 Tunnel credentials (added 2026-10-14, from the lake#730 work)

Lake's migration **`0085_network_tunnel_ca`** adds two columns; `0084_network` stays as it is.

```sql
ALTER TABLE network_tunnel_grants       ADD COLUMN IF NOT EXISTS client_ca_pem text NOT NULL DEFAULT '';  -- Lake: the CA that signs this grant's client certificates
ALTER TABLE network_tunnel_grant_status ADD COLUMN IF NOT EXISTS relay_ca_pem  text NOT NULL DEFAULT '';  -- operator: the CA of the relay's server certificate
INSERT INTO schema_migrations (version) VALUES ('0085_network_tunnel_ca') ON CONFLICT DO NOTHING;
```

- **The client CA is per customer**, from Lake's existing PKI (one CA per customer). Lake writes its public certificate into `client_ca_pem` when it creates the grant. The relay trusts the union of `client_ca_pem` of the live grants, and checks the grant id in the certificate as before.
- **The developer's credential**: the grant's own user (and only that user) calls a Lake capability with a certificate signing request (PKCS#10, generated on their machine). Lake returns the signed client certificate (SAN `urn:orama:tunnel-grant:<id>`, ending no later than `expires_at`, at most 12 hours), the relay's address (`network_tunnel_grant_status.endpoint`) and the relay's CA (`relay_ca_pem`). The private key never leaves the developer's machine. Administrators can grant and revoke, never fetch someone else's credential.
- **The operator** writes `endpoint` and `relay_ca_pem` in the grant's status when the relay serves it.
