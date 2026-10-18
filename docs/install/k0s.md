# Install a Lake on the liquid k0s distribution

This runbook starts with a fresh Linux machine, installs the `liquid` branch of
[`oramasearch/k0s`](https://github.com/oramasearch/k0s/tree/liquid), installs CloudNativePG and the
orama-operator chart, and applies one `Lake`. The matching distribution guide is
`docs/liquid/lake-on-k0s.md` in that branch; its `hack/lake-on-k0s/install.sh` automates the
connected procedure below.

The k0s and air-gapped executions for operator issue #123 are deferred because this development
host has no Linux cluster or VM. The manifests, controller wiring, generated CRDs, and native Go
tests are verified in this repository; a real host must fill in the run records at the end.

## Fresh-machine requirements

Use a dedicated Linux host supported by k0s, with no existing `/var/lib/k0s`. Install Git, curl,
Helm 3, GNU Make, and Go. Obtain these artifacts at immutable revisions:

- the `liquid` branch of `oramasearch/k0s`, or a k0s binary built from that branch;
- this repository and its `chart/` directory;
- published immutable tags for `oramasearch/orama-operator` and `oramasearch/lake`;
- a published Lake e2e runner image only when the `LakeE2ETest` proof will be run.

An upstream k0s binary is not equivalent. The liquid distribution declares Cilium, Gateway API,
`local-path`, and the node-side liquid components used by this installation.

## 1. Install the liquid k0s binary

Build on the Linux host, or transfer a binary built from the same revision:

```sh
git clone --branch liquid --single-branch git@github.com:oramasearch/k0s.git
cd k0s
make DOCKER="" EMBEDDED_BINS_BUILDMODE=none BUILD_GO_LDFLAGS_EXTRA="" build
sudo install -m 0755 ./k0s /usr/local/bin/k0s
sudo k0s config create | sudo tee /etc/k0s/lake-on-k0s.yaml >/dev/null
sudo k0s install controller \
  --config=/etc/k0s/lake-on-k0s.yaml \
  --enable-worker \
  --no-taints \
  --start
```

Wait for Kubernetes and every declared distribution component:

```sh
sudo k0s kubectl get --raw=/readyz
sudo k0s kubectl wait --for=condition=Ready node --all --timeout=15m
sudo k0s liquid components
```

Every enabled row from `k0s liquid components` must report `deployed`; explicitly replaced or
disabled components report `external` or `disabled`.

## 2. Create namespaces and private-image credentials

The operator and Lake images are private at the time of writing. k0s issue #13 requires the same
pull-only Docker Hub credential in both namespaces: `orama-system` for the manager and `lake-dev`
for the Lake. Do not put the token in a manifest or shell history.

```bash
export ORAMA_DOCKERHUB_USERNAME=orama-reader
read -r -s ORAMA_DOCKERHUB_TOKEN
export ORAMA_DOCKERHUB_TOKEN
sudo k0s kubectl create namespace orama-system
sudo k0s kubectl create namespace lake-dev
for namespace in orama-system lake-dev; do
  sudo --preserve-env=ORAMA_DOCKERHUB_USERNAME,ORAMA_DOCKERHUB_TOKEN \
    k0s kubectl create secret docker-registry oramasearch-dockerhub \
    --namespace="$namespace" \
    --docker-server=https://index.docker.io/v1/ \
    --docker-username="$ORAMA_DOCKERHUB_USERNAME" \
    --docker-password="$ORAMA_DOCKERHUB_TOKEN"
done
unset ORAMA_DOCKERHUB_TOKEN
```

Skip the Secrets only when all referenced images are public or already present in the node's
containerd content store. For an authenticated internal mirror, create the same Secret names with
that mirror's server and credentials.

## 3. Install CloudNativePG and the operator chart

The liquid distribution proof pins the CloudNativePG chart to `0.29.0`, whose operator is `1.30.0`.
Write a protected admin kubeconfig so Helm talks to k0s, then install the dependency and operator:

```sh
sudo k0s kubeconfig admin > k0s-admin.conf
chmod 600 k0s-admin.conf
export KUBECONFIG="$PWD/k0s-admin.conf"
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm repo update cnpg
helm upgrade --install cloudnative-pg cnpg/cloudnative-pg \
  --namespace cnpg-system \
  --create-namespace \
  --version 0.29.0 \
  --wait \
  --timeout 15m
helm upgrade --install orama-operator /absolute/path/to/orama-operator/chart \
  --namespace orama-system \
  --set operator.image.repository=oramasearch/orama-operator \
  --set operator.image.tag=<immutable-tag> \
  --set 'operator.imagePullSecrets[0].name=oramasearch-dockerhub' \
  --wait \
  --timeout 15m
```

Helm installs CRDs only on the first chart installation. Apply regenerated CRDs explicitly before
upgrading an existing release:

```sh
sudo k0s kubectl apply -f /absolute/path/to/orama-operator/chart/crds/
```

## 4. Apply one Lake

The single-node distribution uses its `local-path` class. A one-replica Lake may use
`ReadWriteOnce`; a multi-node production Lake needs replicated storage and `ReadWriteMany` for its
shared libliquid store. `LAKE_LOG_MODE=air-gapped` avoids any transparency-log anchoring endpoint.

```yaml
apiVersion: orama.cloud/v1alpha1
kind: Lake
metadata:
  name: hello
spec:
  namespace: lake-dev
  image: oramasearch/lake:<immutable-tag>
  pullSecretName: oramasearch-dockerhub
  replicas: 1
  store:
    size: 100Gi
    storageClassName: local-path
    accessModes:
      - ReadWriteOnce
  env:
    - name: LAKE_LOG_MODE
      value: air-gapped
  postgres:
    instances: 1
    storageSize: 50Gi
    storageClassName: local-path
```

Save that object as `lake.yaml`, apply it, and wait for the components and overall Lake readiness:

```sh
sudo k0s kubectl apply -f lake.yaml
sudo k0s kubectl wait -n lake-dev \
  --for=condition=Ready cluster.postgresql.cnpg.io/lake-hello-controldb \
  --timeout=15m
sudo k0s kubectl rollout status -n lake-dev deployment/lake-hello --timeout=15m
sudo k0s kubectl wait lake/hello \
  --for=jsonpath='{.status.schemaPhase}'=Applied \
  --timeout=15m
sudo k0s kubectl wait lake/hello --for=condition=Ready --timeout=15m
sudo k0s kubectl get lake hello
```

Ready means the Lake row reports `READY=1`, `DB=1/1`, `SCHEMA=Applied`,
`STORE=lake-hello-store`, and `PHASE=Ready`, with `Ready=True`. The Deployment must have
observed its current generation and have the required ready, updated, and available replicas;
old ready pods during a rollout do not satisfy this check. The store must be Bound, all configured
Postgres instances must be ready, and the schema must be Applied for its current generation and
current version. The enabled Zetta data plane must also be Ready. Enabled backup configuration
must be valid; backup completion is reported separately.

Pending dependencies or rollout work report `Progressing`. A failed schema or Deployment, degraded
Zetta, or invalid backup configuration reports `Degraded`; inspect `status.conditions[Ready]` for
the explanation. CNPG remains polled every 20 seconds after success, so a later database readiness
loss updates the Lake row even with backups disabled. Unchanged polls write no status. The full
contract is in the [Lake reference](../crd-reference.md#lake-separate-crd-cluster-scoped--the-lake-service).
Also inspect the namespaced objects:

```sh
sudo k0s kubectl -n lake-dev get pods,pvc,service
sudo k0s kubectl -n lake-dev get postgresschema lake-hello-controldb-schema
```

For a machine-local HTTP check without external routing:

```sh
sudo k0s kubectl -n lake-dev port-forward service/lake-hello 4900:80
curl -fsS http://127.0.0.1:4900/readyz
```

The distribution's `hack/lake-on-k0s/install.sh` also creates a Cilium Gateway API `NodePort` and
prints the reachable node address. It deliberately leaves the Lake nginx Ingress off: there is no
cloud load balancer or automatic public DNS on this path.

## 5. Run the Lake service test

Use the runner image `oramasearch/lake-e2e` at an immutable tag, mirrored into the private
registry ([`../crd-reference.md`](../crd-reference.md#lakee2etest-separate-crd-cluster-scoped)). It
signs up a fresh person, pushes and pulls a repository with the real liquid client, and runs the
rest of the suite. This Lake belongs to no set, so the test names it with `targetRef`:

```sh
cat <<'YAML' | sudo k0s kubectl apply -f -
apiVersion: orama.cloud/v1alpha1
kind: LakeE2ETest
metadata:
  name: hello
spec:
  targetRef: hello
  image: <mirrored-runner-image>
YAML
sudo k0s kubectl get lakee2e hello --watch
sudo k0s kubectl logs -n lake-dev job/hello-e2e-1
```

The controller does not create the Job until the target Lake, Postgres, and schema are ready.

## Air-gapped variant

Prepare the k0s binary, chart archives, manifests, and OCI image bundle on a connected Linux staging
host of the same architecture. Transfer them over the customer's approved offline channel. Do not
build from source or add Helm repositories on the disconnected machine.

Start the image list with everything declared by the exact liquid k0s binary, then append every
image outside the distribution:

```text
<output of: k0s airgap list-images --all>
ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0
ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie
postgres:18.6-alpine
oramasearch/orama-operator:<immutable-tag>
oramasearch/lake:<immutable-tag>
oramasearch/lake-e2e:<immutable-tag>  # only when running LakeE2ETest
```

These CloudNativePG references are the defaults for chart `0.29.0` / operator `1.30.0`. If that
chart changes, render it and read the matching operator release's `DefaultImageName` before making
the bundle. `postgres:18.6-alpine` runs the Lake schema Job. Preserve every digest selected while
building the bundle and record it in the installation change log.

The liquid k0s bundler reads OCI registry authentication from a standard registry configuration.
After authenticating it to the private sources on the connected staging host:

```sh
k0s airgap bundle-artifacts \
  --concurrency=1 \
  --platform=linux/amd64 \
  --output=image-bundle.tar \
  < airgap-images.txt
```

Transfer `image-bundle.tar`, the liquid k0s binary, the packaged operator chart, the packaged
CloudNativePG `0.29.0` chart, and `lake.yaml`. Before starting k0s, place the bundle where its
containerd importer watches:

```sh
sudo install -d -m 0755 /var/lib/k0s/images
sudo install -m 0644 image-bundle.tar /var/lib/k0s/images/image-bundle.tar
```

Install both charts from their local archives. Keep the original image references in the values and
Lake manifest when using the imported bundle; containerd resolves them locally with
`IfNotPresent`. If policy requires an internal registry instead, rewrite every reference to that
registry and create `oramasearch-dockerhub`-named pull Secrets in both `orama-system` and `lake-dev`
using the internal registry credentials.

An air-gapped Lake needs no `lake.live` access. Keep `ingressEnabled` false, omit `host` and
`dnsSecretRef`, keep `LAKE_LOG_MODE=air-gapped`, and do not configure a TSA or transparency-log
aggregator that points outside the site. Deny public egress at the site boundary, repeat the Ready
and optional `LakeE2ETest` checks above, and capture firewall evidence showing no DNS lookup or
connection to `lake.live` succeeded.

## Run records

### Connected single-node k0s

Deferred: no Linux cluster or VM is available from this host.

- Host and kernel:
- liquid k0s commit and binary digest:
- operator chart, image tag, and digest:
- Lake image tag and digest:
- `kubectl get lake hello` output:
- HTTP readiness response:
- `LakeE2ETest` output:

### Air-gapped

Deferred: no isolated Linux cluster or VM is available from this host.

- Host and kernel:
- image-bundle digest and image inventory:
- local chart digests:
- egress-denial evidence:
- `kubectl get lake hello` output:
- HTTP readiness response:
- `LakeE2ETest` output:
