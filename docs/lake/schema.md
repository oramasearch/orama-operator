# Lake control-plane schema

The operator vendors lake's `db/schema.sql` unchanged at
`internal/controller/assets/lake-schema.sql`. The current source is
[`oramasearch/lake` commit b88670fa27dbc58638d11ac945b407d77054cbd0](https://github.com/oramasearch/lake/blob/b88670fa27dbc58638d11ac945b407d77054cbd0/db/schema.sql),
verified on lake `main`. Its SHA-256 is
`f4578982ace5a673fd7663c8950f9dd4130309a8c3e814996cb8c2256710b394`.
The adjacent `lake-schema.source.json` records the repository, path, commit, and digest.
`TestLakeSchemaPinnedBytes` checks every embedded byte against that digest during ordinary tests;
`hack/check-lake-schema.py` independently downloads the pinned upstream file and compares bytes.

## Migration and rollout ordering

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
completed Job's version, and steady reconciliation does not rewrite status. A database already
at `0021` makes the migration Job exit successfully without executing the SQL file.

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
It covers a pre-0021 database, an already-updated database, and upgrade from a retained completed
0020 Job. It checks the parent column, cascading foreign key, index, migration marker, one-time
API/static token revocation, unchanged browser sessions, and stable status after success.
Because envtest has no kubelet or Job controller, the test executes the actual rendered Job
command with local `psql`, substituting only the ConfigMap mount path, and reports its exit
success through Job.status. It also completes foreground deletion after verifying that request;
there are no Kubernetes pods in this native harness.

`TestLakeSchemaGatesWorkloadRollout` uses the fake API client to check first startup, image-update
ordering, failed/pending schema status, stale versions, stale generations, and a missing success
condition. Additional Job tests cover replacing obsolete running and failed attempts. The version test shows the guard itself changes the hash for identical SQL:
`identity_exit_guard` gives `sha256:c9d5b1949040`; `session_lookup_guard` gives
`sha256:9aec095673cf` for this vendored file.

The regression was run before the fix: the rendered command printed
`schema already applied (identity_exit_guard present)`, status became Applied, and the test
failed with `PostgresSchema reported Applied with parent_session_id missing (column count 0)`.
The fixed tree applies the file before reporting Applied; the already-updated case prints
`schema already applied (session_lookup_guard present)` and preserves new tokens.
