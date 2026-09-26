# lake-sqlite

`lake-sqlite` is the small web service behind lake's **`sqlite` service kind**: one SQLite database
behind an HTTP API with a bearer token. SQLite has no upstream Kubernetes operator, so the Orama
operator runs it itself, one StatefulSet per service ([services.md](services.md#sqlite-decision-47-amended);
liquid-bridge decision 47, amended). The contract is the `sqlite` row of lake's
[`docs/control-plane/services.md`](https://github.com/oramasearch/lake/blob/17b4ad57f2571c30516f7ecb85c03a4165e32e59/docs/control-plane/services.md)
and the paragraph after its table; this page is the whole API as built.

| | |
|---|---|
| Source | [`cmd/lake-sqlite/`](../../cmd/lake-sqlite/) (`main.go`, and the HTTP API in `internal/server/`) |
| Image | `oramasearch/lake-sqlite:<tag>`, built from [`build/lake-sqlite/Dockerfile`](../../build/lake-sqlite/Dockerfile) for `linux/amd64` |
| Driver | [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) v1.38.2 — SQLite translated to pure Go, no cgo; **BSD-3-Clause** (SQLite itself is public domain). Chosen at Steve's direction that SQLite be offered |
| Runtime | `gcr.io/distroless/static:nonroot` (no shell, no libc), user `65532:65532`, one static binary |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `LAKE_SQLITE_TOKEN` | — (required) | the bearer token; the operator mounts it from Secret `<name>-credentials`, key `token` |
| `LAKE_SQLITE_PATH` | `/data/main.db` | the database file; its directory must exist and be writable |
| `LAKE_SQLITE_ADDR` | `:8080` | the listen address |
| `LAKE_SQLITE_MEMORY_LIMIT` | unset | the container memory limit in bytes (the operator passes `limits.memory`); the Go heap is held to three quarters of it (`debug.SetMemoryLimit`) and SQLite's allocator to a quarter (`PRAGMA soft_heap_limit`) |

The database is opened with `journal_mode=WAL`, `busy_timeout=5000` and `foreign_keys=ON` on every
connection, and transactions begin `IMMEDIATE` (they take the write lock at `BEGIN`, so two batches
never deadlock upgrading a read lock). `synchronous` stays SQLite's default, `FULL`: every commit is
on disk before it is answered. The pool holds up to four connections: WAL lets readers run beside the
one writer, and writers wait up to five seconds for the lock. At startup the service opens the
database and runs `SELECT 1`, and exits non-zero if either fails or the token is empty.

## API

Every route but `GET /healthz` needs **`Authorization: Bearer <token>`**. The presented and the
configured token are both hashed with SHA-256 and compared in constant time; a missing, malformed
or wrong token is `401` (with `WWW-Authenticate: Bearer`), before the route or method is looked at.

A statement is `{"sql": "...", "params": [...]}`. `params` is positional (`?` or `?NNN`) and may be
omitted. Each parameter binds as:

| JSON | Binds as |
|---|---|
| integer literal (`42`, `-7`) | `INTEGER` (64-bit; a literal that does not fit is refused) |
| number with a fraction or exponent (`2.5`, `1e3`) | `REAL` |
| string | `TEXT` |
| `true` / `false` | `INTEGER` 1 / 0 (SQLite has no boolean) |
| `null` | `NULL` |
| `{"$base64": "<standard base64>"}` | `BLOB` |

Any other array or object is refused with `400 invalid_params`. Request bodies are strict JSON:
unknown fields and trailing data are refused.

### `POST /v1/query`

`{"sql", "params"}` answers `200`:

```json
{"columns": ["id", "name", "score", "avatar", "note"],
 "rows": [[1, "ada", 9.5, {"$base64": "iVBORw0K..."}, null]],
 "truncated": true}
```

Values come back as real JSON types: `INTEGER` as a JSON integer (exact to 64 bits; a JavaScript
client loses precision above 2^53), `REAL` as a JSON number that always has a fraction or exponent
(`1.0`, never `1`) so it is told apart from an integer, `TEXT` as a string, `BLOB` as
`{"$base64": "..."}`, `NULL` as `null`. SQLite can store an infinite `REAL`, which JSON cannot hold:
it is the string `"Infinity"` or `"-Infinity"`. At most **10,000 rows** are returned; when there
were more, the answer stops there and carries `"truncated": true` (the key is absent otherwise). A
statement that returns no rows still answers `columns` and an empty `rows`.

Text stored in a column **declared** `DATE`, `DATETIME` or `TIMESTAMP` is read by the driver as a
time when it parses as one, and is returned as text again in SQLite's own format (`2026-09-26`, or
`2026-09-26 10:11:12[.fraction][±hh:mm]`). A value stored as `2026-09-26T10:11:12Z` therefore reads
back as `2026-09-26 10:11:12`. Declare such columns `TEXT` (or store Unix times as `INTEGER`) to have
every byte returned as stored.

### `POST /v1/exec`

`{"sql", "params"}` answers `200` `{"rows_affected": 2, "last_insert_id": 3}`. They are SQLite's
`changes()` and `last_insert_rowid()` for the connection that ran the statement; after a statement
that is not an `INSERT`, `UPDATE` or `DELETE`, SQLite leaves them at their previous values.

### `POST /v1/batch`

`{"statements": [{"sql", "params"}, ...]}` runs every statement **in order, in one transaction**,
and answers `200` `{"results": [...]}`, one result per statement: a statement that returns columns
(a `SELECT`, or an `INSERT ... RETURNING`) answers like `/v1/query` (with the same 10,000-row cap
per statement), any other like `/v1/exec`. If any statement fails, the whole transaction is rolled
back and the answer is that statement's error, its message prefixed with `statements[<i>]: `.
Parameters are checked before the transaction begins, so a bad parameter runs nothing. An empty
`statements` is `400 invalid_request`.

### `GET /healthz`

No token. `200` `{"status": "ok"}` only when the database opens and `SELECT 1` answers within two
seconds; otherwise `503` `{"error": {"code": "unavailable", ...}}`. `HEAD` is accepted too. The
operator's startup, readiness and liveness probes all call it, so a service is Ready exactly while
its database answers.

### Errors

Every error is `{"error": {"code": "...", "message": "..."}}`:

| Status | `code` | When |
|---|---|---|
| 400 | `sql_error` | SQLite refused the statement (syntax, missing table, constraint or foreign-key violation, …); the message is SQLite's |
| 400 | `invalid_request` | the body is not the expected JSON, `sql` is empty, or `statements` is empty |
| 400 | `invalid_params` | a parameter cannot be bound (see the table above) |
| 401 | `unauthorized` | no or a wrong bearer token |
| 404 | `not_found` | an unknown route (with a valid token) |
| 405 | `method_not_allowed` | a route called with the wrong method |
| 413 | `body_too_large` | a body over **16 MiB** |
| 503 | `busy` | the database stayed locked past the 5-second busy timeout |
| 503 | `unavailable` | `/healthz`: the database does not answer |
| 504 | `timeout` | the request ran past its **30-second** limit; the running statement is interrupted |
| 507 | `disk_full` | the volume is full |

### Limits and lifecycle

- Every request is bounded to 30 s, the SQL included (the statement is interrupted when the time
  runs out). The server reads headers within 10 s, and closes idle connections after 2 minutes.
- On `SIGTERM` (or `SIGINT`) the server stops accepting connections, lets in-flight requests finish
  for up to 30 s, then closes the database, which checkpoints the WAL into `main.db`. The pod's
  `terminationGracePeriodSeconds` is 45.
- One JSON line per request on stdout: `method`, `path`, `status`, `bytes`, `request_bytes`,
  `duration_ms`. The SQL, its parameters, the body, headers and the token are **never** logged.

## Build

The image is built from the repository root, for `linux/amd64` (the cluster's architecture):

```sh
docker buildx build --platform linux/amd64 -f build/lake-sqlite/Dockerfile \
  -t oramasearch/lake-sqlite:<tag> .
# or: make docker-build-lake-sqlite LAKE_SQLITE_IMG=oramasearch/lake-sqlite:<tag>
```

The builder is the same digest-pinned `golang:1.23` as the operator's own image; the build runs
with `CGO_ENABLED=0`, `-trimpath` and a stripped binary. The chart's default is
`lakeServices.sqliteImage: oramasearch/lake-sqlite:0.1.0`, so that tag must be published (private
Docker Hub, like the operator) before a SQLite service can start; a Lake overrides it with
`spec.services.sqliteImage`. Changing the image rolls every SQLite service's pod once.

## Run locally

```sh
go test ./cmd/lake-sqlite/...
mkdir -p /tmp/lake-sqlite
LAKE_SQLITE_TOKEN=dev LAKE_SQLITE_PATH=/tmp/lake-sqlite/main.db LAKE_SQLITE_ADDR=127.0.0.1:8080 \
  go run ./cmd/lake-sqlite

curl -s localhost:8080/healthz
curl -s -H 'Authorization: Bearer dev' -d '{"sql":"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"}' localhost:8080/v1/exec
curl -s -H 'Authorization: Bearer dev' \
  -d '{"statements":[{"sql":"INSERT INTO t (v) VALUES (?)","params":["a"]},{"sql":"SELECT * FROM t"}]}' \
  localhost:8080/v1/batch
```

The tests (`cmd/lake-sqlite/internal/server/server_test.go`, table-driven over an `httptest` server
and a temporary directory) cover the token (missing, wrong, prefix, suffix, other schemes, and
`/healthz` without one), every value type in both directions, request errors and their codes, the
16 MiB cap, exec results, WAL and foreign keys, batch commit, rollback on a SQL error and on a bad
parameter, the 10,000-row cap on `/v1/query` and in a batch, the request timeout, `/healthz` on an
open and a closed database, refusing to start without a token or a writable path, and that the
request log never carries the token, the SQL or a parameter.

## Known limits

- **One copy.** One pod on one `ReadWriteOnce` volume, no replication and no backup yet: a lost
  volume loses the database, and the service is down while its pod restarts or moves.
- A `sql` holding several statements separated by `;` runs all of them, each committing on its
  own, and `/v1/query` answers only the last one's rows. Send one statement per `sql`, and use
  `/v1/batch` for several that must commit together.
- No per-customer quotas inside the database beyond the volume size; a full volume answers
  `507 disk_full`.
