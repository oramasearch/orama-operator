# Datastore state as declarative, owned CRDs

**Epic:** [operator#40](https://github.com/oramasearch/orama-operator/issues/40).

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
an unstable vendor dependency ([#41](https://github.com/oramasearch/orama-operator/issues/41)).

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

Admin creds come from `<cr>-broker-admin` (keys `username`/`password`), generated once with a random
password where the broker runs locally ([#307](https://github.com/oramasearch/orama-operator/issues/307)).
The same Secret is rendered into the broker's `rabbitmq.conf` and its boot definitions
(`credentials.json`, the admin user, `/` and its permissions), which RabbitMQ imports on every start,
so a changed password reaches a broker whose data volume holds the old one when the broker rolls. The
broker and harvest-master carry the Secret's data hash and roll on a change; the writer and readers
carry `<cr>-broker-users`' hash instead. All
broker Secrets are **create-if-absent**, so generated or supplied material survives reconciles; see
[components.md](components.md#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo).

### Two deliberate constraints (and their follow-ups)

1. **Bare stream, no retention.** RabbitMQ compares stream properties on (re-)declaration, and the
   native **stream protocol** `Create` OramaCore issues on connect is idempotent **only when
   properties match** — a mismatch makes OramaCore's Create fail. Retention (`x-max-age` /
   `x-max-length-bytes`) is **declaration-time only** (it cannot be applied later via policy). So the
   `oramacore-operations` stream is declared **bare** to match OramaCore's default Create; bounding its
   growth (principle #8) requires matching OramaCore's exact create args, confirmed on a live cluster —
   tracked as a follow-up, not set blind. The CRD fully supports `maxAge`/`maxLengthBytes` for streams
   the operator owns end-to-end.
2. **The engine signs in as the per-service users**
   ([#99](https://github.com/oramasearch/orama-operator/issues/99)). The writer uses `oramacore-writer`,
   a reader-only pod `oramacore-reader`, and every pod of the combined image `oramacore-writer` (its
   readers also produce, which `oramacore-reader` may not). render-config fills `__BROKER_PASSWORD__`
   from the user's key of `<cr>-broker-users`. The engine is written only once this topology reports
   `Ready` for its generation with both users applied, so a fresh stack never starts it before the
   users exist. harvest-master still uses the admin user; its queues are named per crawl, so its own
   user needs a pattern of its own (follow-up on #99).

## `PostgresSchema` — the control-DB schema

**Status: implemented.** `api/v1alpha1/postgresschema_types.go` +
`internal/controller/postgresschema_controller.go`.

The control-plane Postgres schema (the dashboard's vendored Drizzle migrations) was applied by a bare
idempotent psql **Job** ([#34](https://github.com/oramasearch/orama-operator/issues/34)). That
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
- Later dashboard migrations get their own ordered `PostgresSchema`: `<cr>-controldb-schema-0008`
  (guard `team_migrations`) and `<cr>-controldb-reference-data` (`applyEveryVersion`) follow the base
  schema once it is current ([#378](https://github.com/oramasearch/orama-operator/issues/378);
  [`components.md`](components.md#the-control-dbs-schema)).

**Version changes:** the controller foreground-deletes an obsolete migration Job, waits for its
pods to stop, and creates a replacement. Status stays `Applying` during replacement and records
`Applied` only for the completed desired Job. The combined SQL is skipped only when the guard table
exists and every migration the file records (`INSERT INTO schema_migrations …`) is in the database's
`schema_migrations` ([#271](https://github.com/oramasearch/orama-operator/issues/271)); Lake hashes
both SQL and its latest migration guard
([schema guide](lake/schema.md)). True incremental migrations remain #126. **Least-privilege Postgres roles** (CNPG
`managed.roles`: one login role per service instead of the shared `oramacore` owner, per the #40
refinement + principle #4) is also a separate slice. The first is in: the `migration` role of
[#653](https://github.com/oramasearch/orama-operator/issues/653), which the Prod 1 migration tool
signs in as, with row grants on `public` applied by the last control DB `PostgresSchema`
([`cloud-v1-migration.md`](cloud-v1-migration.md#what-the-stack-provides)).

## Deferred / tracked separately

- **`ClickHouseSchema` + `Typesense`/`Mongo` schema CRDs**
  ([#97](https://github.com/oramasearch/orama-operator/issues/97)) — the analytics tables are
  currently auto-created by analytics-processor (`create_tables: true`); declaring them needs the
  authoritative DDL (owned by the app, not this repo), so we don't pin analytics to a guessed schema.
- **Least-privilege adoption** ([#99](https://github.com/oramasearch/orama-operator/issues/99))
  — wire the engine to the per-service broker users (they're declared but unused today), bound the
  `oramacore-operations` stream's retention (needs OramaCore's exact create args), and model CNPG
  `managed.roles` per service instead of the shared owner (principle #4, revisits #32).
- **Pin every image** ([#98](https://github.com/oramasearch/orama-operator/issues/98)) —
  determinism cross-cutting (no `:latest`); needs the intended production tags for the
  oramacore/ai-server/selenium images.
- **DR (backup / restore / snapshot CRDs)** — its own epic
  ([#33](https://github.com/oramasearch/orama-operator/issues/33)).
