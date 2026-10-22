# Deep search: in-cluster builds of tenant source (orama-operator#712)

Issue: [orama-operator#712](https://github.com/oramasearch/orama-operator/issues/712), part of the
multitenancy security review [lake#873](https://github.com/oramasearch/lake/issues/873) and the
cloud-service security epic [lake#725](https://github.com/oramasearch/lake/issues/725).
Date: 2026-10-21.

The question: what can a tenant's Dockerfile, run by Lake's in-cluster build, reach? The node,
other builds, the BuildKit cache, the network, registry credentials, other tenants' source,
images and logs.

## Verdict

| # | Check | Result | Issue |
|---|---|---|---|
| 1 | Build namespace and its Pod Security level | **Fail**, known: `privileged` enforced | [#759](https://github.com/oramasearch/orama-operator/issues/759) |
| 2 | BuildKit privilege (rootless or not) | Pass with a caveat: rootless, uid 1000, but no process sandbox, and seccomp and AppArmor unconfined | #758, #759 |
| 3 | What a build step can reach on the node | **Fail**: shared nodes, no runtime isolation, full kernel syscall surface | [#759](https://github.com/oramasearch/orama-operator/issues/759) |
| 4 | What a build step can reach on the network | **Fail, fixed in this PR**: no Cilium metadata deny on build namespaces, so the instance metadata service and Azure's WireServer were not reliably blocked | [#761](https://github.com/oramasearch/orama-operator/issues/761) |
| 5 | Build cache shared between tenants (cache poisoning) | Pass | none |
| 6 | Registry credentials visible to a build | **Fail**: every build gets Orama's own Docker Hub credential | [#758](https://github.com/oramasearch/orama-operator/issues/758) (p1) |
| 7 | How the built image is pushed and pinned | Pass across tenants. Within a workspace, the push token is too broad | [lake#909](https://github.com/oramasearch/lake/issues/909) (p3) |
| 8 | One tenant's build reading another's source, cache, logs or tokens | Pass | none |
| 9 | Disposable, VM-isolated build workers | Not in place. A `kata-vm-isolation` RuntimeClass exists on the managing cluster, but no node can run it | [#759](https://github.com/oramasearch/orama-operator/issues/759) |

Line numbers are for this PR's head. On `dev` before it, the `privileged` label is at
`lake_builds.go:208`. Lake is cited at `oramasearch/lake` `dev` `3b436919`, and lake-builder at
`main` `31a48d9`.

## How it was checked

- **Code read**: the operator's `internal/controller/lake_builds.go` (whole file), lake's
  `src/api/build_capabilities.cpp`, `src/api/registry.cpp` (`authorize_build_token`),
  `src/api/http_server.cpp` (`registry_access`, `tenant_image`), `src/control/builds.cpp`, and
  lake-builder's `Dockerfile`, `src/lake_builder/{build,registry,__main__}.py`. BuildKit's
  `docs/rootless.md` covers `--oci-worker-no-process-sandbox`.
- **Live reads, managing cluster `orama-production-cluster-01`, 2026-10-21**, all `get` only:
  - the build namespace `lake-prod-builds-7cf58eef22f1`: its labels, annotations and
    NetworkPolicies, and whether it has any Jobs or Pods (none at the time);
  - the CiliumNetworkPolicies in every namespace;
  - the kube-system DaemonSets, the RuntimeClasses and the node labels;
  - `Lake/prod` and the two `LakeSet` templates.
- **Nonprod `orama-nonprod-cluster-01`: not read.** Reading it needs its kubeconfig Secret, and
  this session was not allowed to write that credential to disk. The script below reads it.
  Nonprod has the same operator and the same `pullSecretName` as the other Lakes (from the
  `lake-nonprod` LakeSet template), so the code findings apply to it unchanged.
- **Tests written**: `internal/controller/lake_builds_isolation_test.go` holds one regression
  test per isolation rule this review relied on (listed at the end). The envtest
  `TestLakeBuildEnvtestRowToJobToStatus` now also requires the Cilium deny.

## Findings per check

### 1. Build namespace and Pod Security: fail (known)

- One namespace per (Lake, space): `lake-<lake>-builds-<sha256(space)[:12]>`
  (`buildNamespace`, `lake_builds.go:174`).
- The space is the customer's name, set by lake and never by the caller.
  `create_build` builds the target as `<registry>/<customer->name>/<name:tag>`
  (`build_capabilities.cpp:223`), and `valid_target` refuses `/` in the caller's part
  (`build_capabilities.cpp:82-85`). The operator accepts only a three-part target
  (`parseBuildTarget`, `lake_builds.go:160`).
- If the namespace already exists with another Lake's or another space's annotation, the operator
  refuses it rather than adopting it (`lake_builds.go:252`).
- Pod Security: `enforce: privileged`, with `audit` and `warn` set to `restricted`
  (`buildNamespaceLabels`, `lake_builds.go:203-215`). Read live: the labels match the code.
- Why `privileged`: `baseline` rejects the `Unconfined` seccomp and AppArmor profiles that rootless
  BuildKit needs (`docs/lake/builds.md`). The SETUID/SETGID capabilities alone would pass
  `baseline`.
- No tenant can create pods in the namespace (no tenant has Kubernetes API access, and the builder
  has no service-account token). So `privileged` only removes admission's second check on the
  operator's own pod spec. The new `TestBuilderPodHasNoHostAccessOrExtraPrivilege` now does that
  check in code.

### 2. BuildKit privilege: rootless, but no process sandbox

- The image is `moby/buildkit:v0.33.0-rootless`, plus Python and lake-builder, running as
  `1000:1000` (lake-builder `Dockerfile`).
- The pod runs as uid/gid 1000 with `runAsNonRoot` and drops every capability except `SETUID` and
  `SETGID`. It allows privilege escalation for `newuidmap`/`newgidmap`, and sets seccomp
  `Unconfined` and AppArmor `Unconfined` (`buildPod`, `lake_builds.go:498-548`).
- It has no service-account token (`:506`), no host namespaces and no host mounts. Its only
  volumes are size-limited `emptyDir`s plus the base-image pull Secret (`:543-566`).
- `BUILDKITD_FLAGS=--oci-worker-no-process-sandbox` (`:490`). BuildKit's `docs/rootless.md` says
  that flag lets a build step (`ExecOp`) `kill`, and depending on seccomp `ptrace`, any process in
  the daemon's container. Seccomp is unconfined here.
- So a `RUN` step can read the builder's own processes: `/proc/<pid>/environ` of the Python
  entrypoint (`SOURCE_TOKEN`, `PUSH_TOKEN`, `EVENTS_TOKEN`) and `/proc/<pid>/root/...` (the
  merged `DOCKER_CONFIG`).
- The build's own three tokens are its tenant's, scoped to one build (see 7 and 8). Leaking them
  stays inside the tenant, apart from lake#909.
- Orama's Docker Hub credential is also in that container. That is check 6.

### 3. What a build step reaches on the node: fail

- `buildPod` sets no `nodeSelector`, toleration, affinity or `runtimeClassName`.
- Read live on the managing cluster: two nodes in one pool (`agentpool`), no taints, Ubuntu with
  kernel `6.8.0-1059-azure`. Builds share those nodes with `orama-operator`, `Lake/prod`, its
  Postgres and the Cloud v1 sandbox.
- With seccomp unconfined, tenant code can create user namespaces and reach the whole syscall
  surface, which is the usual path to a local root exploit.
- Root on the node gives every Secret mounted on it. On this cluster that includes the operator's
  token, which reads every leaf's kubeconfig.
- Filed as [#759](https://github.com/oramasearch/orama-operator/issues/759) (p2): dedicated tainted
  build nodes, one build per node, then Kata VM isolation. RuntimeClass `kata-vm-isolation` exists,
  but its `nodeSelector` `kubernetes.azure.com/kata-vm-isolation: true` matches no node.

### 4. What a build step reaches on the network: fail, fixed here

Policies (`reconcileBuildNetworkPolicies`, `lake_builds.go:279`; `buildEgressRules`, `:375`). Read
live, they match the code:

- `build-default-deny` selects every pod and denies both ingress and egress. No pod can connect to
  a builder, including another build in the same namespace.
- `build-network` allows egress only to:
  - kube-dns on 53;
  - the Lake's own pods on its container port (4900);
  - TCP 80 and 443 to `0.0.0.0/0`, minus the private, loopback, link-local, documentation,
    multicast and reserved ranges.
- IPv6 has no allow rule, so it is denied.

What was wrong:

- **There was no Cilium metadata deny in build namespaces.** On 2026-10-14 the live #466 proof on
  this cluster found that AKS-managed Cilium did **not** enforce an `ipBlock` exclusion of
  `169.254.0.0/16`: a pod reached `http://169.254.169.254/`. Only an explicit CiliumNetworkPolicy
  `egressDeny` blocked it (`docs/infrastructure/network-sandbox.md`, *Mandatory cloud metadata
  deny*).
- Every `NetworkSandbox` namespace has that `deny-cloud-metadata` policy. Read live:
  `deny-cloud-metadata` exists in `lake`, `orama-system`, the three `prod-ws-*` workspaces and both
  zetta-tunnel namespaces, and **not** in `lake-prod-builds-7cf58eef22f1`.
- So a build step could reach the instance metadata service, which hands out the node's managed
  identity token, and Azure's WireServer `168.63.129.16`. The WireServer is a public address that
  the `ipBlock` did not exclude at all, and it serves node provisioning data on port 80.

Fixed in this PR ([#761](https://github.com/oramasearch/orama-operator/issues/761)):

- `reconcileBuildMetadataDeny` (`lake_builds.go:313`) gives every build namespace the same
  `deny-cloud-metadata` CiliumNetworkPolicy, before any Job exists:
  - `endpointSelector: {}`, because the namespace belongs to one Lake space;
  - `egressDeny` to `169.254.169.254/32`, `168.63.129.16/32` and `fd00:ec2::254/128`, on every
    port;
  - controlled by the Lake. A policy of that name controlled by anything else is refused, and the
    build waits.
- The address list is now one function, `cloudMetadataDenyCIDRs`, shared with the
  `NetworkSandbox` renderer.
- `168.63.129.16/32` is also added to the public rule's `ipBlock` exclusions
  (`buildPublicEgressExcept`, `:419`).
- It is the same on every cluster: there is no switch. The operator already needs Cilium and its
  RBAC for `ciliumnetworkpolicies`.

What is still open:

- Builds reach the public internet on 80/443. That includes the AKS API server's public endpoint,
  but builds have no credential for it. It also includes any host that receives exfiltrated data,
  which is why the credential in check 6 matters.
- Narrowing egress to package indexes needs name-based egress (zetta-tunnel's gateway,
  `docs/infrastructure/egress-rules.md`). That is outside this review.
- NetworkPolicy enforcement itself relies on Cilium. The managing cluster runs the `cilium`
  DaemonSet (2/2 ready). Requiring and checking it on every cluster is
  [#699](https://github.com/oramasearch/orama-operator/issues/699) (PR #717).

### 5. Build cache sharing: pass

- BuildKit's local state is a per-pod `emptyDir` (`buildkit`, 8Gi; `lake_builds.go:544`). It dies
  with the pod, so no two builds share a local cache.
- The registry cache is `<registry>/<space>/cache/<name>` (`buildCacheRef`, `:464`), inside the
  customer's own namespace in the registry.
- A push token may only touch repositories under `<customer>/` (`registry_access`,
  `http_server.cpp:10897` onwards).
- A route belongs to one workspace, and a token reaches only its own workspace's routes
  (`tenant_image`, `:11083`). A build may create a route only for its own target or
  `cache/<target>` (`:11052-11055`).
- So tenant A cannot write a layer into tenant B's cache, or read it.

### 6. Registry credentials visible to a build: fail (p1)

- Each build gets three random 32-byte tokens in its own Secret: source, push and events
  (`reconcileBuildTokens`, `lake_builds.go:580`). Lake stores only their hashes, accepts them only
  while the build is not terminal (`builds.cpp:9`), and the operator deletes the Secret on any
  terminal state (`cleanupBuildTokens`, `:943`).
- **Every build also receives the Lake's `spec.pullSecretName`**:
  - the operator copies it as `builder-pull` into the build namespace
    (`reconcileBuildPullSecret`, `:335`);
  - it mounts it into the container, and `BASE_REGISTRY_AUTH` points at it (`:493`, `:550-566`);
  - lake-builder merges its `auths` into the BuildKit `config.json`, next to the push token
    (`registry.py:65-70`).
- Read live: every Lake sets `pullSecretName: oramasearch-dockerhub`, both `Lake/prod` and both
  `LakeSet` templates. That is **Orama's Docker Hub credential for the private `oramasearch/*`
  images**.
- With no process sandbox (check 2) and public 443 open (check 4), any user who can start a build
  can take it.
- Filed as [#758](https://github.com/oramasearch/orama-operator/issues/758), p1. It has the attack
  path, the fix (base images from lake's registry, or a separate token scoped read-only to the
  runtime images; restore the process sandbox) and the rotation.
- Whether the token can push was not read, because it is a credential. If it can, this is a
  supply-chain compromise of every cluster.

### 7. How the image is pushed and pinned: pass across tenants

- BuildKit pushes `<registry>/<customer>/<name>:<tag>` over the in-cluster Service with the
  build's push token (lake-builder `build.py`).
- lake-builder reads the digest from BuildKit's metadata, re-fetches every manifest, and checks
  each one's sha256 before printing `{"digest","size"}` (`registry.py`, `image_size`).
- The operator takes the digest from the last non-empty log line and checks its format
  (`parseBuildResult`, `lake_builds.go:834`; `buildLogs` reads 20 lines, `:870`).
- With no process sandbox, a build step could write a different digest line itself. The only
  effect is on that tenant's own build record: apps and methodology runs pull only from their own
  `<customer>/<image>` repository in their own workspace (`registry_access`), so a forged digest
  cannot name another tenant's image. Recorded here, not filed.
- **Within one workspace**, a build's push token can overwrite any image already routed to that
  workspace, not only its own target. `own_target` is checked only when a route is first created
  (`http_server.cpp:11052-11055`). Filed as [lake#909](https://github.com/oramasearch/lake/issues/909),
  p3, with the fix and its test.

### 8. Another tenant's source, cache, logs or tokens: pass

- **Source**: the source token reads only `repository_tree`/`repository_file` of the build's own
  repository at its own revision (`authorize_build_token`, `registry.cpp:604-629`).
  `create_build` checks that the repository belongs to the customer (`build_capabilities.cpp:187`).
- **Logs and events**: the events token may only call `publish_build_event` for its own build and
  workspace (`registry.cpp:630-636`). Events go to topic `build-events.<build_id>` on the build's
  own workspace bus.
- Reading builds and their events needs the caller's permission on that customer, and the
  workspace must match (`build_capabilities.cpp`, `list_builds` and the event readers).
- The operator reads only the last 20 log lines of its own Job's pod, for the result.
- **Tokens**: each build's tokens are in its own Secret. Builds of one customer share a namespace,
  but no builder has an API token to read Secrets, and the only env vars from a Secret are the
  build's own (`TestBuilderPodHasNoHostAccessOrExtraPrivilege`).
- **Cross-workspace, same customer**: workspaces of one customer share the build namespace and its
  policies. The registry still keeps each image in its own workspace (`tenant_image`).

### 9. Disposable, VM-isolated workers

- Not in place. Each build is a fresh Job (`backoffLimit: 0`, one-hour deadline, TTL), but it runs
  on long-lived shared nodes.
- The research is `docs/research/secure-runtimes/`, and the plan is
  [#759](https://github.com/oramasearch/orama-operator/issues/759).
- What can be done now, without new nodes:
  1. the metadata deny (this PR);
  2. removing Orama's credential from builds (#758);
  3. narrowing the push token (lake#909).
- What needs node pools, and so Steve's consent and the helmfile/platform sync:
  1. a tainted build pool;
  2. then Kata.

## Overlap with orama-operator#711 (the app runtime)

- The Cilium metadata deny already applies to workspace namespaces, where apps run, through their
  `NetworkSandbox`. This PR adds it only to build namespaces.
- Node sharing (#759) affects app pods as well, because they land on the same nodes. The build
  part is filed here; #711 covers the app side.
- `docs/security/README.md` is created here. If #711's PR creates it too, the two index lines are
  merged.

## Read-only probe scripts (need Steve's consent to run)

**Nonprod reads** (read-only, but they need the leaf kubeconfig on disk):

```sh
umask 077
kubectl --context orama-production-cluster-01 -n orama-system get secret orama-nonprod-cluster-01-kubeconfig \
  -o jsonpath='{.data.kubeconfig}' | base64 -d > "$TMPDIR/nonprod.kubeconfig"
export KUBECONFIG="$TMPDIR/nonprod.kubeconfig"
kubectl get ns -l app.kubernetes.io/name=lake-builds --show-labels
for ns in $(kubectl get ns -l app.kubernetes.io/name=lake-builds -o name | cut -d/ -f2); do
  kubectl -n "$ns" get networkpolicy,ciliumnetworkpolicy
  kubectl -n "$ns" get jobs,pods -o wide
done
kubectl -n kube-system get ds cilium
kubectl get runtimeclass
kubectl get nodes -o 'custom-columns=NAME:.metadata.name,POOL:.metadata.labels.agentpool,TAINTS:.spec.taints[*].key,KERNEL:.status.nodeInfo.kernelVersion'
rm -f "$TMPDIR/nonprod.kubeconfig"
```

**Live probe of a running build's pod spec** (read-only; run while a build is active):

```sh
kubectl --context orama-production-cluster-01 get pods -A -l app.kubernetes.io/name=lake-builder \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.spec.securityContext} {.spec.containers[0].securityContext} {.spec.nodeName}{"\n"}{end}'
```

**Dataplane probe: a build that tests its own reach.** This writes to a cluster (it runs a build),
so it needs Steve's consent. Use a throwaway customer on `lake-dev`, never production. Submit it as
an inline Dockerfile with `build_image`, and read the result in the build log:

```dockerfile
FROM alpine:3.20
RUN apk add --no-cache curl >/dev/null; \
    for u in http://169.254.169.254/metadata/instance?api-version=2021-02-01 \
             http://168.63.129.16/machine?comp=goalstate \
             https://kubernetes.default.svc https://10.0.0.1; do \
      printf '%s ' "$u"; curl -s -m 5 -o /dev/null -w '%{http_code}\n' -H Metadata:true "$u" || echo blocked; \
    done; \
    ls /proc/*/root/etc/lake-builder/pull/ 2>&1 | head -3 || true; \
    echo done
```

Expected after this PR and #758: every URL `blocked` (curl exit, no HTTP code), and the `ls` lists
nothing. The `ls` only checks whether the file is visible. It never prints the credential.

## Regression tests (one per isolation rule relied on)

All in `internal/controller/lake_builds_isolation_test.go` unless noted.

| Rule | Test |
|---|---|
| The public rule never reaches the metadata service, Azure's WireServer or private ranges, and opens only 80/443 | `TestBuildPublicEgressNeverReachesPlatformOrPrivateAddresses` |
| In-cluster egress goes only to kube-dns and the build's own Lake | `TestBuildEgressReachesOnlyClusterDNSAndItsLake` |
| Every build namespace has the Cilium metadata deny, selecting every pod | `TestBuildNamespaceDeniesCloudMetadataWithCilium`; envtest `TestLakeBuildEnvtestRowToJobToStatus` |
| A foreign metadata deny is not adopted, and no build starts behind it | `TestBuildDoesNotStartWhenItsMetadataDenyIsForeign` |
| The builder has no host namespaces, no host mounts, no API token, no extra capabilities and no other Secret | `TestBuilderPodHasNoHostAccessOrExtraPrivilege` |
| One namespace per space, and the cache stays in the space | `TestBuildNamespaceAndCacheArePerSpace` |
| A namespace owned by another space is never adopted | `TestBuildNamespaceOwnedByAnotherSpaceIsNotAdopted` |
| Default-deny covers every pod in both directions, and nothing admits ingress | `TestBuildNamespaceDefaultDenyCoversEveryPod` |

Lake's side of the rules (the source token reads one repository at one revision, the push token
stays within one customer and one workspace, tokens die at a terminal phase) is lake's to test.
The missing write restriction is lake#909, which brings its test.
