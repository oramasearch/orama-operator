# Licensing and the right to resell: section G, verbatim

**Asked:** 2026-10-14, through Perplexity deep research (`perplexity_research`), with section G of [`prompt.md`](prompt.md) expanded to every component and alternative named in the first report.
**Returned:** verbatim below, citations included. Nothing in it was verified against the sources here; [`README.md`](README.md) says how it is read. Not legal advice.

---

**Recommendation:** Keep the `tunnel` resource provider-neutral. Ship a self-hosted provider built from **wireguard-go** and your own permissively licensed control plane for the enterprise and air-gapped product. For a managed provider, **ngrok is the closest documented fit for customer-facing embedded connectivity**, but obtain written confirmation for a product marketed as a competing tunnel service. Do **not** launch one-Cloudflare-account-per-workspace resale under a self-serve account: negotiate express Cloudflare authorization first. Cloudflare’s restrictions cover both selling third-party access and Zero Trust delivered through a managed-service or service-bureau relationship.[web:136][web:212][web:361][web:31][web:300]

### Licensing and redistribution

**Status below addresses the component’s software license and, where applicable, the hosted service separately.** “Permitted with obligations” means both commercial redistribution in a customer-installed product and operating the software as your own hosted service are allowed under the stated open-source license; it does **not** grant rights to a vendor’s separately operated cloud. For Apache-2.0 distributions, supply the license, retain applicable copyright and attribution notices, mark modified files, and carry forward a relevant `NOTICE` file. MIT requires its copyright and license text; BSD-3-Clause also bars endorsement using the copyright holder’s name. None grants a trademark or product-endorsement right. Hosted use alone ordinarily does not trigger these *distribution* requirements.[web:76][web:92][web:136]

| Component or service | License or governing terms, including relevant change | Resale status | Source |
|---|---|---|---|
| Istio **istiod**, sidecars and Istio gateway | **Apache-2.0**; no identified change in the last three years. The Istio gateway is an Istio deployment, not a separately licensed Gateway API product. | **Permitted with obligations**—Apache notices and license on redistribution; hosted use permitted. | [web:88][web:89] |
| Istio ambient **ztunnel** | **Apache-2.0**; separate upstream repository. | **Permitted with obligations**. | [web:79] |
| Envoy Proxy | **Apache-2.0**. | **Permitted with obligations**. | [web:76] |
| Cilium dataplane, network policy, Cluster Mesh and upstream FQDN/DNS policy | **Apache-2.0** for upstream Cilium; no separate Cluster Mesh or FQDN-policy license. An AKS-managed installation is additionally subject to Azure’s service offering. | **Permitted with obligations** for upstream software; Azure-managed feature availability and charges are separate. | [web:80][web:78][web:1] |
| Kubernetes Gateway API specification and CRDs | **Apache-2.0**. This is an API, **not** a gateway implementation. | **Permitted with obligations**. | [web:62][web:68] |
| Envoy Gateway | **Apache-2.0**; its Envoy dataplane is also Apache-2.0. | **Permitted with obligations**. | [web:64][web:76] |
| Cilium Gateway API implementation | Part of **Apache-2.0** Cilium, not a separate gateway license. | **Permitted with obligations**, subject to what the selected AKS-managed Cilium configuration supports. | [web:80][web:78][web:394] |
| NGINX Gateway Fabric | Controller **Apache-2.0**; NGINX Open Source dataplane **BSD-2-Clause**. Optional NGINX Plus is commercial and has different terms. | **Permitted with obligations** with the open-source dataplane; **requires a commercial agreement** for Plus. | [web:63][web:439][web:443] |
| SPIFFE specification / SPIRE server and agent | **Apache-2.0** in their respective repositories. | **Permitted with obligations**. | [web:317][web:46] |
| CoreDNS | **Apache-2.0**. | **Permitted with obligations**. | [web:58] |
| cert-manager | **Apache-2.0**; review its dependency-license inventory when assembling a distribution. | **Permitted with obligations**. | [web:49][web:53] |
| `cloudflared` **client software** | **Apache-2.0**; this does **not** license Cloudflare’s network or Tunnel service. | **Permitted with obligations** to redistribute the client; **requires a commercial agreement** for the proposed resale of Cloudflare service. | [web:197][web:31][web:300] |
| Cloudflare Tunnel / Zero Trust / Access **hosted services** | Cloudflare Self-Serve Subscription Agreement and Zero Trust Service-Specific Terms; **no SPDX software license** for the hosted service. | **Requires express written commercial authorization** for the proposed customer-facing resale; otherwise **not permitted** as resale. | [web:31][web:300] |
| Tailscale core Linux client and Kubernetes operator | **BSD-3-Clause**; the operator is in the client repository. Closed-platform GUI portions are not covered by that blanket statement. | **Permitted with obligations** for covered source/binaries; this grants **no resale right** to Tailscale’s hosted service. | [web:92][web:208][web:465] |
| Tailscale coordination server and hosted service | Proprietary coordination server; hosted access under Tailscale Terms or an agreed channel-partner contract; **no SPDX identifier**. | **Requires a commercial channel agreement** for resale or managed service; self-serve resale **not permitted**. | [web:465][web:240][web:246] |
| Headscale | **BSD-3-Clause** independently developed coordination server. | **Permitted with obligations** to redistribute or self-host; validate the particular client builds and features you package. | [web:94][web:242] |
| ngrok current agent | Proprietary binary, **no SPDX identifier**; the current agent is not the old open-source ngrok 1.x agent. Its distribution rights are specified in ngrok’s Terms. | **Permitted with terms for the expressly defined “Customer Licensees” case**; **requires written consent** to distribute it for customers using their own ngrok accounts. Competing-service use needs express clearance. | [web:213][web:212] |
| ngrok Kubernetes operator | **MIT**; an open-source operator does not license ngrok’s agent or cloud service. | **Permitted with obligations** for the operator; hosted service governed separately. | [web:221][web:212] |
| ngrok hosted service | ngrok Terms; **no SPDX identifier** for the service. | **Permitted only within its express embedded-application/customer-licensee grant**; **requires a commercial agreement** for standalone resale or a competing tunnel offering. | [web:212][web:361] |
| Twingate client, connector and hosted controller | Proprietary service/software under Twingate customer terms; **no SPDX identifier** for these components. | **Requires a reseller/MSP agreement**; customer-agreement resale or competitive use **not permitted** without written permission. | [web:257][web:260] |
| Twingate Kubernetes operator | **MPL-2.0** in the upstream `LICENSE` file. Retain notices and provide source of covered files, including distributed modifications, under MPL; a larger separate work need not all be MPL. Its license does not cover the Twingate service. | **Permitted with obligations** for the operator; **commercial agreement** for service resale. | [web:107][web:288][web:257] |
| Teleport **source**, excluding `/api` | **AGPL-3.0** source; `/api` is **Apache-2.0**. For a modified AGPL service, offer corresponding source to its network users; distributions also carry AGPL obligations. | **Permitted with substantial copyleft obligations** for AGPL source, subject to separating out commercial-only features. | [web:187][web:193] |
| Teleport Community **vendor binaries/images**, version 16 onward | Teleport Community commercial license, **no SPDX identifier**; changed **June 2024** from the earlier Apache-2.0 binary/image distribution. It restricts qualifying companies by size and revenue and forbids embedding/resale. Do not equate these binary terms with the AGPL source terms. | **Not permitted** for the proposed embedding or resale without a commercial agreement. | [web:181][web:175][web:193] |
| HashiCorp/IBM Boundary, version 0.14.0 onward | **BUSL-1.1**, source-available, **not permissive**; the repository’s IBM grant excludes a paid hosted or embedded offering significantly overlapping IBM’s paid Boundary capabilities. Each version changes to **MPL-2.0 four years after publication**. HashiCorp announced its MPL-to-BUSL change in **August 2023**, just outside the requested three-year window. | **Requires a commercial agreement** if the proposed product is competitive; do not treat its general redistribution permission as permission for competitive production resale. | [web:188][web:169][web:423] |
| Pomerium open-source proxy | **Apache-2.0**; assess any separately purchased enterprise features on their own terms. | **Permitted with obligations** for this repository. | [web:167][web:166] |
| NetBird client and unaffected repository directories | **BSD-3-Clause**. | **Permitted with obligations** for these parts, not the whole self-hosted stack. | [web:121] |
| NetBird management, signal, relay, combined and dashboard | **AGPL-3.0**. Changed from BSD-3-Clause beginning with **v0.53.0, scheduled August 5, 2025**; earlier releases were not relicensed retroactively. Provide AGPL source as required, including to network users of a modified server. | **Permitted with substantial copyleft obligations**, or seek separate commercial terms for a proprietary fork. | [web:121][web:315][web:135] |
| Netmaker community/core | **Apache-2.0** outside expressly excluded `pro/` code and third-party components. The server moved **SSPL-1.0 → Apache-2.0 on September 21, 2023**, just outside the three-year window; old SSPL releases remain distinct. | **Permitted with obligations** for current Apache-covered core. | [web:130][web:299] |
| Netmaker `pro/` | Netmaker Enterprise **proprietary license**, **no SPDX identifier**; production requires the appropriate subscription, and the license forbids ordinary redistribution/sublicensing. | **Requires a commercial agreement** covering your embedding, hosted use and redistribution; otherwise **not permitted**. | [web:126] |
| `wireguard-go` | **MIT** userspace implementation. | **Permitted with obligations**; suitable permissive base for an air-gapped product. | [web:136] |
| Linux kernel WireGuard / out-of-tree backport | Kernel implementation **GPL-2.0-only**; backport GPLv2. It is **not** MIT merely because `wireguard-go` is. | **Permitted with GPL obligations** when distributing kernel/module binaries; using an already-installed customer kernel does not redistribute it. | [web:156][web:153][web:159] |
| `wireguard-tools` (`wg`, `wg-quick`) | **GPL-2.0-only**; account for it separately if included in your image or appliance. | **Permitted with GPL obligations** on redistribution. | [web:155][web:162] |
| `frp` | **Apache-2.0**. | **Permitted with obligations**; a self-hosted reverse-tunnel alternative. | [web:450][web:457] |
| inlets OSS, historical | Older open-source, HTTP-oriented project, superseded as the maintained product by inlets Pro. Do **not** infer the Pro binary’s license from the old project or its MIT tooling. | **Do not select as the maintained permissive production fallback** without pinning and reviewing the exact historical release/license. | [web:338][web:343] |
| inlets Pro tunnel binary | Commercial inlets Pro EULA, **no SPDX identifier**; requires a license key and limits use to licensed internal business purposes, barring distribution and service-bureau use absent other terms. | **Requires a negotiated commercial agreement**; ordinary subscription **does not permit** this resale. | [web:238][web:343] |
| `inletsctl` and inlets operator | **MIT** tooling; this does not relicense the Pro tunnel binary it deploys. | **Permitted with obligations** for tooling; Pro remains restricted. | [web:229][web:333][web:436] |
| `chisel` | **MIT**. | **Permitted with obligations** for redistribution and self-hosting. | [web:151] |
| `rathole` | **Apache-2.0**. | **Permitted with obligations** for redistribution and self-hosting. | [web:152] |

