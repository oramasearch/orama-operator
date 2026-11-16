# Contributing to orama-operator

Thank you for helping. This repository is the Orama operator: a Kubernetes operator (Go,
kubebuilder) and the Helm chart that installs it.

## Before you start

- Read [`README.md`](README.md) and [`docs/development.md`](docs/development.md) for the toolchain
  and the repository layout.
- For a large change, open an issue first so the approach can be agreed before you write it.
- Report security problems privately, as [`SECURITY.md`](SECURITY.md) says, never in a public issue.
- Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## Making a change

1. Fork the repository and branch from `dev`. Pull requests target `dev`; `main` holds the latest
   release.
2. Keep each pull request to one topic. Explain at the top what it changes, why, and how.
3. Change the CRD Go types in `api/v1alpha1/`, then regenerate: `make generate manifests`, and
   copy the CRDs into the chart (`make helm-package`). Commit the generated files.
4. Add or update tests next to the code (`internal/controller/*_test.go`, `cmd/*_test.go`), and
   run them: `make test`.
5. Update the documentation the change affects, in the same pull request, including the
   `README.md` of each folder you touch.
6. Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat(lake): ...`,
   `fix(cloudflare): ...`).

## The rules the tests enforce

- **One operator, everywhere.** The operator is the same on every cluster: the same chart, image,
  CRDs, controllers and permissions. A cluster's role comes only from the resources that exist on it.
  Do not add a flag, a value or a condition that turns a controller, a kind or a permission on or
  off. `TestNoClusterDiffersFromAnother` and `TestTheChartHasOneCRDDirectoryWithEveryKind` check
  this.
- **Two installation paths, one result.** A management cluster is installed from `helmfile.yaml`;
  the clusters it manages get the same platform from their `ClusterHelmRelease` resources. A change
  to one is made to both, in the same pull request; `cmd/platform_sync_test.go` checks it.
- **Change happens through the operator.** Behaviour belongs in the operator's code or in the custom
  resources it reconciles, never in a manual step on a cluster.

## Contributor License Agreement

orama-operator is owned by OramaSearch, Inc. Every contributor signs the
[Contributor License Agreement](CLA.md) (CLA) once, before their first pull request is merged. Under
it you assign the copyright in your contributions to OramaSearch, Inc., you keep a licence to use
your own contributions for any purpose, and you grant a patent licence for them. The project is
published under the license in [`LICENSE`](LICENSE).

When you open your first pull request, a bot comments on it with a link to the agreement. Sign
there with your GitHub account; it covers all your later contributions to projects of OramaSearch,
Inc. If you contribute as part of your job, your employer signs too: write to
**opensource@orama.com** for the entity agreement.
