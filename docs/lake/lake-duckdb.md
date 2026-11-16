# lake-duckdb

`lake-duckdb`, **powered by DuckDB**, the web service behind lake's `duckdb` service kind (lake#195),
lives in its own private repository: **[oramasearch/lake-duckdb](https://github.com/oramasearch/lake-duckdb)**
(the whole API, the engine lockdown and its tests in its `docs/api.md`), with its own private image
`oramasearch/lake-duckdb:<tag>`, under the umbrella rule that every service is its own private repo
with its own private image. It serves one DuckDB database with lake-sqlite's API (`/v1/query`,
`/v1/exec`, `/v1/batch`, `/healthz`, a bearer token), in Go with `github.com/duckdb/duckdb-go/v2`
(MIT; DuckDB 1.5.5, MIT). It is never called "DuckDB Cloud".

This operator only runs it: `internal/controller/lake_service_wrapper.go`, with the DuckDB values in
`lake_service_duckdb.go`, writes one StatefulSet of one pod, a Service and a credential Secret per
DuckDB service ([`services.md`](services.md#duckdb-and-polars-lake195)). The image is
`spec.services.duckdbImage` on the Lake, else `--lake-duckdb-image`, else the chart's
`lakeServices.duckdbImage` (currently `oramasearch/lake-duckdb:0.2.0`). To release a new version,
build it on an amd64 builder from the lake-duckdb repository (cgo), push it to the private Docker Hub
repository (created private before the first push), then bump that default here.

From [v0.2.0](https://github.com/oramasearch/lake-duckdb/releases/tag/v0.2.0) each `sql` must hold
one statement (`400 invalid_request` otherwise), and `COPY`, `EXPORT DATABASE`, `DETACH`, `query(...)`
calls and the functions and pragmas that change engine state past `lock_configuration` (logging,
profiling, verification and similar) answer `403 forbidden`. Answers stop at 32 MiB of values, and
every request gets a fresh connection. Rolling it changes what existing customers' SQL may do.
