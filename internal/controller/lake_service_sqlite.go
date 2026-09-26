/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// SQLite services (liquid-bridge decision 47, amended; the contract is lake's
// docs/control-plane/services.md, the sqlite row): lake-sqlite (github.com/oramasearch/lake-sqlite,
// docs/lake/lake-sqlite.md), one pod over one database on a storage_gi volume, run through the
// wrapper machinery (lake_service_wrapper.go). SQLite has one writer, so instances is always 1
// (validateService).

// DefaultLakeSQLiteImage is the lake-sqlite image when neither the Lake nor the operator's flag
// names one. The chart passes its own default (lakeServices.sqliteImage) as the flag.
const DefaultLakeSQLiteImage = "oramasearch/lake-sqlite:0.1.1"

var sqliteServiceKind = wrapperServiceKind{
	kind:      serviceKindSQLite,
	titleText: "SQLite",
	app:       "lake-sqlite",
	envPrefix: "LAKE_SQLITE",
	leadEnv: []corev1.EnvVar{
		{Name: "LAKE_SQLITE_PATH", Value: wrapperDataDir + "/main.db"},
		{Name: "LAKE_SQLITE_ADDR", Value: fmt.Sprintf(":%d", wrapperPort)},
	},
	// SQLite spills large sorts and temporary tables here; the root filesystem is read-only.
	trailEnv:   []corev1.EnvVar{{Name: "SQLITE_TMPDIR", Value: wrapperTempDir}},
	memRequest: "64Mi",
	memLimit:   "512Mi",
	database:   "main",
}
