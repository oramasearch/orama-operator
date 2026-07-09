# orama-cloud-v1-operator — working rules

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
   so we **dropped them** and model the broker's streams/topology with our **own** small CRD
   reconciling the plain RabbitMQ StatefulSet via its management API ([#41](https://github.com/oramasearch/orama-cloud-v1-operator/issues/41)).
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
   and tested (see [operator#33](https://github.com/oramasearch/orama-cloud-v1-operator/issues/33)).
   "Can we rebuild this from nothing, and recover data after a loss?" must always be **yes**.
7. **Status is accurate, component-level observability — and anti-flap.** Every CRD's `.status`
   reflects real child readiness at **component granularity** — one entry per deployable component
   (writer/reader/broker/dashboard/…), **not** per-pod and **not** per-field. Surface a
   `Ready | Progressing | Degraded` phase, an `n/total` count, a per-component list, and standard
   `conditions` (+ `printcolumn`s for at-a-glance `kubectl get`). **Write the status subresource
   ONLY on a real state transition** (deep-compare first): a steady stack must produce **zero**
   control-plane traffic. Middle ground — accurate enough to trust, coarse enough not to flap.
8. **e2e is the forcing function, not the target.** The e2e suite progresses **only** as the
   operator matures to encapsulate this complexity (real writer→reader replication, real schema,
   real creds, …). **Never hack the test green** — make the operator correct and e2e follows.

When in doubt, choose the option that makes the operator more complete and the system more
deterministic/recoverable, even if it is more work now.

## Docs discipline (non-negotiable)

**Keep the docs in lockstep with the source.** Every change to the operator's
behavior updates documentation in the **same change**:

- Change the CRD (`api/v1alpha1/*.go`) → update [`docs/crd-reference.md`](docs/crd-reference.md)
  **and** regenerate (`make generate manifests` + copy the CRD into `chart/crds/`).
- Add/alter a component reconciler (`internal/controller/`) → update
  [`docs/components.md`](docs/components.md) (what it creates + how it maps to prod).
- Change the build/test/deploy flow → update [`docs/development.md`](docs/development.md).
- A decision changes → update [`DESIGN.md`](DESIGN.md)'s decision table.
- Anything user-facing → update [`README.md`](README.md).

A PR/commit that changes behavior without the matching doc update is incomplete. The
ops truth for the *production* system being modeled lives in
[`techops-docs`](https://github.com/oramasearch/techops-docs) — keep this repo's docs
about the **operator**, not a copy of prod docs.

## Build / codegen flow

Go lives at `/opt/homebrew/bin` (`export PATH="/opt/homebrew/bin:$(go env GOPATH)/bin:$PATH"`).

- After editing `api/v1alpha1/*.go`: `make generate` (DeepCopy) + `make manifests`
  (CRD + RBAC) + `cp config/crd/bases/*.yaml chart/crds/`. The generated
  `zz_generated.deepcopy.go` and CRDs **are committed**.
- `go build ./...` + `go vet ./...` must stay green.
- `helm lint chart` must pass; the chart carries the generated CRD in `chart/crds/`.

## Test

- **Local run:** `make run` (or `go run ./cmd/main.go`) reconciles against your current
  kube-context — the standard dev loop.
- **AKS smoke test:** context `orama-production-cluster-01` is a fresh, empty,
  single-node **sandbox** (no GPU). Install the CRD + apply a sample into an isolated
  `orama-system` namespace. **No GPU node yet → set `writer.gpu: false`** when testing
  the engine, or the writer pod stays Pending.

## Conventions

- **No AI attribution** in commits/PRs (org policy): no `Co-Authored-By: Claude`,
  "Generated with…", or any Claude/Codex/AI mention.
- **Reuse published `oramasearch/*` images** — no build-from-source of the app
  components in this repo.
- Conventional Commits (`feat:`/`fix:`/`docs:`/`chore:`).
- Shipping: commit + push this repo → bump the pointer in `orama-cloud-v1-bridge` →
  bump the pointer in the umbrella `orama-bridge` (fast-forward to `main`).
