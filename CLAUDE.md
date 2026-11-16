# orama-operator: working rules

A **kubebuilder operator** (Go) and an **install Helm chart / helmfile**. It reconciles the whole
Orama stack in a cluster from custom resources: Lake (`Lake`, `LakeSet`), Orama Zetta
(`OramaZettaCluster`), Orama Cloud v1 (`OramaCloudV1`, `OramaCloudV1Set`), the datastore kinds, and
the cluster-management kinds (`KubernetesCluster`, `ClusterHelmRelease`, `OramaOperator`).
Read [`README.md`](README.md) for the overview. Deep docs live in [`docs/`](docs/).

## Guiding principles (non-negotiable)

The operator's job is to **encapsulate the entire operational complexity of the stack** so the
whole system is deterministic, reproducible and recoverable. Every change is judged against
production-readiness, not "does a pod start".

1. **Everything production-grade.** No shortcuts that "just make it pass". If a thing is wrong
   for production (a hack, a placeholder, a single point of failure, a non-reproducible manual
   step), it is not done, even if a test cluster appears healthy.
2. **Codify all operational complexity in the operator** (first choice) or the **helmfile**
   (platform prerequisites only, e.g. CNPG or an ingress controller). **No out-of-band `kubectl`,
   no undocumented manual steps.** If an operator can own it, the operator owns it.
