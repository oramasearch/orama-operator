# Copying a Prod 1 team: `OramaCloudV1Migration`

An `OramaCloudV1Migration` copies one Prod 1 team into one `OramaCloudV1` stack
([operator#653](https://github.com/oramasearch/orama-operator/issues/653), epic
[orama-bridge#137](https://github.com/oramasearch/orama-bridge/issues/137)). It is applied in the
stack's namespace and names the stack, the team, the team's projects, the tool's run, Prod 1, and how
far the copy should go. The operator runs each step as a Job on the cluster that holds the stack and
records the outcome in the resource's status.

The operator does not copy anything itself. Each Job runs `run-step` from the
`oramasearch/cloud-v1-migration` image, the `oramacore migration` tool of oramacore-aws packaged to
run unattended ([oramacore-aws#108](https://github.com/oramasearch/oramacore-aws/issues/108)). The
tool writes its `status --json` to the container's termination message and exits 0 (completed),
3 (refused by a guard or preflight), 4 (verification found a difference) or 1 (anything else).

Before this kind, an agent ran `oramacore migration plan / seed / catch-up / verify` from a Mac,
writing into a cluster's database outside the operator. The resource replaces that: what was copied
where is a file in the repository, and each run is an owned Job kept for a week.

On the managed clusters a migration is not applied by hand: it is an entry in the stack's
`OramaCloudV1Set` file, which the managing operator applies to the member's cluster
([below](#declared-in-the-set-oramacloudv1set)).

Sample: [`../config/samples/oramacloudv1migration.yaml`](../config/samples/oramacloudv1migration.yaml).
Field reference: [`crd-reference.md`](crd-reference.md#oramacloudv1migration-separate-crd-namespaced--one-prod-1-team-copied-into-a-stack).
Short name `ocv1m`; `kubectl get ocv1m` shows the target, step, apply and phase.

## Steps

`spec.step` is how far the migration should go. Each distinct `(step, apply, attempt)` runs once, as
its own Job, and an earlier Job is kept as the record (`ttlSecondsAfterFinished` one week):

| Step | Command | Job | Writes |
|---|---|---|---|
| `plan` | `run-step plan` | `<name>-plan-<attempt>` | never |
| `seed`, no `apply` | `run-step seed` | `<name>-seed-dry-run-<attempt>` | never (the tool's dry run) |
| `seed`, `apply` | `run-step seed --apply` | `<name>-seed-<attempt>` | the target |
| `catchUp`, no `apply` | `run-step catch-up` | `<name>-catch-up-dry-run-<attempt>` | never |
| `catchUp`, `apply` | `run-step catch-up --apply` | `<name>-catch-up-<attempt>` | the target |
| `verify` | `run-step verify` | `<name>-verify-<attempt>` | never |

The usual order is `plan`, `seed` without `apply`, `seed` with `apply`, `catchUp` with `apply`, and
`verify`. Raising `spec.attempt` runs the current step again: each catch-up before the cutover, or a
retry once a `Blocked` or `Failed` step's cause is fixed. `apply` counts only for `seed` and
`catchUp`, so toggling it never re-runs `plan` or `verify`. A name longer than 63 characters with its
suffix is cut and given a six-character hash of the whole name.

The Job has `backoffLimit: 0` and a six-hour `activeDeadlineSeconds`. The tool resumes from its run
directory, so a failed copy is read by a person and resumed by raising `attempt`, never retried
automatically. Two Jobs of one migration never run at once: a new run waits (`Pending`) while
another of its Jobs is still running. A finished run is never started again, even after its Job has
expired.

`spec.targetRef`, `teamID`, `projectIDs`, `runID` and `source` cannot change once created (a CEL
rule); a different team, run or source is a new migration. `step`, `apply`, `attempt`,
`evidenceSecretRef`, `image`, `runVolume` and `removeOnDelete` can.

## Before a Job starts

The operator checks, in order, and starts nothing until every check passes:

1. **No other Job of this migration is running** (`Pending`).
2. **The target is ready** (`Pending`, checked every 20 seconds and on every change of the stack):
   the `OramaCloudV1` exists, its phase is `Ready`, its `ClusterRowReady` condition is `True` (the
   tool reads the target's cluster id from the `c1` row), and every control DB `PostgresSchema` is
   applied at its current version, the migration role's grants included.
3. **The target is safe for a writing step** (`Blocked`): for `seed` or `catchUp` with `apply`, the
   stack must have `spec.components.dashboard.crons.suspend: true` and, when the analytics import is
   enabled, `spec.components.analyticsProcessor.import.suspend: true`. A copied team must not run
   the dashboard's schedules or feed analytics before its cutover. The message names the field.
4. **The step is in order** (`Blocked`): `catchUp` and `verify` need an applied `seed` that
   succeeded under this resource, recorded in the `Seeded` condition, which is set once and never
   cleared. `seed` and `catchUp` need `spec.evidenceSecretRef`.
5. **Every Secret exists with its key** (`Pending`): the Prod 1 database, master key and SSM
   credentials, the evidence Secret for `seed` and `catchUp`, the pull secret, the stack's
   `<target>-controldb-migration` and its master key Secret. Only the keys' presence is checked;
   no value reaches status, events or logs.

A `Blocked` refusal waits for a change to the migration or its stack. The run volume
`<name>-runs` is created before the first Job: `ReadWriteOnce`, owned by the migration, 20Gi on the
cluster's default class unless `spec.runVolume` says otherwise, and grown to a larger
`runVolume.size` when the next Job starts (the storage class must allow expansion). It holds the tool's manifest and payloads, which are customer data, and is kept across steps
so every step resumes the same run.

## The Job

The pod runs as uid and gid 10001 (`fsGroup` 10001, the image's user) with a read-only root
filesystem, an `emptyDir` on `/tmp`, no service-account token, all capabilities dropped and the
runtime's seccomp profile. Its labels are the stack's (`app.kubernetes.io/name: orama-cloud-v1`,
`instance: <target>`, the operator's `managed-by`) with `app.kubernetes.io/component: migration` and
`orama.cloud/migration: <name>`, which put it inside the stack's network sandbox.

An init container, `wait-for-database`, runs the stack's postgres-client image and waits up to ten
minutes for the migration role to sign in, so the tool never starts before CNPG has turned the
role's login on. It takes the connection from `PG*` variables, its password from the role's Secret.

Mounts: the run volume on `/runs`, `/tmp`, and for `seed` and `catchUp` the evidence key on
`/evidence/isolation.json` (read-only, mode 0400). The tool's environment is a contract with
`run-step`; every credential is a `secretKeyRef`:

| Variable | Value |
|---|---|
| `TEAM_ID`, `RUN_ID` | `spec.teamID`, `spec.runID` |
| `PROJECT_IDS` | `spec.projectIDs`, sorted and comma-joined |
| `RUNS_DIR` | `/runs` |
| `EVIDENCE_PATH` | `/evidence/isolation.json`, for `seed` and `catchUp` only |
| `ORAMA_MIGRATION_PROD1_DSN` | `source.databaseSecretRef` |
| `ORAMA_MIGRATION_PROD1_WRITER_URL`, `_READER_URL` | `source.writerURL`, `source.readerURL` |
| `ORAMA_MIGRATION_PROD1_MASTER_KEY` | `source.masterKeySecretRef` |
| `ORAMA_MIGRATION_PROD1_DEPLOYMENT_REVISION`, `_SCHEMA_REVISION` | `source.deploymentRevision`, `source.schemaRevision` |
| `ORAMA_MIGRATION_PROD1_SETTINGS_SSM_REGION`, `_INSTANCE_ID`, `_DOCUMENT` | `source.settings.region`, `.instanceID`, `.document` |
| `ORAMA_MIGRATION_PROD1_SETTINGS_DATA_DIR` | `source.settings.dataDir` |
| `ORAMA_MIGRATION_PROD1_SETTINGS_AWS_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY` | `source.settings.credentialsSecretRef`, keys `access_key_id`, `secret_access_key` |
| `ORAMA_MIGRATION_PROD2_DSN` | `<target>-controldb-migration`, key `uri` |
| `ORAMA_MIGRATION_PROD2_WRITER_URL` | `http://<target>-writer:8080` |
| `ORAMA_MIGRATION_PROD2_READER_URL` | `http://<target>-reader` |
| `ORAMA_MIGRATION_PROD2_MASTER_KEY` | the stack's master key Secret (`spec.masterAPIKeySecretRef`, or the generated `<target>-master-api-key`) |
| `ORAMA_MIGRATION_PROD2_DEPLOYMENT_REVISION` | the tag of the stack's writer image (its digest when it has no tag) |
| `ORAMA_MIGRATION_PROD2_SCHEMA_REVISION` | the applied version of the last control DB schema that creates tables, `<target>-controldb-schema-0008` today |

`ORAMA_MIGRATION_PROD2_CLUSTER_ID` is left unset: the tool reads the target's `c1` row. The reference
rows and the grants are left out of the schema revision because they are applied again on their own
changes (a Stripe price change); the tool binds its run to both revisions, so they must stay the
same from one step to the next.

### Prod 1's settings over SSM

The tool reads each index's `info.json` from the Prod 1 writer through one custom SSM document on
one instance (orama-bridge#205; Juan, 2026-10-19). The writer has no public address and its
security group admits only 8080 from its load balancer, so no cluster can reach it over SSH.
`source.settings` names the region, instance, document and the writer's `data_dir`; its
`credentialsSecretRef` is an IAM user that may run only that document on that instance.

## Outcome and status

When the Job finishes, the operator reads its newest pod:

- exit 0: `Succeeded`; an applied seed also sets `Seeded`;
- exit 3: `Blocked`; the tool's log says which guard refused;
- exit 4: `Failed`, verification found a difference;
- any other exit: `Failed`;
- a Job stopped at its deadline: `Failed`, whatever its pod says;
- `wait-for-database` failing: `Failed`, the tool never started.

The termination message, when it is a JSON object, is copied to `status.result` as the API server
stores it (the same object, keys sorted, whitespace dropped). The operator does not depend on its
keys. `status.step`, `attempt`, `apply`, `jobName`, `startedAt` and `completedAt` describe the last
Job. Status is written only on a real transition: a steady migration produces no writes.

| Phase | Meaning |
|---|---|
| `Pending` | waiting for an earlier Job, the target or a Secret |
| `Running` | the Job is running |
| `Succeeded` | the tool exited 0 |
| `Blocked` | the operator refused the step, or the tool did (exit 3) |
| `Failed` | the tool failed, the Job ran out of time, or the role could not sign in |
| `Removing` | the resource is being deleted |

Conditions: `Seeded` (an applied seed succeeded here), `TargetWritten` (an applied seed or
catch-up started, so the target may hold part of the team) and `Removed` (the removal on deletion
succeeded). Each is set once and never cleared.

## What the stack provides

Every `OramaCloudV1` provides the following, migration or not (principle 11: nothing is switched on
per stack). The stack's reconciler watches the migrations that target it.

- **The migration role** (principle 4). The control DB's CNPG `Cluster` declares the managed role
  `migration`: `superuser: false`, no `createdb`, `createrole`, `replication` or `bypassrls`,
  `connectionLimit: 4`, and `login: true` only while at least one `OramaCloudV1Migration` in the
  stack's namespace names it as `targetRef`. Its password is in `<target>-controldb-migration`
  (`kubernetes.io/basic-auth`: `username`, `password`, and `uri`, the tool's connection to the
  primary), generated once by the operator and labelled `cnpg.io/reload` so CNPG reads it.
- **Its grants**, the last control DB `PostgresSchema`, `<target>-controldb-migration-grants`:
  `SELECT, INSERT, UPDATE, DELETE` on every table in `public`, `USAGE, SELECT` on its sequences, and
  the same as default privileges for tables the owner creates later. No DDL, no `TRUNCATE`, no other
  schema. The file names the versions of the schemas before it that create tables, so a new schema
  version re-applies the grants. See [the control DB's schema](components.md#the-control-dbs-schema).
- **Network paths** in the stack's sandbox
  ([`network-sandbox.md`](infrastructure/network-sandbox.md#orama-cloud-v1-stack-namespace)): the
  migration pod reaches the control DB on 5432, the writer on 8080 and the readers on their pod port
  (80 with Envoy, 8080 without), and each of those admits it; DNS is the stack's `allow-dns-egress`.
- **Prod 1 egress**, `allow-migration-prod1-egress`, only while a migration targets the stack: the
  migration pod reaches public addresses on 5432 and 443. Its comment names the hosts it is for, the
  union over the stack's migrations of `source.databaseHost` on 5432 and the hosts of
  `source.writerURL`, `source.readerURL` and `ssm.<source.settings.region>.amazonaws.com` on 443.
  Enforcement is by port and public range, not by name: Cilium's FQDN policy needs AKS Advanced
  Container Networking Services, which these clusters do not run. Private, metadata, API-server,
  node and pod ranges stay excluded. The Prod 1 side adds its own limits: the IAM key runs one
  document on one instance, from the clusters' egress addresses only. Enforcing the hosts by name is
  a follow-up tied to ACNS.

## Deletion

The migration carries the finalizer `orama.cloud/migration`. Deleting it:

1. waits for a running Job to finish;
2. when `removeOnDelete` is set and `TargetWritten` is `True`, runs `run-step remove-target --apply`
   as `<name>-remove-<attempt>` (`Removing`). Exit 0 sets `Removed` and continues; once `Removed` is
   set the removal never runs again, whatever `spec.attempt` says and whether or not its Job still
   exists. Exit 3 means the team was switched to the target: the migration stays `Blocked` with the
   finalizer, because deleting a resource never deletes a switched customer, and a person decides.
   Any other failure also keeps it, `Failed`; raising `spec.attempt` runs the removal again. A stack
   that is gone or being deleted takes the copy with it, and the removal is skipped;
3. deletes the migration's Jobs, all finished by now, with their pods, and waits until no pod of the
   migration is left: `kubernetes.io/pvc-protection` keeps the run volume while any pod that mounts
   it exists, finished or not, and a finished Job keeps its pod for its week-long TTL;
4. deletes the run volume, which holds customer payloads, and waits until it is gone;
5. removes the finalizer.

Steps 3 to 5 report `Removing` with what they wait for. A failed or refused removal stops before
step 3, so its Jobs stay as the record.

To let go of a switched team without removing it, set `removeOnDelete: false` on the resource being
deleted: the run volume is deleted and the copy stays on the target.

## Declared in the set (`OramaCloudV1Set`)

Every Cloud v1 stack is a member of an `OramaCloudV1Set` on the managing cluster, and its
migrations are declared there too, in `spec.migrations`
([operator#654](https://github.com/oramasearch/orama-operator/issues/654)). One committed file says
which stacks exist and which teams are being copied into them. How to write an entry and the step
order: [`config/clusters/README.md`](../config/clusters/README.md#copying-a-prod-1-team-into-a-member).
Field reference: [`crd-reference.md`](crd-reference.md#lakeset-and-oramacloudv1set-separate-crds-cluster-scoped--instances-on-managed-clusters).

On each pass, after the members have been applied, the set's controller takes each entry in order:

1. **Waits for its member.** A member that is not applied (its cluster not `Ready`, its operator not
   installed, a refusal) leaves the migration `Pending`, and so does a cluster without the
   `OramaCloudV1Migration` kind.
2. **Checks the entry.** It refuses (`Blocked`, with the reason) a `spec.targetRef` that is not the
   member, a migration that would move to another member or namespace, a Secret named like one the
   stack gets from the set or its member, and one Secret name copied into one member from two
   sources. Two migrations may share one Secret from one source. Into a member whose cluster is not
   `production` it also refuses a production credential: a Secret its `spec.source` names
   (`databaseSecretRef`, `masterKeySecretRef`, `settings.credentialsSecretRef`), which is Prod 1's,
   unless labelled `orama.cloud/purpose` with that cluster's purpose as a fixture; one a production
   set lists; one labelled for another purpose
   ([#787](https://github.com/oramasearch/orama-operator/issues/787),
   [the rule](crd-reference.md#production-credentials-never-reach-a-nonprod-member)). Copying real
   Prod 1 data therefore runs only into production members.
3. **Copies its Secrets** into the member's namespace, the way a member's are copied, labelled
   `orama.cloud/managed-by-set: OramaCloudV1Set.<set>`.
4. **Applies the migration**: server-side apply, field owner `orama-operator-management`, the same
   label, `spec` as written plus `targetRef: <member>`. What the cluster refuses, a change to an
   immutable field included, is on the migration and in the set's message, and returned so it is
   retried; the applied migration stays as it was and is never deleted to get a change in, since
   deleting it runs `remove-target`.
5. **Reads its status back** into `status.migrations[]`: `step` and `phase` and `message` as the
   member's cluster reports them (`Pending` until that cluster's operator has reconciled the latest
   generation), with the cluster, namespace and copied Secrets. `status.migrationSummary`, the
   `Migrations` column, counts those that succeeded (`1/2 Succeeded`).

The set's phase is its members' alone: a `Blocked` or `Failed` migration does not make it `Degraded`.
The set looks again every 15 seconds while a migration is `Pending`, `Running` or `Removing`.

**Removal.** An entry removed from the file is deleted on its cluster, and its own finalizer then
does the cleanup described above. The set keeps reporting it in `status.migrations` until it is
gone, then deletes the Secrets it copied for it, except those another migration at that place, the
set or the member still copies there, and only those carrying the set's label. A removal holds only
that migration. Removing a member (whose entries go with it) under `deletionPolicy: Delete` deletes
its stack only once its migrations are gone; until then the member stays in `status.memberStatuses`
as `Deleting`. Deleting the set deletes every migration first, under either policy, and keeps the
set's finalizer until they are gone: a stack deleted under its migration would skip `remove-target`
and its refusal for a switched team. A migration stuck in `Blocked` holds that deletion, with its
message on the set. A cluster that is gone or being deleted takes its migrations with it.

## Not handled here

- Creating the isolation receipt: it stays an external, reviewed input.
- `switch` and `cutback`: switching a customer stays a human step.
