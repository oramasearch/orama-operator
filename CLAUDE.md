# orama-operator — working rules

A **kubebuilder operator** (Go) + an **install Helm chart / helmfile** that deploys an
entire self-contained **Orama Cloud v1** stack in-cluster from one `OramaCloudV1` CR.
Read [`README.md`](README.md) for the overview and [`DESIGN.md`](DESIGN.md) for the
architecture + decision log. Deep docs live in [`docs/`](docs/).

## Guiding principle — this is a production migration (non-negotiable)

**We are migrating Orama Cloud v1 to run on this operator in production.** The operator's job
is to **encapsulate the entire operational complexity of the stack** so the whole system is
deterministic, reproducible, and recoverable — not to be a dev/demo toy. Every change is judged
against production-readiness, not "does a pod start."

1. **Everything production-grade.** No shortcuts that "just make it pass." If a thing is wrong
   for production (a hack, a placeholder, a single point of failure, a non-reproducible manual
   step), it is not done — even if the sandbox appears healthy.
2. **Codify all operational complexity in the operator** (first choice) or the **helmfile**
   (platform prereqs only, e.g. CNPG / an ingress controller). **No out-of-band `kubectl`, no
   undocumented manual steps.** If an operator can own it, the operator owns it.
3. **Model all infrastructure state as CRDs — declarative, owned, reconciled (never imperative
   one-shot Jobs or app-boot side effects). Reuse a datastore's OWN operator's CRDs only when that
   operator is RELIABLE; otherwise own the CRD yourself.** Judge the dependency first — is it
   maintained, are its images actually pullable, is the release cadence sane? **CNPG is reliable**
   → use its `managed.roles` / `Database` / `DatabaseRole` CRDs. The **bitnami RabbitMQ
   Cluster/Topology operators were NOT** — their images 404'd (`ErrImagePull`, deprecated catalog),
   so we **dropped them** and model the broker's streams/topology with our **own** small
   `RabbitMQTopology` CRD reconciling the plain RabbitMQ StatefulSet via its management API
   ([#41](https://github.com/oramasearch/orama-operator/issues/41); design +
   the datastore-state-as-CRDs epic in [`docs/datastore-crds.md`](docs/datastore-crds.md)).
   A purpose-built CRD you control beats an unstable vendor dependency: faster to build, and it
   can't rot out from under you. This holds for **off-cluster infra too** (Cloudflare DNS/KV/R2 are
   owned CRDs). Learn an operator's API **and check its health** (perplexity) before relying on it.
4. **Least-privilege, per-service access.** Every service that connects to a datastore gets its
   **own role/user with only the grants it needs** — never a shared superuser/owner. Postgres:
   one CNPG `managed.role` per service (`login`, `superuser: false`, `connectionLimit`, scoped
   `GRANT`s); RabbitMQ: one `User` + scoped `Permission` per service; etc. The owner/admin role is
   used only by migrations/admin, never by a running app.
5. **Determinism.** The same `OramaCloudV1` (+ its child CRs) must produce the same stack every
   time. Idempotent reconcile, pinned versions, no hidden/implicit state, ordered dependencies.
