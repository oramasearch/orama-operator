# Secure networking for Lake: research

Research for [operator#437](https://github.com/oramasearch/orama-operator/issues/437), the epic for
secure networking in Lake: a mesh, workspaces as locked-down sandboxes, explicit network and DNS
connections, connections that carry credentials, developer tunnels, and peer connections.

**This folder is research, not a design, and nothing in it is decided.** The design that follows from it is [`docs/infrastructure/secure-networking.md`](../../infrastructure/secure-networking.md).

## The direction that comes before any provider choice

Steve, 2026-10-13, on receiving the report:

> before deciding on any of these providers, know that our instinct is to implement our own c++
> components, so that's probably the best path for customization, flexibility, observability, etc...

So the report below is read as **a map of the problem and of what exists**, not as a shopping list.
Its recommendation (upstream Istio in ambient mode, Cilium policy as the boundary, Cloudflare Tunnel
for developers) describes what could be assembled from other people's software. The working
assumption is the opposite: Orama builds its own components in C++, and adopts someone else's only
where building it is plainly the wrong use of effort.

What that changes in how to use the report:

- **Its questions and failure modes are the valuable part.** What must be enforced and at which
  layer, what a workload can bypass, what the datastore operators need, which protocols negotiate
  their own TLS, what breaks first. A component of our own has to answer the same questions.
- **Its custom resource model is a starting point for ours**, since the abstract kinds (a sandbox, a
  connection, a tunnel, a peer connection) are needed whoever implements them. The abstract kind
  with a provider behind it stays the rule: our own implementation is one provider, and a
  third-party one can be another.
- **Its product comparisons and licensing findings stand** whatever is built.
- **What it does not contain**: any assessment of building these components ourselves. What a C++
  data plane, tunnel or policy enforcer of our own would take, where it would sit relative to the
  CNI, and what it would reuse from Zetta's networking, has not been researched. That is the next
  piece of research, and it is not written.

## What is here

| File | What it is |
|---|---|
| [`perplexity-report-2026-10-13.md`](perplexity-report-2026-10-13.md) | The deep-research report, **verbatim** as returned on 2026-10-13: a recommended architecture, a compatibility matrix, answers to 26 questions with citations, a proposed custom resource model, a phased plan and open risks. It reports its own version baseline (Istio 1.31.x, Gateway API 1.5.0, Cilium 1.20.2, `cloudflared` 2026.10.0); none of those was checked here. |
| [`prompt.md`](prompt.md) | The prompt that produced it, and section G on licensing, written afterwards and not yet run. |
| [`licensing.md`](licensing.md) | The first licensing check: which components may be resold, and why the Cloudflare **service** is not cleared. |
| [`licensing-perplexity-2026-10-14.md`](licensing-perplexity-2026-10-14.md) | **Section G, run** (2026-10-14), verbatim: the licence and resale status of every component and alternative, the service terms of Cloudflare, Tailscale, ngrok and Twingate quoted with dates, and a licensing table with a source per row. |
| [`build-our-own-perplexity-2026-10-14.md`](build-our-own-perplexity-2026-10-14.md) | **The build-our-own path** (2026-10-14), verbatim: which layers to own in C++ and which to keep, the C++ building blocks and their licences, a node proxy beside AKS-managed Cilium, how each datastore protocol negotiates TLS, observability, and a staged plan that keeps one API. |

## Read this before relying on the report

- **It is one model's synthesis of web sources.** Its claims carry citations, and none has been
  verified against a cluster or re-read at the source by us.
- **It was asked about Istio and Cloudflare by name**, so it recommends them. It was not asked to
  compare against building our own.
- **It has no licensing analysis.** It was run before the resale requirement was stated.
- **Its own first phase is a lab**: it says the combination of Istio's ambient mode with AKS-managed
  Cilium has to be proven on a disposable cluster before anything is chosen, because AKS-managed
  Cilium is not the same as self-managed Cilium.

## What the two 2026-10-14 reports add

Both are one model's synthesis of web sources, cited but not verified here. Read as such:

- **Cloudflare Tunnel, one tunnel per workspace in our own account, is not a safe resale.** The
  Self-Serve Subscription Agreement (updated 2025-09-12) §2.2.1(a) forbids selling access to the
  services or signing up for a third party without written permission, and §2.2.1(j) forbids using
  them to provide a VPN or similar proxy service. The Zero Trust terms §2.2 forbid resale, including
  as a managed security service or service bureau. A customer's own Cloudflare account, with us
  selling only our software, avoids our reselling it. Default limits: 1,000 tunnels and 500 Access
  applications per account.
- **A self-hostable, permissively licensed tunnel is possible**: WireGuard (`wireguard-go`, MIT;
  kernel WireGuard is GPL-2.0, used through the OS) with our own control plane and relay. That is
  the provider for the hosted product and for air-gapped installs; others (ngrok with written
  clearance, a customer's own Cloudflare) are optional providers behind the same abstract kind.
- **Licences to avoid**: Teleport's binaries since v16 (commercial licence), Boundary (BUSL-1.1),
  NetBird's server since v0.53.0 (AGPL-3.0), inlets Pro and Netmaker `pro/` (proprietary), Vector
  and the Twingate operator (MPL-2.0).
- **FQDN egress on AKS needs Azure's paid Advanced Container Networking Services (ACNS)**. Checked
  on 2026-10-14: ACNS is **off** on `orama-production-cluster-01` (`advancedNetworking.enabled:
  false`). Without it, Kubernetes `NetworkPolicy` and Cilium L3/L4 policy enforce by address and
  port only, never by DNS name.
- **What to own, per the build-our-own report**: the workspace policy model and its generation, the
  identity-to-permission mapping, the credential broker (an explicit, protocol-aware proxy per
  datastore, Postgres first), the developer-tunnel control plane, and service-addressed gateways
  for peering. **What to keep**: AKS-managed Cilium as the enforcer (no second eBPF dataplane on its
  hooks), CoreDNS, and a proven mesh or proxy where transparent workload mTLS is needed. A C++ node
  proxy is a later, separately reviewed step behind the same resources.
- **Peering over overlapping networks** (every cluster's VNet is `10.224.0.0/12`) is done with
  service-addressed gateways, never by routing pod or VPC addresses. Cilium Cluster Mesh needs
  non-overlapping pod ranges.
- **Credentials cannot be injected into a protocol whose TLS is end to end.** Postgres, MySQL/TiDB,
  Kafka, Valkey and ClickHouse each need a protocol-aware, terminating proxy, which changes the TLS
  trust boundary; Zetta's node-to-node mTLS stays end to end.

## Open, in order

1. ~~Research the build-our-own path in C++.~~ Done 2026-10-14 (above). What Zetta already has for
   networking is still to be read in Zetta's own repository.
2. ~~Run section G.~~ Done 2026-10-14.
3. A commercial answer from Cloudflare, only if its service is to be used as a provider we resell.
4. The design: [`docs/infrastructure/secure-networking.md`](../../infrastructure/secure-networking.md).
