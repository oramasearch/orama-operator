# Lake methodology runs

**Status: implemented 2026-10-11 ([orama-operator#291](https://github.com/oramasearch/orama-operator/issues/291),
[lake#655](https://github.com/oramasearch/lake/issues/655)); fake-client and pgx contract coverage; not
deployed. A live run waits for the lake side of lake#655 and a published
`oramasearch/methodology-runtime` image.** The per-workspace cap on concurrent runs and the
placement of a GPU run's work volume ([orama-operator#293](https://github.com/oramasearch/orama-operator/issues/293),
lake migration `0082_methodology_run_queue`) are implemented with fake-client and pgx contract
coverage; not deployed. Serving a trained model from Ship → Models
([lake#690](https://github.com/oramasearch/lake/issues/690), lake migration
`0083_methodology_deployments`, [below](#serving-a-model)) is implemented with fake-client and pgx
contract coverage; not deployed.

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
| `methodology_run_status` | the operator (phase `Queued` from lake migration `0082_methodology_run_queue`) | `phase`, `message`, `exit_code`, `credential_sha256`, `observed_generation`, `started_at`, `finished_at` |
| `workload_resources` | the operator | kind `methodology_run`, the sandbox pod's resources (see [workload-resources.md](workload-resources.md)) |

The rows ride the control-rows poll with builds, which every Lake runs, on the same connection,
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
- **Credential pods never start in the work volume** (orama-operator#714). Stage, collect and the
  model stage start in their own `/tmp` emptyDir (`workingDir: /tmp`) with `PYTHONSAFEPATH=1`. The
  runtime runs `python3 -m`, which puts the working directory first on the module path, and the
  image's working directory is `/work`, which the sandbox writes: a module planted there would
  otherwise run in the collect pod with the credential and the transfer network. The runtime's own
  half is [methodology-runtime#4](https://github.com/oramasearch/methodology-runtime/issues/4);
  collect following symbolic links in `/work/out` is
  [methodology-runtime#5](https://github.com/oramasearch/methodology-runtime/issues/5).
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
  Pod Security `restricted` and has exactly four policies: `methodology-default-deny` (every pod,
  ingress and egress), `methodology-transfer` (stage and collect pods: DNS and lake's pods on lake's
  container port), `methodology-sandbox-internet` (sandboxes and serve pods labelled
  `orama.cloud/methodology-run-network=granted`: DNS and `0.0.0.0/0` except private, shared, loopback,
  link-local and reserved ranges, so never the cluster, its nodes or the metadata service), and
  `methodology-serve-ingress` (serve pods, [below](#serving-a-model): ingress from lake's pods only,
  on port 8080). Nothing else admits ingress; a run's pods admit none.

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

## Serving a model

A model a workspace methodology trained (kind `python:W-…`) is served by its own code (lake#690).
When a person publishes it in Ship → Models, lake writes a `methodology_deployments` row (lake
migration `0083_methodology_deployments`): the model, the training run's image build, the runtime
image, CPU, memory, GPUs, GPU class and the network grant, slug `md-<12 hex>`. The operator serves
it in the same namespace, with the same split between the code and the credential:

| Object | What it is |
|---|---|
| Secret `<slug>-credential` | the deployment's credential `lmd.<deployment id>.<secret>`; its SHA-256 goes to `methodology_deployment_status` |
| Secret `<slug>-pull` | the Lake's pull entries plus lake's registry with Basic user `methodology-deployment` and the credential, for kubelet only |
| PVC `<slug>-model` | the model volume (`spec.methodologies.workVolumeSize`, `storageClassName`), ReadWriteOnce |
| Job `<slug>-stage` | the pristine runtime image, role `stage` (the transfer policy: DNS and lake only): `methodology stage-model --lake $(LAKE_URL) --deployment $(DEPLOYMENT_ID) --credential-file /var/run/orama/credential --out /work/model`, which pages the model's artifact from lake's `methodology_deployment_input` and checks its size and SHA-256. For a GPU model it is pinned to the zones where its GPU class has nodes, as a run's stage pod is |
| Deployment `<slug>` | one replica (`Recreate`), role `serve`: the methodology's image, `methodology serve --model /work/model --source /methodology --port 8080`, the model volume **read-only**, no Secret mounted, no `LAKE_*`/`ZETTA_*` environment, no service account token, no service links, the run pods' security context, requests equal to limits from the row, the GPU class as its nodeSelector, `restartPolicy: Always`, readiness and liveness on `GET /healthz` |
| Service `<slug>` | ClusterIP, port 80 to the serve pod's 8080 |

The serve pod accepts connections from lake's pods only (`methodology-serve-ingress`) and opens
none, unless the person granted the methodology network (then `methodology-sandbox-internet`
applies, never the cluster). Lake forwards each `predict` of the model to the Service and answers
the caller; nothing else can reach it.

| Phase | When |
|---|---|
| `Building` | its image build is queued or running |
| `Queued` | a GPU model with no node of its class in the cluster yet (*… the model is served when one is*) |
| `Staging` | the stage Job is bringing the model in |
| `Starting` | the serve Deployment exists and is not ready; `endpoint` is set |
| `Ready` | a serve pod is ready; `ready_at` is set; the status stops changing |
| `Failed` | the build failed or was cancelled, the stage failed (*could not bring the model in*), or a serve pod exited three times (*the model's server exited with N: \<what it said\>*, from the container's termination message, which `methodology serve` writes as its findings); its objects are removed |
| `Stopped` | lake set `desired = absent` (the model was unpublished or deleted); its objects are removed and `stopped_at` is set |

`endpoint` is `http://<slug>.<namespace>.svc.cluster.local`; lake's schema refuses any other.
The credential stays live while the deployment is present, because kubelet pulls the image with it
whenever a serve pod restarts; the stage Job is not recreated once the Deployment exists (its Job
may be gone by its TTL, and the model is already on the volume). At `Failed` or `Stopped` the
digest and endpoint are cleared, so the credential stops resolving in lake.

**Metering.** The serve pods are workload kind `methodology_deployment` in `workload_resources`,
counted by the workload pass while they run (`pods` above 0) and while the deployment is present;
the stage Job's pod is the operator's transfer and is not counted. Lake charges the time they run
times their CPU, memory, GPUs and GPU memory, as for any long-running workload.

## Configuration

| Field | Meaning |
|---|---|
| `spec.methodologies.runtimeImage` | the private `oramasearch/methodology-runtime` image; injected into lake as `LAKE_METHODOLOGY_RUNTIME_IMAGE`. Empty uses the operator's default (`internal/controller/lake_defaults.go`) |
| `spec.methodologies.workVolumeSize` | each run's work volume and each served model's model volume, default `10Gi` |
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
go test ./internal/controller/ -run 'Methodology|MethodologyCredentialPods|TheSandbox|AGranted|ARunSucceeds|AFailing|ACode|AStage|Stopping|ACancelled|RunsFail|TheWorkloadPass|AWorkspacesRuns|TheLakeSetsTheCap|AQueuedRun|AGPURun|ACPURun'
go test ./internal/controller/ -run 'AServedModel|TheServePod|OnlyLakeReaches|AServerThatKeeps|Unpublishing|AGPUModel|DeploymentsAreSkipped'
LAKE_SERVICES_TEST_POSTGRES=1 LAKE_SERVICES_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres \
  go test ./internal/controller/ -run TestServicesPgxStoreAgainstPostgres   # checkMethodologyRunsPgx and checkMethodologyDeploymentsPgx
```
