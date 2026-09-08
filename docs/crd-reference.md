# `OramaCloudV1` reference

One umbrella CR per cluster describes an entire regional v1 deployment. Source of
truth: [`../api/v1alpha1/oramacloudv1_types.go`](../api/v1alpha1/oramacloudv1_types.go)
(regenerate this doc's tables when the types change). Samples:
[`../config/samples/primary-us-east.yaml`](../config/samples/primary-us-east.yaml) (a primary
region) and [`../config/samples/read-eu.yaml`](../config/samples/read-eu.yaml) (a read tier
pointing at the primary); the full sample list is in
[install.md](install.md#3-deploy-a-v1-stack-single-cluster-quickstart).

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

`engine.writer.gpu` requests `nvidia.com/gpu` for the writer's built-in embeddings (prod runs an
L40S). `workers` bundles all ported CF workers into one image → one Deployment.

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
Coverage is tracked by [operator#44](https://github.com/oramasearch/orama-operator/issues/44).
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
reusing the same Cloudflare child-controller path as the dashboard DNS. The Ingress proxies the **whole
host** to the Zetta node's own HTTP server on `:4800`: the bare root (`/`) is the node's **web dashboard**
([zetta#647](https://github.com/oramasearch/zetta/issues/647)) and every other path is MCP/REST/serving.
(An earlier operator version parked a static `zetta` placeholder at `/` while the node HTTP server was
unreliable; that shadowed the working dashboard and has been removed — the operator now also tears down
any leftover `<name>-zetta-landing` Deployment/Service/ConfigMap.)
This stage only **runs** Zetta headless — no operator↔Zetta symbiosis and **no** changes to Zetta's MCP
service. Sample: [`../config/samples/oramazettacluster.yaml`](../config/samples/oramazettacluster.yaml).

| Field | Purpose |
|---|---|
| `spec.replicas` | Number of equal-peer headless zetta nodes (default 1). |
| `spec.image` | Zetta-headless image (default `oramasearch/zetta:headless-v1-sandbox`), built by [`zetta-bridge/build/images/zetta-headless`](https://github.com/oramasearch/zetta-bridge/tree/main/build/images/zetta-headless). |
| `spec.pullSecretName` | Pull secret for the private zetta image. |
| `spec.storage.size` / `.storageClassName` | Per-node PVC at `--data-dir` (`/data`) — the CRDT doc, `node_id`, certs, JSONL/large-object data. |
| `spec.resources` | Per-node mem/cpu caps (right-sizing). |
| _(cluster join token — automatic)_ | Inter-node peer traffic is **always mTLS**. The operator mints one shared cluster join token per cluster into Secret **`<name>-zetta-cluster`** (`token` = 64-hex, minted once, never rotated) and injects it into **every** node as `ZETTA_CLUSTER_TOKEN`; Zetta derives the cluster CA deterministically from it, so all nodes trust each other and form **one** cluster ([operator#92](https://github.com/oramasearch/orama-operator/issues/92)). There is no plaintext-peer knob — an unencrypted mode was removed as a security hole. See [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md). |
| `spec.clusterName` | The **GLOBAL** Zetta cluster name. CRs (in the same or other k8s clusters) sharing it are **siblings** — equal peers of one multi-leader cluster ([operator#71](https://github.com/oramasearch/orama-operator/issues/71)); this CR is a **slice**. Empty = a standalone cluster named after the CR. |
| `spec.externalPeers[]` | Route table ([operator#70](https://github.com/oramasearch/orama-operator/issues/70)) — `host:port` addresses of **wan-accessible** siblings, each appended as `--peer` so this slice's nodes dial OUT and join the shared cluster. Copy a sibling's `.status.wanAddress` here. Zetta is the mesh (gossip relays transitively), so a connected dial-graph is enough. |
| `spec.reachability` | **Default `dial-out-only`.** The node-reachability type ([zetta#739](https://github.com/oramasearch/zetta/issues/739)): `dial-out-only` (behind NAT / a private VNet — dials OUT only, **never gets an inbound LoadBalancer**) or `wan-accessible` (a routable rendezvous others dial out to — the operator advertises its address as `.status.wanAddress`, which siblings put in `externalPeers` and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)) consumes). Also rendered INTO the engine: a `dial-out-only` slice gets `--dial-out-only` ([zetta#803](https://github.com/oramasearch/zetta/issues/803)) so its nodes advertise no dialable address and `system.nodes.reachability` reflects it. |
| `spec.ephemeral` | **Default `false`.** Marks the slice as burst/autoscaled capacity expected to churn ([operator#69](https://github.com/oramasearch/orama-operator/issues/69)). **Always forced to `dial-out-only`** (an ephemeral node is never advertised as a rendezvous — its address would flap); its membership blips aren't treated as a cluster fault. Rendered into the engine as `--node-type ephemeral` (else `--node-type permanent`, [zetta#810](https://github.com/oramasearch/zetta/issues/810)) → `system.nodes.node_type`; permanent nodes carry the durability-bearing replication factor, ephemeral ones don't. |
| `spec.wanAddress` | For a `wan-accessible` slice, the routable address (`host` or `host:port`; `:7777` assumed) siblings use — a mesh-VPN address, a stable public IP, or a DNS name. **Authoritative** when set (advertised verbatim as `.status.wanAddress`); when empty the operator derives it from `spec.hostNetwork` (node host IP) or `spec.exposePeerLoadBalancer` (cloud LB IP). Ignored unless `wan-accessible`. |
| `spec.hostNetwork` | **Default `false`.** Run node pods on the host network so `:7777`/`:4800` are reachable at the k8s node's own address — the simplest routable address for a **wan-accessible** slice (esp. single-node). Packs ordinals onto per-ordinal ports when `replicas>1`. |
| `spec.exposePeerLoadBalancer` | **Default `false`.** A cloud `LoadBalancer` fronting the peer port `:7777`. **Valid ONLY for a `wan-accessible` slice** — on a `dial-out-only`/ephemeral slice it is **ignored** (a warning is logged, any existing LB torn down) because a private node must dial OUT, not accept inbound (the [zetta#739](https://github.com/oramasearch/zetta/issues/739) anti-pattern). Even for wan-accessible it's only one option (a host with a public IP or a `wanAddress` needs none). Peer traffic is **mTLS** on the wire (the shared cluster join token derives the CA, [operator#92](https://github.com/oramasearch/orama-operator/issues/92)), so keep the `<name>-zetta-cluster` Secret protected. |
| `spec.provisionOperatorSchema` | **Default `true`.** Makes this cluster the operator's own datastore (One Orama symbiosis v0): once the node is up, an in-cluster Job applies the operator's schema via Zetta's `/v1/sql` — the **`orama_control`** database (`components`, `reconcile_events`, `topology`, `deployments`) and the **`orama_data`** database (`documents`, the app-data landing zone). Idempotent (tolerates "already exists"; Zetta has no `IF NOT EXISTS`). A rendered-schema hash automatically replaces the immutable Job when `operatorZettaSchema` changes. Set `false` to opt out. |
| _(readiness probe — disabled)_ | The pod's readiness probe is **off** ([operator#68](https://github.com/oramasearch/orama-operator/issues/68)) because the node's flaky HTTP server ([zetta#731](https://github.com/oramasearch/zetta/issues/731)) made it flap out of the Service. Liveness + startup (peer port) remain. Restore `GET /mcp.md` readiness once zetta#731 lands. |
| `spec.mcpTokenAuth` | **DEPRECATED / no-op** as of [zetta#799](https://github.com/oramasearch/zetta/issues/799). The engine made auth **mandatory** and removed the `--mcp-token` flag (passing it now crashes the node), so the operator no longer renders it or provisions a `<name>-zetta-mcp` Secret. Auth is enforced with the seeded admin token instead (see `spec.requireAuth`). Retained for API compatibility; setting it has no effect. |
| `spec.requireAuth` | **DEPRECATED / no-op** as of [zetta#799](https://github.com/oramasearch/zetta/issues/799). The engine made auth **mandatory** and removed `--require-auth` — every node ALWAYS gates its HTTP/MCP/SQL surface on a bearer that resolves to a user; there is no unauthenticated/plaintext/legacy-shared-secret mode. The operator now **ALWAYS** mints a 64-hex admin token into Secret `<name>-zetta-admin` (key `token`, mint-once), injects it as env `ZETTA_ADMIN_TOKEN` → `--admin-token` on every node (redacted to `****` in the pod log) to seed the owner, authenticates its **own** Zetta SQL client + schema-provisioning Job with it, and the `ZettaClusterE2ETest` always runs its authz assertions (`ZETTA_E2E_REQUIRE_AUTH=1`). **Image requirement:** the target's image MUST be an **auth-capable build** ([zetta#799](https://github.com/oramasearch/zetta/issues/799)) — the pre-#799 sandbox image rejects it. The field is retained for API compatibility; setting it has no effect (auth is enforced regardless). Sample: [`../config/samples/zetta-require-auth.yaml`](../config/samples/zetta-require-auth.yaml). |
| `spec.ingressEnabled` | **Default `false`. The explicit external-exposure gate.** When `true` **and** `spec.host` is set, the operator provisions the nginx Ingress (+ the Cloudflare DNS record below if `dnsSecretRef` is set), proxying the whole host to `:4800`. When `false` (or `host` empty) **nothing external is created and any previously created Ingress/DNS are torn down** — even if `host` is still set. Lets you keep `host` recorded while toggling exposure off, instead of overloading "empty host" as the off switch. |
| `spec.host` | The **domain name** the API/UI (:4800) is exposed at (e.g. `zetta.oramadev.net`) when `ingressEnabled` is `true` — the whole host proxies to the node's `:4800`, so root `/` is the node's **web dashboard** and other paths are MCP/REST/serving. Requires an ingress controller. Names the endpoint; `ingressEnabled` is the on/off switch. |
| `spec.dnsSecretRef.name` | With `ingressEnabled` + `spec.host`, create a **proxied Cloudflare DNS record** (host → ingress LB) as an owned `CloudflareDNSRecord` CR, using the named Secret (keys `token`/`zoneID`/`accountID`) — normally the same `cloudflare-api-token` Secret the `OramaCloudV1` uses. |
| `spec.capacity.karpenter.enabled` | **Default `false`. Opt-in elastic node capacity** ([operator#69](https://github.com/oramasearch/orama-operator/issues/69), first slice). When `true`, the operator owns a Karpenter **`NodePool`** and pins the Zetta pods onto it (a dedicated taint `orama.cloud/zetta:NoSchedule` + toleration and the `orama.cloud/zetta-pool` nodeSelector), so raising `spec.replicas` past current node capacity leaves pods **Pending → Karpenter provisions matching nodes just-in-time →** the new pods schedule and join the CRDT cluster. Scheduling-driven scale-up; the Zetta-signal-driven trigger (`orama_control.scale_requests`) is a later slice. |
| `spec.capacity.karpenter.nodeClassRef` | The provider **NodeClass** Karpenter launches nodes from — `{group, kind, name}` (the Karpenter v1 `nodeClassRef` shape). Defaults `group: karpenter.k8s.aws`, `kind: EC2NodeClass` (EKS); on AKS NAP set `group: karpenter.azure.com`, `kind: AKSNodeClass`. **`name` is REQUIRED when enabled** — the operator owns only the cloud-neutral NodePool; the NodeClass (AMI/subnet/IAM on AWS) is platform-owned. Missing name ⇒ `CapacityReady=False, Reason=Blocked`. |
| `spec.capacity.karpenter.capacityType` / `.arch` | `on-demand` (**default**, stateful-safe) \| `spot`; `amd64` (**default**) \| `arm64`. Spot only with enough replicas to tolerate an interruption (a reclaim evicts a peer). |
| `spec.capacity.karpenter.instanceTypes[]` | Optional pin via the portable `node.kubernetes.io/instance-type` label. Empty (recommended) ⇒ Karpenter chooses the cheapest fitting type from the NodeClass offerings. |
| `spec.capacity.karpenter.limits` | `{cpu, memory, nodes}` — the **cost/scale guardrail**; Karpenter stops provisioning once any cap is hit. Defaults `cpu: "16"`, `memory: 64Gi`, `nodes: 3`. |
| _(disruption / quorum-safety)_ | The generated NodePool is **stateful-safe**: `consolidationPolicy: WhenEmpty` (never evict a live peer for underutilization), a `nodes: "1"` disruption budget (cap churn), `expireAfter: Never`, `terminationGracePeriod: 1h`; the Zetta pods carry `karpenter.sh/do-not-disrupt` and a `maxUnavailable: 1` PodDisruptionBudget. Operator-driven drain-safe **scale-down** is a later slice. |
| _(Karpenter absent)_ | If the `karpenter.sh/v1 NodePool` CRD isn't installed (bare metal / no autoscaler), enabling capacity is a **graceful no-op**: `CapacityReady=False, Reason=KarpenterNotInstalled`, no crash-loop. `spec.replicas` still packs more pods onto the fixed host until full. Or set `install: true` (below) to have the operator install Karpenter itself. |
| `spec.capacity.karpenter.install` | **Default `false`. Opt-in — make the operator OWN the Karpenter install** ([operator#75](https://github.com/oramasearch/orama-operator/issues/75)). When `true`, the operator server-side-applies the pinned upstream Karpenter controller (Deployment + ServiceAccount + RBAC) and the Karpenter CRDs (`NodePool`/`NodeClaim`/`EC2NodeClass`) as owned resources (modelled as `unstructured` — **no Karpenter Go types vendored**; version pinned to **v1.13.0** under `internal/controller/assets/karpenter/`). **Cloud-gated:** a clean no-op on bare-metal/k0s (`Reason=BareMetalNoCloud`) and on AKS, where **Node Auto Provisioning already manages Karpenter** (`Reason=ManagedByAKSNAP` — use `enabled`, not `install`); the self-install path targets **EKS**. Surfaced via the `KarpenterInstalled` condition. |
| `spec.capacity.karpenter.controllerRoleARN` | **The cloud IAM identity the operator CANNOT mint** — a cloud-account prerequisite, same class as the image pull secret. The AWS IAM role ARN Karpenter's **controller** assumes (via IRSA — annotated onto its ServiceAccount) to create/terminate EC2 instances. **Required when `install` on EKS** (unless wired out-of-band, e.g. EKS Pod Identity); empty ⇒ `KarpenterInstalled=False, Reason=CloudIdentityMissing` and **the controller is not deployed** (no crash-loop). This is the *controller* identity; the launched *nodes* get their own instance profile via the platform-owned `nodeClassRef`. See [`docs/karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md). |
| `spec.capacity.karpenter.installClusterName` | The AWS EKS cluster name Karpenter's controller uses (`settings.clusterName`) to discover the cluster's tagged subnets/security-groups/interruption queue. **Required when `install` on EKS**; empty ⇒ `KarpenterInstalled=False, Reason=InstallClusterNameRequired`. |
| `KarpenterInstalled` condition | Present only when `install` is set. `BareMetalNoCloud` / `ManagedByAKSNAP` / `UnsupportedCloud` / `CloudIdentityMissing` / `InstallClusterNameRequired` (gated or blocked) → `InstallProgressing` (applied, controller not yet Available) → `Installed` (controller available in `kube-system`). Written **only on change** (anti-flap). |
| `status.phase` / `.readyNodes` / `.totalNodes` | Readiness roll-up. `kubectl get ozc`. |
| `status.provisionedNodes` / `CapacityReady` condition | Number of nodes Karpenter has provisioned for this cluster (`karpenter.sh/nodepool=<pool>`), plus a `CapacityReady` condition (`Disabled`-dropped / `Blocked` / `KarpenterNotInstalled` / `NodePoolProgressing` / `NodePoolReady`). Written **only on change** (anti-flap). |
| `status.reachability` / `.ephemeral` | The **effective** reachability type of the slice (a print column) + whether it's ephemeral — echoed from spec (`reachability` forced to `dial-out-only` when `ephemeral`). |
| `status.wanAddress` | The routable rendezvous address (`host:port`) siblings dial OUT to when this slice is **wan-accessible** — copy it into a sibling's `spec.externalPeers`, and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)) is populated from the wan-accessible set (its HTTP origin is the same host at `:4800`). Empty for a `dial-out-only` slice or until the address is known. Shown by `kubectl get ozc -o wide`. |

**Live (`oramadev.net` sandbox):** `GET https://zetta.oramadev.net/` returns the node's **web dashboard**,
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
| `ZETTA_E2E_REQUIRE_AUTH` | **Always `1`** — auth is mandatory ([zetta#799](https://github.com/oramasearch/zetta/issues/799)), so the suite always runs its **authz-enforcement assertions** (an unauthenticated request must be refused; the full RLS/masking/scoped-grants/token-lifecycle matrix lands as the authz system does). |
| `ZETTA_E2E_TOKEN` | **Always** a `SecretKeyRef` to `<targetRef>-zetta-admin` key `token` (the **seeded admin** principal, always provisioned, so authenticated calls pass). |

If the target `OramaZettaCluster` doesn't exist yet, the test parks in `Pending`
(`waiting for target OramaZettaCluster <ref>`) and requeues — it never creates a Job without a target.
A change to Zetta coverage touches **three** repos: zetta (the script) → zetta-bridge (the image) →
this operator (the CRD). Sample: [`../config/samples/zetta-e2e-test.yaml`](../config/samples/zetta-e2e-test.yaml).

**Authz coverage (always on):** auth is mandatory ([zetta#799](https://github.com/oramasearch/zetta/issues/799),
[issue #88](https://github.com/oramasearch/orama-operator/issues/88)), so this test **always** runs
the suite's authz-enforcement assertions — it sets `ZETTA_E2E_REQUIRE_AUTH=1` and hands the suite the
target's **seeded admin token** (`<targetRef>-zetta-admin`, always provisioned). **Multi-node** authz
coverage needs no extra field: the reconciler already builds `ZETTA_E2E_PEER_URLS` from the target's
replicas, so pointing this at a `replicas>=2` cluster exercises the authenticated path across every node.
End-to-end sample (a 3-replica target + its test): [`../config/samples/zetta-require-auth.yaml`](../config/samples/zetta-require-auth.yaml).

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the `OramaZettaCluster` to test (same namespace). Required. |
| `spec.image` | **Runner image** (default `oramasearch/zetta-e2e:v1-sandbox`) that bakes the suite as its `ENTRYPOINT` — the operator sets only `Env`, never a `Command`. |
| `spec.levels` | Which test levels to run (`ZETTA_E2E_LEVELS`, default `2,3`). |
| `spec.pullSecretName` | Pull secret authenticating image pulls (also avoids Docker Hub anon rate limits). |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed` (from the Job). `kubectl get ozce2e`; `kubectl logs job/<name>-e2e`. |
## `CloudflareLoadBalancer` (separate CRD) — the global geo-LB

Owns a Cloudflare **global Load Balancer** ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)):
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
([operator#74](https://github.com/oramasearch/orama-operator/issues/74) /
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
([operator#40](https://github.com/oramasearch/orama-operator/issues/40) /
[#41](https://github.com/oramasearch/orama-operator/issues/41)) — declarative topology
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

## `PostgresSchema` (separate CRD) — a versioned, tracked SQL schema

Applies + tracks a **versioned Postgres schema**
([operator#40](https://github.com/oramasearch/orama-operator/issues/40)) — superseding the
bare idempotent migration Job ([#34](https://github.com/oramasearch/orama-operator/issues/34))
with a reconciled, auditable CRD that records the applied version in `.status`. The controller
(`postgresschema_controller.go`) **owns** an idempotent migration Job (wait-for-DB → guard →
apply-in-one-transaction) and reflects `phase` + `appliedVersion` (read back from the Job, so a bumped
desired version never misreports). It carries **no finalizer** — deleting the CR never drops the
schema/data, it only stops managing the migration.

`OramaCloudV1` creates + owns a `<cr>-controldb-schema` instance (`reconcileControlDBMigrations`)
applying the dashboard's vendored Drizzle schema to the CNPG control DB. See
[`datastore-crds.md`](datastore-crds.md) for the full design. Sample:
[`../config/samples/postgresschema.yaml`](../config/samples/postgresschema.yaml).

| Field | Purpose |
|---|---|
| `spec.connectionSecretRef` | `{ name, key }` — Secret + key holding the Postgres connection URI (the CNPG `<cr>-controldb-app` Secret, key `uri`). Missing = safe no-op (waits). |
| `spec.schemaConfigMapRef` | `{ name, key }` — ConfigMap + key holding the schema SQL (applied all-or-nothing in one transaction). |
| `spec.version` | Schema version recorded in `.status.appliedVersion` once applied. A change automatically replaces the prior Job with foreground deletion, waiting for its pods before starting the guarded apply. Lake hashes SQL plus guard; the dashboard hashes SQL. |
| `spec.guardTable` | A `public` table whose existence means the schema is already applied (idempotency guard). Default `crawler_configs`. |
| `spec.image` | psql-carrying image for the migration Job. Default `postgres:16-alpine`. |
| `status.phase` / `.appliedVersion` | `Pending \| Applying \| Applied \| Failed` + the version actually applied. `kubectl get pgschema`. |
| `status.conditions[Applied]` | `Applied` \| `Applying` \| `ReplacingMigrationJob` \| `WaitingForDatabase` \| `MigrationFailed`. Written only on a real transition (anti-flap). |

## `ClerkApplication` (separate CRD) — Clerk auth per environment

Provisions a **Clerk application per environment** (dev/testing/staging/prod) via the Clerk
**Platform API** (`POST /v1/platform/applications`), so Studio auth ([zetta#889](https://github.com/oramasearch/zetta/issues/889))
gets its issuer + keys with no manual dashboard work. One CR per environment; the resulting
publishable/secret keys + `CLERK_ISSUER` land in an owned Secret. Template: `CloudflareKVNamespace`.
Design: [orama-bridge#116](https://github.com/oramasearch/orama-cloud-v1-operator/issues/116).

| Field | Meaning |
|---|---|
| `spec.secretRef` | Secret holding the Clerk **Platform API token** (key `platformToken`). You create it by hand — the operator never sets or logs it. |
| `spec.applicationName` | Clerk app display name; the **idempotency key** — the reconciler adopts an existing app of this name instead of creating a duplicate. |
| `spec.environment` | `dev \| testing \| staging \| prod`. Informational + the default for `instanceType`. |
| `spec.instanceType` | `development \| production`. Default: `prod` → production (needs `spec.domain`), else development (free `clerk.accounts.dev`). |
| `spec.template` / `spec.domain` | optional Clerk template (`b2c-saas`…) / custom domain for a production instance. |
| `spec.keysSecretName` | where keys + issuer are written. Default `<name>-clerk-keys` (owner-ref'd → GC'd on delete). Keys: `publishableKey`, `secretKey`, `issuer`. |
| `spec.deletionPolicy` | `Retain` (default) orphans the Clerk app on CR delete — a user pool is **never destroyed by accident**; `Delete` removes it via the Platform API. |
| `status.applicationID` / `.instanceID` | the Clerk `app_…` / `ins_…` ids. `kubectl get clerkapp`. |
| `status.issuer` | the Frontend API URL → `CLERK_ISSUER` on the node (derived from the publishable key). |
| `status.publishableKey` | non-secret, surfaced in status; the **secret key lives only in the keys Secret**, never in status or a log. |

Wire the node: set `CLERK_ISSUER` on the `OramaZettaCluster` from the keys Secret's `issuer` — the
node then verifies Clerk session JWTs via JWKS ([zetta#889](https://github.com/oramasearch/zetta/issues/889)),
with the native bearer/session token still the on-prem fallback. A node with no `CLERK_ISSUER` is
bearer-only.
## `Lake` (separate CRD, cluster-scoped) — the lake service

Deploys **lake** — the hosted service behind `lake.live` (control plane on Postgres via CNPG, data plane on Zetta) (forge, drive, platform, identity, secrets,
PKI, registry and search over one content-addressed store; specified in
[`oramasearch/liquid`](https://github.com/oramasearch/liquid) `docs/features/18-lake-service.md`) — from
one **cluster-scoped** CR. It creates the target **Namespace** (if missing; not owned, so deleting a
Lake never takes a namespace with it), a hardened **Deployment** (`lake-<name>`, running
`spec.image`), a ClusterIP **Service** (`lake-<name>`, port 80), and an owned
**`OramaZettaCluster`** (`lake-<name>-zetta`) for the data plane. Replicated workloads also get a
PodDisruptionBudget and topology spread. A Lake finalizer starts and waits for Zetta's ordered
cleanup before releasing the parent. With `spec.ingressEnabled` and `spec.host` it fronts the
host with an **nginx Ingress** (the whole host → the lake Service), optional origin TLS, and, when
`spec.dnsSecretRef` is given, an **owned `CloudflareDNSRecord`** (proxied, host → the ingress
LoadBalancer) — the same contract and child-controller path as `OramaZettaCluster`. Gating off
tears the Ingress and the record down and leaves the workloads. Sample:
[`../config/samples/orama_v1alpha1_lake.yaml`](../config/samples/orama_v1alpha1_lake.yaml).

| Field | Purpose |
|---|---|
| `spec.namespace` | Namespace the workloads land in (default `lake-dev`). Created if missing; never deleted by the operator. |
| `spec.image` | The lake image, run as built. **Required, no default**; the sample uses the private `oramasearch/lake:v1-sandbox`. |
| `spec.pullSecretName` | imagePullSecret attached to the pods, for a private `spec.image`. Must exist in `spec.namespace`. |
| `spec.port` | Container port the Service (port 80) and probes target. Default 4900, lake's `--listen` default. Readiness is `GET /readyz`; liveness is `GET /healthz`. |
| `spec.args` | Args for the image's entrypoint; empty runs the image as built (lake listens on `0.0.0.0:4900`). |
| `spec.replicas` | Deployment replicas (default 1). |
| `spec.resources` | Complete Kubernetes container resource override. Default requests: CPU `50m`, memory `128Mi`; default limit: memory `512Mi`, with no CPU limit. |
| `spec.topologySpreadConstraints[]` | Complete pod topology-spread override. When omitted with more than one replica, defaults to `maxSkew: 1`, `topologyKey: kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`, selecting this Lake's pods. An explicit empty list disables the default. At one replica no default is rendered. |
| _(workload hardening — automatic)_ | The pod and container require non-root uid/gid 10001, disallow privilege escalation, drop all Linux capabilities, and use `RuntimeDefault` seccomp. The container gets a five-second pre-stop drain and the pod gets a 30-second termination grace period. Rolling updates use `maxUnavailable: 0`, `maxSurge: 1`. More than one replica creates `PodDisruptionBudget lake-<name>` with `maxUnavailable: 1`; scaling to one deletes it. |
| `spec.store` | Persistent libliquid customer store mounted read-write at `/var/lib/lake/store`, with `LAKE_STORE_ROOT` fixed to that path. Without `existingClaim`, the operator creates and owns `lake-<name>-store` once. Later store spec changes never mutate, replace, or delete it. |
| `spec.store.size` / `.storageClassName` / `.accessModes` | Creation-time settings for the generated claim: quantity `100Gi` by default, cluster-default storage class, and `[ReadWriteMany]`. RWX is the multi-replica default; `ReadWriteOnce` requires `spec.replicas: 1`. |
| `spec.store.existingClaim` | PVC in `spec.namespace` to mount instead of creating one. It is referenced without adoption, mutation, or deletion. |
| `spec.env[]` | Kubernetes `EnvVar` entries appended after operator env. `value` and `valueFrom` are supported. Conflicts with `LAKE_ADMIN_TOKEN`, `LAKE_INSTANCE_KEK`, `LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`, `LAKE_DATA_PLANE_URL`, `LAKE_DATA_PLANE_TOKEN`, `LAKE_SEALING_PUBLIC_KEY`, `LAKE_SEALING_KID`, or `LAKE_STORE_ROOT` are ignored so operator values win. Use it for Lake runtime settings such as `LAKE_LOG_MODE`, `LAKE_LOG_WITNESSES`, and `LAKE_PUBLIC_HOST`. |
| `spec.sealing.generation` | Desired X25519 sealing-key generation (default 1). Increasing it rotates; decreasing it is refused. Old private keys remain while any `LakeSealedSecret` record names them. |
| `spec.postgres` | **lake's control plane**: a CloudNativePG `Cluster` named `lake-<name>-controldb` in `spec.namespace`, the same path `OramaCloudV1` uses for its control DB. `enabled` (default `true`; `false` tears it down, deleting its data), `instances` (default 1), `storageSize` (default `50Gi`; CNPG grows the PVC in place when the StorageClass allows expansion), `storageClassName`. CNPG generates the `lake-<name>-controldb-app` Secret; the lake container gets `LAKE_CONTROL_DB_URI` and `LAKE_CONTROL_DB_PASSWORD` from it (optional refs, so the pod starts before the DB does). Requires the CNPG operator (helmfile). |
| `spec.zetta` | **lake's data plane**: an owned `OramaZettaCluster` named `lake-<name>-zetta` in `spec.namespace`. `enabled` defaults to `true`; `false` deletes the cluster and its per-node data volumes. `image`, `replicas` (default 1), `storage.size` (Zetta default `10Gi`), `storage.storageClassName`, and `reachability` (`dial-out-only` by default or `wan-accessible`) map to the child CR. The child inherits the Lake's `pullSecretName`. It sets `provisionOperatorSchema: false` because this cluster stores Lake data rather than operator state. The lake container gets `LAKE_DATA_PLANE_URL=http://lake-<name>-zetta-zetta.<namespace>.svc.cluster.local:4800` and `LAKE_DATA_PLANE_TOKEN` from Secret `lake-<name>-zetta-zetta-admin`, key `token`; the token reference is required, so kubelet waits for the Zetta controller to mint it. |
| `spec.postgres.backup` | Opt-in (`enabled`, default `false`) CNPG Barman object-store backup. `schedule` defaults to six-field `0 0 3 * * *`; `retention` defaults to `30d`; `destination.bucket` and `secretRef.name` are required when enabled. `destination.path` defaults to `lake-<name>/controldb`; `endpointURL` defaults to the Secret's `s3ApiEndpoint`. The Secret must be in `spec.namespace` and contain `s3ApiEndpoint`, `accessKeyId`, and `secretAccessKey`. The operator configures gzip WAL compression and creates owned `lake-<name>-controldb-backup` `ScheduledBackup` resources. See [`lake/backups.md`](lake/backups.md). |
| _(admin token — automatic)_ | Secret **`lake-<name>-admin`** (key `token`, 64 hex), minted once and preserved, owned by the Lake; injected as `LAKE_ADMIN_TOKEN`. lake requires it as `Authorization: Bearer` on every `/v1` capability except `server_info` and on MCP tool calls. Retrieve: `kubectl get secret lake-<name>-admin -n <ns> -o jsonpath='{.data.token}' \| base64 -d`. |
| _(sealing key — automatic)_ | Secret **`lake-<name>-sealing-key`** holds the current and still-needed prior X25519 private keys. Only the active public key and `kid` are injected into lake. See [`lake/sealed-secrets.md`](lake/sealed-secrets.md). |
| _(schema — automatic)_ | With Postgres on, the Lake owns a `lake-<name>-controldb-migrations` ConfigMap holding lake's control-plane schema (vendored byte-identical from `oramasearch/lake` `db/schema.sql`) and a `lake-<name>-controldb-schema` **`PostgresSchema`** that applies it in one transaction, guarded on the newest migration table (`token_attenuation_guard` for `0021_token_attenuation`) and versioned by a hash of SQL plus guard. Schema completion gates Deployment creation and updates. Re-vendor lake `db/schema.sql` unchanged, update the pinned source and guard, and run the checks in [the schema guide](lake/schema.md). Incremental migrations remain #126. |
| `spec.ingressEnabled` | **Default `false`. The explicit external-exposure gate.** With `spec.host`, provisions the nginx Ingress (+ the DNS record below if `dnsSecretRef` is set). `false` tears both down even if `host` is still set. The Ingress permits `64m` request bodies and disables request/response buffering for Lake's streaming transfers. |
| `spec.tls.secretName` | Optional `kubernetes.io/tls` Secret in `spec.namespace`. With ingress enabled and a host set, renders the Ingress TLS host and enables nginx HTTPS redirect. Empty is the backward-compatible HTTP-only default. |
| `spec.host` | Domain the lake is exposed at (e.g. `hello.lake.live`) when `ingressEnabled` is `true`. |
| `spec.dnsSecretRef.name` | With `ingressEnabled` + `host`, creates a **proxied Cloudflare DNS record** (host → ingress LB) as an owned `CloudflareDNSRecord`, once the ingress controller has assigned an address (the owned Ingress's status change re-queues the Lake). The Secret (keys `token`/`zoneID`) must live in `spec.namespace` and hold the zone of `host` — a `lake.live` host needs a `lake.live`-zone Secret, not the `oramadev.net` one the Zetta clusters use. |
| `spec.previewWildcard` | DNS suffix for preview environments. `preview.example.com` yields `<change-short>.preview.example.com`; with Lake ingress and DNS enabled, the Lake owns one `*.preview.example.com` Cloudflare record and previews do not create records per change. |
| `spec.deploySync.interval` | How often the operator reads desired deployment events from its Zetta `orama_control.deployments` table. Default `15s`; positive values below `1s` are clamped to `1s`. |
| `status.readyReplicas` / `.phase` | Replica count and overall service state: `Progressing \| Ready \| Degraded`. `Ready` requires the current Deployment generation with the desired positive ready/updated/available replica counts, a Bound store, enabled Postgres readiness and current applied schema, enabled Zetta readiness, and valid enabled backup configuration. Replica count alone does not establish readiness. Written only on a real transition. |
| `status.conditions[Ready]` | `LakeReady` on success. Pending work reports `Progressing` with `DeploymentProgressing`, `PostgresNotReady`, `SchemaPending`, `SchemaNotCurrent`, or the store/Zetta reason. Failed schema (`SchemaFailed`), Deployment replica failure (`ReplicaFailure`) or progress deadline (`ProgressDeadlineExceeded`), degraded Zetta, and invalid enabled backup configuration report `Degraded`. Failures take precedence over pending work; messages explain the blocker. Unchanged observations preserve transition times and write no status. |
| `status.store` / `conditions[StoreReady]` | Selected `claimName`, PVC `phase`, and bound `capacity`. `StoreReady=False` reports a missing or unbound claim; an unbound store keeps the overall phase out of `Ready`. |
| `status.schemaPhase` / `.schemaVersion` | Mirrors the owned PostgresSchema: `Pending \| Applying \| Applied \| Failed` and the applied `sha256:<12>`. Overall readiness requires the schema controller to observe the current generation, `Applied=True`, and the applied version to match the desired SQL-plus-guard hash. A stale `Applied` value keeps the Lake `Progressing` and cannot release a rollout. |
| `status.postgresReady` / `.postgresInstances` | True when CNPG reports at least the configured `spec.postgres.instances` ready instances (default 1). The count mirrors CNPG `readyInstances`/`instances`, e.g. `1/1`; missing/unreadable status is unready. Polled every 20s whenever Postgres is enabled, including after success and with backups disabled. Readiness loss removes overall `Ready` on the next poll. Disabled Postgres reports false with an empty count and does not gate readiness or schema. |
| `status.zettaReady` / `conditions[ZettaReady]` | True only when the owned `OramaZettaCluster` reports `Ready` with every desired node ready. `ZettaProgressing`, `ZettaDegraded`, and `ZettaDisabled` explain false states. The Lake owns and watches the child and also polls every 20s while it is enabled but unready. |
| `status.backupReady` / `.lastBackupTime` | True only when the ScheduledBackup exists and the newest CNPG Backup labelled `cnpg.io/cluster=lake-<name>-controldb` is `completed`; completion time comes from that Backup. The `Backup` print column shows `lastBackupTime`. Invalid enabled configuration reports `BackupReady=False` with a condition and `Phase=Degraded`. |
| `status.dataServices[]` | One row per associated data service: `name`, `kind` (`postgres \| zetta`), `managed`, and `backup`, `restore`, `upgrade`, `failover` evidence (`tested \| untested \| none`), plus optional `lastVerified` and `notes`. `managed` is true only when all operations required by the current configuration are tested. Single-instance Postgres reports `failover: none`; owned Zetta remains unmanaged until its lifecycle procedures are rehearsed. The `Data` print column uses `dataServicesSummary` to show managed/unmanaged counts. |
| `status.sealing` | Active `{cluster, generation, kid, publicKey}` recipient plus `previousKeys[] {kid, remaining}` and `unresolvedRecords`. `cluster` is the Lake UID and is part of envelope AAD. `remaining` counts records still requiring an old private key; malformed records retain all old keys until corrected. |

## `LakeE2ETest` (separate CRD, cluster-scoped)

Runs the lake service exit suite as a Job against a cluster-scoped `Lake`. The suite remains in a
dedicated image so its signup and liquid protocol coverage evolves with lake and liquid; the
operator schedules it and reports the Job result. The controller waits until the target reports a
ready replica, ready control-plane Postgres, and an applied schema. It then creates `<name>-e2e` in
the Lake namespace with service-account token mounting disabled. Sample:
[`../config/samples/lake-e2e-test.yaml`](../config/samples/lake-e2e-test.yaml).

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the cluster-scoped `Lake`. Required. The Job runs in that Lake's `spec.namespace`, or `lake-dev` when omitted. |
| `spec.image` | Required runner image. Its entrypoint signs up a fresh user, creates a repository, uses the real liquid client to push and pull through the Lake sync routes, and verifies the pulled file bytes. Use an immutable published tag. |
| `spec.pullSecretName` | Optional runner image pull Secret in the Lake namespace. Empty reuses the target Lake's `spec.pullSecretName`. |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed`, written only when the phase, message, or Job name changes. `Pending` creates no Job while the target is absent or not ready. |
| `status.message` / `.jobName` | Current explanation and the retained Job name. Logs are available with `kubectl logs -n <lake-namespace> job/<name>-e2e`. |

The runner receives exactly two values from the controller: `LAKE_E2E_BASE_URL` is
`http://lake-<target>.<namespace>.svc.cluster.local`, and `LAKE_E2E_RUN_ID` is the test resource UID
for collision-free account and repository names. Authentication begins with the suite's public
signup path; the operator does not inject the Lake admin bearer.

## `LakeEnvironment` (separate CRD, namespaced) — one desired deployment

A `LakeEnvironment` is the Kubernetes desired-state record materialized from one deployment in the
operator's Zetta control plane. It lives in the referenced Lake's `spec.namespace` and is owned by the
cluster-scoped `Lake`. The controller gives it a dedicated profiled Namespace, Deployment, Service and
optional Ingress. See [`lake/environments.md`](lake/environments.md) for the event model, lifecycle and
verification. Sample:
[`../config/samples/orama_v1alpha1_lakeenvironment.yaml`](../config/samples/orama_v1alpha1_lakeenvironment.yaml).

| Field | Purpose |
|---|---|
| `spec.lakeRef` | Name of the cluster-scoped `Lake`; the `LakeEnvironment` must live in that Lake's `spec.namespace`. |
| `spec.repository` / `.environment` | Stable Lake repository identifier and human-facing environment name. |
| `spec.kind` | `named` or `preview`. Preview lifecycle also ends when its change is closed. |
| `spec.change` | Immutable liquid change identifier. The DNS-safe short form prefixes a preview hostname. |
| `spec.image` | Required immutable OCI digest reference (`...@sha256:<64 hex>`). Rollback is another deployment event naming an older change and image. |
| `spec.replicas` / `.port` | Deployment replicas (default `1`) and required container port exposed by Service port 80. |
| `spec.env[]` | Literal `{name,value}` workload environment variables. |
| `spec.secretRefs[]` | Names or `secret://<name>` references for `LakeSealedSecret` resources in the Lake namespace. Every reference must belong to the same Lake and have delivered its current generation. The produced Secrets are copied into the isolated workload namespace and imported with `envFrom`; unresolved references report `Degraded` and remove the workload. |
| `spec.host` | Exact hostname for a named environment. Preview hosts are derived from the Lake's `previewWildcard`. |
| `status.phase` | `Progressing \| Ready \| Degraded \| TornDown`, written only on a real transition. |
| `status.url` / `.readyReplicas` | Public URL when ingress is configured, and observed Deployment readiness. |
| `status.deploymentId` | Stable deployment-event identity copied from the `orama.cloud/deployment-id` annotation. |
| `status.conditions[]` | Standard `Ready` condition with transition reason and message. |

## `LakeRestoreRehearsal` (separate CRD, cluster-scoped)

Runs and records a Lake data-service lifecycle rehearsal. The spec is immutable. Restore and
upgrade recover a selected CNPG object-store backup into an isolated scratch namespace, compare
guard-table row counts and the newest schema migration with the live database, and delete every
scratch object after success or failure. Upgrade changes the scratch Cluster image before
verification. Failover requests a CNPG switchover on the live replicated control DB and verifies
the existing application Secret still connects through the write Service.

| Field | Purpose |
|---|---|
| `spec.lakeRef` | Name of the cluster-scoped `Lake`. |
| `spec.service` | `postgres` in this version. |
| `spec.operation` | `restore \| upgrade \| failover`. |
| `spec.from` | `latest` by default, or the name of a completed CNPG `Backup`. Restore and upgrade pin its Barman `status.backupId`. |
| `spec.targetImage` | Required for `upgrade`: the CloudNativePG-compatible PostgreSQL operand image tested on the scratch clone. |
| `status.phase` / `.message` | `Pending \| Running \| Succeeded \| Failed` and the current or terminal explanation. |
| `status.scratchNamespace` / `.selectedBackup` | Deterministic scratch location and the pinned CNPG Backup object for restore and upgrade. |
| `status.previousPrimary` / `.targetPrimary` | The primary transition selected by a failover rehearsal. |
| `status.startedAt` / `.completedAt` / `.observedGeneration` | Durable audit timestamps and the represented immutable spec generation. |

`kubectl get lrr` shows Lake, Service, Operation, Phase, and completion time. Detailed procedures:
[`lake/data-services/postgres.md`](lake/data-services/postgres.md).

## `LakeSealedSecret` (separate CRD, namespaced) — one-way Secret delivery

Produces one owned Kubernetes Secret from authored ciphertext or a lake reference. The referenced
cluster-scoped Lake owns the `LakeSealedSecret`; the resource owns the produced Secret. Literal
records open only with the target Lake's private key and require exact AAD for the Lake UID,
resource namespace, and `spec.template.name`. The output is labelled
`lake.orama.cloud/sealed: "true"`. Complete envelope and rotation rules:
[`lake/sealed-secrets.md`](lake/sealed-secrets.md). Sample:
[`../config/samples/lake-sealed-secret.yaml`](../config/samples/lake-sealed-secret.yaml).

| Field | Purpose |
|---|---|
| `spec.lakeRef.name` | Cluster-scoped `Lake` that owns the recipient key and lake service endpoint. |
| `spec.template.name` / `.type` | Produced Secret name and optional Kubernetes Secret type (`Opaque` when omitted). The name is bound into envelope AAD. |
| `spec.data` | Map of Secret keys to standard-base64 canonical-CBOR records. Required only for `literal`. All entries must use one `kid`. |
| `spec.source.type` | `literal \| lake \| dynamic \| delivery`. `delivery` is created by the environment controller. |
| `spec.source.lake` | Static lake reference `{mount, name, version?}`. It contains no value. |
| `spec.source.dynamic` | Dynamic request `{engine, role, ttl}`. It contains no credential. |
| `spec.source.delivery` | Operator-managed `{environment, name}` reference to a LakeEnvironment and original LakeSealedSecret in the Lake namespace. Owns the copied Secret in the workload namespace; contains no ciphertext. See [environment ownership and upgrade order](lake/environments.md#sealed-secrets). |
| `status.phase` | `Pending \| Ready \| Error`. Written only on a real transition. |
| `status.unsealedAt` / `.kid` | Last successful produced-Secret update and the key that opened its record set. |
| `status.lease` | Dynamic lease `{id, expiresAt, renewAt}`; no credential value. |
| `status.conditions[Ready]` | `Delivered` or a stable refusal reason such as `UnsealFailed`, `SecretOwnershipConflict`, or `MintFailed`. |
