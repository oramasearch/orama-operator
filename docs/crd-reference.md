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
| `cloudflare.*` | object | enabled | Operator manages CF **DNS + Load Balancing + R2/KV** via the CF API. **Two Secrets:** `apiTokenSecretRef` → `token`+`zoneID`+`accountID` (control plane; `token` is also the DNS-01 credential of the stack's Let's Encrypt `Issuer`, so a stack with any host needs it); `r2SecretRef` → `s3ApiEndpoint`+`accessKeyId`+`secretAccessKey` (R2 S3 data plane). |
| `peers[]` | list | — | Other regions (read tiers → primary `writerEndpoint`/`postgresEndpoint`); no mesh. |
| `demoSeed` | bool | false | Seed a demo project+index so a fresh install answers queries. |

## `spec.components`

Every component has `enabled`. Data stores also have `mode: in-cluster|external`,
`placement: local|central`, `replicate`, and `endpoint`/`credentialsSecretRef`.
`spec.components.ingressClassName` (`nginx`, the default, or `zetta-tunnel`) picks the ingress that
serves the stack's Ingresses; DNS follows ([`infrastructure/ingress.md`](infrastructure/ingress.md)).
Every host (`dashboard.host`, `docs.host`) gets origin TLS: the stack owns cert-manager `Issuer`
`<cr>-letsencrypt` (Let's Encrypt, DNS-01 with `cloudflare.apiTokenSecretRef` key `token`), and each
host's certificate lives in Secret `<host>-tls` beside the Ingress. There is no per-host TLS field and
no way to turn it off; without the token the reconcile fails rather than serving HTTP
([`components.md`](components.md#origin-tls-ingress_tlsgo-457)).

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
reusing the same Cloudflare child-controller path as the dashboard DNS. A `CloudflareDNSRecord` whose
name (or type) changes deletes the record it made before (`status.recordID`) once the new one exists,
so a host change never leaves the old name live (#529); a record already gone counts as deleted.
A `CloudflareDNSRecord` takes `type` `A`, `CNAME` or `TXT`; `proxied` is always written (default
`true` only when left out), so a controller's `false` is kept; and `deletionPolicy: Retain` leaves the
Cloudflare record when the resource is deleted (`Delete`, the default, removes it), which a Lake's Clerk
records use because every Lake sharing the instance writes them (operator#541). The Ingress proxies the **whole
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
| `spec.storage.size` / `.storageClassName` | Per-node PVC at `--data-dir` (`/data`) — the CRDT doc, `node_id`, certs, JSONL/large-object data. They shape the StatefulSet's claim template only when the StatefulSet is created: the template is immutable, so a later change is kept in the spec and never rewrites it. Whoever raises the size grows the existing claims in place (the Lake workspace reconciler does, operator#259); a replica added later starts at the template's size. While spec.storage differs from the template, `status.conditions[StorageTemplateCurrent]` is `False` with reason `ClaimTemplateUnchanged`, saying who grows the claims (the workspace reconciler, or nobody for any other cluster). |
| _(pod security and volume ownership — automatic)_ | Zetta pods mount no ServiceAccount token, require non-root uid/gid/fsGroup 10001, use `RuntimeDefault` seccomp, disallow privilege escalation and drop all capabilities. `fsGroupChangePolicy: OnRootMismatch` avoids a recursive ownership change when a `/data` volume already has the correct group. This satisfies restricted Pod Security Admission when host networking is off. |
| `spec.resources` | Per-node mem/cpu caps (right-sizing). |
| `spec.env[]` | Kubernetes `EnvVar` entries appended to the Zetta container after operator-managed variables; `value` and `valueFrom` are supported. Changes update the StatefulSet pod template and roll the nodes. `POD_NAME`, `HOST_IP`, `ZETTA_CLUSTER_TOKEN`, `ZETTA_ADMIN_TOKEN`, `ZETTA_HOST_NETWORK`, `ZETTA_ADVERTISE_MAP`, `ORAMA_ZETTA_SEED`, and the engine's own provisioning variables `ZETTA_SEED_PEER` and `ZETTA_DATA_DIR` (an unvalidated peer, or a data dir off the PVC — [operator#279](https://github.com/oramasearch/orama-operator/issues/279)) are reserved: operator values win, and collisions appear in `status.ignoredEnvVars`. Use this for engine settings such as `ZETTA_MALLOC_TRIM_THRESHOLD_BYTES` and `MALLOC_ARENA_MAX`. |
| _(cluster join token — automatic)_ | Inter-node peer traffic is **always mTLS**. The operator mints one shared cluster join token per cluster into Secret **`<name>-zetta-cluster`** (`token` = 64-hex, minted once, never rotated) and injects it into **every** node as `ZETTA_CLUSTER_TOKEN`; Zetta derives the cluster CA deterministically from it, so all nodes trust each other and form **one** cluster ([operator#92](https://github.com/oramasearch/orama-operator/issues/92)). There is no plaintext-peer knob — an unencrypted mode was removed as a security hole. See [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md). |
| `spec.clusterName` | The **GLOBAL** Zetta cluster name. CRs (in the same or other k8s clusters) sharing it are **siblings** — equal peers of one multi-leader cluster ([operator#71](https://github.com/oramasearch/orama-operator/issues/71)); this CR is a **slice**. Empty = a standalone cluster named after the CR. |
| `spec.externalPeers[]` | Route table ([operator#70](https://github.com/oramasearch/orama-operator/issues/70)) — `host:port` addresses of **wan-accessible** siblings, each appended as `--peer` so this slice's nodes dial OUT and join the shared cluster. Copy a sibling's `.status.wanAddress` here. Zetta is the mesh (gossip relays transitively), so a connected dial-graph is enough. **Each entry must be exactly `host:port`** — a DNS name, an IPv4 address or a bracketed IPv6 address (`[fd00::1]:7777`), port 1–65535; at most 64 entries ([operator#279](https://github.com/oramasearch/orama-operator/issues/279)). The API server refuses anything else (CRD pattern). A CR stored before that rule with an invalid entry is **refused by the controller**: the StatefulSet is left unchanged, `status.phase` is `Degraded` and `PeerAddressesValid=False` (`InvalidSpecPeerAddress`) names the bad entries. Peers reach Zetta as process arguments, never as shell text. |
| `spec.reachability` | **Default `dial-out-only`.** The node-reachability type ([zetta#739](https://github.com/oramasearch/zetta/issues/739)): `dial-out-only` (behind NAT / a private VNet — dials OUT only, **never gets an inbound LoadBalancer**) or `wan-accessible` (a routable rendezvous others dial out to — the operator advertises its address as `.status.wanAddress`, which siblings put in `externalPeers` and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)) consumes). Also rendered INTO the engine: a `dial-out-only` slice gets `--dial-out-only` ([zetta#803](https://github.com/oramasearch/zetta/issues/803)) so its nodes advertise no dialable address and `system.nodes.reachability` reflects it. |
| `spec.ephemeral` | **Default `false`.** Marks the slice as burst/autoscaled capacity expected to churn ([operator#69](https://github.com/oramasearch/orama-operator/issues/69)). **Always forced to `dial-out-only`** (an ephemeral node is never advertised as a rendezvous — its address would flap); its membership blips aren't treated as a cluster fault. Rendered into the engine as `--node-type ephemeral` (else `--node-type permanent`, [zetta#810](https://github.com/oramasearch/zetta/issues/810)) → `system.nodes.node_type`; permanent nodes carry the durability-bearing replication factor, ephemeral ones don't. |
| `spec.wanAddress` | For a `wan-accessible` slice, the routable address (`host` or `host:port`; `:7777` assumed) siblings use — a mesh-VPN address, a stable public IP, or a DNS name. **Authoritative** when set (advertised verbatim as `.status.wanAddress`); when empty the operator derives it from `spec.hostNetwork` (node host IP) or `spec.exposePeerLoadBalancer` (cloud LB IP). Ignored unless `wan-accessible`. Validated like `externalPeers` (DNS name, IPv4, `[IPv6]` or a bare IPv6 literal, optional port 1–65535) by the CRD pattern and the controller; an invalid stored value is refused the same way (`PeerAddressesValid=False`), whatever the reachability ([operator#279](https://github.com/oramasearch/orama-operator/issues/279)). |
| `spec.hostNetwork` | **Default `false`.** Run node pods on the host network so `:7777`/`:4800` are reachable at the k8s node's own address — the simplest routable address for a **wan-accessible** slice (esp. single-node). Packs ordinals onto per-ordinal ports when `replicas>1`. |
| `spec.exposePeerLoadBalancer` | **Default `false`.** A cloud `LoadBalancer` fronting the peer port `:7777`. **Valid ONLY for a `wan-accessible` slice** — on a `dial-out-only`/ephemeral slice it is **ignored** (a warning is logged, any existing LB torn down) because a private node must dial OUT, not accept inbound (the [zetta#739](https://github.com/oramasearch/zetta/issues/739) anti-pattern). Even for wan-accessible it's only one option (a host with a public IP or a `wanAddress` needs none). Peer traffic is **mTLS** on the wire (the shared cluster join token derives the CA, [operator#92](https://github.com/oramasearch/orama-operator/issues/92)), so keep the `<name>-zetta-cluster` Secret protected. |
| `spec.provisionOperatorSchema` | **Default `true`.** Makes this cluster the operator's own datastore (One Orama symbiosis v0): once the node is up, an in-cluster Job applies the operator's schema via Zetta's `/v1/sql` — the **`orama_control`** database (`components`, `reconcile_events`, `topology`, `deployments`, `deployment_records`, and lake's `environment_env` and `domains`) and the **`orama_data`** database (`documents`, the app-data landing zone). Idempotent (tolerates "already exists"; Zetta has no `IF NOT EXISTS`). A rendered-schema hash automatically replaces the immutable Job when `operatorZettaSchema` changes. Set `false` to opt out. |
| _(readiness probe — disabled)_ | The pod's readiness probe is **off** ([operator#68](https://github.com/oramasearch/orama-operator/issues/68)) because the node's flaky HTTP server ([zetta#731](https://github.com/oramasearch/zetta/issues/731)) made it flap out of the Service. Liveness + startup (peer port) remain. Restore `GET /mcp.md` readiness once zetta#731 lands. |
| `spec.mcpTokenAuth` | **DEPRECATED / no-op** as of [zetta#799](https://github.com/oramasearch/zetta/issues/799). The engine made auth **mandatory** and removed the `--mcp-token` flag (passing it now crashes the node), so the operator no longer renders it or provisions a `<name>-zetta-mcp` Secret. Auth is enforced with the seeded admin token instead (see `spec.requireAuth`). Retained for API compatibility; setting it has no effect. |
| `spec.requireAuth` | **DEPRECATED / no-op** as of [zetta#799](https://github.com/oramasearch/zetta/issues/799). The engine made auth **mandatory** and removed `--require-auth` — every node ALWAYS gates its HTTP/MCP/SQL surface on a bearer that resolves to a user; there is no unauthenticated/plaintext/legacy-shared-secret mode. The operator now **ALWAYS** mints a 64-hex admin token into Secret `<name>-zetta-admin` (key `token`, mint-once), injects it as env `ZETTA_ADMIN_TOKEN` on every node, from which the engine seeds the owner (never as `--admin-token`: argv is readable by every process on the node, [operator#279](https://github.com/oramasearch/orama-operator/issues/279); redacted to `****` in the pod log), authenticates its **own** Zetta SQL client + schema-provisioning Job with it, and the `ZettaClusterE2ETest` always runs its authz assertions (`ZETTA_E2E_REQUIRE_AUTH=1`). **Image requirement:** the target's image MUST be an **auth-capable build** ([zetta#799](https://github.com/oramasearch/zetta/issues/799)) — the pre-#799 sandbox image rejects it. The field is retained for API compatibility; setting it has no effect (auth is enforced regardless). Sample: [`../config/samples/zetta-require-auth.yaml`](../config/samples/zetta-require-auth.yaml). |
| `spec.ingressEnabled` | **Default `false`. The explicit external-exposure gate.** When `true` **and** `spec.host` is set, the operator provisions the nginx Ingress (+ the Cloudflare DNS record below if `dnsSecretRef` is set), proxying the whole host to `:4800`. When `false` (or `host` empty) **nothing external is created and any previously created Ingress/DNS are torn down** — even if `host` is still set. Lets you keep `host` recorded while toggling exposure off, instead of overloading "empty host" as the off switch. |
| `spec.host` | The **domain name** the API/UI (:4800) is exposed at (e.g. `zetta.oramadev.net`) when `ingressEnabled` is `true` — the whole host proxies to the node's `:4800`, so root `/` is the node's **web dashboard** and other paths are MCP/REST/serving. Requires an ingress controller. Names the endpoint; `ingressEnabled` is the on/off switch. |
| `spec.ingressClassName` | `nginx` (default) or `zetta-tunnel`: which ingress serves the cluster's Ingress; DNS follows ([`infrastructure/ingress.md`](infrastructure/ingress.md)). |
| `spec.dnsSecretRef.name` | With `ingressEnabled` + `spec.host`, create a **proxied Cloudflare DNS record** (host → ingress LB) as an owned `CloudflareDNSRecord` CR, using the named Secret (keys `token`/`zoneID`/`accountID`) — normally the same `cloudflare-api-token` Secret the `OramaCloudV1` uses. |
| `spec.capacity.karpenter.enabled` | **Default `false`. Opt-in elastic node capacity** ([operator#69](https://github.com/oramasearch/orama-operator/issues/69), first slice). When `true`, the operator owns a Karpenter **`NodePool`** and pins the Zetta pods onto it (a dedicated taint `orama.cloud/zetta:NoSchedule` + toleration and the `orama.cloud/zetta-pool` nodeSelector), so raising `spec.replicas` past current node capacity leaves pods **Pending → Karpenter provisions matching nodes just-in-time →** the new pods schedule and join the CRDT cluster. Scheduling-driven scale-up; the Zetta-signal-driven trigger (`orama_control.scale_requests`) is a later slice. |
| `spec.capacity.karpenter.nodeClassRef` | The provider **NodeClass** Karpenter launches nodes from — `{group, kind, name}` (the Karpenter v1 `nodeClassRef` shape). Defaults `group: karpenter.k8s.aws`, `kind: EC2NodeClass` (EKS); on AKS NAP set `group: karpenter.azure.com`, `kind: AKSNodeClass`. **`name` is REQUIRED when enabled** — the operator owns only the cloud-neutral NodePool; the NodeClass (AMI/subnet/IAM on AWS) is platform-owned. Missing name ⇒ `CapacityReady=False, Reason=Blocked`. |
| `spec.capacity.karpenter.capacityType` / `.arch` | `on-demand` (**default**, stateful-safe) \| `spot`; `amd64` (**default**) \| `arm64`. Spot only with enough replicas to tolerate an interruption (a reclaim evicts a peer). |
| `spec.capacity.karpenter.instanceTypes[]` | Optional pin via the portable `node.kubernetes.io/instance-type` label. Empty (recommended) ⇒ Karpenter chooses the cheapest fitting type from the NodeClass offerings. |
| `spec.capacity.karpenter.limits` | `{cpu, memory, nodes}` — the **cost/scale guardrail**; Karpenter stops provisioning once any cap is hit. Defaults `cpu: "16"`, `memory: 64Gi`, `nodes: 3`. |
| _(disruption / quorum-safety)_ | The generated NodePool is **stateful-safe**: `consolidationPolicy: WhenEmpty` (never evict a live peer for underutilization), a `nodes: "1"` disruption budget (cap churn), `expireAfter: Never`, `terminationGracePeriod: 1h`; the Zetta pods carry `karpenter.sh/do-not-disrupt` and a `maxUnavailable: 1` PodDisruptionBudget. Operator-driven drain-safe **scale-down** is a later slice. |
| _(Karpenter absent)_ | If the `karpenter.sh/v1 NodePool` CRD isn't installed (bare metal / no autoscaler), enabling capacity is a **graceful no-op**: `CapacityReady=False, Reason=KarpenterNotInstalled`, no crash-loop. `spec.replicas` still packs more pods onto the fixed host until full. Or set `install: true` (below) to have the operator install Karpenter itself. |
| `spec.capacity.karpenter.install` | **Default `false`. Opt-in — make the operator OWN the Karpenter install** ([operator#75](https://github.com/oramasearch/orama-operator/issues/75)). When `true`, the operator server-side-applies the pinned upstream Karpenter controller (Deployment + ServiceAccount + RBAC) and the Karpenter CRDs (`NodePool`/`NodeClaim`/`EC2NodeClass`) as owned resources (modelled as `unstructured` — **no Karpenter Go types vendored**; version pinned to **v1.13.0** under `internal/controller/assets/karpenter/`). **Cloud-gated:** a clean no-op on bare-metal/k0s (`Reason=BareMetalNoCloud`) and on AKS, where **Node Auto Provisioning already manages Karpenter** (`Reason=ManagedByAKSNAP` — use `enabled`, not `install`); the self-install path targets **EKS**. Surfaced via the `KarpenterInstalled` condition. |
| `spec.capacity.karpenter.controllerRoleARN` | **The cloud IAM identity the operator CANNOT mint** — a cloud-account prerequisite, same class as the image pull secret. The AWS IAM role ARN Karpenter's **controller** assumes (via IRSA — annotated onto its ServiceAccount) to create/terminate EC2 instances. **Required when `install` on EKS** (unless wired out-of-band, e.g. EKS Pod Identity); empty ⇒ `KarpenterInstalled=False, Reason=CloudIdentityMissing` and **the controller is not deployed** (no crash-loop). This is the *controller* identity; the launched *nodes* get their own instance profile via the platform-owned `nodeClassRef`. See [`docs/karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md). **Validated** ([operator#79](https://github.com/oramasearch/orama-operator/issues/79)): pattern `^arn:aws(-cn|-us-gov)?:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_\-/]{1,512}$`, `maxLength` 550 — the API server refuses anything else, and the reconciler re-checks (`KarpenterInstalled=False, Reason=InvalidInstallSpec`). Set on the parsed ServiceAccount as a structured annotation, never pasted into YAML. |
| `spec.capacity.karpenter.installClusterName` | The AWS EKS cluster name Karpenter's controller uses (`settings.clusterName`) to discover the cluster's tagged subnets/security-groups/interruption queue. **Required when `install` on EKS**; empty ⇒ `KarpenterInstalled=False, Reason=InstallClusterNameRequired`. **Validated** (operator#79): the EKS cluster-name rule `^[0-9A-Za-z][A-Za-z0-9\-_]{0,99}$`, `maxLength` 100 — refused at admission, and re-checked by the reconciler (`InvalidInstallSpec`). Set on the parsed Deployment as the controller's `CLUSTER_NAME` env value, never pasted into YAML. |
| `KarpenterInstalled` condition | Present only when `install` is set. `InvalidInstallSpec` (a stored `installClusterName` or `controllerRoleARN` fails the pattern; nothing is applied, on any cloud) / `BareMetalNoCloud` / `ManagedByAKSNAP` / `UnsupportedCloud` / `CloudIdentityMissing` / `InstallClusterNameRequired` (gated or blocked) → `InstallProgressing` (applied, controller not yet Available) → `Installed` (controller available in `kube-system`). Written **only on change** (anti-flap). |
| `status.phase` / `.readyNodes` / `.totalNodes` | Readiness roll-up. `kubectl get ozc`. |
| `status.provisionedNodes` / `CapacityReady` condition | Number of nodes Karpenter has provisioned for this cluster (`karpenter.sh/nodepool=<pool>`), plus a `CapacityReady` condition (`Disabled`-dropped / `Blocked` / `KarpenterNotInstalled` / `NodePoolProgressing` / `NodePoolReady`). Written **only on change** (anti-flap). |
| `status.reachability` / `.ephemeral` | The **effective** reachability type of the slice (a print column) + whether it's ephemeral — echoed from spec (`reachability` forced to `dial-out-only` when `ephemeral`). |
| `status.wanAddress` | The routable rendezvous address (`host:port`) siblings dial OUT to when this slice is **wan-accessible** — copy it into a sibling's `spec.externalPeers`, and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)) is populated from the wan-accessible set (its HTTP origin is the same host at `:4800`). Empty for a `dial-out-only` slice or until the address is known. Shown by `kubectl get ozc -o wide`. |
| `status.ignoredEnvVars[]` | Sorted, de-duplicated names from `spec.env` that collided with operator-managed variables and were not added to the container. This observation does not degrade cluster readiness and is written only when it changes. |
| `PeerAddressesValid` condition | Whether every peer address the slice uses is a valid `host:port` ([operator#279](https://github.com/oramasearch/orama-operator/issues/279)). `True`/`Valid` normally. `False`/`InvalidSpecPeerAddress`: an invalid `spec.externalPeers` or `spec.wanAddress` — the StatefulSet is not updated until the spec is fixed and `status.phase` is `Degraded`. `False`/`InvalidTopologyPeerDropped`: rows read from the shared `orama_control.topology` table carried invalid addresses; they were dropped (quoted in the message) and the cluster reconciles with the remaining peers. Left unchanged on a round where the topology could not be read, so a flaky read does not flap it — except a stale `InvalidSpecPeerAddress`, which clears as soon as the spec is valid. Values in the message are Go-quoted (no raw newlines). |

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
| `spec.guardTable` | A `public` table that must exist for the Job to skip the apply. Default `crawler_configs`. When the schema file records migrations (`INSERT INTO schema_migrations (version) VALUES ('NNNN_…')`), the Job also requires every one of them in the database's `schema_migrations`, and otherwise applies the whole idempotent file ([#271](https://github.com/oramasearch/orama-operator/issues/271)). With no `schema_migrations` table, or a file that records none, the guard alone decides. |
| `spec.image` | psql-carrying image for the migration Job. Default `postgres:16-alpine`. |
| `status.phase` / `.appliedVersion` | `Pending \| Applying \| Applied \| Failed` + the version actually applied. `kubectl get pgschema`. |
| `status.conditions[Applied]` | `Applied` \| `Applying` \| `ReplacingMigrationJob` \| `WaitingForDatabase` \| `MigrationFailed`. Written only on a real transition (anti-flap). |

## `NetworkSandbox` (separate CRD, namespaced) — default-deny with explicit paths

Short name: `netsandbox`. One resource describes the network boundary of its owner's pods.
Required `spec.podSelector` identifies those pods; explicit `{}` is appropriate only in a fully
owned workspace namespace. The controller always renders `NetworkPolicy/default-deny`, selecting
only those pods for both ingress and egress with no rules. It also always renders the owner-scoped
`CiliumNetworkPolicy/deny-cloud-metadata`, denying all egress to `169.254.169.254/32`,
`168.63.129.16/32` and `fd00:ec2::254/128`, regardless of other allows. Cilium v2 API and dataplane
support are required. Each allowance selector is intersected with the owner selector, including
Cilium endpoint selectors. `spec.policies` lists exceptions expressible as Kubernetes NetworkPolicy;
each item has a unique `name`, a required human `comment` identifying
the component and reason, and the complete `networking.k8s.io/v1` policy `spec`. The comment is
copied to the rendered policy as `orama.cloud/network-policy-comment`.

`spec.ciliumPolicies` has the same name/comment contract and a Cilium v2 `spec`. It is only for a
path Kubernetes NetworkPolicy cannot express and is empty for ordinary pod, namespace, CIDR and
port rules. The mandatory metadata deny is generated even when this field is empty, and its name
is reserved: declarations cannot override it. `spec.policyLabels` is copied to all rendered policies
so a parent can preserve its existing ownership labels while the sandbox adopts live policies.
`spec.mesh.enrolled` enrols every selected pod in zetta-tunnel's mesh: each declared Connection
allowance (`connection-…`) then renders only TCP 15008, the mesh's mutual-TLS port, and the node
agent captures the pods ([`infrastructure/mesh.md`](infrastructure/mesh.md)). A Lake workspace's
comes from `spec.workspaces.overrides.<id>.mesh`.

The controller owns the rendered policies, removes an owned policy no longer declared, and refuses
to take over a policy controlled by another resource. It reports `status.observedGeneration`, the
ordinary and Cilium policy counts (including mandatory denies), and `conditions[Ready]`. Stable
reconciliation writes no status. Owner watches repair both kinds; owner references garbage-collect
them with the sandbox. Details and the AKS live observation are in
[`infrastructure/network-sandbox.md`](infrastructure/network-sandbox.md#mandatory-cloud-metadata-deny).

## `Connection` (separate CRD, namespaced) — one declared workload path

Short name: `conn`. Lake's guarded Postgres intake creates one resource in the owning workspace
namespace for each `network_connections` row. Users and tenant workloads do not write this kind.
The controller resolves Lake IDs to operator-owned pod labels, writes paired ingress and egress
allowances into that workspace's `NetworkSandbox`, and renders them synchronously. The sandbox,
not the `Connection`, owns the resulting `NetworkPolicy` objects. Details:
[`infrastructure/connections.md`](infrastructure/connections.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` | Cluster-scoped Lake that owns and materialized the resource. Immutable. |
| `spec.connectionId` | Immutable UUID of the Lake `network_connections` row. |
| `spec.customerId` / `.workspaceId` | Immutable Lake ownership UUIDs. The resource must be in the reconciled namespace of that workspace. |
| `spec.sourceKind` / `.sourceId` | Source identity: `app \| service \| agent \| tunnel` plus its Lake UUID. Apps resolve through their existing `LakeApp` workload identity; services and agents use operator-owned ID labels. |
| `spec.targetKind` / `.targetId` | Destination identity: `service \| app` plus its Lake UUID. Selection is restricted to the CR's namespace. |
| `spec.ports[]` | Destination TCP ports, each 1–65535. Empty resolves to the target's Kubernetes Service target ports, including named ports; it never means all pod ports. |
| `spec.credentialScope` | Optional, app to service only: `{access: read \| write, schema?}`. The operator keeps one role for this connection in the target's datastore (Postgres first) with exactly these grants and writes its credential as a workspace secret ([operator#469](https://github.com/oramasearch/orama-operator/issues/469)). |
| `spec.desired` | `present \| absent`. Absent removes both rendered sides, and drops any credential role, before status becomes `Deleted`. |
| `spec.generation` | Lake row generation, minimum 1. A new generation of a credential-scoped connection rotates its credential. |
| `status.phase` / `.message` | `Pending \| Ready \| Failed \| Deleted` and a stable explanation. |
| `status.observedGeneration` | Lake generation represented by the status and returned to Postgres. |
| `status.credentialSecret` | `lake-secret://<mount id>/connection-<id>`: the workspace secret holding the issued credential, returned to Postgres as `credential_secret`. Never the credential. |
| `status.credentialRole` / `.credentialGeneration` | The datastore role issued for this connection and the generation it was issued for. |
| `status.conditions[Ready]` | True only after the paired policies have been rendered. Stable reconciliation does not rewrite it. |

## `EgressRule` (separate CRD, namespaced) — one outside destination

Short name: `egr`. Lake's guarded Postgres intake creates one resource per `network_egress_rules`
row in the owning workspace namespace (`egress-<row id>`); the workspace reconciler keeps one more,
`internet-https`, for a workspace whose `internetEgress` override is on. Users and tenant workloads
do not write this kind. The controller adds the rule's allowances to the workspace `NetworkSandbox`
and, for a DNS name, keeps the workspace's zetta-tunnel egress gateway. Details:
[`infrastructure/egress-rules.md`](infrastructure/egress-rules.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` | Cluster-scoped Lake that owns the resource. Immutable. |
| `spec.ruleId` | Immutable UUID of the Lake row. Empty only for the operator's `internet-https` rule. |
| `spec.customerId` / `.workspaceId` | Lake ownership UUIDs. |
| `spec.sourceKind` / `.sourceId` | `app \| service \| agent` and its Lake UUID, set together. Unset: every workload in the workspace. |
| `spec.cidr` | An outside range, enforced by policy. Loopback, link-local and cluster addresses inside it are always excepted; a range wholly inside them is refused. |
| `spec.dnsName` | An exact lower-case host name, enforced by the egress gateway; the source may reach only the gateway. |
| `spec.publicInternet` | Every public address, as `internetEgress` has always allowed. Only the operator's own rule. |
| `spec.ports[]` | 1 to 64 distinct TCP ports. |
| `spec.desired` | `present \| absent`. Absent removes every allowance and re-renders the gateway snapshot before `Deleted`. |
| `spec.generation` | Lake row generation, minimum 1. |
| `status.phase` / `.message` | `Pending \| Ready \| Failed \| Deleted`; a Ready name rule's message names the gateway address. |
| `status.observedGeneration` | Lake generation represented by the status and returned to Postgres. |

Exactly one of `cidr`, `dnsName` and `publicInternet` is set (a CEL rule on the CRD).

## `Tunnel` (separate CRD, namespaced) — one person, one target, for a time

Short name: `tun`. The abstract kind (principle 10): Lake's guarded intake creates one per
`network_tunnel_grants` row in the workspace namespace, named `tunnel-<grant id>`. Users and tenant
workloads do not write it. The controller picks the concrete kind from `spec.providerRef`, keeps
exactly that child, mirrors its status, and on expiry or revocation deletes the child before
reporting `Expired` or `Revoked`. Details: [`infrastructure/tunnels.md`](infrastructure/tunnels.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` / `.grantId` | The Lake and the grant row id. Immutable. The grant id is the identity in the developer's certificate. |
| `spec.customerId` / `.workspaceId` | Immutable Lake ownership UUIDs. |
| `spec.userId` | The Lake user the grant is for. |
| `spec.targetKind` / `.targetId` / `.port` | `service \| app`, its Lake UUID and the target Service's port (1–65535). Nothing else is reachable. |
| `spec.expiresAt` / `.revokedAt` | The grant ends at `expiresAt`, or at once when `revokedAt` is set. |
| `spec.clientCAPEM` | Lake's per-customer CA (public), from `client_ca_pem` (Lake 0085). Required by `ZettaTunnel`. |
| `spec.subject` | The person's e-mail address, set only for a `cloudflare` provider (Cloudflare Access). |
| `spec.providerRef` | A `TunnelProvider` in the namespace. Empty: Orama's own `ZettaTunnel`. |
| `spec.generation` | Lake row generation, minimum 1. |
| `status.phase` / `.message` | `Pending \| Ready \| Expired \| Revoked \| Failed`. |
| `status.provider` | `ZettaTunnel` or `CloudflareTunnel`. |
| `status.endpoint` / `.relayCAPEM` | What the client connects to, and the relay's CA, only while `Ready`; returned to `network_tunnel_grant_status`. |
| `status.observedGeneration` / `.conditions[Ready]` | Lake generation acknowledged; comparison-gated. |

## `TunnelProvider` (separate CRD, namespaced) — the vendor, account and credentials behind tunnels

Short name: `tunprov`. A `TunnelProvider` named `default` in a workspace namespace is the provider
for that workspace's Lake-granted tunnels. CEL rules require `spec.cloudflare` for type `cloudflare`
and forbid it for `zetta`. The controller reports `conditions[Ready]` (the token Secret and key
exist); it never calls the vendor.

| Field | Meaning |
|---|---|
| `spec.type` | `zetta` (Orama's own zetta-tunnel) or `cloudflare` (the customer's own account only). |
| `spec.customerId` | The Lake customer it belongs to. A `Tunnel` of another customer is refused. |
| `spec.cloudflare.accountId` / `.zoneId` / `.domain` | The customer's account, zone and the domain under which hostnames `tunnel-<grant id>.<domain>` are made. |
| `spec.cloudflare.apiTokenSecretRef` | `{name, key}` of a Secret in the namespace: the customer's API token (Cloudflare Tunnel edit, Access apps and policies edit, DNS edit). Never written or logged by the operator. |

## `ZettaTunnel` (separate CRD, namespaced) — a tunnel carried by zetta-tunnel

Short name: `ztun`. Created and owned by a `Tunnel`. Runs the grant's zetta-tunnel endpoint in the
workspace (Secret, Service and Deployment `tunnel-<grant id>`), the generated
`Connection/connection-tunnel-<grant id>` from the endpoint to the target, and the workspace sandbox
allowance admitting only the relay. Every `ZettaTunnel` feeds the cluster's one relay in namespace
`zetta-tunnel`. A finalizer removes the allowance, the endpoint and the Connection before it goes.

| Field | Meaning |
|---|---|
| `spec.lakeRef`, `.grantId`, `.customerId`, `.workspaceId`, `.targetKind`, `.targetId`, `.port`, `.expiresAt` | The grant, copied from the `Tunnel`. |
| `spec.clientCAPEM` | The CA(s) the relay trusts for this grant's developer certificates. |
| `status.phase` / `.message` | `Pending \| Ready \| Expired \| Failed`. |
| `status.endpoint` / `.relayCAPEM` | The relay's public `host:443` and its server CA, while `Ready`. |
| `status.target` | The in-cluster `host:port` the endpoint is fixed to. |

## `CloudflareTunnel` (separate CRD, namespaced) — a tunnel carried by the customer's Cloudflare account

Short name: `cftun`. Created and owned by a `Tunnel` whose provider is of type `cloudflare`. Makes a
remotely managed tunnel, a CNAME and an Access application admitting only `spec.subject` in the
customer's account, and runs `cloudflared` in the workspace with the generated Connection to the
target. A finalizer deletes those Cloudflare objects with the customer's token.

| Field | Meaning |
|---|---|
| grant fields | As on `ZettaTunnel`. |
| `spec.providerRef` | The `cloudflare` `TunnelProvider`. |
| `spec.subject` | The one e-mail address Cloudflare Access admits. |
| `status.endpoint` | The hostname `tunnel-<grant id>.<domain>`. |
| `status.tunnelId` / `.dnsRecordId` / `.accessApplicationId` | The Cloudflare objects made, deleted exactly at teardown. |

## `PeerConnection` (separate CRD, namespaced) — one end of a peer

Short name: `peer`. Named services exchanged with another cluster or network through zetta-tunnel's
peer gateway (decision D6); no address is routed. Lake's guarded intake creates one per
`network_peers` row (`peer-<row id>`); Orama's own stacks write their own. Details:
[`infrastructure/peer-connections.md`](infrastructure/peer-connections.md).

| Field | Meaning |
|---|---|
| `spec.peerId` | The identity both ends share (the Lake row id). Immutable. |
| `spec.lakeRef` / `.customerId` / `.workspaceId` | Set for a Lake peer. |
| `spec.remote` | The other gateway's `host:port`. Empty: the address the `PeerLink` passed in the credentials Secret. |
| `spec.exports[]` | `{name?, targetKind: service \| app \| kubernetesService, targetId \| serviceName, port}`. A CEL rule requires `serviceName` for `kubernetesService`, else `targetId`. Empty `name`: the target Service's name. |
| `spec.imports[]` | `{name, port}`: the other end's export names (lowercase DNS-style, never an address). Each becomes Service `peer-<name>` here. |
| `spec.credentialsSecretName` | Default `<name>-credentials`; written only by a `PeerLink`. |
| `spec.pullSecretName` | The gateway's image pull secret; a Lake peer uses the Lake's. |
| `spec.desired` / `.generation` | `present \| absent` (default `present`) and the row generation (default 1). |
| `status.phase` / `.message` | `Pending \| Ready \| Failed \| Deleted`; the message lists where each import is reachable. |
| `status.address` / `.identity` | This gateway's public `host:7446` and certificate identity. |
| `status.imports[]` / `.exports[]` | Resolved Services and targets. |

## `PeerLink` (separate CRD, cluster-scoped) — credentials for a pair, through the managing cluster

Short name: `peerlink`. Lives on the managing cluster. Mints the pair's CA once, one certificate per
end (`<end>.<peerId>.peer.orama.internal`, client and server), and writes each end's credentials
Secret on its cluster with the other end's identity and address. Deleting it deletes both Secrets.

| Field | Meaning |
|---|---|
| `spec.peerId` | The pair's peer id. Immutable. |
| `spec.ends[2]` | `{name, clusterRef?, namespace, peerConnection}`; `clusterRef` empty is this cluster. The two names differ (CEL). |
| `status.phase` | `Ready` once both Secrets are current and both ends report an address. |
| `status.ends[]` | `{name, identity, written, address, message}`. |

## `ClerkApplication` (separate CRD) — Clerk auth per environment

Provisions a **Clerk application per environment** (dev/testing/staging/prod) via the Clerk
**Platform API** (`POST /v1/platform/applications`), so Studio auth ([zetta#889](https://github.com/oramasearch/zetta/issues/889))
gets its issuer + keys with no manual dashboard work. One CR per environment; the resulting
publishable/secret keys + `CLERK_ISSUER` land in an owned Secret. Template: `CloudflareKVNamespace`.
Design: [orama-operator#116](https://github.com/oramasearch/orama-operator/issues/116).

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
PodDisruptionBudget and topology spread. A spec change rolls the Deployment once. A ReadWriteMany
store surges with `maxUnavailable: 0`. A single-replica Lake on a ReadWriteOnce store recreates by
default and surges onto the running pod's node only with `spec.rollout.overlapOnSharedStore` (see
`docs/lake/README.md`, Rollouts). A Lake finalizer starts and waits for Zetta's
ordered cleanup before releasing the parent. With `spec.ingressEnabled` and `spec.host` it fronts the
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
| _(workload hardening — automatic)_ | The pod and container require non-root uid/gid 10001, use fsGroup 10001 with `fsGroupChangePolicy: OnRootMismatch`, disallow privilege escalation, drop all Linux capabilities, and use `RuntimeDefault` seccomp. Readiness is `GET /readyz` every 5 s, a startup probe on `GET /healthz` allows five minutes before liveness applies, the container gets a 15-second kubelet `sleep` pre-stop drain, and the pod gets a 45-second termination grace period. A ReadWriteMany store or a multi-replica Lake rolls with `maxSurge: 1`, `maxUnavailable: 0`. A single-replica Lake on any other store uses `Recreate` unless `spec.rollout.overlapOnSharedStore` is set; a ReadWriteOncePod claim always uses `Recreate`. More than one replica creates `PodDisruptionBudget lake-<name>` with `maxUnavailable: 1`; scaling to one deletes it. |
| `spec.store` | Persistent libliquid customer store mounted read-write at `/var/lib/lake/store`, with `LAKE_STORE_ROOT` fixed to that path. Without `existingClaim`, the operator creates and owns `lake-<name>-store` once. Later store spec changes never mutate, replace, or delete it. |
| `spec.store.size` / `.storageClassName` / `.accessModes` | Creation-time settings for the generated claim: quantity `100Gi` by default, cluster-default storage class, and `[ReadWriteMany]`. RWX is the multi-replica default; `ReadWriteOnce` requires `spec.replicas: 1`. |
| `spec.store.existingClaim` | PVC in `spec.namespace` to mount instead of creating one. It is referenced without adoption, mutation, or deletion. |
| `spec.rollout.overlapOnSharedStore` | Default `false`. A single-replica Lake on a ReadWriteOnce claim (or a claim that does not exist yet) rolls with `Recreate`, which leaves a restart gap. `true` makes it surge instead (`maxSurge: 1`, `maxUnavailable: 0`) and adds required pod affinity on `kubernetes.io/hostname` to its own pods, so the new pod mounts the volume beside the old one. During that overlap two lake processes open one store root, so set it only once that has been proven safe (see `docs/lake/README.md`, Rollouts). It has no effect on a ReadWriteMany claim or a multi-replica Lake, which always surge, or on a ReadWriteOncePod claim, which always recreates. Toggling it changes the pod template once. |
| `spec.env[]` | Kubernetes `EnvVar` entries appended after operator env. `value` and `valueFrom` are supported. Conflicts with `LAKE_ADMIN_TOKEN`, `LAKE_APPS_RUNTIME_IMAGE`, `LAKE_METHODOLOGY_RUNTIME_IMAGE`, `LAKE_INSTANCE_KEK`, `LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`, `LAKE_DATA_PLANE_URL`, `LAKE_DATA_PLANE_TOKEN`, `LAKE_INTERNAL_URL`, `LAKE_SEALING_PUBLIC_KEY`, `LAKE_SEALING_KID`, `LAKE_SERVICE_DIR`, or `LAKE_STORE_ROOT` are ignored so operator values win. `LAKE_APPS_WILDCARD` is a compatibility exception: the last explicit entry wins over `spec.apps.wildcard` and is rendered once. Use `spec.env` for Lake runtime settings such as `LAKE_LOG_MODE`, `LAKE_LOG_WITNESSES`, and `LAKE_PUBLIC_HOST`. |
| `spec.sealing.generation` | Desired X25519 sealing-key generation (default 1). Increasing it rotates; decreasing it is refused. Old private keys remain while any `LakeSealedSecret` record names them. |
| `spec.postgres` | **lake's control plane**: a CloudNativePG `Cluster` named `lake-<name>-controldb` in `spec.namespace`, the same path `OramaCloudV1` uses for its control DB. `enabled` (default `true`; `false` tears it down, deleting its data), `instances` (default 1), `storageSize` (default `50Gi`; CNPG grows the PVC in place when the StorageClass allows expansion), `storageClassName`. CNPG generates the `lake-<name>-controldb-app` Secret; the lake container gets `LAKE_CONTROL_DB_URI` and `LAKE_CONTROL_DB_PASSWORD` from it (optional refs, so the pod starts before the DB does). Requires the CNPG operator (helmfile). |
| `spec.zetta` | **lake's data plane**: an owned `OramaZettaCluster` named `lake-<name>-zetta` in `spec.namespace`. `enabled` defaults to `true`; `false` deletes the cluster and its per-node data volumes. `image` (empty uses the operator default in `internal/controller/lake_defaults.go`), `replicas` (default 1), `storage.size` (Zetta default `10Gi`), `storage.storageClassName`, `env` (followed by the operator's default Zetta env for each name it does not set: `ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864`, `MALLOC_ARENA_MAX=2`), and `reachability` (`dial-out-only` by default or `wan-accessible`) map to the child CR. The child inherits the Lake's `pullSecretName`. It sets `provisionOperatorSchema: true`: the cluster is also the Lake's deployment control plane, the one operator-schema Zetta in the Lake namespace (operator#557; managed services are still requested through the control Postgres). The lake container gets `LAKE_DATA_PLANE_URL=http://lake-<name>-zetta-zetta.<namespace>.svc.cluster.local:4800` and `LAKE_DATA_PLANE_TOKEN` from Secret `lake-<name>-zetta-zetta-admin`, key `token`; the token reference is required, so kubelet waits for the Zetta controller to mint it. With a public host (`spec.host`, or `LAKE_PUBLIC_HOST` in `spec.env`) it also gets the same URL and Secret reference as `LAKE_OPERATOR_ZETTA_URL` and `LAKE_OPERATOR_ZETTA_TOKEN`, and `LAKE_PUBLIC_HOST=<spec.host>` when `spec.env` does not state it; all three are reserved. See [Lake deployment environments](lake/environments.md#every-lakes-control-plane-is-its-own-zetta). |
| `spec.postgres.backup` | Opt-in (`enabled`, default `false`) CNPG Barman object-store backup. `schedule` defaults to six-field `0 0 3 * * *`; `retention` defaults to `30d`; `destination.bucket` and `secretRef.name` are required when enabled. `destination.path` defaults to `lake-<name>/controldb`; `endpointURL` defaults to the Secret's `s3ApiEndpoint`. The Secret must be in `spec.namespace` and contain `s3ApiEndpoint`, `accessKeyId`, and `secretAccessKey`. The operator configures gzip WAL compression and creates owned `lake-<name>-controldb-backup` `ScheduledBackup` resources. See [`lake/backups.md`](lake/backups.md). |
| _(admin token — automatic)_ | Secret **`lake-<name>-admin`** (key `token`, 64 hex), minted once and preserved, owned by the Lake; injected as `LAKE_ADMIN_TOKEN`. lake requires it as `Authorization: Bearer` on every `/v1` capability except `server_info` and on MCP tool calls. Retrieve: `kubectl get secret lake-<name>-admin -n <ns> -o jsonpath='{.data.token}' \| base64 -d`. |
| _(sealing key — automatic)_ | Secret **`lake-<name>-sealing-key`** holds the current and still-needed prior X25519 private keys. Only the active public key and `kid` are injected into lake. See [`lake/sealed-secrets.md`](lake/sealed-secrets.md). |
| _(schema — automatic)_ | With Postgres on, the Lake owns a `lake-<name>-controldb-migrations` ConfigMap holding lake's control-plane schema (vendored byte-identical from `oramasearch/lake` `db/schema.sql`) and a `lake-<name>-controldb-schema` **`PostgresSchema`** that applies it in one transaction, guarded on the newest migration table (`workspace_volumes_guard` for `0069_workspace_volumes`) plus every migration the file records being present in `schema_migrations`, and versioned by a hash of SQL plus guard. Schema completion gates Deployment creation and updates. Re-vendor lake `db/schema.sql` unchanged, update the pinned source and guard, and run the checks in [the schema guide](lake/schema.md). Incremental migrations remain #126. |
| `spec.ingressEnabled` | **Default `false`. The explicit external-exposure gate.** With `spec.host`, provisions the nginx Ingress (+ the DNS record below if `dnsSecretRef` is set). `false` tears both down even if `host` is still set. The Ingress permits `64m` request bodies and disables request/response buffering for Lake's streaming transfers. |
| `spec.tls.secretName` | Optional `kubernetes.io/tls` Secret in `spec.namespace`. With ingress enabled and a host set, renders the Ingress TLS host and enables nginx HTTPS redirect. Empty is the backward-compatible HTTP-only default. |
| `spec.host` | Domain the lake is exposed at (e.g. `hello.lake.live`) when `ingressEnabled` is `true`. |
| `spec.ingressClassName` | `nginx` (default) or `zetta-tunnel`: which ingress serves the Lake's Ingress and its environments'. DNS follows, because it is written from the Ingress's address. The per-cluster cutover sets it in the cluster's set member ([`infrastructure/ingress.md`](infrastructure/ingress.md)). |
| `spec.dnsSecretRef.name` | With `ingressEnabled` + `host`, creates a **proxied Cloudflare DNS record** (host → ingress LB) as an owned `CloudflareDNSRecord`, once the ingress controller has assigned an address (the owned Ingress's status change re-queues the Lake). The Secret (keys `token`/`zoneID`) must live in `spec.namespace` and hold the zone of `host` — a `lake.live` host needs a `lake.live`-zone Secret, not the `oramadev.net` one the Zetta clusters use. |
| `spec.region` | `{name, label}`: where the Lake runs, for its region picker (decision 64). Rendered as `LAKE_REGION` and `LAKE_REGION_LABEL`. In a LakeSet, `name` comes from the member's cluster ([`lake/README.md#regions`](lake/README.md#regions)). |
| `spec.regions` | The deployment's regions as the picker offers them, `[{name, label, host, latitude, longitude}]`, rendered as `LAKE_REGIONS` (JSON). A LakeSet computes it from its members. |
| `spec.apexHost` | A second host the Lake answers (the region picker, e.g. `lake.live`). With `ingressEnabled` + `host`, adds an Ingress rule (and TLS host) and, with `dnsSecretRef`, an owned proxied `<lake>-apex-dns` record; rendered as `LAKE_APEX_HOST`. Removing it removes both. |
| `spec.clerk.secretKeySecretRef.name` | A Secret in `spec.namespace` holding a Clerk instance's Backend API secret key (key `secretKey`, optional `publishableKey`). The operator keeps the instance configured for lake through the Backend API (the `lake` JWT template, the host and apex as allowed origins, a webhook endpoint at `https://<host>/identity/clerk-webhook`), writes a production instance's DNS records as owned, unproxied, `Retain` `CloudflareDNSRecord`s with `dnsSecretRef`, and renders the reserved `LAKE_CLERK_PUBLISHABLE_KEY` and `LAKE_CLERK_WEBHOOK_SECRET` from the owned Secret `lake-<name>-clerk`. Condition `ClerkReady`. Without `spec.clerk` nothing changes. See [`lake/clerk.md`](lake/clerk.md) ([operator#541](https://github.com/oramasearch/orama-operator/issues/541)). |
| `spec.previewWildcard` | DNS suffix for preview environments. `preview.example.com` yields `<change-short>.preview.example.com`; with Lake ingress and DNS enabled, the Lake owns one `*.preview.example.com` Cloudflare record and previews do not create records per change. |
| _(every feature — no switch)_ | **Every `Lake` runs every feature** ([operator#539](https://github.com/oramasearch/orama-operator/issues/539)): apps, builds, methodology runs and deployments, agents (the whole Agent Library and user-created launches and schedules), managed services and stores, workspaces, connections, egress rules, tunnels, peers and volume snapshots. There is no per-Lake `enabled` field; the old `apps`, `builds`, `services`, `workspaces` and `snapshots` `enabled` fields were removed and the CRD prunes stored values. Every image has an operator default, one table in `internal/controller/lake_defaults.go` (listed in [`lake/README.md`](lake/README.md#default-images)); a spec image overrides its default. The one prerequisite is the control Postgres (`spec.postgres.enabled`, default `true`): without it, condition `FeaturesReady` is `False` with reason `PostgresDisabled` and names the features that cannot run. |
| `spec.apps.runtimeImage` | Private app-runtime image used by `LakeApp` resources in runtime mode. Empty uses the operator default (`internal/controller/lake_defaults.go`). Injected into lake as the reserved `LAKE_APPS_RUNTIME_IMAGE`, which lake uses as the base image for generated app builds. Apps-table synchronization from Lake control Postgres uses `lake_apps` notifications and a 60-second fallback list. |
| `spec.apps.wildcard` | DNS suffix for apps. An app without `spec.host` uses `<app>.<wildcard>`. **Empty uses the Lake's `spec.host`**, so an app is `<app>.<lake host>` (`myapp.lake-dev.oramadev.net`); HTTPS on such two-level names needs a certificate per zone ([#515](https://github.com/oramasearch/orama-operator/issues/515)). The operator injects it into lake as `LAKE_APPS_WILDCARD`; an explicit `spec.env` entry wins and is rendered once. With `dnsSecretRef`, the shared `*.<wildcard>` DNS record is owned by the apps-table sync path. |
| `spec.apps.ingressClass` | Ingress class for apps; empty defaults to `nginx`. |
| `spec.builds` | Every Lake reconciles lake's in-cluster build rows; lake gets its Service address as `LAKE_INTERNAL_URL`, which it uses as the registry authority. Each queued row becomes one isolated rootless BuildKit Job. See [`lake/builds.md`](lake/builds.md). |
| `spec.builds.builderImage` | Private `oramasearch/lake-builder:<tag>` image used for every build. Empty uses the operator default (`internal/controller/lake_defaults.go`). When `spec.pullSecretName` is set, the operator copies that Secret from the Lake namespace to each build namespace as `builder-pull`; the Job references the local copy because image pull Secrets cannot cross namespaces, mounts its `.dockerconfigjson` read-only at `/etc/lake-builder/pull/.dockerconfigjson`, and sets `BASE_REGISTRY_AUTH` to that path for private base-image pulls. With no Lake pull Secret, the auth mount and environment variable are absent. |
| `spec.builds.resources` | Builder container resource override, merged over bounded defaults for CPU, memory, and ephemeral storage. |
| `spec.builds.concurrency.perLake` / `.perSpace` | Active Job caps for the whole Lake and one target registry space. Defaults are 4 and 2; both must be at least 1. |
| `spec.methodologies.runtimeImage` | Private `oramasearch/methodology-runtime` image. Injected into lake as the reserved `LAKE_METHODOLOGY_RUNTIME_IMAGE` (the base of every methodology image build) and run by each methodology run's stage and collect pods. Empty uses the operator default (`internal/controller/lake_defaults.go`). See [`lake/methodology-runs.md`](lake/methodology-runs.md). |
| `spec.methodologies.workVolumeSize` / `.storageClassName` | Each methodology run's work volume: size (default `10Gi`) and storage class (default the cluster's). |
| `spec.builds.cacheEnabled` | Enables a per-space BuildKit registry cache (`<registry>/<space>/cache/<image>`). Defaults to `true`. |
| `spec.deploySync.interval` | How often the operator reads desired deployment events from its Zetta `orama_control.deployments` table and polls every Lake's control rows (`services`, stores, agents, and `builds`). Default `15s`; positive values below `1s` are clamped to `1s`. Workspaces have a fixed 60-second recovery list in addition to `lake_workspaces` notifications. |
| `spec.agents.runtimeImage` | `oramasearch/agent-runtime:<tag>`, the image every launch of a user-created Python agent runs on ([`lake/agents.md`](lake/agents.md), lake#231). Empty uses the operator default (`internal/controller/lake_defaults.go`). Each row requires `workspace_id`; a present launch becomes a Job (`job`) or a Deployment + Service (`service`) `<name>-<slug>` in the joined workspace namespace, with a local credential Secret `<name>-<slug>-credential` whose SHA-256 only is written to `agent_launch_status`. |
| `spec.agents.library[]` | `{key, image}`: overrides the image of one of the Agent Library's specialized agents. **Every Lake runs all nine** on the operator's default images (`internal/controller/lake_defaults.go`); an entry replaces the image of its key and cannot remove an agent. The agents (`agent-ask-orama`, `agent-project`, `agent-autosuggest`, `agent-creator`, and since lake#263 `agent-eval-generator`, `agent-autosuggest-corpus`, `agent-project-corpus`, and since lake#296 `agent-application`, which builds and deploys apps only through lake's capabilities and needs no cluster privileges, and since lake#304 `agent-experiments`, which runs AutoML studies the same way), each a Deployment + Service `<name>-<key>` on 8080; lake gets `LAKE_AGENT_ENDPOINTS`, `LAKE_INTERNAL_URL`, `LAKE_AGENT_MODEL`, `LAKE_AGENT_SERVICE_TOKEN` (Secret `lake-<name>-agent-service-token`, minted once) and `LAKE_AGENT_BUS_DIR` (Secret `lake-<name>-agent-bus`, one agent-bus credential per agent; each agent gets only its own as `AGENT_BUS_TOKEN`). A library agent whose key the operator no longer runs is deleted. |
| `spec.agents.openaiSecretName` | A Secret in `spec.namespace` with `OPENAI_API_KEY`, the general Orama key; people provide it, and the operator copies it into user-agent workspace namespaces. |
| `spec.agents.model` | The frontier model agents default to (default `gpt-5.5`). |
| `spec.agents.jobDeadlineSeconds` | `activeDeadlineSeconds` of a job launch (default 3600, minimum 60). |
| `spec.services` | Managed services, run by every Lake ([`lake/services.md`](lake/services.md), liquid-bridge decision 46); needs `spec.postgres` enabled. Every row requires `workspace_id` and joins the operator-written workspace namespace; there is no Lake-namespace fallback. The operator creates the selected Zetta, Postgres, catalog, SQLite, DuckDB or Polars resource there with Lake, workspace, customer, service and kind labels. The Zetta named by `workspaces.zetta_service_id` is excluded because the workspace reconciler owns its cluster and status. Connections are projected into Lake's trusted `lake-<name>-services` Secret for the control plane. Status is generation guarded. An `absent` row irreversibly removes only resources that pass ownership checks. See the service guide for catalog resources, readiness, credentials and deletion order. |
| `spec.services.maxServices` | Cap on live services for this Lake (`int32`, minimum `0`). `0` is unlimited. A **new** service past the cap creates nothing and is `Failed` with message `service quota reached (<n>)`; it starts on a later poll once there is room. Existing services are never refused or removed. Workspace count has its own `spec.workspaces.maxWorkspaces`. |
| `spec.services.zettaImage` | Image for Zetta services and workspace Zettas. Empty falls back to `spec.workspaces.image`, then the operator default (`internal/controller/lake_defaults.go`). |
| `spec.services.zettaEnv[]` | Kubernetes `EnvVar` entries copied to `spec.env` on every managed Zetta service, including the workspace-owned cluster through the workspace reconciler, followed by the operator's default Zetta env for each name the list does not set (`ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864`, `MALLOC_ARENA_MAX=2`). Changes roll each service cluster's StatefulSet. Reserved-name handling and collision status follow the `OramaZettaCluster` contract. |
| `spec.services.sqliteImage` | Image for SQLite services: [lake-sqlite](lake/lake-sqlite.md), which the operator runs itself. Empty falls back to the operator's `--lake-sqlite-image` flag, which the chart sets from `lakeServices.sqliteImage` (default `oramasearch/lake-sqlite:0.1.1`). The pods pull with the Lake's `pullSecretName`. Changing it rewrites every SQLite service's pod template, rolling each pod once. |
| `spec.services.duckdbImage` | Image for DuckDB services: [lake-duckdb](lake/lake-duckdb.md), powered by DuckDB, which the operator runs itself (one StatefulSet of one pod per service). Empty falls back to the operator's `--lake-duckdb-image` flag, which the chart sets from `lakeServices.duckdbImage` (default `oramasearch/lake-duckdb:0.1.0`). The pods pull with the Lake's `pullSecretName`. Changing it rewrites every DuckDB service's pod template, rolling each pod once. |
| `spec.services.polarsImage` | Image for Polars services: [lake-polars](lake/lake-polars.md), powered by Polars, which the operator runs itself (one StatefulSet of `instances` workers per service). Empty falls back to the operator's `--lake-polars-image` flag, which the chart sets from `lakeServices.polarsImage` (default `oramasearch/lake-polars:0.1.0`). The pods pull with the Lake's `pullSecretName`. Changing it rewrites every Polars service's pod template, rolling each pod once. |
| `spec.workspaces` | Every Lake reconciles lake's control-Postgres `workspaces` rows into isolated `<environment>-ws-<first 12 UUID hex>` namespaces. `lake_workspaces` notifications wake the controller; a complete list every 60 seconds recovers missed notifications. Needs Postgres and waits for the current Lake schema. See [`lake/workspaces.md`](lake/workspaces.md). |
| `spec.workspaces.image` / `.storage` / `.resources` | Workspace Zetta settings. `spec.services.zettaImage` takes image precedence, then `workspaces.image`, then the operator default (`internal/controller/lake_defaults.go`). `storage.storageClassName` is the class of every workspace volume. The size comes from lake: each workspace Zetta is created at its row's `storage_gi` (lake migration `0069`, default 5 GiB), and `storage.size` (default `5Gi`) is **deprecated and ignored** with lake's 0069 schema: it applies only to a row without `storage_gi`, and new workspaces take lake's column default. Change a workspace's size with lake's `set_workspace_storage`. The workspace namespace's ResourceQuota `requests.storage` is the plan's value (headroom for the namespace's other claims) plus `storage_gi` × replicas, so growth is not capped by the plan. When `storage_gi` grows, the operator patches `spec.resources.requests.storage` of each claim `data-<cluster>-zetta-<n>` in place (the StatefulSet's claim template is immutable and is left as created), waits for `status.capacity`, raises the child's `spec.storage.size` to match, and writes `storage_gi_observed`, `storage_phase` (`Pending \| Resizing \| Ready \| Failed`) and `storage_message` on the workspace row. A size below a claim's is ignored and reported; volumes never shrink. A class without `allowVolumeExpansion`, or an API refusal such as a quota, is `Failed` with the reason and does not fail the workspace. See [`lake/workspaces.md`](lake/workspaces.md#workspace-volume-size-operator259). Resources default to a `64Mi` memory request and `1Gi` memory limit. The Lake pull Secret is copied into each workspace namespace. |
| `spec.snapshots.className` / `.driver` | Every Lake takes scheduled CSI snapshots of lake's store volume and of every workspace Zetta's volumes, from lake's `volume_snapshot_policies` (lake migration `0069`, operator#260). The operator creates the `VolumeSnapshotClass` `className` (default `lake-retain`) for `driver` (default `disk.csi.azure.com`) with `deletionPolicy: Retain` when it is absent, and refuses an existing one that is not `Retain`. Each target's claims get a `VolumeSnapshot` `<claim>-<UTC timestamp>` labelled `orama.cloud/snapshot-target`, when the schedule comes due, when there is none yet and when lake's `requested_at` is newer than the last; only `readyToUse` snapshots count toward `keep` and the newest ready one is never deleted; the newest failed one is kept for diagnosis and older failed ones deleted; before a live target's snapshot is pruned its bound content (checked for the driver and its binding) is switched to `deletionPolicy: Delete` so the cloud snapshot goes too; hand-made and other targets' snapshots are never touched; bound contents are labelled with the target; requests and schedules are judged by Postgres's `now()`; a schedule that never fires is reported and takes nothing (a request still works); and `volume_snapshot_status` reports the newest. `lake_volume_snapshots` notifications wake the controller; it also polls every 60 seconds. Requires Postgres and the `snapshot.storage.k8s.io` CRDs. See [`lake/volume-snapshots.md`](lake/volume-snapshots.md). |
| `spec.workspaces.maxWorkspaces` | Cap on running or stopped workspace namespaces. `0` is unlimited. Lowering it never removes an existing namespace. A new row over the cap is `Failed` with `workspace quota reached (<n>)`. |
| `spec.workspaces.defaultPlan` / `.plans` | Named workspace ResourceQuota and LimitRange profiles. Empty `defaultPlan` selects the built-in `default` plan. A custom plan provides `quota`, `defaultRequest` and `default` ResourceLists; missing or incomplete plans fail closed. |
| `spec.workspaces.overrides[uuid]` | Per-workspace `{plan, internetEgress, mesh}`. `mesh: true` enrols the workspace's sandbox in zetta-tunnel's mesh (default `false`; [`infrastructure/mesh.md`](infrastructure/mesh.md)). A missing override uses the default plan and denies internet access. `internetEgress: true` allows only public TCP 443 while excluding private/reserved ranges, metadata, Kubernetes API Service and endpoints, and current Node addresses and Pod CIDRs; the operator carries it as the workspace's `EgressRule/internet-https`. |
| `status.readyReplicas` / `.phase` | Replica count and overall service state: `Progressing \| Ready \| Degraded`. `Ready` requires the current Deployment generation with the desired positive ready/updated/available replica counts, a Bound store, enabled Postgres readiness and current applied schema, enabled Zetta readiness, and valid enabled backup configuration. Replica count alone does not establish readiness. Written only on a real transition. |
| `status.conditions[Ready]` | `LakeReady` on success. Pending work reports `Progressing` with `DeploymentProgressing`, `PostgresNotReady`, `SchemaPending`, `SchemaNotCurrent`, or the store/Zetta reason. Failed schema (`SchemaFailed`), Deployment replica failure (`ReplicaFailure`) or progress deadline (`ProgressDeadlineExceeded`), degraded Zetta, and invalid enabled backup configuration report `Degraded`. Failures take precedence over pending work; messages explain the blocker. Unchanged observations preserve transition times and write no status. |
| `status.conditions[FeaturesReady]` | `True` (`AllFeatures`) when the Lake can run every feature. `False` (`PostgresDisabled`) when `spec.postgres.enabled` is `false`, naming the features that cannot run without the control Postgres. It does not change `phase`. |
| _(`LAKE_MCP_RUNNER_IMAGE`)_ | The image lake runs MCP servers on. The operator injects its default (`internal/controller/lake_defaults.go`) unless `spec.env` sets the variable, whose value is then rendered once. |
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

## `LakeApp` (separate CRD, namespaced) — one deployed app

A `LakeApp` is one Lake-owned application desired state. It lives in the referenced Lake's
namespace. Synchronized workloads run in their workspace namespace; standalone workloads use a
private namespace. See the operational contract in
[`lake/apps.md`](lake/apps.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` | Name of the cluster-scoped `Lake`; the `LakeApp` must live in that Lake's `spec.namespace`. |
| `spec.mode` | `runtime` (default) or `image`. Runtime requires `source`; image requires a digest-pinned `image`. |
| `spec.source.repository` | Lake repository identifier loaded by app-runtime. |
| `spec.source.revision` | Explicit pinned repository revision. It becomes `status.observedRevision` after reconciliation. |
| `spec.source.path` | Application directory within the repository; empty means its root. |
| `spec.source.entrypoint` | Optional framework entrypoint override. Empty omits `APP_ENTRYPOINT`, letting app-runtime read `app.toml`. |
| `spec.source.framework` | `fastapi`, `streamlit`, or `static`. |
| `spec.source.tokenSecretRef` | Standalone app's `{name, key}` selecting its repository read token in the Lake namespace. Synchronized apps use their uniquely named generated token Secret in the workspace namespace instead. |
| `spec.sync` | Operator-managed apps-row identity: immutable `id`, `customerId` and `workspaceId`; the operator-written `workspaceNamespace`; desired `generation`; and optional `tokenHashes` (`source`, nullable `zetta`, `events`, `pull` SHA-256 digests). Requires Lake ownership; token hashes acknowledge successful registration in Postgres before workload startup. Plaintext is never in this field. |
| `spec.image` | Image-mode OCI reference, required to match `name@sha256:<64 hex>`. |
| `spec.pullSecretName` | Image-mode override for the Lake's private-image pull Secret. Synchronized apps merge its Docker auth entries with generated Lake-registry credentials in the workload namespace; standalone apps copy it. |
| `spec.port` | App container port, default `8080`. The Service exposes it on port 80. |
| `spec.replicas` | Desired replicas, default `1`. Zero stops pods while retaining the app resources. |
| `spec.resources` | App-container requests/limits; defaults to `100m`/`128Mi` requests and `1`/`1Gi` limits. |
| `spec.env[]` | Literal `{name, value}` entries. Operator-managed runtime variables take precedence. |
| `spec.secretEnv[]` | `{name, secretKeyRef: {name, key}, lakeSecret?: {mount, name, field}}` entries. Each source Secret is read from the Lake namespace, only selected keys are copied into the isolated app namespace, and the Deployment uses `env[].valueFrom.secretKeyRef`. With `lakeSecret` (lake#625) the controller owns a `LakeSealedSecret` with a lake source that produces `secretKeyRef.name`, so the value comes from lake's secret store and follows its new versions ([apps](lake/apps.md#lake-secret-references-lake625)). Operator-managed names take precedence; secret-backed values take precedence over literal `spec.env`. |
| `spec.secretRefs[]` | One-release legacy input: Lake-namespace whole Secrets copied into the app namespace and exposed through `envFrom`. New apps use `spec.secretEnv`. |
| `spec.host` | Exact hostname. Empty derives `<app>.<Lake spec.apps.wildcard>`. |
| `spec.auth` | `lake` (default) uses Lake's session authorization through nginx `auth-url`; `public` omits that gate. |
| `spec.bindings.zetta` | `{customerId, scopes}` for scoped read-only workspace access. Synchronized apps use an operator-generated token validated by Lake; standalone apps supply the token through their binding Secret. |
| `spec.bindings.services[]` | Lake service identifiers whose connection documents are delivered. |
| `spec.bindings.serviceNames` | Service-ID-to-filename map for `/var/run/orama/bindings/services/<name>`. The sync resolves same-workspace service names; standalone apps can omit it to retain ID filenames. |
| `spec.bindings.secretRef` | Standalone app's Lake-namespace source Secret. Key `zetta` contains the workspace token; each requested service identifier names its connection. Required for standalone data bindings; synchronized apps use generated tokens and the Lake services Secret. |
| `spec.eventsSecretRef` | Lake-namespace Secret with `url` and `token`; copied to the app and used for best-effort publication to `app-events.<app>`. |
| `spec.stores[]` | Written by the apps sync from lake's `store_mounts` (lake#666); only a synchronized app may carry it (max 32). Each `{id, name, slug, type: volume\|object, provider, claimName?, mountPath?, readOnly?, bucket?, endpoint?, region?, prefix?, secretName?}`. A volume's claim is mounted at `mountPath` (pod `fsGroup` 10001); a bucket's connection is `STORE_<NAME>_BUCKET`, `_ENDPOINT`, `_REGION`, `_PREFIX` and optional `secretKeyRef`s for `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY`, `_SESSION_TOKEN`, `_ACCOUNT_KEY`, `_SAS_TOKEN`, read from a copy of the Lake-namespace `secretName` ([stores](lake/stores.md#mounts-lake666)). |
| `status.phase` | `Progressing`, `Ready`, `Degraded`, or `TornDown`. |
| `status.url` | `https://<host>` when ingress is configured. |
| `status.ready` | True only when every desired replica is updated and available at the current revision. |
| `status.observedRevision` | Runtime source revision or image digest observed by the controller. |
| `status.message` | Stable rollout summary or highest-priority pod failure. |
| `status.conditions` | Standard `Ready` condition, deep-compared before status writes. |

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
| `spec.env[]` | Literal `{name,value}` workload environment variables: the deployment row's own, then lake's `orama_control.environment_env` values, which win for the same name. |
| `spec.secretRefs[]` | Names or `secret://<name>` references for `LakeSealedSecret` resources in the Lake namespace. Every reference must belong to the same Lake and have delivered its current generation. The produced Secrets are copied into the isolated workload namespace and imported with `envFrom`; unresolved references report `Degraded` and remove the workload. |
| `spec.host` | Exact hostname for a named environment. Preview hosts are derived from the Lake's `previewWildcard`. |
| `spec.domains[]` | Custom domains `{host, tls}` (`tls` `acme` or `provided`), from lake's `orama_control.domains` requests. Each is a rule of the environment's Ingress and, with the Lake's `dnsSecretRef`, a proxied `CloudflareDNSRecord` owned by the environment; removing it removes both. Keyed by `host`. See [the row contract](lake/environments.md#orama_controldomains). |
| `status.phase` | `Progressing \| Ready \| Degraded \| TornDown`, written only on a real transition. |
| `status.url` / `.readyReplicas` | Public URL when ingress is configured, and observed Deployment readiness. |
| `status.deploymentId` | Stable deployment-event identity copied from the `orama.cloud/deployment-id` annotation. |
| `status.domains[]` | Per custom domain: `host`, `status` (`pending \| active \| failed`) and `message`. `active` once its DNS record is Ready; the operator copies it into lake's `domains` rows. |
| `status.conditions[]` | Standard `Ready` condition with transition reason and message. |

## `LakeRestoreRehearsal` (separate CRD, cluster-scoped)

Runs and records a Lake data-service lifecycle rehearsal. The spec is immutable. Restore and
upgrade recover a selected CNPG object-store backup into an isolated scratch namespace, compare
guard-table row counts, the migrations missing from `schema_migrations` and the newest schema migration
with the live database, fail when either side lacks a migration the vendored lake schema records, and delete every
scratch object after success or failure. Upgrade changes the scratch Cluster image before
verification. Failover requests a CNPG switchover on the live replicated control DB and verifies
the existing application Secret still connects through the write Service and that the database
holds every migration the vendored lake schema records.

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
| `status.mintedAt` / `.version` | For a lake source: the last delivery from lake and the secret version the Secret holds. An unpinned lake source is delivered again every five minutes. |
| `status.conditions[Ready]` | `Delivered` or a stable refusal reason such as `UnsealFailed`, `SecretOwnershipConflict`, or `MintFailed`. |

## `KubernetesCluster` (separate CRD, cluster-scoped) — a cluster the operator provisions

The abstract kind of the infrastructure work ([#351](https://github.com/oramasearch/orama-operator/issues/351),
[#354](https://github.com/oramasearch/orama-operator/issues/354); design in
[`infrastructure/README.md`](infrastructure/README.md)). It says what cluster is wanted in
provider-neutral terms and names a `KubernetesClusterProvider`. That provider's type decides the
concrete kind the operator creates for it: `AksCluster` for type `aks`. The controller never calls
a vendor's API.

**On every cluster.** These kinds ship in `chart/crds` and their controllers run in every
cluster's operator: there is no difference between a management cluster's operator and a leaf's
([`infrastructure/README.md`](infrastructure/README.md#one-operator-on-every-cluster)). A cluster
manages others by holding these resources.

| Field | Meaning |
|---|---|
| `spec.providerRef` | Name of the `KubernetesClusterProvider`. Immutable. |
| `spec.region` | The provider's name for the region, such as `westus2`. Immutable. |
| `spec.kubernetesVersion` | `1.35` or `1.35.8`. |
| `spec.nodePools[]` | `name`, `role` (`system` or `user`), `machineSize` (the provider's word, such as `Standard_D8as_v7`), and either `count` or `minCount` with `maxCount` (which turns on the provider's autoscaling between the two; `count` is then the starting size). Optional `zones`, `labels`, `taints`. At least one pool must be `system`, with at least one node. |
| `spec.deletionPolicy` | **Required, no default.** `Delete` destroys the real cluster when the resource is deleted; `Retain` leaves it running. Clusters are ephemeral, so whoever writes the resource chooses. |
| `spec.kubeconfigSecretRef` | Where the admin kubeconfig is written (key `kubeconfig`). Default: `<name>-kubeconfig` in the namespace of the provider's credentials Secret. |
| `spec.tags` | Attached to the cluster where the provider supports them. **For an AKS provider a tag name cannot contain `<`, `>`, `%`, `&`, `\`, `?` or `/`**, so a Kubernetes-style name such as `orama.cloud/role` is refused: the cluster is `Degraded` with the tag named, and Azure is never called. At most 50 tags, names up to 512 characters, values up to 256. |
| `status.phase` | `Pending`, `Provisioning`, `Ready`, `Degraded` or `Deleting`, summarized from the concrete resource. `kubectl get kcluster` (the short name `kc` belongs to Strimzi's `KafkaConnect`). |
| `status.concreteRef` | The concrete resource, as `AksCluster/<name>`. |
| `status.endpoint` / `.kubernetesVersion` / `.nodePools[]` | The API server's address, the version running, and each pool's state, count and the provider's `error` for it. |
| `status.message` | Why the cluster is not `Ready`, in the provider's words, copied from the concrete resource. A column of `kubectl get kcluster`. |
| `status.kubeconfigSecretRef` | Set once the kubeconfig Secret holds a credential. |
| `Ready` / `InSync` conditions | Carried across from the concrete resource. |

A cluster whose spec is invalid in a way the schema cannot express (no system pool, `minCount`
above `maxCount`) is `Degraded` with the reason, and nothing is provisioned. A cluster whose
provider does not exist is `Pending` until it does.

Sample: [`config/samples/kubernetescluster-nonprod.yaml`](../config/samples/kubernetescluster-nonprod.yaml).

## `KubernetesClusterProvider` (separate CRD, cluster-scoped) — a vendor, an account, its credentials

Written once per account and shared by every cluster in it. Swapping a provider for a client that
requires a different one means pointing a `KubernetesCluster` at a different provider resource.

| Field | Meaning |
|---|---|
| `spec.type` | `aks`. Immutable. |
| `spec.credentialsSecretRef` | `{name, namespace}` of the Secret the operator authenticates with. For `aks`: keys `tenantID` and `clientID`, plus `clientSecret` for a service principal. **Without `clientSecret` the operator uses workload identity**: it exchanges its projected service-account token for an Azure token, so no long-lived secret exists. |
| `spec.aks.subscriptionID` | The Azure subscription clusters are created in. |
| `spec.aks.resourceGroupPrefix` / `.resourceGroupSuffix` | Each cluster's resource group is `<prefix><cluster name><suffix>`. The suffix defaults to `_group`. |
| `spec.aks.skuTier` | `Free`, `Standard` (default) or `Premium`. |
| `spec.aks.network` | `plugin` (`azure`), `pluginMode` (`overlay`), `dataplane` (`cilium`), `policy` (`cilium`), `podCIDR`, `serviceCIDR`, `dnsServiceIP`. The defaults are the existing cluster's. |
| `spec.aks.upgradeChannel` / `.nodeOSUpgradeChannel` | Defaults `patch` and `NodeImage`. |
| `spec.aks.osDiskSizeGB` | Default 256. |
| `spec.aks.workloadIdentity` | Default `true`: turns on the OIDC issuer and workload identity in each cluster. |
| `status.ready` | True once one cheap call (reading the subscription) succeeded with the credentials. |
| `status.authMethod` | `workload-identity` or `service-principal`, as read from the Secret. |
| `status.clusters` | How many `KubernetesCluster` resources point at this provider. **A provider cannot be deleted while that is above zero**; the delete is held and `status.message` says why. |

## `AksCluster` (separate CRD, cluster-scoped) — one AKS managed cluster

The concrete kind, and the only one whose controller calls Azure. A `KubernetesCluster` creates
and owns it; its spec is complete on its own. It calls the Azure Resource Manager REST API
directly, as the Cloudflare and Clerk controllers call theirs: there is no Azure SDK dependency.

| Field | Meaning |
|---|---|
| `spec.subscriptionID` / `.resourceGroup` / `.location` | Where the cluster lives. Immutable. |
| `spec.clusterName` | The managed cluster's name in Azure. Default: the resource's name. |
| `spec.credentialsSecretRef` | The Azure credentials Secret, as on the provider. |
| `spec.kubernetesVersion`, `.skuTier`, `.network`, `.upgradeChannel`, `.nodeOSUpgradeChannel`, `.workloadIdentity` | As on the provider and the abstract kind. |
| `spec.nodePools[]` | `name`, `mode` (`System` or `User`), `vmSize`, `count`, `enableAutoScaling` with `minCount` and `maxCount`, `osDiskSizeGB`, `availabilityZones`, `nodeLabels`, `nodeTaints`. |
| `spec.deletionPolicy` | Required. `Delete` deletes the managed cluster, then the resource group **if the operator created it**; `Retain` deletes nothing. |
| `spec.kubeconfigSecretRef` | Where the admin kubeconfig is written. The Secret is owned by the `AksCluster`. |
| `status.phase` / `.provisioningState` / `.powerState` | The operator's phase, and Azure's own words. `kubectl get aks`. |
| `status.resourceID`, `.fqdn`, `.kubernetesVersion`, `.nodeResourceGroup`, `.oidcIssuerURL`, `.nodePools[]` | What Azure reports. |
| `status.operation` | The latest operation Azure ran on the cluster, read from `operations/latest` on every reconcile: `id` (to quote to Azure support), `status` (Azure's progress line while it runs, such as `CreatingAgentPools: 0/1 nodes completed`, then `Succeeded`, `Failed` or `Canceled`), `startTime`, `endTime`, and `error` (`code: message`, followed by the causes Azure nests under it). The `Operation` column of `kubectl get aks`. |
| `status.provisioningError`, `.nodePools[].error` | The provisioning error Azure reports on the cluster and on each pool. |
| `status.message` | Why the cluster is not `Ready`, in Azure's words: the provisioning state, the operation's progress, and every error Azure reports on the operation, the cluster and each pool. The same text is the `Ready` condition's message and is copied to the `KubernetesCluster`. `kubectl get aks -o wide`, `kubectl get kcluster`. A failure is also logged by the operator as an error, once per distinct failure. |
| `status.createdResourceGroup` | True when the operator created the resource group, and so may delete it. |
| `status.kubeconfigWritten` | True once the kubeconfig Secret holds a credential. |
| `InSync` condition | False when the spec asks for something the cluster does not have. **This version reports the difference and does not act on it**: a cluster is not changed after it is created. |

What a reconcile does: register the finalizer; ensure the resource group; create the managed
cluster if it is absent; poll every 30 seconds until Azure reports `Succeeded`; fetch the admin
credential once and write the kubeconfig Secret; then re-read the cluster every five minutes.
Status is written only when it changes. On deletion with `Delete`, the resource is held until
Azure reports the cluster gone; if the credentials are missing, it is held and says so, so a
running cluster is never orphaned silently.

## `ClusterHelmRelease` (separate CRD, cluster-scoped) — one chart release on a managed cluster

Short name `chr`. Reconciled wherever it is created; the cluster holding it is managing the cluster it names.
How it fits: [`infrastructure/README.md`](infrastructure/README.md#installing-it-clusterhelmrelease-and-oramaoperator).

| Field | Meaning |
|---|---|
| `spec.clusterRef` | The `KubernetesCluster` to install onto. Immutable. The release waits while the cluster is not `Ready`. |
| `spec.releaseName`, `spec.namespace` | The Helm release and its namespace on that cluster. Immutable. The namespace is created when absent. |
| `spec.chart.repository`, `.name`, `.version` | A chart repository (`https://...`) with the chart's `name`, or an OCI repository (`oci://...`) holding the chart. `version` is required; changing it upgrades the release. |
| `spec.chart.credentialsSecretRef` | For a private chart: a `kubernetes.io/dockerconfigjson` Secret with an entry for the repository's host, or a Secret with `username` and `password`. |
| `spec.values` | The chart's values. |
| `spec.dependsOn[]` | Other `ClusterHelmRelease` names, on the same cluster, that must be `Ready` first. A release others depend on is not uninstalled until they are deleted. |
| `spec.requiredCRDs[]` | CRDs that must exist on the cluster before this chart is installed or upgraded. |
| `spec.crdURLs[]` | `https://` URLs of manifests holding CRDs the chart ships apart from itself. Applied server-side before the chart on every install and upgrade; a manifest holding anything but CustomResourceDefinitions is refused. |
| `status.phase` | `Pending` (waiting for the cluster, a dependency or a CRD), `Installing`, `Ready`, `Degraded` or `Deleting`. |
| `status.chartVersion`, `.appVersion`, `.revision`, `.releaseStatus` | The release on the cluster, in Helm's words. |
| `status.workloads`, `.readyWorkloads` | The release's Deployments, StatefulSets and DaemonSets, and how many are rolled out. |
| `status.message` | Why it is not `Ready`: each workload that is not rolled out with what its pods report, or what the chart repository, Helm or the cluster refused. |

## `OramaOperator` (separate CRD, cluster-scoped) — a managed cluster's own operator, at a version

Short name `ooperator`. Reconciled wherever it is created.

| Field | Meaning |
|---|---|
| `spec.clusterRef` | The `KubernetesCluster` to install onto. Immutable. |
| `spec.version` | The operator version the cluster runs: the image tag, and the chart published with it. Changing it upgrades the cluster's operator, CRDs first. |
| `spec.namespace` | Default `orama-system`. Immutable. |
| `spec.imagePullSecretRef` | A `kubernetes.io/dockerconfigjson` Secret on the management cluster. Copied to the leaf for the image pull, and used to pull the chart. |
| `spec.imageRepository`, `spec.chartRepository` | Defaults `oramasearch/orama-operator` and `registry-1.docker.io/oramasearch/orama-operator-chart`. |
| `spec.values` | Chart values for the cluster's operator. The image and its pull secret are set by the controller and win; nothing else differs from any other cluster's operator. |
| `status.phase` | `Pending` (waiting for the cluster, or for prerequisites), `Installing`, `Ready`, `Degraded` or `Deleting`. |
| `status.installedVersion`, `.chartVersion`, `.revision`, `.releaseStatus` | The Helm release on the leaf. |
| `status.image`, `.replicas`, `.readyReplicas` | The operator's Deployment on the leaf. `Ready` needs every replica ready on the image asked for. |
| `status.message` | Why it is not `Ready`. When prerequisites are missing it names the CRDs and says nothing was installed; when pods are failing it carries the kubelet's reason. |

## `LakeSet` and `OramaCloudV1Set` (separate CRDs, cluster-scoped) — instances on managed clusters

Short names `lset` and `ocv1set`. One shape for both; how it fits:
[`infrastructure/README.md`](infrastructure/README.md#workloads-on-managed-clusters-lakeset-and-oramacloudv1set).

| Field | Meaning |
|---|---|
| `spec.template` | The instance's spec (a `Lake`'s, an `OramaCloudV1`'s), the default for every member. Validated by each member's cluster when applied. A `LakeSet` template does not set `namespace`; an `OramaCloudV1Set` template does not set `region.name`. |
| `spec.secrets[]` | `name` and `namespace` of a Secret on this cluster, copied into every member's namespace before its instance; `as` renames it there. |
| `spec.clusters[]` | The members. Independent: none waits for another. `clusterRef` (a `KubernetesCluster`), `name` (the instance's name there), optional `namespace` (default `lake-<name>` or `v1-<name>`), optional `spec` (merged over the template; lists replace), optional `secrets` (replace the set's of the same name for this member). |
| `spec.deletionPolicy` | `Retain` (default): removing a member or deleting the set leaves the instance running. `Delete`: the instance is deleted from its cluster. |
| `status.phase` | `Ready` (every member `Ready`), `Progressing`, or `Degraded` (the set was refused, or a member failed). |
| `status.purpose` | The `purpose` tag all the members' clusters share. A set whose clusters differ in purpose, or lack one, applies nothing. |
| `status.members`, `.readyMembers` | Counts. Columns of `kubectl get lset`. |
| `status.memberStatuses[]` | Per member: `cluster`, `name`, `namespace`, `phase` (`Pending`, `Progressing`, `Ready`, `Degraded`), `version` (a Lake's image, a v1's image tag) and `message`. |
| `status.message` | Which members are not `Ready`, and what each one's cluster says about it. |
