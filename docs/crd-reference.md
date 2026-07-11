# `OramaCloudV1` reference

One umbrella CR per cluster describes an entire regional v1 deployment. Source of
truth: [`../api/v1alpha1/oramacloudv1_types.go`](../api/v1alpha1/oramacloudv1_types.go)
(regenerate this doc's tables when the types change).

## Top-level `spec`

| Field | Type | Default | Purpose |
|---|---|---|---|
| `region.name` | string | — | Region label (`us-east`, `eu`, `ap`). |
| `region.role` | enum | `primary` | `primary` (write path + stateful stores) or `read` (regional read tier). |
| `profile` | enum | `light` | `light` (dev/no-repl) · `medium` (single-tenant prod) · `heavy` (multi-tenant). Presets replication + sizing. |
| `idp.provider` | enum | `none` | `none \| keycloak \| firebase \| clerk`. |
| `images.registry/tag/overrides` | — | `oramasearch` / `latest` | Image resolution (overrides win). **`tag` applies to the operator's OWN built images only.** Reused published images resolve independently (off `:latest`, not the sandbox tag): `oramacore` → `oramasearch/oramacore:latest`; `embeddings` (the engine ai-server :50051) → `oramasearch/oramacore-ai-server:latest`, or `-cuda` when `writer.gpu`. Override either via `overrides.oramacore` / `overrides.embeddings`. |
| `images.pullSecretName` | string | — | docker-registry Secret (CR namespace) for **private** images — e.g. the private Docker Hub repos built by the bridge's [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images) tooling. Attached to every pod. |
| `llm.providers[]` | list | groq+openai+anthropic | **Multiple** LLM providers (`name`, `models[]`, `apiKeySecretRef`, `gpu`) — end users pick per query. Empty ⇒ the prod set. |
| `llm.default` | enum | `groq` | Provider used when a request doesn't name one. |
| `cloudflare.*` | object | enabled | Operator manages CF **DNS + Load Balancing + R2/KV** via the CF API. **Two Secrets:** `apiTokenSecretRef` → `token`+`zoneID`+`accountID` (control plane); `r2SecretRef` → `s3ApiEndpoint`+`accessKeyId`+`secretAccessKey` (R2 S3 data plane). |
| `peers[]` | list | — | Other regions (read tiers → primary `writerEndpoint`/`postgresEndpoint`); no mesh. |
| `demoSeed` | bool | false | Seed a demo project+index so a fresh install answers queries. |

## `spec.components`

Every component has `enabled`. Data stores also have `mode: in-cluster|external`,
`placement: local|central`, `replicate`, and `endpoint`/`credentialsSecretRef`.

| Component | Extra fields | Maps to (prod) |
|---|---|---|
| `engine` | `writer.gpu`, `writer.{resources,nodeSelector,tolerations}`, `reader.{replicas,hpa,resources}`, `envoy` | OramaCore writer + readers + Envoy |
| `dashboard` | `host` | app.orama.com (Next.js) |
| `docs` | `host` | docs.orama.com (Next.js + Fumadocs) |
| `ingestion` | `harvest` | index-builder (Deployment) + HARVEST |
| `controlDB` | data-store | PlanetScale → CloudNativePG |
| `analytics` | data-store | ClickHouse |
| `cache` | data-store | Upstash Redis |
| `broker` | data-store (`placement: central`) | RabbitMQ |
| `search` | data-store | Typesense hosted-search |
| `disco` | `enabled` | DISCO semantic cache |
| `workers` | `image`, `replicas` | the CF workers, bundled into one image |

## `OramaCloudV1E2ETest` (separate CRD)

Runs the **comprehensive functional e2e suite** as a Job against a deployed stack — a test is a
first-class, repeatable resource (the operator never runs suites during normal reconcile). The suite
(`internal/controller/assets/e2e-functional.sh`, embedded via `go:embed` and shipped in a ConfigMap)
is a curl+jq script that self-bootstraps a clean-slate collection+index and asserts **every critical
OramaCore function in variations** — liveness, auth (keys + negatives), collection/index lifecycle +
document CRUD/upsert/delete, the write→searchable replication canary, fulltext (typo tolerance/exact/
threshold/boost/pagination), vector + hybrid, filters (number/date/bool/string/geo + and/or/not),
facets, sort, groupBy, batch-get, KV, hooks, merchandising, MCP, analytics, and answer/RAG. It is the
forcing function (see the production-migration principle in `CLAUDE.md`) — never softened to pass.
Coverage is tracked by [operator#44](https://github.com/oramasearch/orama-cloud-v1-operator/issues/44).
Sample: [`../config/samples/e2e-test.yaml`](../config/samples/e2e-test.yaml).

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the `OramaCloudV1` to test (same namespace); the Job points at `<targetRef>-writer` / `-reader`. |
| `spec.image` | **Runner base image** (default `alpine:3.20`). Must provide `sh` + a package manager — the Job installs curl+jq into it, then runs the embedded suite. Override only with another curl/jq-capable base. |
| `spec.pullSecretName` | Pull secret authenticating image pulls (also avoids Docker Hub anon rate limits). |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed` (from the Job). `kubectl get ocv1e2e`. |

## `OramaZettaCluster` (separate CRD) — step 1 of One Orama

Deploys a **headless Orama Zetta cluster** (a StatefulSet of equal-peer CRDT nodes) from one CR — the
first step of the [One Orama](https://github.com/oramasearch/orama-bridge/tree/main/docs/projects/one-orama)
convergence ([orama-bridge#130](https://github.com/oramasearch/orama-bridge/issues/130)). It creates a
**headless Service** (peer DNS on 7777), a **client Service** (the HTTP API on 4800 — MCP + REST +
serving), and the **StatefulSet** (each node a per-node PVC at `/data`; `node-0` bootstraps, peers join
it). With `spec.host` set it also fronts the host with an **nginx Ingress** and — when `spec.dnsSecretRef`
is given — an **owned `CloudflareDNSRecord`** (a proxied A/CNAME record → the ingress LoadBalancer),
reusing the same Cloudflare child-controller path as the dashboard DNS. The Ingress is **dual-path**: the
bare root (`/`, nginx `location = /`) serves a static **`zetta` landing placeholder** from a tiny
operator-owned nginx pod (ConfigMap + Deployment + Service), while every other path proxies to the Zetta
HTTP API/UI (`:4800`). The placeholder is **decoupled from Zetta's own HTTP server** (currently
unreliable — [zetta#700](https://github.com/oramasearch/zetta/issues/700) /
[zetta#695](https://github.com/oramasearch/zetta/issues/695)) so the host always resolves to something;
swap it for the real Zetta web UI when [zetta#647](https://github.com/oramasearch/zetta/issues/647) ships.
This stage only **runs** Zetta headless — no operator↔Zetta symbiosis and **no** changes to Zetta's MCP
service. Sample: [`../config/samples/oramazettacluster.yaml`](../config/samples/oramazettacluster.yaml).

| Field | Purpose |
|---|---|
| `spec.replicas` | Number of equal-peer headless zetta nodes (default 1). |
| `spec.image` | Zetta-headless image (default `oramasearch/zetta:headless-v1-sandbox`), built by [`zetta-bridge/build/images/zetta-headless`](https://github.com/oramasearch/zetta-bridge/tree/main/build/images/zetta-headless). |
| `spec.pullSecretName` | Pull secret for the private zetta image. |
| `spec.storage.size` / `.storageClassName` | Per-node PVC at `--data-dir` (`/data`) — the CRDT doc, `node_id`, certs, JSONL/large-object data. |
| `spec.resources` | Per-node mem/cpu caps (right-sizing). |
| _(cluster join token — automatic)_ | Inter-node peer traffic is **always mTLS**. The operator mints one shared cluster join token per cluster into Secret **`<name>-zetta-cluster`** (`token` = 64-hex, minted once, never rotated) and injects it into **every** node as `ZETTA_CLUSTER_TOKEN`; Zetta derives the cluster CA deterministically from it, so all nodes trust each other and form **one** cluster ([operator#92](https://github.com/oramasearch/orama-cloud-v1-operator/issues/92)). There is no plaintext-peer knob — an unencrypted mode was removed as a security hole. See [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md). |
| `spec.clusterName` | The **GLOBAL** Zetta cluster name. CRs (in the same or other k8s clusters) sharing it are **siblings** — equal peers of one multi-leader cluster ([operator#71](https://github.com/oramasearch/orama-cloud-v1-operator/issues/71)); this CR is a **slice**. Empty = a standalone cluster named after the CR. |
| `spec.externalPeers[]` | Route table ([operator#70](https://github.com/oramasearch/orama-cloud-v1-operator/issues/70)) — `host:port` addresses of **wan-accessible** siblings, each appended as `--peer` so this slice's nodes dial OUT and join the shared cluster. Copy a sibling's `.status.wanAddress` here. Zetta is the mesh (gossip relays transitively), so a connected dial-graph is enough. |
| `spec.reachability` | **Default `dial-out-only`.** The node-reachability type ([zetta#739](https://github.com/oramasearch/zetta/issues/739)): `dial-out-only` (behind NAT / a private VNet — dials OUT only, **never gets an inbound LoadBalancer**) or `wan-accessible` (a routable rendezvous others dial out to — the operator advertises its address as `.status.wanAddress`, which siblings put in `externalPeers` and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73)) consumes). |
| `spec.ephemeral` | **Default `false`.** Marks the slice as burst/autoscaled capacity expected to churn ([operator#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69)). **Always forced to `dial-out-only`** (an ephemeral node is never advertised as a rendezvous — its address would flap); its membership blips aren't treated as a cluster fault. |
| `spec.wanAddress` | For a `wan-accessible` slice, the routable address (`host` or `host:port`; `:7777` assumed) siblings use — a mesh-VPN address, a stable public IP, or a DNS name. **Authoritative** when set (advertised verbatim as `.status.wanAddress`); when empty the operator derives it from `spec.hostNetwork` (node host IP) or `spec.exposePeerLoadBalancer` (cloud LB IP). Ignored unless `wan-accessible`. |
| `spec.hostNetwork` | **Default `false`.** Run node pods on the host network so `:7777`/`:4800` are reachable at the k8s node's own address — the simplest routable address for a **wan-accessible** slice (esp. single-node). Packs ordinals onto per-ordinal ports when `replicas>1`. |
| `spec.exposePeerLoadBalancer` | **Default `false`.** A cloud `LoadBalancer` fronting the peer port `:7777`. **Valid ONLY for a `wan-accessible` slice** — on a `dial-out-only`/ephemeral slice it is **ignored** (a warning is logged, any existing LB torn down) because a private node must dial OUT, not accept inbound (the [zetta#739](https://github.com/oramasearch/zetta/issues/739) anti-pattern). Even for wan-accessible it's only one option (a host with a public IP or a `wanAddress` needs none). Peer traffic is **mTLS** on the wire (the shared cluster join token derives the CA, [operator#92](https://github.com/oramasearch/orama-cloud-v1-operator/issues/92)), so keep the `<name>-zetta-cluster` Secret protected. |
| `spec.provisionOperatorSchema` | **Default `true`.** Makes this cluster the operator's own datastore (One Orama symbiosis v0): once the node is up, an in-cluster Job applies the operator's schema via Zetta's `/v1/sql` — the **`orama_control`** database (`components`, `reconcile_events`) and the **`orama_data`** database (`documents`, the app-data landing zone). Idempotent (tolerates "already exists"; Zetta has no `IF NOT EXISTS`). Edit `operatorZettaSchema` + delete the `<name>-zetta-schema` Job to re-apply. Set `false` to opt out. |
| _(readiness probe — disabled)_ | The pod's readiness probe is **off** ([operator#68](https://github.com/oramasearch/orama-cloud-v1-operator/issues/68)) because the node's flaky HTTP server ([zetta#731](https://github.com/oramasearch/zetta/issues/731)) made it flap out of the Service. Liveness + startup (peer port) remain. Restore `GET /mcp.md` readiness once zetta#731 lands. |
| `spec.mcpTokenAuth` | **Default `false`.** When `true`, mints a 64-char bearer token into Secret `<name>-zetta-mcp` (key `token`), injects it as `--mcp-token` (never logged — entrypoint echoes `****`), and switches readiness to a TCP check. **Leave off:** Zetta's `--mcp-token` treats the token as an *access-key id* that must already be a provisioned principal, so a bare token authenticates but is then denied every capability (`unknown access key`) on both `/mcp` and `/v1/*` — the node authenticates but does nothing. Default (`false`) runs the functional unauthenticated-local session. A working authenticated mode needs an owner-key bootstrap in the node ([zetta#733](https://github.com/oramasearch/zetta/issues/733)). Connect instructions: `GET /mcp.md` (public). |
| `spec.requireAuth` | **Default `false`.** OPT-IN test profile: when `true`, mints a 64-hex admin token once into Secret `<name>-zetta-admin` (key `token`, preserved across reconciles — same mint-once shape as `-zetta-mcp`), injects it as env `ZETTA_ADMIN_TOKEN`, and runs every node with `--require-auth --admin-token <token>` (redacted to `****` in the pod log) so the **authz system is ENFORCED** (deny-by-default). This exists to drive the `ZettaClusterE2ETest` authz-enforcement assertions — the regression harness for the zetta auth work, which has known open issues ([zetta#759–#769](https://github.com/oramasearch/zetta/issues/780)). **Image requirement:** `--require-auth`/`--admin-token` are the auth work ([zetta#780](https://github.com/oramasearch/zetta/issues/780)); the default sandbox image **predates** them and **rejects unknown flags** (the node exits with a usage error — it does **not** silently no-op), so only enable this against an auth-capable build. Default (`false`) = today's unauthenticated-local behavior — no Secret, no flags. Sample: [`../config/samples/zetta-require-auth.yaml`](../config/samples/zetta-require-auth.yaml). |
| `spec.host` | Hostname to expose at (e.g. `zetta.oramadev.net`) via an nginx Ingress — root `/` serves the `zetta` landing placeholder, other paths proxy to the API/UI (:4800). Empty = no external exposure (the Ingress/DNS/landing are torn down if the field is later cleared). |
| `spec.dnsSecretRef.name` | With `spec.host`, create a **proxied Cloudflare DNS record** (host → ingress LB) using the named Secret (keys `token`/`zoneID`/`accountID`) — normally the same `cloudflare-api-token` Secret the `OramaCloudV1` uses. |
| `spec.capacity.karpenter.enabled` | **Default `false`. Opt-in elastic node capacity** ([operator#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69), first slice). When `true`, the operator owns a Karpenter **`NodePool`** and pins the Zetta pods onto it (a dedicated taint `orama.cloud/zetta:NoSchedule` + toleration and the `orama.cloud/zetta-pool` nodeSelector), so raising `spec.replicas` past current node capacity leaves pods **Pending → Karpenter provisions matching nodes just-in-time →** the new pods schedule and join the CRDT cluster. Scheduling-driven scale-up; the Zetta-signal-driven trigger (`orama_control.scale_requests`) is a later slice. |
| `spec.capacity.karpenter.nodeClassRef` | The provider **NodeClass** Karpenter launches nodes from — `{group, kind, name}` (the Karpenter v1 `nodeClassRef` shape). Defaults `group: karpenter.k8s.aws`, `kind: EC2NodeClass` (EKS); on AKS NAP set `group: karpenter.azure.com`, `kind: AKSNodeClass`. **`name` is REQUIRED when enabled** — the operator owns only the cloud-neutral NodePool; the NodeClass (AMI/subnet/IAM on AWS) is platform-owned. Missing name ⇒ `CapacityReady=False, Reason=Blocked`. |
| `spec.capacity.karpenter.capacityType` / `.arch` | `on-demand` (**default**, stateful-safe) \| `spot`; `amd64` (**default**) \| `arm64`. Spot only with enough replicas to tolerate an interruption (a reclaim evicts a peer). |
| `spec.capacity.karpenter.instanceTypes[]` | Optional pin via the portable `node.kubernetes.io/instance-type` label. Empty (recommended) ⇒ Karpenter chooses the cheapest fitting type from the NodeClass offerings. |
| `spec.capacity.karpenter.limits` | `{cpu, memory, nodes}` — the **cost/scale guardrail**; Karpenter stops provisioning once any cap is hit. Defaults `cpu: "16"`, `memory: 64Gi`, `nodes: 3`. |
| _(disruption / quorum-safety)_ | The generated NodePool is **stateful-safe**: `consolidationPolicy: WhenEmpty` (never evict a live peer for underutilization), a `nodes: "1"` disruption budget (cap churn), `expireAfter: Never`, `terminationGracePeriod: 1h`; the Zetta pods carry `karpenter.sh/do-not-disrupt` and a `maxUnavailable: 1` PodDisruptionBudget. Operator-driven drain-safe **scale-down** is a later slice. |
| _(Karpenter absent)_ | If the `karpenter.sh/v1 NodePool` CRD isn't installed (bare metal / no autoscaler), enabling capacity is a **graceful no-op**: `CapacityReady=False, Reason=KarpenterNotInstalled`, no crash-loop. `spec.replicas` still packs more pods onto the fixed host until full. Or set `install: true` (below) to have the operator install Karpenter itself. |
| `spec.capacity.karpenter.install` | **Default `false`. Opt-in — make the operator OWN the Karpenter install** ([operator#75](https://github.com/oramasearch/orama-cloud-v1-operator/issues/75)). When `true`, the operator server-side-applies the pinned upstream Karpenter controller (Deployment + ServiceAccount + RBAC) and the Karpenter CRDs (`NodePool`/`NodeClaim`/`EC2NodeClass`) as owned resources (modelled as `unstructured` — **no Karpenter Go types vendored**; version pinned to **v1.13.0** under `internal/controller/assets/karpenter/`). **Cloud-gated:** a clean no-op on bare-metal/k0s (`Reason=BareMetalNoCloud`) and on AKS, where **Node Auto Provisioning already manages Karpenter** (`Reason=ManagedByAKSNAP` — use `enabled`, not `install`); the self-install path targets **EKS**. Surfaced via the `KarpenterInstalled` condition. |
| `spec.capacity.karpenter.controllerRoleARN` | **The cloud IAM identity the operator CANNOT mint** — a cloud-account prerequisite, same class as the image pull secret. The AWS IAM role ARN Karpenter's **controller** assumes (via IRSA — annotated onto its ServiceAccount) to create/terminate EC2 instances. **Required when `install` on EKS** (unless wired out-of-band, e.g. EKS Pod Identity); empty ⇒ `KarpenterInstalled=False, Reason=CloudIdentityMissing` and **the controller is not deployed** (no crash-loop). This is the *controller* identity; the launched *nodes* get their own instance profile via the platform-owned `nodeClassRef`. See [`docs/karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md). |
| `spec.capacity.karpenter.installClusterName` | The AWS EKS cluster name Karpenter's controller uses (`settings.clusterName`) to discover the cluster's tagged subnets/security-groups/interruption queue. **Required when `install` on EKS**; empty ⇒ `KarpenterInstalled=False, Reason=InstallClusterNameRequired`. |
| `KarpenterInstalled` condition | Present only when `install` is set. `BareMetalNoCloud` / `ManagedByAKSNAP` / `UnsupportedCloud` / `CloudIdentityMissing` / `InstallClusterNameRequired` (gated or blocked) → `InstallProgressing` (applied, controller not yet Available) → `Installed` (controller available in `kube-system`). Written **only on change** (anti-flap). |
| `status.phase` / `.readyNodes` / `.totalNodes` | Readiness roll-up. `kubectl get ozc`. |
| `status.provisionedNodes` / `CapacityReady` condition | Number of nodes Karpenter has provisioned for this cluster (`karpenter.sh/nodepool=<pool>`), plus a `CapacityReady` condition (`Disabled`-dropped / `Blocked` / `KarpenterNotInstalled` / `NodePoolProgressing` / `NodePoolReady`). Written **only on change** (anti-flap). |
| `status.reachability` / `.ephemeral` | The **effective** reachability type of the slice (a print column) + whether it's ephemeral — echoed from spec (`reachability` forced to `dial-out-only` when `ephemeral`). |
| `status.wanAddress` | The routable rendezvous address (`host:port`) siblings dial OUT to when this slice is **wan-accessible** — copy it into a sibling's `spec.externalPeers`, and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73)) is populated from the wan-accessible set (its HTTP origin is the same host at `:4800`). Empty for a `dial-out-only` slice or until the address is known. Shown by `kubectl get ozc -o wide`. |

**Live (`oramadev.net` sandbox):** `GET https://zetta.oramadev.net/` returns `zetta` (the landing pod),
and `GET https://zetta.oramadev.net/v1/server_info` returns the node's server info from the Zetta API —
both through Cloudflare (proxied), the same reachability path as `app.oramadev.net` → the dashboard.

## `ZettaClusterE2ETest` (separate CRD)

Runs the **live-cluster zetta e2e suite** as a Job against a deployed `OramaZettaCluster` — a test is a
first-class, repeatable resource (the operator never runs suites during normal reconcile). **Unlike the
`OramaCloudV1E2ETest` suite** — which is embedded in the operator via `go:embed` — the Zetta suite lives
in the **zetta repo** (`tests/e2e/zetta-e2e.sh`) and is **baked into the `oramasearch/zetta-e2e` image**
(its `ENTRYPOINT` runs the script) so it evolves in lockstep with the engine; this CRD only **triggers**
it. The reconciler reads the target cluster's `spec.replicas` and wires the cluster addressing to the
suite via env (the exact contract the script reads — **do not rename**):

| Env var | Value |
|---|---|
| `ZETTA_E2E_BASE_URL` | `http://<targetRef>-zetta-0.<targetRef>-zetta-headless.<ns>.svc.cluster.local:4800` — the writer, pinned to node-0 via per-pod DNS. |
| `ZETTA_E2E_PEER_URLS` | Space-joined per-pod DNS of ordinals `1..replicas-1` (`<targetRef>-zetta-<i>.<targetRef>-zetta-headless.<ns>.svc.cluster.local:4800`); empty for a single node. |
| `ZETTA_E2E_LEVELS` | `spec.levels` (default `2,3`). The suite runs L2 (`zetta::testkit`) + L3 (black-box integration over the deployed surfaces). |
| `ZETTA_E2E_REQUIRE_AUTH` | `1` only when the target has `spec.requireAuth` — turns on the suite's **authz-enforcement assertions** (LEVEL 3.12: an unauthenticated request must be refused; the full RLS/masking/scoped-grants/token-lifecycle matrix lands as the authz system does, [zetta#780](https://github.com/oramasearch/zetta/issues/780)). |
| `ZETTA_E2E_TOKEN` | When the target has `spec.requireAuth` — a `SecretKeyRef` to `<targetRef>-zetta-admin` key `token` (the **seeded admin** principal, so authenticated calls pass). Otherwise, when the target has `spec.mcpTokenAuth` — a `SecretKeyRef` to `<targetRef>-zetta-mcp` key `token` (best-effort: authenticates-but-denies today, [zetta#733](https://github.com/oramasearch/zetta/issues/733)). `requireAuth` takes precedence over `mcpTokenAuth` for the token source. |

If the target `OramaZettaCluster` doesn't exist yet, the test parks in `Pending`
(`waiting for target OramaZettaCluster <ref>`) and requeues — it never creates a Job without a target.
A change to Zetta coverage touches **three** repos: zetta (the script) → zetta-bridge (the image) →
this operator (the CRD). Sample: [`../config/samples/zetta-e2e-test.yaml`](../config/samples/zetta-e2e-test.yaml).

**Authz coverage (require-auth):** when the target has `spec.requireAuth`, this test runs the suite's
authz-enforcement assertions ([issue #88](https://github.com/oramasearch/orama-cloud-v1-operator/issues/88))
— it sets `ZETTA_E2E_REQUIRE_AUTH=1` and hands the suite the target's **seeded admin token**
(`<targetRef>-zetta-admin`). **Multi-node** authz coverage needs no extra field: the reconciler already
builds `ZETTA_E2E_PEER_URLS` from the target's replicas, so pointing this at a `replicas>=2`,
`requireAuth: true` cluster exercises the authenticated path across every node. End-to-end sample (a
3-replica require-auth target + its test): [`../config/samples/zetta-require-auth.yaml`](../config/samples/zetta-require-auth.yaml).

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the `OramaZettaCluster` to test (same namespace). Required. |
| `spec.image` | **Runner image** (default `oramasearch/zetta-e2e:v1-sandbox`) that bakes the suite as its `ENTRYPOINT` — the operator sets only `Env`, never a `Command`. |
| `spec.levels` | Which test levels to run (`ZETTA_E2E_LEVELS`, default `2,3`). |
| `spec.pullSecretName` | Pull secret authenticating image pulls (also avoids Docker Hub anon rate limits). |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed` (from the Job). `kubectl get ozce2e`; `kubectl logs job/<name>-e2e`. |
## `CloudflareLoadBalancer` (separate CRD) — the global geo-LB

Owns a Cloudflare **global Load Balancer** ([operator#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73)):
a single hostname served across the cluster's wan-accessible nodes with origin pools, health checks,
geo/proximity steering, and failover. Same "external resource as an owned CRD" pattern as
`CloudflareDNSRecord` — the child controller (`cloudflareloadbalancer_controller.go`) makes the CF
Load Balancing API calls and holds a finalizer, so the main loop never mutates Cloudflare directly.
From one CR it reconciles an account-scoped **Monitor**, one or more account-scoped **Pools**, and the
zone-scoped **Load Balancer**; teardown deletes them in dependency order (LB → pools → monitor). All
credentials come from the referenced Secret (`token`/`zoneID`/`accountID`).

**Cloudflare Load Balancing is a PAID add-on** — enable it on the account first
(dashboard: Delivery & performance → Load Balancing). Until then the reconciler is a safe no-op that
reports `Ready=False (LoadBalancingNotEnabled)` rather than crash-looping. Populating pool origins from
the **live** wan-accessible node set is tracked separately
([operator#74](https://github.com/oramasearch/orama-cloud-v1-operator/issues/74) /
[zetta#739](https://github.com/oramasearch/zetta/issues/739)).
Sample: [`../config/samples/cloudflareloadbalancer.yaml`](../config/samples/cloudflareloadbalancer.yaml).

| Field | Purpose |
|---|---|
| `spec.secretRef` | Secret with the Cloudflare credentials (keys `token`/`zoneID`/`accountID`) — normally the `cloudflare-api-token` Secret. |
| `spec.hostname` | The FQDN the load balancer answers on (e.g. `zetta.oramadev.net`). Cloudflare owns this DNS entry — do **not** also manage it with a `CloudflareDNSRecord`. |
| `spec.steeringPolicy` | `off` (failover order) `\| geo` (by region via `regionPools`) `\| dynamic` (by measured latency; sent as `dynamic_latency`) `\| proximity` (nearest origin) `\| random`. Default `geo`. |
| `spec.fallbackPool` | Pool name (`pools[].name`) used as the last resort when all default pools are unhealthy. Empty = the last declared pool. |
| `spec.regionPools` | Map of Cloudflare region code (e.g. `ENAM`, `WEUR`) → ordered list of pool names — for `geo` steering. |
| `spec.proxied` | Route the hostname through Cloudflare's proxy (orange-cloud). Default `true`. |
| `spec.sessionAffinity` | `none \| cookie \| ip_cookie \| header`. Default `none`. |
| `spec.monitor.*` | The health monitor: `type` (`http\|https\|tcp`, default `https`), `method` (default `GET`), `path` (default `/v1/server_info`), `port`, `expectedCodes` (default `200`), `expectedBody`, `interval` (60), `timeout` (5), `retries` (2), `followRedirects`. |
| `spec.pools[]` | Origin pools (≥1). Each: `name`, `origins[]`, `minimumOrigins` (1), `enabled` (true), `useMonitor` (true = attach the monitor). Become the LB's `default_pools` in declared order. |
| `spec.pools[].origins[]` | `{ name, address, enabled (true), weight ("1", a decimal 0..1) }`. `address` is a node's public endpoint; the monitor probes it. |
| `status.loadBalancerID` / `.monitorID` / `.pools[]` | The provisioned Cloudflare IDs + per-pool `id` and `health` (`healthy\|unhealthy\|unknown`). `kubectl get cflb`. |
| `status.conditions[Ready]` | `Reconciled` \| `WaitingForCredentials` \| `LoadBalancingNotEnabled` \| `ReconcileError`. Written only on a real transition (anti-flap). |
| `status.pass` / `.fail` / `.skip` | **Best-effort** assertion counts, parsed from the Job's pod logs (`Zetta e2e: pass=N fail=N skip=N`) once the Job is terminal, shown as `kubectl get ozce2e` print columns. `0` until the summary is readable (the operator reads pod logs via a clientset — RBAC `pods/log get`); a crash before the summary line leaves them at their last value. |

## `RabbitMQTopology` (separate CRD) — the broker's declared topology

Models the RabbitMQ broker's **vhosts / users / permissions / streams** as an owned, reconciled CRD
([operator#40](https://github.com/oramasearch/orama-cloud-v1-operator/issues/40) /
[#41](https://github.com/oramasearch/orama-cloud-v1-operator/issues/41)) — declarative topology
instead of OramaCore creating the replication stream as an app-boot side effect and every service
sharing the admin user. Same "external resource as an owned CRD" pattern as `CloudflareDNSRecord`: the
child controller (`rabbitmqtopology_controller.go`) drives the broker's **management HTTP API**
(`:15672`) and holds a finalizer, so the main loop never mutates the broker directly. This is the
operator's **own** topology CRD — the bitnami RabbitMQ Cluster / Messaging-Topology operators were
dropped (their images 404'd), so the broker runs as a plain StatefulSet and the operator owns its
topology (principle #3). See [`datastore-crds.md`](datastore-crds.md) for the full design.

`OramaCloudV1` creates + owns one `<cr>-broker` instance (in `reconcileBrokerTopology`) declaring the
`/` vhost, the least-privilege per-service users `oramacore-writer` / `oramacore-reader` (passwords
generated into `<cr>-broker-users`), their stream-scoped permissions, and the `oramacore-operations`
replication stream. **Ordered readiness gating:** until the broker (and its `<cr>-broker-admin`
Secret) is reachable the reconciler is a safe no-op reporting `Ready=False (BrokerUnavailable /
WaitingForCredentials)`, not an error. Sample:
[`../config/samples/rabbitmqtopology.yaml`](../config/samples/rabbitmqtopology.yaml).

| Field | Purpose |
|---|---|
| `spec.brokerService` | Broker Service DNS name whose management API is reconciled (e.g. `<cr>-broker`); reached at `http://<brokerService>.<ns>.svc.cluster.local:<managementPort>`. |
| `spec.managementPort` | RabbitMQ management HTTP API port. Default `15672`. |
| `spec.adminSecretRef` | Secret with the broker admin creds (keys `username`+`password`). Missing = safe no-op (waits). |
| `spec.vhosts[]` | Virtual hosts to ensure (idempotent). `/` always exists but may be listed for intent. |
| `spec.users[]` | Per-service accounts (least-privilege, principle #4): `{ name, passwordSecretRef {name,key}, tags }`. Password is sourced from a Secret — never in spec; service accounts stay untagged. |
| `spec.permissions[]` | `{ user, vhost (`/`), configure, write, read }` regexes. Scope to the exact stream (`^oramacore-operations$`) — a producer needs `write`, a consumer `read`, a declarer `configure`. Default regex `^$` (none). |
| `spec.streams[]` | Stream queues (x-queue-type=stream): `{ name, vhost, maxAge, maxLengthBytes, maxSegmentSizeBytes }`. **Retention is declaration-time only** (RabbitMQ cannot change it later via policy) and must MATCH any native stream-protocol client that also `Create`s the stream, or the client's Create conflicts — leave retention unset unless you know the client's exact create args. |
| `status.vhosts` / `.users` / `.permissions` / `.streams[]` | Applied counts + per-stream `present`. `kubectl get rmqtopo`. |
| `status.conditions[Ready]` | `Reconciled` \| `WaitingForCredentials` \| `BrokerUnavailable` \| `ReconcileError`. Written only on a real transition (anti-flap, principle #7). |
