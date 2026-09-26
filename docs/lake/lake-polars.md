# lake-polars

`lake-polars`, **powered by Polars**, the web service behind lake's `polars` service kind (lake#195),
lives in its own private repository: **[oramasearch/lake-polars](https://github.com/oramasearch/lake-polars)**
(the whole API in its `docs/api.md`), with its own private image `oramasearch/lake-polars:<tag>`,
under the umbrella rule that every service is its own private repo with its own private image. It is
analytics compute in Rust with the open-source `polars` crate (MIT): `POST /v1/datasets/<name>`
uploads CSV, JSONL or Parquet (stored as Parquet), `GET /v1/datasets` lists them, `DELETE
/v1/datasets/<name>` removes one, and `POST /v1/query` `{sql}` runs Polars SQL over them, behind a
bearer token. The commercial Polars On-Prem product and its operator are not used.

This operator only runs it: `internal/controller/lake_service_wrapper.go`, with the Polars values in
`lake_service_polars.go`, writes one StatefulSet of `instances` workers (each with its own volume), a
Service and a credential Secret per Polars service ([`services.md`](services.md#duckdb-and-polars-lake195)).
Datasets are per worker until shared storage exists (lake#196). The image is
`spec.services.polarsImage` on the Lake, else `--lake-polars-image`, else the chart's
`lakeServices.polarsImage` (currently `oramasearch/lake-polars:0.1.0`). To release a new version,
build it from the lake-polars repository for `linux/amd64`, push it to the private Docker Hub
repository (created private before the first push), then bump that default here.
