# Workload resources

lake meters compute by the resources each workload is **scheduled with** and prices it as CPU, RAM,
GPU and VRAM minutes ([lake#643](https://github.com/oramasearch/lake/issues/643)), and charges every
finished build, agent job and agent cron run once
([lake#658](https://github.com/oramasearch/lake/issues/658)). The operator is the only writer of
the numbers: one row per workload of a Lake in that Lake's control Postgres, table
`workload_resources` (lake migrations `0075_workload_resources` and `0077_metered_jobs`). lake's owner loop reads the
rows and records the resource-seconds in each workspace's Zetta. The rows hold numbers only, never
workspace content.

Code: `internal/controller/lake_workload_resources.go` (the pass and the arithmetic) and
`lake_workload_resources_pgx.go` (the SQL).

## The table

| Column | Meaning |
|---|---|
| `object_kind`, `object_id` | The lake row the workload belongs to (primary key). |
| `pods` | How many of its pods are scheduled now. |
| `cpu_millis` | Total effective CPU request of those pods, in millicores. |
| `memory_bytes` | Total effective memory request, in bytes. |
| `gpus` | Total `nvidia.com/gpu` plus `amd.com/gpu`. |
| `gpu_class` | Short class of the GPUs (`l4`, `a10g`, `a100`, `h100`, `t4`, ...), `''` when unknown or no GPUs. |
| `vram_bytes` | Total GPU memory: each pod's GPUs times the memory of one GPU on its node. |
| `observed_at` | When the operator last wrote the row, which is when its numbers last changed. |

## What each kind maps to

| `object_kind` | `object_id` | Pods |
|---|---|---|
| `workspace` | `workspaces.id` | The workspace's own Zetta engine: pods `app.kubernetes.io/name=orama-zetta`, `app.kubernetes.io/instance=<lake>-ws-<12 hex of the id>`, `orama.cloud/workspace=<id>`. Never also reported as a `service`: lake maps the workspace engine's `services` row to this row. |
| `service` | `services.id` | Every other managed service in the workspace namespace. Zetta, SQLite, DuckDB and Polars pods carry `orama.cloud/service-id`. The others are matched by the labels their operator puts on the pods of the resource `<lake>-<slug>`: CloudNativePG `cnpg.io/cluster`; Strimzi `strimzi.io/cluster` and `strimzi.io/kind=Kafka`; Altinity `clickhouse.altinity.com/chi` and the Keeper's `clickhouse-keeper.altinity.com/chk` (both with `.../app=chop`); TiDB `app.kubernetes.io/instance` and `app.kubernetes.io/managed-by=tidb-operator`; cass-operator `cassandra.datastax.com/cluster=<slug>`; the Valkey operator `valkey.io/cluster` and `app.kubernetes.io/managed-by=valkey-operator`. These are the selectors the service deletion already uses for each kind's claims. |
| `app` | `apps.id` | The `LakeApp` Deployment's pods, by `orama.cloud/lake-app-id` (the LakeApp's identity hash), mapped to the row through the LakeApp's `spec.sync.id`. |
| `build` | `builds.id` | The build Job's pods, `orama.cloud/build-id` and `orama.cloud/build-lake=<lake>`, in the build space's namespace. |
| `agent_launch` | `agent_launches.id` | The agent-runtime Job (mode `job`) or Deployment (mode `service`) pods, `orama.cloud/agent-launch-id`. |
| `agent_cron_run` | `agent_cron_runs.id` (the Job's uid) | One Job an agent cron schedule's CronJob started (lake#658): pods with `orama.cloud/agent-schedule-id` and the Job uid Kubernetes puts on them (`batch.kubernetes.io/controller-uid`, or the older `controller-uid`), in the schedule's workspace namespace. |
| `store` | `stores.id` | Never written: a store is a volume or a bucket and runs no pods. |

A pod counts only in its own object's namespace (a build in its build space), so a label copied onto
a pod in another workspace counts for nothing. Pods that match no row are ignored: the Lake's own
pods, the Agent Library's agents, and anything else in a workspace namespace.

## The numbers

A pod is **scheduled** when it has a node and is neither `Succeeded` nor `Failed`. For each scheduled
pod, per resource, the operator takes Kubernetes' effective pod request:

- each container counts its **request**, or its **limit** when it has no request;
- the app containers are summed together with every restartable init container (sidecar);
- an ordinary init container counts itself plus the sidecars started before it;
- the pod's amount is the larger of the two; the pod `overhead` (RuntimeClass) is added for CPU and
  memory.

LimitRange defaults are already applied to live pods by admission, so live pods are read as they
are. A row's numbers are the sums over the object's scheduled pods.

**GPU class**, per pod, from its node: the GPU feature discovery label `nvidia.com/gpu.product` (or
AMD's `amd.com/gpu.product-name`) normalized to its model, so `NVIDIA-L4` is `l4`, `NVIDIA-A10G` is
`a10g`, `NVIDIA-A100-SXM4-80GB` is `a100`, `NVIDIA-H100-80GB-HBM3` is `h100`, `Tesla-T4` is `t4`;
else the node label `orama.cloud/gpu-class`; else the pod's nodeSelector `orama.cloud/gpu-class`;
else `''`. Anything that does not match `^([a-z0-9][a-z0-9-]{0,31})?$` becomes `''`. When an
object's GPU pods run on different classes, the row carries the class running the most GPUs.

**VRAM** per GPU: the node label `nvidia.com/gpu.memory` (MiB); else the built-in table (L4 and
A10G 24 GiB, T4 16 GiB, H100 80 GiB, A100 80 GiB when the product name says `80GB` and 40 GiB
otherwise); else 0.

## Lifecycle of a row

- **Long-running objects** (workspaces, services, apps, service-mode agent launches) have a row as
  long as lake wants them, with `pods` 0 and zeros when nothing is scheduled (a stopped workspace or
  app, a service still pending).
- **A build, a job-mode agent launch or an agent cron run** gets its row **as soon as its Job
  exists** (lake#658), from the Job's pod template with `pods` 0: each container's request, its
  limit when it has none, and the namespace's LimitRange default request (then default limit) when
  it has neither. So a run that starts and finishes between two passes is still priced. Once a pod
  of it is scheduled, the row is that pod's numbers; when its pods finish, the row keeps the last
  values it ran with and `pods` becomes 0, so lake can price duration times resources.
- **Backfill** (lake#658). A finished run with no row and no Job left (it ran before the operator
  reported resources, or its Job's one-hour TTL passed before a pass saw it) is listed by
  `listUnmeteredJobsSQL`: builds and job launches that `Succeeded` or `Failed` (a `Cancelled` build
  may never have run) and cron runs that `Succeeded` or `Failed`. Each gets a row from the pod spec
  the operator schedules such runs with today (`scheduledJobResources`: `buildPod`'s
  `buildResources`, i.e. `spec.builds.resources` or 500m and 1 GiB; `agentJobPod`'s 100m and
  256 MiB), with `pods` 0. Their requests are explicit, so no LimitRange default applies. Lake then
  charges each once, for how long it ran.
- **Deletion**: a row is deleted once lake no longer has its object, or once the object is desired
  absent or deleted (`services.desired='absent'`, `workspaces`/`apps.desired='deleted'`,
  service-mode `agent_launches.desired='absent'`) and none of its pods is scheduled. A build's, a
  job launch's and a cron run's row stays until lake deletes the build, launch or run row, so a
  cancelled run is still priced.

## When it runs

On the Lake's control-rows poll (`reconcileLakeServices`, so with `spec.services.enabled` or
`spec.builds.enabled` and the schema applied), over the same control-Postgres connection, at most
once every 30 seconds per Lake. Each pass reads the existing rows and the objects lake has, lists
the pods of every workspace namespace and the Lake's build pods, and writes a row **only when its
numbers changed**: a steady cluster writes nothing, and `observed_at` is the time of the last change.
Any failed read stops the pass before it writes, so a partial view never zeroes or deletes a row. A
schema older than `0075_workload_resources` skips the pass and logs it.

Reads use the uncached API reader. The RBAC it needs was already granted: `pods` and `nodes`
get/list, `limitranges` get/list and `jobs` get/list (kubebuilder markers in the file; the chart's
ClusterRole already lists them).

## Proof

- `go test ./internal/controller/ -run 'WorkloadResources|PodSpecResources|GPU|ServicePodSelectors|WorkloadTotals'`:
  the arithmetic (requests, limit fallback, LimitRange defaults, init containers, sidecars, overhead,
  GPUs), GPU class normalization and the VRAM table, mapping pods of every kind to their objects on a
  fake API server, keeping a finished build's and job's values, the Job-template fallback, deletion,
  and a steady pass writing nothing; and (lake#658) a Job's row before any pod of it is scheduled
  (`TestWorkloadResourcesFromAJobAsSoonAsItExists`), a cron run's pods and kept values, a foreign
  namespace's ignored (`TestWorkloadResourcesReportsCronRuns`), and finished runs backfilled from
  the scheduled pod spec once (`TestWorkloadResourcesBackfillsFinishedRunsFromTheScheduledPodSpec`).
- `LAKE_SERVICES_TEST_POSTGRES=1 go test ./internal/controller/ -run TestServicesPgxStoreAgainstPostgres`:
  the SQL against the vendored schema on a real Postgres (listing, history by id, upsert, delete,
  the `gpu_class` check, the missing-table error; lake#658: a cron run by id, the unmetered finished
  runs and an `agent_cron_run` row that `0077_metered_jobs` accepts).
