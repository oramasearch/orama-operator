# orama-operator

The Kubernetes operator for Orama's cloud. It is a kubebuilder operator in Go, plus a thin
install Helm chart, and it reconciles a small set of custom resources in the `orama.cloud` group
into running systems:

- `OramaZettaCluster`: an Orama Zetta cluster. Zetta is the operator's control plane (its
  datastore for observed state and the intake for desired deployments) and the data plane a
  Lake will own (#125). Every node forms one mTLS cluster from a shared join token.
- `Lake`: the hosted lake service for the liquid project, with its control-plane Postgres, a
  shared libliquid store, sealed-secret delivery, per-change `LakeEnvironment` namespaces, and
  an authenticated REST `/v1` and MCP deployment surface that liquid drives.
- `OramaCloudV1`: the complete, self-contained Orama Cloud v1 stack (engine, dashboard,
  control-plane DB, analytics, cache, broker, ingestion, workers), one CR per cluster.
- The supporting CRDs those own: `PostgresSchema`, `RabbitMQTopology`, `CloudflareDNSRecord`,
  `CloudflareLoadBalancer`, `CloudflareKVNamespace`, `CloudflareR2Bucket`, `LakeSealedSecret`,
  `LakeRestoreRehearsal`, and the two e2e test CRDs.

Kubernetes holds the desired state, Zetta holds the observed state, and a steady stack produces
zero writes. The repository was renamed from `orama-cloud-v1-operator`; the Go module path, image
name and chart name keep the old name until that rename is done as its own deliberate step.

## Status

Runs in production on AKS. `Lake/prod` is live at https://lake.oramadev.net on this operator.
Every `OramaCloudV1` component reconciler is implemented and validated on AKS; the Cloudflare
reconcilers are implemented but not yet live-tested. Per-reconciler status and the remaining
hardening work are in [`docs/components.md`](docs/components.md).

## Build and test

Needs Go, kubebuilder, helm, helmfile and kubectl. Paths and pinned versions are in
[`docs/development.md`](docs/development.md).

```sh
go build ./...
go vet ./...
go test ./...
helm lint chart/
```

After editing `api/v1alpha1/*.go`, run `make generate manifests` and copy the CRDs into
`chart/crds/`; the generated files are committed. The full loop, including `make run` against
your current kube-context, the envtest setup, and the AKS smoke test, is in
[`docs/development.md`](docs/development.md).

## How it is deployed

[`docs/install.md`](docs/install.md) covers the platform operators, installing the chart,
applying a CR, overriding the dummy secrets and images, multi-region, and uninstall.
[`docs/lake/README.md`](docs/lake/README.md) covers deploying the operator with Lake support
and running a Lake. Those steps are not repeated here.

## Documentation

- [`docs/README.md`](docs/README.md): the index of every page.
- [`DESIGN.md`](DESIGN.md): architecture and the decision log.
- [`docs/crd-reference.md`](docs/crd-reference.md): the field reference for every CRD.
- [`docs/components.md`](docs/components.md): what each reconciler creates, and its status.
- [`docs/lake/README.md`](docs/lake/README.md): the `Lake` CR, with
  [`environments.md`](docs/lake/environments.md),
  [`sealed-secrets.md`](docs/lake/sealed-secrets.md), [`backups.md`](docs/lake/backups.md)
  and [`data-services/`](docs/lake/data-services/README.md).
- [`docs/control-plane/README.md`](docs/control-plane/README.md): the deployment control plane
  liquid talks to over REST `/v1` and MCP.
- [`docs/zetta-datastore.md`](docs/zetta-datastore.md) and
  [`docs/zetta-cluster-mtls.md`](docs/zetta-cluster-mtls.md): Zetta as the datastore, and the
  shared cluster join token.
- [`docs/connecting.md`](docs/connecting.md): reaching a deployed `OramaZettaCluster`.
- [`docs/provisioning-runbook.md`](docs/provisioning-runbook.md): the external dependencies.
- [`CLAUDE.md`](CLAUDE.md): the working rules.

The production v1 system this operator models is documented in
[`techops-docs/docs/orama-cloud-v1/`](https://github.com/oramasearch/techops-docs/tree/main/docs/orama-cloud-v1).

## Rules

- This is a production migration. Every change is judged against production-readiness, not
  against whether a pod starts. The full principle is in [`CLAUDE.md`](CLAUDE.md).
- Docs move with the code, in the same change: the CRD reference for a type change, the
  components page for a reconciler change, the development guide for a build or deploy change,
  the `DESIGN.md` decision table for a decision. This README is a summary and never holds the
  only copy of anything.
- PR-based workflow. Never fast-forward `main`; open a PR and leave it open for review.
- No AI attribution in commits or PRs.
- Reuse published `oramasearch/*` images; deploy by immutable tag.
- The ops truth for the production system lives in `techops-docs`, not here.
