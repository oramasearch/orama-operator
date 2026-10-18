# Lake deployment environments

How a deployment written to the operator's Zetta control plane becomes an isolated Kubernetes
environment, how status returns to the caller, and how teardown is made safe. Field definitions are in
the [`LakeEnvironment` CRD reference](../crd-reference.md#lakeenvironment-separate-crd-namespaced--one-desired-deployment).

## Every Lake's control plane is its own Zetta

Deployments are on for every Lake ([operator#557](https://github.com/oramasearch/orama-operator/issues/557),
under [#539](https://github.com/oramasearch/orama-operator/issues/539)), with nothing set per Lake:

- **The Lake's Zetta carries the operator schema.** `lake-<name>-zetta` is created with
  `spec.provisionOperatorSchema: true`, so its namespace holds exactly one operator-schema Zetta: the
  one the deployment REST/MCP contract selects and the deploy sync below polls. Service and workspace
  Zettas stay `false`, so they are never counted.
- **lake is connected to it.** The lake pod gets `LAKE_OPERATOR_ZETTA_URL` (the same in-cluster URL as
  `LAKE_DATA_PLANE_URL`) and `LAKE_OPERATOR_ZETTA_TOKEN` (the same Secret reference as
  `LAKE_DATA_PLANE_TOKEN`, `lake-<name>-zetta-zetta-admin` key `token`). Both are reserved: `spec.env`
  cannot set them. The token is the one lake already holds for this cluster, so lake gains no access.
- **lake has a name for its rows.** lake reads and writes `orama_control` rows with
  `lake = LAKE_PUBLIC_HOST` and refuses to start the client without it. `spec.env` keeps a
  `LAKE_PUBLIC_HOST` it states; otherwise the operator gives `spec.host`. A Lake with neither gets no
  operator Zetta variables, and its `FeaturesReady` condition is `False` with reason `NoPublicHost`; a
  Lake with `spec.zetta.enabled: false` gets reason `ZettaDisabled`.
- **lake's tables exist.** lake's platform sync reads `deployments`, `environment_env` and `domains`
  on every pass and stays degraded if any read fails, so the operator schema creates `environment_env`
  and `domains` beside `deployments`.
- **The rows are lake's shape** (operator#560, below): every row the operator writes is keyed by the
  Lake's public host and carries every column lake selects, so lake's reads succeed and show the
  operator's status.

With that, lake's `server_info` answers `deployments: true` and `/healthz` reports `platform: ready`.

**Option not taken:** a second, small control Zetta per Lake. It would add a StatefulSet, a volume and
a new credential to every Lake for state that fits on the Zetta lake already uses, and lake's contract
only asks for "the operator Zetta service URL".

## The row contract: lake's format, the operator conforms

[operator#560](https://github.com/oramasearch/orama-operator/issues/560), decided by Steve on 2026-10-17:
lake's format is the contract and the operator conforms. lake's side is
`include/lake/control/zetta_control.hpp` (the columns), `src/control/platform.cpp` (`PlatformService::sync`,
the reads and the row order) and `docs/api/platform.md` in `oramasearch/lake`. The operator's copy of
the columns is `lakeDeploymentColumns`, `lakeEnvironmentEnvColumns` and `lakeDomainColumns` in
`internal/controller/zetta_deployments.go`; `zetta_lake_contract_test.go` runs lake's own SQL strings
against the rows the operator writes.

**How a row names its Lake.** Every row's `lake` column is the Lake's `LAKE_PUBLIC_HOST`: the literal
value in `spec.env` when it states one, otherwise `spec.host` (the same value the operator gives the lake
pod). lake reads `WHERE lake = <that host>`, and so does the operator. A Lake whose `LAKE_PUBLIC_HOST`
comes from a Secret or ConfigMap has no host the operator can know, so it gets no deployments (the
deployment contract answers `unavailable` and says so). `LakeEnvironment` resources keep being named by
the Lake's **resource** name (`spec.lakeRef`, `lakeEnvironmentResourceName(lake.Name, repository,
environment)`), so a host change renames no environment.

**Why every row carries every column.** Zetta stores rows as schemaless JSON and refuses a `SELECT`
that names a column no row of the table has ([zetta#1218](https://github.com/oramasearch/zetta/issues/1218)).
So the operator writes every column on every row, desired and status alike, and the `CREATE TABLE` list
only documents them: on a Zetta where the table exists the statement answers "already exists".

### `orama_control.deployments`

| Column | Desired row (the deployment contract) | Status row (the operator) |
|---|---|---|
| `operator` | This operator's ID (`ORAMA_OPERATOR_ID`, else the `kube-system` Namespace UID) | Same |
| `lake` | The Lake's public host | Same |
| `repository`, `environment`, `kind`, `change`, `image` | The deployment | Copied from the desired row |
| `deployment_id` | The action ID (`deployment-<hash>`) | Same |
| `requested_by` | `lake-admin-token/<lake>`: the contract authenticates the Lake's admin token, not a person | Copied |
| `requested_at` | When it was requested | Copied |
| `phase` | `Pending` | `Progressing`, `Ready`, `Degraded` or `TornDown` |
| `namespace` | Empty | The workload Namespace `lake-<lake>-env-<hash>` (lake's log page queries Loki by it) |
| `url`, `message` | Empty | The environment's URL and `Ready` condition message |
| `updated_at` | Lake's format: UTC, exactly six fractional digits | Now, and at least one microsecond after the newest row of the deployment |
| `ordering_token` | A new UUID | A new UUID |
| `record_type` | `desired` | `status` |
| `replicas`, `port`, `env_json`, `secret_refs_json`, `host` | The workload (the contract gives `1`, `8080`, `[]`, the secret references, empty) | Copied |
| `withdrawn`, `change_closed` | Lifecycle signals, `false` from the contract | Copied |

lake shows the newest row per `deployment_id` by `(updated_at, ordering_token, row)` and refuses its whole
sync on a row without `deployment_id`, `repository`, `environment` or `updated_at` ("incomplete platform
row", which degrades `healthz.platform`). That is why a status row repeats the desired row and is timed
after it: lake then shows the status. The operator reduces the same rows in the same order: the newest
`desired` row per deployment is what it materializes, and the newest `status` row is what it compares
before appending another, so a steady environment writes nothing.

### `orama_control.environment_env`

lake appends `lake, repository, environment, key, value, deleted, set_by, updated_at, ordering_token`
(`set_environment_env`, `unset_environment_env`). The operator only reads it: the newest row per
`(repository, environment, key)`, tombstones (`deleted = true`) dropped, becomes the environment's
variables. They are added to the `LakeEnvironment`'s `spec.env` after the deployment row's own `env_json`
and win for the same name, so a set or an unset rolls the workload. A key containing `=` is skipped.

### `orama_control.domains`

lake appends a request `lake, repository, environment, host, tls, status = 'pending', message = '',
deleted, requested_by, updated_at, ordering_token` (`create_domain`, `delete_domain`). The operator reads
the newest row per `(repository, environment, host)`:

- **Applied.** A live request becomes an entry of the `LakeEnvironment`'s `spec.domains` (`host`, `tls`
  `acme` or `provided`) when the host is a lowercase DNS name, is not the Lake's own host, and is not a
  domain of another environment of the same Lake (the first in `(repository, environment, host)` order
  keeps it). The environment serves it from its Ingress (one rule per host) and, when the Lake has
  `spec.dnsSecretRef`, keeps one proxied `CloudflareDNSRecord` for it, owned by the environment and
  labelled with it.
- **Removed.** A tombstone takes it out of `spec.domains`; the environment deletes its Ingress rule and
  its DNS record. Deleting the environment deletes them all.
- **Answered.** The operator appends a row with lake's columns and its own `status` and `message`
  whenever they differ from the newest row: `failed` with the reason when refused; `pending` with
  "waiting for a deployment of environment ..." while the environment has no deployment, and with what
  it waits for (the ingress address, the DNS record) while it is applied; `active` once the DNS record
  is Ready; `deleted` once a tombstone is applied. Without a `dnsSecretRef` the domain stays `pending`,
  naming the address to point the host at. The `LakeEnvironment` reports the same per domain in
  `status.domains`.

lake and the operator both append rows for one domain and the newest wins, so the operator times an
answer **one microsecond after the row it answers**, not by its own clock: an answer timed by a clock
ahead of lake's would sort after lake's next request and hide it.

TLS: the environment's Ingress has no TLS section, as for its own host; HTTPS is served by Cloudflare's
proxy in front of it. `tls` is carried in `spec.domains` for when the Ingress terminates TLS itself.

## Control-plane model

Liquid-bridge decision 22 makes Zetta, not Lake's HTTP API, the deployment intake path. An
`OramaZettaCluster` with `spec.provisionOperatorSchema` enabled creates the append-only
`orama_control` tables. The deployment contract writes one `desired` row; the operator reads lake's
three tables over Zetta's authenticated `/v1/sql` capability and creates or updates a `LakeEnvironment`.
The pull/status implementation is `internal/controller/zetta_deployments.go`; it runs from the existing
`OramaZettaCluster` reconcile loop after operator-schema provisioning.

The operator reads a Lake's rows only when both of these are true:

- the cluster-scoped `Lake` exists locally and has a public host, which its rows are keyed by; and
- that Lake's `spec.namespace` is the same namespace as the `OramaZettaCluster` being polled.

This locality rule prevents a Zetta peer in another namespace from creating a second copy. The
Lake/repository/environment identity deterministically names the `LakeEnvironment`, so polling the same
desired row again does not create or update anything. A later deployment for that identity updates the
same resource; older resources from the deployment-ID naming scheme are deleted through their verified
finalizers. A legacy-named CR retains deployment-ID Namespace derivation during teardown, so its
finalizer can delete only the Namespace it originally owned. The materializer refuses to overwrite or
delete a stable-name collision that is not controlled by the referenced Lake.

The Zetta admin bearer is read from `<zetta-name>-zetta-admin`; unauthenticated fallback is not permitted.

Each Zetta controller polls once per local Lake (three `SELECT ... WHERE lake = <host>`), at the shortest
`spec.deploySync.interval` among Lakes in its namespace, defaulting to `15s`. Kubernetes workload status
changes also reconcile the `LakeEnvironment`; the next poll returns the transition to Zetta. A failure
to reach Zetta is retried on the next poll and does not destabilize the Zetta StatefulSet.
Positive intervals below `1s` are clamped to `1s` to prevent a configuration mistake from creating a
busy poll loop.

## What one `LakeEnvironment` creates

The CR lives in the Lake's namespace and carries an owner reference to the cluster-scoped Lake. It
creates a dedicated Namespace named `lake-<lake>-env-<environment-hash>`. A namespaced CR cannot own a
cluster-scoped Namespace or objects in another namespace through Kubernetes owner references, so every
object carries `orama.cloud/lake-environment=<cr-namespace>/<cr-name>` and teardown is guarded by a
finalizer that verifies that exact annotation before deleting the Namespace.

The Namespace profile matches k0s decision 31:

- `liquid.oramasearch.com/environment=true`, which selects both k0s environment admission policies;
- Pod Security Standards `restricted` in enforce, audit and warn modes, all at `latest`;
- default-deny ingress and egress `NetworkPolicy`;
- one narrow policy admitting nginx ingress traffic to the application port and DNS traffic to
  `kube-system` CoreDNS only;
- `ResourceQuota`: 50 pods, 20 services and PVCs, 4 requested / 8 limited CPU, 8 GiB requested /
  16 GiB limited memory;
- `LimitRange`: 100m / 128Mi default request and 1 CPU / 1Gi default limit.

The workload is a Deployment plus ClusterIP Service. Every pod uses `runtimeClassName: untrusted`, the
k0s Kata/Firecracker contract, and the restricted container security context. The Deployment retains ten
ReplicaSet revisions so rollback is a normal spec update from a new desired event naming an older change
and immutable image digest. Readiness, liveness and startup probes use the declared TCP port.

Liquid's public deployment contract is served by the operator; see
[`../control-plane/README.md`](../control-plane/README.md). Contract-created environments default to one
replica and port `8080`. They are named environments unless the environment name exactly equals the
change digest, which selects a preview.

## Hosts and DNS

Ingress is enabled only when the referenced Lake has `spec.ingressEnabled: true`.

- A named environment uses its exact `spec.host`. Once its Ingress has an address, the operator creates
  one `CloudflareDNSRecord` in the Lake namespace using the Lake's `dnsSecretRef`.
- Each `spec.domains` entry (lake's custom domains, above) is another rule of the same Ingress and, with a
  `dnsSecretRef`, another `CloudflareDNSRecord`, for named and preview environments alike. Its state is in
  `status.domains`.
- A preview derives `<first-12-DNS-safe-change>.<previewWildcard>`. The Lake owns one
  `*.<previewWildcard>` Cloudflare record against the shared nginx load balancer. Preview environments
  create no individual DNS records.

The status URL is present while the host is configured, including while replicas are still progressing.

## Sealed secrets

`spec.secretRefs` accepts a `LakeSealedSecret` resource name or the equivalent
`secret://<resource-name>` reference emitted by liquid repository configuration. Before creating the
workload, the controller verifies that every referenced resource exists in the Lake namespace, belongs
to the same Lake, reports `Ready` for its current generation, and owns its produced Secret. An unknown,
foreign, stale, or undelivered reference keeps the environment `Degraded` with a specific reason and
removes its workload so it cannot start without the requested credential.

Each delivered Secret is copied into the isolated workload namespace without changing its name or
contents. The application container imports the copies with `envFrom`. Its Pod template carries
`liquid.oramasearch.com/sealed-secret-names`, a comma-separated list of the exact resolved Secret
names in `envFrom` order. These can differ from the `LakeSealedSecret` reference names. Updates and
removals rebuild the list; removing every reference removes the annotation.

The copy carries `lake.orama.cloud/sealed=true`, the environment labels and ownership annotation,
and a controller reference to a **local** `LakeSealedSecret`. This local resource has the same name as
the copied Secret, its own UID, and a controller reference to the cluster-scoped Lake. Its
`source.type: delivery` binds it to the `LakeEnvironment` and original sealed-secret name in the Lake
namespace. It contains no plaintext or copied ciphertext: the source envelope's namespace-bound AAD
is unchanged. Neither the Secret nor its owner has an invalid cross-namespace owner reference.

Both controllers use the environment reconciliation path for these local deliveries, retaining the
uncached Namespace reader so the operator needs no Namespace list/watch grant. It verifies the
current environment authorization and original source, copies rotations into the existing Secret,
and reports Ready only after delivery succeeds. A steady delivery does not rewrite Secret or owner
status. An unauthorized local delivery removes only Secrets it controls and reports `DeliveryInvalid`.
An unrelated local resource or Secret is never adopted. The earlier unowned copies carrying the
matching environment annotation acquire their local owner on reconciliation.

Copies and local delivery owners are deleted when a reference is removed or becomes invalid.
Deleting the environment Namespace removes every copy and local owner without changing the source
`LakeSealedSecret` or its Secret in the Lake namespace.

### Admission and tenant access

Both `liquid-environment-secrets` and `liquid-environment-pods` apply **only** to namespaces carrying
`liquid.oramasearch.com/environment=true`. The operator adds this label on creation and repairs it
on subsequent reconciliation, including existing environments. The Secret policy requires the sealed
label and a `LakeSealedSecret` controller reference; the Pod policy requires the annotation for Secret
volume and environment references. Neither policy validates owner existence, the referenced Secret,
or the annotation's author.

Under [liquid-bridge decision 38](https://github.com/oramasearch/liquid-bridge/blob/main/docs/decisions.md#38-who-may-write-secrets-in-a-liquid-environment--decided-steve-2026-09-07-rbac-is-the-control-admission-is-defence-in-depth),
RBAC is authoritative and admission is defence in depth. The
[k0s tenant contract at PR #41](https://github.com/oramasearch/k0s/blob/cc922054ad10fd1991ab1077bb8a2c3bf3eeeae2/docs/liquid/platform-surface.md#environment-policy)
grants the `liquid:tenant:<namespace>` group workload lifecycle and selected reads, with **no Secret
access**. The profile's RoleBinding must be the tenant's only grant there: RBAC is additive. The
operator uses its separately installed service account permissions. Tenant RBAC remains the k0s
profile's responsibility; this controller does not introduce a second tenant grant definition.

### Upgrade order

Install the updated `LakeSealedSecret` CRD from
`chart/crds/orama.cloud_lakesealedsecrets.yaml` before rolling out the updated operator. Existing
`literal`, `lake`, and `dynamic` resources remain compatible. Helm does not upgrade installed CRDs
from `crds/` automatically; include the CRD update in the operator upgrade procedure. The controller
then adds environment labels and local delivery owners during normal reconciliation. No source
ciphertext or existing credentials need to be regenerated.

## Status and teardown

`LakeEnvironment.status` is written only when phase, URL, ready replicas, deployment ID or the `Ready`
condition changes. `Ready` comes from Deployment readiness; a Deployment replica failure reports
`Degraded`. An unresolved `secretRefs` entry reports `Degraded` and does not start the workload.

Workload and profile objects are watched for drift, Lake changes fan out to their environments, and a
steady Ready environment periodically rechecks its Namespace labels. These paths repair drift without
writing status when the observed state is unchanged.

On each Zetta poll, the operator compares the CR's phase, namespace, URL and message with the latest
status row. Only a real transition appends another row. This comparison is durable across operator restarts and
keeps a steady environment at zero Zetta writes.

A desired row with `withdrawn=true`, or a preview row with `change_closed=true`, first appends one
`TornDown` transition and then deletes the `LakeEnvironment`. Its finalizer deletes the named DNS record,
workload Namespace and everything inside it. It refuses to delete a Namespace whose ownership
annotation does not match.

## Verify

```sh
kubectl get lakeenvironment -A
kubectl get lakeenvironment -n <lake-namespace> <name> -o yaml
kubectl get namespace -l app.kubernetes.io/managed-by=orama-operator
kubectl get deployment,service,ingress,networkpolicy,resourcequota,limitrange \
  -n lake-<lake>-env-<environment-hash>
kubectl get secret -n lake-<lake>-env-<environment-hash>
kubectl get cloudflarednsrecord -n <lake-namespace>
```

To read the rows as lake does (lake's own statement; the host is the Lake's public host):

```sh
curl -sG -H "Authorization: Bearer $TOKEN" http://lake-<lake>-zetta-zetta.<lake-namespace>.svc.cluster.local:4800/v1/sql \
  --data-urlencode 'database=orama_control' \
  --data-urlencode "statement=SELECT operator, lake, repository, environment, kind, change, image, deployment_id, requested_by, requested_at, phase, namespace, url, message, updated_at, ordering_token FROM deployments WHERE lake = '<public-host>'"
```

The same with `environment_env` (`lake, repository, environment, key, value, deleted, set_by, updated_at,
ordering_token`) and `domains` (`lake, repository, environment, host, tls, status, message, deleted,
requested_by, updated_at, ordering_token`). lake's `/healthz` reports `platform: ready` while all three
succeed.

The real repository-to-preview proof needs liquid's deploy writer, Lake's change page, and the k0s
runtime. Cross-image chunk sharing is implemented and measured by k0s #14, not by this operator.

### Native admission regression

With Kubernetes 1.30.3 `KUBEBUILDER_ASSETS` set, run:

```sh
go test -count=1 -v ./internal/controller -run '^TestLakeEnvironmentSealedDeliveryAdmission$'
```

The test installs both exact k0s policies and bindings plus the tenant RBAC from commit
`cc922054ad10fd1991ab1077bb8a2c3bf3eeeae2`. It renders the chart's operator RBAC and authenticates
with a real operator service-account token. A tenant certificate uses the profile's group. The
operator creates the namespace and delivers synthetic source values; the tenant submits Pods from
the real Deployment template, just as a workload controller submits a child Pod. The chart does not
grant direct Pod creation to the operator.

The test proves missing metadata is refused, real delivery and rotation are admitted, local owner
UIDs resolve in the workload namespace, reference changes/removal update annotations and delete
copies/owners, steady reconciliation does not rewrite delivery state, and tenant plain/forged Secret
writes are refused by RBAC. It asserts the operator-created namespace label, demonstrates the selector
gap in the disposable server, and verifies label repair restores enforcement. Envtest has no workload,
quota or service-account controllers, so the fixture initializes quota status and the default service
account; it does not schedule workloads or run source decryption. The existing sealed-envelope suite
covers decryption. No live cluster is used.