**AKS-specific licensing versus capability:** Your AKS-managed Cilium’s ordinary network policies do not themselves imply FQDN egress enforcement. Microsoft says its managed **FQDN and L7 filtering requires the paid Advanced Container Networking Services (ACNS) Container Network Security feature**; if that security feature is disabled, those policies are blocked. Budget and enable it before promising workspace domain allowlists. Similarly, upstream Cilium Cluster Mesh being Apache-licensed does not mean you can switch it on in AKS-managed Cilium for arbitrary customer networks: Microsoft’s managed cross-cluster offering uses Fleet and ACNS and has reachability and management constraints. Treat customer AWS VPC connectivity as a separate routed peer/VPN design, not an assumed Cluster Mesh entitlement.[web:1][web:2][web:395]

### Hosted-service terms: what the product may actually sell

| Hosted service | Dated operative restriction and customer-account distinction | Consequence for this product |
|---|---|---|
| **Cloudflare Tunnel; Cloudflare Zero Trust / Access** | Cloudflare’s Self-Serve Subscription Agreement, **last updated September 12, 2025**, §2.2.1(a), says that without express written permission you may not “**rent, lease, loan, export, or sell access to the Services to any third party, or sign up for the Services on behalf of a third party**.” Section 2.2.1(j) also prohibits using the Services “**to provide a virtual private network or other similar proxy services**.” Zero Trust’s Service-Specific Terms, **last updated September 28, 2026**, §2.2 say: “**You shall not resell Cloudflare Zero Trust to any third parties (e.g., in an ASP, managed security services, outsourcing, time-sharing or service bureau relationship) unless expressly permitted by Cloudflare in writing**”; breach can immediately terminate the account. The terms do not say that *all traffic from your SaaS’s customers* is forbidden: serving your own application to visitors differs from selling those customers Tunnel/Access capability. But **one named tunnel per customer workspace, provisioned and charged as your networking feature in your account, is sufficiently close to the expressly barred resale/service-bureau use that it is not a safe self-serve launch**. A customer who opens and pays for **their own** Cloudflare account and authorizes your integration avoids *your* signing up for or reselling Cloudflare service, provided you sell your software/management rather than sublicensing their service and comply with that customer’s account permissions and terms. | **Get written authorization covering your precise own-account multi-tenant model**, or use genuine customer-owned accounts. Merely calling the CRD `tunnel`, hiding Cloudflare branding, or routing traffic through your account does not create an exception. [web:31][web:300] |
| **Tailscale hosted coordination** | Tailscale self-serve Terms, **last updated August 25, 2026**, §2.3 prohibit “**sell, resell, rent or lease use of the Services**” and use to develop or sell a service that could “**directly or indirectly, compete with the Services**.” Its separate Channel Partner Agreement expressly authorizes resale and managed services **under that agreement and a purchasing document**. A BSD-licensed client or customer-owned account does not authorize you to resell the coordination service. | **Commercial channel agreement required** for your branded managed offering; independently self-hosted Headscale is a different option. [web:240][web:246][web:94] |
| **ngrok hosted service and current agent** | ngrok Terms, **last updated March 2, 2026**, expressly allow an account holder to distribute its agent to **“Customer Licensees”** receiving an application the holder created using ngrok, where **you, not the customer, maintain the ngrok account**. They require **prior written consent** to distribute the agent to customers maintaining their *own* ngrok accounts. The same terms otherwise forbid “**rent, transfer, resell, lease, license, assign, grant any rights in, or otherwise make available the ngrok Services ... to third parties or offer it on a standalone basis**,” and use reasonably deemed competitive with ngrok. ngrok publicly offers enterprise **per-device or per-customer** connectivity pricing. | **Best documented candidate for an embedded customer-connectivity feature**, not blanket permission to sell a standalone competing tunnel service. Describe your exact developer-tunnel and enterprise redistribution model to ngrok and obtain written clearance where it crosses that line. [web:212][web:361] |
| **Twingate hosted service** | Twingate’s Customer Agreement, **last updated April 22, 2024**, prohibits, absent written permission, “**lease, sell, resell, or use for timesharing or service bureau purposes, the Services**” and use to build or improve a directly competitive product. Its separate partner terms provide a route for resale, with customer terms and provisioning arrangements; an open-source operator is not such permission. | **MSP/reseller agreement required**, with the intended branded-product and competitive-use scope addressed expressly. [web:257][web:260][web:107] |

