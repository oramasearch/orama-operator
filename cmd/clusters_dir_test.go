// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
)

// clustersDir is the folder of cluster resources the tests read. By default it is the example in
// config/clusters. Set ORAMA_CLUSTERS_DIR to an absolute path to run the same tests against a real
// deployment's resources.
func clustersDir() string {
	if dir := os.Getenv("ORAMA_CLUSTERS_DIR"); dir != "" {
		return dir
	}
	return filepath.Join("..", "config", "clusters")
}
