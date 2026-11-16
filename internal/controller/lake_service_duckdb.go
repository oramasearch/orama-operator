// SPDX-License-Identifier: MIT

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// DuckDB services (lake#195; the contract is lake's docs/control-plane/services.md, the duckdb
// row): lake-duckdb, powered by DuckDB (github.com/oramasearch/lake-duckdb, docs/lake/lake-duckdb.md),
// the same API as lake-sqlite over one DuckDB file on a storage_gi volume, with the engine locked
// down (no file or network access beyond its database, no extensions, lock_configuration). It runs
// through the wrapper machinery (lake_service_wrapper.go). DuckDB has one writer process, so
// instances is always 1 (validateService).

// DefaultLakeDuckDBImage is the lake-duckdb image when neither the Lake nor the operator's flag
// names one. The chart passes its own default (lakeServices.duckdbImage) as the flag.
const DefaultLakeDuckDBImage = "oramasearch/lake-duckdb:0.2.0"

// duckdbThreads bounds DuckDB's worker threads: the pod has no CPU limit, and DuckDB would
// otherwise start one thread per node CPU.
const duckdbThreads = "2"

var duckdbServiceKind = wrapperServiceKind{
	kind:      serviceKindDuckDB,
	titleText: "DuckDB",
	app:       "lake-duckdb",
	envPrefix: "LAKE_DUCKDB",
	leadEnv: []corev1.EnvVar{
		{Name: "LAKE_DUCKDB_PATH", Value: wrapperDataDir + "/main.duckdb"},
		{Name: "LAKE_DUCKDB_ADDR", Value: fmt.Sprintf(":%d", wrapperPort)},
	},
	trailEnv: []corev1.EnvVar{{Name: "LAKE_DUCKDB_THREADS", Value: duckdbThreads}},
	// lake-duckdb gives DuckDB half the limit and its Go heap a quarter.
	memRequest: "256Mi",
	memLimit:   "2Gi",
	database:   "main",
}
