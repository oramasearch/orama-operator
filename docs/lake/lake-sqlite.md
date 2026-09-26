# lake-sqlite

`lake-sqlite`, the web service behind lake's `sqlite` service kind, lives in its own private
repository: **[oramasearch/lake-sqlite](https://github.com/oramasearch/lake-sqlite)** (API in its
`docs/api.md`), with its own private image `oramasearch/lake-sqlite:<tag>`. It moved out of this
repository on 2026-09-26 under the umbrella rule that every service is its own private repo with its
own private image.

This operator only runs it: `internal/controller/lake_service_sqlite.go` writes one StatefulSet,
Service and credential Secret per SQLite service ([`services.md`](services.md)). The image is
`spec.services.sqliteImage` on the Lake, else `--lake-sqlite-image`, else the chart's
`lakeServices.sqliteImage` (currently `oramasearch/lake-sqlite:0.1.1`). To release a new version,
build and push it from the lake-sqlite repository, then bump that default here.
