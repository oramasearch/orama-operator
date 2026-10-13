# Secure runtimes for apps, agents and builds: research

Research for [lake#725](https://github.com/oramasearch/lake/issues/725), the epic for the security
of Lake as a cloud service. Steve, 2026-10-13: *"apps/agents should not be able to jailbreak out of
their sandboxes ... we might need secure runtimes for them too (use perplexity mcp for this; eg.
firecracker?)"*.

**This folder is research, not a design, and nothing in it is decided.** As with the networking
research ([`../secure-networking/`](../secure-networking/README.md)), the stated direction is to
build our own C++ components where that is the better path, so what follows is a map of what
exists.

## What is here

| File | What it is |
|---|---|
| [`perplexity-answer-2026-10-13.md`](perplexity-answer-2026-10-13.md) | Perplexity's answer, as returned, with its citations. |

## What it says, briefly

- **No runtime makes hostile code safe by itself.** A virtual-machine boundary is stronger than a
  container or a userspace kernel; network, storage, identity and admission stay separate attack
  surfaces.
- **gVisor** is a userspace kernel, not a VM. **Kata Containers** puts each pod in a small VM, with
  Cloud Hypervisor, QEMU or Firecracker underneath. **Firecracker used directly** is a microVM
  monitor and means building the orchestration around it. **AKS Pod Sandboxing** is Microsoft's
  managed Kata on its own hypervisor. **Confidential Containers** are in preview on AKS.
  **WebAssembly runtimes** suit narrowly scoped agent tools, not arbitrary native code.
- **For our clusters, AKS Pod Sandboxing is the closest fit and is unverified.** It needs Azure
  Linux node pools and VM sizes with nested virtualization. Our nodes are Ubuntu on
  `Standard_D8as_v7`.
- **It singled out the build namespace**: untrusted source should not be built where `privileged`
  is enforced. Ours is (`internal/controller/lake_builds.go`).

## What it could not establish

Perplexity said so itself, and these are the questions a deeper pass must answer:

- current versions and whether AKS Pod Sandboxing is generally available;
- whether it works with `Standard_D8as_v7`, with AKS-managed Cilium policy, and with our volumes;
- the CVE and escape history of each runtime;
- overhead and cold-start figures;
- how other platforms that run customer or agent code isolate it.

## Licences, read from each project's repository (2026-10-13)

| Project | Licence | May be resold |
|---|---|---|
| gVisor, Kata Containers, Firecracker, containerd | Apache-2.0 | Yes |
| Wasmtime, WasmEdge, Spin | Apache-2.0 | Yes |
| Confidential Containers operator | Apache-2.0 | Yes |
| Trivy, Grype, Falco, cosign, Kyverno, Gatekeeper (scanning and policy, for the same epic) | Apache-2.0 | Yes |
| QEMU | GPL-2.0, with parts under other licences | Running it is fine; shipping a modified copy carries GPL obligations |
| Cloud Hypervisor | Not read (the licence API returned nothing) | Check before relying on it |
| AKS Pod Sandboxing | A Microsoft Azure service feature | Governed by Azure's terms, not a licence we redistribute; not available outside Azure, which matters for an enterprise install |

## The question

> I run a multi-tenant platform on Azure Kubernetes Service (Kubernetes 1.35, Azure CNI overlay
> with the Cilium dataplane, AMD Standard_D8as_v7 nodes). Customers run untrusted code: web apps
> built from their source, and AI agents that execute generated code and tools. Today these run as
> ordinary containers under the 'restricted' Pod Security Standard in one namespace per workspace.
> I need to know which SECURE CONTAINER RUNTIMES can stop a workload escaping its sandbox to the
> node or to other tenants. Compare, with current versions and dates and primary sources: (1)
> gVisor (runsc), (2) Kata Containers with Cloud Hypervisor, QEMU and Firecracker, (3) AKS Pod
> Sandboxing (Kata on the Microsoft hypervisor): is it GA, which node OS and VM sizes does it need,
> does it need nested virtualization and do Dasv7/AMD sizes support it, what is unsupported, (4)
> Firecracker used directly rather than through Kata, and how Fly.io, AWS Lambda, E2B, Modal,
> Vercel Sandbox, Daytona, Cloudflare, Replit and GitHub Codespaces isolate untrusted or agent
> code, (5) Confidential Containers, (6) WebAssembly runtimes as an alternative for agent tools.
> For each give: isolation boundary and known escape history, performance and memory overhead and
> cold start, compatibility with Kubernetes RuntimeClass, NetworkPolicy/Cilium, persistent volumes,
> GPUs and the restricted Pod Security Standard, operational burden, whether it works on AKS and on
> EKS/GKE, and its licence with whether it may be embedded in a product that is resold. Then
> recommend what to use for (a) long-running customer web apps, (b) short-lived AI agent code
> execution, (c) builds of customer source code (currently a privileged-enforce namespace), and
> say what container-level hardening is still required with each. Be explicit about what is not
> production-ready.

## Open

1. A deeper research pass on the questions above, including the build-our-own path.
2. A threat model: what "cannot jailbreak" must mean for apps, agents and builds.
3. Then a design.
