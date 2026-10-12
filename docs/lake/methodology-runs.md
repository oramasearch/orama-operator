# Lake methodology runs

**Status: implemented 2026-10-11 ([orama-operator#291](https://github.com/oramasearch/orama-operator/issues/291),
[lake#655](https://github.com/oramasearch/lake/issues/655)); fake-client and pgx contract coverage; not
deployed. A live run waits for the lake side of lake#655 and a published
`oramasearch/methodology-runtime` image.** The per-workspace cap on concurrent runs and the
placement of a GPU run's work volume ([orama-operator#293](https://github.com/oramasearch/orama-operator/issues/293),
lake migration `0079_methodology_run_queue`) are implemented with fake-client and pgx contract
coverage; not deployed.

A workspace can write its own methodology as Python code in one of its repositories (lake#655). That
code is not trusted, so the operator runs it in a sandbox: no credential, no Kubernetes token, no
network unless the person granted it, locked-down pods with fixed resources and a time limit, and
the data in and the result out only through Orama's own runtime code. Lake's contract is
`docs/api/methodologies.md` in `oramasearch/lake`; the image and its CLI are
`oramasearch/methodology-runtime`.

## The rows (lake migration `0078_methodology_runs`)

| Table | Written by | What the operator uses |
|---|---|---|
| `methodology_runs` | lake (`NOTIFY lake_services`) | `id`, `mode` (`validate` or `train`), `build_id` (the methodology's image build), `image` (empty until lake knows it), `runtime_image`, `cpu_millis`, `memory_bytes`, `gpus`, `gpu_class`, `timeout_seconds`, `network`, `slug` (`mr-<12 hex>`), `desired` (`present` or `absent`), `generation` |
| `methodology_run_status` | the operator (phase `Queued` from lake migration `0079_methodology_run_queue`) | `phase`, `message`, `exit_code`, `credential_sha256`, `observed_generation`, `started_at`, `finished_at` |
| `workload_resources` | the operator | kind `methodology_run`, the sandbox pod's resources (see [workload-resources.md](workload-resources.md)) |

The rows ride the control-rows poll with builds (`spec.builds.enabled`), on the same connection,
woken by `lake_services`. A schema older than `0078` skips them.

## Three pods, one volume

Each run gets, in the Lake's namespace `lake-<lake>-methodology-runs`:

| Pod (Job) | Image | Command | What it can reach |
|---|---|---|---|
| **stage** (`<slug>-stage`) | `runtime_image` | `/opt/orama/bin/methodology stage --lake $(LAKE_URL) --run $(RUN_ID) --credential-file /var/run/orama/credential --out /work/in` | DNS and lake's pods only |
| **run** (`<slug>-run`) | the built image | `/opt/orama/bin/methodology run --in /work/in --out /work/out --source /methodology` | nothing; with `network` true, DNS and the public internet only |
| **collect** (`<slug>-collect`) | `runtime_image` | `/opt/orama/bin/methodology collect --lake $(LAKE_URL) --run $(RUN_ID) --credential-file /var/run/orama/credential --in /work/out [--run-exit-code N]` (the sandbox's exit code once known) | DNS and lake's pods only |

- `LAKE_URL` is lake's in-cluster Service (`http://lake-<lake>.<namespace>.svc.cluster.local:80`),
  `RUN_ID` the row's id, `HOME=/tmp`. The credential is a file from Secret `<slug>-credential`, never
  an environment value.
- **Why three pods.** NetworkPolicy applies to a pod, not a container, so the code that is not
  trusted must not share a pod (a network namespace) with the credential-carrying transfers. The
  pods run one after another and share the run's work volume, a `ReadWriteOnce` PVC
  `<slug>-work` (`spec.methodologies.workVolumeSize`, default 10Gi; `storageClassName`, default the
  cluster's). An `emptyDir` cannot be shared between pods; a sequential RWO claim can, on any node
  of its zone.
- **Where the volume goes.** The stage pod is the volume's first consumer, so with a
  `WaitForFirstConsumer` storage class (AKS's `managed-csi` and `default` are) the volume is created
  in the zone the stage pod lands in, and the sandbox and collect pods follow it there. A run with
  GPUs pins its stage pod (required node affinity on `topology.kubernetes.io/zone`) to the zones
  where nodes of its GPU class are: schedulable nodes labelled `orama.cloud/gpu-class=<class>`, the
  label its sandbox selects, or, with no class, nodes offering `nvidia.com/gpu`. Nodes without a zone
  label add no zone; when none has one, the stage pod is not pinned. With no such node at all the
  run is `Queued` (*no node of GPU class \<class\> is in the cluster yet*) and nothing is created
  until one appears. A storage class that binds immediately places the volume before any pod, so
  this placement cannot help it. The decision is `methodologyRunPlacement`.
- **Why its own namespace.** A workspace namespace carries allow policies that select every pod
  (lake, DNS, HTTPS to the internet, the workspace's own services), and NetworkPolicies only ever add
  what they allow, so a sandbox there could never be denied. The methodology-runs namespace enforces
  Pod Security `restricted` and has exactly three policies: `methodology-default-deny` (every pod,
  ingress and egress), `methodology-transfer` (stage and collect pods: DNS and lake's pods on lake's
  container port), `methodology-sandbox-internet` (sandboxes labelled
  `orama.cloud/methodology-run-network=granted`: DNS and `0.0.0.0/0` except private, shared, loopback,
  link-local and reserved ranges, so never the cluster, its nodes or the metadata service). No
  policy admits ingress.

**Every pod** runs as uid/gid 10001 with `fsGroup` 10001 (which owns the work volume), a read-only
root filesystem with a 1Gi `/tmp`, all capabilities dropped, `RuntimeDefault` seccomp, no privilege
escalation, `automountServiceAccountToken: false`, `enableServiceLinks: false`, `backoffLimit: 0`
and `restartPolicy: Never`. **The sandbox** additionally mounts no Secret and takes no Secret or
`LAKE_*`/`ZETTA_*` environment; its CPU, memory and GPUs are requests equal to limits from the row,
with the GPU class as the `orama.cloud/gpu-class` nodeSelector; `activeDeadlineSeconds` is
`timeout_seconds`. The stage and collect pods are bounded at 100m/256Mi requests, 1 CPU/1Gi limits
and 30 minutes.

**Pulling the built image.** The image lives in lake's registry. The operator resolves a succeeded
build to `<public host>/<space>/<name>@<digest>`, as lake's `build_json` does (public host:
`LAKE_PUBLIC_HOST` in `spec.env`, else `spec.host`), unless lake set `image` itself (it must then be
pinned by digest). Each run gets a pull Secret `<slug>-pull`: the Lake's `spec.pullSecretName`
entries (for the runtime image) plus the public host with Basic user `methodology-run` and the run's
credential. Only kubelet reads it; nothing in the sandbox can. Lake accepts that credential for
reads of that run's image only.

## Phases

| Phase | When |
|---|---|
| `Building` | the build is queued or running; no credential exists yet |
| `Queued` | the image is built, but the run waits, with no credential and no object yet: its workspace already has `spec.methodologies.maxConcurrentRunsPerWorkspace` runs (default 2) `Staging`, `Running` or `Collecting` (*Queued: 2 of this workspace's methodology runs are running (the limit is 2); this run is next.*), or no node of its GPU class exists (above). Runs are admitted oldest first (lake's `created_at`), as places free, in the same pass a run ends |
| `Staging` | the stage pod is bringing the data in; the credential and its digest exist |
| `Running` | the sandbox Job exists; `started_at` is set once its container has started |
| `Collecting` | the code finished (any outcome); `finished_at` and `exit_code` are set and the sandbox's resources are written to `workload_resources` again (they were first written before its Job was created) |
| `Succeeded` | collect succeeded and the code exited 0 |
| `Failed` | the build failed or was cancelled (*the methodology's image build failed: \<build message\>*), there was no build, the stage failed (*could not bring the data in*), the code exited non-zero (*the methodology's code exited with N*) or ran past its time limit (*… ran past its time limit (N seconds)*), or collect failed (*could not report the result to Lake*) |
| `Stopped` | lake set `desired = absent` before the run finished |

A failed code is still collected, so its findings reach lake. `started_at` and `finished_at` are
the sandbox container's own times (whole seconds, UTC); a sandbox killed at its deadline without a
termination record ends at the Job's failure time; a stopped sandbox ends when it was stopped.
Lake prices the run's minutes as `finished_at - started_at` times the `workload_resources` row.
That row is written from the sandbox pod's spec **before its Job is created** (pods 0), so a run is
metered whether or not any pod of it is ever observed: a short run, a Job removed by its TTL or an
operator restart cannot leave it unpriced. A row that cannot be written holds the sandbox back
until the next pass; no run's code starts unmetered. It is written again when the code finishes,
and the workload pass sets the live pod count while the pod runs and keeps the numbers after.

At a terminal phase the credential digest is cleared (the credential stops resolving in lake) and
the Jobs, their pods, the two Secrets and the work volume are deleted; the namespace and its
policies stay. Status is written only when it changes.

## Configuration

| Field | Meaning |
|---|---|
| `spec.methodologies.runtimeImage` | the private `oramasearch/methodology-runtime` image; injected into lake as `LAKE_METHODOLOGY_RUNTIME_IMAGE`. Empty: every run fails with that reason |
| `spec.methodologies.workVolumeSize` | each run's work volume, default `10Gi` |
| `spec.methodologies.storageClassName` | its storage class, default the cluster's; it should bind on first use (`WaitForFirstConsumer`) for GPU placement |
| `spec.methodologies.maxConcurrentRunsPerWorkspace` | how many of one workspace's runs may be staging, running or collecting at once, default `2`, at least 1; the rest wait `Queued` |

Builds also accept the `METHODOLOGY_RUNTIME_IMAGE` and `METHODOLOGY_PACKAGES` build arguments (`BUILD_ARG_ALLOW`), which lake's
generated methodology Dockerfile builds `FROM`.

## Limits and follow-ups

- The cap is per workspace; there is no cap across a Lake's workspaces beyond what the cluster can
  schedule.
- A GPU class whose nodes an autoscaler creates only on demand (a pool scaled to zero) has no node
  to place the volume next to, so its runs stay `Queued` until a node of the class exists.
- The NetworkPolicies need a CNI that enforces them (Cilium on the dev AKS cluster does).

## Verify

```sh
go test ./internal/controller/ -run 'Methodology|TheSandbox|AGranted|ARunSucceeds|AFailing|ACode|AStage|Stopping|ACancelled|RunsFail|TheWorkloadPass|AWorkspacesRuns|TheLakeSetsTheCap|AQueuedRun|AGPURun|ACPURun'
LAKE_SERVICES_TEST_POSTGRES=1 LAKE_SERVICES_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres \
  go test ./internal/controller/ -run TestServicesPgxStoreAgainstPostgres   # includes checkMethodologyRunsPgx
```
