# Building our own components in C++: research, verbatim

**Asked:** 2026-10-14, through Perplexity deep research (`perplexity_research`): which layers of secure networking to own in C++, which to keep, the C++ building blocks and their licences, how a node proxy would sit with AKS-managed Cilium, how each datastore protocol negotiates TLS, observability, and a staged plan that keeps one API.
**Returned:** verbatim below, citations included. Nothing in it was verified against the sources here; [`README.md`](README.md) says how it is read.

---

**Recommendation:** own the **workspace policy model, operator, identity-to-authorization mapping, credential broker, developer-tunnel control plane, and service-addressed peer gateways**. Do **not** build a second Kubernetes dataplane or make a transparent C++ node proxy a prerequisite for isolation. On this AKS configuration, use its managed Cilium enforcement first; AKS supports Kubernetes `NetworkPolicy` and Cilium L3/L4 policies, but does not support arbitrary changes to its managed `cilium-config`. FQDN filtering and managed network observability require ACNS. [web:1]

### 1. Ownership boundaries

| Layer | Recommendation | Principal risk or constraint |
|---|---|---|
| L3/L4 enforcement | **Keep Cilium; own policy generation.** Have the operator create namespace-wide default-deny ingress and egress, then narrowly scoped `NetworkPolicy`/`CiliumNetworkPolicy` allows for each declared `Connection`. Do not attach competing eBPF enforcement programs to AKS-managed Cilium hooks. | Kubernetes allows are **additive**: a tenant who can create another permissive policy can reopen access. Reserve policy-writing RBAC for the platform and test admissions and exceptions. AKS policies do not apply to `hostNetwork` pods, and `ipBlock` cannot be used to allow pod or node IPs. [web:168][web:1] |
| DNS enforcement | **Keep CoreDNS and, where purchased/enabled, Cilium’s DNS proxy.** With ACNS, generate both a restrictive DNS-query rule and a port-scoped `toFQDNs` connection rule for declared external names. Use pod selectors—not FQDN rules—for in-cluster Services. An own DNS proxy is worthwhile later only if you need portable query authorization or a unified audit API on non-Cilium clusters. | AKS FQDN filtering requires ACNS; its documented FQDN policy requires a DNS inspection rule as well as `toFQDNs`, and does not support Kubernetes Service names. Match the **actual** DNS backend: AKS LocalDNS needs an explicit allowance, while node-local redirection can change the destination endpoint. FQDN policy ultimately allows resolved **IPs**, not a cryptographic binding of every connection to a hostname. [web:1][web:2][web:197] |
| L7 identity and mTLS | **Keep a proven mesh/proxy where general transparent workload mTLS is required; do not lead with an own ztunnel.** Istio’s Rust ztunnel provides L4 identity, mTLS and telemetry without parsing application HTTP; use Envoy for gateways or protocol-specific processing. Preserve Zetta’s existing node-to-node mTLS rather than silently terminating it. A C++ node proxy is a later, separately security-reviewed project. | L4 mesh identity does not authenticate a database user, inject a credential, or inspect an application’s TLS payload. Transparent capture is a security boundary, not merely a socket-forwarding feature. [web:3][web:4] |
| Scoped datastore credentials | **Own the broker and authorization semantics; implement one explicit proxy endpoint per supported protocol only when needed.** Prefer each datastore’s native roles/ACLs and short-lived, connection-scoped credentials. Start with Postgres; add Valkey and Kafka after proving their client semantics. PgBouncer/PgCat are useful references, **not** a generic injector; Envoy’s Postgres filter is experimental, primarily decodes/exports metadata, and stops decoding encrypted sessions. Envoy’s credential-injector filter targets **HTTP** headers, not these database protocols. [web:302][web:226][web:51][web:54] | An unauthenticated local listener or shared upstream credential would turn a “scoped” connection into a namespace-wide escalation path. Authenticate the calling workload, bind its identity to an allowed service/role, and make bypass of the proxy impossible by policy. |
| Developer tunnels | **Own the product-facing authorization, enrollment, audit and endpoint naming.** Begin with an explicit TCP forwarder and self-hosted relay; consider QUIC/HTTP-3 for multiplexing and outbound-only connectivity. Do not require Cloudflare Tunnel for the air-gapped product: `cloudflared` is Apache-2.0, but its service connects to Cloudflare’s network. `frp` and `rathole` are permissively licensed self-hostable comparisons, not reasons to invent a transport immediately. [web:215][web:212][web:115][web:221] | Require laptop/user authentication, service-scoped grants, expiry, revocation and audit. Treat an endpoint reachable from a developer laptop as an ingress path that needs a corresponding narrow workspace policy. |
| Cross-cluster and customer-VPC peering | **Own service-addressed gateways, not pod-IP routing.** Assign each side stable logical service identities; gateways authenticate peers, authorize a named service/port, and connect to a **local** selected backend. A WireGuard link may protect gateway-to-gateway transport, but by itself neither resolves overlapping addresses nor supplies per-service authorization. Istio multi-network east-west gateways are the established comparison. | Standard Cilium Cluster Mesh requires non-overlapping PodCIDRs and configuration of both Cilium clusters—poor fit for overlapping `10.224.0.0/12` and AKS-managed Cilium. Istio ambient multi-network multicluster offers gateways, but its current install guide still lists topology, service-scope and waypoint limitations; evaluate those against the required production topology. Never advertise overlapping remote pod or VPC addresses as directly routable. [web:31][web:3][web:4] |

