// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The operator's memory (operator#677). It was OOMKilled 20 seconds after starting on the managing
// cluster under the chart's 256Mi limit. Two things made the start-up peak, and both are bounded
// here, the same way on every cluster (principle 11):
//
//   - The cache listed every Secret in the cluster, and Helm keeps every revision of every release
//     as a Secret (label owner=helm): on the managing cluster 57 of 224 Secrets held 96% of the
//     bytes. Syncing them retained 18 MiB and allocated 48 MiB; without them, 2 MiB and 4 MiB. The
//     operator never reads a Helm release Secret through its cache: Helm reads its own, on the leaf,
//     through its own client.
//   - The Go runtime did not know the container's limit. With GOGC=100 the heap may grow to twice
//     what is live before a collection, so the concurrent Helm history reads at start-up (up to
//     eight ClusterHelmReleases at once, 13-43 MiB allocated each) took it past the limit. The
//     chart gives the container's memory limit as OPERATOR_MEMORY_LIMIT and the runtime collects
//     harder as the heap nears memoryLimitFraction of it (principle 8: the binary respects the cap).

// memoryLimitEnv carries the container's memory limit in bytes, from the chart's resourceFieldRef.
const memoryLimitEnv = "OPERATOR_MEMORY_LIMIT"

// memoryLimitFraction of the container limit is the Go runtime's soft limit. The rest is for what
// the runtime does not count against it, or cannot return at once: goroutine stacks, C memory,
// and fragmentation.
const memoryLimitFraction = 0.85

// goMemoryLimit is the soft limit to give the Go runtime, from the environment: none when GOMEMLIMIT
// is set (the runtime has already read it) or no container limit is given (a local run).
func goMemoryLimit(getenv func(string) string) (int64, error) {
	if getenv("GOMEMLIMIT") != "" {
		return 0, nil
	}
	raw := getenv(memoryLimitEnv)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("%s=%q is not a positive byte count", memoryLimitEnv, raw)
	}
	return int64(float64(limit) * memoryLimitFraction), nil
}

// applyGoMemoryLimit gives the Go runtime its soft limit and returns it (0 when none was set).
func applyGoMemoryLimit() (int64, error) {
	limit, err := goMemoryLimit(os.Getenv)
	if err != nil || limit == 0 {
		return 0, err
	}
	debug.SetMemoryLimit(limit)
	return limit, nil
}

// helmReleaseOwnerLabel and helmReleaseOwner are the label Helm's Secret storage driver puts on every release revision.
const helmReleaseOwnerLabel, helmReleaseOwner = "owner", "helm"

// managerCacheOptions is the manager's cache, the same on every cluster: every Secret except Helm's
// release revisions, and no object's managedFields, which no controller reads. watchNamespace,
// when set, confines the cache to one namespace (a local run against a shared cluster).
func managerCacheOptions(watchNamespace string) cache.Options {
	notHelm, err := labels.NewRequirement(helmReleaseOwnerLabel, selection.NotEquals, []string{helmReleaseOwner})
	if err != nil {
		panic(err) // a constant requirement; it cannot fail
	}
	options := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Label: labels.NewSelector().Add(*notHelm)},
		},
		DefaultTransform: cache.TransformStripManagedFields(),
	}
	if watchNamespace != "" {
		options.DefaultNamespaces = map[string]cache.Config{watchNamespace: {}}
	}
	return options
}
