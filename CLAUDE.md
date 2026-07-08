# orama-cloud-v1-operator — working rules

A **kubebuilder operator** (Go) + an **install Helm chart / helmfile** that deploys an
entire self-contained **Orama Cloud v1** stack in-cluster from one `OramaCloudV1` CR.
Read [`README.md`](README.md) for the overview and [`DESIGN.md`](DESIGN.md) for the
architecture + decision log. Deep docs live in [`docs/`](docs/).

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
