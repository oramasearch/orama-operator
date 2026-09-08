# Lake deployment environments

How a deployment written to the operator's Zetta control plane becomes an isolated Kubernetes
environment, how status returns to the caller, and how teardown is made safe. Field definitions are in
the [`LakeEnvironment` CRD reference](../crd-reference.md#lakeenvironment-separate-crd-namespaced--one-desired-deployment).

## Control-plane model

Liquid-bridge decision 22 makes Zetta, not Lake's HTTP API, the deployment intake path. An
`OramaZettaCluster` with `spec.provisionOperatorSchema` enabled creates the append-only
`orama_control.deployments` table. A client writes one event with `record_type=desired`; the operator
reads it over Zetta's authenticated `/v1/sql` capability and creates or updates a `LakeEnvironment`.
The pull/status implementation is `internal/controller/zetta_deployments.go`; it runs from the existing
`OramaZettaCluster` reconcile loop after operator-schema provisioning.

The operator processes a row only when both of these are true:

- a cluster-scoped `Lake` named by the row's `lake` field exists locally; and
- that Lake's `spec.namespace` is the same namespace as the `OramaZettaCluster` being polled.

This locality rule prevents a Zetta peer in another namespace from creating a second copy. The
Lake/repository/environment identity deterministically names the `LakeEnvironment`, so polling the same
desired event again does not create or update anything. A later deployment for that identity updates the
same resource; older resources from the deployment-ID naming scheme are deleted through their verified
finalizers. A legacy-named CR retains deployment-ID Namespace derivation during teardown, so its
finalizer can delete only the Namespace it originally owned. The materializer refuses to overwrite or
delete a stable-name collision that is not controlled by the referenced Lake.

Zetta is append-only on the deployed engine. The table is therefore an event stream:

| Column | Desired event | Status event |
|---|---|---|
| `deployment_id` | Stable identity; required | Same identity |
| `record_type` | `desired` | `status` |
| `lake`, `repository`, `environment`, `kind`, `change_id` | Environment identity | Same Lake; other identity fields empty |
| `image`, `replicas`, `port`, `env_json`, `secret_refs_json`, `host` | Desired workload | Empty |
| `withdrawn`, `change_closed` | Lifecycle signals | Empty |
| `phase`, `url`, `message` | Empty | Observed transition |
| `updated_at` | RFC 3339 ordering value | RFC 3339 ordering value |

The operator reduces desired and status streams independently, latest-wins by `updated_at`. This means
an observed status event never erases the last desired configuration. The Zetta admin bearer is read
from `<zetta-name>-zetta-admin`; unauthenticated fallback is not permitted.

Each Zetta controller polls once per local Lake with a `WHERE lake = ...` query, at the shortest
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

On each Zetta poll, the operator compares the CR's phase, URL and message with the latest status event.
Only a real transition appends another event. This comparison is durable across operator restarts and
keeps a steady environment at zero Zetta writes.

A desired event with `withdrawn=true`, or a preview event with `change_closed=true`, first appends one
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

To inspect the event stream directly:

```sh
curl -sG -H "Authorization: Bearer $TOKEN" https://<zetta-host>/v1/sql \
  --data-urlencode 'database=orama_control' \
  --data-urlencode 'statement=SELECT * FROM deployments'
```

The real repository-to-preview proof needs liquid's deploy writer, Lake's change page, and the k0s
runtime. Cross-image chunk sharing is implemented and measured by k0s #14, not by this operator.
