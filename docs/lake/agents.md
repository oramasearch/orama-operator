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
| `openaiSecretName` | a Secret in `spec.namespace` holding `OPENAI_API_KEY`, the general Orama key. **People provide it**; the operator copies it into each launch's workspace and never writes the key to a row or status |
| `model` | the frontier model agents default to (default `gpt-5.5`) |
| `jobDeadlineSeconds` | the bound on one job launch (default 3600) |

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
its own key. The service token is never a bus credential, and no user-created launch holds it
(agent-runtime#14, below). The operator runs no task loop: tasks, claims and results travel on
the bus.

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
   The pull credential and the OpenAI key are copied into the workspace namespace. **The shared agent
   service token is not** (agent-runtime#14): every library agent accepts it on `POST /v1/turn`, and a
   launch runs the account's own code. A copy an earlier operator made there (controlled by the Lake,
   labelled with the workspace) is deleted.
2. **`job`** — a Job `<lake>-<slug>` from `runtimeImage` with `backoffLimit: 0` (an agent run is never
   retried behind the person's back), `activeDeadlineSeconds` = `jobDeadlineSeconds`,
   `ttlSecondsAfterFinished: 3600`, `AGENT_MODE=job` and the launch's `input` as `AGENT_INPUT`.
   Status: `Pending`, `Running`, then `Succeeded` or `Failed` with the container's exit code and its
   termination message (the runtime's ≤ 4 KiB summary) as `result`. A finished run's
   `credential_sha256` is cleared, so its credential stops resolving; its outcome is kept after the
   Job is garbage-collected.
3. **`service`** — a Deployment and Service `<lake>-<slug>` from `runtimeImage`, `AGENT_MODE=service`,
   `AGENT_SERVICE_TOKEN` from the launch's own credential Secret, key `service-token`: hex
   HMAC-SHA256 of the shared token over `lake-agent-launch:<launch id>`, which lake derives the same way
   when it calls the launch, so it opens that launch alone (agent-runtime#14,
   `TestLaunchServiceTokenMatchesLakesDerivation`). `Ready` with `endpoint = http://<lake>-<slug>.<ns>.svc.cluster.local:8080` once a replica is
   available; `Pending` until then.

**Agent pods never carry the Lake's `app.kubernetes.io/name` and `instance`**: those are the selector
of lake's Service and PodDisruptionBudget. A launch pod is `agent-runtime` / `<lake>-<slug>`, a library
agent `<key>` / `<lake>-<key>`. (A job pod once carried the Lake's labels and received lake's traffic
while it ran, which made the job's own calls to lake fail with *connection refused*.)

A job is `Succeeded` only once its pod's exit code and termination message are readable; until then
it stays `Running` with *collecting the result*, since a finished run is never revisited.

Every user-agent pod runs in its workspace namespace as UID 10001, non-root, with a read-only root
filesystem, a 1 GiB `emptyDir` at `/tmp`, no service-account token, all capabilities dropped, the
localized pull Secret, and `LAKE_URL`, `WORKSPACE_ID`, `CUSTOMER_ID`, `AGENT_ID`, `OPENAI_API_KEY`,
the model and `TRACE_DIR`. `LAKE_URL` is Lake's in-cluster Service followed by
`/w/<workspace id>`; workspace NetworkPolicy permits that service while denying other Lake and
cluster traffic. Library agents remain trusted control-plane workloads in `spec.namespace`.

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
