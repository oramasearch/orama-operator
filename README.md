# orama-operator

The Kubernetes operator for Orama's cloud. It is a kubebuilder operator in Go, plus a thin
install Helm chart, and it reconciles a small set of custom resources in the `orama.cloud` group
into running systems:

- `OramaZettaCluster`: an Orama Zetta cluster. Zetta is the operator's control plane (its
  datastore for observed state and the intake for desired deployments) and the owned data plane
  provisioned for each Lake. Every node forms one mTLS cluster from a shared join token.
- `Lake`: the hosted lake service for the liquid project, with its control-plane Postgres, a
  shared libliquid store, sealed-secret delivery, per-change `LakeEnvironment` namespaces,
  opt-in isolated workspace namespaces for services, stores, user agents and synchronized apps,
  apps-table synchronization into `LakeApp` workloads, and
  an authenticated REST `/v1` and MCP deployment surface that liquid drives.
- `OramaCloudV1`: the complete, self-contained Orama Cloud v1 stack (engine, dashboard,
  control-plane DB, analytics, cache, broker, ingestion, workers), one CR per cluster.
- The supporting CRDs those own: `PostgresSchema`, `RabbitMQTopology`, `CloudflareDNSRecord`,
  `CloudflareLoadBalancer`, `CloudflareKVNamespace`, `CloudflareR2Bucket`, `ClerkApplication`,
  `NetworkSandbox`, `Connection`, `Tunnel`, `TunnelProvider`, `ZettaTunnel`, `CloudflareTunnel`,
  `PeerConnection`, `PeerLink`,
  `LakeSealedSecret`, `LakeEnvironment`, `LakeApp`, `LakeRestoreRehearsal`, and
  the three e2e test CRDs, including `LakeE2ETest`.
- The infrastructure kinds: `KubernetesClusterProvider`, `KubernetesCluster` and `AksCluster`
  create clusters; `ClusterHelmRelease` and `OramaOperator` install a managed cluster's platform
  layer and its own operator; `LakeSet` and `OramaCloudV1Set` run instances on managed clusters
  from one template.

That is 32 resource kinds, all installed by the chart on every cluster. The release notes cover the
first 25: [v0.1.0](docs/releases/v0.1.0.md) for the first 17 and
[v0.2.0](docs/releases/v0.2.0.md) for the next eight. `Connection` is documented in
[`Declared workspace connections`](docs/infrastructure/connections.md) the four tunnel kinds in
[`Developer tunnels`](docs/infrastructure/tunnels.md) and the two peer kinds in
[`Peer connections`](docs/infrastructure/peer-connections.md), for the next release.

Kubernetes holds the desired state, Zetta holds the observed state, and a steady stack produces
zero writes. The Go module, image, Helm chart, default release, and rendered operator Deployment
are all named `orama-operator`.

## Status

The current release is **v0.2.0** (2026-10-14);
[`docs/releases/v0.2.0.md`](docs/releases/v0.2.0.md) says what is new since v0.1.0, the first.