3. **Model all infrastructure state as CRDs: declarative, owned, reconciled** (never imperative
   one-shot Jobs or app-boot side effects). **Reuse a datastore's own operator's CRDs only when that
   operator is reliable; otherwise own the CRD yourself.** Judge the dependency first: is it
   maintained, are its images actually pullable, is the release cadence sane? **CNPG is reliable**,
   so use its `managed.roles` / `Database` / `DatabaseRole` CRDs. The Bitnami RabbitMQ
   Cluster/Topology operators were not (their images 404'd from a deprecated catalog), so the broker's
   streams and topology are modelled by our own small `RabbitMQTopology` CRD, which reconciles the
   plain RabbitMQ StatefulSet through its management API (see
   [`docs/datastore-crds.md`](docs/datastore-crds.md)). A purpose-built CRD you control beats an
   unstable vendor dependency. This holds for **off-cluster infrastructure too** (DNS, KV and object
   storage are owned CRDs). Learn an operator's API **and check its health** before relying on it.
4. **Least-privilege, per-service access.** Every service that connects to a datastore gets its
   **own role or user with only the grants it needs**, never a shared superuser or owner. Postgres:
   one CNPG `managed.role` per service (`login`, `superuser: false`, `connectionLimit`, scoped
   `GRANT`s); RabbitMQ: one `User` plus a scoped `Permission` per service; and so on. The owner or
   admin role is used only by migrations and administration, never by a running app.
5. **Determinism.** The same resource (and its child resources) must produce the same stack every
   time: idempotent reconcile, pinned versions, no hidden or implicit state, ordered dependencies.
6. **Disaster recovery is first-class.** Backup, restore and config snapshots are CRDs, scheduled
   and tested ([`docs/backups.md`](docs/backups.md)). "Can we rebuild this from nothing, and recover
   data after a loss?" must always be **yes**.
7. **Status is accurate, component-level observability, and anti-flap.** Every CRD's `.status`
   reflects real child readiness at **component granularity**: one entry per deployable component
   (writer, reader, broker, dashboard, ...), **not** per pod and **not** per field. Surface a
   `Ready | Progressing | Degraded` phase, an `n/total` count, a per-component list and standard
   `conditions` (plus `printcolumn`s for an at-a-glance `kubectl get`). **Write the status
   subresource only on a real state transition** (deep-compare first): a steady stack must produce
   **zero** control-plane traffic.
8. **Right-size everything: cap the pod and make its binary respect the cap.** Be deliberate about
   memory, CPU, storage and IOPS. Two halves, both required: (a) per-component Kubernetes
   `requests`/`limits`, PVC size and an IOPS-provisioned storage class; and (b) **translate that
   intent into the binary's own config**, because a binary that assumes it owns the node OOMs even
   under a cgroup limit. For example ClickHouse needs a `config.d` overlay (`mark_cache_size`,
   default 5 GiB, `max_server_memory_usage_to_ram_ratio`, `uncompressed_cache_size=0`); Redis
   `maxmemory`; RabbitMQ `vm_memory_high_watermark`; JVM `-Xmx`/`MaxRAMPercentage`; Node
   `--max-old-space-size`; Mongo's wiredTiger cache. Derive the binary limits from the **same
   numbers** as the Kubernetes limits. Mount config overlays with **`subPath`** so you add to, and do
   not replace and hide, the image's config directory.
9. **e2e is the forcing function, not the target.** The e2e suites progress **only** as the operator
   matures to encapsulate this complexity (real replication, real schema, real credentials, ...).
   **Never hack a test green**: make the operator correct and the test follows.
10. **Abstract kinds, concrete providers.** Use abstract objects (`KubernetesCluster`, `DnsRecord`,
    `ServiceMesh`) whose type provisions a concrete one (`AksCluster`, `CloudflareDnsRecord`,
    `IstioServiceMesh`), so a provider can be swapped when an installation requires a different one.
    A new kind for something a vendor provides is **two kinds**: an abstract one named for the
    thing, which is what people and other reconcilers ask for, and a concrete one named for the
    provider, which the abstract kind's type provisions and owns and which makes the vendor's API
    calls. The vendor, the account and the credentials live on a **provider resource** that the
    abstract kind points at (a `DnsProvider`, a `KubernetesClusterProvider`), not on each record or
    cluster. Nothing outside the concrete kind's controller names the vendor. The kinds that predate
    this rule (`CloudflareDNSRecord`, `CloudflareLoadBalancer`, `CloudflareKVNamespace`,
    `CloudflareR2Bucket`, `ClerkApplication`) are concrete only; do not add another like them. See
    [`docs/infrastructure/README.md`](docs/infrastructure/README.md).
11. **One operator: no differences between clusters.** The operator is **identical on every
    cluster**: the same chart, the same image, **every CRD**, **every controller running**, the same
    RBAC. A cluster's role (management, leaf, testing, production) is decided **only by which
    resources exist on it**, never by how its operator is installed. Any cluster can manage others.
    Concretely, **never**:

    - add a flag, environment variable or chart value that turns a controller, a kind or a
      permission on or off (`--enable-cluster-management` and `--enable-e2e-controllers` both
      existed and were both removed);
    - put a CRD anywhere but `chart/crds`, or apply some CRDs only to some clusters
      (`chart/management-crds` and `chart/e2e-crds` both existed and were both removed);
    - register a controller inside a condition in `cmd/main.go`;
    - have `OramaOperator`, a build script or a runbook set a value on one cluster that it does
      not set on another, beyond the image, its pull secret and the cluster's own cloud identity;
    - keep "one small exception" because a difference seems reasonable. "No differences" means
      none: if you believe an exception is needed, **stop and ask before writing it**.

    What may differ is a cluster's **configuration**, not its operator: where images are pulled
    from, and the identity the pod uses to call a cloud.

    `cmd/crd_coverage_test.go` enforces this (`TestNoClusterDiffersFromAnother`,
    `TestTheChartHasOneCRDDirectoryWithEveryKind`, `TestManagementKindsShipAndRunOnEveryCluster`).
    **Never weaken or delete those tests to make a change pass.**

When in doubt, choose the option that makes the operator more complete and the system more
deterministic and recoverable, even if it is more work now.

## Two installation paths, one result: `helmfile.yaml` and the operator stay in sync (STRICT)

- **A management cluster is installed from [`helmfile.yaml`](helmfile.yaml).** It has to be: it is
  the cluster the managing operator runs on, so it exists before any operator can install anything.
- **Every leaf cluster is installed by the management cluster's operator**, from
  `ClusterHelmRelease` and `OramaOperator` resources laid out as in
  [`config/clusters/`](config/clusters/README.md), which is an example of the shape.
- **Both must install the same thing**: same charts, same chart versions, same namespaces, same
  values, same order, same operator version. A management cluster and a leaf are effectively the
  same cluster.

**Every platform change is made in both places, together.** Change a release in `helmfile.yaml` and
you change the matching `ClusterHelmRelease` in every `config/clusters/*/platform.yaml`; change a
`ClusterHelmRelease` and you change `helmfile.yaml`. A new operator version on the leaves is the
same version on the management cluster. **A change that is in only one of them is not finished.**

`cmd/platform_sync_test.go` fails when they differ (against the example by default, or against
another directory of cluster resources when `ORAMA_CLUSTERS_DIR` names it). **Never weaken or delete
it to make a change pass**; fix the side that is behind. The full statement, with the table of who
installs what, is
[`docs/infrastructure/README.md`](docs/infrastructure/README.md#two-installation-paths-one-result).

## The operator is how change happens

A cluster must be rebuildable from the operator's code and the committed resources alone. There are
two ways to change what runs on a cluster, and only two:

1. **Change this operator**: code, chart, CRDs. If the operator cannot make a change, the fix is to
   teach it, not to do the change by hand.
2. **Change a custom resource the operator reconciles**, by editing its committed file and applying
   that file. File, apply and commit go together; `kubectl diff -R -f <your cluster resources>` must
   be empty.

So, as a coding agent, **do not change a cluster by hand**: no `helm install/upgrade/uninstall/
rollback`, no `kubectl apply/create/patch/edit/replace/scale/rollout restart/label/annotate/delete`
or writing `exec` on anything but the operator's own custom resources from their committed files, no
cloud-CLI change, no write to a database on a cluster, unless the person you are working for tells
you to do that specific action. Deleting an object "so it is recreated" counts. Reading is always
fine: `get`, `describe`, `logs`, `diff`, `wait`, a server dry-run, `helm list/get/history/template`,
a read-only `select`. **When a by-hand action looks like the fix, stop**, say what is wrong, and make
the operator or a resource do it. Anything an operator of the clusters would need to do by hand is a
missing feature: file it and build it.

Where to look: [`docs/infrastructure/operations.md`](docs/infrastructure/operations.md) is the
runbook for creating a cluster, installing its platform layer and operator, and running sets;
[`docs/infrastructure/README.md`](docs/infrastructure/README.md) is the architecture;
[`docs/infrastructure/orders.md`](docs/infrastructure/orders.md) covers resources a `LakeOrderSource`
creates from Lake's orders (found by the `orama.cloud/order-id` label; never edit or delete one by
hand).

## Zetta is the operator's datastore (multi-cluster)

The operator keeps its observed state in Orama Zetta. Know the model; see
[`docs/zetta-datastore.md`](docs/zetta-datastore.md):

- **Kubernetes (etcd) is the source of truth for reconciled desired state** (the CRs). **Zetta
  normally holds observed state**, including the coarse component projection in
  `orama_control.components`. The one explicit intake exception is `orama_control.deployments`:
  liquid appends an external desired event, the operator materializes it into a Lake-owned
  `LakeEnvironment` CR, and that CR is authoritative from then on. Status returns as append-only
  events only on a real transition. Both paths use the same anti-flap discipline (a steady stack
  makes zero Zetta writes).
- **One Zetta cluster can span many Kubernetes clusters** (equal CRDT peers on :7777). So the
  topology is **N Kubernetes clusters, one operator each, one shared Zetta.** Every synced row is
  namespaced by an **operator identity** (`ORAMA_OPERATOR_ID`, default the `kube-system` Namespace
  UID) so states never collide.
- **Peer traffic is always mTLS, with one shared cluster join token per cluster.** The join token is
  the cluster identity (the CA is derived from it), so the operator mints **one** token (Secret
  `<name>-zetta-cluster`) and injects it into **every** node as `ZETTA_CLUSTER_TOKEN`; otherwise each
  node derives its own CA and forms a cluster of one. There is **no plaintext-peer mode**. See
  [`docs/zetta-cluster-mtls.md`](docs/zetta-cluster-mtls.md).
- **Zetta SQL is a Postgres subset**, and all Zetta writes are best-effort and retried. Adding a
  capability to Zetta's MCP surface is a design decision, not a drive-by change.
- **Auth is mandatory.** The engine gates every HTTP, MCP and SQL call on a bearer that resolves to a
  user. So the operator **always** mints and injects a seeded admin token (Secret
  `<name>-zetta-admin`, env `ZETTA_ADMIN_TOKEN` on every node; never the `--admin-token` flag, whose
  value would be readable in the process argv) and **authenticates its own SQL client**: the
  state-sync client (`zetta_statesync.go`) and the schema-provisioning Job (`zetta_controlplane.go`)
  send `Authorization: Bearer <admin>`. `spec.requireAuth` and `spec.mcpTokenAuth` are deprecated
  no-ops.

## Docs discipline (non-negotiable)

**Keep the docs in lockstep with the source.** Every change to the operator's behavior updates
documentation in the **same commit**:

- Change a CRD (`api/v1alpha1/*.go`) → update [`docs/crd-reference.md`](docs/crd-reference.md)
  **and** regenerate (`make generate manifests`, then copy the CRD into `chart/crds/`).
- Add or alter a component reconciler (`internal/controller/`) → update
  [`docs/components.md`](docs/components.md) (what it creates and how it maps to the stack).
- Change the build, test or deploy flow → update [`docs/development.md`](docs/development.md).
- Anything user-facing → update the page under `docs/` that owns it (the README is a front door
  only: what the operator is, build and test, pointers; never append feature sections to it).

A commit that changes behavior without the matching doc update is incomplete. Keep this repo's docs
about the **operator**, not about any one deployment of it.

### The `Lake` CR

`Lake` deploys the Lake service: namespace, admin-token, instance-KEK and cluster-sealing-key
Secrets, Deployment, Service, a CNPG control-plane Postgres with Lake's schema applied through
`PostgresSchema`, an owned `OramaZettaCluster` data plane, opt-in scheduled S3-compatible backup
through CNPG Barman, cluster-scoped `LakeRestoreRehearsal` restore/upgrade/failover proof, opt-in
Ingress and DNS, and Lake-owned `LakeEnvironment` CRs materialized from the operator's Zetta
deployment table. **Every Lake runs every feature, with no per-Lake switch, and the operator supplies
a default for every image** (`internal/controller/lake_defaults.go`,
`docs/lake/README.md#every-lake-runs-every-feature`): never add an `enabled` field to a Lake feature,
and never let an empty image turn a feature off.

- It synchronizes the control Postgres `apps` table into Lake-owned `LakeApp` resources, registers
  generated token hashes, returns status, and maintains the apps wildcard DNS record
  (`docs/lake/apps.md`).
- It synchronizes control-Postgres workspace rows into restricted, default-deny namespaces with local
  Zetta credentials. Required `workspace_id` values place managed services, stores, user agents and
  synchronized apps in the same boundary (`docs/lake/workspaces.md`).
- It runs isolated image-build Jobs from Lake's `builds` table (`docs/lake/builds.md`).
- With `spec.clerk` it keeps one Clerk instance configured for Lake through Clerk's Backend API
  (`docs/lake/clerk.md`).
- With `spec.payments` it owns a `PaymentProvider`, a webhook endpoint and Lake's catalog, and gives
  the Lake its Stripe keys only once all are ready (`docs/lake/payments.md`). `spec.payments` requires
  `spec.billing.enabled` (CEL), which gives the Lake `LAKE_BILLING=on`; an install that leaves
  `spec.billing` unset bills nothing.
- `LakeSealedSecret` delivers cluster-bound ciphertext into owned Kubernetes Secrets
  (`docs/lake/sealed-secrets.md`).

Read [`docs/lake/README.md`](docs/lake/README.md) and the `Lake` section of
[`docs/crd-reference.md`](docs/crd-reference.md) before touching
`internal/controller/lake_controller.go`. The schema's source of truth is the lake repository's
`db/schema.sql`; current Lake images carry it, and the operator runs `lake migrate` from the image,
reading its Stripe events, products and guard tables from `lake manifest` (`lake_manifest.go`), so a
Lake release is only the `image:` in a set file. For older images `assets/lake-schema.sql` is a
byte-identical vendored copy (a test checks its pinned SHA-256; `hack/check-lake-schema.py` compares
the upstream bytes). Schema apply gates Lake workload rollout (`docs/lake/schema.md`). The operator
serves liquid's authenticated deployment REST/MCP contract from its manager Service
(`docs/control-plane/README.md`).

The chart ClusterRole is hand-maintained: a new owned kind needs its entry there or the manager
crash-loops on cache sync. That, its CRD in `chart/crds`, and that CRD being present on the target
cluster are **checked rather than remembered**: `go test ./cmd/...` covers the first two and
`hack/check-cluster-crds.sh` the third. Read
[`docs/operations/crd-rollout.md`](docs/operations/crd-rollout.md) before any roll.

## Build and codegen

Go, kubebuilder, helm, helmfile and kubectl on `PATH` (plus `$(go env GOPATH)/bin`).

- After editing `api/v1alpha1/*.go`: `make generate` (DeepCopy) and `make manifests` (CRD and
  RBAC), then `cp config/crd/bases/*.yaml chart/crds/`. The generated `zz_generated.deepcopy.go` and
  CRDs **are committed**. The `Makefile` pins controller-gen to `v0.21.0`; do not use `@latest`,
  which can restamp unrelated CRDs.
- `go build ./...` and `go vet ./...` must stay green.
- `helm lint chart` must pass; the chart carries the generated CRDs in `chart/crds/`.

## Test

- **Unit and envtest suites:** `make test` (or `go test ./...` with the envtest assets).
  `go test ./cmd/...` holds the structural checks (CRD coverage, platform sync, field removal).
- **Local run:** `make run` (`go run ./cmd --control-plane-bind-address=""`) reconciles against your current kube-context.
  Use a disposable cluster (kind, k3d, a sandbox), never one you were not told to use.
- **Every cluster gets every CRD and runs every controller** (principle 11). The e2e kinds
  (`OramaCloudV1E2ETest`, `ZettaClusterE2ETest`, `LakeE2ETest`) are in `chart/crds` and reconciled on
  every cluster; a cluster runs an e2e suite only when one of those resources is created on it.
- **Functional e2e (`OramaCloudV1E2ETest`):** the suite is
  `internal/controller/assets/e2e-functional.sh` (a curl and jq script embedded with `go:embed`,
  shipped in a ConfigMap, run by an `alpine` Job). It bootstraps a clean collection and index and
  asserts every critical OramaCore function in variations (search modes, filters, facets, sort,
  group, CRUD, upsert, delete, replication canary, batch-get, KV, hooks, merchandising, MCP,
  answer/RAG; never the analytics export, which erases readers' events). It sources
  `assets/e2e-lib.sh` and `assets/e2e-control-plane.sh` (the dashboard's JWKS, write tokens, RBAC and
  CSRF guards, the edge workers' auth, the expired token), whose local harness is
  `e2e_control_plane_script_test.go`. The write-token sections need `spec.dashboardFixtureSecretRef`
  and otherwise skip with the reason. **It must stay `fail=0`.** Editing the script requires
  rebuilding the operator (the script is embedded at compile time). Per principle 9, extend the
  operator to make an assertion pass; never soften the assertion.
- **Zetta e2e (`ZettaClusterE2ETest`):** runs the live-cluster Zetta suite as a Job against a
  deployed `OramaZettaCluster`. The script lives in the zetta repository (`tests/e2e/zetta-e2e.sh`)
  and is baked into the `zetta-e2e` image, so it evolves with the engine; the CRD only triggers it,
  reading the target's `spec.replicas` and passing the addressing as `ZETTA_E2E_*` env (`BASE_URL`
  pinned to node 0, `PEER_URLS` for ordinals 1..N-1, `LEVELS`, and the admin `TOKEN`). Auth is
  mandatory, so the e2e always sets `ZETTA_E2E_REQUIRE_AUTH=1` and `ZETTA_E2E_TOKEN` (the seeded
  admin token). The status carries best-effort pass/fail/skip counts parsed from the Job's pod logs.
  Watch `kubectl get ozce2e`; logs with `kubectl logs job/<name>-e2e`.
- **Lake e2e (`LakeE2ETest`):** runs the `lake-e2e` runner image as a Job in the target Lake's
  namespace: every Lake feature as a freshly signed-up person (never the admin bearer), through REST,
  MCP and a headless browser. It targets a `LakeSet` member (`spec.member`; refused unless the Lake
  carries that set's label) or a Lake by name, waits for the Lake, Postgres and schema, runs one Job
  `<name>-e2e-<generation>` per spec generation (`spec.runID` re-runs it), and records pass/fail/skip
  counts and each failed and skipped step's reason from the runner's termination message. See
  `docs/lake/README.md#service-e2e`.

## Branches and releases

- **Every pull request targets `dev`.** `dev` is the integration branch.
- **`main` moves only on a release**, by a pull request that promotes `dev` to `main`. Between
  releases `main` equals the last tag. Never push to `dev` or `main` directly.
- Merge pull requests with a **merge commit**, never a squash or rebase.
- A release writes `docs/releases/vX.Y.Z.md` (what it includes, what it does not, known issues, the
  proof: `go build ./...`, `go vet ./...`, `go test ./...` with envtest assets, `helm lint chart`, and
  which opt-in tests did not run), moves `version` and `appVersion` in `chart/Chart.yaml`, and tags
  the merge commit on `main` `vX.Y.Z`.

## Conventions

- **No AI attribution** in commits or pull requests: no `Co-Authored-By` trailer for an assistant,
  no "Generated with ...", no mention of an AI tool.
- **Conventional Commits** (`feat:`, `fix:`, `docs:`, `chore:`, `test:`).
- **Do not write time or size estimates** for work; describe what is done and what is not.
- **Never write the word "seam".** It has been used for an interface, an extension point, an injected
  dependency and a boundary, which are different things. Name the actual mechanism.
- The operator deploys published images; it does not build the stack's application components from
  source.
- Before any `helm upgrade`, run `make check-crds CONTEXT=<kube-context>`. Helm applies `chart/crds`
  on install only, so an upgrade never adds a new CRD; see
  [`docs/operations/crd-rollout.md`](docs/operations/crd-rollout.md).
- **CRDs go on in two steps around a roll**: widened by what the cluster still has before the new
  operator starts (`hack/crdunion`), exact once it alone runs. Never `kubectl apply -f chart/crds/`
  before a roll: a removed field is pruned under the old operator. Never change a field's type or
  meaning; add a new field. A removal is named in `cmd/crd_field_removal_test.go`. See
  [`docs/operations/crd-rollout.md`](docs/operations/crd-rollout.md#removing-or-changing-a-field-crds-in-two-steps-around-the-roll).