An AWS customer-VPC peer should therefore be a **customer-installed gateway** with an approved route or outbound connection to your gateway, exposing only approved services. Do not promise transparent access to arbitrary addresses in that VPC: address overlap, customer routing approval and customer firewalls remain separate problems.

### 2. C++ building blocks and license fit

| Use | Building block and license | How to use it |
|---|---|---|
| Proxy/gateway | **Envoy — Apache-2.0**; **Envoy Mobile — Apache-2.0** for a client-oriented library. [web:72][web:69] | Prefer a separately deployed, configured Envoy or a maintained extension over treating the full Envoy server as a small, stable embeddable C++ library. |
| BPF tooling | **libbpf — BSD-2-Clause *or* LGPL-2.1**, at the adopter’s choice; Cilium userspace is Apache-2.0 and its BPF templates have a BSD-2-Clause alternative. [web:46][web:297] | Suitable for later portable collectors; a permissive license does **not** grant permission to alter managed AKS dataplane attachments. |
| TLS and identity | **OpenSSL 3+ — Apache-2.0**; **BoringSSL — mixed legacy OpenSSL/SSLeay, ISC and component licenses**, rather than “Apache-2.0.” **SPIRE — Apache-2.0**, exposing the SPIFFE Workload API and Envoy SDS. **c-spiffe — Apache-2.0**, but SPIFFE describes that C/C++ library as outside its official repo and still developing: assess maintenance before making it foundational. [web:255][web:73][web:256][web:52] | For C++, a small Workload-API client or SPIRE SDS-to-Envoy integration is a sounder initial dependency than an unreviewed identity library. Verify workload attestation and certificate rotation, not just certificate issuance. |
| DNS and event loops | **c-ares — MIT** resolver; **Boost.Asio — Boost-1.0**; **Seastar — Apache-2.0**; **liburing — LGPL/MIT dual licensing for its main code**. [web:181][web:241][web:227][web:251] | These help implement a proxy or gateway. c-ares is a **client resolver**, not an enforcing DNS server; `io_uring` is an optimization, not a policy architecture. |
| QUIC/HTTP-3 | **MsQuic — MIT**, C API with C++ support; **ngtcp2 — MIT**, QUIC transport; **quiche — BSD-2-Clause**, Rust with a C FFI. [web:193][web:70][web:106][web:315] | Start with MsQuic if a substantially C/C++ implementation matters. Budget separately for relay authorization, multiplexing, reconnects, congestion behavior and HTTP/3 integration; a QUIC library is not a tunnel product. |
| WireGuard | **Linux kernel WireGuard — GPL-2.0 kernel component**; its `embeddable-wg-library` is **LGPL-2.1+** and **configures kernel interfaces**, rather than implementing a WireGuard cipher/transport library. **BoringTun — BSD-3-Clause**, Rust with a C ABI; **wireguard-go — MIT**, Go; `wireguard-tools` is GPL-2.0. [web:79][web:77][web:303][web:314][web:76] | A proprietary C++ gateway can use the OS kernel interface through an appropriately reviewed netlink implementation without copying GPL code into the product. If **every bundled component must itself be permissively licensed**, exclude the LGPL helper and GPL tools from the shipped component set; review customer OS/kernel obligations separately. |
| Protocol/relay precedents | **PgBouncer — ISC**; **PgCat — MIT**, Rust; **cloudflared, frp, rathole — Apache-2.0**. [web:302][web:226][web:215][web:115][web:221] | License fit and production utility are different questions: test required authentication, pooling and relay behavior, and vendor an air-gap-installable dependency set. |

### 3. A node proxy on AKS: feasible, but not the first enforcement boundary

| Question | Concrete answer |
|---|---|
| How would it coexist with Cilium? | Leave Azure CNI overlay/Cilium as the **primary CNI and policy enforcer**. A privileged, cluster-installed node agent would have to arrange transparent redirection **inside selected pods’ network namespaces**, hand namespace context to a node proxy, and exempt the proxy’s own sockets to avoid loops. Istio ambient does this using a chained CNI notification plugin, an `istio-cni` node agent, in-pod redirection rules, and netns file descriptors passed to ztunnel. None requires `NET_ADMIN` in the application container—but the platform-owned installer/agent does require privileged node integration. Verify that the customer’s managed or locked-down cluster permits that integration before offering the feature. [web:4] |
| What does Restricted Pod Security permit? | Keep customer apps and agents under **Restricted**: no added `NET_ADMIN`, privileged container, `hostNetwork`, or hostPath access. Put any privileged node infrastructure in a separately governed system namespace; do not claim Restricted applies to that infrastructure. [web:167][web:176] |
| Where can it fail open or break? | Pod creation/enrollment races, missed restarts, traffic excluded from capture, proxy self-loops, original-destination/identity attribution, DNS and UDP behavior, and policy evaluation on the **encapsulated** path all need tests. Istio calls capture security-critical: bypass can bypass authorization. Its ambient policy guidance also notes that existing default-deny policies need an explicit allowance for tunneled port `15008`; the tunnel does **not** bypass Kubernetes policy. Specify fail-closed behavior and prove it during agent or proxy failure. [web:4][web:166] |

### 4. TLS negotiation and why “transparent credential injection” is not generic