**Cloudflare scale, cost and protocols.** Default limits, documented **September 4, 2026**, are **1,000 `cloudflared` tunnels per account**, **1,000 total hostname-plus-CIDR routes** shared with Cloudflare Mesh, **25 active replicas per tunnel**, **500 Access applications** and **50 service tokens**. Thus “one tunnel and one hostname route per workspace” reaches an account limit at **1,000 workspaces**; “one Access application per workspace” reaches **500 first**. These are defaults, not promises of an increase; Enterprise customers can ask their account team. A tunnel does **not** itself consume a user seat: Zero Trust’s published plans are **Free, $0 for up to 50 users; pay-as-you-go, $7/user/month; contract, custom annual per-user pricing**. Service tokens do not consume user seats, though they have their own limit. Neither the free seat allowance nor a route limit confers resale rights.[web:21][web:271][web:412][web:300]

Cloudflare **Tunnel is not simply a public arbitrary-TCP socket**. Its published TCP route streams TCP over WebSocket and requires `cloudflared access tcp` at the connecting client; Cloudflare recommends Client-to-Tunnel for long-lived connections. For direct public database-client TCP/UDP on arbitrary ports, assess **Spectrum** and its plan/port restrictions separately; Spectrum integration with Tunnel is documented for **HTTP/HTTPS only**. Do not promise that a PostgreSQL client can connect directly to a Tunnel hostname and port without the required client-side arrangement. Also keep the §2.2.1(j) proxy/VPN restriction in scope for the proposed peer-network product.[web:19][web:302][web:30][web:31]

**Cloudflare commercial route and pricing:** The **Partner Network** offers resell/manage routes, but admission alone is not a substitute for a written grant covering *your* Tunnel and Zero Trust model. The **Tenant API** is a partner-provisioning mechanism enabled following a partner agreement—not a generally purchasable license or an inherent requirement if customers instead administer their own accounts. The documented Gateway Tenant API is **Enterprise-partner-only and DNS-policy-limited**; it is not a general Tunnel-resale waiver. **Cloudflare for Platforms** is a family of multi-tenant products, not automatically a Tunnel/Access reseller license: published Cloudflare-for-SaaS pricing is **100 included custom hostnames and $0.10 per additional hostname** on listed self-serve plans, which is **not** a per-tunnel or Zero Trust resale price. A price and entitlement for the proposed partner Tunnel/Access arrangement must be quoted in your agreement; no public fixed price establishes it.[web:375][web:377][web:33][web:277][web:300]

**Implementation choice:** For your own hosted and air-gapped tiers, make the baseline a tenant-scoped WireGuard peer controller, `wireguard-go` endpoints where a userspace implementation is needed, your own credential issuance/rotation and route policy, and a customer-operated rendezvous/relay where NAT traversal demands one. This keeps the critical tunnel/control plane under permissions you can grant to customers, rather than relying on a vendor’s SaaS resale terms. Use already-installed kernel WireGuard for performance where available, but track GPL obligations if you actually ship the kernel module or `wireguard-tools`. Offer **ngrok only as the specifically cleared hosted integration** and **Cloudflare BYO-account or expressly contracted partner integration** as additional providers. Keep mesh identity, namespace egress/DNS enforcement, app credentials, developer ingress tunnels and cross-cloud peer routing as **separate policies and resources**: permission to distribute a proxy or create a tunnel is not permission—or proof of technical support—to connect customer VPCs.[web:136][web:156][web:155][web:212][web:31][web:1][web:395]