6. **Disaster recovery is first-class.** Backup / restore / config-snapshot are CRDs, scheduled,
   and tested (see [operator#33](https://github.com/oramasearch/orama-operator/issues/33)).
   "Can we rebuild this from nothing, and recover data after a loss?" must always be **yes**.
7. **Status is accurate, component-level observability — and anti-flap.** Every CRD's `.status`
   reflects real child readiness at **component granularity** — one entry per deployable component
   (writer/reader/broker/dashboard/…), **not** per-pod and **not** per-field. Surface a
   `Ready | Progressing | Degraded` phase, an `n/total` count, a per-component list, and standard
   `conditions` (+ `printcolumn`s for at-a-glance `kubectl get`). **Write the status subresource
   ONLY on a real state transition** (deep-compare first): a steady stack must produce **zero**
   control-plane traffic. Middle ground — accurate enough to trust, coarse enough not to flap.
8. **Right-size everything — cap the pod AND make its binary respect the cap.** Be deliberate
   about resource use (mem/cpu/**storage**/**IOPS**). Two halves, both required: (a) per-component
   k8s `requests`/`limits` + PVC size + an IOPS-provisioned storageClass; and (b) **translate that
   intent into the binary's OWN config** — a binary that assumes it owns the node OOMs even under a
   cgroup limit. E.g. ClickHouse needs a `config.d` overlay (`mark_cache_size` — default **5 GiB**!,
   `max_server_memory_usage_to_ram_ratio`, `uncompressed_cache_size=0`); Redis `maxmemory`; RabbitMQ
   `vm_memory_high_watermark`; JVM `-Xmx`/`MaxRAMPercentage`; Node `--max-old-space-size`; Mongo
   wiredTiger cache. Derive the binary limits from the **same numbers** as the k8s limits. Mount
   config overlays via **`subPath`** so you add to (not replace + hide) the image's config dir.
   ([#42](https://github.com/oramasearch/orama-operator/issues/42))
9. **e2e is the forcing function, not the target.** The e2e suite progresses **only** as the
   operator matures to encapsulate this complexity (real writer→reader replication, real schema,
   real creds, …). **Never hack the test green** — make the operator correct and e2e follows.

10. **Abstract kinds, concrete providers.** Steve, 2026-10-12: *"as a general rule, try to use
    abstract objects (KubernetesCluster, DnsRecord, ServiceMesh) with a type that provisions a
    concrete AksCluster, CloudflareDnsRecord, IstioServiceMesh ... that way, we can swap out
    different providers if an enterprise client has hard requirements for one provider over
    another."* A new kind for something a vendor provides is **two kinds**: an abstract one named
    for the thing, which is what people and other reconcilers ask for, and a concrete one named for
    the provider, which the abstract kind's type provisions and owns and which makes the vendor's
    API calls. The vendor, the account and the credentials live on a **provider resource** that
    the abstract kind points at (a `DnsProvider`, a `KubernetesClusterProvider`), not on each
    record or cluster. Nothing outside the concrete kind's controller names the vendor. The kinds that
    predate this rule (`CloudflareDNSRecord`, `CloudflareLoadBalancer`, `CloudflareKVNamespace`,
    `CloudflareR2Bucket`, `ClerkApplication`) are concrete only; do not add another like them. See
    [`docs/infrastructure/README.md`](docs/infrastructure/README.md).

When in doubt, choose the option that makes the operator more complete and the system more
deterministic/recoverable, even if it is more work now.

11. **One operator: no differences between clusters (non-negotiable).** Steve, 2026-10-12, three
    times in one session before it was done: *"install the same operator everywhere... all crds"*,
    *"no differences between management and leaf operator"*, *"any leaf can be a mgnt cluster
    too... no differences"*, then *"I said no differences!!! I said it like 3 times"*.

    The operator is **identical on every cluster**: the same chart, the same image, **every CRD**,
    **every controller running**, the same RBAC. A cluster's role (management, leaf, testing,
    production) is decided **only by which resources exist on it**, never by how its operator is
    installed. Concretely, **never**:

    - add a flag, environment variable or chart value that turns a controller, a kind or a
      permission on or off (`--enable-cluster-management` and `--enable-e2e-controllers` both
      existed and were both removed);
    - put a CRD anywhere but `chart/crds`, or apply some CRDs only to some clusters
      (`chart/management-crds` and `chart/e2e-crds` both existed and were both removed);
    - register a controller inside a condition in `cmd/main.go`;
    - have `OramaOperator`, a build script or a runbook set a value on one cluster that it does
      not set on another, beyond the image, its pull secret and the cluster's own cloud identity;
    - keep "one small exception" because a difference seems reasonable. When Steve says no
      differences, there are none: if you believe an exception is needed, **stop and ask before
      writing it**, do not ship it and mention it afterwards.

    What may differ is a cluster's **configuration**, not its operator: where images are pulled
    from, and the identity the pod uses to call a cloud.

    `cmd/crd_coverage_test.go` enforces this (`TestNoClusterDiffersFromAnother`,
    `TestTheChartHasOneCRDDirectoryWithEveryKind`, `TestManagementKindsShipAndRunOnEveryCluster`).
    **Never weaken or delete those tests to make a change pass.**

## One Orama: Zetta is the operator's datastore (multi-cluster)

Beyond deploying v1, this operator is becoming **symbiotic with Orama Zetta** (One Orama stage 2,
[orama-bridge#130](https://github.com/oramasearch/orama-bridge/issues/130)). Know the model — see
[`docs/zetta-datastore.md`](docs/zetta-datastore.md):

- **k8s (etcd) is the source of truth for reconciled DESIRED state** (the CRs). **Zetta normally holds
  OBSERVED state**, including the coarse component projection in `orama_control.components`. The one
  explicit intake exception is `orama_control.deployments`: liquid appends an external desired event,
  the operator materializes it into a Lake-owned `LakeEnvironment` CR, and that CR is authoritative from
  then on. Status returns as append-only events only on a real transition. Both paths use the same
  anti-flap discipline (steady stack ⇒ zero Zetta writes).
- **One Zetta cluster can span many k8s clusters** (equal CRDT peers on :7777 — not bound to one
  cluster). So the topology is **N k8s clusters → 1 operator each → 1 shared Zetta.** Every synced row is
  namespaced by an **operator identity** (`ORAMA_OPERATOR_ID`, default the `kube-system` Namespace UID)
  so states never collide.
- **Peer traffic is always mTLS — one shared cluster join token per cluster.** The join token *is* the
  cluster identity (the CA is derived from it), so the operator mints **one** token (Secret
  `<name>-zetta-cluster`, minted once/never rotated) and injects it into **every** node as
  `ZETTA_CLUSTER_TOKEN` — else each node derives its own CA and forms a cluster-of-one. There is **no
  plaintext-peer mode** (removed as a security hole). See
  [`docs/zetta-cluster-mtls.md`](docs/zetta-cluster-mtls.md) ([operator#92](https://github.com/oramasearch/orama-operator/issues/92));
  multi-node convergence also needs [zetta#785](https://github.com/oramasearch/zetta/issues/785)/[#786](https://github.com/oramasearch/zetta/issues/786).
- **Elastic capacity (planned):** Zetta signals the operator, the operator drives the cluster's node
  autoscaler (Karpenter/NAP), and more Zetta nodes schedule onto the new nodes —
  [operator#69](https://github.com/oramasearch/orama-operator/issues/69).
- **Zetta SQL is a Postgres subset** (append-only on the deployed engine — no `UPDATE`/`DELETE` until
  [zetta#735](https://github.com/oramasearch/zetta/issues/735) ships); the node HTTP server is flaky
  ([zetta#731](https://github.com/oramasearch/zetta/issues/731)) so all Zetta writes are best-effort +
  retried. The **MCP boundary** holds: adding a capability to Zetta's MCP needs a green light.
- **Auth is mandatory ([zetta#799](https://github.com/oramasearch/zetta/issues/799)).** The engine gates
  every HTTP/MCP/SQL call on a bearer that resolves to a user — there is no unauthenticated/`--mcp-token`/
  `--cluster-plaintext` mode (those flags were removed; passing them crashes the node). So the operator
  **always** mints + injects a seeded admin token (Secret `<name>-zetta-admin`, env `ZETTA_ADMIN_TOKEN` on every
  node — never the `--admin-token` flag, whose value would be readable in the process argv, operator#279) and **authenticates its own SQL client** — the state-sync client (`zetta_statesync.go`) and the
  schema-provisioning curl Job (`zetta_controlplane.go`) send `Authorization: Bearer <admin>`.
  `spec.requireAuth`/`spec.mcpTokenAuth` are now **deprecated no-ops**; the target image must be
  auth-capable (post-#799).

## Docs discipline (non-negotiable)

**Keep the docs in lockstep with the source.** Every change to the operator's
behavior updates documentation in the **same change**:

- Change the CRD (`api/v1alpha1/*.go`) → update [`docs/crd-reference.md`](docs/crd-reference.md)
  **and** regenerate (`make generate manifests` + copy the CRD into `chart/crds/`).
- Add/alter a component reconciler (`internal/controller/`) → update
  [`docs/components.md`](docs/components.md) (what it creates + how it maps to prod).
- Change the build/test/deploy flow → update [`docs/development.md`](docs/development.md).
- A decision changes → update [`DESIGN.md`](DESIGN.md)'s decision table.
- Anything user-facing → update the page under `docs/` that owns it (the README is a front door only:
  what the operator is, build and test, pointers; never append feature sections to it).

A PR/commit that changes behavior without the matching doc update is incomplete. The
ops truth for the *production* system being modeled lives in
[`techops-docs`](https://github.com/oramasearch/techops-docs) — keep this repo's docs
about the **operator**, not a copy of prod docs.

### The `Lake` CR (lake, liquid project)

`Lake` deploys the lake service: namespace, admin-token, instance-KEK and cluster-sealing-key Secrets, Deployment, Service, a CNPG
control-plane Postgres with lake's schema applied through `PostgresSchema`, an owned `OramaZettaCluster` data plane, opt-in scheduled R2/S3
backup through CNPG Barman, cluster-scoped `LakeRestoreRehearsal` restore/upgrade/failover proof,
opt-in Ingress + Cloudflare DNS, and Lake-owned `LakeEnvironment` CRs materialized from the
operator's Zetta deployment table. Opt-in `spec.apps.enabled` synchronizes the control Postgres
`apps` table into Lake-owned `LakeApp` resources, registers generated token hashes, returns status,
and maintains the apps wildcard DNS record; see `docs/lake/apps.md`.
Opt-in `spec.workspaces.enabled` synchronizes control-Postgres workspace rows into restricted,
default-deny namespaces with local Zetta credentials. Required `workspace_id` values place managed
services, stores, user agents and synchronized apps in the same boundary; see
`docs/lake/workspaces.md`.
It also runs opt-in, isolated image-build Jobs from Lake's `builds` table; see `docs/lake/builds.md`.
`LakeSealedSecret` delivers cluster-bound ciphertext into
owned Kubernetes Secrets; see `docs/lake/sealed-secrets.md`.
Read [`docs/lake/README.md`](docs/lake/README.md) (what it reconciles, deploy and run runbook, the current
production instance) and the `Lake` section of [`docs/crd-reference.md`](docs/crd-reference.md) before touching
`internal/controller/lake_controller.go`. The schema's source of truth is `oramasearch/lake` `db/schema.sql`;
`assets/lake-schema.sql` is a byte-identical vendored copy (a test checks its pinned SHA-256;
`hack/check-lake-schema.py` compares the upstream bytes). Schema apply gates Lake workload rollout;
see `docs/lake/schema.md`. The operator serves
liquid's authenticated deployment REST/MCP contract from its manager Service; see
`docs/control-plane/README.md`. The chart ClusterRole is
hand-maintained: a new owned kind needs its entry there or the manager CrashLoops on cache sync.
That, its CRD in `chart/crds`, and that CRD being present on the target cluster are all now
**checked rather than remembered** — `go test ./cmd/...` covers the first two and
`hack/check-cluster-crds.sh` the third. Read
[`docs/operations/crd-rollout.md`](docs/operations/crd-rollout.md); it exists because forgetting two
of them took production down for ~12 hours
([#166](https://github.com/oramasearch/orama-operator/issues/166)).

## Build / codegen flow

Go lives at `/opt/homebrew/bin` (`export PATH="/opt/homebrew/bin:$(go env GOPATH)/bin:$PATH"`).

- After editing `api/v1alpha1/*.go`: `make generate` (DeepCopy) + `make manifests`
  (CRD + RBAC) + `cp config/crd/bases/*.yaml chart/crds/`. The generated
  `zz_generated.deepcopy.go` and CRDs **are committed**. `Makefile` pins
  controller-gen to `v0.21.0`; do not use `@latest`, which can restamp unrelated CRDs.
- `go build ./...` + `go vet ./...` must stay green.
- `helm lint chart` must pass; the chart carries the generated CRD in `chart/crds/`.

## Test

- **Local run:** `make run` (or `go run ./cmd/main.go`) reconciles against your current
  kube-context — the standard dev loop.
- **AKS smoke test:** context `orama-production-cluster-01` is a fresh, empty,
  single-node **sandbox** (no GPU). Install the CRD + apply a sample into an isolated
  `orama-system` namespace. **No GPU node yet → set `writer.gpu: false`** when testing
  the engine, or the writer pod stays Pending.
**Every cluster gets every CRD and runs every controller** (principle 11). The e2e kinds
(`OramaCloudV1E2ETest`, `ZettaClusterE2ETest`, `LakeE2ETest`) are in `chart/crds` and reconciled on
every cluster; a cluster runs an e2e suite only when one of those resources is created on it.
Production runs no e2e suites because none is created there
([#166](https://github.com/oramasearch/orama-operator/issues/166)).

- **Functional e2e (`OramaCloudV1E2ETest`):** the comprehensive suite lives at
  `internal/controller/assets/e2e-functional.sh` (a curl+jq script embedded via `go:embed`,
  shipped in a ConfigMap, run by an `alpine` Job that installs curl+jq). It self-bootstraps a
  clean-slate collection+index and asserts **every critical OramaCore function in variations**
  (search modes/filters/facets/sort/group, CRUD/upsert/delete, replication canary, batch-get, KV,
  hooks, merchandising, MCP, analytics, answer/RAG). **~70 assertions; must stay `fail=0`.** Editing
  the script requires re-running the operator (the script is embedded at compile time). Coverage +
  the per-subsystem backlog are tracked under
  [operator#44](https://github.com/oramasearch/orama-operator/issues/44). Per principle #9,
  extend the operator to make an assertion pass — never soften the assertion.
- **Zetta e2e (`ZettaClusterE2ETest`):** runs the **live-cluster zetta suite** as a Job against a
  deployed `OramaZettaCluster`. Unlike the v1 suite (embedded via `go:embed`), the Zetta script lives in
  the **zetta repo** (`tests/e2e/zetta-e2e.sh`) and is **baked into the `oramasearch/zetta-e2e` image**
  (its `ENTRYPOINT` runs it) so it evolves with the engine — the CRD only *triggers* it, reading the
  target's `spec.replicas` and passing the addressing as `ZETTA_E2E_*` env (`BASE_URL` pinned to node-0,
  `PEER_URLS` for ordinals 1..N-1, `LEVELS`, and the admin `TOKEN`). Runs L2
  (`zetta::testkit`) + L3 (black-box integration). Watch `kubectl get ozce2e`; logs `kubectl logs
  job/<name>-e2e`. A coverage change touches three repos: zetta (script) → zetta-bridge (image) →
  operator (CRD). **Authz coverage (always on):** auth is **mandatory** ([zetta#799](https://github.com/oramasearch/zetta/issues/799)),
  so the e2e ALWAYS sets `ZETTA_E2E_REQUIRE_AUTH=1` + `ZETTA_E2E_TOKEN` (the seeded admin token, Secret
  `<name>-zetta-admin`) to exercise authz enforcement — the regression harness for the zetta auth work
  ([operator#88](https://github.com/oramasearch/orama-operator/issues/88),
  [zetta#799](https://github.com/oramasearch/zetta/issues/799)). The target's image MUST be an auth-capable
  build (post-#799). `spec.requireAuth`/`spec.mcpTokenAuth` are deprecated no-ops. The test's status also
  carries best-effort `pass`/`fail`/`skip` counts parsed from the Job's pod logs (RBAC `pods/log get`).
- **Lake e2e (`LakeE2ETest`):** runs a lake/liquid-owned runner image as a Job in the target
  cluster-scoped `Lake` resource's namespace. The controller waits for the Lake Deployment,
  control-plane Postgres, and schema, then supplies `LAKE_E2E_BASE_URL` and a unique
  `LAKE_E2E_RUN_ID`. The image entrypoint signs up a fresh user, creates a repository, pushes and
  pulls it through liquid's sync protocol, and verifies the pulled bytes. The runner image is
  required and should use an immutable published tag; the operator does not embed a copy of the
  cross-repository suite. Watch `kubectl get lakee2e`; logs are on Job `<name>-e2e` in the Lake
  namespace.

## Branches: work lands on `dev`, `main` is the last release

Steve, 2026-10-12, after two pull requests were merged into `main` the day `v0.1.0` was cut:
*"shouldn't you be merging into dev branch? we're not cutting a release yet"*.

- **Every pull request targets `dev`.** `dev` is the integration branch.
- **`main` moves only on a release**, by a pull request that promotes `dev` to `main`. Between
  releases `main` equals the last tag.
- **Never push to `dev` or `main` directly.**
- **The development cluster runs `dev`** between releases: the manager's image is built from the
  tip of `dev` and tagged `dev-<sha>`. A release image is built from the tag and tagged
  `main-<sha>`.
- Until 2026-10-12 this repository had no `dev` and pull requests went to `main`. Older pages,
  issues and skills that say so are out of date.

## Cutting a release

A release promotes `dev` to `main` and tags the result. The pages and their conventions are in
[`docs/releases/`](docs/releases/README.md). `v0.1.0` (2026-10-12) was the first.

1. **Steve names the version.** `dev` is what gets released, so confirm it is what the cluster
   runs: the manager's image tag is `dev-<sha>`, and that commit must be the tip of `origin/dev`,
   or an ancestor with nothing unproven after it.
2. **On a branch off `dev`**, `docs/release-vX.Y.Z`, write `docs/releases/vX.Y.Z.md` and add it to
   `docs/releases/README.md`: what the release includes, what it does not, known issues, and
   verification. Say where each thing has run (implemented, validated on the cluster, live-tested).
   Move `version` and `appVersion` in `chart/Chart.yaml` when the version changes.
3. **Prove it**: `go build ./...`, `go vet ./...`, `go test ./...` with the envtest assets, and
   `helm lint chart`. Put the results in the page, and say which opt-in tests did not run.
4. **Open the pull request into `dev`** and merge it with a merge commit, never a squash.
5. **Promote `dev` to `main`** with a pull request titled `release: vX.Y.Z — ...`, merged with a
   merge commit. `dev` is kept. Then merge `main` back into `dev` with a pull request, so the two
   share history.
6. **Tag the merge commit on `main`** `vX.Y.Z` and publish the GitHub release from the page
   (`gh release create vX.Y.Z --repo oramasearch/orama-operator --verify-tag --title "Release vX.Y.Z" --notes-file <page without its first heading, relative links made absolute at the tag>`).
7. **Build the image from the tag and roll it.** `make check-crds CONTEXT=<context>` first, then
   build `oramasearch/orama-operator:main-<sha>` on the cluster BuildKit, apply `chart/crds/`, and
   upgrade the release with the stored
   values as a file (liquid-bridge's `liquid-dev` skill has the commands). Confirm the pod runs the
   new digest and that every `Lake` and `OramaZettaCluster` is still `Ready`.
   Publish the chart for the same tag, so a management cluster can install this version on a
   leaf: `go run ./hack/pushchart -version <image tag>` (and again for `vX.Y.Z` after step 8). It
   goes to the private repository `oramasearch/orama-operator-chart`.
8. **Tag the image `vX.Y.Z` in Docker Hub**, naming the same digest that is deployed, without
   rebuilding: `retag-image.py orama-operator main-<sha> vX.Y.Z` (in the `liquid-dev` skill). Check
   that an anonymous request for the repository still answers 404.

A release is not finished until the Git tag, the GitHub release and the image tag all exist and
the cluster runs that image.

## Conventions

- **No AI attribution** in commits/PRs (org policy): no `Co-Authored-By: Claude`,
  "Generated with…", or any Claude/Codex/AI mention.
- **Reuse published `oramasearch/*` images** — no build-from-source of the app
  components in this repo.
- Conventional Commits (`feat:`/`fix:`/`docs:`/`chore:`).
- Shipping: a pull request into `dev` (see *Branches* above), merged with a merge commit → bump the
  pointer in the umbrella `orama-bridge` (fast-forward to its `main`). This repo left
  `orama-cloud-v1-bridge` on 2026-09-09 when it was renamed from `orama-cloud-v1-operator` and
  promoted to a top-level module — it is no longer v1-specific, so there is no v1-bridge pointer in
  the chain any more.
- Before any `helm upgrade`, run `make check-crds CONTEXT=<kube-context>`. Helm applies
  `chart/crds` on install only, so an upgrade never adds a new CRD; see
  [`docs/operations/crd-rollout.md`](docs/operations/crd-rollout.md).