| Store / protocol | TLS initiation | Consequence for an injecting proxy |
|---|---|---|
| CloudNativePG / PostgreSQL | Traditionally client `SSLRequest`, server `S`/`N`, then TLS; PostgreSQL 17+ also supports immediate TLS negotiation. [web:101][web:91] | A TCP pass-through cannot rewrite authentication inside TLS. A Postgres-aware proxy must handle both startup forms, terminate the client’s verified TLS, authenticate its caller, and establish a separately verified upstream TLS/authenticated session. |
| TiDB / MySQL protocol | Server sends its greeting; client can send `SSLRequest`, establish TLS, then send its authentication response. [web:138] | Different handshake order from Postgres. Requires a MySQL-aware endpoint and explicit handling of TiDB-supported authentication methods—not a generic TLS terminator. |
| Strimzi / Kafka | Listener selects plaintext, SSL, SASL_PLAINTEXT or SASL_SSL; on SASL_SSL, TLS precedes Kafka SASL exchanges. [web:94] | An injector must handle TLS and SASL **and** broker discovery. Clients follow broker addresses returned in metadata; an externally accessed gateway must supply reachable advertised listeners or correctly proxy/rewrite discovery, not merely forward the bootstrap socket. [web:157][web:159] |
| Cassandra / CQL native | Client-native TLS is configured on the native transport, ordinarily port `9042`; Cassandra also has separate internode encryption configuration. [web:145] | Treat client and internode paths separately. Passing native TLS through preserves end-to-end encryption but hides client authentication and CQL from the proxy. |
| Valkey / RESP2 or RESP3 | TLS is enabled on a TLS listener; application authentication then uses `AUTH` or `HELLO ... AUTH`, unless configured certificate-based identity applies. [web:152][web:151] | A Valkey-aware terminating proxy can map workload identity to a restricted ACL user. For pass-through TLS it cannot insert `AUTH`; do not mistake successful client-certificate validation for a particular ACL user unless configured that way. |
| ClickHouse native | TLS wraps the TCP transport, conventionally secure native port `9440`; native `ClientHello` carries user/password **inside** that transport. [web:143][web:136] | Terminate TLS and understand the native handshake to inject credentials, or leave TLS end-to-end and supply credentials to the client by another approved mechanism. |
| Zetta | Already uses mTLS between its nodes, as specified. | Preserve that node identity and TLS end-to-end. Add a separately designed client-facing authentication path if customer apps need scoped Zetta access; avoid double-proxying node traffic by default. |

For all terminating proxies, the security contract is **client → verified proxy → verified datastore**, with a documented change in TLS trust boundary. Do not advertise pass-through encryption and in-band credential replacement for the same connection.

### 5. Observability

| Source | Useful signals | Limit |
|---|---|---|
| Cilium/Hubble, or AKS ACNS network logs | Pod/namespace identity, peers, addresses/ports, direction, flow and policy verdicts, drops, DNS events, traffic metrics; use these first for denied-connection and policy tests. ACNS provides Hubble integration and is a **paid AKS offering**, so ship an independent observability path for the customer-cluster/air-gapped edition. Hubble itself is Apache-2.0. [web:287][web:290][web:291] | L7 visibility depends on interception/policy and protocol; a flow record is not a complete transaction trace. [web:123] |
| Your explicit proxy/gateway | Authenticated workload/user/peer identity, requested logical service, policy decision, connection count and bytes, connect/TLS latency, upstream choice, retries, credential-issuance outcome, and—only where the protocol is terminated—selected L7 timings. | Record credential **identifiers**, not passwords or tokens. Bound cardinality and retention per tenant; never promise SQL/command visibility across untouched end-to-end TLS. |
| Pixie-derived collectors | Pixie is Apache-2.0; its eBPF syscall probes feed userspace protocol parsing, while TLS-library uprobes can observe data before encryption. Its C++/eBPF collector code can be **evaluated for reuse**, rather than assuming its entire control plane is needed. [web:135][web:121] | Reuse entails kernel, TLS-library, runtime and AKS privilege compatibility testing, plus redaction and tenant isolation. Uprobes are not a universal decryptor and are a poor default mechanism for collecting customers’ database payloads. |

### 6. Staged implementation, preserving one API

| Stage | Stable resource and implementation | Exit test |
|---|---|---|
| **0 — contract and bootstrap** | Define `Sandbox` (workspace namespace and security defaults), `Connection` (source workload → named destination, port/protocol and optional credential scope), `Tunnel` (user/device → allowed service), and `PeerConnection` (cluster/customer gateway → logical service). Reconcile **default-deny before admitting tenant workloads**; enforce Restricted admission, controlled pod labels and service accounts, and platform-only policy-writing RBAC. | A fresh workspace, including its operators’ datastore pods, has no unintended cross-workspace or internet path; a tenant cannot add an allow-all policy, use `hostNetwork`, or select another tenant’s identity. NetworkPolicy’s additive semantics make the RBAC check essential. [web:168][web:1][web:167] |
| **1 — minimum viable sandbox** | Generate a namespace-wide ingress **and** egress default-deny. Generate exact source/destination pod-selector and TCP/UDP-port allows for each `Connection`, including both sides where needed. Add only the actual DNS resolver on TCP/UDP 53. **Require ACNS for the AKS profile that promises per-name external DNS and egress**: generate DNS `matchName`/narrow patterns plus corresponding port-scoped `toFQDNs`; otherwise offer only explicit IP/CIDR egress or a separately enforced DNS-gateway design—do not claim Kubernetes `NetworkPolicy` filters DNS questions. Allow necessary cluster-control/operator traffic explicitly, without blanket namespace-to-namespace access. [web:1][web:2][web:168] | From app, agent and each datastore pod, test allowed and forbidden ingress, intra-/inter-workspace, external IP literals, allowed/disallowed DNS names, UDP **and** TCP DNS, search-suffix and CNAME behavior, LocalDNS/node-local DNS, DNS failure, restarts and policy propagation. Confirm observed drops and accepted flows on the **actual AKS 1.35** configuration, not just a local test cluster. [web:1][web:2][web:290] |
| **2 — scoped access and tunnels** | Issue short-lived service/role grants tied to an authenticated workload; expose an **explicit** Postgres proxy endpoint first and restrict direct datastore ingress. Add a self-hosted authenticated developer relay using the same `Tunnel` resource. | Revocation, credential rotation, client TLS verification, denied role escalation, direct-to-database bypass, laptop disconnect/reconnect and per-user audit all pass. |
| **3 — overlapping-network peers** | Implement `PeerConnection` as mutually authenticated, **service-named** gateways. Select local Services at each end; optionally protect gateway transport with QUIC or OS WireGuard. Never put the overlapping remote CIDR into a workload’s routing table merely to make a demo work. | Two clusters with identical `10.224.0.0/12` addresses and a customer VPC can access only explicitly exported services; test duplicate pod IPs, peer revocation, gateway loss and failover. Cilium Cluster Mesh’s non-overlap prerequisite is the negative control. [web:31] |
| **4 — optional C++ replacement** | Replace an **individual adapter**—relay, Postgres proxy, DNS authorization gateway, or finally a node proxy—behind the unchanged resource API. Keep Cilium L3/L4 policy as defense in depth. | Demonstrate no bypass under node churn, policy updates and proxy failure; compare latency, throughput, observability and upgrade behavior with the retained implementation before switching defaults. Istio’s CNI/ztunnel capture design is the minimum engineering benchmark, not merely an implementation sketch. [web:4] |