Citations:
[16] https://www.cloudflare.com/terms/
[17] https://developers.cloudflare.com/tunnel/concepts/routing/
[18] https://developers.cloudflare.com/cloudflare-wan/zero-trust/connectivity-options/
[19] https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/protocols/
[20] https://developers.cloudflare.com/tunnel/get-started/
[21] https://developers.cloudflare.com/cloudflare-one/account-limits/
[22] https://community.cloudflare.com/t/cloudflare-terms-of-service-managing-our-clients-services-under-our-account/630255
[23] https://developers.cloudflare.com/cloudflare-one/traffic-controls/network-policies/
[24] https://www.cloudflare.com/service-specific-terms-zero-trust-services/
[25] https://github.com/uxiaohan/CFTun-UI/blob/main/TUN-API.md
[26] https://shattered.io/cloudflare-tunnel-setup-guide-2026/
[27] https://cf-assets.www.cloudflare.com/dzlvafdwdttg/6zhhv1fb1JejwVBWMlsdC5/1d821077eb89a47adbb0eeefa6fb12de/Enterprise_Subscription_Agreement_January_17__2023.pdf
[28] https://developers.cloudflare.com/hyperdrive/configuration/connect-to-private-database/
[29] https://teitunnel.teispace.com/docs/reference/limits/
[30] https://developers.cloudflare.com/tunnel/integrations/
[31] https://www.cloudflare.com/terms/
[32] https://developers.cloudflare.com/tenant/reference/subscriptions/
[33] https://developers.cloudflare.com/cloudflare-one/traffic-policies/tiered-policies/tenant-api/
[34] https://developers.cloudflare.com/cloudflare-one/traffic-controls/tiered-policies/tenant-api/
[35] https://cf-assets.www.cloudflare.com/slt3lc6tev37/1TTgT35GoUNlKZYGuKWBFy/4e7dfc8cf402419a9b1cf624291fc69f/cloudflare_customer_dpa-v6.4_april_3_2026.pdf?_gl=1*z4hm27*_gcl_au*OTU1NDg2NDQxLjE3NzY0MTY4Nzk.*_ga*OTE5ZDFlM2UtZjVkYy00YzQwLWE1ZDQtODc5Y2E0YjgyMTQw*_ga_SQCRB0TXZW*czE3NzkxMDM0OTMkbzkzJGcwJHQxNzc5MTAzNDkzJGo2MCRsMCRoMCRkT0xHY3VTUlhpbmhTUkdYT3YxLWl6TTVTamh4eS1pX1ZHUQ..
[36] https://www.cloudflare.com/solutions/platforms/
[37] https://www.cloudflare.com/solutions/platforms.md
[38] https://cf-assets.www.cloudflare.com/slt3lc6tev37/33zd4ZHJyP8tADGLBSUGB7/8cb20da3929645487342eed96bedd898/Cloudflare_Customer_DPA_v6.2_Nov_9_2023.pdf
[39] https://cf-assets.www.cloudflare.com/slt3lc6tev37/6Bd9np6yL3LL73aAsfTSty/d9d79570ef7f7edd30877bff98c4c1eb/Tenant_API_Guide__2025_.pdf
[40] https://cf-assets.www.cloudflare.com/slt3lc6tev37/2wdhbwL52YTheg2uErH2VG/d0db20e0236f2ecc20d2347f531d6816/Intro_to_Cloudflare__2025_.pdf
[41] https://edit.tosdr.org/documents/1506
[42] https://developers.cloudflare.com/tenant/get-started/
[43] https://developers.cloudflare.com/tenant/llms-full.txt
[44] https://developers.cloudflare.com/api/
[45] https://community.cloudflare.com/t/cloudflare-terms-of-service-managing-our-clients-services-under-our-account/630255
[1] https://docs.azure.cn/en-us/aks/network-policy-best-practices
[2] https://docs.azure.cn/en-us/aks/container-network-security-fqdn-filtering-concepts
[3] https://techcommunity.microsoft.com/blog/azurenetworkingblog/securing-microservices-with-cilium-and-istio/4381646
[4] https://techcommunity.microsoft.com/blog/azurenetworkingblog/secure-high-performance-networking-for-data-intensive-kubernetes-workloads/4271478
[5] https://docs.azure.cn/en-us/aks/how-to-apply-fqdn-filtering-policies
[6] https://techcommunity.microsoft.com/tag/open-source
[7] https://docs.azure.cn/en-us/aks/azure-cni-powered-by-cilium
[8] https://learn.microsoft.com/en-us/azure/aks/use-network-policies
[9] https://docs.cilium.io/en/stable/network/kubernetes/
[10] https://techcommunity.microsoft.com/tag/multi-cloud
[11] https://www.infoworld.com/article/3566814/secure-azure-kubernetes-with-advanced-container-networking-services.html
[12] https://learn.microsoft.com/vi-vn/Azure/aks/container-network-security-fqdn-filtering-concepts
[13] https://docs.azure.cn/en-us/aks/advanced-container-networking-services-overview
[14] https://learn.microsoft.com/he-il/azure/aks/advanced-container-networking-services-overview?tabs=cilium
[15] https://docs.azure.cn/en-us/aks/container-network-observability-metrics
[76] https://github.com/envoyproxy/envoy/blob/main/LICENSE
[77] https://github.com/envoyproxy/envoy-mobile/blob/main/LICENSE
[78] https://github.com/cilium/cilium
[79] https://github.com/istio/ztunnel/blob/master/LICENSE
[80] https://github.com/cilium/cilium/blob/main/LICENSE
[81] https://github.com/istio
[82] https://github.com/orgs/envoyproxy/repositories
[83] https://github.com/envoyproxy/gateway
[84] http://github.com/orgs/istio/repositories
[85] https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/security/external_deps
[86] https://github.com/envoyproxy/gateway/blob/main/internal/xds/translator/route.go
[87] https://github.com/envoyproxy
[88] https://github.com/istio/istio
[89] https://istio.io/latest/about/faq/
[90] https://github.com/mdaoss/cilium
[61] https://github.com/envoyproxy/gateway
[62] https://github.com/kubernetes-sigs/gateway-api/blob/main/LICENSE
[63] https://github.com/nginx/nginx-gateway-fabric/blob/main/LICENSE
[64] https://github.com/envoyproxy/gateway/blob/main/LICENSE
[65] https://github.com/envoyproxy/ai-gateway/blob/main/LICENSE
[66] https://github.com/envoyproxy/ai-gateway/blob/main/.licenserc.yaml
[67] https://raw.githubusercontent.com/envoyproxy/ai-gateway/main/examples/basic/basic.yaml
[68] https://github.com/kubernetes-sigs/gateway-api
[69] https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.3/apis/v1/gateway_types.go
[70] https://github.com/nginx/nginx-gateway-fabric
[71] https://github.com/envoyproxy/ai-gateway/issues/1560
[72] https://docs.nginx.com/nginx-gateway-fabric/install/manifests/plus-experimental/
[73] https://github.com/envoyproxy
[74] https://github.com/orgs/nginx/packages/container/nginx-gateway-fabric/453661656
[75] https://pkg.go.dev/sigs.k8s.io/gateway-api
[46] https://github.com/spiffe/spire/blob/main/LICENSE
[47] https://github.com/spiffe/spire
[48] https://github.com/coredns/deployment/blob/master/LICENSE
[49] https://github.com/cert-manager/cert-manager/blob/v1.21.2/LICENSE
[50] https://github.com/spire/spire/blob/master/LICENSE
[51] https://github.com/spiffe/spire-examples/blob/main/LICENSE
[52] https://github.com/spiffe/spire-identity-exchange
[53] https://github.com/cert-manager/cert-manager/blob/master/LICENSES
[54] http://github.com/coredns/alternate/blob/master/LICENSE
[55] https://github.com/dokku/coredns-docker/blob/main/LICENSE
[56] https://github.com/nholuongut/cert-manager
[57] https://github.com/spiffe/spire-tutorials
[58] https://coredns.io/
[59] https://github.com/spiffe/spire-controller-manager
[60] https://github.com/cert-manager
[91] https://github.com/tailscale/tailscale-rs/blob/main/LICENSE
[92] https://github.com/tailscale/tailscale/blob/main/LICENSE
[93] https://github.com/tailscale/tailscale-android/blob/main/LICENSE
[94] https://github.com/juanfont/headscale/blob/main/LICENSE
[95] https://github.com/cloudflare/developer-platform/blob/main/LICENSE
[96] https://github.com/tailscale/tailscale
[97] https://github.com/tailscale/libtailscale/blob/main/LICENSE
[98] https://github.com/juanfont/headscale
[99] https://github.com/cloudflare/computer/blob/main/LICENSE
[100] https://github.com/cloudflare/security-audit-skill/blob/main/LICENSE
[101] https://github.com/tailscale/tailscale/blob/main/licenses/tailscale.md
[102] https://github.com/tailscale/tailscale-rs
[103] https://github.com/hwdsl2/headscale-install/blob/main/LICENSE.md
[104] https://github.com/tailscale/go/
[105] https://layers.openembedded.org/layerindex/recipe/481759/
[106] https://ngrok.github.io/ngrok-operator/
[107] https://github.com/Twingate/kubernetes-operator/blob/main/LICENSE
[108] https://github.com/ngrok/kubernetes-ingress-controller
[109] https://ngrok.com/docs/integrations/kubernetes-ingress/google-kubernetes-engine
[110] https://github.com/ngrok/ngrok-operator
[111] https://github.com/orgs/ngrok/repositories
[112] https://www.twingate.com/docs/k8s
[113] https://github.com/Twingate/kubernetes-operator
[114] https://github.com/orgs/Twingate/repositories
[115] https://www.twingate.com/integrations/kubernetes
[116] https://github.com/ngrok
[117] https://github.com/Twingate
[118] https://github.com/Twingate/kubernetes-operator/pkgs/container/helmcharts%2Ftwingate-operator
[119] https://www.twingate.com/blog/k8s-operator
[120] https://ngrok.com/docs/getting-started/kubernetes/ingress
[121] https://github.com/netbirdio/netbird/blob/main/LICENSE
[122] https://learn.netmaker.io/license
[123] https://github.com/gravitl/netmaker
[124] https://hysenlabs.com/projects/gravitl-netmaker
[125] https://github.com/orgs/gravitl/repositories
[126] https://github.com/gravitl/netmaker/blob/develop/pro/PRO_LICENSE
[127] https://deps.dev/go/github.com%2Fnetbirdio%2Fnetbird/v0.71.5-0.20260601173800-e7c9182ff93b
[128] https://learn.netmaker.io/getting-started/walkthrough/how-to-install-netmaker
[129] https://github.com/gravitl/netmaker-1
[130] https://github.com/gravitl/netmaker/blob/master/LICENSE.md
[131] https://cybercompanyprofiles.com/companies/netmaker
[132] https://github.com/gravitl/netmaker-docs/blob/master/license.rst
[133] https://github.com/netbirdio/netbird
[134] https://deps.dev/go/github.com%2Fnetbirdio%2Fnetbird/v0.74.3
[135] https://netbird.io/knowledge-hub/netbird-agpl-announcement
[166] https://github.com/pomerium/pomerium
[167] https://github.com/pomerium/pomerium/blob/main/LICENSE
[168] https://hysenlabs.com/projects/pomerium-pomerium
[169] https://github.com/hashicorp/boundary/blob/main/.release/docker/docker-entrypoint.sh
[170] https://www.ithome.com.tw/news/179607
[171] https://hysenlabs.com/en/projects/hashicorp-boundary
[172] https://hysenlabs.com/en/projects/gravitational-teleport
[173] https://github.com/hashicorp/boundary/blob/main/buf.gen.yaml
[174] https://github.com/hashicorp/boundary/blob/main/.git-blame-ignore-revs
[175] https://raw.githubusercontent.com/gravitational/teleport/master/build.assets/LICENSE-community
[176] https://github.com/hashicorp/boundary/blob/main/buf.work.yaml
[177] https://www.hashicorp.com/en/bsl
[178] https://github.com/gravitational/teleport-plugins/blob/master/LICENSE
[179] https://github.com/hashicorp/boundary-guides/blob/main/LICENSE
[180] https://www.hashicorp.com/en/license-faq
[136] https://github.com/WireGuard/wireguard-go/blob/master/LICENSE
[137] https://github.com/fatedier/frp
[138] https://github.com/orgs/WireGuard/repositories
[139] https://github.com/devopsleague/fatedier-frp
[140] https://gittrend.io/repo/fatedier/frp
[141] https://github.com/fatedier
[142] https://github.com/wireguard
[143] https://ports.macports.org/port/wireguard-go/details/
[144] https://github.com/WireGuard/wireguard-go
[145] https://deps.dev/go/github.com%2Ffatedier%2Ffrp/v0.69.1
[146] https://github.com/baibaicloud/frp
[147] https://ports.macports.org/port/wireguard-go/stats/
[148] https://deps.dev/go/github.com%2F0xAFz%2Fwireguard-go/v0.0.3
[149] https://pkg.go.dev/github.com/flannel-io/flannel/pkg/backend/wireguard
[150] https://github.com/fatedier/fft/blob/master/LICENSE
[151] https://github.com/jpillora/chisel/blob/master/LICENSE
[152] https://github.com/rapiz1/rathole/blob/main/LICENSE
[153] https://github.com/WireGuard/wireguard-linux-compat
[154] https://github.com/WireGuard/wireguard-monolithic-historical
[155] https://github.com/WireGuard/wireguard-tools
[156] https://github.com/WireGuard/wireguard-linux/blob/stable/drivers/net/wireguard/device.c
[157] https://github.com/orgs/WireGuard/repositories
[158] https://hysenlabs.com/projects/jpillora-chisel
[159] https://git.zx2c4.com/wireguard-linux/tree/Documentation/process/license-rules.rst?id=08232a4544cc6befaabfbec2087bedaf21b0da34
[160] https://github.com/WireGuard
[161] https://github.com/WireGuard/wireguard-linux
[162] https://github.com/WireGuard/wireguard-tools/blob/master/src/wg-quick/linux.bash
[163] https://github.com/jonz-secops/jpillora_chisel
[164] https://deps.dev/go/github.com%2Fjpillora%2Fchisel/v1.9.0
[165] https://www.freshports.org/net/rathole/
[196] https://github.com/cloudflare/cloudflare.github.io
[197] https://github.com/cloudflare/cloudflared/blob/HEAD/LICENSE
[198] https://github.com/cloudflare/cloudflare-os/blob/main/LICENSE
[199] https://github.com/tailscale/tailscale/blob/main/LICENSE
[200] https://github.com/tailscale/tailscale-rs/blob/main/LICENSE
[201] https://github.com/cloudflare/cloudflared/blob/master/LICENSE?ref=apidog.com
[202] https://github.com/cloudflare/ci/blob/main/LICENSE
[203] https://github.com/cloudflare/cloudflared
[204] https://github.com/cloudflare/cloudflared/issues/573
[205] https://deps.dev/go/github.com%2Fcloudflare%2Fcloudflared/v0.0.0-20260916172655-156742e3acd9
[206] https://github.com/tailscale/libtailscale/blob/main/LICENSE
[207] https://github.com/inlets/inlets-pro
[208] https://github.com/tailscale/tailscale/blob/main/cmd/k8s-operator/operator.go
[209] https://archlinux.org/packages/extra/x86_64/cloudflared/
[210] https://developers.cloudflare.com/fundamentals/reference/policies-compliances/licenses/
[181] https://goteleport.com/blog/teleport-16/
[182] https://raw.githubusercontent.com/gravitational/teleport/master/build.assets/LICENSE-community
[183] https://www.hashicorp.com/en/bsl
[184] https://github.com/hashicorp/boundary-reference-architecture/blob/main/LICENSE
[185] https://tech-insider.org/au/teleport-pam-essential-eight-setup-2026/
[186] https://github.com/hashicorp/boundary-guides/blob/main/LICENSE
[187] https://github.com/gravitational/teleport
[188] https://www.hashicorp.com/en/blog/hashicorp-adopts-business-source-license
[189] https://hysenlabs.com/projects/gravitational-teleport
[190] https://github.com/mattrobinsonsre/bamf/blob/main/docs/comparison.md
[191] https://goteleport.com/blog/march-newsletter-2024/
[192] https://github.com/hashicorp/boundary-plugin-aws/blob/main/LICENSE
[193] https://goteleport.com/blog/teleport-community-license/
[194] https://goteleport.com/blog/tags/Announcements/
[195] https://en.wikipedia.org/wiki/Teleport_(software)
[211] https://github.com/orgs/ngrok/repositories
[212] https://ngrok.com/tos
[213] https://ngrok.com/docs/faq
[214] https://github.com/NGROK
[215] https://github.com/ngrok/ngrok-go
[216] https://github.com/ngrok/ngrok-python/
[217] https://ngrok.github.io/ngrok-python/index.html
[218] https://github.com/ngrok/ngrok-go/blob/main/LICENSE.txt
[219] https://ngrok.github.io/ngrok-javascript/index.html
[220] https://pkg.go.dev/golang.ngrok.com/ngrok
[221] https://github.com/ngrok/ngrok-operator/blob/main/LICENSE
[222] https://clause.watch/contracts/ngrok
[223] https://ngrok.com/docs/agent-sdks
[224] https://ngrok.com/dpa
[225] https://cleartos.com/c/ngrok
[240] https://tailscale.com/terms
[241] https://marketplace.microsoft.com/en-us/product/saas/lynxroute.headscale?tab=overview
[242] https://tailscale.com/opensource
[243] https://tailscale.com/msa
[244] https://marketplace.microsoft.com/en-us/product/lynxroute.headscale?tab=Overview
[245] https://www.infralovers.com/blog/2026-05-22-headscale-self-hosted-tailscale-alternative/
[246] https://github.com/tailscale/terms-and-conditions/blob/main/channel-partner-agreement/index.md
[247] https://netbird.io/knowledge-hub/tailscale-vs-netbird
[248] https://www.bitdoze.com/headscale-self-hosted-tailscale-setup/
[249] https://tailscale.com/legal
[250] https://github.com/kimjayney/headscale
[251] https://dashy.to/docs/authentication/tailscale/
[252] https://dev.to/selfhostingsh/headscale-vs-tailscale-self-hosted-control-plane-1h1f
[253] https://github.com/hwdsl2/docker-headscale
[254] https://github.com/tailscale/terms-and-conditions/blob/main/dpa/index.md
[255] https://www.twingate.com/terms/user
[256] https://www.twingate.com/msp
[257] https://www.twingate.com/terms
[258] https://www.twingate.com/terms/otra
[259] https://www.twingate.com/blog/other/introducing-twingate-partner-alliance
[260] https://www.twingate.com/terms/partner-20230501
[261] https://www.twingate.com/customers/tech-solutions
[262] https://costbench.com/software/business-vpn/twingate/
[263] https://www.twingate.com//partners
[264] https://www.twingate.com/terms/20240301
[265] https://xamplify.com/partner-programs/nordlayer-removed-dup/
[266] https://costbench.com/software/business-vpn/twingate/discounts/
[267] https://www.twingate.com/docs/msp-billing
[268] https://cloudfactorygroup.com/cloud-services/vendors/twingate
[269] https://www.twingate.com/blog/iam-comparison
[226] https://github.com/hashicorp/boundary-reference-architecture/blob/main/LICENSE
[227] https://github.com/hashicorp/boundary-guides/blob/main/LICENSE
[228] https://github.com/hashicorp/boundary-plugin-aws/blob/main/LICENSE
[229] https://github.com/inlets/inletsctl/blob/master/LICENSE
[230] https://github.com/hashicorp/boundary/blob/main/.git-blame-ignore-revs
[231] https://github.com/hashicorp/boundary/blob/main/buf.gen.yaml
[232] https://github.com/hashicorp/boundary/blob/main/.copywrite.hcl
[233] https://github.com/hashicorp/boundary/blob/main/buf.work.yaml
[234] https://github.com/hashicorp/boundary/blob/main/scripts/schema-diff.sh
[235] https://github.com/hashicorp/setup-boundary/blob/main/LICENSE
[236] https://www.hashicorp.com/en/blog/hashicorp-adopts-business-source-license
[237] https://github.com/mitchellh/go-ps/blob/master/LICENSE.md
[238] https://github.com/inlets/inlets-pro/blob/master/EULA.md
[239] https://github.com/inlets
[270] https://www.cloudflare.com/sase/products/access/
[271] https://www.cloudflare.com/plans/sase-zero-trust.md
[272] https://costbench.com/software/business-vpn/cloudflare-zero-trust/
[273] https://costbench.com/software/business-vpn/cloudflare-zero-trust/calculator/
[274] https://www.cloudflare.com/plans/zero-trust-services/
[275] https://costbench.com/software/business-vpn/cloudflare-zero-trust/free-plan/
[276] https://www.cloudflare.com/plans/
[277] https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/plans/
[278] https://pixlodo.com/cloudflare-zero-trust-pricing/
[279] https://www.spendhound.com/marketplace/cloudflare-pricing
[280] https://zerotrustcost.com/cloudflare-zero-trust-pricing
[281] https://www.cloudflare.com/pricing/.md
[282] https://developers.cloudflare.com/cloudflare-for-platforms/workers-for-platforms/reference/pricing/
[283] https://www.cloudflare.com/solutions/platforms/
[284] https://www.cloudflare.com/solutions/platforms/.md
[300] https://www.cloudflare.com/service-specific-terms-zero-trust-services/
[301] https://www.cloudflare.com/terms/
[302] https://developers.cloudflare.com/fundamentals/reference/network-ports/
[303] https://developers.cloudflare.com/spectrum/reference/configuration-options/index.md
[304] https://developers.cloudflare.com/fundamentals/reference/network-ports/index.md
[305] https://blog.cloudflare.com/updated-tos/
[306] https://www.cloudflare.com/en-gb/__mesa/
[307] https://www.cloudflare.com/service-specific-terms-network-services/
[308] https://www.cloudflare.com/en-gb/service-specific-terms-professional-services/
[309] https://developers.cloudflare.com/api/typescript/resources/spectrum/
[310] https://edit.tosdr.org/documents/1506
[311] https://www.cloudflare.com/__esa/
[312] https://www.cloudflare.com/policies/terms/
[313] https://developers.cloudflare.com/spectrum/llms-full.txt
[314] https://www.cloudflare.com/cloudflare-customer-dpa/
[285] https://github.com/Twingate
[286] https://github.com/Twingate/kubernetes-operator/releases
[287] https://github.com/inlets/inlets-pro
[288] https://github.com/Twingate/kubernetes-operator/blob/main/LICENSE
[289] https://github.com/inlets/inletsctl/blob/master/LICENSE
[290] https://github.com/gravitl/netmaker/blob/develop/LICENSE.md
[291] https://github.com/inlets/inlets-pro/blob/master/docs/cli-reference.md
[292] https://github.com/gravitl/netmaker-docs/blob/master/license.rst
[293] https://github.com/inlets
[294] https://github.com/Twingate/kubernetes-operator
[295] https://www.twingate.com/integrations/kubernetes
[296] https://www.twingate.com/changelog/k8s-operator-generally-available
[297] https://docs.inlets.dev/reference/inlets-operator/
[298] https://news.ycombinator.com/item?id=37142388
[299] https://www.netmaker.io/resources/netmaker-goes-open-source-why-we-switched-to-apache-2-0
[330] https://github.com/inlets/inlets-pro
[331] https://github.com/inlets/inlets-pro/blob/master/EULA.md
[332] https://github.com/inlets/inlets-pro/blob/master/README.md
[333] https://github.com/inlets/inletsctl
[334] https://github.com/inlets/inletsctl/blob/master/LICENSE
[335] https://github.com/inlets/inlets-pro/blob/master/docs/cli-reference.md
[336] https://github.com/inlets
[337] https://docs.inlets.dev/reference/inlets-operator/
[338] https://github.com/ALeggeUp/inlets-oss
[339] https://inlets.dev/blog/2020/10/15/openfaas-public-endpoints.html
[340] https://github.com/inlets/inlets-svc
[341] https://docs.inlets.dev/tutorial/http-authentication/
[342] https://github.com/puppetlabs/inlets
[343] https://inlets.dev/
[344] https://docs.inlets.dev/uplink/installation/
[345] https://github.com/istio/istio.io/blob/master/LICENSE
[346] https://github.com/istio/istio/blob/master/licenses/k8s.io/klog/v2/LICENSE
[347] https://www.hashicorp.com/en/bsl
[348] https://github.com/istio/community/blob/master/LICENSE
[349] https://github.com/istio-ecosystem/dns-discovery/blob/master/LICENSE
[350] https://www.hashicorp.com/en/blog/hashicorp-adopts-business-source-license
[351] https://github.com/layer5io/istio-service-mesh-workshop/blob/master/LICENSE
[352] https://www.hashicorp.com/en/license-faq
[353] https://github.com/coredns/coredns/blob/master/coredns.1.md
[354] https://discuss.hashicorp.com/t/hashicorp-projects-changing-license-to-business-source-license-v1-1/57106
[355] https://github.com/coredns/deployment/blob/master/LICENSE
[356] https://github.com/istio/istio/blob/master/istioctl/pkg/version/version.go
[357] https://istio.io/latest/about/faq/general/
[358] https://github.com/hashicorp/boundary-plugin-aws/blob/main/LICENSE
[359] https://github.com/istio/istio/blob/master/samples/bookinfo/src/productpage/Dockerfile
[315] https://github.com/netbirdio/dashboard
[316] https://github.com/netbirdio/netbird/blob/main/LICENSE
[317] https://github.com/spiffe/spiffe/blob/main/LICENSE
[318] https://github.com/netbirdio/netbird
[319] https://github.com/spiffe/spiffe
[320] https://netbird.io/knowledge-hub/netbird-agpl-announcement
[321] https://github.com/netbirdio
[322] https://github.com/spiffe/spire/blob/main/LICENSE
[323] https://github.com/spiffe/spire
[324] https://github.com/netbirdio/netbird/blob/main/README.md
[325] https://pkg.go.dev/github.com/netbirdio/netbird/management
[326] https://github.com/spiffe/go-spiffe
[327] https://github.com/maxlambrecht/rust-spiffe
[328] http://github.com/orgs/spiffe/repositories
[329] https://github.com/spiffe
[360] https://ngrok.com/tos
[361] https://ngrok.com/pricing
[362] https://ngrok.com/pricing.md
[363] https://ngrok.com/blog/introducing-pay-as-you-go-pricing-for-ngrok
[364] https://ngrok.com/docs/pricing-limits/how-ngrok-charges
[365] https://ngrok.com/docs/pricing-limits
[366] https://cleartos.com/c/ngrok
[367] https://getpulsesignal.com/pricing/ngrok
[368] https://ngrok.com/blog/why-does-usage-based-pricing-work-for-developers
[369] https://dev.to/instatunnel/surviving-the-saas-pivot-why-agpl-open-source-tunnels-win-1m3p
[370] https://clause.watch/contracts/ngrok
[371] https://pricingsaas.com/companies/ngrok
[372] https://www.vendr.com/marketplace/ngrok
[373] https://aws.amazon.com/marketplace/pp/prodview-hbrw3lqzlajzs
[374] https://ngrok.com/docs/gateway/site-to-site-connectivity/end-customers
[375] https://www.cloudflare.com/partners/
[376] https://www.cloudflare.com/partners/power-up-program/
[377] https://developers.cloudflare.com/tenant/get-started/
[378] https://cloudflare-partner.org/
[379] https://cf-assets.www.cloudflare.com/slt3lc6tev37/6Bd9np6yL3LL73aAsfTSty/d9d79570ef7f7edd30877bff98c4c1eb/Tenant_API_Guide__2025_.pdf
[380] https://developers.cloudflare.com/tenant/llms-full.txt
[381] https://developers.cloudflare.com/tenant/reference/subscriptions/
[382] https://developers.cloudflare.com/cloudflare-one/traffic-controls/tiered-policies/tenant-api/
[383] https://developers.cloudflare.com/cloudflare-one/traffic-policies/tiered-policies/tenant-api/
[384] https://www.cloudflare.com/partners/technology-partners/
[385] https://www.businesswire.com/news/home/20220623005365/en/Cloudflare-Equips-Partners-to-Deliver-Complete-Zero-Trust-Solution-with-Cloudflare-One-Partner-Program
[386] https://github.com/cloudflare/cloudflare-docs/blob/production/src/content/docs/tenant/get-started.mdx
[387] https://blog.cloudflare.com/cloudflare-one-partner-program/
[388] https://cf-assets.www.cloudflare.com/slt3lc6tev37/2wdhbwL52YTheg2uErH2VG/d0db20e0236f2ecc20d2347f531d6816/Intro_to_Cloudflare__2025_.pdf
[389] https://blog.cloudflare.com/zero-trust-managed-services/
[390] https://docs.azure.cn/en-us/aks/azure-cni-powered-by-cilium
[391] https://docs.cilium.io/en/latest/installation/k8s-install-aks/
[392] https://docs.azure.cn/en-us/aks/concepts-network-cni-overview
[393] https://docs.azure.cn/en-us/aks/update-azure-cni
[394] https://docs.cilium.io/en/stable/gettingstarted/k8s-install-default/
[395] https://docs.azure.cn/en-us/aks/cross-cluster-networking-fleet-use-cases
[396] https://microsoft.github.io/k8s-on-azure-workshop/module-2/2_aks_networking/2_cni_networks/index.html
[397] https://docs.azure.cn/en-us/aks/azure-cni-overlay
[398] https://docs.cilium.io/en/latest/network/clustermesh/setup/
[399] https://docs.cilium.io/en/latest/network/servicemesh/istio/
[400] https://cilium.io/use-cases/cluster-mesh/
[401] https://docs.cilium.io/en/latest/network/kubernetes/compatibility/
[402] https://docs.cilium.io/en/stable/network/clustermesh/setup/
[403] https://docs.cilium.io/en/latest/network/clustermesh/aks-clustermesh-prep/
[404] https://learn.microsoft.com/en-us/azure/aks/use-network-policies
[420] https://github.com/istio/istio.io/blob/master/LICENSE
[421] https://github.com/istio/istio/blob/master/licenses/k8s.io/klog/v2/LICENSE
[422] https://github.com/istio/istio/blob/master/licenses/cel.dev/expr/LICENSE
[423] https://www.hashicorp.com/en/bsl
[424] https://github.com/istio-ecosystem/dns-discovery/blob/master/LICENSE
[425] https://github.com/coredns/deployment/blob/master/LICENSE
[426] https://github.com/istio/community/blob/master/LICENSE
[427] http://github.com/coredns/alternate/blob/master/LICENSE
[428] https://github.com/layer5io/istio-service-mesh-workshop/blob/master/LICENSE
[429] https://github.com/istio/istio/blob/master/istioctl/pkg/version/version.go
[430] https://github.com/hashicorp/packer/blob/main/LICENSE
[431] https://github.com/istio/istio/blob/master/pilot/pkg/model/jwks_resolver.go
[432] https://github.com/coredns/coredns
[433] https://github.com/hashicorp/nomad/blob/main/LICENSE
[434] https://github.com/istio/istio
[435] https://github.com/inlets/inletsctl/blob/master/LICENSE
[436] https://github.com/inlets/inlets-operator/pkgs/container/inlets-operator
[437] https://github.com/Nginx/
[438] https://github.com/inlets/inlets-operator/blob/master/chart/inlets-operator/README.md
[439] https://github.com/orgs/nginx/repositories
[440] https://docs.nginx.com/nginx-gateway-fabric/overview/gateway-architecture/
[441] https://github.com/orgs/inlets/repositories
[442] https://github.com/nginx/nginx-gateway-fabric/pkgs/container/nginx-gateway-fabric%2Fnginx
[443] https://docs.nginx.com/nginx-gateway-fabric/overview/nginx-plus/
[444] https://inlets.dev/eula
[445] https://dev.to/instatunnel/self-hosting-for-data-sovereignty-the-open-source-shift-in-reverse-proxying-205j
[446] https://nginx.org/
[447] https://docs.inlets.dev/reference/inlets-operator/
[448] https://github.com/inlets
[449] https://github.com/ALeggeUp/inlets-oss
[405] https://cloudflare-docs.justalittlebyte.ovh/cloudflare-one/identity/users/seat-management/
[406] https://developers.cloudflare.com/api/resources/zero_trust/subresources/devices/subresources/ip_profiles/models/ip_profile/
[407] https://developers.cloudflare.com/api/typescript/resources/zero_trust/subresources/access/subresources/users/
[408] https://developers.cloudflare.com/api/terraform/resources/zero_trust/subresources/organizations/
[409] https://developers.cloudflare.com/api/resources/zero_trust/subresources/organizations/methods/create/
[410] https://developers.cloudflare.com/api/go/resources/zero_trust/subresources/organizations/methods/update/
[411] https://teitunnel.teispace.com/docs/reference/limits/
[412] https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/device-registration/
[413] https://www.cloudflare.com/service-specific-terms-zero-trust-services/
[414] https://51e47fea.previews.developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/protocols/
[415] https://developers.cloudflare.com/api/resources/zero_trust/subresources/access/subresources/users/methods/list/
[416] https://developers.cloudflare.com/api/node/resources/zero_trust/subresources/organizations/methods/create/
[417] https://blog.cloudflare.com/quick-tunnels-anytime-anywhere/
[418] https://hookdeck.com/webhooks/platforms/cloudflare-tunnel-alternatives-for-local-webhook-development
[419] https://localtonet.com/blog/cloudflare-tunnel-alternative
[450] https://github.com/fatedier/frp/blob/dev/cmd/frps/root.go
[451] https://github.com/fatedier/frp/blob/d20a2329/client/proxy/proxy.go
[452] https://github.com/fatedier/frp
[453] https://github.com/cert-manager/cert-manager/blob/master/cmd/controller/app/controller.go
[454] https://github.com/fatedier/frp/blob/dev/cmd/frpc/sub/verify.go
[455] https://github.com/cert-manager/cert-manager/blob/master/pkg/issuer/acme/http/http.go
[456] https://github.com/cert-manager/cert-manager/blob/master/pkg/issuer/vault/setup.go
[457] https://www.wares.gg/apps/frp
[458] https://github.com/fatedier/frp/blob/d20a2329/server/control_test.go
[459] https://github.com/cert-manager/cert-manager/blob/master/cmd/controller/app/options/options.go
[460] https://github.com/fatedier/frp/blob/d20a2329/pkg/config/v1/server.go
[461] https://github.com/cert-manager/cert-manager/blob/master/pkg/controller/test/context_builder.go
[462] https://github.com/fatedier/fft/blob/master/LICENSE
[463] https://github.com/cert-manager/cert-manager/blob/master/hack/containers/Containerfile.cainjector
[464] https://github.com/fatedier/frp/blob/v0.61.2/pkg/config/v1/api.go
[465] https://tailscale.com/opensource
[466] https://tech-insider.org/tailscale-vs-wireguard-2026/
[467] https://netbird.io/knowledge-hub/tailscale-vs-netbird
[468] https://pangolin.net/news/twingate-v-tailscale
[469] https://www.ssdnodes.com/learn/netbird-vs-tailscale
[470] https://marketplace.microsoft.com/en-us/product/saas/lynxroute.headscale?tab=overview
[471] https://www.opentechhub.io/headscale/
[472] https://www.opensourcedrop.com/tools/juanfont/headscale
[473] https://www.cloudflare.com/plans/
[474] https://blog.cloudflare.com/quick-tunnels-anytime-anywhere/
[475] https://marketplace.microsoft.com/en-us/product/lynxroute.headscale?tab=Overview
[476] https://simeononsecurity.com/articles/tailscale-vs-headscale-comparison-guide/
[477] https://github.com/Twingate
[478] https://toolradar.com/tools/cloudflare-tunnel/pricing
[479] https://www.cloudflare.com/pricing/.md
