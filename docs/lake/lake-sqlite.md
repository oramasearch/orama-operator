# lake-sqlite

`lake-sqlite`, the web service behind lake's `sqlite` service kind, lives in its own private
repository: **[oramasearch/lake-sqlite](https://github.com/oramasearch/lake-sqlite)** (API in its
`docs/api.md`), with its own private image `oramasearch/lake-sqlite:<tag>`. It moved out of this
repository on 2026-09-26 under the umbrella rule that every service is its own private repo with its
own private image.

This operator only runs it: `internal/controller/lake_service_wrapper.go`, with the SQLite values in
`lake_service_sqlite.go`, writes one StatefulSet,
Service and credential Secret per SQLite service ([`services.md`](services.md)). The image is
`spec.services.sqliteImage` on the Lake, else `--lake-sqlite-image`, else the chart's
`lakeServices.sqliteImage` (currently `oramasearch/lake-sqlite:0.2.0`). To release a new version,
build and push it from the lake-sqlite repository, then bump that default here.

From [v0.2.0](https://github.com/oramasearch/lake-sqlite/releases/tag/v0.2.0) the image runs `FROM
scratch`, and a statement guard refuses `ATTACH`, `DETACH`, `VACUUM`, transaction control outside
`/v1/batch`, and pragmas that change the connection (`403 not_allowed`). It also caps values at
8 MiB, a response at 32 MiB and a batch at 1,000 statements. Rolling it changes what existing
customers' SQL may do.
