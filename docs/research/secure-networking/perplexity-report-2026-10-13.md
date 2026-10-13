<img src="https://r2cdn.perplexity.ai/pplx-full-logo-primary-dark%402x.png" style="height:64px;margin-right:32px"/>

# Secure Networking for Lake

**Assessment date: October 13, 2026.** The recommended design is a portable, layered architecture: **upstream Istio ambient installed by Lake through pinned Helm releases; Cilium/Kubernetes policy as the non-bypassable L3/L4 boundary; dedicated egress gateways; and provider-neutral tunnel and peer-connection APIs.** Do not treat Istio’s registry, DNS proxy, Cloudflare Access, or a service mesh alone as a sandbox boundary.

A critical qualification is that **AKS-managed Cilium is not operationally equivalent to self-managed Cilium**. Before choosing ambient for production, Lake must validate Microsoft-supported Cilium configuration, CNI chaining, BPF masquerading, health probes, DNS policy, and upgrade behavior on the exact AKS SKU and version.

## Version baseline

| Component | Baseline for this report | Release date/status | Qualification |
| :-- | --: | :-- | :-- |
| Istio | 1.31.x; 1.31.1 is the latest patch directly evidenced in the current upstream upgrade documentation | Current stable minor in September–October 2026 | Confirm the patch tag in the Istio release feed immediately before pinning; the available primary material establishes 1.31.1 but did not independently exclude a newer 1.31 patch. Istio normally supports the current and previous minor, making upgrades a recurring platform operation rather than an annual event. [^1_1][^1_2] |
| Kubernetes Gateway API | 1.5.0 | Stable standard channel | The Istio 1.31 installation and upgrade material identifies 1.5.0 as the supported current release. Experimental CRDs remain separate from the standard bundle. [^1_3][^1_4] |
| Cilium upstream | 1.20.2 | Stable documentation published September 14, 2026 | AKS chooses and manages its own Cilium build; Lake cannot assume every upstream 1.20 option is exposed or supported by Microsoft. [^1_5][^1_6] |
| `cloudflared` | 2026.10.0 | Released October 5, 2026 | Cloudflare recommends current releases; some integrations require at least 2025.7.0. [^1_7][^1_8] |
| CloudNativePG | 1.30.x | 1.30.0 released June 28, 2026 | 1.30 introduced the standalone `DatabaseRole` CRD and optional operator-managed client certificates. [^1_9] |
| Strimzi | 0.50.0 | Current stable documentation found | `KafkaUser` remains the declarative user and ACL interface. [^1_10] |
| Kubernetes | 1.35 | Lake-provided cluster baseline | Verify each Istio patch’s tested Kubernetes range before every cluster upgrade. |

Material changes in the past year include ambient DNS capture becoming enabled by default in Istio 1.29, optional network policies for Istio components, Cloudflare hostname-based private routing becoming GA, removal of Cloudflare’s CIDR-encoded route API endpoints on October 5, 2026, and CloudNativePG’s new `DatabaseRole` CRD.[^1_11][^1_12][^1_9][^1_13]

## Executive summary

### Recommended architecture

- **Network boundary:** Cilium-enforced default-deny ingress and egress in every workspace. This is the principal anti-bypass boundary.
- **Workload identity:** one Kubernetes service account per Lake application, deployment, agent class, or service identity—not a shared workspace service account.
- **Mesh:** upstream Istio ambient, installed using pinned Helm charts. Use ztunnel for transparent mTLS and identity-aware L4 authorization; add waypoints only where HTTP policy, controlled egress, or richer telemetry justifies them.
- **Authorization:** namespace-wide empty `ALLOW` policies for default deny, followed by generated per-connection `AuthorizationPolicy` resources matching source SPIFFE principal and destination port.
- **Egress:** selected workload may reach DNS plus a dedicated egress gateway; the gateway may reach only declared destinations. Use Istio `ServiceEntry` and a waypoint/egress gateway for routing and identity policy, with Cilium FQDN or CIDR policy as the enforcement backstop.
- **DNS:** Cilium DNS-aware policy when supported by the managed provider; otherwise allow only cluster DNS and rely on the egress gateway for destination enforcement. Block external port 53, DNS over TLS, known DoH endpoints, and generic HTTPS except through approved egress.
- **Developer tunnels:** initially use one remotely managed Cloudflare tunnel per cluster or trust zone, with two or more `cloudflared` replicas, rather than one tunnel per workspace or connection. Each Lake `Tunnel` becomes a Cloudflare route plus Access application/policy.
- **Credential binding:** make credential lifecycle part of `Connection`, not part of `Tunnel`. Create a distinct database/Kafka principal per connection and project it only into the authorized consumer.
- **Peering:** keep peer networking outside Istio’s abstraction. Model routing, CIDRs, ownership, DNS, HA, and authorization separately, then optionally layer Istio multicluster over the resulting reachability.


### Decisions that matter

| Decision | Recommendation | Rejected alternative | Why |
| :-- | :-- | :-- | :-- |
| Mesh distribution | Upstream Istio Helm charts owned by Lake | AKS Istio add-on | The AKS add-on is Microsoft-supported but does not support ambient and couples versions and configuration to AKS. It cannot provide identical lifecycle behavior on EKS, GKE, or other providers. [^1_14] |
| Data plane | Ambient first, with workload/namespace waypoints only when needed | Sidecar everywhere | Ambient removes per-pod proxy lifecycle and privilege concerns, keeps L4 identity and mTLS, and fits mixed stateful workloads better. Retain a supported sidecar escape hatch for uncovered ambient features. |
| Security boundary | Cilium L3/L4 policy plus Istio identity policy | `REGISTRY_ONLY` and `ServiceEntry` alone | Istio explicitly warns that sidecar egress controls can be bypassed and are not a secure egress boundary. NetworkPolicy must prevent a workload from dialing arbitrary IPs around the proxy. [^1_15] |
| Tunnel granularity | Shared HA connector pool, per-resource route and Access policy | One tunnel Deployment per connection | A Cloudflare tunnel is a connector/failure-domain object, not an authorization object. Cloudflare supports multiple replicas and routes per tunnel; per-connection tunnels create token, pod, API-quota, and upgrade sprawl without improving resource-level authorization. [^1_16][^1_17] |

## Compatibility matrix

“Works” means the architecture is supported by the relevant upstream design. It does **not** mean that every Lake datastore version has passed an interoperability test.


| Target | Istio sidecar | Istio ambient | Required action or evidence |
| :-- | :-- | :-- | :-- |
| AKS Azure CNI overlay with managed Cilium | **Works with configuration; provider support boundary unclear for upstream Istio** | **Works with configuration; must certify on AKS** | Cilium socket-level service translation can bypass a sidecar’s view of the original ClusterIP; upstream Cilium prescribes `socketLB.hostNamespaceOnly=true`. Ambient requires CNI coexistence and must not use incompatible BPF masquerading. [^1_5][^1_18][^1_19] |
| Cilium NetworkPolicy | **Works** | **Works with ambient exceptions** | Ambient never bypasses CNI policy, but HBONE/health-probe paths require explicit policy handling, including port 15008 and Istio’s link-local probe addresses. [^1_20] |
| `restricted` Pod Security | **Works with Istio CNI** | **Works for application namespaces** | Without Istio CNI, the injected init container requires `NET_ADMIN` and `NET_RAW`. `istio-cni` itself requires a privileged system namespace and host-path access; workload pods can remain restricted. [^1_21][^1_22] |
| CloudNativePG | **Expected with explicit TCP ports; test required** | **Expected at L4; test required** | PostgreSQL client, replication, and operator paths are TCP-compatible. Preserve CNPG TLS and test STARTTLS, replication, backup, probes, switchover, and operator reconciliation. No product-specific Istio support statement was verified. |
| Strimzi/Kafka | **Known configuration sensitivity** | **Expected at L4; test required** | Explicitly declare Kafka ports as TCP, allow brokers, controllers, replication, Entity Operator, Cruise Control, and metrics paths. Avoid HTTP protocol inference. Kafka ACLs remain independent of mesh authorization. [^1_23][^1_10] |
| K8ssandra/Cassandra | **Expected with configuration; unverified** | **Expected at L4; unverified** | Allow CQL, internode storage, TLS-internode, gossip, JMX/management, repair, and seed discovery. Test StatefulSet bootstrap and replacement under default deny. |
| Altinity ClickHouse operator | **Expected with configuration; unverified** | **Expected with configuration; unverified** | ClickHouse HTTP and native TCP require different port declarations. Verify whether the operator directly queries ClickHouse services and permit that identity if it does. |
| TiDB operator | **Expected with configuration; unverified** | **Expected with configuration; unverified** | PD, TiKV, TiDB/MySQL, peer replication, status and monitoring ports must be modeled separately. Test bootstrap and scale operations. |
| Valkey operator | **Expected with configuration; unverified** | **Expected at L4; unverified** | Model client, replication, Sentinel/cluster-bus and operator paths separately. Redis/Valkey protocol remains opaque to L4 authorization. |
| Cloudflare Tunnel | **Works; normally keep connector out of sidecar interception** | **Works; place connector in an infrastructure namespace** | `cloudflared` needs outbound TCP/UDP 7844 plus DNS and optional HTTPS. Route its origin connection through an Istio ingress/internal gateway if mesh identity enforcement is desired. [^1_24] |

For all datastore rows marked “test required” or “unverified,” no authoritative compatibility certification from the datastore operator was established. These must become release-gating conformance tests rather than assumptions.

# A. Istio today

## 1. Release and ambient

Istio 1.31.x is the current stable line evidenced by the October 2026 documentation. Revision examples use 1.31.1; Lake should resolve the latest signed patch at build time, record its digest, and promote that immutable digest through environments rather than following `latest`.[^1_1][^1_2]

Ambient is no longer an experimental architecture: ztunnel provides transparent HBONE mTLS, workload identity, telemetry and L4 authorization, while waypoint proxies provide destination-oriented L7 processing. Ambient’s node agent holds separate X.509 identities for each node-local service account and uses SPIFFE identities over HBONE.[^1_25]

Operational differences remain:


| Capability | Sidecar | Ambient ztunnel | Ambient waypoint |
| :-- | :-- | :-- | :-- |
| Transparent mTLS | Yes | Yes | Inherits ztunnel/HBONE |
| Source service-account identity | Yes | Yes | Yes |
| TCP source-principal/port authorization | Yes | Yes | Yes |
| HTTP method/path/header policy | Yes | No | Yes |
| HTTP traffic shifting/retries/faults | Yes | No | Yes |
| Per-workload proxy | Yes | No | No; waypoint is shared |
| Per-pod proxy resource cost | Yes | No | Only waypoint cost |
| Failure coupling | Pod-local | Node-local ztunnel | Shared by waypoint scope |
| Mature feature coverage | Broadest | Mature core L4 | Still has feature gaps relative to sidecars |

Treat ambient L4 as the default and maintain a `dataPlaneMode: Sidecar` override for protocols or features not demonstrated in Lake’s conformance suite. Avoid advertising complete sidecar parity.

## 2. AKS and Cilium

Upstream Istio reports testing ambient on AKS, EKS and GKE with their supplied CNIs and with third-party Cilium. That establishes upstream interoperability intent, not a Microsoft support promise for every upstream Helm configuration.[^1_19]

Three interactions matter:

- **CNI coexistence:** upstream Istio’s ambient prerequisites say Cilium must not remove other CNI configurations; self-managed Cilium uses `cni.exclusive=false`.[^1_18]
- **Socket load balancing:** Cilium socket LB can rewrite a Service VIP to a backend Pod IP before an Istio sidecar sees it. Cilium documents `socketLB.hostNamespaceOnly=true`, corresponding to `bpf-lb-sock-hostns-only=true`, to preserve service-mesh interception in pod namespaces.[^1_5][^1_26]
- **BPF masquerading and probes:** Istio documents Cilium BPF masquerading as incompatible with ambient’s link-local health-probe mechanism, and default-deny Cilium policy requires explicit probe exceptions.[^1_20][^1_18]

For ambient, do not assume the sidecar-era socket-LB setting is either required or harmless. Upstream discussion has conflicting observations about direct-to-pod traffic and ambient feature behavior. Test all of the following with the actual AKS-managed image:[^1_27]

1. Service VIP and direct Pod-IP calls.
2. Same-node and cross-node paths.
3. ztunnel restart and Cilium restart.
4. Headless services and StatefulSet DNS.
5. NetworkPolicy enforcement before and after ambient enrollment.
6. Readiness/liveness probes.
7. FQDN policy and DNS capture.
8. Node upgrades and CNI sequencing.

Whether Lake may change `cni.exclusive`, BPF masquerading, or `bpf-lb-sock-hostns-only` on AKS-managed Cilium without leaving Microsoft’s supported configuration was not established. Make this a formal Microsoft support question. If required settings are immutable or unsupported, sidecars or ambient on managed Cilium should remain a preview feature, or Lake should use a CNI configuration that it can own on future providers.

## 3. AKS add-on or Helm

The AKS Istio add-on is a tested and Microsoft-supported AKS integration, but Microsoft states that it does not support ambient mode. It also follows Azure’s offered revisions and upgrade controls rather than a platform-defined cross-cloud release train.[^1_14]

Use upstream Helm for Lake because:

- Lake needs one install model across AKS, EKS, GKE and later providers.
- Lake needs ambient, CNI, ztunnel and waypoint configuration.
- Lake’s operator needs deterministic version pinning, rendered-resource inspection, canary control planes and provider-independent status.
- Add-on-specific support does not transfer to non-Azure clusters.

The trade-off is ownership: Lake assumes responsibility for compatibility qualification, CVE patching, CNI sequencing and support escalation. Maintain provider-specific `MeshProvider` capabilities so a future customer may choose a cloud-supported mesh distribution, but make upstream Istio the first concrete provider.

## 4. Restricted Pod Security

