# Licensing: what may be resold

Steve, 2026-10-13: *"this also needs to be permissible licensed for us to resell"*.

Everything the networking work builds on must be licensed so Orama can resell it, as the hosted
product and as an enterprise install delivered to a customer. This page is the first check. It was
done by an engineer and is **not legal advice**; a contract or a hosted service's terms need a
lawyer or the vendor's written answer.

## First check (2026-10-13)

Licenses read from each project's own repository (GitHub's license API and the `LICENSE` file); Cloudflare's service terms through Perplexity, which quoted the clause from https://www.cloudflare.com/terms/. This is a first pass by an engineer, not legal advice.

### Software: what we would install or embed

| Project | License | Resell and embed |
|---|---|---|
| Istio (`istio/istio`, `istio/ztunnel`) | Apache-2.0 | Yes. Keep the license and NOTICE files; no source disclosure; no trademark rights. |
| Envoy | Apache-2.0 | Yes, same terms. |
| Cilium | Apache-2.0 | Yes, same terms. |
| Kubernetes Gateway API | Apache-2.0 | Yes, same terms. |
| `cloudflared` (the tunnel client) | Apache-2.0 | Yes, the **software**. The **service** is a separate question, below. |
| SPIRE, CoreDNS, cert-manager, ingress-nginx | Apache-2.0 | Yes. |
| The datastore operators already installed: CloudNativePG, Strimzi, K8ssandra, Altinity ClickHouse, TiDB, Valkey | Apache-2.0 | Yes. |

### Software: alternative tunnel providers, for the abstract kind

| Project | License | Resell and embed |
|---|---|---|
| Tailscale client and Kubernetes operator | BSD-3-Clause | The code, yes. The coordination service is proprietary and has its own terms. |
| ngrok Kubernetes operator | MIT | The operator, yes. The ngrok service and agent have their own terms. |
| `wireguard-go` | MIT | Yes. (The Linux kernel implementation is GPL-2.0, as part of the kernel; using it does not affect our code.) |
| **Teleport** | **AGPL-3.0** | **Not without care.** AGPL requires offering the source of the program, with modifications, to users who interact with it over a network. Embedding it in a resold or hosted product needs a legal decision or Teleport's commercial license. |
| **HashiCorp (IBM) Boundary** | **Business Source License 1.1** | **No.** Its Additional Use Grant excludes "offering the Licensed Work to third parties on a hosted or embedded basis in order to compete with" the vendor's paid version. Reselling it embedded in our product is the excluded case. |

### The Cloudflare service, which is the real risk

The `cloudflared` license does not cover the Cloudflare Tunnel **service**. Cloudflare's Self-Serve Subscription Agreement, section 2.2.1, says:

> Unless otherwise expressly permitted in writing by Cloudflare, you will not and you have no right to: (a) rent, lease, loan, export, or sell access to the Services to any third party, or sign up for the Services on behalf of a third party

So on a self-serve plan we may not sell access to Cloudflare Tunnel or Zero Trust, and may not sign customers up for it, without Cloudflare's written permission. Using Cloudflare behind our own product is not categorically forbidden by that sentence, but the agreement does not authorize resale either. Cloudflare's Tenant API, the documented way for a provider to create and manage customer accounts, is available only to Cloudflare Partners on Enterprise plans, under a partner agreement.

**Not established by this check:** whether tunnels in *our* account carrying customers' traffic count as "selling access"; current Tunnel limits per plan; Zero Trust seat pricing for developers using WARP or Access; and any restriction on proxying database (non-HTTP) traffic. Each needs Cloudflare's current documents or a written answer from Cloudflare.

## What this means for the epic

1. **Istio and everything in the mesh layer is clear to resell.** All Apache-2.0.
2. **Cloudflare Tunnel as the first tunnel provider needs a commercial answer from Cloudflare before it is built into a product we resell.** This is the strongest reason the tunnel must be an abstract kind: a customer, or we, may need a provider we are allowed to resell.
3. **Boundary is out, and Teleport needs a legal decision,** if either is ever proposed as a provider.
4. **An enterprise install is a different case from the hosted product.** There the customer could bring their own Cloudflare account, and we would ship only Apache-2.0 software. Worth designing for.
5. **Every new dependency this epic adds gets its license recorded** where it is introduced, with the same check.

The research prompt has a licensing section to match ([`prompt.md`](prompt.md), section G), not yet run.
