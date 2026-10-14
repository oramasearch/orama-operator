# Secure container runtimes: Perplexity's answer, 2026-10-13

Returned by Perplexity (`perplexity_ask`, high search context) to the question in
[`README.md`](README.md#the-question). Stored as returned; the citations are Perplexity's. Nothing
here was verified by us.

---

There is **no single runtime that makes arbitrary customer code safe by itself**. For hostile multi-tenant code, a VM boundary is stronger than an ordinary container or a userspace-kernel sandbox; network, storage, identity, admission policy, and node administration remain separate attack surfaces. In particular, AKS pod sandboxing is a useful isolation layer, but Microsoft explicitly says it does not by itself provide complete hard multitenancy.[1][10]

**Important freshness caveat:** the available primary-source results do not establish the requested current versions, release dates, complete CVE histories, or a definitive AKS support matrix for the named node SKU and Cilium setup. Some search results are dated in the future relative to today, so they cannot reliably substantiate "current" status. The comparison below distinguishes what the available sources support from what needs confirmation before production.

## Runtime comparison

| Option | Isolation boundary and Kubernetes fit | AKS, networking, storage, GPU, and PSA | Operations, performance, license |
|---|---|---|---|
| **gVisor (`runsc`)** | Intercepts application system calls in a userspace kernel (Sentry), reducing direct exposure to the host kernel. It is not a VM boundary, and gVisor does not make `hostNetwork`, `hostPID`, `hostPath`, or privileged workloads safe.[7] A Kubernetes `RuntimeClass` can select it. | The results do not verify AKS support for `runsc`, compatibility with Azure CNI overlay/Cilium, CSI, GPU, or current CVEs. Standard restricted PSA remains appropriate; still prohibit host namespaces, host paths, and privilege. | Requires installing and maintaining `runsc` and enforcing RuntimeClass selection; pods can otherwise silently use the default runtime.[7] No current performance or cold-start measurements were returned. License: not verified by the available sources; check the exact release's license before embedding or reselling. |
| **Kata with Cloud Hypervisor, QEMU, or Firecracker** | Kata makes the pod sandbox a VM with a guest kernel, rather than sharing the host kernel. The project lists QEMU, Cloud Hypervisor, and Firecracker as supported VMM choices; Cloud Hypervisor offers virtio-fs, while Kata's Firecracker backend does not support filesystem sharing and lacks VFIO/passthrough.[6] | Kubernetes uses a Kata runtime/shim, but the results do not establish that upstream Kata is supported on AKS, or that any of these backends work with this cluster's Cilium dataplane, CSI drivers, GPUs, or service mesh. Restricted PSA still applies. | Needs nested virtualization or bare metal according to the project's software page.[9] VM startup, guest memory, storage integration, and node capacity require workload-specific testing; no defensible current benchmark figures are available here. Kata software is Apache-2.0 according to the project; confirm licenses for bundled components and dependencies before product redistribution.[9] |
| **AKS Pod Sandboxing** | Microsoft describes a per-pod lightweight VM with its own guest kernel, using Kata and the Azure security-hardened hypervisor. Its boundary isolates the workload from the host kernel and other pod VMs, but not the control plane, storage/data paths, or cluster administrators.[1][10] | The published AKS material says sandboxing requires a Linux node pool using Azure Linux and Generation 2 VM sizes that support nested virtualization; Ubuntu and Windows are not supported.[1][8] The documented RuntimeClass names vary across the supplied results (`kata-mshv-vm-isolation` versus `kata-vm-isolation`), so verify the name exposed by the exact AKS feature/version before deployment.[4][8] The results do **not** establish support for `Standard_D8as_v7`, AMD nested virtualization, Cilium-specific policy behavior, CSI volumes, host networking, GPU, or service mesh. Do not assume these work. | This is the most directly relevant managed option for AKS, but the available sources do not establish whether it is GA today. They also do not give a reliable complete unsupported-feature list or current cold-start benchmark. Treat GA status, SKU eligibility, and feature compatibility as **unverified** until confirmed in current Microsoft AKS documentation for the target region and cluster configuration. |
| **Firecracker directly** | A minimal microVM VMM, but using it directly is not the same as using Kubernetes or Kata: you must supply lifecycle management, image handling, networking, storage, and orchestration. Kata's Firecracker backend limitations include no virtio-fs filesystem sharing and no VFIO/passthrough.[6] | No evidence in the results establishes direct Firecracker support as an AKS RuntimeClass or compatibility with Cilium, CSI, GPU, or service mesh. It is not a drop-in replacement for a container runtime. | Requires substantial custom platform engineering. No current CVE, performance, cold-start, or direct embedding-license details were returned; verify Firecracker's release license and its dependencies before redistribution. |
| **Confidential Containers** | Adds a confidential-computing trust boundary, typically protecting guest memory from some host-level access; it is distinct from ordinary pod sandboxing. The supplied AKS source says confidential containers use AMD SEV-SNP and are currently preview, with limitations.[5] | The result says AKS confidential containers currently support Azure Linux and describes `KataCcIsolation` as limited to Azure Linux 2.0; it also says they cannot share a node pool with pod sandboxing.[5] It does not verify compatibility with the stated D8as_v7 nodes, Cilium, CSI, GPUs, or service mesh. | Preview status and constraints make this a poor default for general production untrusted workloads. It is most relevant when protection from infrastructure operators is a requirement, not as a substitute for tenant isolation and application hardening. License and current upstream CVEs were not verified. |
| **WebAssembly: Wasmtime, WasmEdge, Spin** | Can restrict programs to a WASI/host API capability set, which is attractive for narrowly scoped agent tools. It is not automatically a VM boundary: safety depends on runtime correctness, enabled interfaces, and the host functions exposed. | The results do not verify the named runtimes' current Kubernetes `RuntimeClass`, AKS, Cilium, CSI, GPU, or persistent-volume integrations. Restricted PSA can still constrain the runtime pod, but does not itself secure a permissive host API. | Good candidate for tools that can be ported to WASI and need only explicit capabilities; poor fit for arbitrary native code, full Linux compatibility, or unmodified customer applications. Versions, cold-start/memory measurements, CVEs, and SPDX licenses were not verified in the available material. |

### CVE history and release status

The returned sources do not provide a reliable, comparable CVE timeline or exact current release/version for gVisor, Kata/VMMs, Firecracker, Confidential Containers, or the Wasm runtimes. Naming selected historical CVEs without checking affected versions and fixes would be misleading. Before choosing one, review each upstream project's security advisories and release notes, then pin a patched version and maintain an update process. The same applies to AKS GA status: the available AKS excerpts are inconsistent in terminology and do not securely establish today's status.

### Hosted sandbox products

These products should not be treated as equivalent runtime implementations or as evidence of AKS compatibility. The available sources do not establish how **Fly.io, AWS Lambda, E2B, Modal, Vercel Sandbox, Daytona, Cloudflare Workers/Containers, Replit, or GitHub Codespaces** isolate customer or agent code internally, nor their current isolation boundaries, CVE history, performance, or licenses. For a vendor decision, require current documentation covering: tenant boundary (process, container, microVM, or VM), host-kernel sharing, network egress controls, persistence and cleanup, secrets exposure, escape-response process, and contractual security commitments. Cloudflare Workers' isolate model and its Containers offering should be evaluated separately rather than assumed to share one boundary.

## Recommendations

**(a) Long-running customer web apps:** Prefer a **dedicated node pool or dedicated cluster per trust tier**, and use AKS Pod Sandboxing only after Microsoft confirms support for the exact AKS version, Azure Linux image, VM SKU, and Cilium configuration. If those checks fail, use a separately managed VM-based isolation tier rather than running hostile apps as ordinary `runc` containers on shared nodes. For persistent application data, externalize state where possible and test the chosen CSI path explicitly.

**(b) Short-lived AI agent code execution:** Use a **microVM-backed managed sandbox** where practical, or an AKS Kata sandbox after validating startup latency, resource overhead, networking, and cleanup. For small, well-defined tools that can run in WASI, Wasmtime/WasmEdge/Spin are a complementary least-capability option, not a general substitute for a VM when agents can execute arbitrary native code.

**(c) Customer source builds:** Do **not** build untrusted source in a `privileged` namespace on shared nodes. Move builds to disposable isolated workers, preferably microVM/VM-backed, and remove privileged mode, host mounts, host namespaces, and access to cluster credentials. If a build truly requires elevated privileges, isolate it on dedicated disposable infrastructure with no sensitive tenant workloads on the same node.

## Hardening still required

Apply these controls regardless of runtime:

- Keep the **restricted Pod Security Standard** and enforce the sandbox RuntimeClass through admission policy; do not rely on developers to opt in.
- Drop all Linux capabilities, disallow privileged containers and privilege escalation, and use a restrictive seccomp profile. Apply AppArmor where supported by the selected node/runtime.
- Run as a non-root UID; use a read-only root filesystem where the workload permits it; mount only narrowly scoped writable temporary storage.
- Set `automountServiceAccountToken: false` unless the workload needs Kubernetes API access; grant no broad cloud identity or secrets by default.
- Forbid host networking, host PID/IPC, host paths, device access, and unsafe mounts. Enforce CPU, memory, process, ephemeral-storage, and execution-time limits.
- Apply default-deny ingress and egress policies, then explicitly allow required destinations. Test Cilium policy behavior with the sandbox runtime rather than assuming ordinary-pod behavior carries over.
- Keep nodes patched, isolate sandbox workloads from trusted services, avoid sharing sensitive data paths, and treat cluster-admin and storage access as outside the pod-VM boundary.

**Bottom line:** AKS Kata-style pod sandboxing is the closest fit to the stated AKS environment, but the supplied evidence does not verify GA status or compatibility with `Standard_D8as_v7`, AMD nested virtualization, or the specified Cilium/CSI/GPU/service-mesh features. Do not move production tenants to it until those exact points are confirmed and tested. Ordinary restricted containers alone are not an adequate boundary for hostile code.

Citations:

1. https://learn.microsoft.com/en-us/azure/architecture/guide/multitenant/service/aks
2. https://azure-samples.github.io/aks-labs/docs/security/pod-sandboxing-on-aks/
3. https://docs.azure.cn/en-us/aks/considerations-pod-sandboxing
4. https://learn.microsoft.com/en-au/azure/aks/use-pod-sandboxing
5. https://docs.azure.cn/en-us/aks/confidential-containers-overview
6. https://kata-containers.github.io/kata-containers/design/virtualization/
7. https://www.h2security.io/secure/gvisor-untrusted-workloads-kubernetes/
8. https://docs.azure.cn/en-us/aks/use-pod-sandboxing
9. https://katacontainers.org/software/
10. https://docs.azure.cn/en-us/aks/concepts-pod-sandboxing