Application namespaces can enforce `restricted` when Istio CNI performs traffic redirection. Without it, the `istio-init` container requires `NET_ADMIN` and `NET_RAW`, which violates even the baseline Pod Security profile.[^1_21]

The privilege moves to the infrastructure plane:

- `istio-cni-node` is a node DaemonSet.
- It requires host-path access to CNI directories.
- Its namespace must permit privileged system components.
- Ambient requires Istio CNI; sidecar mode strongly benefits from it.
- Application pods need a compliant pod-level `seccompProfile`, normally `RuntimeDefault`.[^1_28][^1_21]

Therefore, keep `istio-system`, Cilium, gateways and tunnel connectors out of customer workspaces. Workspace admission should prohibit disabling mesh enrollment, excluding ports/IP ranges, using `hostNetwork`, adding capabilities, overriding DNS, or selecting an unauthorized service account.

## 5. Installation lifecycle

The in-cluster Istio operator was deprecated in 1.23 and removed in 1.24. Helm and `istioctl` remain supported; Lake should not build against the removed in-cluster controller.[^1_29][^1_30]

Recommended install order:

1. Gateway API 1.5.0 standard CRDs.
2. Istio `base` chart and Istio CRDs.
3. Revisioned `istiod`.
4. Istio CNI with ambient profile.
5. ztunnel.
6. ingress, egress and waypoint gateways.
7. mesh-wide `PeerAuthentication`, baseline authorization and infrastructure NetworkPolicies.
8. controlled workspace enrollment.
9. workload restart/reconciliation where interception state requires it.

For upgrades, update CRDs first, install a canary `istiod` revision, move test namespaces through a stable revision tag, then upgrade CNI and ztunnel in place. ztunnel and Istio CNI are cluster-wide DaemonSets and do not provide the same per-workload revision isolation as `istiod`; this is the highest-risk part of ambient upgrades.[^1_31][^1_32][^1_1]

The Lake operator should use Helm as a library or render/apply pinned charts through server-side apply. Do not shell out to a long-running in-cluster Istio operator. Run `istioctl x precheck`, `istioctl analyze`, synthetic connectivity tests and rollback gates around each rollout.[^1_1]

## 6. Gateway API

Use Gateway API for:

- Public and private ingress: `Gateway`, `HTTPRoute`, `GRPCRoute`, `TCPRoute` or `TLSRoute`.
- Ambient waypoints: `Gateway` with `gatewayClassName: istio-waypoint`.
- Cross-namespace route attachment: `ReferenceGrant`.
- Egress gateway deployment where the supported Istio pattern covers the protocol.

Istio intends Gateway API to become its default traffic API, but states that it does not yet cover its entire feature set.[^1_33]

Retain Istio APIs where no portable Gateway API equivalent exists:


| Istio resource | Continued use |
| :-- | :-- |
| `ServiceEntry` | Register external DNS names, IPs, ports or non-Kubernetes services. [^1_34] |
| `DestinationRule` | TLS origination, subsets and connection-pool behavior not represented portably. |
| `Sidecar` | Sidecar-specific registry scope and capture configuration; avoid in ambient abstractions. |
| `PeerAuthentication` | Require mesh mTLS. |
| `AuthorizationPolicy` | Workload identity and L4/L7 authorization. |
| `VirtualService` | Only when a required routing feature is absent from Gateway API. |
| Istio `Gateway` | Legacy or feature-specific deployments; do not use for new portable APIs without need. |

# B. Workspace lockdown

## 7. Default-deny design

Use two independent enforcement layers:

### Cilium layer

Each workspace receives:

- Default-deny ingress.
- Default-deny egress.
- Allow DNS only to the approved resolver path.
- Allow kubelet health probes as required.
- Allow source/destination selectors generated from active `Connection` objects.
- Allow egress only to the Lake egress gateway or explicit provider-managed destinations.
- No general internet, metadata service, node, control-plane or host-network access.

Kubernetes NetworkPolicy controls L3/L4 traffic and is enforced outside Istio. Ambient respects it and cannot bypass it.[^1_20]

### Istio layer

Each workspace receives:

- Namespace ambient enrollment.
- `PeerAuthentication` in `STRICT`.
- Empty/default-deny `AuthorizationPolicy`.
- Per-connection allow policies selecting the destination and matching source principals.
- Optional destination waypoint.
- No tenant ability to modify Istio policy or mesh labels.

Istio’s default is permissive unless an applicable `ALLOW` policy exists, so the explicit default-deny object is mandatory. `STRICT` rejects plaintext but does not itself decide which authenticated identity may connect.[^1_35][^1_36][^1_37][^1_38]

### Identity model

Assign a dedicated service account to every independently authorized Lake workload. The practical identity becomes:

```text
spiffe://cluster.local/ns/<workspace>/sa/<lake-workload-id>
```

Do not authorize solely by namespace or mutable application labels. Labels select the server to which a policy applies; the source principal should be the authenticated service account. Istio supports principal, namespace and destination-port attributes for TCP authorization.[^1_39][^1_37]

For multicluster operation, define a stable trust-domain strategy before launch. Include cluster identity in trust-domain aliases or use distinct trust domains where cluster provenance must remain visible.

## 8. Explicit egress

| Mechanism | What it provides | Bypass characteristics |
| :-- | :-- | :-- |
| `REGISTRY_ONLY` | Rejects destinations unknown to Istio’s registry | Not a security boundary. Istio warns malicious workloads can bypass the sidecar. [^1_15] |
| `ServiceEntry` | Registry metadata, DNS resolution, protocol and optional waypoint attachment | An allow/routing declaration, not an anti-bypass firewall. [^1_34] |
| Sidecar egress gateway | Central policy, logging, TLS origination and stable source | Workload may dial around it unless CNI policy blocks direct external egress. |
| Ambient egress waypoint | Destination-oriented L7 gateway associated with a `ServiceEntry` | Requires CNI backstop and correct DNS/IP capture. [^1_40] |
| Cilium `toFQDNs` | Converts observed DNS answers into permitted destination IPs | Stronger L3 enforcement, but shared/CDN IPs and alternate resolution paths remain concerns. [^1_41] |
| Cilium CIDR policy | Non-bypassable destination IP and port enforcement | Operationally difficult for dynamic SaaS/CDN addresses. |
| Azure ACNS FQDN filtering | Managed AKS implementation of Cilium FQDN policy | Requires ACNS; without it AKS blocks or does not provide the FQDN/L7 capability. [^1_42][^1_43] |

The secure pattern is:

1. Workload can reach approved DNS and the egress gateway.
2. Workload cannot directly reach public CIDRs.
3. Egress gateway can reach only the destination set generated from active connections.
4. `ServiceEntry` represents the external host.
5. Istio authorization limits which SPIFFE identities can use that `ServiceEntry`/gateway.
6. Azure NAT Gateway or equivalent gives the egress subnet a stable public IP.

For HTTPS and opaque TCP, the gateway can reliably enforce destination IP/port and often SNI, but not application objects such as an S3 bucket unless it terminates or originates TLS and understands the protocol. Bucket-, schema- or account-level authorization belongs in credentials and the external service.

## 9. Explicit DNS

Cilium DNS policy is the most realistic per-workload DNS-name enforcement available in this stack. It intercepts DNS responses per endpoint, requires an L7 DNS rule, observes the returned addresses, and permits those addresses for matching `toFQDNs` rules while respecting TTLs.[^1_41][^1_44]

On AKS this requires ACNS security. Microsoft requires a DNS `rules.dns.matchPattern` section with `toFQDNs`; merely permitting port 53 is insufficient because the agent would not learn the FQDN-to-IP association.[^1_43]

Istio DNS capture serves a different primary purpose: service discovery and synthetic addresses, especially for ambient egress. It forwards queries it cannot answer and is not a general per-workload DNS authorization system. Ambient DNS capture became default in Istio 1.29.[^1_45][^1_11]

Recommended policy:

- Allow UDP/TCP 53 only to the approved cluster or node-local DNS destination.
- Use exact Cilium DNS match names where practical; avoid broad wildcards.
- Block direct port 53 and 853 to all other IPs.
- Block arbitrary internet HTTPS so that a workload cannot use an unapproved DoH server.
- Explicitly allow approved SaaS/API FQDNs through the egress path.
- Prohibit `hostNetwork`, custom `dnsPolicy: None`, privileged pods, raw sockets and injected egress proxies.
- Treat DNS answers as discovery input, not proof of destination ownership.

Per-namespace CoreDNS views can hide names but do not prevent direct IP connections. They also add stateful configuration and caching complexity. Use them only for namespace-specific naming semantics, not as the primary firewall.

NodeLocal DNSCache changes the destination seen by policy. Upstream Cilium documents a Local Redirect Policy pattern; AKS LocalDNS has its own compatibility conditions and requires explicit Cilium policy to its local addresses. Choose exactly one supported node-local DNS architecture and test it with both Istio DNS capture and FQDN policy.[^1_46][^1_47]

“Every DNS resolution is explicit” cannot be guaranteed for fully hostile code while permitting unrestricted TLS egress. A workload can embed IPs, implement DNS over HTTPS, tunnel name resolution through an allowed API, or use a shared CDN address. The enforceable promise is narrower: **Lake blocks standard DNS queries and network destinations not represented by an active connection, subject to approved shared-destination and application-layer exceptions.**

## 10. Third-party operators

Controllers normally reconcile Kubernetes API objects and do not require a blanket network path into every managed pod. Admission webhooks receive API-server traffic in the operator namespace, not from customer workloads. Do not solve operator compatibility by globally allowing operator namespaces into every workspace.

Classify each path:

- Kubernetes API reconciliation.
- Admission webhook.
- Operator-to-managed-service control connection.
- Sidecar/agent inside the database pod.
- Peer or replication traffic.
- Metrics and backup traffic.
- User client traffic.

Generate separate policy for each. Operator service accounts should never inherit customer application access.

Required conformance cases include:


| System | Paths to test |
| :-- | :-- |
| CloudNativePG | Postgres client, primary/replica streaming, instance manager, probes, failover, backup/restore, pooler, operator reconciliation, client/server TLS |
| Strimzi | Client/broker, broker replication, KRaft controllers, Entity/User/Topic Operator, Cruise Control, exporters, TLS and SASL |
| K8ssandra | CQL, internode storage, TLS internode, gossip/seed discovery, Stargate, management API, repair and replacement |
| ClickHouse | Native TCP, HTTP, inter-server replication, Keeper, operator reconciliation and monitoring |
| TiDB | MySQL protocol, PD, TiKV peer traffic, status/API, scheduling, backup and monitoring |
| Valkey | Client, replication, Sentinel or cluster bus, failover controller and metrics |

Headless Services and direct pod DNS deserve dedicated tests because StatefulSets frequently connect to Pod IPs rather than a stable ClusterIP. Use explicit named ports and `appProtocol` where available, but declare database wire protocols as `TCP` unless Istio explicitly supports their L7 parsing.

Do not disable mesh capture for an entire datastore as the first workaround. Prefer a narrowly documented port exclusion only when conformance proves that transparent tunnelling breaks the protocol, and pair every exclusion with Cilium policy because excluded traffic loses Istio identity enforcement.

## 11. Non-HTTP protocols

Postgres, Kafka, Cassandra CQL, MySQL wire and Redis/Valkey all traverse Istio as TCP streams. Mesh mTLS wraps the stream between proxies; it does not replace database TLS, users, roles or ACLs.

Application TLS—including PostgreSQL SSL negotiation/STARTTLS—can remain inside the mesh mTLS tunnel. This produces double encryption but preserves end-to-end database authentication. Problems usually arise from incorrect protocol detection rather than from TLS nesting, so use explicitly named TCP ports and avoid HTTP classification.

L4 authorization can express:

- Source SPIFFE principal.
- Source namespace.
- Source IP where identity is unavailable.
- Destination workload.
- Destination TCP port.

It cannot express:

- Postgres database, schema, table or SQL operation.
- Kafka topic or consumer group.
- Cassandra keyspace/table.
- MySQL database or statement.
- Redis key prefix or command.

Those are credential and datastore-authorization concerns. Istio’s ambient L4 policy explicitly supports principal, namespace, IP and destination port, with richer HTTP attributes requiring a waypoint.[^1_37][^1_48]

# C. Connections and credentials

## 12. Credential-carrying connections

Model a connection as a binding between a consumer identity, endpoint and service-native authorization object:

```text
Connection =
  network authorization
  + service identity
  + service-native role/ACL
  + credential projection
  + rotation/revocation lifecycle
  + audit status
```


### PostgreSQL

CloudNativePG 1.30’s `DatabaseRole` is an excellent concrete target. It gives each role its own lifecycle and status and can generate and renew a PostgreSQL client certificate. Where 1.30 is unavailable, `.spec.managed.roles` can reconcile role presence, password Secret and `validUntil`.[^1_49][^1_50][^1_9]

Lake still needs a grants controller or SQL job to apply schema/table permissions because role creation does not by itself define all object grants. Prefer:

- One login role per connection.
- One reusable non-login capability role per permission profile.
- Grant capability role to login role.
- SCRAM or client certificate.
- Short `VALID UNTIL` where application behavior permits.
- `ensure: absent` and immediate session revocation on deletion.


### Kafka

Create one Strimzi `KafkaUser` per connection and express topic, consumer-group, cluster and transactional-ID ACLs. Strimzi’s simple authorization maps `KafkaUser` ACLs into Kafka’s authorizer.[^1_23][^1_10]

### Other datastores

Define a provider-specific `CredentialProfile` implementation. Do not pretend there is a portable “read-only” meaning across Cassandra, Kafka, Redis and SQL databases. The provider-neutral API can name an intent such as `ReadOnly`, but status must expose the exact concrete grants produced.

### Token choices

- Use projected Kubernetes service-account tokens for Kubernetes and workload-identity exchanges.
- Use Istio certificates only for mesh peer identity; applications should not read or reuse proxy private keys.
- Use SPIRE only if Lake needs identities outside Istio/Kubernetes or needs direct application consumption of SVIDs. Adding SPIRE solely to duplicate Istio workload identity increases CA and rotation complexity.
- Prefer short-lived external credentials when the destination supports federation.
- Otherwise rotate a per-connection Secret and trigger an application rollout or hot reload.

