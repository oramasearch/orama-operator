# The `Lake` CR

What the operator does for a `Lake`, how it is deployed, and how to run it. Field reference:
[`../crd-reference.md`](../crd-reference.md) (`Lake` section). lake's own docs (server, schema,
deployment state): https://github.com/oramasearch/lake/tree/main/docs.

## What a Lake reconciles into

In order, every reconcile (`internal/controller/lake_controller.go`):

1. **Namespace** `spec.namespace` — created with a blind `Create` (AlreadyExists ignored), never
   owned, never deleted: the operator holds `namespaces: get, create` only (operator#106: no
   Namespace informer).
2. **Admin token** — Secret `lake-<name>-admin` (key `token`, 64 hex), minted once, owned;
   injected as `LAKE_ADMIN_TOKEN`. lake's `/v1` capabilities and MCP tool calls require it as a
   bearer.
3. **Deployment** `lake-<name>` — `spec.image` run as built, `spec.args`, port `spec.port`
   (default 4900), readiness `GET /`, `spec.replicas`, `imagePullSecrets` from
   `spec.pullSecretName`; env `LAKE_CONTROL_DB_URI` / `LAKE_CONTROL_DB_PASSWORD` from the CNPG
   app Secret (optional refs).
4. **Service** `lake-<name>` — ClusterIP 80 → `spec.port`.
5. **Control-plane Postgres** — CloudNativePG `Cluster` `lake-<name>-controldb`
   (`spec.postgres`: enabled by default, instances, storageSize 50Gi, storageClassName), the same
   unstructured path `OramaCloudV1` uses (`controldb.go`). Disabling tears it down **with its
   data**.
6. **Schema** — ConfigMap `lake-<name>-controldb-migrations` + `PostgresSchema`
   `lake-<name>-controldb-schema` applying `assets/lake-schema.sql` (vendored byte-identical from
   `oramasearch/lake` `db/schema.sql`), guard table `customers`, version = content hash.
   Single-shot today; incremental migrations are #126.
7. **Ingress + DNS** (opt-in: `ingressEnabled` + `host`, `dnsSecretRef`) — nginx Ingress for the
   whole host and an owned `CloudflareDNSRecord`, the `OramaZettaCluster` contract; gating off
   tears both down.

**Status** mirrors the Deployment (`readyReplicas`, `phase`), the CNPG cluster (`postgresReady`,
`postgresInstances`) and the PostgresSchema (`schemaPhase`, `schemaVersion`); written only on a
real transition. The CNPG cluster is not watched: the Lake **requeues every 20 s** until the DB is
ready and the schema Applied. `kubectl get lake` columns: Namespace, Host, Ready, DB, Schema,
Phase.

**Owned kinds** (`SetupWithManager`): Deployment, Service, Ingress, CloudflareDNSRecord,
ConfigMap, Secret, PostgresSchema. Not owned: the Namespace, the CNPG Cluster (polled).

**RBAC.** The chart ClusterRole is hand-maintained (`chart/templates/rbac.yaml`); a Lake needs
`lakes`, `lakes/status`, `lakes/finalizers`, `namespaces: create`, plus the shared grants for
deployments, services, configmaps, ingresses, cloudflarednsrecords, postgresschemas and
`postgresql.cnpg.io/clusters`. Keep it in sync with `config/rbac/role.yaml` or the manager
CrashLoops on cache sync.

## Deploying the operator with Lake support

The Lake CR lives on PR #118 (branch `feat/lake-crd`) until merged; production AKS runs that
branch's image. Build, push, refresh CRDs, upgrade:

```sh
docker buildx build --platform linux/amd64 -t oramasearch/orama-operator:lake-crd-<sha> --load .
docker push oramasearch/orama-operator:lake-crd-<sha>
kubectl apply -f config/crd/bases/orama.cloud_lakes.yaml        # helm never upgrades crds/
helm upgrade orama-cloud-v1-operator ./chart -n orama-system \
  --set operator.image.repository=oramasearch/orama-operator \
  --set operator.image.tag=lake-crd-<sha> \
  --set 'operator.imagePullSecrets[0].name=oramasearch-dockerhub'
```

Use immutable tags (the `v1-sandbox` + `IfNotPresent` stale-cache failure is documented in
[`../install.md`](../install.md)). The first upgrade after a `kubectl set image` digest pin needs
`--force-conflicts`: helm 4 applies server-side and the `image` field is owned by `kubectl-set`.

## Running a Lake

```sh
kubectl create namespace lake
# copy the pull Secret and the Cloudflare zone token into it, then:
kubectl apply -f - <<'Y'
apiVersion: orama.cloud/v1alpha1
kind: Lake
metadata: { name: prod }
spec:
  namespace: lake
  image: oramasearch/lake:<tag>
  pullSecretName: oramasearch-dockerhub
  args: ["--listen", "0.0.0.0:4900", "--node", "prod"]
  replicas: 3
  postgres: { instances: 1, storageSize: 50Gi }
  ingressEnabled: true
  host: lake.oramadev.net
  dnsSecretRef: { name: cloudflare-api-token }
Y
kubectl wait lake/prod --for=jsonpath='{.status.schemaPhase}'=Applied --timeout=600s
kubectl get lake prod
```

`kubectl delete lake <name>` removes everything owned, including the database.

## Current production instance

`Lake/prod`, namespace `lake`, 3 replicas, 50Gi Postgres, schema `0001_identity_and_customers`
applied, https://lake.oramadev.net; operator helm revision 8, `lake-crd-8058ab5` (2026-09-02).
Details and the not-yet list: https://github.com/oramasearch/lake/blob/main/docs/deployment/aks.md.

## Open

- #125 Zetta data plane (owned `OramaZettaCluster` per Lake).
- #126 incremental migrations in `PostgresSchema`.
- #122 hardening: resources, liveness, PDB, TLS.
- #119 land PR #118; #120 module/chart/release rename.
