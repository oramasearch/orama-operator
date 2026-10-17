# Install

Install the manager, then reconcile a CPU-only Orama Cloud v1 engine and its broker. Run the
commands from this repository root, in order. The larger v1 samples are described in §4.
For a Lake installation, use [install/k0s.md](install/k0s.md).

## Prerequisites — obtain these before installing

- A Kubernetes cluster (1.29+) and a `kubectl` context with permission to install CRDs, RBAC,
  platform controllers, and namespaced workloads. Use a checkout of this repository compatible
  with the operator image supplied below.
- **Helm 3.x**, **helmfile**, and **kubectl**. These commands use Helmfile's `sync` command;
  they do not require the Helm diff plugin.
- A default **StorageClass** with `ReadWriteOnce`, capacity for the engine, embeddings service,
  and broker, and node network access to the image registries and required model downloads.
  The sample sets `components.engine.writer.gpu: false`; no GPU is required.
- **Private registry access from the Orama release maintainer:** a Docker Hub username and a
  read-only token authorized for `oramasearch/orama-operator` and the selected application images.
  Neither the chart nor helmfile creates this credential. A clean node cannot rely on cached images.
- **A published immutable operator tag** that the supplied identity can pull, built for the
  cluster's Linux architecture and compatible with this checkout's CRDs and RBAC. Set
  `OPERATOR_TAG` to that tag. This page does not supply or promise a published release tag;
  `v1-sandbox` is the chart's mutable default, not the tag to use for this installation.
- For the sample in §3, **compatible published immutable image references** for OramaCore and its
  CPU ai-server from their release maintainer. Set `ORAMACORE_IMAGE` and `EMBEDDINGS_IMAGE` to
  those full references (`repository:tag` or `repository@sha256:digest`). Obtain registry access
  to both; the global application tag does not select these two images. If their model artifacts
  require credentials, arrange those with the maintainer before applying the sample.

If an image or access grant is missing, obtain it before continuing. Maintainers publish the
operator through cluster BuildKit with `buildctl`; the native checks, remote build procedure, and
published-image alternative are in [development.md](development.md#obtain-or-publish-the-operator-image).
Application images come from their owning projects.

> **This page is the helmfile path, which is how a management cluster is installed.** A leaf
> cluster is not installed this way: the management cluster's operator installs it from
> `ClusterHelmRelease` and `OramaOperator` resources. The two paths must install the same thing, so
> a change to `helmfile.yaml` is also a change to every `config/clusters/*/platform.yaml`
> ([`infrastructure/README.md`](infrastructure/README.md#two-installation-paths-one-result)).

Three layers, each with one job:

- **`helmfile.yaml`** composes **CloudNativePG and ingress-nginx**, the **service operators** that
  run lake managed services (label `tier=service-operators`, below), plus the operator chart.
  Select the platform releases first, as below; a bare `helmfile apply` also installs the manager
  using chart defaults without credentials.
- The **chart** installs the **operator Deployment, CRDs, RBAC, ServiceAccount, and manager
  control-plane Service**. It does not deploy the v1 application or create a registry Secret.
- The **operator** reconciles `OramaCloudV1` into the selected workloads and dependency CRs.
  RabbitMQ runs as a StatefulSet with an operator-owned `RabbitMQTopology` CR; there is no
  upstream RabbitMQ operator to install. ClickHouse also runs as a StatefulSet.

## 1. Prepare registry access and platform controllers

Create the release namespace **before** its pull Secret. Enter the supplied credentials in a
Bash session with tracing disabled; do not paste a real token into a manifest or shell history:

```bash
set +x
kubectl create namespace orama-system --dry-run=client -o yaml | kubectl apply -f -
read -r -p 'Docker Hub username: ' ORAMA_DOCKERHUB_USERNAME
read -r -s -p 'Read-only Docker Hub token: ' ORAMA_DOCKERHUB_TOKEN
printf '\n'
: "${ORAMA_DOCKERHUB_USERNAME:?Registry username is required}"
: "${ORAMA_DOCKERHUB_TOKEN:?Registry token is required}"
kubectl create secret docker-registry oramasearch-dockerhub -n orama-system \
  --docker-server=https://index.docker.io/v1/ \
  --docker-username="$ORAMA_DOCKERHUB_USERNAME" \
  --docker-password="$ORAMA_DOCKERHUB_TOKEN"
unset ORAMA_DOCKERHUB_TOKEN
```

For an existing installation, preserve its valid Secret. For an internal registry, use its
server and supplied credentials and change the image repository below. Secrets are namespaced:
this Secret serves pods in `orama-system`; another application namespace needs its own credential.

Write the **non-secret manager values**, using the published tag obtained above:

```sh
: "${OPERATOR_TAG:?Set the published immutable operator tag supplied by the maintainer}"
cat > operator-values.yaml <<EOF_VALUES
operator:
  image:
    repository: oramasearch/orama-operator
    tag: "${OPERATOR_TAG}"
  imagePullSecrets:
    - name: oramasearch-dockerhub
EOF_VALUES
```

`operator.imagePullSecrets` reaches the manager's Pod spec. The application's
`spec.images.pullSecretName` is a separate setting, used in §3. Setting one does not configure
or create the other. See [install/credentials.md](install/credentials.md) for the mapping and
failure diagnosis.

Install the platform controllers first, omitting a selector for any release already provided
by the cluster. CloudNativePG must be available to the manager's CNPG watches even when this
sample does not enable a control DB. ingress-nginx supplies external access for the larger
samples; the minimal engine sample below does not expose an Ingress.

```sh
helmfile -l name=cloudnative-pg -l name=ingress-nginx sync --wait --concurrency 1
```

### Service operators (lake managed services)

Only needed where a `Lake` enables `spec.services` and customers create kinds beyond Zetta and
Postgres (liquid-bridge decision 47; the per-kind contract is lake
`docs/control-plane/services.md`, what this operator writes is [`lake/services.md`](lake/services.md)).
All are pinned and cluster-scoped:

| Release | Chart | Version | Namespace | Runs |
|---|---|---|---|---|
| `cert-manager` | `jetstack/cert-manager` | v1.21.2 | `cert-manager` | K8ssandra's webhook certificates; origin TLS for each Cloud v1 stack's Ingresses, from the namespaced Let's Encrypt `Issuer` the operator renders per stack (#457). Nothing else is installed for it. |
| `strimzi` | `strimzi/strimzi-kafka-operator` | 1.2.0 | `strimzi-system` | Kafka (`STRIMZI_POD_SECURITY_PROVIDER_CLASS=restricted`, so its pods are admitted to a workspace namespace) |
| `clickhouse-operator` | `altinity/altinity-clickhouse-operator` | 0.27.4 | `clickhouse-system` | ClickHouse (`watch.namespaces.include: [".*"]`: empty means its own namespace only) |
| `tidb-operator` | `pingcap/tidb-operator` | v1.6.6 | `tidb-admin` | TiDB (CRDs from the release's `crd.yaml`, applied server-side by a presync hook) |
| `k8ssandra-operator` | `k8ssandra/k8ssandra-operator` | 1.33.0 | `k8ssandra-operator` | Cassandra |
| `valkey-operator` | `valkey/valkey-operator` | 0.6.0 | `valkey-operator-system` | Valkey — **preview**, upstream says not production-ready |

These namespaces, and `cnpg-system` for CloudNativePG, are also the peers of the workspace
NetworkPolicies that let each operator reach its pods in a default-deny workspace namespace
([`lake/workspaces.md`](lake/workspaces.md#datastore-operator-flows-operator268)). Installing an
operator into another namespace needs the matching change in
`internal/controller/lake_workspace_datastore_policies.go`.

```sh
helmfile -l tier=service-operators sync --wait --concurrency 1
kubectl get deploy -A | grep -E 'strimzi|clickhouse|tidb|k8ssandra|cass-operator|valkey|cert-manager'
```

`needs:` installs cert-manager before K8ssandra; rendering K8ssandra alone (`helmfile template`)
fails with "cass-operator webhooks require cert-manager" until cert-manager exists, which is
expected. Upgrading one of them is a version bump here, checked against its release notes for CRD
changes (Valkey 0.6.0 broke its TLS field), then the same `sync`.

The selectors include **only** CloudNativePG and ingress-nginx. The manager is installed next.
Every platform chart is pinned in `helmfile.yaml`, to the same version each leaf's
`ClusterHelmRelease` has; `cmd/platform_sync_test.go` fails on an unpinned release. See [components.md](components.md) for the component mapping.

## 2. Render, install, and wait for the manager

For a new installation, use the current release name. An AKS installation still running the
former release must follow the [rename cutover runbook](operations/rename-cutover.md).

First check the manifest using exactly the values that will be installed:

```sh
helm template orama-operator chart/ --namespace orama-system -f operator-values.yaml \
  | grep -A3 -E "image:|imagePullSecrets"
```

The manager Deployment must contain `imagePullSecrets: [{name: oramasearch-dockerhub}]` and
`image: "oramasearch/orama-operator:<the supplied tag>"`. The pull policy remains `IfNotPresent`.
Without the values file, the chart renders `oramasearch/orama-operator:v1-sandbox`,
`IfNotPresent`, and **no `imagePullSecrets`**; it does not default to `latest`.
Rendering proves the value wiring, not registry access or image publication.

```sh
helm install orama-operator ./chart -n orama-system \
  -f operator-values.yaml --wait --timeout 10m
kubectl -n orama-system rollout status deployment/orama-operator --timeout=10m
kubectl -n orama-system get pods -l app.kubernetes.io/name=orama-operator
kubectl -n orama-system logs deployment/orama-operator --tail=50
kubectl get crd oramacloudv1s.orama.cloud
```

Alternatively, install the **manager release only** through Helmfile with the same chart values:

```sh
helmfile -l app=orama-operator sync --values "$PWD/operator-values.yaml" --wait
```

Run the same rollout and log checks after that alternative. `--values` passes chart values;
`--state-values-file` configures Helmfile's own template state and is not a substitute here.

**The manager becomes pullable at this installation step:** its Pod now references the existing
Secret in `orama-system` and an image the supplied identity can access. Wait for a successful
rollout, a running manager with no restarts, and logs without cache-sync or RBAC errors before
applying a CR. The chart currently has no manager readiness probe, so rollout alone does not
prove that all controllers have started. Pull failures and startup checks are covered in
[install/credentials.md](install/credentials.md#diagnose-a-manager-that-does-not-start).

## 3. Reconcile the CPU engine sample

The minimal sample enables the writer, reader, shared CPU embeddings service, and RabbitMQ
broker used by writer-to-reader replication. It leaves external Cloudflare operations and demo
seeding disabled. It does not enable the dashboard, SaaS integrations, or the rest of the v1
application tier. The operator seeds development component credentials; it does **not** generate
the registry credential. Use an isolated installation for these development defaults.

```sh
: "${ORAMACORE_IMAGE:?Set the supplied immutable OramaCore image reference}"
: "${EMBEDDINGS_IMAGE:?Set the supplied immutable CPU ai-server image reference}"
kubectl apply -n orama-system -f - <<EOF_SAMPLE
apiVersion: orama.cloud/v1alpha1
kind: OramaCloudV1
metadata:
  name: v1
  namespace: orama-system
spec:
  region:
    name: local
    role: primary
  profile: light
  masterAPIKeySecretRef:
    name: v1-master-api-key   # a supplied Secret in orama-system; key MASTER_API_KEY
  idp:
    provider: none
  images:
    pullSecretName: oramasearch-dockerhub
    overrides:
      oramacore: "${ORAMACORE_IMAGE}"
      embeddings: "${EMBEDDINGS_IMAGE}"
  cloudflare:
    enabled: false
  demoSeed: false
  components:
    engine:
      enabled: true
      writer:
        gpu: false
      reader:
        replicas: 1
      envoy: true
    broker:
      enabled: true
      mode: in-cluster
      placement: local
EOF_SAMPLE
kubectl -n orama-system wait --for=condition=Ready ocv1/v1 --timeout=15m
kubectl -n orama-system get ocv1 v1
kubectl -n orama-system get pods,statefulsets,deployments,svc,pvc
```

The `Ready` condition must be true and every entry in `.status.components` ready. If it does
not converge, inspect `kubectl -n orama-system describe ocv1 v1`, workload events, and the manager
logs. Check image access, bound PVCs, model downloads, broker readiness, and node capacity.
The manager's successful rollout and the sample's Ready condition are separate checks.

## 4. Deploy the larger v1 samples and real credentials

[`config/samples/full-stack.yaml`](../config/samples/full-stack.yaml) enables the application tier
and all its in-cluster data stores, including the CNPG control DB. Before applying it, obtain the
compatible published component images and registry grants, enough node/storage capacity, and any
external SaaS credentials required by the enabled features. Copy it to a local manifest, replace
its mutable `images.tag`, set immutable `images.overrides` where needed, and keep
`images.pullSecretName: oramasearch-dockerhub` in the CR's namespace. Then apply that manifest and
repeat §3's readiness checks. It replaces the minimal `v1` CR when applied in the same namespace.

[`config/samples/sandbox.yaml`](../config/samples/sandbox.yaml) enables the app tier while disabling
its data stores. It is a partial smoke-test configuration, not a self-contained stack with those
dependencies running. [`primary-us-east.yaml`](../config/samples/primary-us-east.yaml) and
[`read-eu.yaml`](../config/samples/read-eu.yaml) require the production and peer prerequisites.

Image selection is independent of the manager chart values:

- `spec.images.registry` and `tag` select the operator-specific application images supplied by the
  owning projects, such as dashboard, workers, and ingestion. Obtain published builds first.
- `oramacore` defaults to `oramasearch/oramacore:latest`; `embeddings` defaults to
  `oramasearch/oramacore-ai-server:latest` on CPU, or `oramasearch/oramacore-ai-server-cuda:latest`
  on GPU. They do not use the global `spec.images.tag`. §3 pins both through `overrides`.
- `spec.images.overrides` maps component keys to full image references and wins over defaults.
  The manager image is selected only by `operator.image.repository` and `operator.image.tag`.

Component Secrets are named `v1-<component>-secret` (for example, `v1-engine-secret`). The
operator seeds missing keys with development values while preserving existing values. Supply
real values for enabled integrations; the operator rolls the consumers of a Secret it watches when
a value changes. The broker and Redis passwords and the readers' analytics key are generated
(`v1-broker-admin`, `v1-cache-auth`, `v1-analytics-api-key`), and the engine master key is never generated: name a supplied Secret in
`spec.masterAPIKeySecretRef`, or the writer and readers stay at zero replicas. The external
Cloudflare and SaaS prerequisites are detailed in [provisioning-runbook.md](provisioning-runbook.md);
the field reference is [crd-reference.md](crd-reference.md). These credentials do not grant
registry access.

## Installation and upgrade gotchas

- **Mutable image tags:** the default `IfNotPresent` may reuse a cached tag. Use the immutable
  published operator tag obtained in the prerequisites; inspect running pods' `imageID` after an
  upgrade. Changing pull policy does not supply registry access. Mutable-tag behavior is tracked
  separately in [#103](https://github.com/oramasearch/orama-operator/issues/103).
- **Existing CRDs:** Helm installs files under `crds/` on first installation but does not upgrade
  them. Apply `kubectl apply -f chart/crds/` before an upgrade that changes the CRDs. An installation
  that already manages CRDs separately can use `--skip-crds` — but Helm applies `chart/crds` on
  install only, so an upgrade never adds a new one. See
  [`operations/crd-rollout.md`](operations/crd-rollout.md) and run
  `hack/check-cluster-crds.sh` around every rollout.
- **RBAC and image compatibility:** the manager must watch only kinds installed by the matching
  chart and allowed by its ClusterRole. Cache-sync failures in logs require checking those grants
  and the CRDs; registry credentials cannot fix them.
- **Volume snapshots and growth (operator#259, operator#260):** the ClusterRole reads
  `storageclasses` and manages `snapshot.storage.k8s.io` snapshots, classes and contents (never
  deleting a class or a content). Those grants are harmless where the snapshot CRDs are absent; a
  Lake's `spec.snapshots.enabled` then reports the missing CRDs in lake's `volume_snapshot_status`.
  The `Lake` CRD gains `spec.snapshots`, so apply `chart/crds/` before upgrading. See
  [`lake/volume-snapshots.md`](lake/volume-snapshots.md).
- **Multi-region:** install one operator and one v1 CR per cluster, with one primary and the
  others using the read role. Supply `spec.peers` and the external connectivity, certificates,
  ingress, and Cloudflare credentials appropriate to that deployment; use the read-tier sample
  and [provisioning-runbook.md](provisioning-runbook.md). Shared Zetta behavior is described in
  [zetta-datastore.md](zetta-datastore.md) and [zetta-cluster-mtls.md](zetta-cluster-mtls.md).

## Uninstall

```sh
kubectl delete ocv1 v1 -n orama-system
helm uninstall orama-operator -n orama-system
```

Application PVCs are retained on CR deletion. Review retained storage before explicitly deleting
any claim. The registry Secret created in §1 and the separately installed platform controllers
also remain; remove them only when no remaining workloads need them.