The operator runs on four AKS clusters. The management cluster, `orama-production-cluster-01`, is
the development environment whatever its name says; it runs `Lake/prod`, live at
https://lake.oramadev.net with its workspace Zettas, apps, agents and services. It manages three
clusters it created: `orama-nonprod-cluster-01` and the production clusters
`orama-production-cluster-02` and `-03`, which hold no customers yet. Where each runs and how they
are operated: [`config/clusters/`](config/clusters/README.md) and
[`docs/infrastructure/operations.md`](docs/infrastructure/operations.md).
Every `OramaCloudV1` component reconciler is implemented and was validated on AKS; the Cloudflare
load balancer and the Karpenter capacity slices are implemented but not yet live-tested.
The owner-scoped `NetworkSandbox` rollout for Lake, workspace and Cloud v1 workloads, including
resolved-address public egress lockdown for Cloud v1 writer and reader pods, is implemented and
native-tested. The 2026-10-14 AKS probe found that metadata traffic bypassed NetworkPolicy `ipBlock`
exclusions; an explicit Cilium deny blocked it while public HTTPS worked. Every sandbox now renders
that mandatory owner-scoped deny for cloud metadata and host-service addresses; generated-policy
rollout verification remains pending. Only the operator manager can write Kubernetes, Cilium or
Orama secure-networking policy; tenant, Lake, app, agent and build identities are denied and covered
by static and native API-server RBAC tests. Shared namespaces leave unrelated pods, including the operator,
outside the sandbox. Lake `Connection` rows now become paired, sandbox-owned ingress and egress;
the broad workspace-internal allowance remains only until every initial row is enforced. Native proof
is complete and the named nonprod connectivity probe remains pending. Details:
[`Network sandboxes`](docs/infrastructure/network-sandbox.md) and
[`Declared connections`](docs/infrastructure/connections.md). Lake tunnel grants become `Tunnel`s carried
by zetta-tunnel (one relay per cluster, one endpoint per grant) or a customer's own Cloudflare account:
[`Developer tunnels`](docs/infrastructure/tunnels.md). Peers exchange named services between clusters
through zetta-tunnel gateways, with credentials only from the managing cluster:
[`Peer connections`](docs/infrastructure/peer-connections.md). zetta-tunnel's ingress gateway runs on every
cluster beside ingress-nginx until each cluster's cutover: [`Ingress`](docs/infrastructure/ingress.md).
Per-reconciler status and the remaining hardening work are in
[`docs/components.md`](docs/components.md).

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
and running a Lake. [`docs/install/k0s.md`](docs/install/k0s.md) is the connected and air-gapped
on-premises path for the liquid k0s distribution. Those steps are not repeated here.
Existing AKS installations that use the former release name must follow the scheduled
[`rename cutover runbook`](docs/operations/rename-cutover.md).

## Documentation

- [`docs/README.md`](docs/README.md): the index of every page.
- [`docs/releases/README.md`](docs/releases/README.md): what each release contains.
- [`DESIGN.md`](DESIGN.md): architecture and the decision log.
- [`docs/crd-reference.md`](docs/crd-reference.md): the field reference for every CRD.
- [`docs/components.md`](docs/components.md): what each reconciler creates, and its status.
- [`docs/infrastructure/README.md`](docs/infrastructure/README.md): management and leaf clusters, the
  design the operator's infrastructure work follows.
- [`docs/install/k0s.md`](docs/install/k0s.md): install one Lake on connected or air-gapped liquid k0s.
- [`docs/lake/README.md`](docs/lake/README.md): the `Lake` CR, with
  [`environments.md`](docs/lake/environments.md), [`apps.md`](docs/lake/apps.md), [`builds.md`](docs/lake/builds.md),
  [`workload-resources.md`](docs/lake/workload-resources.md), [`app-transfer.md`](docs/lake/app-transfer.md), [`methodology-runs.md`](docs/lake/methodology-runs.md),
  [`sealed-secrets.md`](docs/lake/sealed-secrets.md), [`backups.md`](docs/lake/backups.md)
  and [`data-services/`](docs/lake/data-services/README.md).
- [`docs/control-plane/README.md`](docs/control-plane/README.md): the deployment control plane
  liquid talks to over REST `/v1` and MCP.
- [`docs/zetta-datastore.md`](docs/zetta-datastore.md) and
  [`docs/zetta-cluster-mtls.md`](docs/zetta-cluster-mtls.md): Zetta as the datastore, and the
  shared cluster join token.
- [`docs/connecting.md`](docs/connecting.md): reaching a deployed `OramaZettaCluster`.
- [`docs/provisioning-runbook.md`](docs/provisioning-runbook.md): the external dependencies.
- [`docs/operations/rename-cutover.md`](docs/operations/rename-cutover.md): the one-time AKS Helm
  release handoff that preserves CRDs and the leader-election Lease.
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
- **Work lands on `dev`, through a pull request.** `main` moves only when a release is cut, by
  promoting `dev`. Never push to either directly; open a PR and leave it open for review.
- No AI attribution in commits or PRs.
- Reuse published `oramasearch/*` images; deploy by immutable tag.
- The ops truth for the production system lives in `techops-docs`, not here.
