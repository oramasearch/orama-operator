# Egress rules: outbound by address and by DNS name

[`EgressRule`](../crd-reference.md#egressrule-separate-crd-namespaced--one-outside-destination)
implements stage 3 of the [secure-networking design](secure-networking.md) for
[operator#470](https://github.com/oramasearch/orama-operator/issues/470). A rule lets the workloads
of one workspace reach one outside destination: an address range and ports, or a DNS name and
ports. Everything else outbound stays denied by the workspace's `NetworkSandbox`. There is no ACNS:
a name is enforced by Orama's own egress gateway (decision D3), and Cilium enforces addresses.

## Desired state and ownership

Lake owns desired state in its control Postgres. Migration `0084_network` creates
`network_egress_rules` and `network_egress_rule_status`; Lake writes desired rows and the operator
writes only status rows. The operator reads nothing until `network_guard` exists. The Lake
reconciler's control-row poll (woken by `LISTEN lake_network`) materializes one Lake-controlled
`EgressRule/egress-<row UUID>` in the workspace namespace per row, and deletes the CR once the row
is `absent` and reported `Deleted`. A hard-removed row's orphan CR is deleted; its finalizer removes
what it rendered.

| Row | `EgressRule` |
|---|---|
| `source_kind`, `source_id` | `spec.sourceKind`, `spec.sourceId`; both null means every workload in the workspace |
| `cidr` | `spec.cidr` |
| `dns_name` | `spec.dnsName`, lower case, one final dot removed |
| `ports` | `spec.ports`, sorted and distinct, 1 to 64 TCP ports |
| `desired`, `generation` | `spec.desired`, `spec.generation`, acknowledged in `observed_generation` |

A row is `Failed` without a CR when its ids are not canonical UUIDs, both or neither of `cidr` and
`dns_name` are set, the source is half set, `dns_name` is an IP literal, a wildcard, a single label
or not a host name, a `cidr` has host bits set, or the ports are out of range.

Sources resolve to pods exactly as for [Connections](connections.md#workload-identity-and-ports):
an app through its `LakeApp` identity, a service and an agent through their operator-owned labels.
Every allowance below is added to the workspace `NetworkSandbox`, which renders and owns the
`NetworkPolicy` objects and keeps its mandatory metadata deny.

## By address

A CIDR rule is one allowance, `egress-<id>`: egress from the source pods to the range on the
rule's TCP ports. The range never includes what no rule may reach: loopback and link-local (the
cloud metadata address included) are excepted always, and so is every cluster address inside the
range (the Kubernetes API Service and its endpoints, every node address, every node's pod range).
A range that lies wholly inside one of those is `Failed`. A CIDR rule is also how an IP literal is
allowed: nothing else opens a direct path to an address.

## By DNS name

A name rule never opens a path to its destination. Its three allowances are:

| Allowance | Selects | Allows |
|---|---|---|
| `egress-<id>` | the source pods | egress only to the workspace's egress gateway pods, on 7445 |
| `egress-<id>-gateway-ingress` | the gateway pods | ingress only from the source pods, on 7445 |
| `egress-<id>-gateway-egress` | the gateway pods | egress to public addresses on the rule's ports (private, shared, link-local, multicast, reserved and cluster ranges excepted) |

**The gateway** is zetta-tunnel's `egress` role ([its contract](https://github.com/oramasearch/zetta-tunnel/blob/issue-8-image/docs/egress.md)),
image `oramasearch/zetta-tunnel:main-9cd11b4` pulled with the Lake's `pullSecretName`. The operator
runs one Deployment `zetta-tunnel-egress` per workspace that has at least one name rule, and removes
it with the last one:

```text
zetta-tunnel egress --rules /etc/zetta-tunnel/egress/rules.json --listen 0.0.0.0:7445
  --health-listen 0.0.0.0:8080 --sni-port 443 --audit -
```

It runs as 65532, non-root, read-only root file system, every capability dropped, no service
account token, no environment. Readiness is `/readyz` and liveness `/healthz` on 8080, which no
policy admits, so only the kubelet reaches them. Decision records go to standard output as JSON
lines (writing them to the workspace Zetta waits for zetta#1281). The Service `zetta-tunnel-egress`
exposes 443 (TLS with the destination's SNI) and 7445 (the explicit `ZTEG1 <name> <port>` preface),
both to the gateway's 7445.

**The rule snapshot** is the ConfigMap `zetta-tunnel-egress`, key `rules.json`, mounted at
`/etc/zetta-tunnel/egress`. Each present name rule becomes one entry: its Lake id, the workspace
namespace, the current IP addresses of its running source pods (the gateway excluded), its name and
ports. The kubelet replaces the mounted file atomically; the operator also annotates the gateway
pods with the ConfigMap version so the kubelet refreshes it promptly. The gateway reloads within two
seconds, and a removed rule closes its sessions. The EgressRule controller watches pods, so a new
or moved source pod is added to the snapshot.

The gateway refuses a snapshot in which two entries authorize the same source address, name and
port. Where two rules name the same host, the earlier one (workspace-wide first, then by id) keeps
the address; a later rule that covers the same ports gives it up, and a later rule that covers only
some of them is `Failed` and names the rule it overlaps. A rule with more than 256 running source
pods is `Failed` (the gateway's limit). A rule with no running source pods yet is not in the
snapshot and authorizes nobody.

**What the gateway enforces**, by its own tests and by
`TestEgressGatewayAgainstZettaTunnel` here: it authorizes the numeric source address, the exact
name and the port before it forwards a byte; it resolves the name itself and connects to an
address from that lookup; a name not allowed is refused before DNS, even when it resolves to the
same address as an allowed name; an IP literal is always refused (only a CIDR rule allows an
address).

**How a workload uses it.** HTTPS clients connect to `zetta-tunnel-egress.<namespace>.svc` on 443
and send their ordinary ClientHello with the destination's name as SNI (for example
`curl --connect-to api.example.com:443:zetta-tunnel-egress.<ns>.svc:443 https://api.example.com/`);
TLS stays end to end. Other TCP clients connect on 7445 and send `ZTEG1 <name> <port>\n` first. A
`Ready` rule's message names the address. Pointing a workload's own DNS lookups at the gateway
without client configuration is not part of this change.

## internetEgress is an EgressRule

A workspace's `spec.workspaces.overrides[<id>].internetEgress` lives only in the `Lake` resource, so
Lake writes no row for it. The workspace reconciler keeps one operator-owned rule for it,
`EgressRule/internet-https`, with `publicInternet: true`, port 443, no source and no Lake rule id.
Its controller renders the allowance under its original name `allow-internet-https`, with exactly
the policy it always had (public TCP 443 for every workspace pod, with the private, metadata, API,
pod and node exclusions); `TestWorkspaceNetworkSandboxPreservesLegacyPolicyNamesAndSpecs` and
`TestInternetEgressRuleRendersTheLegacyPolicyAndLeavesWithTheOverride` compare it with the legacy
policy. The workspace reconciler writes the rule before it rebuilds the sandbox and keeps the
rendered allowance across rebuilds, so the path never closes during the change. Turning the override
off drops the allowance in that same pass and deletes the rule.

A workspace with internetEgress on already reaches every public address on 443 directly, so name
rules on 443 add nothing there; they matter for workspaces without it and for other ports.

## A Lake Zetta's own allowlist (defense in depth)

A Zetta node denies every outbound connection except what `ZETTA_EGRESS_ALLOW` lists (zetta
[#75](https://github.com/oramasearch/zetta/issues/75), zetta's `docs/governance/egress-policy.md`).
The operator renders that variable for every Zetta a Lake owns from what its namespace's
`NetworkSandbox` allows it ([#724](https://github.com/oramasearch/orama-operator/issues/724)), so
the engine refuses the destinations the network policy refuses: two independent layers, one
declaration. The sandbox stays the source of truth. The list is read from the allowances the sandbox
carries, never kept as a second list, so a rule or Connection that is `Pending` or `Failed` adds
nothing (`internal/controller/lake_zetta_egress.go`).

| What the sandbox carries | Entries for a Zetta whose Lake service id is `S` |
|---|---|
| A name rule with no source or source `service S` | `<dnsName>:<port>` per port |
| A CIDR rule with no source or source `service S` | `<cidr>:<port>` per port (IPv6 bracketed), with link-local cut out (169.254.0.0/16, the cloud metadata address included, and fe80::/10) |
| `internet-https` (the workspace's `internetEgress`) | `*:443` |
| A Connection from `service S` | `<service>.<namespace>.svc.cluster.local:<port>` for each target Service port whose pod port the Connection opens (every Service port when it declares none; a named target port is left out when it declares some) |
| `allow-workspace-internal` (before every initial Connection is enforced) | `*.<namespace>.svc.cluster.local` |

A rule for another source adds nothing. Cluster DNS, the Lake API and the other nodes of the Zetta's
own cluster are not listed: a Zetta never dials them through its HTTP client, and peer links are not
egress. The Lake's own data-plane Zetta (`spec.zetta`) gets an empty list (loopback only), because
the Lake namespace allows its pods nothing outside but DNS and their own cluster;
`TestTheLakeDataPlaneZettaIsAllowedNothingOutside` fails when that changes.

**Where the engine is wider than the policy.** Its syntax has no exceptions. `*:443` also admits
in-cluster names on 443, and a CIDR entry keeps the pod, node and API server addresses the policy
cuts out. Those addresses change as nodes come and go, and every change to the variable rolls the
StatefulSet, so the policy alone keeps excluding them.

**Name rules go through the gateway, the engine does not know it.** The policy lets a name rule's
source reach only the egress gateway. The engine dials the destination name directly and has no
gateway client, so the entry states the destination the rule allows. It takes effect when the engine
routes named destinations through the gateway
([zetta#1331](https://github.com/oramasearch/zetta/issues/1331)).

**Changes roll the Zetta.** The variable is in the pod template, so adding or removing a rule or
Connection for a Zetta restarts its pods on the next workspace or services poll (up to 60 seconds
after the sandbox changes). A file the engine re-reads would avoid that and needs an engine change
([zetta#1332](https://github.com/oramasearch/zetta/issues/1332)). The list is sorted and
de-duplicated, so an unchanged sandbox never rolls anything. When the namespace cannot be read, the
running value is kept; a list longer than 64 KiB (beyond what one environment string can safely hold)
becomes deny and is logged. A spec that names `ZETTA_EGRESS_ALLOW` (`spec.zetta.env`,
`spec.services.zettaEnv`) keeps its own value.

## A workspace Zetta's approved connector destinations

Separately from egress, a Zetta dials a **private or loopback connector source** (a Postgres, a
MySQL-protocol TiDB, or one of lake's SQLite, DuckDB and Polars services) only when
`ZETTA_CONNECTOR_ALLOW_DESTINATIONS` names it as an exact `host:port`, comma-separated. Metadata and
link-local addresses are refused whatever it says (zetta
[#1469](https://github.com/oramasearch/zetta/pull/1469), LSR-241,
[zetta#1414](https://github.com/oramasearch/zetta/issues/1414)). Without the variable, a workspace's
in-cluster sources stop working once a Zetta image with that change rolls.

The operator renders it on every managed Zetta service and every workspace Zetta
([#882](https://github.com/oramasearch/orama-operator/issues/882),
`internal/controller/lake_zetta_connector_destinations.go`). It is in the `OramaZettaCluster`'s
`spec.env`, so every pod of the cluster carries it, including each node that takes part in a
distributed fetch. The entries come only from the Lake services the operator reconciles in that
Zetta's workspace namespace:

| Lake service in the namespace | Entry |
|---|---|
| `postgres` (CloudNativePG `Cluster` `<name>`) | `<name>-rw.<namespace>.svc.cluster.local:5432` |
| `tidb` (TiDB Operator `TidbCluster` `<name>`) | `<name>-tidb.<namespace>.svc.cluster.local:4000` |
| `sqlite`, `duckdb`, `polars` (`StatefulSet` `<name>`) | `<name>.<namespace>.svc.cluster.local:8080` |

Each entry is the authority of the endpoint lake hands the workspace in that service's credential,
so the connector dials exactly what is approved. An object counts only when the Lake controls it,
it carries the Lake's `orama.cloud/lake` and `orama.cloud/service-kind` labels, and it is not being
deleted. A kind whose upstream operator is not installed has no entries. There are no wildcards and
no address ranges, and a namespace never lists another namespace's services.

- **No sources, empty value.** A namespace with none of these services gets the variable with an
  empty value. That approves no private destination, the same as the engine's default, and keeps the
  variable visible in the spec, as `ZETTA_EGRESS_ALLOW` is.
- **The operator's alone.** A `ZETTA_CONNECTOR_ALLOW_DESTINATIONS` stated in
  `spec.services.zettaEnv` is dropped. There is no flag, chart value or per-cluster setting, so every
  cluster renders the same value from the same resources.
- **It follows the services.** A service added or removed changes the value on the next workspace
  or services poll (up to 60 seconds), which rolls the Zetta's pods. The list is sorted and
  de-duplicated, so unchanged services never roll anything. When the namespace cannot be read, the
  running value is kept.
- **The network policy still applies.** Approval lets the engine dial; the namespace's
  `NetworkSandbox` still decides whether the pod can reach the service.
- The Lake's own data-plane Zetta (`spec.zetta`) is no workspace Zetta and gets no value.

Tests: `TestWorkspaceZettaApprovesExactlyItsOwnServices`, `TestAWorkspaceWithNoSourcesApprovesNothing`,
`TestWorkspaceZettaConnectorDestinationsFollowItsServices` and
`TestConnectorDestinationsKeepTheCurrentValueWhenUnreadable`.

## Status

`network_egress_rule_status` receives only real transitions with the Lake generation:
`Pending` (sandbox, source or gateway not ready), `Ready`, `Failed` (invalid, unreachable range, or
an overlap) or `Deleted`. Absent rows remove every allowance and re-render the gateway snapshot in
one reconcile before `Deleted`. Ready rules are re-rendered every five minutes so changed node, pod
range and API addresses are excepted again.

## Verification boundary

Native tests cover the source-only-reaches-the-gateway policy, the gateway's exact arguments,
image, security context and Service, the snapshot contents, removal in one reconcile, a workspace's
last name rule removing the gateway, overlap resolution, CIDR exceptions and the refused metadata
range, the internetEgress conversion against the legacy policy, Lake row intake and refusal, and
nothing read before `network_guard`. The opt-in `TestEgressRulesPgxStoreAgainstPostgres` creates the
0084 egress tables itself. The opt-in `TestEgressGatewayAgainstZettaTunnel` runs the real
zetta-tunnel binary (commit 933d6fc, the image's) with the operator's arguments and snapshot.

The named acceptance probe on `orama-nonprod-cluster-01` is deferred to rollout: a workload with a
name rule reaches the name through the gateway, is refused another name (including one resolving to
the same address) and an IP literal, and reaches an address directly only with a CIDR rule.
