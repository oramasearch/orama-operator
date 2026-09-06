# Lake deployment environments

How a deployment written to the operator's Zetta control plane becomes an isolated Kubernetes
environment, how status returns to the caller, and how teardown is made safe. Field definitions are in
the [`LakeEnvironment` CRD reference](../crd-reference.md#lakeenvironment-separate-crd-namespaced--one-desired-deployment).

## Control-plane model

Liquid-bridge decision 22 makes Zetta, not Lake's HTTP API, the deployment intake path. An
`OramaZettaCluster` with `spec.provisionOperatorSchema` enabled creates the append-only
`orama_control.deployments` table. A client writes one event with `record_type=desired`; the operator
reads it over Zetta's authenticated `/v1/sql` capability and creates or updates a `LakeEnvironment`.

The operator processes a row only when both of these are true:

- a cluster-scoped `Lake` named by the row's `lake` field exists locally; and
- that Lake's `spec.namespace` is the same namespace as the `OramaZettaCluster` being polled.

This locality rule prevents a Zetta peer in another namespace from creating a second copy. The
deployment ID deterministically names the `LakeEnvironment`, so polling the same desired event again
does not create or update anything.

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
creates a dedicated Namespace named `lake-<lake>-env-<deployment-hash>`. A namespaced CR cannot own a
cluster-scoped Namespace or objects in another namespace through Kubernetes owner references, so every
object carries `orama.cloud/lake-environment=<cr-namespace>/<cr-name>` and teardown is guarded by a
finalizer that verifies that exact annotation before deleting the Namespace.

The Namespace profile matches k0s decision 31:

- Pod Security Standards `restricted` in enforce, audit and warn modes, all at `latest`;
- default-deny ingress and egress `NetworkPolicy`;
- one narrow policy admitting nginx ingress traffic to the application port and DNS traffic to
  `kube-system` CoreDNS only;
- `ResourceQuota`: 50 pods, 20 services and PVCs, 4 requested / 8 limited CPU, 8 GiB requested /
  16 GiB limited memory;
- `LimitRange`: 100m / 128Mi default request and 1 CPU / 1Gi default limit.

The workload is a Deployment plus ClusterIP Service. Every pod uses `runtimeClassName: untrusted`, the
k0s Kata/Firecracker contract, and the restricted container security context. The Deployment retains
ten ReplicaSet revisions so rollback is a normal new desired event naming an older change and immutable
image digest. Readiness, liveness and startup probes use the declared TCP port.

## Hosts and DNS

Ingress is enabled only when the referenced Lake has `spec.ingressEnabled: true`.

- A named environment uses its exact `spec.host`. Once its Ingress has an address, the operator creates
  one `CloudflareDNSRecord` in the Lake namespace using the Lake's `dnsSecretRef`.
- A preview derives `<first-12-DNS-safe-change>.<previewWildcard>`. The Lake owns one
  `*.<previewWildcard>` Cloudflare record against the shared nginx load balancer. Preview environments
  create no individual DNS records.

The status URL is present while the host is configured, including while replicas are still progressing.

## Status and teardown

`LakeEnvironment.status` is written only when phase, URL, ready replicas, deployment ID or the `Ready`
condition changes. `Ready` comes from Deployment readiness; a Deployment replica failure reports
`Degraded`. A non-empty `secretRefs` list reports `Degraded` and does not start the workload until
`LakeSealedSecret` delivery lands in operator #129.

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
  -n lake-<lake>-env-<deployment-hash>
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