The Service Binding specification is a useful projection convention, but its repository describes it as a working draft rather than a universal mature control plane. Crossplane’s old composite-resource connection-details feature was removed in v2; current guidance is to compose an explicit Secret, which reinforces making credential output an explicit Lake resource rather than relying on implicit framework behavior.[^1_51][^1_52][^1_53]

Heroku’s attachment model is worth copying conceptually: a credential is a database role with specific privileges, attached to applications and rotatable across attachments. Avoid copying the practice of placing a complete credential URL in a broadly visible environment variable.[^1_54]

## 13. External data connections

An `ExternalConnection` should create:

- Destination descriptor: DNS names, CIDRs, ports and protocol.
- Cilium/NetworkPolicy path from consumer to egress gateway.
- `ServiceEntry`.
- Egress waypoint or gateway route.
- Destination-specific TLS policy.
- Credential projection.
- Audit and flow labels.
- Stable egress identity.

For stable public source IP, put egress gateway pods on a dedicated node pool/subnet whose outbound traffic uses Azure NAT Gateway with one or more reserved public IPs. On AWS use NAT Gateway or an egress appliance with Elastic IP; on GCP use Cloud NAT. Expose these addresses in status as `status.egressAddresses`.

A shared gateway may multiplex destinations, but use multiple gateway pools when customers require different allow-listed IPs, compliance boundaries or failure domains. The provider-neutral field should be `egressIdentityRef`, not `azureNatGatewayId`.

# D. Cloudflare Tunnel

## 14. Current model

`cloudflared` 2026.10.0 was released October 5, 2026.[^1_7]

Use remotely managed tunnels. Cloudflare stores their configuration centrally and exposes API operations to create tunnels and update `/cfd_tunnel/{id}/configurations`; local tunnels keep YAML and credentials on the connector host. The API’s old `remote_config` flag is deprecated in favor of `config_src`.[^1_55][^1_56]

Run at least two replicas across nodes and zones. Cloudflare supports up to 100 connections, described as 25 replicas, per tunnel; all replicas serve the same routes.[^1_16]

One tunnel per workspace is technically possible but usually the wrong unit. Prefer:

- One tunnel per cluster and security/failure domain.
- Separate tunnel for regulated tenants or dedicated egress identity.
- Multiple route/application objects per tunnel.
- Per-resource Access policies.
- At least two connector replicas.

Cloudflare Access is free for teams under 50 users; published pay-as-you-go pricing is \$7 per user per month billed annually. Contract limits, application counts, log retention and support—not tunnel bandwidth alone—will become the relevant commercial constraints at Lake scale.[^1_57]

## 15. Developer TCP access

| Method | Client requirement | Protocols | Authorization model | Lake use |
| :-- | :-- | :-- | :-- | :-- |
| `cloudflared access tcp` | `cloudflared` CLI | Arbitrary TCP over WebSocket | Access application and IdP login | Good short-term developer experience; Cloudflare recommends Client-to-Tunnel for long-lived connections. [^1_58] |
| Cloudflare One Client/WARP | Installed and enrolled device | TCP and UDP private networking | Access private application and/or Gateway identity/device policy | Best general solution for Postgres, Kafka, Cassandra, MySQL and Redis. [^1_59][^1_60] |
| WARP Connector | Connector for routing networks, not the normal end-user client | Network routing | Gateway/network policy | Better for site/network connectivity than an individual developer tunnel. |
| Browser rendered | Browser only | SSH, RDP, VNC | Access login plus target credentials where applicable | Not suitable for database wire protocols. [^1_61] |
| Service token | Automated client supplies client ID/secret | Primarily Access-authenticated automation | Service-token rule | Long-lived credential unless Lake manages expiration and rotation; not a human developer credential. [^1_62] |
| Short-lived certificate | Cloudflare-supported SSH flow | SSH | IdP identity mapped to ephemeral SSH certificate | SSH-specific, not a general Postgres/Kafka certificate issuer. [^1_63][^1_64] |

For raw database protocols, WARP provides the cleanest experience because the native client dials the private hostname normally. `cloudflared access tcp` is useful as an initial opt-in method:

```bash
cloudflared access tcp \
  --hostname pg-tunnel.example.com \
  --url localhost:15432

psql postgres://user@localhost:15432/database
```

Cloudflare Access authenticates and authorizes the user at its edge. It does **not** automatically turn that human identity into an Istio SPIFFE identity or a Postgres role. The database still sees the origin connection from the tunnel-side connector or gateway, so Lake must issue a service credential or add a protocol-aware identity broker if per-human database attribution is required.

## 16. Running `cloudflared`

A normal `cloudflared` connector does not require `/dev/net/tun`, `NET_ADMIN`, privileged mode or host networking. It can run under `restricted` with:

- Non-root UID.
- Read-only root filesystem.
- Dropped capabilities.
- `seccompProfile: RuntimeDefault`.
- No service-account token unless Kubernetes discovery is required.
- Tunnel token from a narrowly scoped Secret.
- Two or more replicas.
- Pod disruption budget and topology spread.

Allow:

- DNS to the approved resolver.
- TCP and UDP 7844 to Cloudflare Tunnel edge addresses.
- Optional TCP 443 for documented ancillary features.
- Origin traffic only to the internal gateway or designated Services.

Cloudflare documents TCP/UDP 7844 for HTTP/2 and QUIC and publishes the edge IPs and SNI names. If UDP is blocked, HTTP/2 over TCP 7844 is available; if both protocols on 7844 are blocked, the tunnel fails.[^1_24][^1_65]

Deployment choices:


| Pattern | Recommendation |
| :-- | :-- |
| Sidecar in customer pod | Avoid. Couples tunnel and workload lifecycle, exposes tunnel credentials in the tenant pod boundary and multiplies connectors. |
| Per-workspace Deployment | Use only for strong tenant isolation requirements. |
| Shared cluster Deployment | Default; combine with an internal Istio Gateway and generated per-route authorization. |
| Dedicated per-customer connector pool | Offer for compliance, dedicated limits or customer-owned Cloudflare accounts. |

The clean ingress path is:

```text
Developer
  -> Cloudflare Access
  -> Cloudflare edge
  -> cloudflared connector
  -> private Istio Gateway
  -> destination waypoint/ztunnel
  -> workload
```

Give `cloudflared` a dedicated Kubernetes service account. Istio then authenticates that connector or gateway identity, not the individual developer. HTTP requests may carry validated identity headers/JWT, but raw TCP cannot carry equivalent headers. Do not claim end-to-end human identity for TCP unless Lake adds a protocol-aware broker.

## 17. Kubernetes integrations

No official Cloudflare production Kubernetes ingress or Gateway API controller was established in the primary Cloudflare material reviewed. Cloudflare provides APIs and deployment guidance, but the prominent controllers are community projects.

The STRRL ingress controller manages Cloudflare Tunnel from Kubernetes `Ingress` objects and has meaningful adoption, but it is not an official Cloudflare controller. Other community projects include Cloudflare tunnel operators and Gateway implementations.[^1_66][^1_67][^1_68]

For Lake, calling the Cloudflare API from the existing controller-runtime operator is the better fit because:

- Lake requires an abstract/provider/concrete object graph.
- Developer TCP tunnels are not naturally modeled by HTTP Ingress alone.
- Lake must coordinate Access applications, policies, DNS, connector deployments and workspace authorization.
- Ownership and deletion ordering must be explicit.
- Lake can minimize account API permissions and centralize retry/idempotency behavior.

Copy reconciliation and status patterns from mature operators, not their public API wholesale.

## 18. Tunnel alternatives

| Provider | Provider-neutral interpretation | Distinguishing provider fields |
| :-- | :-- | :-- |
| Cloudflare | Connector pool plus published/private route and identity policy | Account, zone, tunnel ID, public/private hostname, Access application, WARP routing |
| Tailscale | Tailnet service/subnet route advertised by proxy nodes | Tailnet, tags, grants, `ProxyGroup`, auth/OAuth credentials |
| Twingate | Remote network, connector pool and protected resource | Network, connector group, resource address and Twingate policy groups |
| ngrok | Agent endpoint/cloud endpoint with edge traffic policy | Domain, endpoint type, traffic policy, region |
| Teleport | Database/application proxy and role assignment | Teleport cluster, resource labels, database protocol, role mapping |
| Boundary | Target, host set, worker and credential broker | Scope/project, target type, worker filter, credential library |
| Azure Private Link | Provider-private endpoint to a published service | Subscription, resource ID, private endpoint, approval workflow and private DNS zone |
| AWS PrivateLink | Interface endpoint to an endpoint service | Account, region, endpoint service name, principals and acceptance |
| WireGuard | Peer keys and routed CIDRs | Public keys, endpoints, allowed IPs, keepalive and key rotation |

Tailscale has an official Kubernetes operator with `ProxyClass`, `ProxyGroup`, Service/Ingress exposure and tailnet grants. Its proxy pods generally need privileged access or at least `NET_ADMIN`, `NET_RAW` and `/dev/net/tun`, making it unsuitable for a customer `restricted` namespace but acceptable in a privileged infrastructure namespace.[^1_69][^1_70]

ngrok also has an official Kubernetes operator supporting Ingress, Gateway API and ngrok CRDs, with controller and agent components.[^1_71][^1_72][^1_73]

Provider-neutral `Tunnel` fields should include:

- `targetRef`.
- `mode`: `Published`, `PrivateNetwork`, `ClientProxy`, `SiteConnector`.
- `protocol`: `HTTP`, `HTTPS`, `TCP`, `UDP`, `SSH`, `RDP`.
- `port`.
- `hostname` or requested endpoint name.
- `authenticationRef`.
- `authorization`: subjects, groups, service identities, device requirements.
- `connectorPlacementRef`.
- `replicas` or HA class.
- `sessionPolicy`.
- `dnsPolicy`.
- `auditPolicy`.
- `providerRef`.

Do not put Cloudflare account IDs, Tailscale tags, WireGuard keys or PrivateLink service IDs in the abstract kind.

# E. Peering

## 19. Istio multicluster

Istio supports the established sidecar-era topologies:

- **Multi-primary:** each cluster has its own control plane.
- **Primary-remote:** a remote cluster uses a control plane in a primary cluster.
- **Single network:** pods are directly routable.
- **Multiple networks:** east-west gateways bridge otherwise disconnected networks.
- **Shared or federated trust:** service-account identities are validated across clusters.

For Lake, use multi-primary and multiple networks. A control-plane outage in one region should not disable configuration distribution in all leaf clusters.

Because Lake clusters reuse overlay CIDRs, do not attempt direct flat pod routing. Overlap makes a Pod IP ambiguous outside its cluster. Route cross-cluster service traffic through east-west gateways with distinct network names and cluster identities. Peer networking may provide gateway reachability, but must not advertise overlapping pod CIDRs into a common routing domain.

Ambient multicluster must remain **not production-approved** until the exact Istio 1.31 capability and limitations are validated. The available evidence did not establish feature parity for ambient multicluster, overlapping CIDRs, waypoint traversal and failover. Use one of these initial positions:

- Sidecar/east-west gateways for the few cross-cluster services.
- Application-native public rendezvous protected by TLS while private peering is built.
- Explicit regional gateways outside ambient enrollment.


## 20. Cilium Cluster Mesh

Cluster Mesh provides Cilium identity, service discovery and policy across clusters, but it requires control over Cilium configuration, cluster identity, certificate lifecycle and cross-cluster control-plane reachability.

Treat Cluster Mesh as unavailable for Lake’s AKS-managed Cilium unless Microsoft explicitly supports and exposes it. Even if technically possible, modifying managed Cilium components can put the cluster outside the managed service’s support boundary.

Cluster Mesh also does not erase overlapping-address constraints automatically. Service-routing modes and gateways must be selected so the same remote Pod IP cannot mean multiple endpoints. Do not make Lake’s portable connection API depend on Cluster Mesh.

## 21. AKS to customer AWS

| Method | Customer burden | Cost profile | Strengths | Weaknesses |
| :-- | :-- | :-- | :-- | :-- |
| Azure VPN Gateway plus AWS Site-to-Site VPN | High; customer configures AWS gateway/routes/BGP and coordinates shared parameters | Gateway-hour, tunnel and data-processing costs on both clouds | Standards-based, routed private connectivity | Long setup, overlapping CIDRs, route ownership and operational coordination |
| Azure Private Link / AWS PrivateLink bridge | Medium to high; service must be published through each cloud’s endpoint construct | Per endpoint-hour plus data | Least route exposure; service-specific | No native single object directly joins arbitrary Azure and AWS networks; generally needs an intermediary service/appliance |
| Cloudflare WAN / WARP Connector | Medium; deploy/configure connector and Cloudflare policy | Contract/SASE pricing | Identity and policy through shared provider; easier NAT traversal | Vendor dependency and potentially substantial recurring cost |
| Tailscale subnet router | Low to medium; customer deploys router and approves routes | Seat/plan dependent | Fast onboarding and good NAT traversal | `/dev/net/tun`/privilege, customer tailnet design and routing-policy dependence |
| Plain WireGuard | Medium; customer runs peer and routes | Low software cost; high operational cost | Portable and efficient | Lake owns key rotation, HA, discovery, routing and support |

Model them all as `PeerConnection` with:

- Local and remote endpoint owners.
- Exported and imported CIDRs/services.
- Overlap handling.
- Routing mode.
- Encryption/authentication.
- HA requirement.
- DNS zones.
- Approval state.
- Provider reference.
- Observed routes and health.

Provider-specific resources then express VPN BGP ASN, Cloudflare route IDs, Tailscale tags or WireGuard keys.

## 22. Distributed database peering

A private mesh or peer layer offers:

