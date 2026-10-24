# Lake control-plane schema

Schema Job pods inherit the owning Lake's `orama.cloud/lake-instance` network-sandbox label,
without the Lake application's Service labels. An immutable Job missing the required identity
is foreground-deleted and recreated with the same guarded script; replacement waits for the old
Job and pods to disappear. This is a metadata upgrade, not a schema version change. See
[`../infrastructure/network-sandbox.md`](../infrastructure/network-sandbox.md).

The operator vendors lake's `db/schema.sql` unchanged at
`internal/controller/assets/lake-schema.sql`. The current source is
[`oramasearch/lake` tag v0.5.0, commit 9c7c2fc787ae110cb62e32089750f3d37117dcf1](https://github.com/oramasearch/lake/blob/9c7c2fc787ae110cb62e32089750f3d37117dcf1/db/schema.sql),
through `0098_authentication_attempts`. Its SHA-256 is
`c8a7f174a979f4c1a414e3d3040b1f2f4c86613b033b7f472d093229b6728ec2`. Before it, lake v0.4.0 (9115632e),
through `0093_root_recovery_participants`.

**Pin only a commit on lake `dev` or a release tag, never a pull request's branch.** Lake records a
migration by name only, with no checksum, so a migration applied from a branch and changed before it
merges leaves every Lake on the old version, and nothing reports it.
The adjacent `lake-schema.source.json` records the repository, path, commit, and digest.
`TestLakeSchemaPinnedBytes` checks every embedded byte against that digest during ordinary tests;
`hack/check-lake-schema.py` independently downloads the pinned upstream file and compares bytes.

## Migration and rollout ordering

Migrations `0094` to `0098`, in lake v0.5.0, apply before the lake v0.5.0 image rolls; the schema
gate holds that image until they have applied. Lake v0.4.0 keeps running on them. Not all are purely
additive:

| Migration | What it does | Guard |
|---|---|---|
| `0094_clerk_revocation` (lake#948) | `clerk_user_states`, `users.clerk_suspended`; **once, revokes every session and device of a Clerk-linked account** (everyone signs in again) | `clerk_revocation_guard` |
| `0095_model_token_prices` (lake#961) | widens the price classes and seeds the default models' token prices | `model_token_prices_guard` |
| `0096_email_recovery_limits` (lake#968) | `offline_recovery_budgets`, a cancelled recovery status | `email_recovery_limits_guard` |
| `0097_cloud_order_entitlements` (lake#972) | the subscription and grace columns on `cloud_orders`, the view `cloud_order_desired_state`, and a **unique index on `stripe_subscription_id`**: it fails if two orders already share one | `cloud_order_entitlements_guard` |
| `0098_authentication_attempts` (lake#966) | the shared failed-verification limits table | `authentication_attempts_guard` |

Migrations `0091_account_deletion` (lake#810, lake#1014), `0092_saml_sign_in` (lake#836, lake#1019)
and `0093_root_recovery_participants` (lake#934, lake#1038), all in lake v0.4.0, are additive. Their
guards are `account_deletion_guard`, `saml_sign_in_guard` and `root_recovery_participants_guard`.
Apply them before the lake v0.4.0 image rolls; the schema gate holds that image until they have
applied.

Migration `0090_billing_contacts` (lake#857, lake#864) adds `billing_contacts`, written by lake alone:
one row per account with the email, name and address its invoices carry, with guard
`billing_contacts_guard`. Apply it before a lake image containing lake#864 rolls; the schema gate
holds that image until it has applied.

Migration `0089_cloud_orders` (lake#831, in lake v0.3.0) adds `cloud_orders` and
`cloud_order_events`, with guard `cloud_orders_guard`. Lake writes the orders; a `LakeOrderSource`
(orama-operator#634, [`../infrastructure/orders.md`](../infrastructure/orders.md)) claims paid ones and
writes only their state, host, failure reason and claim. Apply it before lake v0.3.0 rolls; the schema
gate holds that image until it has applied.

Migration `0083_methodology_deployments` (lake#690) adds `methodology_deployments` (written by lake
when a workspace methodology's model is published), `methodology_deployment_status` (written by the
operator: phase, message, the SHA-256 of the deployment's `lmd.` credential, the serve Service's
in-cluster endpoint) and the `methodology_deployment` kind of `workload_resources`, with guard
`methodology_deployments_guard`, now Lake's PostgresSchema guard. Every `workload_resources` kind
check in the schema (the table's own, `0077`'s, `0078`'s and `0083`'s) lists all nine kinds, so the
schema reapplies over rows of every kind. Until it has applied, the operator serves no model
([methodology-runs.md](methodology-runs.md#serving-a-model)).

Migration `0082_methodology_run_queue` (orama-operator#293) lets `methodology_run_status.phase` be
`Queued`, with guard `methodology_run_queue_guard`. The operator holds runs back past a workspace's
cap and places a GPU run's work volume where its class has nodes
([methodology-runs.md](methodology-runs.md)).

Migration `0081_app_transfer` (lake#661) adds `app_transfer`, written only by the operator: each
app's bytes in and out through the ingress controller, by UTC hour, with the app's workspace. Guard
`app_transfer_guard`. The operator reports app traffic ([app-transfer.md](app-transfer.md)) and,
until this schema has applied, skips the write.

Migration `0080_store_mounts` (lake#666) adds `store_mounts` (written by lake: which services and
apps mount a store, at which path, read-only or not) and `store_status.used_bytes` /
`used_bytes_at` (written by the operator), with guard `store_mounts_guard`. Until it has applied,
the operator mounts nothing and measures nothing ([stores.md](stores.md#mounts-lake666)).

Migration `0079_mcp_connection_dispatch` (lake#684, lake#685) adds `mcp_connection_dispatch`, written
by lake alone: one row per MCP connection that streams or re-exports tools (ids, a flag and a
digest; the connection itself lives in its workspace Zetta), with guard
`mcp_connection_dispatch_guard`. The operator reads nothing of it. Apply it before a lake with
lake#684 rolls: that lake reads the table on every workspace MCP request and every stream pass.

Migration `0078_methodology_runs` (lake#655) adds `methodology_runs` (written by lake),
`methodology_run_status` (written by the operator: phase, exit code, the SHA-256 of each run's
credential, the sandbox's start and finish) and the `methodology_run` kind of `workload_resources`,
with guard `methodology_runs_guard`. The operator runs methodology runs
([methodology-runs.md](methodology-runs.md)) and, until this schema has applied, skips them; the
workload pass reads them by id and finds none.

Migration `0030_agent_launches` (lake#231) adds `agent_launches` (written by lake),
`agent_launch_status` (written by the operator, including the SHA-256 of each launch's credential),
`agent_schedules`, `ask_messages`, and the completion table `public.agents_guard`. Migration
`0031_agent_cron_schedules` (lake#244) adds `agent_cron_schedules` (written by lake: any number of
cron schedules per agent), `agent_cron_schedule_status` and `agent_cron_runs` (written by the
operator: each schedule's CronJob status and credential digest, and one row per Job it started),
and the completion table `public.agent_cron_guard`. It leaves 0030's `agent_schedules` unread and
in place. Migration `0032_agent_tasks` (lake#269) adds `agent_tasks` (a library agent's turn, published
on lake's agent bus), `agent_task_status` (written by lake as the agent claims and reports; the
SHA-256 of each task's credential), `opportunities` (Design → Opportunities), and the completion
table `public.agent_tasks_guard`. Migration `0033_agent_task_events` (lake#275) adds `agent_task_status.events_from` (where a task's events start on `agent-events`) and `public.agent_task_events_guard`, now Lake's PostgresSchema guard. The operator reads none of them:
tasks travel on Zetta pub/sub (lake `docs/api/agent-bus.md`). Migration `0029_non_expiring_credentials` (lake#234) adds
`customers.allow_non_expiring_credentials` and `agent_sessions.revoked_at` with guard
`credentials_guard`. Both are idempotent; a database at `0028` applies the whole file in one
transaction. The operator runs launches ([agents.md](agents.md)) and, until this schema has applied,
skips them without touching services or stores.

Migration `0028_stores` (lake#196) adds a customer's stores: the tables `stores` (written by lake)
and `store_status` (written by the operator), their indexes, and the completion table
`public.stores_guard`, which is now Lake's PostgresSchema guard. No existing table changes. A
database already at `0027` applies the whole idempotent file in one transaction; one already at
`0028` exits with `schema already applied (stores_guard present)`. The operator reconciles stores
([stores.md](stores.md)) and, until this schema has applied, skips them without touching services.

Migration `0027_analytics_services` (lake#195) widens the `services.kind` check once more, adding
`duckdb` and `polars` (dropping and re-adding `service_kind_check`, which every existing row already
satisfies), and adds the completion table `public.analytics_services_guard`, which was Lake's
PostgresSchema guard until `0028`. No data changes. A database already at `0026` applies the whole idempotent
file in one transaction; one already at `0027` exits with
`schema already applied (analytics_services_guard present)`. The operator runs the new kinds
([services.md](services.md#duckdb-and-polars-lake195)); lake can only write them once this schema has
applied.

Migration `0026_sqlite_services` (liquid-bridge decision 47, amended) widens the `services.kind`
check once more, adding `sqlite` (dropping and re-adding `service_kind_check`, which every existing
row already satisfies), and adds the completion table `public.sqlite_services_guard`, which was
Lake's PostgresSchema guard until `0027`. No data changes. A database already at `0025` applies the whole
idempotent file in one transaction; one already at `0026` exits with
`schema already applied (sqlite_services_guard present)`. The operator runs the new kind
([services.md](services.md#sqlite-decision-47-amended)); lake can only write it once this schema has
applied.

Migration `0025_service_catalog` (liquid-bridge decision 47) widens the `services.kind` check to
`zetta, postgres, kafka, clickhouse, tidb, cassandra, valkey` (dropping and re-adding
`service_kind_check`, which every existing row already satisfies) and adds the completion table
`public.service_catalog_guard`, which was Lake's PostgresSchema guard until `0026`. No data changes. A
database already at `0024` applies the whole idempotent file in one transaction; one already at
`0025` exits with `schema already applied (service_catalog_guard present)`. The operator runs the
new kinds ([services.md](services.md#the-catalog-decision-47)); lake can only write them once this
schema has applied.

Migration `0024_services` (liquid-bridge decision 46) adds `services`, written by lake, and
`service_status`, written by the operator's managed-services poll ([services.md](services.md)),
plus the completion table `public.services_guard`, which was Lake's PostgresSchema guard until `0025`. It
is additive: no backfill. A database already at `0023` applies the whole idempotent file in one
transaction and gains the three tables; one already at `0024` exits with
`schema already applied (services_guard present)`. The services poll waits for the schema to report
Applied at this version before it reads the tables.

Migration `0023_custodial_identities` (lake#148) adds `custodial_identities`, the TAAP identities
lake mints and holds for people who sign in through Clerk, and the completion table
`public.custodial_identity_guard`, which was Lake's PostgresSchema guard until `0024`. It is additive: no
backfill and no revocation. A database already at `0022` applies the whole idempotent file in one
transaction and gains the two tables; one already at `0023` exits with
`schema already applied (custodial_identity_guard present)`. The binary that writes the table
(lake `c045b96` or later) must roll only after the schema reports Applied at this version.

Migration `0021_token_attenuation` adds `sessions.parent_session_id`, its cascading foreign key,
and `sessions_parent_session_id_idx`. It revokes existing API and static tokens once, using the
`schema_migrations` record to preserve tokens issued after migration. Its completion table is
`public.session_lookup_guard`. Lake's PostgresSchema uses that table as its guard; the full
SQL runs in one transaction, so the table becomes visible only after successful commit.

Lake sets `spec.version` to `schemaVersion(SQL + "\n" + guardTable)`. A SQL change or guard change
therefore starts a new apply attempt. On a version change, the PostgresSchema controller deletes
its obsolete Job with foreground propagation and waits for the Job and its pods to disappear
before creating a replacement. This also handles a completed old Job already present at upgrade.
It refuses to replace another resource's Job. A same-version failed Job remains Failed until
explicitly retried by deleting that Job; changing the desired version replaces it automatically.

The schema remains Applying while a Job is running or being replaced. Applied records the
completed Job's version, and steady reconciliation does not rewrite status.

### When the Job skips the file

The Job skips the SQL file only when both hold:

1. the guard table exists; and
2. every `INSERT INTO schema_migrations (version) VALUES ('NNNN_…')` in the mounted file names a
   version present in the database's `schema_migrations`.

It then prints `schema already applied (<guard> present, every schema_migrations row recorded) —
nothing to do`. If any recorded version is missing, it prints `<guard> present but
schema_migrations lacks: <versions>` and applies the whole file in one transaction, exactly as on a
fresh database. Every lake migration is idempotent and its one-time data changes check their own
`schema_migrations` row, so the migrations already present are no-ops.

The guard alone was not enough
([#271](https://github.com/oramasearch/orama-operator/issues/271)). On 2026-10-05 lake's
`0067_workspace_store` merged after `0068` and `0069` had applied on dev. The vendored file contained
0067, but the Job saw `workspace_volumes_guard` (0069) and printed `schema already applied
(workspace_volumes_guard present) — nothing to do`, so the PostgresSchema reported `Applied` without
0067. Dev was repaired by hand.

The guard check stays. A database with the guard but no `schema_migrations` table, and a schema file
that records no migrations (the v1 dashboard schema), keep the guard-only behavior. The Job extracts
versions with `sed`; `schemaMigrationVersions` in Go uses the same pattern, and
`TestSchemaMigrationSedMatchesGo` checks both find the same versions in the vendored file.
`LakeRestoreRehearsal` uses the Go list to fail a restore, upgrade or failover rehearsal whose
database lacks any of them ([Postgres data-service rehearsals](data-services/postgres.md)).

Lake reconciles the control DB and schema before creating or updating its Deployment and
disruption budget. It requires the desired schema version, the observed current generation,
phase Applied, and condition Applied=True. Until then, first startup creates no Deployment;
existing Deployments keep their current spec. Pending, failed, or stale schema status cannot
release a new image. Disabling operator-managed Postgres opts out of this gate. Incremental
migration files and per-migration orchestration remain [#126](https://github.com/oramasearch/orama-operator/issues/126).

## Re-vendoring

When lake adds a migration:

1. Resolve lake `main` to a commit and fetch its complete `db/schema.sql` without editing it:

   ```sh
   lake_commit=$(gh api repos/oramasearch/lake/commits/main --jq .sha)
   gh api -H 'Accept: application/vnd.github.raw+json' \
     "repos/oramasearch/lake/contents/db/schema.sql?ref=$lake_commit" \
     > internal/controller/assets/lake-schema.sql
   shasum -a 256 internal/controller/assets/lake-schema.sql
   ```

2. Record that commit and SHA-256 in `internal/controller/assets/lake-schema.source.json`.
   Set `lakeSchemaGuardTable` in `lake_controller.go` to the newest migration's completion table.
   Each migration must record itself with `INSERT INTO schema_migrations (version) VALUES ('NNNN_…')`
   on a line of its own: the Job and the rehearsal both read those lines to decide what is applied.
   The hash includes the guard. Confirm the entire combined SQL remains safe to reapply and that
   the guard becomes visible only after the migration commits. Add the guard to the Postgres
   rehearsal verification query and extend the schema proof for the migration's actual effects.
3. Run `python3 hack/check-lake-schema.py` to compare against the pinned upstream bytes. It exits
   unsuccessfully on a digest or byte mismatch. To check a local lake checkout instead, pass its
   `db/schema.sql` path. Keep the source record, SQL, guard, tests, and matching documentation in
   the same reviewed change.
4. Run the proof below before proposing the operator update. The reviewed operator update must
   reach the environment before the new lake image is requested; the schema gate then holds
   that image until its required schema has applied.

## Verification

Use native envtest Kubernetes 1.30 binaries and a local Postgres server. No running Kubernetes
cluster is used. The suite needs `psql`, `createdb`, and `dropdb` on `PATH`, plus an existing local
role with permission to create databases:

```sh
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path 1.30.x)"
export LAKE_SCHEMA_TEST_POSTGRES=1
export PGUSER=stevesperandeo
export GOFLAGS=-p=6
export GOMAXPROCS=6
go build ./... && go vet ./... && go test ./...
go test ./internal/controller -run 'TestLakeSchema' -count=1 -v
helm lint chart/
python3 hack/check-lake-schema.py
```

The envtest suite creates fresh databases at `127.0.0.1:5432` and drops only those it created.
It covers a pre-0021 database, an already-updated database, upgrade from a retained completed
0020 Job, and a database holding `workspace_volumes_guard` with 0067's column, completion table and
`schema_migrations` row removed (`guard_present_earlier_migration_missing`), which must re-apply the
file, restore 0067 and leave earlier one-time migrations alone. Every case checks that
`schema_migrations` ends with exactly the versions the file records. It checks the parent column, cascading foreign key, index, migration marker, one-time
API/static token revocation, unchanged browser sessions, and stable status after success.
Because envtest has no kubelet or Job controller, the test executes the actual rendered Job
command with local `psql`, substituting only the ConfigMap mount path, and reports its exit
success through Job.status. It also completes foreground deletion after verifying that request;
there are no Kubernetes pods in this native harness.

`TestPgMigrationScriptDecidesFromEveryMigration` runs in the ordinary suite without Postgres: it
executes the rendered Job script against a stub `psql` that answers the missing-migration query from
the versions the script actually sent. It covers a fresh database, a guard with 0067 missing, a fully
recorded database, a guard without `schema_migrations`, and a file that records no migrations.

`TestLakeSchemaGatesWorkloadRollout` uses the fake API client to check first startup, image-update
ordering, failed/pending schema status, stale versions, stale generations, and a missing success
condition. Additional Job tests cover replacing obsolete running and failed attempts. The version test shows the guard itself changes the hash for identical SQL:
for the file vendored at `0a701ef`, `services_guard` gives `sha256:8fcd390d01ef` and
`service_catalog_guard` gives `sha256:b4958802878c`; for the file vendored at `17b4ad5`,
`service_catalog_guard` gives `sha256:684a7cbc81a8` and `sqlite_services_guard` gives
`sha256:5f51287439d4`; for the file vendored at `aee5cda`, `sqlite_services_guard` gives
`sha256:7cfdcc166fea` and `analytics_services_guard` gives `sha256:c6ca15f2c44f`; for the file
vendored at `9aa6141`, `analytics_services_guard` gives `sha256:d39a657de76b` and `stores_guard`
gives `sha256:28cc3b7c40b6`.

The regression was run before the fix: the rendered command printed
`schema already applied (identity_exit_guard present)`, status became Applied, and the test
failed with `PostgresSchema reported Applied with parent_session_id missing (column count 0)`.
The fixed tree applies the file before reporting Applied; the already-updated case prints
`schema already applied (custodial_identity_guard present)` and preserves new tokens.

The #271 regression was run before the fix by restoring the guard-only decision in the rendered
script: `guard_present_earlier_migration_missing` failed with `missing migration was not re-applied`
and the stub-`psql` case reported `applied = false, want true`. With the fix both pass.
