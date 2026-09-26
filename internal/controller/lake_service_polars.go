/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Polars services (lake#195; the contract is lake's docs/control-plane/services.md, the polars row):
// lake-polars, powered by Polars (github.com/oramasearch/lake-polars, docs/lake/lake-polars.md),
// analytics compute over Parquet datasets. A StatefulSet of spec.instances workers (1–3) sharing
// nothing, each with its own storage_gi volume — a StatefulSet so a volume stays with its pod —
// behind one ClusterIP Service. Datasets are per worker until shared storage exists (lake#196), so 1
// is the useful value. It runs through the wrapper machinery (lake_service_wrapper.go). The
// commercial Polars On-Prem product and its operator are not used.

// DefaultLakePolarsImage is the lake-polars image when neither the Lake nor the operator's flag
// names one. The chart passes its own default (lakeServices.polarsImage) as the flag.
const DefaultLakePolarsImage = "oramasearch/lake-polars:0.1.0"

// polarsThreads bounds Polars' thread pool: the pod has no CPU limit.
const polarsThreads = "2"

var polarsServiceKind = wrapperServiceKind{
	kind:      serviceKindPolars,
	titleText: "Polars",
	app:       "lake-polars",
	envPrefix: "LAKE_POLARS",
	leadEnv: []corev1.EnvVar{
		{Name: "LAKE_POLARS_DIR", Value: wrapperDataDir},
		{Name: "LAKE_POLARS_ADDR", Value: fmt.Sprintf("0.0.0.0:%d", wrapperPort)},
	},
	trailEnv: []corev1.EnvVar{
		{Name: "LAKE_POLARS_THREADS", Value: polarsThreads},
		// Polars spills here; the root filesystem is read-only.
		{Name: "TMPDIR", Value: wrapperTempDir},
	},
	// lake-polars caps an upload at a quarter of the limit (512 MiB here; its own default cap is
	// 256 MiB) and runs two requests at once.
	memRequest:    "256Mi",
	memLimit:      "2Gi",
	multiInstance: true,
}