- No public listener.
- Mutual workload identity.
- Explicit cluster-to-cluster authorization.
- Central route and certificate lifecycle.
- Better flow telemetry.
- Optional stable private addressing and failover targets.

It does not reduce geographic propagation latency. An extra east-west gateway, tunnel edge or waypoint adds another queue, connection state and failure domain. Cross-region quorum protocols remain bounded by physical RTT and tail latency.

For Zetta:

- Keep application-level node identity and TLS even if the network is private.
- Prefer direct private regional peering or dedicated east-west gateways over a user-focused Zero Trust tunnel for latency-sensitive replication.
- Use the public rendezvous only for discovery, not bulk data, if possible.
- Benchmark p50/p95/p99 commit and recovery behavior under packet loss, gateway restart and region partition.
- Avoid cross-region synchronous quorum unless the product’s consistency requirements justify the latency.


# F. Lake API and product

## 23. Custom resources

### Object graph

```text
Mesh
  -> MeshProvider
     -> IstioMesh

NetworkSandbox
  -> NetworkSandboxProvider
     -> CiliumIstioSandbox

Connection
  -> ConnectionProvider
     -> IstioServiceConnection
     -> PostgresConnectionBinding
     -> KafkaConnectionBinding

ExternalConnection
  -> ExternalConnectionProvider
     -> IstioCiliumEgressConnection

Tunnel
  -> TunnelProvider
     -> CloudflareTunnel

PeerConnection
  -> PeerConnectionProvider
     -> AzureAwsVpnPeer
     -> CloudflareWanPeer
     -> TailscalePeer
     -> WireGuardPeer
```

Use conditions consistently:

- `Accepted`.
- `ProviderResolved`.
- `DependenciesReady`.
- `NetworkReady`.
- `CredentialReady`.
- `EndpointReady`.
- `Authorized`.
- `Ready`.
- `Degraded`.
- `Deleting`.

All status should include `observedGeneration`, concrete resource references, endpoint addresses, last successful probe, reason codes and provider IDs. Never place credential values in status.

### Mesh

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: Mesh
metadata:
  name: platform
spec:
  providerRef:
    name: upstream-istio
  clusters:
    selector:
      matchLabels:
        lake.dev/mesh: enabled
  dataPlane:
    defaultMode: Ambient
    sidecarAllowed: true
  trust:
    domain: lake.internal
    clusterIdentityMode: Distinct
  mtls:
    mode: Strict
  gatewayAPI:
    version: v1.5.0
  upgrades:
    channel: Pinned
    canaryNamespaces:
      - lake-mesh-canary
status:
  observedVersion: 1.31.1
  conditions: []
```

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: MeshProvider
metadata:
  name: upstream-istio
spec:
  type: Istio
  version: 1.31.1
  installation:
    method: Helm
    chartRegistry: docker.io/istio
  capabilities:
    ambient: Required
    gatewayAPI: Required
```

The concrete `IstioMesh` should hold Helm values, revision tags, image digests, CNI/platform overrides and component placement. Those fields do not belong in `Mesh`.

### Workspace sandbox

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: NetworkSandbox
metadata:
  name: default
  namespace: customer-a-prod
spec:
  providerRef:
    name: cilium-istio
  defaultIngress: Deny
  defaultEgress: Deny
  mesh:
    mode: Ambient
    mtls: Strict
    authorizationDefault: Deny
  dns:
    mode: AllowListed
    resolverClass: Cluster
    blockExternalResolvers: true
  metadataEndpoints:
    deny: true
  exceptions: []
status:
  enforcedLayers:
    - CiliumL3L4
    - IstioL4
  conditions: []
```

The provider creates NetworkPolicies/Cilium policies, namespace mesh labels, `PeerAuthentication`, baseline `AuthorizationPolicy`, DNS allowances and probe exceptions.

### Internal connection

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: Connection
metadata:
  name: reporting-to-orders-db
  namespace: customer-a-prod
spec:
  providerRef:
    name: istio-service-connection
  source:
    workloadRef:
      apiVersion: apps.lake.dev/v1
      kind: App
      name: reporting
  destination:
    serviceRef:
      apiVersion: data.lake.dev/v1
      kind: Postgres
      name: orders
    portName: postgres
  access:
    protocol: PostgreSQL
    networkScope: Service
  credential:
    mode: Managed
    role:
      intent: ReadOnly
      parameters:
        database: orders
        schemas:
          - reporting
    projection:
      mountPath: /var/run/lake/connections/orders
  revocation:
    terminateSessions: true
status:
  endpoint:
    host: orders-rw.customer-a-prod.svc
    port: 5432
  credentialRef:
    name: reporting-to-orders-db
  conditions: []
```

The operator creates a unique service account if the application lacks one, Cilium source/destination policy, Istio authorization, CloudNativePG `DatabaseRole`, grants job/controller state and a projected Secret.

### External connection

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: ExternalConnection
metadata:
  name: analytics-snowflake
  namespace: customer-a-prod
spec:
  providerRef:
    name: istio-cilium-egress
  source:
    workloadRef:
      apiVersion: apps.lake.dev/v1
      kind: App
      name: analytics
  destination:
    hostnames:
      - acme.snowflakecomputing.com
    ports:
      - number: 443
        protocol: TLS
  egressIdentityRef:
    name: customer-a-default
  credential:
    secretStoreRef:
      name: lake-customer-secrets
    item: snowflake-analytics
    projection:
      mountPath: /var/run/lake/connections/snowflake
  tls:
    verification: Required
    serverName: acme.snowflakecomputing.com
status:
  egressAddresses:
    - 203.0.113.20
  resolvedAddresses: []
  conditions: []
```


### Developer tunnel

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: Tunnel
metadata:
  name: orders-postgres-dev
  namespace: customer-a-prod
spec:
  providerRef:
    name: cloudflare-default
  targetRef:
    apiVersion: data.lake.dev/v1
    kind: Postgres
    name: orders
  mode: ClientProxy
  protocol: TCP
  targetPort: 5432
  endpoint:
    requestedHostname: orders-pg.customer-a.dev.lake.example
  authorization:
    groups:
      - customer-a-developers
    requireMFA: true
    devicePostureRef:
      name: lake-managed-device
  session:
    maximumDuration: 8h
  audit:
    enabled: true
status:
  endpoint:
    hostname: orders-pg.customer-a.dev.lake.example
  client:
    type: CloudflaredAccessTCP
    commandTemplate: >-
      cloudflared access tcp --hostname
      orders-pg.customer-a.dev.lake.example --url localhost:15432
  conditions: []
```

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: TunnelProvider
metadata:
  name: cloudflare-default
spec:
  type: Cloudflare
  credentialRef:
    name: cloudflare-operator
    namespace: lake-system
  connectorPoolRef:
    name: east-us-shared
  capabilities:
    - HTTP
    - TCP
    - PrivateNetwork
    - IdentityAccess
```

The concrete `CloudflareTunnel` stores Cloudflare account/zone references, tunnel UUID, route type, Access application ID, policy IDs, connector deployment and remote configuration version.

### Peer connection

```yaml
apiVersion: network.lake.dev/v1alpha1
kind: PeerConnection
metadata:
  name: customer-a-aws
spec:
  providerRef:
    name: azure-aws-vpn
  local:
    clusterRef:
      name: lake-east-us
    exports:
      services:
        - namespace: customer-a-prod
          name: zetta-gateway
  remote:
    organizationRef:
      name: customer-a
    networks:
      - 10.70.0.0/16
  routing:
    mode: Dynamic
    overlapPolicy: Reject
  dns:
    importZones:
      - internal.customer-a.example
  availability:
    tunnels: 2
  approval:
    remoteApprovalRequired: true
status:
  phase: AwaitingRemoteApproval
  observedRoutes: []
  conditions: []
