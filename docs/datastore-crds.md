# Datastore state as declarative, owned CRDs

**Epic:** [operator#40](https://github.com/oramasearch/orama-cloud-v1-operator/issues/40).

Per the production-migration principle ([CLAUDE.md](../CLAUDE.md) #3–#4), each datastore's
schema/topology must be modelled as its **own owned, reconciled CRD** — deterministic, idempotent,
auditable, garbage-collected on delete — instead of imperative one-shot Jobs, app-boot side effects,
or manual `kubectl`. This page tracks that CRD family and its cross-cutting concerns.

## The reconciled-CRD pattern

Every datastore-state CRD follows the same "external resource as an owned CRD" shape already used by
`CloudflareDNSRecord` / `CloudflareLoadBalancer`:

- **Owned by `OramaCloudV1`** (`SetControllerReference` → GC on delete), created from the parent
  reconcile (like the Cloudflare children), and listed in the parent's `Owns(...)` for GC + watch.
- **Reconciled idempotently** against the datastore's own API (HTTP management API, SQL, …), never a
  one-shot Job that races or drifts.
- **Finalizer** where external cleanup matters, so deleting the CR cascades to real teardown.
- **Anti-flap `.status`** (principle #7): the status subresource is written **only on a real state
  transition** (deep-compare first), with a standard `Ready` condition + print columns.
- **Ordered readiness gating:** the reconciler is a safe **no-op** (a clear `Ready=False` condition,
  not an error) until its datastore — and any credential Secret — is reachable, so it settles as soon
  as the store comes up rather than crash-looping.

## `RabbitMQTopology` — the broker's vhosts / users / permissions / streams

**Status: implemented.** `api/v1alpha1/rabbitmqtopology_types.go` +
`internal/controller/rabbitmqtopology_controller.go`.

### Why our own CRD (not a vendor operator)

The refinement on #40 first suggested driving the broker via the **RabbitMQ Cluster + Messaging
Topology operators**. Those were evaluated and **dropped** (principle #3): their bitnami images 404'd
(`ErrImagePull`, deprecated catalog). So the broker runs as a plain `rabbitmq:3.13-management`
StatefulSet (`reconcileBroker`), and the operator models its topology with **its own** small CRD
reconciled via the broker's **management HTTP API** (`:15672`) — a purpose-built CRD we control beats
an unstable vendor dependency ([#41](https://github.com/oramasearch/orama-cloud-v1-operator/issues/41)).

### What it reconciles

The management API is idempotent-PUT driven, applied in dependency order (vhosts → users → the
permissions that reference them → streams):

| Resource | Management API call |
|---|---|
| vhost | `PUT /api/vhosts/{vhost}` |
| user | `PUT /api/users/{user}` — `{ password (from a Secret), tags }` |
| permission | `PUT /api/permissions/{vhost}/{user}` — `{ configure, write, read }` regexes |
| stream | `PUT /api/queues/{vhost}/{name}` — `arguments: { x-queue-type: stream, … }` |

The default vhost `/` is percent-encoded to `%2F` in the path. A **transport** error (broker not up
yet) parks the CR in `Ready=False (BrokerUnavailable)` and requeues; a **management-API** error (e.g. a
stream declared with conflicting properties) surfaces `ReconcileError` and degrades **only that CR**,
never the main stack loop.

### The `OramaCloudV1`-owned instance

`reconcileBrokerTopology` creates + owns one `<cr>-broker` `RabbitMQTopology` wherever the broker runs
in-cluster (removing it — which runs the finalizer — where it doesn't). It declares:

- **vhost** `/`.
- **users** `oramacore-writer` / `oramacore-reader` — least-privilege per-service accounts
  (principle #4), untagged, passwords **generated once** into `<cr>-broker-users` (create-if-absent,
  `randHex`).
- **permissions** scoped to the exact stream `^oramacore-operations$` — the writer gets
  configure+write+read, the reader configure+read (each may declare the stream on connect, so both get
  `configure`; only the writer gets `write`).
- **stream** `oramacore-operations` — the writer→reader replication stream, declared as
  `x-queue-type=stream`.

Admin creds come from `<cr>-broker-admin` (keys `username`/`password`), whose values share a single
source of truth (`brokerAdminUser` / `brokerAdminPassword`) with the mounted `rabbitmq.conf` so the two
can never drift. All broker secrets are **create-if-absent** so generated/rotated material survives
reconciles.

### Two deliberate constraints (and their follow-ups)

1. **Bare stream, no retention.** RabbitMQ compares stream properties on (re-)declaration, and the
   native **stream protocol** `Create` OramaCore issues on connect is idempotent **only when
   properties match** — a mismatch makes OramaCore's Create fail. Retention (`x-max-age` /
   `x-max-length-bytes`) is **declaration-time only** (it cannot be applied later via policy). So the
   `oramacore-operations` stream is declared **bare** to match OramaCore's default Create; bounding its
   growth (principle #8) requires matching OramaCore's exact create args, confirmed on a live cluster —
   tracked as a follow-up, not set blind. The CRD fully supports `maxAge`/`maxLengthBytes` for streams
   the operator owns end-to-end.
2. **Engine still uses the shared admin user (for now).** The per-service users are declared so
   least-privilege is *ready to adopt*, but the engine's writer/reader still authenticate as the shared
   admin user — switching the engine config to `oramacore-writer`/`oramacore-reader` (generated password
   secrets, render-config substitution) is the fast-follow, kept separate so the replication e2e can be
   validated against the credential switch on a real cluster.

## `PostgresSchema` — the control-DB schema

**Status: implemented.** `api/v1alpha1/postgresschema_types.go` +
`internal/controller/postgresschema_controller.go`.

The control-plane Postgres schema (the dashboard's vendored Drizzle migrations) was applied by a bare
idempotent psql **Job** ([#34](https://github.com/oramasearch/orama-cloud-v1-operator/issues/34)). That
Job worked but was an imperative one-shot with **no applied-version tracking** — exactly what the epic
supersedes. It is now a reconciled `PostgresSchema` CRD:

- `reconcileControlDBMigrations` renders the schema into the `<cr>-controldb-migrations` ConfigMap and
  declares a `<cr>-controldb-schema` `PostgresSchema` referencing it, the CNPG connection Secret
  (`<cr>-controldb-app`, key `uri`), and a **version** = a content hash of the schema SQL (so it changes
  exactly when the vendored schema does).
- The `PostgresSchema` controller **owns** the migration Job (wait-for-DB → guard on the
  `crawler_configs` table → apply the whole schema in one transaction) and records
  `.status.appliedVersion` + `.status.phase` (`Pending`/`Applying`/`Applied`/`Failed`) + an `Applied`
  condition. The applied version is **read back from the Job's annotation**, so a bumped-but-not-re-run
  desired version never misreports.
- **No finalizer** — deleting the CR must never drop the schema/data; it only stops managing the
  migration (the Job is GC'd). **Ordered gating:** a safe no-op (`Applied=False WaitingForDatabase`)
  until the connection Secret exists.
- The pre-CRD `<cr>-controldb-migrate` Job is cleaned up on upgrade (the new Job runs under
  `<cr>-controldb-schema-migrate`).

**Follow-up:** the schema is a single combined SQL applied once (guard-gated), so a `version` bump
re-applies only after the prior Job is deleted — true incremental/versioned re-application needs
incremental migration files, tracked separately. **Least-privilege Postgres roles** (CNPG
`managed.roles`: one login role per service instead of the shared `oramacore` owner, per the #40
refinement + principle #4) is also a separate slice.

## Deferred / tracked separately

- **`ClickHouseSchema`** — the analytics tables are currently auto-created by analytics-processor
  (`create_tables: true`). Declaring them needs the authoritative DDL (owned by the app, not this
  repo) — tracked as a follow-up so we don't pin analytics to a guessed schema.
- **`TypesenseSchema` / `MongoSchema`** — as the ingestion/search paths require; tracked separately.
- **Pin every image** — determinism cross-cutting (no `:latest`); tracked separately (needs the
  intended production tags for the oramacore/ai-server/selenium images).
- **DR (backup / restore / snapshot CRDs)** — its own epic
  ([#33](https://github.com/oramasearch/orama-cloud-v1-operator/issues/33)).
