# orama-operator

**Runs the Orama Lake stack on your own Kubernetes clusters.**

A Kubernetes operator, written in Go with kubebuilder, and the Helm chart that installs it. You
describe a Lake, its datastores and the clusters it runs on as custom resources in the
`orama.cloud` group. The operator creates and keeps everything they need, reports each
component's readiness in the resource's status, and writes nothing while the stack is steady.

Part of [Orama Lake](https://github.com/oramasearch/lake-platform), the sovereign AI stack.
MIT-licensed.

## What it does

- **Lake**: the lake service with its control-plane Postgres and schema, its own Zetta data plane,
  sealed-secret delivery, per-change environments, workspaces as default-deny namespaces, apps,
  builds and backups. [Docs](docs/lake/README.md)
- **Zetta clusters**: `OramaZettaCluster` runs Zetta nodes as one mutual-TLS cluster from a shared
  join token; Zetta also holds the operator's observed state. [Docs](docs/zetta-datastore.md),
  [mTLS](docs/zetta-cluster-mtls.md)
- **Deployment control plane**: an authenticated REST `/v1` and MCP surface that liquid deploys
  through. [Docs](docs/control-plane/README.md)
- **Secure networking**: `NetworkSandbox` policy for every workload, declared `Connection`s,
  developer `Tunnel`s, `PeerConnection`s between clusters, ingress and a node mesh through
  zetta-tunnel; the named connectivity probe on a live cluster is still pending.
  [Docs](docs/infrastructure/network-sandbox.md)
- **Clusters**: `KubernetesCluster` creates clusters through a provider resource (one managed
  provider today), and `ClusterHelmRelease` and `OramaOperator` install a managed cluster's
  platform layer and this operator. [Docs](docs/infrastructure/README.md)
- **Edge, DNS and payments**: abstract kinds (`EdgeZonePolicy`, `PaymentCatalog`,
  `PaymentWebhookEndpoint`, …) backed by concrete provider kinds. [Docs](docs/infrastructure/payments.md)
- **Monitoring**: the platform layer's Prometheus, Alertmanager and Perses on every cluster, with
  no Grafana-family component; alerts are not delivered to a receiver yet.
  [Docs](docs/infrastructure/observability.md)
- **Status pages**: external checks of every host, alerts on a change, and a static public page.
  [Docs](docs/infrastructure/status-pages.md)

Every kind and field is in [the CRD reference](docs/crd-reference.md). What each reconciler creates,
and how far each one has been tested, is in [the components page](docs/components.md); what each
release contains is in [the release notes](docs/releases/README.md). The current release is
[v0.12.0](docs/releases/v0.12.0.md).

## Quickstart

You need a Kubernetes cluster (1.29 or later) whose network plugin is **Cilium**, with
**CloudNativePG** installed: the operator writes Cilium network policy for every workload and runs
its Postgres databases through CloudNativePG, and it does not start without either
([#1200](https://github.com/oramasearch/orama-operator/issues/1200)).

From this checkout, install the CRDs and run the manager on your machine. It reconciles against your
**current kube-context**, so point that at a cluster you mean to change:

```sh
kubectl apply --server-side -f chart/crds/
make run                        # go run ./cmd --control-plane-bind-address=""
```

The manager logs `Starting workers` for each controller. `kubectl get crd | grep orama.cloud`
lists every kind, and `kubectl get lake,ozc -A` the Lakes and Zetta clusters it reconciles.
Creating a `Lake` today also needs the stack's images (see [Known limits](#known-limits)).

To install the operator in the cluster with its chart, follow [`docs/install.md`](docs/install.md);
for one Lake on a single k0s node, connected or air-gapped, follow
[`docs/install/k0s.md`](docs/install/k0s.md).

### Build from source

Needs Go (the version in `go.mod`), Helm 3, and `kubectl`. The tests that start a real API server
need the envtest binaries ([`docs/development.md`](docs/development.md)).

```sh
git clone https://github.com/oramasearch/orama-operator.git
cd orama-operator
go build ./...
go vet ./...
go test ./...
helm lint chart
```

After editing `api/v1alpha1/*.go`, run `make generate manifests` and copy the CRDs into
`chart/crds/`; the generated files are committed.

## How it fits the stack

The operator is what turns the stack's resources into running workloads. lake is the service it
deploys most; zetta is both a datastore it runs for each Lake and where it records what it
observes; liquid deploys environments through its control plane; zetta-tunnel carries the tunnels,
peer links, ingress and mesh it configures.

| Piece | What it does | Relation to this repo |
|---|---|---|
| [liquid](https://github.com/oramasearch/liquid) | Version control, builds, packages, deploys | Deploys through the operator's REST `/v1` and MCP surface |
| [lake](https://github.com/oramasearch/lake) | The AI workspace | Run by the operator (`Lake`), with its schema applied from lake's image |
| [zetta](https://github.com/oramasearch/zetta) | The engine | Run by the operator (`OramaZettaCluster`); the operator's observed-state store |
| [zetta-tunnel](https://github.com/oramasearch/zetta-tunnel) | Tunnels, mesh and ingress | Run and configured by the operator |

## Known limits

- **Images are not public yet.** The built-in default images and the operator's own image are in
  private registries until the public release images are published
  ([#1186](https://github.com/oramasearch/orama-operator/issues/1186)); until then a `Lake` needs a
  pull Secret and image overrides.
- **Tested on two kinds of cluster**: one managed Kubernetes provider and the k0s distribution,
  both with Cilium. A cluster without Cilium or CloudNativePG stops the manager
  ([#1200](https://github.com/oramasearch/orama-operator/issues/1200)).
- **Alerts are not delivered.** Each cluster's Alertmanager evaluates the rules, but no receiver is
  configured since the previous observability stack was removed in v0.12.0
  ([#1192](https://github.com/oramasearch/orama-operator/issues/1192)).
- **Orama Cloud v1 kinds** (`OramaCloudV1`, `OramaCloudV1Set`, `OramaCloudV1Migration`) run Orama's
  previous product and are not part of the Orama Lake release.

## Documentation

- [`docs/README.md`](docs/README.md): the index of every page.
- [`docs/crd-reference.md`](docs/crd-reference.md): every kind and field.
- [`docs/components.md`](docs/components.md): what each reconciler creates, and its status.
- [`docs/install.md`](docs/install.md) and [`docs/install/k0s.md`](docs/install/k0s.md): installing.
- [`docs/development.md`](docs/development.md): the build, test and codegen loop.
- [`docs/infrastructure/README.md`](docs/infrastructure/README.md): managing clusters, and the
  abstract-kind and provider model.
- [`docs/releases/README.md`](docs/releases/README.md): the release notes.

## Community

- Questions and help: [GitHub Discussions](https://github.com/oramasearch/lake-platform/discussions)
- Bugs and requests: [issues](https://github.com/oramasearch/orama-operator/issues)
- Security reports: privately, per [SECURITY.md](SECURITY.md). Please don't open a public issue.

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test and open
a pull request (against `dev`). Contributions are made under the [CLA](CLA.md), signed once. Good
places to start are labelled
[good first issue](https://github.com/oramasearch/orama-operator/labels/good%20first%20issue).

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## License

MIT. See [LICENSE](LICENSE). Third-party components keep their own licenses, listed in
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES).

"Orama Lake", "liquid", "lake", "zetta" and their logos are trademarks of OramaSearch, Inc. The
[trademark policy](https://github.com/oramasearch/lake-platform/blob/main/TRADEMARKS.md) says how
you can use them, including in a fork.