```

Copy Gateway API’s separation of infrastructure ownership, listeners, routes and cross-namespace grants. Copy Crossplane’s provider references and managed-resource conditions, but do not copy its removed implicit composite connection-secret behavior.[^1_52][^1_53]

## 24. Failure modes

| Failure | Effect | Detection | Mitigation |
| :-- | :-- | :-- | :-- |
| Istio certificate issuance/rotation | New or renewed mTLS sessions fail; existing sessions may survive temporarily | SDS errors, certificate-expiry metrics, failed HBONE handshakes | Alert well before expiry; synthetic cross-node calls; redundant istiod |
| ztunnel failure | Node-local ambient connections fail or fall back only where configuration permits | DaemonSet readiness, ztunnel connection and policy metrics | Priority class, disruption controls, node-by-node upgrade, immediate drain/repair |
| Istio CNI failure | New pods may start without correct interception or be held/repaired | CNI logs, ambient enrollment status, synthetic new-pod test | Fail closed, repair mode, canary node pool |
| Waypoint failure | Services bound to the waypoint lose L7 path | Gateway status, Envoy readiness, request errors | At least two replicas, topology spread, conservative autoscaling |
| Cilium policy error | Broad outage or unintended reachability | Hubble flows, policy verdicts, endpoint policy revision | Server-side dry run, generated-policy tests, staged rollout |
| DNS proxy/cache failure | Name resolution fails while direct IP tests may pass | DNS latency/error metrics, Cilium FQDN state | Redundant CoreDNS; controlled node-local cache; test exact policy path |
| Egress gateway failure | Declared external connections fail | Gateway health and per-destination probes | HA deployment, PDB, multiple zones |
| `cloudflared` replica loss | Reduced tunnel edge diversity; usually no outage with another replica | Connector count and Cloudflare connection state | Minimum two replicas; alert below desired connector count |
| Access policy/API drift | Users denied or unexpectedly admitted | Reconcile diff, Cloudflare audit events, synthetic identity tests | Authoritative reconciliation and deny-by-default |
| Credential rotation mismatch | Network works but authentication fails | Destination auth logs and binding condition | Overlap old/new credential where possible; readiness-gated rollout |

Debug “app cannot reach service” in this order:

1. Resolve the intended connection object and observed generation.
2. Verify source and destination service accounts.
3. Verify DNS response from the actual workload.
4. Check Cilium/Hubble drop verdict.
5. Check ztunnel enrollment and mTLS identity.
6. Run `istioctl x authz check` against the destination pod; Istio documents this command for inspecting effective policy.[^1_74]
7. Check waypoint/gateway route attachment and status.
8. Verify endpoints and named port/protocol.
9. Verify application TLS and service-native credentials.
10. Test same-node and cross-node separately.
11. Inspect provider/API status for external tunnels.

The blast radius differs by component: sidecar failure is pod-local; ztunnel is node-local; waypoint and egress gateways affect their attached service set; istiod primarily affects configuration propagation and certificate issuance; Cilium and Istio CNI failures can affect an entire node or cluster.

## 25. Performance and cost

Do not put universal overhead numbers into Lake’s product contract. Istio and Cilium benchmarks differ in encryption, policy, routing mode, payload, connection reuse and whether kernel CPU is accounted for. Istio’s published ambient comparison reported different throughput and latency behavior from Cilium but also noted accounting differences for kernel encryption CPU.[^1_75][^1_76]

Architectural cost tendencies are reliable:


| Mode | Fixed cost | Marginal cost |
| :-- | :-- | :-- |
| Sidecar | istiod and gateways | Proxy CPU/memory and connections for every pod |
| Ambient L4 | ztunnel and CNI DaemonSets per node | Certificates/connections per workload, but no proxy container per pod |
| Ambient with waypoint | ztunnel per node plus waypoint replicas | L7 proxy cost per waypoint scope |
| Cloudflare shared tunnel | Connector replicas per cluster/pool | Routes and Access objects |
| Tunnel per workspace | Connector replicas per workspace | Large pod/token/API and operational multiplication |

Benchmark Lake’s real workloads before setting defaults:

- Small HTTP apps at high pod density.
- Long-lived Postgres and Kafka sessions.
- Kafka throughput and broker replication.
- Cassandra and ClickHouse east-west throughput.
- Connection churn from agents/jobs.
- Waypoint failover.
- Cross-node and cross-zone traffic.
- DNS/FQDN policy churn.
- Tunnel latency from each Azure region.

Publish measured p50/p95/p99 latency, throughput, CPU, memory and connection limits for Lake’s pinned versions and VM types.

## 26. Product navigation

Use a single **Network** area scoped first to organization and workspace, with cluster/provider details hidden behind an advanced view.

### Overview

- Sandbox state: locked, degraded or exceptions present.
- Active internal and external connections.
- Developer tunnel count and active users.
- Peer health.
- Egress IPs.
- Recent denied flows and policy changes.


### Connections

Two tabs:

- **Internal:** source, destination, port, role, credential age and observed traffic.
- **External:** destination host, protocol, credential, egress IP, DNS state and last successful probe.

The creation flow should start with “Connect this app to…” rather than asking users to write firewall rules.

### Developer access

- Resource.
- Connection method.
- Authorized users/groups.
- MFA/device requirements.
- Expiration.
- Copyable client command.
- Active/recent sessions.
- Immediate revoke.


### Private networks

- Peer connections.
- Imported/exported routes and services.
- CIDR overlap warnings.
- Remote approval steps.
- Tunnel health and latency.


### DNS

- Allowed names by workload.
- Current observed addresses and TTLs.
- Blocked resolver attempts.
- Shared/CDN-address warning.
- Provider capability state.


### Policies

An advanced, primarily read-only explanation of generated policy:

- “Reporting app may reach Orders Postgres on TCP 5432.”
- “No other inbound or outbound traffic is permitted.”
- Effective Kubernetes/Cilium and Istio resources.
- Temporary exceptions with owner and expiry.


### Activity

Correlate:

- Source Lake resource.
- Source workload/service account.
- Destination.
- Allowed or denied.
- Enforcement layer.
- Bytes and sessions.
- User identity where Cloudflare provides it.
- Credential binding.
- Policy revision.

Never claim a Cloudflare user identity at the final TCP service when only the connector’s mesh identity is observable.

# Phased implementation

## Phase 0: capability lab

Build a disposable AKS 1.35 cluster matching production exactly.

Prove:

- Istio CNI coexistence with AKS-managed Cilium.
- Required/available Cilium flags.
- Ambient enrollment under restricted Pod Security.
- Default-deny Cilium policy with probes.
- DNS capture plus AKS ACNS FQDN policy.
- Same-node/cross-node/headless traffic.
- Node and CNI upgrades.
- ztunnel fail-closed behavior.

Exit criterion: Microsoft support position recorded and repeatable automated tests pass.

## Phase 1: sandbox without mesh

Introduce `NetworkSandbox` and `Connection` CRDs using Kubernetes/Cilium policy only.

Prove:

- Default deny.
- Controller-owned exceptions.
- Per-workload service accounts.
- DNS restriction.
- Hubble-based diagnostics.
- No tenant ability to disable policy.

This is the smallest safe step because it creates the anti-bypass boundary before adding proxy complexity.

## Phase 2: ambient identity

Install upstream Istio using pinned Helm artifacts and enroll one canary namespace.

Add:

- Strict mTLS.
- Empty default-deny authorization.
- Per-connection source-principal/port allows.
- Certificate and ztunnel monitoring.

Prove all datastore protocols without waypoints first.

## Phase 3: credential bindings

Implement Postgres and Kafka connection providers:

- CloudNativePG `DatabaseRole`.
- Postgres grants.
- Strimzi `KafkaUser`.
- Projected binding Secrets.
- Rotation and deletion.
- Revocation tests.

Do not generalize a portable role vocabulary until at least two SQL and two non-SQL providers have been implemented.

## Phase 4: controlled egress

Create shared HA egress gateway pools and `ExternalConnection`.

Prove:

- Direct-IP bypass is blocked.
- FQDN changes reconcile.
- DoH and alternate DNS paths are denied.
- TLS/SNI behavior.
- Stable NAT IP.
- Gateway failure and scaling.
- Shared SaaS/CDN destination limitations.


## Phase 5: developer tunnels

Deploy one shared Cloudflare tunnel pool in one leaf cluster.

Implement:

- Remotely managed tunnel.
- Route and Access-policy reconciliation.
- HTTP and `cloudflared access tcp`.
- Two replicas.
- Revocation and audit.
- Internal Istio gateway target.
- Token rotation.

Add WARP private application mode after the simpler TCP proxy is stable.

## Phase 6: datastore certification

Run an automated matrix for every supported operator version:

- Provision.
- Bootstrap.
- Scale.
- Failover.
- Upgrade.
- Backup/restore.
- Credential rotation.
- Network-policy mutation.
- Mesh component restart.
- Node drain.
- Cross-zone behavior.

Publish certified combinations; label everything else unsupported rather than “probably compatible.”

## Phase 7: cross-cloud portability

Deploy the same upstream Helm and Lake CRDs on EKS and GKE.

Keep these provider-specific:

- CNI capabilities.
- DNS architecture.
- Pod-security/system namespace configuration.
- Egress IP implementation.
- Load balancer and private-link resources.

The abstract APIs should remain unchanged.

## Phase 8: peer networking

Begin with one customer AWS connection or one Lake-to-Lake gateway connection.

Prove:

- Approval workflow.
- Route overlap detection.
- HA.
- DNS forwarding.
- Revocation.
- Service-only exports.
- Failure isolation.

Add Istio multicluster only after basic routed gateway connectivity is stable.

# Open risks

## Established risks

- Istio registry controls are bypassable without CNI policy.[^1_15]
- Ambient and default-deny Cilium policy require explicit probe/HBONE handling.[^1_20]
- Cilium socket-level load balancing can bypass sidecar service interception unless configured appropriately.[^1_26][^1_5]
- AKS FQDN filtering requires ACNS and specific Cilium DNS rules.[^1_42][^1_43]
- The AKS Istio add-on does not support ambient.[^1_14]
- `cloudflared access tcp` is less suitable for long-lived connections than WARP/Client-to-Tunnel.[^1_58]
- Cloudflare human identity is not automatically propagated through arbitrary TCP protocols to the final service.
- Overlapping pod CIDRs rule out a simple flat multicluster network.


## Not verified

- The exact latest Istio 1.31 patch after 1.31.1 on October 13, 2026.
- Microsoft support for upstream Istio ambient on AKS-managed Cilium, as distinct from upstream technical interoperability.
- Whether AKS permits and supports changing `cni.exclusive`, BPF masquerading and socket-LB behavior required by the chosen mode.
- Ambient multicluster production status and complete behavior with overlapping CIDRs in Istio 1.31.
- Microsoft-supported Cilium Cluster Mesh availability on AKS-managed Cilium.
- Vendor-certified Istio compatibility for K8ssandra, Altinity ClickHouse operator, TiDB operator and the selected Valkey operator.
- All Cloudflare account limits applicable to Lake’s expected numbers of Access applications, policies, hostnames and service tokens.
- Per-product public benchmark numbers comparable enough to use for Lake capacity planning.
- Exact behavior of each managed Kubernetes provider’s node-local DNS implementation when combined with Istio DNS capture and Cilium FQDN policy.

These items should be treated as release-blocking validation work, not documentation gaps that can be papered over with permissive exceptions.

<span style="display:none">[^1_100][^1_101][^1_102][^1_103][^1_104][^1_105][^1_106][^1_107][^1_108][^1_109][^1_110][^1_111][^1_112][^1_113][^1_114][^1_115][^1_116][^1_117][^1_118][^1_119][^1_120][^1_121][^1_122][^1_123][^1_124][^1_125][^1_126][^1_127][^1_128][^1_129][^1_130][^1_131][^1_132][^1_133][^1_134][^1_135][^1_136][^1_137][^1_138][^1_139][^1_140][^1_141][^1_142][^1_143][^1_144][^1_145][^1_146][^1_147][^1_148][^1_149][^1_150][^1_151][^1_152][^1_153][^1_154][^1_155][^1_156][^1_157][^1_158][^1_159][^1_160][^1_161][^1_162][^1_163][^1_164][^1_165][^1_166][^1_167][^1_168][^1_169][^1_170][^1_171][^1_172][^1_173][^1_174][^1_175][^1_176][^1_177][^1_178][^1_179][^1_180][^1_181][^1_182][^1_183][^1_184][^1_185][^1_186][^1_187][^1_188][^1_189][^1_190][^1_191][^1_192][^1_193][^1_194][^1_195][^1_196][^1_197][^1_198][^1_199][^1_200][^1_201][^1_202][^1_203][^1_204][^1_205][^1_206][^1_207][^1_208][^1_209][^1_210][^1_211][^1_212][^1_213][^1_214][^1_215][^1_216][^1_217][^1_218][^1_219][^1_220][^1_221][^1_222][^1_223][^1_224][^1_225][^1_226][^1_227][^1_228][^1_229][^1_230][^1_231][^1_232][^1_233][^1_234][^1_235][^1_236][^1_237][^1_238][^1_239][^1_240][^1_241][^1_242][^1_243][^1_244][^1_245][^1_246][^1_247][^1_248][^1_249][^1_250][^1_251][^1_252][^1_253][^1_254][^1_255][^1_256][^1_257][^1_258][^1_259][^1_260][^1_261][^1_262][^1_263][^1_264][^1_265][^1_266][^1_267][^1_268][^1_269][^1_270][^1_271][^1_272][^1_273][^1_274][^1_275][^1_276][^1_277][^1_278][^1_279][^1_280][^1_281][^1_282][^1_283][^1_284][^1_285][^1_286][^1_287][^1_288][^1_289][^1_290][^1_291][^1_292][^1_293][^1_294][^1_295][^1_296][^1_297][^1_298][^1_299][^1_300][^1_301][^1_302][^1_303][^1_304][^1_305][^1_306][^1_307][^1_308][^1_309][^1_310][^1_311][^1_312][^1_313][^1_314][^1_315][^1_316][^1_317][^1_318][^1_319][^1_320][^1_321][^1_322][^1_323][^1_324][^1_325][^1_326][^1_327][^1_328][^1_329][^1_330][^1_331][^1_332][^1_333][^1_334][^1_335][^1_336][^1_337][^1_338][^1_339][^1_340][^1_341][^1_342][^1_343][^1_344][^1_345][^1_346][^1_347][^1_348][^1_349][^1_350][^1_351][^1_352][^1_353][^1_354][^1_355][^1_356][^1_357][^1_358][^1_359][^1_360][^1_361][^1_362][^1_363][^1_364][^1_365][^1_366][^1_367][^1_368][^1_369][^1_370][^1_371][^1_372][^1_373][^1_374][^1_375][^1_376][^1_377][^1_378][^1_379][^1_380][^1_381][^1_382][^1_383][^1_384][^1_385][^1_386][^1_387][^1_388][^1_389][^1_390][^1_391][^1_392][^1_393][^1_394][^1_395][^1_396][^1_397][^1_398][^1_399][^1_400][^1_401][^1_402][^1_403][^1_404][^1_405][^1_406][^1_407][^1_408][^1_409][^1_410][^1_411][^1_412][^1_413][^1_414][^1_415][^1_416][^1_417][^1_418][^1_419][^1_420][^1_421][^1_422][^1_423][^1_424][^1_425][^1_426][^1_427][^1_428][^1_429][^1_430][^1_431][^1_432][^1_433][^1_434][^1_435][^1_436][^1_437][^1_438][^1_439][^1_440][^1_441][^1_442][^1_443][^1_444][^1_445][^1_446][^1_447][^1_448][^1_449][^1_450][^1_451][^1_452][^1_453][^1_454][^1_455][^1_456][^1_457][^1_458][^1_459][^1_460][^1_461][^1_462][^1_463][^1_464][^1_465][^1_466][^1_467][^1_468][^1_469][^1_470][^1_471][^1_472][^1_473][^1_474][^1_475][^1_476][^1_477][^1_478][^1_479][^1_480][^1_481][^1_482][^1_483][^1_484][^1_485][^1_486][^1_487][^1_488][^1_489][^1_490][^1_491][^1_492][^1_493][^1_494][^1_495][^1_496][^1_497][^1_498][^1_499][^1_500][^1_501][^1_502][^1_503][^1_504][^1_505][^1_506][^1_507][^1_508][^1_509][^1_510][^1_511][^1_512][^1_513][^1_514][^1_515][^1_516][^1_517][^1_518][^1_519][^1_520][^1_521][^1_522][^1_523][^1_524][^1_525][^1_526][^1_527][^1_528][^1_529][^1_77][^1_78][^1_79][^1_80][^1_81][^1_82][^1_83][^1_84][^1_85][^1_86][^1_87][^1_88][^1_89][^1_90][^1_91][^1_92][^1_93][^1_94][^1_95][^1_96][^1_97][^1_98][^1_99]</span>

<div align="center">⁂</div>

[^1_1]: https://istio.io/latest/uk/docs/setup/upgrade/helm/

[^1_2]: https://istio.io/latest/uk/docs/setup/upgrade/canary/

[^1_3]: https://docs.solo.io/istio/1.31.x/setup/install/manual/

[^1_4]: https://docs.solo.io/istio/1.31.x/operations/upgrade/manual/

[^1_5]: https://docs.cilium.io/en/stable/network/kubernetes/kubeproxy-free/

[^1_6]: https://docs.cilium.io/en/stable/operations/system_requirements/

[^1_7]: https://github.com/cloudflare/cloudflared/releases?after=2021.2.2

[^1_8]: https://developers.cloudflare.com/workers-vpc/configuration/tunnel/

[^1_9]: https://cloudnative-pg.io/releases/cloudnative-pg-1-30.0-released/

[^1_10]: https://strimzi.io/docs/operators/0.50.0/configuring

[^1_11]: https://istio.io/latest/news/releases/1.29.x/announcing-1.29/upgrade-notes/

[^1_12]: https://istio.io/latest/news/releases/1.29.x/announcing-1.29/

[^1_13]: https://developers.cloudflare.com/tunnel/platform/changelog/

[^1_14]: https://docs.azure.cn/en-us/aks/istio-about

[^1_15]: https://istio.io/latest/zh/docs/tasks/traffic-management/egress/egress-control/

[^1_16]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-availability/deploy-replicas/

[^1_17]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/

[^1_18]: https://istio.io/latest/es/docs/ambient/install/platform-prerequisites/

[^1_19]: https://istio.io/latest/blog/2024/inpod-traffic-redirection-ambient/

[^1_20]: https://istio.io/latest/docs/ambient/usage/networkpolicy/

[^1_21]: https://istio.io/latest/docs/setup/additional-setup/pod-security-admission/

[^1_22]: https://istio.io/latest/uk/docs/setup/additional-setup/cni/

[^1_23]: https://strimzi.io/docs/operators/in-development/full/configuring

[^1_24]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-with-firewall/

[^1_25]: https://preliminary.istio.io/latest/docs/ambient/architecture/data-plane/

[^1_26]: https://github.com/istio/istio/issues/27619

[^1_27]: https://github.com/istio/istio.io/issues/15194

[^1_28]: https://github.com/istio/istio/issues/35894

[^1_29]: https://istio.io/latest/blog/2024/in-cluster-operator-deprecation-announcement/

[^1_30]: https://istio.io/latest/news/releases/1.23.x/announcing-1.23/change-notes/

[^1_31]: https://istio.website.cncfstack.com/docs/ambient/upgrade/helm/

[^1_32]: https://docs.apps.rancher.io/reference-guides/istio

[^1_33]: https://istio.io/latest/docs/tasks/traffic-management/ingress/gateway-api/

[^1_34]: https://istio.io/latest/docs/reference/config/networking/service-entry/

[^1_35]: https://istio.io/latest/docs/reference/config/security/peer_authentication/

[^1_36]: https://istio.io/latest/docs/tasks/security/authentication/authn-policy/

[^1_37]: https://istio.io/latest/uk/docs/ambient/usage/l4-policy/

[^1_38]: https://istio.io/latest/docs/concepts/security/

[^1_39]: https://istio.io/latest/docs/reference/config/security/conditions/

[^1_40]: https://istio.io/latest/es/docs/ambient/usage/egress-gateway/

[^1_41]: https://docs.cilium.io/en/stable/security/policy/layer3/

[^1_42]: https://docs.azure.cn/en-us/aks/container-network-security-fqdn-filtering-concepts

[^1_43]: https://learn.microsoft.com/en-us/azure/aks/how-to-apply-fqdn-filtering-policies

[^1_44]: https://docs.cilium.io/en/latest/security/policy/troubleshooting/

[^1_45]: https://istio.io/latest/es/docs/ops/configuration/traffic-management/dns-proxy/

[^1_46]: https://docs.cilium.io/en/stable/network/kubernetes/local-redirect-policy/

[^1_47]: https://learn.microsoft.com/en-us/azure/aks/localdns-custom

[^1_48]: https://istio.io/latest/zh/docs/ambient/usage/l4-policy/

[^1_49]: https://cloudnative-pg.io/docs/1.27/declarative_role_management/

[^1_50]: https://cloudnative-pg.io/docs/1.29/declarative_role_management/

[^1_51]: https://github.com/servicebinding/spec

[^1_52]: https://docs.crossplane.io/latest/whats-new/

[^1_53]: https://docs.crossplane.io/v2.2/guides/connection-details-composition/

[^1_54]: https://devcenter2.assets.heroku.com/articles/heroku-postgresql-credentials

[^1_55]: https://developers.cloudflare.com/api/python/resources/zero_trust/subresources/tunnels/subresources/cloudflared/

[^1_56]: https://developers.cloudflare.com/api/resources/zero_trust/subresources/tunnels/

[^1_57]: https://www.cloudflare.com/sase/products/access/

[^1_58]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/protocols/

[^1_59]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/

[^1_60]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/self-hosted-private-app/

[^1_61]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/browser-rendering/

[^1_62]: https://developers.cloudflare.com/cloudflare-one/access-controls/service-credentials/service-tokens/index.md

[^1_63]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/short-lived-certificates-legacy/

[^1_64]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/ssh/ssh-infrastructure-access/

[^1_65]: https://developers.cloudflare.com/tunnel/troubleshooting/index.md

[^1_66]: https://github.com/STRRL/cloudflare-tunnel-ingress-controller

[^1_67]: https://github.com/topics/cloudflare-tunnel

[^1_68]: https://github.com/beezlabs-org/cloudflare-tunnel-operator

[^1_69]: https://tailscale.com/docs/kubernetes-operator/concepts/proxyclass

[^1_70]: https://tailscale.com/docs/kubernetes-operator/reference/rbac

[^1_71]: https://ngrok.com/docs/k8s

[^1_72]: https://ngrok.com/docs/k8s/installation/architecture

[^1_73]: https://github.com/ngrok/ngrok-operator

[^1_74]: https://istio.io/latest/docs/ops/common-problems/security-issues/

[^1_75]: https://istio.io/latest/es/blog/2024/ambient-vs-cilium/

[^1_76]: https://istio.io/latest/blog/2025/ambient-performance/

[^1_77]: https://istio.io/latest/docs/overview/dataplane-modes/

[^1_78]: https://istio.io/latest/blog/

[^1_79]: https://istio.io/latest/docs/ambient/install/istioctl/

[^1_80]: https://github.com/cloudflare/cloudflared/releases

[^1_81]: https://istio.io/latest/docs/ambient/

[^1_82]: https://preliminary.istio.io/latest/docs/releases/log/

[^1_83]: https://istio.io/latest/news/releases/1.30.x/announcing-1.30/

[^1_84]: https://istio.io/latest/docs/ambient/getting-started/

[^1_85]: https://istio.io/latest/docs/ambient/migrate/

[^1_86]: https://istio.io/latest/news/releases/1.31.x/announcing-1.31/change-notes/

[^1_87]: https://istio.io/latest/docs/ambient/migrate/before-you-begin/

[^1_88]: https://istio.io/latest/news/releases/1.30.x/announcing-1.30/change-notes/

[^1_89]: https://istio.io/latest/docs/releases/supported-releases/

[^1_90]: https://istio.io/latest/news/releases/1.31.x/announcing-1.31.1/

[^1_91]: https://istio.io/latest/blog/2026/ambient-multinetwork-multicluster-beta/

[^1_92]: https://github.com/kubernetes-sigs/gateway-api/releases

[^1_93]: https://github.com/kubernetes-sigs/gateway-api

[^1_94]: https://github.com/cilium/cilium

[^1_95]: https://github.com/cilium/cilium/discussions/47586

[^1_96]: https://github.com/cilium/cilium/releases

[^1_97]: https://gateway-api.sigs.k8s.io/docs/concepts/versioning/

[^1_98]: https://github.com/cilium/cilium/discussions/44191

[^1_99]: https://github.com/cilium/cilium/pull/46246

[^1_100]: https://github.com/cilium/cilium/discussions

[^1_101]: https://github.com/cilium/cilium/pull/45851

[^1_102]: https://github.com/cilium/cilium/pull/46599

[^1_103]: https://github.com/cilium/cilium/pull/45739

[^1_104]: https://gateway-api.sigs.k8s.io/docs/implementations/list/

[^1_105]: https://github.com/cilium/cilium/releases/tag/v1.20.0

[^1_106]: https://github.com/cilium/cilium/pull/47415

[^1_107]: https://learn.microsoft.com/en-us/azure/aks/istio-about

[^1_108]: https://learn.microsoft.com/en-us/azure/aks/istio-meshconfig

[^1_109]: https://learn.microsoft.com/en-us/azure/aks/istio-cni

[^1_110]: https://learn.microsoft.com/en-us/azure/aks/istio-gateway-api

[^1_111]: https://learn.microsoft.com/en-us/azure/aks/istio-deploy-addon

[^1_112]: https://docs.cilium.io/en/latest/network/servicemesh/istio/

[^1_113]: https://learn.microsoft.com/en-us/azure/aks/istio-deploy-egress

[^1_114]: https://learn.microsoft.com/en-us/azure/aks/migration-from-open-source-istio-to-addon

[^1_115]: https://learn.microsoft.com/en-us/azure/aks/istio-support-policy

[^1_116]: https://learn.microsoft.com/en-us/azure/aks/open-service-mesh-istio-migration-guidance

[^1_117]: https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/extensions/istio-add-on-cni-troubleshooting

[^1_118]: https://learn.microsoft.com/ga-ie/azure/application-network/supported-versions

[^1_119]: https://learn.microsoft.com/en-us/azure/aks/app-routing-gateway-api

[^1_120]: https://learn.microsoft.com/en-us/azure/aks/istio-scale

[^1_121]: https://learn.microsoft.com/nb-no/azure/aks/istio-support-policy

[^1_122]: https://istio.io/latest/docs/setup/upgrade/helm/

[^1_123]: https://istio.io/latest/docs/ambient/migrate/install-ambient-components/

[^1_124]: https://istio.io/latest/docs/ambient/install/platform-prerequisites/

[^1_125]: https://istio.io/latest/docs/ambient/install/helm/

[^1_126]: https://istio.io/latest/docs/setup/upgrade/

[^1_127]: https://istio.io/latest/docs/ambient/upgrade/helm/

[^1_128]: https://istio.io/latest/docs/releases/log/

[^1_129]: https://istio.io/latest/docs/setup/install/helm/

[^1_130]: https://istio.io/latest/news/releases/1.25.x/announcing-1.25/change-notes/

[^1_131]: https://istio.io/latest/docs/ambient/install/

[^1_132]: https://istio.io/latest/docs/setup/upgrade/canary/

[^1_133]: https://istio.io/latest/docs/ops/common-problems/observability-issues/

[^1_134]: https://istio.io/latest/docs/setup/additional-setup/cni/

[^1_135]: https://istio.io/latest/blog/2026/egress-dynamic-dns/

[^1_136]: https://istio.io/latest/docs/ops/configuration/traffic-management/dns-proxy/

[^1_137]: https://istio.io/latest/docs/ambient/usage/egress-gateway/

[^1_138]: https://docs.cilium.io/en/latest/security/policy/layer7/

[^1_139]: https://docs.cilium.io/en/stable/security/policy/layer7/

[^1_140]: https://istio.io/latest/docs/tasks/traffic-management/egress/egress-control/

[^1_141]: https://istio.io/latest/news/releases/1.29.x/announcing-1.29/change-notes/

[^1_142]: https://istio.io/latest/docs/ops/configuration/traffic-management/dns/

[^1_143]: https://docs.cilium.io/en/stable/security/policy/language/

[^1_144]: https://istio.io/latest/docs/ambient/usage/serviceentry-visibility/

[^1_145]: https://istio.io/latest/blog/2026/istio-network-security-flat-networks-ambient-mode/

[^1_146]: https://istio.io/latest/es/blog/2026/egress-dynamic-dns/

[^1_147]: https://docs.cilium.io/en/latest/security/standalone-dns-proxy/

[^1_148]: https://docs.cilium.io/en/stable/security/standalone-dns-proxy/

[^1_149]: https://istio.io/latest/docs/ambient/usage/l4-policy/

[^1_150]: https://istio.io/latest/docs/ambient/architecture/data-plane/

[^1_151]: https://kubernetes.io/docs/concepts/services-networking/network-policies/

[^1_152]: https://istio.io/latest/docs/ambient/usage/l7-features/

[^1_153]: https://istio.io/latest/docs/ambient/getting-started/enforce-auth-policies/

[^1_154]: https://v1-34.docs.kubernetes.io/blog/2018/07/18/11-ways-not-to-get-hacked/

[^1_155]: https://istio.io/latest/docs/reference/config/security/authorization-policy/

[^1_156]: https://istio.io/latest/docs/ambient/migrate/migrate-policies/

[^1_157]: https://istio.io/latest/docs/ops/best-practices/security/

[^1_158]: https://istio.io/latest/docs/ambient/usage/waypoint/

[^1_159]: https://istio.io/v1.15/blog/2022/introducing-ambient-mesh/

[^1_160]: https://istio.io/latest/docs/ops/configuration/traffic-management/protocol-selection/

[^1_161]: https://istio.io/latest/docs/ops/deployment/application-requirements/

[^1_162]: https://istio.io/latest/docs/reference/config/networking/sidecar/

[^1_163]: https://istio.io/latest/docs/concepts/traffic-management/

[^1_164]: https://istio.io/latest/about/faq/

[^1_165]: https://istio.io/latest/docs/ops/configuration/traffic-management/traffic-routing/

[^1_166]: https://istio.io/latest/about/faq/traffic-management/

[^1_167]: https://istio.io/latest/zh/about/faq/

[^1_168]: https://istio.io/latest/zh/about/faq/traffic-management/

[^1_169]: https://istio.io/latest/es/about/faq/traffic-management/

[^1_170]: https://istio.io/latest/docs/ops/common-problems/network-issues/

[^1_171]: https://istio.io/latest/blog/2021/aeraki/

[^1_172]: https://istio.io/latest/zh/docs/ops/configuration/traffic-management/protocol-selection/

[^1_173]: https://istio.io/latest/uk/docs/ops/configuration/traffic-management/protocol-selection/

[^1_174]: https://cloudnative-pg.io/docs/devel/declarative_role_management/

[^1_175]: https://cloudnative-pg.io/docs/1.26/declarative_role_management/

[^1_176]: https://strimzi.io/docs/operators/latest/full/deploying

[^1_177]: https://strimzi.io/docs/operators/latest/full/overview

[^1_178]: https://cloudnative-pg.io/documentation/1.21/declarative_role_management/

[^1_179]: https://cloudnative-pg.io/documentation/1.20/declarative_role_management/

[^1_180]: https://cloudnative-pg.io/docs/1.28/declarative_role_management/

[^1_181]: https://strimzi.io/docs/operators/latest/configuring

[^1_182]: https://strimzi.io/docs/operators/0.26.0/full/using

[^1_183]: https://strimzi.io/docs/operators/0.33.2/configuring

[^1_184]: https://www.gabrielebartolini.it/articles/2026/07/cnpg-recipe-25-declarative-roles-and-passwordless-tls-in-cloudnativepg-1.30/

[^1_185]: https://strimzi.io/docs/operators/0.33.1/full/configuring

[^1_186]: https://cloudnative-pg.io/docs/1.30/installation_upgrade/

[^1_187]: https://strimzi.io/docs/operators/latest/deploying

[^1_188]: https://strimzi.io/docs/operators/0.27.1/full/using

[^1_189]: https://docs.crossplane.io/latest/guides/connection-details-composition/

[^1_190]: https://docs.crossplane.io/v1.20/concepts/connection-details/

[^1_191]: https://docs.crossplane.io/v1.20/concepts/managed-resources/

[^1_192]: https://blog.crossplane.io/faq-2-claim-connection-details/

[^1_193]: https://docs.crossplane.io/latest/managed-resources/managed-resources/

[^1_194]: https://servicebinding.io/spec/core/1.1.0/

[^1_195]: https://docs.crossplane.io/latest/guides/upgrade-to-crossplane-v2/

[^1_196]: https://docs.crossplane.io/v2.4/guides/upgrade-to-crossplane-v2/

[^1_197]: https://docs.crossplane.io/master/managed-resources/managed-resources/

[^1_198]: https://istio.io/latest/zh/docs/ops/integrations/spire/

[^1_199]: https://github.com/crossplane/docs/blob/master/content/v1.20/concepts/composite-resource-definitions.md

[^1_200]: https://github.com/crossplane/crossplane/blob/main/design/design-doc-external-secret-stores.md

[^1_201]: https://servicebinding.io/spec/core/1.0.0-rc3/

[^1_202]: https://github.com/crossplane/crossplane/issues/5522

[^1_203]: https://servicebinding.io/spec/core/1.0.0-rc1/

[^1_204]: https://docs.fly.io/networking/private-networking

[^1_205]: https://docs.fly.io/networking/custom-private-networks

[^1_206]: https://render.com/docs/migrate-from-railway.md

[^1_207]: https://devcenter2.assets.heroku.com/articles/platform-api-for-partners

[^1_208]: https://render.com/docs/private-services

[^1_209]: https://docs.fly.io/networking

[^1_210]: https://render.com/docs/private-services.md

[^1_211]: https://docs.railway.com/networking

[^1_212]: https://docs.railway.com/networking/private-networking.md

[^1_213]: https://render.com/docs/private-network

[^1_214]: https://docs.railway.com/networking/tcp-proxy

[^1_215]: https://fly.io/networking

[^1_216]: https://devcenter.heroku.com/articles/platform-api-reference

[^1_217]: https://docs.fly.io/networking/services

[^1_218]: https://fly.io/blog/incoming-6pn-private-networks/

[^1_219]: https://developers.cloudflare.com/tunnel/configuration/

[^1_220]: https://developers.cloudflare.com/cloudflare-one/networks/connectivity-options/

[^1_221]: https://developers.cloudflare.com/api/terraform/resources/zero_trust/subresources/tunnels/subresources/cloudflared/

[^1_222]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/tunnel-useful-terms/

[^1_223]: https://developers.cloudflare.com/reference-architecture/architectures/sase/

[^1_224]: https://developers.cloudflare.com/reference-architecture/architectures/security/

[^1_225]: https://developers.cloudflare.com/tunnel/monitoring/index.md

[^1_226]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/deployment-guides/kubernetes/

[^1_227]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/remote-tunnel-permissions/

[^1_228]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/

[^1_229]: https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/

[^1_230]: https://developers.cloudflare.com/tunnel/reference/tunnel-tokens/index.md

[^1_231]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/private-net/cloudflared/

[^1_232]: https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/

[^1_233]: https://developers.cloudflare.com/cloudflare-wan/zero-trust/connectivity-options/

[^1_234]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/choose-application-type/

[^1_235]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/cloudflared-authentication/

[^1_236]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/index.md

[^1_237]: https://developers.cloudflare.com/cloudflare-one/changelog/access/

[^1_238]: https://developers.cloudflare.com/cloudflare-one/tutorials/kubectl/

[^1_239]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-mesh/concepts/

[^1_240]: https://developers.cloudflare.com/learning-paths/clientless-access/access-application/create-access-app/

[^1_241]: https://developers.cloudflare.com/changelog/product/access/

[^1_242]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/

[^1_243]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/cloudflared-authentication/arbitrary-tcp/

[^1_244]: https://developers.cloudflare.com/learning-paths/replace-vpn/connect-private-network/connection-methods/

[^1_245]: https://developers.cloudflare.com/learning-paths/replace-vpn/connect-private-network/cloudflare-mesh/

[^1_246]: https://developers.cloudflare.com/learning-paths/replace-vpn/troubleshooting/troubleshoot-private-networks/

[^1_247]: https://developers.cloudflare.com/cloudflare-one/access-controls/service-credentials/mutual-tls-authentication/

[^1_248]: https://developers.cloudflare.com/cloudflare-one/account-limits/

[^1_249]: https://developers.cloudflare.com/cloudflare-wan/zero-trust/cloudflare-tunnel/

[^1_250]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/origin-parameters/

[^1_251]: https://developers.cloudflare.com/learning-paths/replace-vpn/connect-private-network/tunnel-capacity/

[^1_252]: https://developers.cloudflare.com/tunnel/llms-full.txt

[^1_253]: https://developers.cloudflare.com/cloudflare-one/changelog/tunnel/

[^1_254]: https://developers.cloudflare.com/cloudflare-one/

[^1_255]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-availability/system-requirements/

[^1_256]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/private-net/cloudflared/connect-private-hostname/

[^1_257]: https://developers.cloudflare.com/changelog/product-group/developer-platform/18/

[^1_258]: https://developers.cloudflare.com/tunnel/concepts/routing/

[^1_259]: https://developers.cloudflare.com/changelog/product/tunnel/

[^1_260]: https://www.cloudflare.com/plans/

[^1_261]: https://developers.cloudflare.com/api/resources/zero_trust/subresources/networks/subresources/hostname_routes/methods/list/

[^1_262]: https://github.com/clbs-io/cloudflare-tunnel-ingress-controller

[^1_263]: https://github.com/STRRL/cloudflare-tunnel-ingress-controller/blob/master/CLAUDE.md

[^1_264]: https://developers.cloudflare.com/tunnel/guides/kubernetes/

[^1_265]: https://github.com/cloudflare/cloudflare-ingress-controller

[^1_266]: https://github.com/StringKe/cloudflare-operator

[^1_267]: https://github.com/topics/cloudflare?l=go

[^1_268]: https://developers.cloudflare.com/api/resources/zero_trust/subresources/tunnels/subresources/cloudflared/

[^1_269]: https://github.com/mfadhlika/cloudflare-tunnels-operator

[^1_270]: https://developers.cloudflare.com/tunnel/get-started/

[^1_271]: https://kubernetes.io/docs/concepts/services-networking/ingress-controllers/

[^1_272]: https://www.cloudflare.com/integrations/kubernetes/

[^1_273]: https://developers.cloudflare.com/cloudflare-one/tutorials/tunnel-kubectl/

[^1_274]: https://pkg.go.dev/github.com/STRRL/cloudflare-tunnel-ingress-controller@v0.1.0/test/e2e

[^1_275]: https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/

[^1_276]: https://ngrok.com/docs/gateway/site-to-site-connectivity/customer-networks

[^1_277]: https://ngrok.com/docs/gateway/k8s/installation/architecture

[^1_278]: https://ngrok.com/docs/gateway/k8s/changelog

[^1_279]: https://ngrok.com/docs/k8s/changelog

[^1_280]: https://ngrok.com/docs/gateway/k8s/guides/how-to/tcp-routing

[^1_281]: https://ngrok.com/docs/gateway/k8s/guides/bindings

[^1_282]: https://ngrok.com/docs/api-reference/edgestcp/list

[^1_283]: https://ngrok.com/docs/getting-started/kubernetes/gateway-api

[^1_284]: https://ngrok.com/docs/gateway/overview

[^1_285]: https://ngrok.com/docs/gateway/k8s/guides/using-gwapi

[^1_286]: https://ngrok.com/docs/k8s/guides/bindings

[^1_287]: https://ngrok.com/docs/gateway/k8s/how-it-works

[^1_288]: https://ngrok.com/docs/gateway/agent/cli-api.md

[^1_289]: https://ngrok.com/docs/k8s/guides/using-gwapi

[^1_290]: https://ngrok.com/docs/k8s.md

[^1_291]: https://developer.hashicorp.com/boundary/docs/targets

[^1_292]: https://developer.hashicorp.com/boundary/docs/targets/create/tcp/kubernetes

[^1_293]: https://developer.hashicorp.com/boundary/docs/targets/create/tcp

[^1_294]: https://developer.hashicorp.com/boundary/docs/getting-started/dev-mode/connect-to-dev-target

[^1_295]: https://developer.hashicorp.com/boundary/docs/workers

[^1_296]: https://developer.hashicorp.com/boundary/docs/targets/create/tcp/web

[^1_297]: https://www.twingate.com/docs/k8s-private-services

[^1_298]: https://developer.hashicorp.com/boundary/tutorials/community-administration/community-manage-workers

[^1_299]: https://developer.hashicorp.com/boundary/tutorials/worker-management/target-aware-workers

[^1_300]: https://developer.hashicorp.com/boundary/tutorials/get-started-community/community-get-started-config

[^1_301]: https://developer.hashicorp.com/boundary/docs/deploy/helm-chart/worker-examples

[^1_302]: https://developer.hashicorp.com/boundary/docs/deploy/helm-chart/deploy-worker

[^1_303]: https://www.twingate.com/docs/kubernetes-access

[^1_304]: https://developer.hashicorp.com/boundary/docs/targets/connections

[^1_305]: https://developer.hashicorp.com/boundary/sandbox

[^1_306]: https://tailscale.com/docs/kubernetes-operator/concepts/architecture

[^1_307]: https://tailscale.com/docs/kubernetes-operator

[^1_308]: https://goteleport.com/docs/enroll-resources/database-access/faq/

[^1_309]: https://tailscale.com/docs/kubernetes

[^1_310]: https://goteleport.com/docs/enroll-resources/database-access/reference/configuration/

[^1_311]: https://goteleport.com/docs/ver/17.x/enroll-resources/database-access/faq/

[^1_312]: https://tailscale.com/docs/kubernetes-operator/ingress

[^1_313]: https://goteleport.com/docs/enroll-resources/database-access/reference/cli/

[^1_314]: https://github.com/rocket-tsang/tailscale-docs/blob/main/docs/features/kubernetes-operator.md

[^1_315]: https://goteleport.com/docs/enroll-resources/database-access/

[^1_316]: https://tailscale.com/docs/kubernetes-operator/reference/limitations

[^1_317]: https://learn.microsoft.com/en-us/azure/architecture/aws-professional/networking

[^1_318]: https://tailscale.com/docs/kubernetes-operator/quickstart.md

[^1_319]: https://aws.amazon.com/privatelink/

[^1_320]: https://docs.livekit.io/deploy/agents/private-links/

[^1_321]: https://istio.io/latest/news/releases/1.28.x/

[^1_322]: https://istio.io/latest/docs/releases/

[^1_323]: https://istio.io/latest/news/releases/1.29.x/announcing-1.29.8/

[^1_324]: https://istio.io/latest/news/security/

[^1_325]: https://istio.io/latest/news/support/announcing-1.28-eol/

[^1_326]: https://istio.io/latest/news/support/announcing-1.29-eol/

[^1_327]: https://istio.io/latest/news/releases/1.29.x/announcing-1.29.3/

[^1_328]: https://istio.io/latest/news/releases/1.29.x/announcing-1.29.7/

[^1_329]: https://istio.io/latest/feed.xml

[^1_330]: https://istio.io/latest/news/

[^1_331]: https://istio.io/latest/news/releases/1.29.x/announcing-1.29.5/

[^1_332]: https://istio.io/latest/news/support/

[^1_333]: https://istio.io/latest/news/releases/

[^1_334]: https://istio.io/latest/news/support/announcing-1.27-eol/

[^1_335]: https://istio.io/latest/news/releases/1.31.x/announcing-1.31/

[^1_336]: https://istio.io/latest/news/releases/1.31.x/announcing-1.31/upgrade-notes/

[^1_337]: https://istio.io/latest/zh/news/releases/1.31.x/announcing-1.31/upgrade-notes/

[^1_338]: https://preliminary.istio.io/latest/uk/news/releases/1.31.x/announcing-1.31/change-notes

[^1_339]: https://istio.io/latest/zh/news/releases/1.31.x/announcing-1.31/change-notes/

[^1_340]: https://istio.io/latest/news/releases/1.30.x/announcing-1.30/upgrade-notes/

[^1_341]: https://istio.io/latest/docs/ambient/usage/

[^1_342]: https://istio.io/latest/docs/ambient/migrate/enable-ambient-mode/

[^1_343]: https://istio.io/latest/blog/2024/ambient-reaches-ga/

[^1_344]: https://istio.io/latest/news/releases/1.24.x/announcing-1.24/change-notes/

[^1_345]: https://istio.io/latest/docs/ambient/usage/agentgateway/

[^1_346]: https://istio.io/latest/news/releases/1.24.x/announcing-1.24/upgrade-notes/

[^1_347]: https://istio.io/latest/docs/ambient/overview/

[^1_348]: https://istio.io/latest/docs/reference/config/annotations/

[^1_349]: https://istio.io/latest/blog/2024/ambient-reaches-beta/

[^1_350]: https://istio.io/latest/docs/ops/deployment/ambient-mc-perf/

[^1_351]: https://istio.io/latest/docs/releases/feature-stages/

[^1_352]: https://istio.io/latest/docs/ambient/install/multicluster/

[^1_353]: https://istio.io/latest/docs/ops/configuration/traffic-management/multicluster/

[^1_354]: https://istio.io/latest/docs/ops/diagnostic-tools/multicluster/

[^1_355]: https://kgateway.dev/docs/envoy/latest/reference/versions/

[^1_356]: https://agentgateway.dev/docs/kubernetes/1.1.x/release-notes/versions/

[^1_357]: https://docs.solo.io/kgateway/2.2.x/reference/versions/

[^1_358]: https://gateway-api.sigs.k8s.io/docs/implementations/versions/v1.4/

[^1_359]: https://kubespec.dev/gateway-api

[^1_360]: https://kubernetes.io/blog/2026/04/21/gateway-api-v1-5/

[^1_361]: https://docs.solo.io/kgateway/2.3.x/reference/versions/

[^1_362]: https://kubernetes.io/blog/2026/08/03/gateway-api-v1-6-release/

[^1_363]: https://gateway.envoyproxy.io/news/releases/matrix/

[^1_364]: https://docs.solo.io/kgateway/2.1.x/reference/versions/

[^1_365]: https://docs.tetrate.io/envoy-gateway/v1.6.x/support

[^1_366]: https://github.com/kubernetes-sigs/gateway-api/discussions/4538

[^1_367]: https://doc.traefik.io/traefik-hub/api-management/release-notes

[^1_368]: https://gateway-api.sigs.k8s.io/

[^1_369]: https://docs.cilium.io/en/latest/network/kubernetes/compatibility/

[^1_370]: https://docs.cilium.io/en/stable/network/kubernetes/compatibility/

[^1_371]: https://docs.cilium.io/en/stable/operations/upgrade/

[^1_372]: https://www.versio.io/en/product-release-end-of-life-eol-cilium-cilium.html

[^1_373]: https://github.com/cilium/cilium-cli/releases

[^1_374]: https://docs.cilium.io/en/stable/

[^1_375]: https://docs.cilium.io/en/stable/contributing/release/

[^1_376]: https://artifacthub.io/packages/helm/cilium/cilium/1.19.3

[^1_377]: https://en.wikipedia.org/wiki/Cilium\_(computing)

[^1_378]: https://github.com/cilium/cilium/releases/tag/v1.19.8

[^1_379]: https://artifacthub.io/packages/helm/cilium/cilium/1.19.1

[^1_380]: https://github.com/cilium/cilium/pull/46764

[^1_381]: https://github.com/cloudflare/cloudflared/releases/tag/2026.10.0

[^1_382]: https://github.com/cloudflare

[^1_383]: https://developers.cloudflare.com/changelog/

[^1_384]: https://developers.cloudflare.com/changelog/product-group/application-security/

[^1_385]: https://developers.cloudflare.com/tunnel/downloads/

[^1_386]: https://releases.sh/cloudflare/cloudflared.md

[^1_387]: https://community-scripts.org/scripts/cloudflared

[^1_388]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/

[^1_389]: https://cxtoday.com/the-reliability-stories-cx-leaders-cant-afford-to-miss

[^1_390]: https://gigazine.net/gsc_news/en/20261013-cloudflare-deno

[^1_391]: https://github.com/cloudflare/cloudflared/releases?after=2021.8.1

[^1_392]: https://github.com/cloudflare/cloudflared

[^1_393]: https://developers.cloudflare.com/tunnel/guides/update-cloudflared/

[^1_394]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/create-local-tunnel/

[^1_395]: https://learn.microsoft.com/ru-ru/Azure/aks/azure-cni-powered-by-cilium

[^1_396]: https://github.com/Azure/AKS/releases

[^1_397]: https://learn.microsoft.com/en-us/azure/aks/azure-cni-powered-by-cilium

[^1_398]: https://learn.microsoft.com/en-us/azure/aks/concepts-network-azure-cni-overlay

[^1_399]: https://learn.microsoft.com/en-us/azure/architecture/reference-architectures/containers/aks-microservices/aks-microservices-advanced

[^1_400]: https://learn.microsoft.com/en-us/azure/aks/concepts-network-cni-overview

[^1_401]: https://learn.microsoft.com/en-us/azure/application-gateway/for-containers/service-mesh-integration

[^1_402]: https://docs.azure.cn/en-us/aks/azure-cni-powered-by-cilium

[^1_403]: https://learn.microsoft.com/en-us/azure/aks/network-policy-best-practices

[^1_404]: https://learn.microsoft.com/en-us/azure/aks/azure-cni-overlay

[^1_405]: https://docs.azure.cn/en-us/aks/supported-kubernetes-versions

[^1_406]: https://learn.microsoft.com/en-us/azure/aks/istio-upgrade

[^1_407]: https://docs.azure.cn/en-us/aks/istio-cni

[^1_408]: https://docs.azure.cn/en-us//aks/istio-upgrade

[^1_409]: https://newreleases.io/project/github/Azure/AKS/release/2026-07-17

[^1_410]: https://preliminary.istio.io/latest/docs/ambient/migrate/

[^1_411]: https://istio.io/latest/docs/reference/config/istio.mesh.v1alpha1/

[^1_412]: https://docs.azure.cn/en-us/aks/istio-deploy-egress

[^1_413]: https://istio.io/latest/docs/tasks/observability/distributed-tracing/mesh-and-proxy-config/

[^1_414]: https://learn.microsoft.com/lt-lt/AZURE/aks/app-routing-gateway-api

[^1_415]: https://newreleases.io/project/github/Azure/AKS/release/2026-06-19

[^1_416]: https://docs.solo.io/istio/1.31.x/setup/install/aks_extension/

[^1_417]: https://docs.solo.io/istio/1.27.x/ambient/setup/install/aks_extension/

[^1_418]: https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/extensions/istio-add-on-general-troubleshooting

[^1_419]: https://docs.azure.cn/en-us/aks/istio-upgrade

[^1_420]: https://github.com/Azure/aks/releases

[^1_421]: https://docs.nebius.com/kubernetes/networking/add-ons

[^1_422]: https://docs.cilium.io/en/latest/network/kubernetes/kubeproxy-free/

[^1_423]: https://stackoverflow.com/questions/77765700/implications-of-different-cilium-configurations-on-istio-integration-in-kubernet

[^1_424]: https://oneuptime.com/blog/post/2026-02-24-how-to-configure-istio-for-ebpf-based-data-plane/view

[^1_425]: https://www.srodi.com/posts/cilium-ebpf-socket-hooks-for-kubernetes-services/

[^1_426]: https://oneuptime.com/blog/post/2026-02-24-how-to-configure-istio-with-cilium-cni/markdown

[^1_427]: https://linkerd.io/docs/reference/cluster-configuration/

[^1_428]: https://docs.cilium.io/en/latest/cmdref/cilium-agent_hive_dot-graph/

[^1_429]: https://istio.io/latest/uk/docs/ambient/install/platform-prerequisites/

[^1_430]: https://istio.io/latest/zh/docs/ambient/install/platform-prerequisites/

[^1_431]: https://istio.io/latest/blog/2024/ambient-vs-cilium/

[^1_432]: https://istio.io/latest/docs/setup/additional-setup/customize-installation/

[^1_433]: https://istio.io/latest/uk/blog/2024/ambient-vs-cilium/

[^1_434]: https://istio.io/latest/docs/reference/commands/install-cni/

[^1_435]: https://istio.io/latest/news/security/istio-security-2023-005/

[^1_436]: https://docs.cloud.google.com/service-mesh/docs/istio-apis/security/best-practices

[^1_437]: https://istio.io/latest/docs/setup/additional-setup/network-policy/

[^1_438]: https://istio.io/latest/zh/docs/setup/additional-setup/pod-security-admission/

[^1_439]: https://istio.io/latest/zh/docs/setup/upgrade/helm/

[^1_440]: https://preliminary.istio.io/latest/docs/setup/upgrade/

[^1_441]: https://docs.solo.io/istio/1.31.x/sidecar/upgrade/manual/

[^1_442]: https://docs.solo.io/istio/1.29.x/ambient/operations/upgrade/manual/

[^1_443]: https://istio.io/latest/zh/docs/ambient/migrate/install-ambient-components/

[^1_444]: https://docs.solo.io/istio/1.31.x/traffic-management/ingress/overview/

[^1_445]: https://istio.io/latest/docs/tasks/traffic-management/egress/egress-gateway/

[^1_446]: https://docs.solo.io/istio/1.29.x/ambient/traffic-management/egress/egress/

[^1_447]: https://docs.solo.io/istio/1.30.x/ambient/waypoints/overview/

[^1_448]: https://docs.solo.io/istio/1.31.x/ambient/traffic-management/egress/agw-egress/

[^1_449]: https://docs.solo.io/istio/1.27.x/ambient/traffic-management/egress/egress-mtls/

[^1_450]: https://docs.solo.io/istio/1.30.x/ambient/traffic-management/egress/egress-mtls/

[^1_451]: https://istio.io/latest/docs/reference/config/networking/gateway/

[^1_452]: https://docs.solo.io/istio/1.28.x/ambient/traffic-management/egress/egress/

[^1_453]: https://docs.solo.io/istio/1.31.x/traffic-management/egress/overview/

[^1_454]: https://docs.cilium.io/en/stable/contributing/development/debugging/

[^1_455]: https://docs.cilium.io/en/latest/security/dns/

[^1_456]: https://factory.com/open-source-wikis/cilium?page=features/network-policy.md

[^1_457]: https://imroc.cc/tke/en/networking/cilium/networkpolicy

[^1_458]: https://github.com/cilium/cilium/issues/44452

[^1_459]: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/nodelocal-dns-cache

[^1_460]: https://istio.io/latest/uk/docs/ops/configuration/traffic-management/dns-proxy/

[^1_461]: https://istio.io/latest/zh/docs/ops/configuration/traffic-management/dns-proxy/

[^1_462]: https://istio.io/latest/news/releases/1.25.x/announcing-1.25/

[^1_463]: https://docs.nebius.com/kubernetes/networking/nodelocal-dns-cache

[^1_464]: https://kubernetes.io/docs/tasks/administer-cluster/nodelocaldns/

[^1_465]: https://learn.microsoft.com/en-us/azure/architecture/guide/devsecops/devsecops-on-aks

[^1_466]: https://learn.microsoft.com/en-us/azure/aks/container-network-insights-agent-overview

[^1_467]: https://github.com/Azure/AKS/issues/4205

[^1_468]: https://techcommunity.microsoft.com/blog/azurenetworkingblog/securing-microservices-with-cilium-and-istio/4381646

[^1_469]: https://learn.microsoft.com/en-us/azure/aks/how-to-apply-l7-policies

[^1_470]: https://learn.microsoft.com/en-us/azure/aks/migrate-from-npm-to-cilium-network-policy

[^1_471]: https://istio.io/latest/docs/tasks/security/authentication/mtls-migration/

[^1_472]: https://istio.io/latest/docs/ops/configuration/security/security-policy-examples/

[^1_473]: https://istio.io/latest/docs/ops/deployment/security-model/

[^1_474]: https://strimzi.io/docs/operators/0.28.0/full/configuring

[^1_475]: https://strimzi.io/docs/operators/0.30.0/full/configuring

[^1_476]: https://cloudnative-pg.io/docs/1.25/declarative_role_management/

[^1_477]: https://cloudnative-pg.io/docs/1.28/cloudnative-pg.v1/

[^1_478]: https://cloudnative-pg.io/docs/1.30/declarative_database_management/

[^1_479]: https://cloudnative-pg.io/releases/cloudnative-pg-1-30.0-rc1-released/

[^1_480]: https://cloudnative-pg.io/documentation/1.20/operator_capability_levels/

[^1_481]: https://cloudnative-pg.io/documentation/1.20/security/

[^1_482]: https://docs.crossplane.io/v2.3/guides/upgrade-to-crossplane-v2/

[^1_483]: https://docs.crossplane.io/latest/managed-resources/managed-resource-definitions/

[^1_484]: https://devcenter3.assets.heroku.com/articles/platform-api-for-partners

[^1_485]: https://devcenter.heroku.com/articles/how-add-ons-work

[^1_486]: https://developers.cloudflare.com/api/typescript/resources/zero_trust/subresources/tunnels/

[^1_487]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/configuration-file/

[^1_488]: https://developers.cloudflare.com/tunnel/reference/run-parameters/index.md

[^1_489]: https://www.cloudflare.com/plans/.md

[^1_490]: https://www.cloudflare.com/pricing.md

[^1_491]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/ssh/ssh-device-client/

[^1_492]: https://teitunnel.teispace.com/docs/reference/limits/

[^1_493]: https://developers.cloudflare.com/fundamentals/api/reference/permissions/

[^1_494]: https://www.cloudflare.com/plans/zero-trust-services/

[^1_495]: https://developers.cloudflare.com/sandbox/sdk/api/tunnels/

[^1_496]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/private-net/cloudflared/connect-cidr/

[^1_497]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/troubleshoot-tunnels/private-networks/

[^1_498]: https://developers.cloudflare.com/cloudflare-one/tutorials/vnc-client-in-browser/index.md

[^1_499]: https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/infrastructure-apps/

[^1_500]: https://developers.cloudflare.com/cloudflare-one/traffic-controls/network-policies/

[^1_501]: https://developers.cloudflare.com/changelog/product/access/2/

[^1_502]: https://developers.cloudflare.com/api/resources/zero_trust/subresources/access/subresources/applications/subresources/cas/methods/create/

[^1_503]: https://developers.cloudflare.com/cloudflare-one/access-controls/access-settings/session-management/

[^1_504]: https://developers.cloudflare.com/cloudflare-one/tutorials/cli/index.md

[^1_505]: https://developers.cloudflare.com/cloudflare-one/traffic-controls/resolver-endpoints/dns-over-https/

[^1_506]: https://developers.cloudflare.com/api/resources/zero_trust/subresources/access/subresources/service_tokens/methods/get/

[^1_507]: https://developers.cloudflare.com/api/resources/zero_trust/subresources/access/subresources/service_tokens/methods/create/

[^1_508]: https://developers.cloudflare.com/api/python/resources/zero_trust/subresources/access/subresources/policies/methods/create/

[^1_509]: https://developers.cloudflare.com/cloudflare-network-firewall/plans/

[^1_510]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-wan/zero-trust/security-services/

[^1_511]: https://developers.cloudflare.com/cloudflare-wan/zero-trust/security-services/

[^1_512]: https://developers.cloudflare.com/tunnel/deployment-guides/

[^1_513]: https://developers.cloudflare.com/tunnel/guides/

[^1_514]: https://developers.cloudflare.com/fundamentals/reference/network-ports/

[^1_515]: https://developers.cloudflare.com/containers/configuration/outbound-traffic/

[^1_516]: https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/cloudflare-one-client/deployment/firewall/

[^1_517]: https://github.com/cloudflare/cloudflare-docs/blob/production/src/content/partials/cloudflare-one/mesh/pages/guides/run-mesh-in-containers.mdx

[^1_518]: https://developers.cloudflare.com/tunnel/

[^1_519]: https://developers.cloudflare.com/waf/custom-rules/use-cases/require-specific-http-ports/

[^1_520]: https://github.com/STRRL/cloudflare-tunnel-ingress-controller/blob/master/docs/src/content/docs/how-to/rotate-cloudflare-credentials.md

[^1_521]: https://pkg.go.dev/github.com/STRRL/cloudflare-tunnel-ingress-controller@v0.1.0/pkg/controller

[^1_522]: https://github.com/topics/ingress

[^1_523]: https://github.com/topics/ingress-controller

[^1_524]: https://ngrok.com/docs/api-reference/kubernetes-operators

[^1_525]: https://ngrok.com/docs/getting-started/kubernetes/crds

[^1_526]: https://ngrok.com/docs/start

[^1_527]: https://ngrok.com/docs/integrations/kubernetes-ingress/apiops

[^1_528]: https://info.ngrok.com/hubfs/PDFs/ngrok-kubernetes-operator-web.pdf

[^1_529]: https://ngrok.com/docs/gateway/agent

