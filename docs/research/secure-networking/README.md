# Secure networking for Lake: research

Research for [operator#437](https://github.com/oramasearch/orama-operator/issues/437), the epic for
secure networking in Lake: a mesh, workspaces as locked-down sandboxes, explicit network and DNS
connections, connections that carry credentials, developer tunnels, and peer connections.

**This folder is research, not a design, and nothing in it is decided.**

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

## Read this before relying on the report

- **It is one model's synthesis of web sources.** Its claims carry citations, and none has been
  verified against a cluster or re-read at the source by us.
- **It was asked about Istio and Cloudflare by name**, so it recommends them. It was not asked to
  compare against building our own.
- **It has no licensing analysis.** It was run before the resale requirement was stated.
- **Its own first phase is a lab**: it says the combination of Istio's ambient mode with AKS-managed
  Cilium has to be proven on a disposable cluster before anything is chosen, because AKS-managed
  Cilium is not the same as self-managed Cilium.

## Open, in order

1. Research the build-our-own path in C++: scope, placement, what Zetta already has.
2. Run section G of the prompt, or get the licensing answers another way.
3. A commercial answer from Cloudflare, if its service is to be used at all.
4. Only then, a design.
