# Lake agents

**What.** The operator runs lake's agents (lake#228): the **Agent Library**'s specialized agents
the **launches** of user-created Python agents on `agent-runtime` (lake#231), and their **cron
schedules**, each a Kubernetes CronJob (lake#244). The contract with
lake is lake's [`docs/api/agent-launches.md`](https://github.com/oramasearch/lake/blob/main/docs/api/agent-launches.md);
the code is `internal/controller/lake_agents.go` and `lake_agent_schedules.go`.

**Why.** Agents are how lake builds Pipelines from intent, recommends Opportunities, and runs the
agents people build. lake records what it wants and never talks to Kubernetes; the operator runs it
and reports back, as it does for services and stores.

## spec.agents

| Field | Meaning |
|---|---|
| `runtimeImage` | `oramasearch/agent-runtime:<tag>`, the one image every launch runs on. Empty uses the operator's default (`internal/controller/lake_defaults.go`) |
| `library[]` | `{key, image}`: replaces the image of one specialized agent. Every Lake runs all nine (`agent-ask-orama`, `agent-project`, `agent-autosuggest`, `agent-creator`, `agent-eval-generator`, `agent-project-corpus`, `agent-autosuggest-corpus`, `agent-application`, `agent-experiments`) on the operator's default images; an entry cannot remove an agent |
| `openaiSecretName` | a Secret in `spec.namespace` holding `OPENAI_API_KEY`, the general Orama key. **People provide it**. Only the library agents in `spec.namespace` get it; it is **never** copied into a workspace namespace or given to a launch or schedule pod ([below](#oramas-provider-key-never-reaches-a-workspace)), and never written to a row or status |
| `model` | the frontier model agents default to (default `gpt-6.1-sol`) |
| `jobDeadlineSeconds` | the bound on one job launch (default 3600) |
| `isolation` | `Shared` (default) or `PerWorkspace`: where the library agents' processes run. `PerWorkspace` runs one process per library agent for each running workspace, each with only its workspace's bus credential ([below](#one-agent-process-per-workspace)) |

Every Lake runs its agents; there is no switch ([operator#539](https://github.com/oramasearch/orama-operator/issues/539)). The default images are listed in [the Lake README](README.md#default-images).

Every agent pod resolves with `ndots:2` and is handed lake and agent addresses as full Service
names (`<svc>.<ns>.svc.cluster.local`), so they resolve on the first query; see
[DNS](README.md#dns-ndots-2-on-lake-agent-and-app-pods).

## The Agent Library

Each `library` entry is a Deployment and a ClusterIP Service `<lake>-<key>` on port 8080 in
`spec.namespace`, controlled by the Lake and labelled `orama.cloud/library-agent`, probed on
`/healthz`. Its pod gets `LAKE_URL` (lake's in-cluster Service, `http://lake-<lake>.<ns>.svc.cluster.local:80`),
`AGENT_SERVICE_TOKEN`, its own agent-bus credential as `AGENT_BUS_TOKEN` with its key as
`AGENT_BUS_AGENT`, `OPENAI_API_KEY` from `openaiSecretName`, and the model as `AGENT_MODEL`,
`ASK_ORAMA_MODEL`, `PROJECT_MODEL` and `AUTOSUGGEST_MODEL`. An entry removed from the list is
deleted (only a Deployment this Lake controls).

lake learns where they are through its own environment:

| lake env | Value |
|---|---|
| `LAKE_AGENT_ENDPOINTS` | `key=http://<lake>-<key>.<ns>.svc.cluster.local:8080,...`, sorted |
| `LAKE_INTERNAL_URL` | lake's in-cluster address, handed to agents |
| `LAKE_AGENT_MODEL` | `spec.agents.model` |
| `LAKE_AGENT_SERVICE_TOKEN` | from Secret `lake-<lake>-agent-service-token` (key `token`, 32 random bytes, minted once), the bearer lake presents to every agent service |
| `LAKE_AGENT_BUS_DIR` | `/var/run/lake/agent-bus`, where Secret `lake-<lake>-agent-bus` is mounted: one file per library agent, its bus credential |

**The agent bus** (lake `docs/api/agent-bus.md`). Library agents take their tasks from lake over
Zetta pub/sub on lake's own Zetta (`spec.zetta`, which needs a Zetta image with pub/sub). Each
library agent authenticates to lake with **its own** credential. Secret `lake-<lake>-agent-bus`
holds one key per `library` entry (32 random bytes, hex, minted when the agent is added, kept
across reconciles, removed with the agent). Lake mounts the whole Secret; each agent pod gets only
its own key. The service token is never a bus credential, because user-created service launches
on `runtimeImage` hold it too. The operator runs no task loop: tasks, claims and results travel on
the bus.

## One agent process per workspace

[operator#1169](https://github.com/oramasearch/orama-operator/issues/1169), the operator half of
[lake#871](https://github.com/oramasearch/lake/issues/871) (lake's half is
[lake#1304](https://github.com/oramasearch/lake/pull/1304)). Decided under Steve's 2026-11-14
delegation: enterprise and isolated installs get one library-agent process per workspace.

A shared library agent is one process that serves every workspace of the Lake, with one bus
credential that reaches every workspace's tasks. Lake accepts a `LAKE_AGENT_BUS_DIR` file named
`<agent>.<workspace id>` as a bus credential **bound to that workspace**: it receives, claims,
reports and acts only for that workspace's tasks, always by reference, so the process never holds
another tenant's task credential.

**Which Lakes.** The Lake's own resource says so: `spec.agents.isolation: PerWorkspace`. No flag or
chart value differs between clusters. **Which workspaces**: the resources that exist, one per
workspace Zetta (`OramaZettaCluster` `<lake>-ws-<12 hex>`) the Lake controls and is not deleting.
The workspaces reconciler creates it for a running workspace and deletes it when the workspace
stops or is deleted, so the agents follow.

For such a Lake, once its image supports it:

- Secret `lake-<lake>-agent-bus` holds one credential per (agent, workspace), keyed
  `<agent>.<workspace id>` (32 random bytes, hex, kept across reconciles), and **no unbound
  `<agent>` key**: a process holding one would reach every workspace.
- Each (agent, workspace) is its own Deployment `<lake>-<agent>-<12 hex of the workspace id>` in
  `spec.namespace`, labelled `orama.cloud/library-agent` and `orama.cloud/workspace`, with the
  library agent's pod, env and network allowances, and `AGENT_BUS_TOKEN` from its own key only. It
  has **no Service**: lake dispatches library-agent tasks over the bus, and `LAKE_AGENT_ENDPOINTS`
  only lists which agents the Lake runs. No shared Deployment or Service runs.
- A workspace whose Zetta goes loses its credentials and its processes. Moving back to `Shared`
  restores the shared agents and removes the workspace ones.
- lake reads `LAKE_AGENT_BUS_DIR` when it starts, so its pod template carries
  `orama.cloud/agent-bus-keys`, a hash of the credential names: **a workspace started, stopped or
  deleted restarts lake once.** A Lake on a `ReadWriteOnce` store recreates its pod
  ([Rollouts](README.md#rollouts)). Lake re-reading the directory would remove the restart.
- The processes run in the Lake's namespace, as the shared ones do, so Orama's provider key stays
  out of workspace namespaces ([below](#oramas-provider-key-never-reaches-a-workspace)).
- Cost: nine Deployments per running workspace. The bus Secret holds about 1 KiB per workspace,
  so a Lake stays under the 1 MiB Secret limit to about 900 running workspaces.

**The switch is what the image says.** A lake image whose `lake manifest` lists the feature
`agent_bus_workspace_credentials` (`status.imageManifest.workspaceBoundAgentBus`) gets this. Until
then a `PerWorkspace` Lake keeps its shared agents, and condition `AgentIsolation` is `False` with
reason `ImageNotSupported`; once in effect it is `True` with reason `PerWorkspace` and the
workspace count. While a new image's manifest is being read, what is in place is kept. Lakes
without `PerWorkspace` carry no `AgentIsolation` condition and their pods do not change.

**Roll order.** This operator first. Then the agent images that claim `by_reference` (lake#871's
agent pull requests), because lake makes every claim by a bound credential by reference. Then a
lake image with lake#1304 whose manifest lists the feature. Then `spec.agents.isolation:
PerWorkspace` on the Lakes that want it.

## Launches

On every services poll, after the services and stores, the operator reads lake's `agent_launches`
(migration `0030`; a schema without it is skipped and logged). Every row requires `workspace_id`;
the query joins the operator-written `workspaces.namespace`, and there is no Lake-namespace
fallback. For each present launch:

1. **Its credential.** A Secret `<lake>-<slug>-credential` (key `token`) holding
   `lal.<launch id>.<32 random hex>`, created once, controlled by the Lake and labelled
   `orama.cloud/agent-launch-id`. The operator writes only its **SHA-256** into
   `agent_launch_status.credential_sha256`; lake resolves the credential by comparing digests and
   never stores it. The pod reads it as `LAKE_TOKEN` through a Secret reference, never a plain value.
   The pull credential, and for a service launch the agent service token, are copied into the
   workspace namespace. **Orama's provider key is not** ([below](#oramas-provider-key-never-reaches-a-workspace)).
2. **`job`** — a Job `<lake>-<slug>` from `runtimeImage` with `backoffLimit: 0` (an agent run is never
   retried behind the person's back), `activeDeadlineSeconds` = `jobDeadlineSeconds`,
   `ttlSecondsAfterFinished: 3600`, `AGENT_MODE=job` and the launch's `input` as `AGENT_INPUT`.
   Status: `Pending`, `Running`, then `Succeeded` or `Failed` with the container's exit code and its
   termination message (the runtime's ≤ 4 KiB summary) as `result`. A finished run's
   `credential_sha256` is cleared, so its credential stops resolving; its outcome is kept after the
   Job is garbage-collected.
3. **`service`** — a Deployment and Service `<lake>-<slug>` from `runtimeImage`, `AGENT_MODE=service`,
   `AGENT_SERVICE_TOKEN`. `Ready` with `endpoint = http://<lake>-<slug>.<ns>.svc.cluster.local:8080` once a replica is
   available; `Pending` until then.

**Agent pods never carry the Lake's `app.kubernetes.io/name` and `instance`**: those are the selector
of lake's Service and PodDisruptionBudget. A launch pod is `agent-runtime` / `<lake>-<slug>`, a library
agent `<key>` / `<lake>-<key>`. (A job pod once carried the Lake's labels and received lake's traffic
while it ran, which made the job's own calls to lake fail with *connection refused*.)

A job is `Succeeded` only once its pod's exit code and termination message are readable; until then
it stays `Running` with *collecting the result*, since a finished run is never revisited.

Every user-agent pod runs in its workspace namespace as UID 10001, non-root, with a read-only root
filesystem, a 1 GiB `emptyDir` at `/tmp`, no service-account token, all capabilities dropped, the
localized pull Secret, and `LAKE_URL`, `LAKE_TOKEN`, `WORKSPACE_ID`, `CUSTOMER_ID`, `AGENT_ID`, the
model and `TRACE_DIR`, and no provider key. `LAKE_URL` is Lake's in-cluster Service followed by
`/w/<workspace id>`; workspace NetworkPolicy permits that service while denying other Lake and
cluster traffic. Library agents remain trusted control-plane workloads in `spec.namespace`.

### Orama's provider key never reaches a workspace

[orama-operator#779](https://github.com/oramasearch/orama-operator/issues/779). `openaiSecretName` is
Orama's own key, a platform Secret of the Lake (umbrella `CLAUDE.md`, *Two levels of secrets*). A
launch or schedule pod runs a tenant's agent with public egress, so it never gets that key: no
`OPENAI_API_KEY`, no reference to the Secret, and no copy of the Secret in the workspace namespace.
Its model calls go through lake's model gateway (lake `docs/api/model-gateway.md`), with its own
`LAKE_TOKEN` as the API key; lake meters them in the run's workspace and stops honouring the
credential when the run ends. A user's own provider key, if they want one, is a workspace secret in
their workspace Zetta (lake#625), never Orama's.

**On roll**: every Lake reconcile runs `removeWorkspaceProviderKeyCopies`, which deletes each copy an
earlier operator made: a Secret named `openaiSecretName`, outside `spec.namespace`, controlled by the
Lake and labelled `orama.cloud/workspace`. A Secret of that name the Lake does not control is kept.
The first reconcile after the operator is rolled therefore removes them all. Launch Deployments and
schedule CronJobs are rewritten without the key on the same pass, so their next pods start without
it; a job pod already running keeps the environment it started with until it finishes.

An **absent** launch's Job or Deployment, Service and credential Secret are deleted (only objects
this Lake controls with this launch's id), and its status becomes `Stopped` with the credential
digest cleared. A finished job keeps `Succeeded` or `Failed`. Status rows are written only on
change; a starting or running launch keeps the Lake's poll fast.

## Schedules

A Python agent may have **any number of cron schedules**; lake writes each as a row of
`agent_cron_schedules` (migration `0031_agent_cron_schedules`), read on the same services poll.
Schedules also require `workspace_id` and run in the joined workspace namespace. For each present
schedule the operator:

1. generates the schedule's credential once, `las.<schedule id>.<secret>`, in Secret
   `<lake>-<slug>-credential` (labelled `orama.cloud/agent-schedule-id`), and writes only its
   SHA-256 into `agent_cron_schedule_status`;
2. keeps a **CronJob** `<lake>-<slug>` with the schedule's `cron` and `timeZone`,
   `concurrencyPolicy: Forbid`, `startingDeadlineSeconds: 300`, three successful and three failed
   Jobs kept, and a Job template that is a job launch's: `backoffLimit: 0`, the runtime image,
   `AGENT_MODE=job`, the schedule's `input` as `AGENT_INPUT`, the credential as `LAKE_TOKEN`, and
   the pod labels `agent-runtime` / `<lake>-<slug>`, never lake's selector;
3. reports the CronJob as `Ready` / *scheduled* with its last firing, and writes one
   `agent_cron_runs` row per Job it started (keyed by the Job's uid) whenever the run's phase
   changes. A Job is `Succeeded` only once its pod's exit code and termination message are readable;
   a finished run is written once.

A schedule lake marks absent loses its CronJob (background propagation, so a run under way stops)
and its credential Secret; the status becomes `Stopped` with no digest, and the credential stops
resolving in lake. A cron or time zone the API server refuses makes the schedule `Failed` with the
server's message. A run under way keeps the Lake's poll fast; otherwise runs are reported on the
regular poll. The operator needs `batch/cronjobs` (both the kubebuilder marker and the chart's
ClusterRole).

## Verify

```sh
go test ./internal/controller/ -run 'Agent|Launch|Library|LakeTellsLake|Schedule'
```

`TestLakeTellsLakeWhereItsAgentsAre`, `TestLibraryAgentsRunAsDeploymentsAndLeaveWhenUnlisted`,
`TestAJobLaunchRunsOnceWithItsCredentialAndReportsItsOutcome`,
`TestAServiceLaunchIsReadyWhenItsDeploymentIs`, `TestLaunchesFailWithoutARuntimeImage`,
`TestEachAgentScheduleIsItsOwnCronJobWithItsOwnCredential`,
`TestAScheduledRunIsReportedOnceItsOutcomeIsReadable`,
`TestARemovedScheduleLosesItsCronJobAndCredential`, `TestAScheduleWithoutAgentsEnabledOrAValidSlugFails`.
Workspace placement tests also assert that Jobs, CronJobs and their Secrets are absent from the
Lake namespace and carry the full `orama.cloud/workspace` UUID label.

## Change it

A new specialized agent is an enum value on `LakeLibraryAgent.key`, its row in lake's Agent Library,
and its private image. A new launch mode is lake's `agent_launch_mode_check` first, then a branch in
`reconcileAgentLaunch`.
