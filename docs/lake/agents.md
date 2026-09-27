# Lake agents

**What.** The operator runs lake's agents (lake#228): the **Agent Library**'s specialized agents
and the **launches** of user-created Python agents on `agent-runtime` (lake#231). The contract with
lake is lake's [`docs/api/agent-launches.md`](https://github.com/oramasearch/lake/blob/main/docs/api/agent-launches.md);
the code is `internal/controller/lake_agents.go`.

**Why.** Agents are how lake builds Pipelines from intent, recommends Opportunities, and runs the
agents people build. lake records what it wants and never talks to Kubernetes; the operator runs it
and reports back, as it does for services and stores.

## spec.agents

| Field | Meaning |
|---|---|
| `runtimeImage` | `oramasearch/agent-runtime:<tag>`, the one image every launch runs on. Empty: every launch is `Failed` with *agents are not enabled on this Lake* |
| `library[]` | `{key, image}` per specialized agent: `agent-ask-orama`, `agent-project`, `agent-autosuggest`, `agent-creator` |
| `openaiSecretName` | a Secret in `spec.namespace` holding `OPENAI_API_KEY`, the general Orama key. **People provide it**; the operator only references it, never creates or reads it |
| `model` | the frontier model agents default to (default `gpt-5.5`) |
| `jobDeadlineSeconds` | the bound on one job launch (default 3600) |

Agents are on when `runtimeImage` or `library` is set.

## The Agent Library

Each `library` entry is a Deployment and a ClusterIP Service `<lake>-<key>` on port 8080 in
`spec.namespace`, controlled by the Lake and labelled `orama.cloud/library-agent`, probed on
`/healthz`. Its pod gets `LAKE_URL` (lake's in-cluster Service, `http://lake-<lake>.<ns>.svc:80`),
`AGENT_SERVICE_TOKEN`, `OPENAI_API_KEY` from `openaiSecretName`, and the model as `AGENT_MODEL`,
`ASK_ORAMA_MODEL`, `PROJECT_MODEL` and `AUTOSUGGEST_MODEL`. An entry removed from the list is
deleted (only a Deployment this Lake controls).

lake learns where they are through its own environment:

| lake env | Value |
|---|---|
| `LAKE_AGENT_ENDPOINTS` | `key=http://<lake>-<key>.<ns>.svc:8080,...`, sorted |
| `LAKE_INTERNAL_URL` | lake's in-cluster address, handed to agents |
| `LAKE_AGENT_MODEL` | `spec.agents.model` |
| `LAKE_AGENT_SERVICE_TOKEN` | from Secret `lake-<lake>-agent-service-token` (key `token`, 32 random bytes, minted once), the bearer lake presents to every agent service |

## Launches

On every services poll, after the services and stores, the operator reads lake's `agent_launches`
(migration `0030`; a schema without it is skipped and logged). For each present launch:

1. **Its credential.** A Secret `<lake>-<slug>-credential` (key `token`) holding
   `lal.<launch id>.<32 random hex>`, created once, controlled by the Lake and labelled
   `orama.cloud/agent-launch-id`. The operator writes only its **SHA-256** into
   `agent_launch_status.credential_sha256`; lake resolves the credential by comparing digests and
   never stores it. The pod reads it as `LAKE_TOKEN` through a Secret reference, never a plain value.
2. **`job`** — a Job `<lake>-<slug>` from `runtimeImage` with `backoffLimit: 0` (an agent run is never
   retried behind the person's back), `activeDeadlineSeconds` = `jobDeadlineSeconds`,
   `ttlSecondsAfterFinished: 3600`, `AGENT_MODE=job` and the launch's `input` as `AGENT_INPUT`.
   Status: `Pending`, `Running`, then `Succeeded` or `Failed` with the container's exit code and its
   termination message (the runtime's ≤ 4 KiB summary) as `result`. A finished run's
   `credential_sha256` is cleared, so its credential stops resolving; its outcome is kept after the
   Job is garbage-collected.
3. **`service`** — a Deployment and Service `<lake>-<slug>` from `runtimeImage`, `AGENT_MODE=service`,
   `AGENT_SERVICE_TOKEN`. `Ready` with `endpoint = http://<lake>-<slug>.<ns>.svc:8080` once a replica is
   available; `Pending` until then.

**Agent pods never carry the Lake's `app.kubernetes.io/name` and `instance`**: those are the selector
of lake's Service and PodDisruptionBudget. A launch pod is `agent-runtime` / `<lake>-<slug>`, a library
agent `<key>` / `<lake>-<key>`. (A job pod once carried the Lake's labels and received lake's traffic
while it ran, which made the job's own calls to lake fail with *connection refused*.)

A job is `Succeeded` only once its pod's exit code and termination message are readable; until then
it stays `Running` with *collecting the result*, since a finished run is never revisited.

Every agent pod runs as UID 10001, non-root, with a read-only root filesystem, a 1 GiB `emptyDir` at
`/tmp`, no service-account token, all capabilities dropped, the Lake's `pullSecretName`, and
`LAKE_URL`, `CUSTOMER_ID`, `AGENT_ID`, `OPENAI_API_KEY`, the model and `TRACE_DIR`.

An **absent** launch's Job or Deployment, Service and credential Secret are deleted (only objects
this Lake controls with this launch's id), and its status becomes `Stopped` with the credential
digest cleared. A finished job keeps `Succeeded` or `Failed`. Status rows are written only on
change; a starting or running launch keeps the Lake's poll fast.

## Verify

```sh
go test ./internal/controller/ -run 'Agent|Launch|Library|LakeTellsLake'
```

`TestLakeTellsLakeWhereItsAgentsAre`, `TestLibraryAgentsRunAsDeploymentsAndLeaveWhenUnlisted`,
`TestAJobLaunchRunsOnceWithItsCredentialAndReportsItsOutcome`,
`TestAServiceLaunchIsReadyWhenItsDeploymentIs`, `TestLaunchesFailWithoutARuntimeImage`.

## Change it

A new specialized agent is an enum value on `LakeLibraryAgent.key`, its row in lake's Agent Library,
and its private image. A new launch mode is lake's `agent_launch_mode_check` first, then a branch in
`reconcileAgentLaunch`.
