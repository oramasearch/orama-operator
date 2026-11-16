# Mesh: zetta-tunnel's node agent

Stage 6 of the [secure-networking design](secure-networking.md), decision D7, for
[operator#474](https://github.com/oramasearch/orama-operator/issues/474): connections between
enrolled workloads carry identity and mutual TLS through
[zetta-tunnel](https://github.com/oramasearch/zetta-tunnel)'s mesh (`docs/mesh.md`). Cilium stays the
enforcer, as defence in depth.

```text
workload TCP -> 127.0.0.1:15001 (agent, in the pod's namespace) -> TLS 1.3 -> peer pod:15008 (agent) -> 127.0.0.1:<port>
```

## The node agent, on every cluster

The operator renders it, so it is on every node of every cluster and the same everywhere (both
installation paths install the operator chart; the image is `networking.zettaTunnelImage`):

| Object | What |
|---|---|
| Namespace `zetta-tunnel-mesh` | The agent's own system namespace, Pod Security `privileged`. Nothing else runs there. |
| DaemonSet `zetta-tunnel-mesh-<node key>`, one per node | Each pinned to its node by a required node affinity on `metadata.name` (tolerates every taint, `system-node-critical`), host network, no service-account token. `<node key>` is FNV-1a 64 of the node name in hex; the full name is in the `orama.cloud/mesh-node-name` annotation. A node joining gets its own; a node leaving takes its DaemonSet and Secret with it. |
| — container `agent` | `zetta-tunnel mesh --configuration /run/zetta-mesh/mesh.json --health-listen 127.0.0.1:15020 --audit -`, privileged, root: entering pod network namespaces and installing nftables capture needs it (zetta-tunnel `docs/mesh.md`). Mounts the host's `/proc` and the projection read-only. Probes on `127.0.0.1:15020`, private to the node. |
| — container `projector` | The operator's own image, `/mesh-projector`: root with only `CAP_SYS_PTRACE` (every other capability dropped, no privilege escalation, read-only root, the runtime's seccomp profile), which the kernel requires to open another process's `ns/net`. Reads this node's template from this node's Secret, the only one it mounts, finds each enrolled pod's network namespace in the host's `/proc` (the lowest PID whose cgroup names exactly that pod UID and no other pod, systemd or cgroupfs layout; a UID prefix is not membership), pins it as the agent checks it (opens the process directory once, reads `cgroup` through it, opens `ns/net` relative to it and reads its inode with `fstat`, reads `cgroup` again, and skips a process that changed), writes each workload's `pod_uid` and `network_namespace_inode` beside `network_namespace` (orama-operator#869; zetta-tunnel LSR-264 refuses a projection without them), and publishes the agent's projection and certificate files into the shared in-memory directory, files first and `mesh.json` last, each by atomic rename. It has no credential and makes no API call. |
| Secret `mesh-node-<node key>`, one per node | That node's template, the certificates and private keys of the workloads on that node only, and the workload CA certificate. Always present (possibly empty), mounted optional and only by that node's agent. |
| Secret `zetta-tunnel-mesh-pki` in the operator's namespace | The mesh's workload CA, minted once. Its key never leaves the operator's namespace. |

Workloads stay under restricted Pod Security with no `NET_ADMIN`; only the agent is privileged.

**A node receives only its own workloads' keys** (orama-operator#788, finding of the Lake
pre-launch security review). The kubelet fetches a Secret only for a pod bound to its node that
mounts it, and the node authorizer refuses it any other, so root on one node, by container escape,
reads the private keys of the workloads on that node and no other's. Until #788 one DaemonSet
mounted 16 shared Secrets (`mesh-templates-0` … `-15`) holding every node's keys, so one node
exposed them all; the operator deletes that DaemonSet and those Secrets on its first pass. The
agents of a node are restarted once by the change (the old DaemonSet goes before the new ones start,
as both bind the same host ports). Test: `TestEachMeshNodeReceivesOnlyItsOwnWorkloadKeys`, over the
rendered DaemonSets and the Secrets they mount.

## Enrolment, per namespace, through the NetworkSandbox

`NetworkSandbox.spec.mesh.enrolled: true` enrols every pod the sandbox selects. For a Lake workspace
it comes from the `Lake` resource: `spec.workspaces.overrides.<workspace id>.mesh: true`, which the
workspace renderer copies into the workspace's sandbox. Nothing is enrolled by default.

The order zetta-tunnel requires, and how it is kept:

| Step | How |
|---|---|
| 1. Cilium / default-deny first; a declared Connection allows **only TCP 15008** between enrolled pods, not the application port | The NetworkSandbox renderer narrows every Connection allowance of an enrolled sandbox to 15008 (the sandbox spec keeps the application ports; only the rendered policies change). |
| 2–4. Project the pod, wait for the agent | The operator projects a sandbox's pods only after the sandbox's current spec has been rendered (`status.observedGeneration` and `Ready` at the current generation). The projector names a pod only once its process exists on the node. |
| 5. Release the application | Not built: a startup gate needs the operator to hold every enrolled pod's containers, including pods other operators create, which needs an admission webhook the operator does not have. Without it a new pod's first connections fail until capture is installed: closed, never open (below). |

Each enrolled pod gets its own identity, `<pod>.<namespace>.mesh.orama.internal` (zetta-tunnel refuses
two workloads of one identity on a node, and replicas share nodes), and a certificate for it from the
mesh CA (client and server, 90 days, reissued with fewer than 30 left). Its projection is derived from
the sandbox's declared Connections: a **route** to each pod the Connection's target selects, on the
Connection's port (named ports resolved on the target pod), and on the target, a **port** whose
`sources` are exactly the source pods' identities. Paths not declared by a Connection have no route
and no source: the agent refuses them. Pods not running, without an IPv4 address or on the host
network are not projected. At most 128 workloads per node and 256 routes per workload, zetta-tunnel's
limits; a pod beyond them is left out, which is closed.

Pods are read without a cluster-wide pod informer (the operator holds none): while any namespace is
enrolled, the operator re-reads its pods every three seconds. Templates reach nodes through the
kubelet's Secret refresh, usually within a minute.

## Fail-closed, case by case

| Case | Why it is closed |
|---|---|
| A pod not yet captured (enrolment race, new pod, restarted pod with a new network namespace) | Cilium allows only 15008 between enrolled pods; the application port is denied, and nothing listens on 15008 until the agent captures the pod. |
| The agent crashes, is killed, or loses its projection or certificates | zetta-tunnel leaves the capture rules in place and closes listeners and sessions; redirected traffic finds no listener. On restart it rebinds the same listeners. |
| The projector stops, or a template cannot be read | The last projection stays, and with it capture. |
| A node joins | The operator creates its DaemonSet and Secret; its template appears with its first enrolled pod. Until then nothing there is captured and Cilium still refuses the application ports. |
| Unenrolment | The operator stops projecting the namespace and stops narrowing its policies. zetta-tunnel never removes capture from a live namespace, so a pod enrolled before keeps refusing until it is replaced. |

## What blocks enrolling a real workspace today

These are properties of zetta-tunnel's mesh as specified, found while building this; enrolment works
mechanically but would cut these paths, so no namespace is enrolled by default:

1. **Kubelet probes.** zetta-tunnel redirects *all* inbound TCP except 15008 to 15008, so an `httpGet`
   or `tcpSocket` probe from the node to an enrolled pod is refused as plaintext, and the pod is killed
   by its liveness probe. Istio's ambient mode exempts probe traffic; zetta-tunnel needs the same.
2. **Peers that are not in the mesh.** Inbound from Lake, the operator, datastore operators (CNPG,
   Strimzi, …), the developer-tunnel relay and the ingress gateway, and outbound to Lake, DNS over TCP
   and the internet, are captured and have no route or source, so they are refused. zetta-tunnel needs
   a way to pass declared non-mesh peers (or the operator a way to declare them), before a namespace
   whose workloads talk to the platform can be enrolled.
3. **The startup gate** (step 5 above).

They are for zetta-tunnel's mesh ([zetta-tunnel#7](https://github.com/oramasearch/zetta-tunnel/issues/7)); the operator side needs no change
for the first two beyond declaring the peers once zetta-tunnel can take them.

## Verify

- `go test ./internal/meshprojection/`: the `/proc` resolver (systemd and cgroupfs layouts, lowest PID,
  exact pod membership, the pinned namespace's inode, a process without a namespace skipped),
  rendering zetta-tunnel's exact projection and leaving out pods not on the node, publishing files
  before the projection and removing stale ones, reading only a node's own template and refusing a
  template that names a path outside its shard.
- `go test ./internal/controller/ -run 'Mesh|EnrolledSandbox|LakeWorkspaceOverrideEnrols'`: nothing is
  projected before the sandbox's mesh-only policies are rendered; routes and sources only for declared
  paths; one identity per replica; certificates signed by the projected CA, stable across passes; the
  CA key never in a shard; a gone pod leaves the projection; unenrolment stops it and the polling; the
  agent is privileged in its own privileged namespace and the projector is not (root with only
  `SYS_PTRACE`); declared Connections
  in an enrolled sandbox render only 15008 while other allowances are unchanged; the Lake override
  enrols and unenrols the workspace sandbox. `TestMeshEnvtestRendersMeshOnlyPoliciesAndAValidAgent`
  checks the rendered policies and the DaemonSet against a real API server.

Live acceptance (fail closed under node churn, pod restarts, enrolment races and an agent crash, with
escaped traffic refused) is a cluster test on `example-nonprod` with a test namespace whose
workloads have no probes and talk only to each other, until the three blockers above are resolved.
