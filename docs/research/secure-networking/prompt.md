# The research prompt

Written 2026-10-13 for Perplexity deep research, from Steve's request (quoted in
[the epic](https://github.com/oramasearch/orama-operator/issues/437)). Sections A to F produced
[`perplexity-report-2026-10-13.md`](perplexity-report-2026-10-13.md).

**Section G, on licensing, was written after that run** and has not been answered by a report.
The first licensing check is in [`licensing.md`](licensing.md).

Three things in the prompt were added by the agent that wrote it, not asked for by Steve: the
comparison with other tunnel providers (so the abstract kind is designed against more than one),
the invitation to say where Cilium or Cloudflare would do a job better than Istio, and treating
peering as "possibly".

---

# Deep research: secure networking for a multi-tenant Kubernetes platform with Istio and Cloudflare Tunnel

## Who is asking and what will be done with the answer

I run a platform called Lake on Azure Kubernetes Service. A custom Kubernetes operator (written in Go with controller-runtime) installs and manages everything declaratively through custom resources. I am about to design a secure networking layer for it. I need a current, cited, engineering-grade report that I can turn directly into custom resource designs and an implementation plan. Assume the reader is a senior platform engineer. Prefer primary sources: official documentation, release notes, GitHub issues and design documents, and vendor engineering blogs. Give the date and version each claim applies to. Where sources disagree or something is unproven, say so plainly.

**Recency matters.** Identify the latest stable releases as of today of Istio, Cloudflare Tunnel (`cloudflared`), the Kubernetes Gateway API, and Cilium, and base every recommendation on those versions. Call out anything that changed in the last 12 months, anything deprecated, and anything still alpha or beta.

## The platform as it exists

- **Clusters:** four AKS clusters on Kubernetes 1.35: one management cluster and three workload ("leaf") clusters in different Azure regions (East US, West US 2, Sweden Central). More will be added, some possibly on AWS or GCP later. There is no network path between the clusters today.
- **CNI:** Azure CNI in overlay mode with the Cilium dataplane and Cilium network policy enabled (the AKS-managed Cilium, not a self-installed one).
- **Ingress:** ingress-nginx with one public load balancer per cluster. Public DNS is in Cloudflare and is written by our operator through the Cloudflare API.
- **No service mesh and no tunnels today.**
- **Tenancy model:** each customer has one or more *workspaces*. A workspace is one Kubernetes namespace. It enforces the `restricted` Pod Security Standard, has a ResourceQuota and LimitRange, and has NetworkPolicies written by our operator.
- **What runs in a workspace:**
  - *Services*: managed datastores created on demand from a catalog. Postgres (CloudNativePG), Kafka (Strimzi), Cassandra (K8ssandra), ClickHouse (Altinity operator), TiDB, Valkey, plus our own engine (Zetta) and small HTTP-wrapped engines (SQLite, DuckDB, Polars). Each is created by an upstream operator that runs in its own namespace and must reach into the workspace namespace.
  - *Apps*: customer web applications built from source and run as Deployments.
  - *Deployments*: longer-lived customer environments.
  - *Agents and jobs*: short-lived workloads.
- **Secrets:** a customer's secrets live in our own store and are projected into Kubernetes Secrets only when a workload must mount them.
- **Design rule we follow:** every piece of infrastructure is an *abstract* custom resource that points at a *provider*, which creates a *concrete* resource. For example `KubernetesCluster` → `KubernetesClusterProvider` → `AksCluster`. Tunnels must follow the same pattern: an abstract `Tunnel` with a provider, and `CloudflareTunnel` as the first concrete kind, so another provider can be swapped in for a customer with different requirements.

## What I want to build

1. **Workspaces as locked-down sandboxes.** A workspace runs untrusted customer workloads. By default nothing in it can reach anything, inside or outside the cluster, and nothing can reach it.
2. **Every network connection and every DNS resolution is explicit.** A workload can connect to a destination, and resolve its name, only if a declared connection allows it. I want to know what can actually enforce this, at which layer, and what cannot be enforced.
3. **Internal connections: app to service.** An app in a workspace connects to a service in the same workspace (and possibly another workspace of the same customer) only through a declared connection. A connection may carry a **credential scoped to what the app needs**, for example a database role with read-only access to one schema, issued for that app and revocable.
4. **External data connections.** An app connects to a data source outside the cluster (a customer's own Postgres, Snowflake, an S3 bucket, a SaaS API) only through a declared connection, with egress limited to that destination.
5. **External tunnels for developers.** A developer on a laptop connects to a specific service, app or deployment in a workspace through a tunnel, with authentication and authorization, without that workload being exposed to the internet. This must work for TCP protocols (Postgres, Kafka, Cassandra CQL, the MySQL wire protocol, Redis) as well as HTTP.
6. **Peer connections, possibly.** Private connectivity between Kubernetes clusters, between clusters of our own distributed engine that span regions, and between a cluster and a customer's AWS VPC.
7. **Istio is the intended mesh**, installed and operated by our operator. I am open to being told a part of this is better done by Cilium or by Cloudflare.

## Questions to answer

### A. Istio today
1. What is the latest stable Istio release, its support window, and what changed in the last year? What is the status of **ambient mode** (ztunnel and waypoint proxies) versus sidecars: which features are GA, beta or missing in ambient?
2. **Istio on AKS with Azure CNI overlay and the AKS-managed Cilium dataplane**: is this combination supported and by whom? What are the known conflicts between Cilium (eBPF, kube-proxy replacement, its own network policy and L7 proxy) and Istio sidecars or ztunnel, and the required Cilium settings (for example socket load-balancing and `bpf-lb-sock-hostns-only`)? Can those settings be changed on AKS-managed Cilium at all?
3. **The AKS Istio add-on versus upstream Istio installed with Helm**: versions offered, ambient support, what can and cannot be configured, upgrade model, and which is better when a custom operator must own the install on every cluster identically, including non-Azure clusters later.
4. Does Istio (sidecar and ambient) work for pods under the **`restricted` Pod Security Standard**? What does istio-cni require, and what privileges does it need on the node?
5. **Installation and lifecycle from an operator**: the recommended install method now (Helm charts, `istioctl`, the in-cluster operator's deprecation status), revision-based and canary upgrades, and the order CRDs, istiod, CNI, ztunnel and gateways must be installed in.
6. **The Gateway API** with Istio: the current recommended API for ingress, for waypoints and for egress, and what remains only in Istio's own APIs (`VirtualService`, `DestinationRule`, `ServiceEntry`, `Sidecar`, `AuthorizationPolicy`, `PeerAuthentication`).

### B. Locking a workspace down
7. A reference design for **default-deny multi-tenancy by namespace** using Istio together with Kubernetes or Cilium NetworkPolicy: what each layer enforces, and why both are needed. Include strict mTLS, `AuthorizationPolicy` default-deny, and how identities (SPIFFE, service accounts) map to "this app".
8. **Explicit egress**: compare `outboundTrafficPolicy: REGISTRY_ONLY` with `ServiceEntry`, an egress gateway, egress in ambient mode (waypoints), Cilium FQDN (`toFQDNs`) policies and Cilium's DNS proxy. Which can be bypassed by a workload (for example by connecting to an IP address directly), and what closes that hole?
9. **Explicit DNS**: how to make a workload able to resolve only the names it has a connection for. Options to evaluate: Cilium DNS-aware policy, Istio's DNS proxying, per-namespace CoreDNS policy or views, NodeLocal DNSCache interactions, and blocking direct access to external resolvers and DNS-over-HTTPS. What is realistic and what is theater?
10. **Third-party operators reaching into locked-down namespaces**: CloudNativePG, Strimzi, K8ssandra, the Altinity ClickHouse operator, the TiDB operator and a Valkey operator each manage pods in the workspace namespace from their own namespace, and several run admission webhooks. What do mesh enrollment and default-deny do to them, and what are the known issues and required exclusions for each (for example Kafka inter-broker traffic, Cassandra gossip, Postgres replication, headless Services and StatefulSet pod-to-pod traffic, server-first protocols, and operators that do their own TLS)?
11. **Non-HTTP protocols in the mesh**: how Postgres, Kafka, Cassandra, MySQL-wire and Redis traffic behave under mTLS in sidecar and ambient modes, including protocols that negotiate their own TLS (Postgres `STARTTLS`), and what L4 authorization can and cannot express.

### C. Connections that carry credentials
12. Patterns for a declared "app A may use service B as role R" connection that both opens the network path and issues a **scoped, revocable credential**: per-connection database roles (for example through CloudNativePG's declarative role management), Kafka ACLs through Strimzi's `KafkaUser`, short-lived credentials, and workload identity (SPIFFE/SPIRE, Istio identities, Kubernetes projected service account tokens). What is mature, and what do comparable platforms do (Heroku add-on attachments, Render, Fly.io, Railway, Vercel integrations, Crossplane connection secrets, the Service Binding specification for Kubernetes)?
13. For **external data connections**, how to combine an egress allowance with a stored credential, and how to give a workload a stable egress IP address that a customer can allow-list.

### D. Cloudflare Tunnel today
14. What is the latest `cloudflared`, and the current model: remotely-managed versus locally-managed tunnels, the API to create and configure them, replicas and high availability, and limits and pricing that matter at the scale of one tunnel per workspace or per connection.
15. **Developer access to private TCP services** through Cloudflare: compare `cloudflared access tcp`, WARP with private network routing, WARP Connector, and browser-rendered access. Which protocols work, what the developer must install, and how authentication and per-resource authorization work with Cloudflare Access (identity providers, service tokens, short-lived certificates).
16. **Running `cloudflared` in Kubernetes**, especially in a namespace with the `restricted` Pod Security Standard and default-deny egress: required egress destinations and ports, whether it can be a sidecar, a per-namespace Deployment or a shared gateway, and how it interacts with an Istio mesh (should the tunnel terminate at an Istio gateway, and how is the caller's identity carried through).
17. Cloudflare's **official Kubernetes integrations**: the state of any Cloudflare Tunnel ingress controller or Gateway API implementation, and the notable community operators. Are any production-ready, or is calling the Cloudflare API from our own operator the better path?
18. **Alternatives behind the same abstraction**, so the abstract `Tunnel` kind is designed correctly: Tailscale (and its Kubernetes operator), Twingate, ngrok (and its operator), Teleport, Boundary, Azure Private Link and AWS PrivateLink, and plain WireGuard. For each: what a "tunnel" object would have to express, and which fields are common to all of them versus provider-specific.

### E. Peering
19. **Istio multi-cluster** today: multi-primary and primary-remote, east-west gateways, trust-domain federation, and the state of multi-cluster in ambient mode. What is required when clusters have overlapping pod CIDRs (ours all use the same overlay ranges)?
20. **Cilium Cluster Mesh** as an alternative or complement, and whether it is available with AKS-managed Cilium.
21. Private connectivity from AKS to a **customer's AWS VPC**: site-to-site VPN, Azure and AWS private link offerings, Cloudflare Magic WAN and WARP Connector, and Tailscale subnet routers. Compare setup burden on the customer, cost, and how each could be modeled as a declarative "peer connection" resource.
22. For a **distributed database that spans regions** (our own engine, which today dials out from each cluster to a publicly reachable rendezvous address): what a mesh or peering layer offers over that model, and the latency and reliability costs.

### F. Turning this into an API and a product
23. Propose a **custom resource model** following the abstract → provider → concrete pattern for: the mesh itself, a workspace's network sandbox, an internal connection (app to service, with an optional credential role), an external data connection, a developer tunnel, and a peer connection. For each, list the fields, which are provider-neutral, the status a user needs to see, and what the operator must create underneath. Point out where existing projects' APIs are worth copying.
24. **Failure modes and operations**: what breaks first (certificate rotation, ztunnel or istio-cni upgrades, DNS proxy outages, tunnel replicas), what to monitor, how to debug "this app cannot reach this service", and the blast radius of a mesh outage on a multi-tenant cluster.
25. **Performance and cost**: the overhead of sidecars versus ambient per pod and per node, and of waypoints, with current published numbers.
26. **Product navigation**: how do comparable platforms present networking to customers (Fly.io private networking, Railway private networking and TCP proxy, Render private services, Heroku Private Spaces, Vercel Secure Compute, Cloudflare Zero Trust's dashboard, Tailscale's admin console)? Propose a navigation structure for a "Network" area of a product covering: the workspace's sandbox rules, internal connections, external data connections, developer tunnels, peer connections, DNS, and an audit of what connected to what.

## What I want back

1. An **executive summary** with a recommended architecture and the three or four decisions that matter most, each with the alternative you rejected and why.
2. A **compatibility matrix**: Istio sidecar and Istio ambient against AKS-managed Cilium on Azure CNI overlay, the `restricted` Pod Security Standard, each of the datastore operators listed, and Cloudflare Tunnel, with a status (works, works with configuration, known problems, unknown) and a source for each cell.
3. Answers to **every numbered question above**, in order, each with citations.
4. A **proposed custom resource model** (question 23) with example YAML.
5. A **phased implementation plan**, smallest safe first step first, with what each phase proves.
6. A list of **open risks and things you could not verify**, clearly separated from what is established.
7. **Version table**: every product and version the report relies on, with release dates.

Do not pad. Where the honest answer is "this is not possible" or "this is not production-ready", say that.

---

## Section G, added after the first run

Paste before "What I want back" when the prompt is run again.

### G. Licensing and the right to resell
I will resell this, both as a hosted service and as an enterprise product installed for a customer. For every component you recommend, and every alternative you compare:
27. State the **current license with its SPDX identifier**, citing the LICENSE file in the official repository, and any license change in the last three years. Flag anything that is not a permissive open-source license: AGPL (Teleport), the Business Source License (HashiCorp/IBM Boundary and other HashiCorp products), SSPL, the Elastic License, "source-available" terms, or open-core products whose needed features are in a paid edition.
28. State plainly whether it may be **embedded in and redistributed as part of a commercial product**, and **offered as a hosted service to third parties**, including one that competes with the vendor, and what the obligations are (NOTICE files, attribution, source disclosure, trademark limits).
29. Separately from software licenses, analyze the **service terms** of every hosted service involved, above all **Cloudflare Tunnel and Cloudflare Zero Trust / Access**. Cloudflare's Self-Serve Subscription Agreement section 2.2.1 prohibits selling access to the services to a third party, or signing up on a third party's behalf, without written permission. Determine: whether creating tunnels in my own Cloudflare account that carry my customers' traffic is permitted; whether the Cloudflare Partner Network, the Tenant API, or Cloudflare for Platforms is required and what they cost; whether a customer bringing their own Cloudflare account avoids the issue; tunnel limits and Zero Trust seat pricing per plan at the scale of one tunnel per workspace; and any restriction on proxying non-HTTP traffic such as database protocols. Quote the clauses and give their dates. Do the same for Tailscale, ngrok and Twingate.
30. Recommend the **tunnel provider or providers I am actually permitted to resell**, and a fully self-hostable, permissively licensed fallback (for example WireGuard-based) for customers who cannot use a third-party service.

Add to the deliverables: a **licensing table** covering every component and service in the report, with license or governing terms, resale status (permitted, permitted with obligations, requires a commercial agreement, not permitted), and a source for each row.
