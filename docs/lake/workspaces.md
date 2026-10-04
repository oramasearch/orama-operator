# Lake workspace namespaces

Each Lake workspace is a Zetta data plane in its own Kubernetes namespace. The namespace is a
security boundary for customer workloads and credentials; it is not a naming convenience. This is
the operator side of [lake#265](https://github.com/oramasearch/lake/issues/265) and
[operator#190](https://github.com/oramasearch/orama-operator/issues/190). The workspace reconciler
is the only writer of that workspace's `OramaZettaCluster`; the managed-services poll explicitly
excludes the service named by `workspaces.zetta_service_id`, except to deliver its connection into
`lake-<lake>-services`.

## Control-Postgres contract

With `Lake.spec.workspaces.enabled`, a dedicated controller reads lake's `workspaces` table through
the CloudNativePG application Secret `lake-<lake>-controldb-app` (`uri`). Lake sends `NOTIFY
lake_workspaces` after every change, and the operator performs a complete list every 60 seconds so a
missed notification, reconnect or manager restart cannot strand desired state.

The fixed row is:

| Column | Owner and meaning |
|---|---|
| `id` | lake; canonical workspace UUID and immutable identity |
| `customer_id`, `name`, `environment` | lake; `environment` is a DNS label |
| `desired` | lake; `running`, `stopped` or `deleted` |
| `generation` | lake; increments with desired state |
| `phase`, `observed_generation`, `message`, `namespace` | operator observations |
| `zetta_service_id` | lake's workspace-to-data-plane identity |
| `created_at`, `updated_at` | lake timestamps |

Status updates include `id`, `customer_id`, `generation` and `desired` in the `WHERE` clause. A
stale pass therefore cannot acknowledge a row that changed after it was listed. An unchanged
status writes nothing.

The same pass joins `zetta_service_id` to its `services` generation and current `service_status`.
It comparison-gates a second observation for that service ID: `Provisioning` with the in-cluster
endpoint while Zetta starts, `Ready` / `service ready` when the cluster is ready, and a deletion
phase with no endpoint once the workspace is stopped or deleted. This preserves lake's service
connection contract without giving the services reconciler a second write path.

The namespace is `<environment>-ws-<first 12 UUID hex characters>`, for example
`prod-ws-123e4567e89b`, with these labels:

```text
orama.cloud/workspace=<full workspace UUID>
orama.cloud/customer=<customer UUID>
orama.cloud/environment=<environment>
orama.cloud/lake=<Lake name>
```

An existing namespace whose identity labels do not match is refused, never adopted. If the
environment changes, the previously reported owned namespace is deleted before the replacement is
created. `desired=stopped` removes the workspace `OramaZettaCluster` but leaves its namespace and
PVCs for restart. `desired=deleted` deletes the owned namespace; Kubernetes namespace deletion
cascades through the Zetta cluster, volumes and Secrets before the row becomes `Deleted`.

Every data-plane row carries the required immutable `workspace_id`. The services, stores, agent
launches, agent schedules and apps readers join that ID to `workspaces.namespace`; an empty or
invalid reconciled namespace fails the object instead of falling back to Lake's namespace. Managed
services, stores, agent Jobs/Deployments/CronJobs and synchronized app workloads are created in the
joined workspace namespace and labelled with the full workspace UUID. App resource and Secret
names include the first 12 app-ID hex characters so multiple apps can share the namespace without
colliding. Standalone `LakeApp` resources retain their separately owned app namespace.

## Workspace Zetta ownership

The workspace reconciler creates `<lake>-ws-<first 12 workspace UUID hex>` in the reconciled
workspace namespace and owns its complete desired spec. Its image is
`spec.services.zettaImage`, then `spec.workspaces.image`, then the `OramaZettaCluster` default.
Resources and Zetta environment variables continue to come from the shared workspace/service
settings. Replicas remain one.

Storage class is copied from `spec.workspaces.storage`; the size comes from lake (next section).
Image, resources, environment, pull credentials and the forced private reachability settings
remain reconciled normally.

### Workspace volume size (operator#259)

Lake's `workspaces.storage_gi` (migration `0069`, default 5, never decreasing: lake's trigger
refuses a smaller value, and `set_workspace_storage` advances the row's generation) is the desired
size of each of the workspace Zetta's volumes. Decision 54 puts the workspace's liquid store,
tenant images and search index on that volume, so it has to grow with the workspace.

- **On create** the child `OramaZettaCluster` gets `storage.size` = `storage_gi` GiB.
  `spec.workspaces.storage.size` is used only for a row without one.
- **On a larger value** the reconciler patches `spec.resources.requests.storage` of each claim
  `data-<cluster>-zetta-<n>` in the workspace namespace (the claim, not the StatefulSet's
  immutable template), and raises the child's `storage.size` to match. It never rewrites the
  template: the `OramaZettaCluster` controller keeps an existing StatefulSet's claim templates as
  they are, so a replica added later starts at the template's size and is grown by the same patch.
  Azure disks grow online; the pod is not restarted.
- **Observation.** On every pass the reconciler writes, with the workspace's phase and in the same
  generation-guarded update, `storage_gi_observed` (the smallest claim capacity in GiB),
  `storage_phase` and `storage_message`: `Pending` before the claim exists, `Resizing` while a
  claim's `status.capacity` is below its request (with `file system resize pending` when the
  claim says so, and that a restart of the Zetta pod may be needed once it has been pending for
  over 15 minutes; the pass requeues fast), and `Ready` once every claim has reached it.
- **Quota.** The workspace ResourceQuota's `requests.storage` is the plan's value plus
  `storage_gi` × replicas: the plan's value is headroom for the namespace's other claims (services,
  stores), and growing the workspace volume past it is never refused by the quota.
- **Never shrink.** A `storage_gi` below a claim's request is ignored; the claim and the recorded
  size stay, and the message says *volumes never shrink*.
- **Failed.** A claim whose storage class (or the cluster default) lacks `allowVolumeExpansion`,
  or a patch the API server refuses (the plan's `requests.storage` quota, for example), is
  `storage_phase: Failed` with the reason. The workspace's own phase is unaffected: its Zetta
  keeps serving at the old size.
- Stopped and deleted workspaces keep the last storage observation, except that a stopped workspace
  whose `storage_gi` is above the observed size reports `Pending` (*the volume was not checked
  while the workspace is stopped*); it grows when it runs.
- `spec.workspaces.storage.size` is deprecated: with the 0069 schema every row has `storage_gi`.

The managed-services query also reads each joined workspace's `zetta_service_id`. A Zetta row
whose ID matches is excluded from service resource reconciliation, deletion, status writes and
the services quota. Every other row in that workspace, including Postgres and additional Zetta
services, remains managed by the services poll in the workspace namespace.

### Connection delivery (operator#264)

The services poll is the one writer of `lake-<lake>-services`, so it also delivers the workspace
Zetta's connection, read-only: once the cluster `<lake>-ws-<12 hex>` in the row's joined workspace
namespace is Lake-controlled, labelled with the workspace, `Ready` with its one node, and its
`<cluster>-zetta-admin` Secret holds a token, the poll writes
`{"kind":"zetta","endpoint":"http://<cluster>-zetta.<namespace>.svc.cluster.local:4800","token":…}`
under `<slug>.json` and under the service id. While such a cluster is still starting the poll keeps
its fast (2 s) cadence. A connection that cannot be read keeps the key already there.

The workspace reconciler reports the workspace `Ready` (and its Zetta `service_status` `Ready`) only
once the Secret holds that same connection under the service id. Until then the workspace stays
`Pending` with *delivering the connection*, the service `Provisioning` with its endpoint, and both
reconcilers poll every 2 s. Before this, the poll only preserved keys written by the pre-#219
adoption path, so every workspace created after 2026-09-29 was `Ready` with no connection and lake
answered `workspace_not_ready` for it.

## Namespace security profile

Every workspace namespace receives all of the following:

- Pod Security Admission `restricted` at `enforce`, `audit` and `warn`, with the corresponding
  version labels set to `latest`.
- The `default` ServiceAccount with `automountServiceAccountToken: false`. Zetta, agent, app and
  operator-owned service pods also set `automountServiceAccountToken: false` on their pod specs.
  The operator creates no Role or RoleBinding in the namespace and binds no ClusterRole to it.
- One `ResourceQuota` and `LimitRange` selected by the workspace's declarative plan.
- A default-deny NetworkPolicy for both ingress and egress, followed only by the explicit policies
  below.
- A local copy of the Lake image-pull Secret, when configured. Zetta's admin and cluster tokens are
  minted by the Zetta controller in this namespace. Agent provider and service credentials are
  copied here, and per-run and synchronized-app credentials are minted here. The trusted Lake pod
  retains its read-only services connection projection for control-plane dispatch; tenant workloads
  receive only local Secret copies. Namespace deletion is the credential deletion boundary.

Zetta pods use uid/gid `10001`, `runAsNonRoot`, `RuntimeDefault` seccomp, no privilege escalation,
and drop all Linux capabilities. Workspace Zetta resources force `hostNetwork`, public ingress and
peer LoadBalancers off.

No dedicated node pool or taint is selected by this feature. AKS placement is cluster-specific and
the operator does not invent a node class or reduce available capacity. The namespace boundary is
enforced with Cilium policy, Pod Security Admission and identity isolation. A cluster that requires
physical separation can apply its existing scheduling policy to the stable workspace labels; an
operator-owned AKS node-pool contract remains a separate platform decision.

## Allowed network flows

Everything not listed is refused, including every other workspace, Lake's Postgres, the Kubernetes
API Service and its backing endpoints, node and pod networks, RFC1918/private ranges, the metadata
endpoint `169.254.169.254`, and non-DNS traffic to `kube-system`.

| From | To | Ports | Why |
|---|---|---|---|
| workspace pod | pod in the same workspace namespace | all | Zetta peers, customer services and local applications must communicate inside their trust boundary |
| Lake pod in the Lake namespace | workspace pod | TCP 4800, 8080 | Lake calls Zetta and agent HTTP APIs; namespace and pod selectors prevent another workspace from using this rule |
| operator pod in its release namespace | workspace pod | TCP 4800, 8080 | operational reconciliation and verification use the same narrow application ports; the chart supplies the release namespace through `POD_NAMESPACE`, so a tenant cannot gain access by copying the operator pod label |
| nginx ingress pod in `ingress-nginx` | synchronized app pod | the app's declared port | serves an app host; the per-app policy selects only that app's identity labels and port |
| workspace pod | CoreDNS pods in `kube-system` | UDP/TCP 53 | service discovery |
| workspace pod | Lake pod in the Lake namespace | Lake container port | agents and applications call Lake's Service; the rule selects Lake pods rather than the namespace generally |
| opted-in workspace pod | public IP addresses | TCP 443 | model-provider HTTPS; private, reserved, node, API-Service and metadata ranges are excluded |

Internet egress is denied by default. `spec.workspaces.overrides[<workspace UUID>].internetEgress`
adds or removes the TCP 443 policy for that workspace alone. Before enabling it, the operator reads
the `default/kubernetes` Service and Endpoints plus every Node address and pod CIDR, then adds them
as exclusions. If those reads fail, it refuses to create the internet policy; the base default-deny
and narrow internal policies are installed before that calculation, and any older internet policy
is removed so a stale exclusion list cannot remain active.

## Quota plans

The built-in `default` plan has these hard limits:

| Resource | Limit |
|---|---:|
| requests CPU / memory | `4` / `8Gi` |
| limits CPU / memory | `8` / `16Gi` |
| requested storage | `100Gi` |
| pods / Services / PVCs / Secrets | `50` / `20` / `20` / `50` |

Its container defaults are a `100m`/`128Mi` request and a `1`/`1Gi` limit. Define named plans under
`spec.workspaces.plans`, choose the fallback with `defaultPlan`, and select a different plan for one
UUID with `overrides[uuid].plan`. A custom plan must supply `quota`, `defaultRequest` and `default`;
an absent or incomplete plan fails that workspace without creating an unbounded workload.

## Legacy workspace replacement

The old `prod-ws-*` clusters live beside Lake in the Lake namespace and cannot be moved with their
PVCs. This is a development environment, so takeover deliberately recreates them with empty data.
For each exact `<lake>-ws-<first 12 workspace UUID hex>` cluster name, the operator:

1. verifies that the legacy `OramaZettaCluster` is controlled by the same Lake;
2. deletes it and logs the Lake, workspace UUID and namespaced cluster name;
3. deletes only PVCs named `data-<workspace>-zetta-<ordinal>` and only the exact
   `<workspace>-zetta-admin` and `<workspace>-zetta-cluster` Secrets, logging every object;
4. creates the isolated namespace and a fresh workspace Zetta there.

It never selects or deletes `lake-<lake>-store`, `lake-<lake>-instance-kek`, or any resource by a
broad customer label. A foreign object at an expected name stops reconciliation.

### Issue #219 dev rollout cleanup

Migration `0039_workspaces` changed the two dev service slugs after their old Lake-namespace
clusters had already been created. Those old clusters are not the isolated workspace children and
their workspaces have since been deleted. During the issue #219 rollout, remove only these exact
development objects after confirming their Lake ownership:

```sh
for name in prod-ws-3a09e97096a0 prod-ws-7f9b8da6a275; do
  kubectl -n lake get oramazettacluster "$name" \
    -o jsonpath='{.metadata.ownerReferences[?(@.kind=="Lake")].name}{"\n"}'
  kubectl -n lake delete oramazettacluster "$name" --ignore-not-found
  kubectl -n lake delete pvc -l "app.kubernetes.io/instance=$name" --ignore-not-found
  kubectl -n lake delete secret \
    "$name-zetta-admin" \
    "$name-zetta-cluster" \
    --ignore-not-found
done
```

The first command must print `prod` for each object before deletion. The explicit names and
instance label keep this cleanup away from the current workspace clusters, Lake's own data plane,
store, and instance keys. The data on these two obsolete dev volumes is intentionally deleted.

## Verification Job

The same Job is used by cluster CI and the dev-cluster runbook:

```sh
make verify-workspace-isolation \
  WORKSPACE_NAMESPACE=prod-ws-123e4567e89b \
  OTHER_WORKSPACE_NAMESPACE=prod-ws-abcdef012345 \
  LAKE_NAMESPACE=lake \
  LAKE_NAME=prod
```

`hack/verify-workspace-isolation.sh` first refuses a namespace containing any RoleBinding or a
ClusterRoleBinding that names one of its ServiceAccounts, then
runs a restricted, tokenless BusyBox Job inside the workspace. The Job must resolve DNS and reach
Lake's Service. It must fail to connect to the Kubernetes API, list pods or Secrets, reach the
other workspace's Zetta, Lake's Postgres, `169.254.169.254`, or a non-DNS CoreDNS port. Its logs are
the live-cluster proof artifact.

The native Go suite covers the deterministic object model, data-plane placement, generation guards,
status anti-flap, internet exclusions, stopped-data preservation, namespace deletion and exact legacy cleanup.
An envtest alternates the workspace and services polls and proves the child generation stays fixed,
the services image wins, creation-time storage is preserved, and `service_status` is written once
per transition. The SQL adapter uses `internal/controller/testdata/lake-workspaces.sql`; lake owns
the production migration, so the fixture is intentionally not copied into the vendored lake schema.

## Verify volume growth on dev (operator#259)

After the operator and lake `0069` are deployed: call lake's `set_workspace_storage` for one
workspace (for example 5 to 10 GiB), then

```sh
kubectl -n <environment>-ws-<id> get pvc data-<cluster>-zetta-0 -o jsonpath='{.spec.resources.requests.storage} {.status.capacity.storage}{"\n"}'
kubectl -n <environment>-ws-<id> exec <cluster>-zetta-0 -- df -h /data
```

The claim's request and capacity reach the new size, `df` inside the running Zetta pod shows it
without a restart (Azure disk online expansion), and lake's `get_workspace` shows
`storage_phase: Ready` with `storage_gi_observed` at the new size. Record the observation in the
deployment record. The envtests `TestWorkspaceStorage*` cover the growth to 20 GiB, the ignored
decrease and a class without expansion.