**Hard product boundary:** an AKS+ACNS implementation and a resellable, air-gapped implementation can share these four resources, but need separate capability profiles and install tests. Do not make a paid Azure feature, an external Cloudflare connection, arbitrary managed-Cilium configuration, or a privileged node hook an unstated requirement for the customer-cluster edition. [web:1][web:290][web:212]

Citations:
[16] https://learn.microsoft.com/en-us/azure/aks/azure-cni-powered-by-cilium
[17] https://learn.microsoft.com/en-us/azure/aks/network-policy-best-practices
[18] https://learn.microsoft.com/en-us/azure/aks/use-network-policies
[19] https://learn.microsoft.com/en-us/azure/architecture/reference-architectures/containers/aks-microservices/aks-microservices-advanced
[20] https://aks-learning.github.io/docs/security/network-security/
[21] https://docs.azure.cn/en-us/aks/container-network-security-fqdn-filtering-concepts
[22] https://techcommunity.microsoft.com/blog/azurenetworkingblog/secure-high-performance-networking-for-data-intensive-kubernetes-workloads/4271478
[23] https://docs.cilium.io/en/latest/security/policy/intro/
[24] https://github.com/Azure/AKS/issues/3450
[25] https://docs.azure.cn/pt-pt/aks/azure-cni-powered-by-cilium
[26] https://github.com/Azure/AKS/issues/4205
[27] https://azure-samples.github.io/aks-labs/docs/networking/acns-lab/
[28] https://docs.cilium.io/en/latest/network/servicemesh/default-deny-ingress-policy/
[29] https://docs.azure.cn/en-us/aks/how-to-apply-fqdn-filtering-policies
[30] https://aks-learning.github.io/docs/networking/concepts/
[1] https://istio.io/latest/docs/ambient/architecture/data-plane/
[2] https://istio.io/latest/docs/ambient/usage/l4-policy/
[3] https://istio.io/latest/docs/ambient/overview/
[4] https://docs.solo.io/istio/1.31.x/troubleshooting/service-mesh/ambient/
[5] https://istio.io/latest/docs/ambient/architecture/traffic-redirection/
[6] https://aws.amazon.com/blogs/containers/better-together-amazon-eks-auto-mode-and-istio-ambient-mesh/
[7] https://istio.io/latest/blog/2024/inpod-traffic-redirection-ambient/
[8] https://istio.io/latest/blog/2022/introducing-ambient-mesh/?s=09
[9] https://devopscube.com/istio-architecture/
[10] https://www.masterthemesh.com/solo/istio-ambient-crds/
[11] https://github.com/istio/istio/blob/master/architecture/ambient/ztunnel.md
[12] https://www.solo.io/blog/traffic-ambient-mesh-istio-cni-node-configuration
[13] https://istio.io/latest/docs/ambient/migrate/install-ambient-components/
[14] https://www.solo.io/blog/istio-ambient-mesh-any-cni
[15] https://istio.io/latest/blog/2022/get-started-ambient/
[31] https://docs.cilium.io/en/latest/network/clustermesh/setup/
[32] https://docs.cilium.io/en/stable/network/clustermesh/clustermesh/
[33] https://docs.cilium.io/en/stable/network/clustermesh/setup/
[34] https://cilium.io/blog/2026/06/13/multi-cluster-kubernetes-explained/
[35] https://istio.io/latest/docs/ops/diagnostic-tools/multicluster/
[36] https://chrisjaimon.com/writeups/one-mesh-two-clouds
[37] https://istio.io/latest/blog/2025/ambient-multicluster/
[38] https://notes.kodekloud.com/docs/Prep-Course-Cilium-Certified-Associate-CCA-Certification/Cluster-Mesh/Demo-Configuring-Cluster-Mesh/page
[39] https://devfloor9.github.io/engineering-playbook/en/docs/eks-best-practices/networking-performance/service-mesh/multi-cluster-communication
[40] https://docs.kubermatic.com/kubermatic/v2.30/tutorials-howtos/networking/cilium-cluster-mesh/
[41] https://notes.kodekloud.com/docs/Prep-Course-Cilium-Certified-Associate-CCA-Certification/Cluster-Mesh/Cluster-Mesh-Basics/page
[42] https://controlplane.com/blog/post/kubernetes-multi-cloud-orchestration
[43] https://docs.daocloud.io/en/network/modules/cilium/notSupport.html
[44] https://docs.kubermatic.com/kubermatic/v2.27/tutorials-howtos/networking/cilium-cluster-mesh/
[45] https://docs.kubermatic.com/kubermatic/v2.29/tutorials-howtos/networking/cilium-cluster-mesh/
[46] https://android.googlesource.com/platform/external/libbpf/+/refs/heads/master
[47] https://android.googlesource.com/platform/external/libbpf/+/fa4d00254d97d09211cc437c0746530bcd098b59
[48] https://android.googlesource.com/platform/external/libbpf/
[49] https://github.com/HewlettPackard/c-spiffe
[50] https://github.com/torvalds/linux/blob/master/tools/lib/bpf/libbpf.c
[51] https://www.envoyproxy.io/docs/envoy/latest/configuration/listeners/network_filters/postgres_proxy_filter
[52] https://spiffe.io/docs/latest/deploying/libraries/
[53] https://lxr.missinglinkelectronics.com/linux/tools/lib/bpf/libbpf.h
[54] https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/credential_injector_filter
[55] https://spiffe.io/docs/latest/spiffe-specs/spiffe_workload_api/
[56] https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/security/external_deps
[57] https://packages.fedoraproject.org/pkgs/libbpf/libbpf/
[58] https://spdx.org/licenses/LGPL-2.1.html
[59] https://android.googlesource.com/platform/external/libbpf/+/refs/tags/aml_neu_341010000/src/bpf_core_read.h
[60] https://spdx.org/licenses/index.html
[61] https://github.com/envoyproxy/envoy-openssl
[62] https://github.com/envoyproxy/envoy/blob/main/BUILD
[63] https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/security/external_deps
[64] https://fossies.org/linux/envoy/docs/BUILD
[65] https://github.com/valkey-io/valkey/issues/1652
[66] https://en.wikipedia.org/wiki/QUIC
[67] https://tetrate.io/press/tetrate-open-source-extensions-marketplace-built-on-envoy
[68] https://pkg.go.dev/github.com/tetratelabs/built-on-envoy
[69] https://github.com/envoyproxy/envoy/blob/main/mobile/README.md
[70] https://github.com/ngtcp2/ngtcp2
[71] https://packagehub.suse.com/packages/ngtcp2/
[72] https://github.com/envoyproxy/envoy/blob/main/LICENSE
[73] https://boringssl.googlesource.com/boringssl.git/+/ab578adf44b3e70255686e3a4009d7d35e6f1231/LICENSE
[74] https://packages.gentoo.org/packages/net-libs/quiche
[75] https://kgateway.dev/docs/envoy/main/reference/osa/
[76] https://github.com/WireGuard/wireguard-tools
[77] https://github.com/WireGuard/wireguard-tools/blob/master/contrib/embeddable-wg-library/README
[78] https://git.openwrt.org/openwrt/openwrt/tree/?path=package/network/utils/wireguard-tools/Makefile
[79] https://www.wireguard.com/
[80] https://www.wireguard.com/embedding/
[81] https://github.com/WireGuard/wireguard-monolithic-historical/blob/master/contrib/examples/embeddable-wg-library/wireguard.c
[82] https://www.nuget.org/packages/Poteko.EmbeddableWireGuard.Net
[83] https://github.com/torvalds/linux/blob/master/drivers/net/wireguard/netlink.c
[84] https://deepwiki.com/WireGuard/wireguard-tools/10-additional-tools-and-libraries
[85] https://layers.openembedded.org/layerindex/recipe/60781/
[86] https://github.com/WireGuard/wireguard-tools/blob/master/contrib/embeddable-wg-library/wireguard.h
[87] https://www.rpmfind.net/linux/RPM/almalinux-kitten/10/appstream/aarch64/wireguard-tools-1.0.20210914-5.el10.aarch64.html
[88] https://deepwiki.com/WireGuard/wireguard-tools/10.1-embeddable-wireguard-library
[89] https://git.zx2c4.com/wireguard-linux/commit/Documentation/netlink?h=devel&amp;id=8f7aa3d3c7323f4ca2768a9e74ebbe359c4f8f88
[90] https://github.com/pia-foss/desktop/blob/master/LICENSE.txt
[91] https://node-postgres.com/features/ssl
[92] https://kafka.apache.org/34/security/authentication-using-sasl/
[93] https://www.postgresql.eu/events/pgconfeu2024/sessions/session/5897/slides/589/Postgres%20protocol.pdf
[94] https://kafka.apache.org/43/security/authentication-using-sasl/
[95] https://docs.rs/postgres/latest/postgres/config/enum.SslNegotiation.html
[96] https://kafka.apache.org/41/security/authentication-using-sasl/
[97] https://axonops.com/docs/data-platforms/kafka/security/authentication/
[98] https://jdbc.postgresql.org/documentation/ssl/
[99] https://www.conduktor.io/kafka-security.md
[100] https://docs.rs/tokio-postgres/latest/tokio_postgres/config/enum.SslNegotiation.html
[101] https://www.postgresql.org/docs/current/protocol-flow.html
[102] https://kafka.apache.org/40/security/authentication-using-sasl/
[103] https://vertx.io/docs/apidocs/io/vertx/pgclient/SslNegotiation.html
[104] https://www.conduktor.io/glossary/kafka-security-best-practices
[105] https://pkg.go.dev/github.com/grepplabs/kafka-proxy
[106] https://github.com/cloudflare/quiche
[107] https://github.com/internet-of-presence/cloudflare-quiche
[108] https://github.com/cloudflare/developer-platform/blob/main/LICENSE
[109] https://github.com/fatedier/frp/
[110] https://github.com/cloudflare/quiche-mallard
[111] https://github.com/cloudflare/computer/blob/main/LICENSE
[112] https://hysenlabs.com/projects/cloudflare-quiche
[113] https://github.com/fatedier/frp/blob/dev/cmd/frps/root.go
[114] https://github.com/cloudflare/cloudflare-docs
[115] https://github.com/fatedier/frp/pkgs/container/frps
[116] https://github.com/fatedier/fft/blob/master/LICENSE
[117] https://archlinux.org/packages/extra/x86_64/quiche/
[118] https://formulae.brew.sh/formula/cloudflare-quiche
[119] https://github.com/orgs/cloudflare/repositories
[120] https://api.github.com/licenses/bsd-2-clause
[136] https://clickhouse.com/docs/concepts/features/security/tls/configuring-tls
[137] https://clickhouse.com/docs/products/kubernetes-operator/guides/tls
[138] https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase.html
[139] https://clickhouse.com/docs/ru/concepts/features/security/tls/configuring-tls.md
[140] https://clickhouse.com/docs/zh/concepts/features/security/tls/configuring-tls
[141] https://dev.mysql.com/doc/dev/mysql-server/9.7.1/page_protocol_connection_phase.html
[142] https://dev.mysql.com/doc/dev/mysql-server/9.7.0/page_protocol_connection_phase_packets_protocol_handshake_v10.html
[143] https://clickhouse.com/docs/resources/develop-contribute/native-protocol/protocol.md
[144] https://clickhouse.com/docs/ko/integrations/language-clients/go/config-reference
[145] https://cassandra.apache.org/doc/latest/cassandra/managing/configuration/cass_yaml_file.html
[146] https://yandex.cloud/en/docs/managed-clickhouse/qa/connection
[147] https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_handshake_v10.html
[148] https://clickhouse.com/docs/integrations/language-clients/go/config-reference
[149] https://axonops.com/docs/data-platforms/cassandra/reference/
[150] https://cassandra.apache.org/doc/latest/cassandra/overview/faq/index.html
[121] https://docs.px.dev/about-pixie/pixie-ebpf/
[122] https://blog.px.dev/ebpf-openssl-tracing/
[123] https://docs.cilium.io/en/stable/observability/visibility/
[124] https://docs.cilium.io/en/latest/observability/visibility/
[125] https://learn.microsoft.com/en-us/azure/aks/container-network-observability-guide
[126] https://github.com/OneUptime/blog/blob/master/posts/2026-03-14-setup-policy-cilium-observability/README.md
[127] https://lpc.events/event/11/contributions/945/attachments/896/1750/Paper%20(PDF).pdf
[128] https://blog.px.dev/ebpf-tls-tracing-past-present-future/
[129] https://veducate.co.uk/cilium-network-policies-from-first-principles-to-production/
[130] https://www.groundcover.com/guides/ebpf-auto-instrumentation-explained
[131] https://dev.co/observability/open-source/pixie
[132] https://cilium.isovalent.com/hubfs/marketing%20briefs/Isovalent%20-%20Cilium%20Hubble%20Cheat%20Sheet.pdf
[133] https://gittrend.io/repo/pixie-io/pixie
[134] https://blog.px.dev/open-source-release/
[135] https://github.com/pixie-io/pixie
[181] https://github.com/c-ares/c-ares
[182] https://github.com/c-ares/c-ares/blob/main/LICENSE.md
[183] https://github.com/sailfishos/c-ares/blob/master/LICENSE
[184] https://github.com/microsoft/msquic
[185] https://github.com/microsoft/msquic/blob/main/THIRD-PARTY-NOTICES
[186] https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/licensing-a-repository
[187] https://boringssl.googlesource.com/boringssl.git/+/ab578adf44b3e70255686e3a4009d7d35e6f1231/LICENSE
[188] https://api.github.com/licenses
[189] https://github.com/c-ares/c-ares/blob/main/src/tools/adig.c
[190] https://boringssl.googlesource.com/boringssl/+/7f02881e96e51f1873afcf384d02f782b48967ca/LICENSE
[191] https://en.wikipedia.org/wiki/BoringSSL
[192] https://github.com/microsoft/msquic/blob/main/src/inc/quic_platform_winkernel.h
[193] https://techcommunity.microsoft.com/blog/networkingblog/msquic-is-open-source/1345441
[194] https://knowledgebase.jedox.com/jedox/third-party-software.htm
[195] https://docs.github.com/en/enterprise-server@3.19/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/licensing-a-repository
[151] https://valkey.io/commands/hello/
[152] https://valkey.io/topics/tls/
[153] https://revisit.tech/blog/pgbouncer-and-its-alternatives-revisit-your-postgresql-connection-pooler/
[154] https://redis.io/docs/latest/operate/oss_and_stack/management/security/
[155] https://redis.io/docs/latest/commands/hello/
[156] https://redis.io/docs/latest/operate/oss_and_stack/management/security/encryption/
[157] https://www.conduktor.io/blog/connect-kafka-across-vpcs-without-extra-peering
[158] https://valkey.io/topics/acl/
[159] https://www.conduktor.io/blog/what-a-kafka-proxy-can-do
[160] https://github.com/valkey-io/valkey/blob/7.2/valkey.conf
[161] https://redis.io/docs/latest/operate/rs/8.0/security/encryption/tls/
[162] https://github.com/cloudnative-pg/pgbouncer-containers/blob/main/README.md
[163] https://github.com/cloudnative-pg/pgbouncer-containers
[164] https://redis.io/docs/latest/operate/rs/7.8/security/encryption/tls/
[165] https://redis.io/docs/latest/operate/rs/security/encryption/tls/
[166] https://istio.io/latest/docs/ambient/usage/networkpolicy/
[167] https://kubernetes.io/docs/concepts/security/pod-security-standards/
[168] https://kubernetes.io/docs/concepts/services-networking/network-policies/
[169] https://istio.io/latest/zh/docs/ambient/usage/networkpolicy/
[170] https://istio.io/latest/uk/docs/ambient/usage/networkpolicy/
[171] https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security
[172] https://computingforgeeks.com/kubernetes-pod-security-standards/
[173] https://k8s.info/docs/advanced/pod-security
[174] https://www.upwind.io/glossary/kubernetes-security-best-practices-secure-clusters-workloads-runtime
[175] https://docs.dynatrace.com/docs/ingest-from/setup-on-k8s/guides/networking-security-compliance/security-configurations/pod-security-standards
[176] https://kubernetes.io/docs/reference/access-authn-authz/psp-to-pod-security-standards/
[177] https://kubernetes.io/docs/reference/kubernetes-api/networking/network-policy-v1/
[178] https://github.com/msahihi/KCSA-study-guide/blob/main/domains/04-kubernetes-threat-model/privilege-escalation.md
[179] https://oneuptime.com/blog/post/2026-01-19-kubernetes-pod-security-standards-pss/view
[180] https://www.vcluster.com/docs/vcluster/security/shared-nodes-hardening
[211] https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-with-firewall/
[212] https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/index.md
[213] https://developers.cloudflare.com/tunnel/
[214] https://github.com/cloudflare/cloudflare.github.io
[215] https://github.com/cloudflare/cloudflared/blob/HEAD/LICENSE
[216] https://github.com/cloudflare/cloudflare-os/blob/main/LICENSE
[217] https://github.com/cloudflare/cloudflare.github.io/blob/master/LICENSE
[218] https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/troubleshoot-tunnels/connectivity-prechecks/
[219] https://github.com/cloudflare/cloudflared/blob/master/LICENSE?ref=apidog.com
[220] https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/rdp/rdp-browser/
[221] https://github.com/rapiz1/rathole/blob/main/LICENSE
[222] https://developers.cloudflare.com/cloudflare-wan/legal/3rdparty/
[223] https://github.com/cloudflare/cloudflare-docs/issues/28227
[224] https://github.com/cloudflare/ci/blob/main/LICENSE
[225] https://github.com/cloudflare/terraform-provider-cloudflare/blob/main/LICENSE
[226] https://github.com/postgresml/pgcat
[227] https://github.com/scylladb/seastar
[228] https://github.com/scylladb/seastar/blob/master/LICENSE
[229] https://github.com/perplexityai/pgcat
[230] https://github.com/avikivity/seastar
[231] https://github.com/open-io/oio-seastar
[232] https://github.com/postgresml/pgcat/blob/main/README.md
[233] https://github.com/postgresml/pgcat/blob/main/LICENSE
[234] https://gittrend.io/repo/postgresml/pgcat
[235] https://github.com/scylladb/seastar/blob/master/include/seastar/core/reactor.hh
[236] https://hysenlabs.com/projects/scylladb-seastar
[237] https://github.com/scylladb/seastar/blob/master/include/seastar/core/seastar.hh
[238] https://www.boost.org/doc/libs/latest/doc/html/boost_asio/using.html
[239] https://github.com/avikivity/seastar/tree/io_uring
[240] https://github.com/scylladb/seastar/blob/master/src/core/resource.cc
[196] https://docs.cilium.io/en/latest/security/dns/
[197] https://docs.cilium.io/en/stable/security/policy/layer7/
[198] https://docs.cilium.io/en/stable/security/policy/language/
[199] https://docs.cilium.io/en/latest/security/policy/layer3/
[200] https://factory.com/open-source-wikis/cilium?page=features/network-policy.md
[201] https://learn.microsoft.com/en-us/azure/aks/azure-cni-powered-by-cilium
[202] https://docs.cilium.io/en/latest/contributing/development/debugging/
[203] https://docs.cilium.io/en/latest/security/policy/troubleshooting/
[204] https://techcommunity.microsoft.com/blog/linuxandopensourceblog/scaling-dns-on-aks-with-cilium-nodelocal-dnscache-lrp-and-fqdn-policies/4486323
[205] https://imroc.cc/tke/en/networking/cilium/networkpolicy
[206] https://veducate.co.uk/cilium-network-policies-from-first-principles-to-production/
[207] https://pkg.go.dev/github.com/cilium/cilium/pkg/fqdn
[208] https://docs.cilium.io/en/stable/contributing/development/debugging/
[209] https://support.kaspersky.com/help/KCS/2.1/en-US/302287.htm
[210] https://docs.cilium.io/en/stable/operations/troubleshooting/
[256] https://github.com/spiffe/spire
[257] https://istio.io/latest/blog/2026/ambient-multinetwork-multicluster-beta/
[258] https://github.com/istio/ztunnel/
[259] https://spiffe.io/docs/latest/deploying/spire_agent/
[260] https://istio.io/latest/docs/ambient/install/multicluster/
[261] https://istio.io/latest/docs/ops/deployment/ambient-mc-perf/
[262] https://github.com/istio/ztunnel/blob/master/LICENSE
[263] https://spiffe.io/docs/latest/deploying/svids/
[264] https://github.com/spiffe/spire/tree/0.8.0
[265] https://istio.io/latest/docs/ambient/install/multicluster/multi-primary_multi-network/
[266] https://docs.solo.io/istio/1.30.x/ambient/about/multicluster/
[267] https://github.com/spiffe/spire-identity-exchange
[268] https://spiffe.io/docs/latest/microservices/envoy-x509/readme/
[269] https://github.com/orgs/istio/repositories
[270] https://istio.io/latest/docs/ambient/install/istioctl/
[241] https://www.boost.org/doc/libs/1_78_0/doc/html/boost_asio.html
[242] https://github.com/openssl/openssl
[243] https://www.boost.org/doc/libs/latest/doc/html/boost_asio/overview/files.html
[244] https://www.boost.org/doc/libs/latest/doc/html/boost_asio/overview.html
[245] https://www.boost.org/doc/libs/1_69_0/doc/html/boost_asio.html
[246] https://www.boost.org/doc/libs/latest/doc/html/boost_asio/reference/io_context/wrap.html
[247] https://github.com/krisman/liburing
[248] https://www.boost.org/doc/libs/latest/doc/html/boost_asio/overview/ssl.html
[249] https://www.boost.org/doc/libs/latest/doc/html/boost_asio/reference/basic_io_object.html
[250] https://github.com/axboe/liburing/blob/master/LICENSE
[251] https://github.com/axboe/liburing
[252] https://android.googlesource.com/platform/external/boost.git/+/077d563efdb280a3d144892e2d0ca01e6a49f454/boost/asio/ssl/impl/context.ipp
[253] https://android.googlesource.com/platform/external/boost/+/refs/heads/jumper-stable/boost/asio/version.hpp
[254] https://www.boost.org/doc/libs/1_50_0/doc/html/boost_asio/examples.html
[255] https://openssl-library.org/source/license/index.html
[271] https://aks-learning.github.io/docs/security/network-security/
[272] https://docs.cilium.io/en/latest/security/policy/
[273] https://docs.cilium.io/en/stable/security/policy/language/
[274] https://docs.cilium.io/en/latest/network/kubernetes/policy/
[275] https://docs.cilium.io/en/stable/network/kubernetes/policy/
[276] https://imroc.cc/tke/en/networking/cilium/networkpolicy
[277] https://learn.microsoft.com/en-us/azure/aks/network-policy-best-practices
[278] https://docs.cilium.io/en/stable/security/policy/
[279] https://docs.cilium.io/en/latest/security/policy/layer3/
[280] https://docs.cilium.io/en/latest/security/policy/intro/
[281] https://factory.com/open-source-wikis/cilium?page=features/network-policy.md
[282] https://docs.cilium.io/en/stable/security/policy/layer7/
[283] https://docs.cilium.io/en/latest/security/policy/kubernetes/
[284] https://doc.crds.dev/github.com/cilium/cilium/cilium.io/CiliumNetworkPolicy/v2@1.12.0
[285] https://notes.kodekloud.com/docs/Prep-Course-Cilium-Certified-Associate-CCA-Certification/Network-Policy/Cilium-Network-Policy/page
[286] https://azure.microsoft.com/en-us/pricing/details/advanced-container-networking-services/
[287] https://learn.microsoft.com/en-us/azure/aks/advanced-container-networking-services-overview
[288] https://techcommunity.microsoft.com/blog/itopstalkblog/lock-down-aks-end-to-end-with-application-gateway-for-containers-and-managed-cil/4534205
[289] https://learn.microsoft.com/bs-latn-ba/azure/aks/container-network-observability-how-to
[290] https://learn.microsoft.com/en-us/azure/aks/container-network-observability-logs
[291] https://github.com/cilium/hubble/blob/main/LICENSE
[292] https://azure.microsoft.com/en-us/blog/announcing-advanced-container-networking-services-for-your-azure-kubernetes-service-clusters/
[293] https://github.com/Azure/AKS/issues/4768
[294] https://learn.microsoft.com/th-th/azure/aks/container-network-observability-how-to
[295] https://github.com/cilium/hubble
[296] https://azure-samples.github.io/aks-labs/docs/networking/acns-lab/
[297] https://github.com/cilium/cilium
[298] https://hysenlabs.com/en/projects/cilium-hubble
[299] https://techcommunity.microsoft.com/tag/aks
[300] https://github.com/mdaoss/cilium
[301] https://hub.docker.com/r/pgbouncer/pgbouncer
[302] https://github.com/pgbouncer/pgbouncer/blob/master/COPYRIGHT
[303] https://github.com/cloudflare/boringtun
[304] https://github.com/cloudnative-pg/pgbouncer-containers/blob/main/README.md
[305] https://github.com/Wiresock-Foundation/wiresock-boringtun
[306] https://github.com/case-88/pgbouncer-aurora-operator/blob/main/README.md
[307] https://www.bytebase.com/blog/open-source-postgres-connection-pooler/
[308] https://github.com/defGuard/boringtun
[309] https://github.com/pgbouncer
[310] https://github.com/cloudnative-pg/pgbouncer-containers
[311] https://github.com/canonical/pgbouncer-operator
[312] https://github.com/NixOS/nixpkgs/blob/master/pkgs/servers/sql/pgbouncer/default.nix
[313] https://archlinux.org/packages/extra-testing/x86_64/pgbouncer/
[314] https://blog.cloudflare.com/boringtun-userspace-wireguard-rust/
[315] https://deepwiki.com/cloudflare/quiche/6-c-foreign-function-interface
