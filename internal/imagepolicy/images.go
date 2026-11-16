// SPDX-License-Identifier: MIT

// Package imagepolicy checks the executable image references in rendered workloads.
package imagepolicy

import (
	_ "crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// Pinned requires a valid repository reference and an immutable SHA-256 digest.
func Pinned(image string) bool {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false
	}
	canonical, ok := ref.(reference.Canonical)
	return ok && canonical.Digest().Algorithm() == digest.SHA256 && canonical.Digest().Validate() == nil
}

// MutableImages returns sorted field paths, never environment or credential values. Check only
// executable specs: status and annotations are observed metadata, not image selection.
func MutableImages(object map[string]interface{}) []string {
	spec, _ := object["spec"].(map[string]interface{})
	var paths []string
	switch selectionKind(object) {
	case "pod":
		walk(spec, "spec", &paths)
	case "postgres":
		check(spec["image"], "spec.image", &paths)
	case "cnpg":
		check(spec["imageName"], "spec.imageName", &paths)
	case "list":
		items, _ := object["items"].([]interface{})
		for i, item := range items {
			obj, _ := item.(map[string]interface{})
			for _, path := range MutableImages(obj) {
				paths = append(paths, fmt.Sprintf("items[%d].%s", i, path))
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func check(value interface{}, path string, paths *[]string) {
	image, _ := value.(string)
	if !Pinned(image) {
		*paths = append(*paths, path)
	}
}

func walk(value interface{}, path string, paths *[]string) {
	switch value := value.(type) {
	case map[string]interface{}:
		for key, item := range value {
			next := path + "." + key
			switch key {
			case "image":
				check(item, next, paths)
			case "containers", "initContainers", "ephemeralContainers":
				containers, _ := item.([]interface{})
				for i, entry := range containers {
					container, _ := entry.(map[string]interface{})
					check(container["image"], fmt.Sprintf("%s[%d].image", next, i), paths)
					// A child worker image is executable too, even though the master passes it via env.
					walk(container["env"], fmt.Sprintf("%s[%d].env", next, i), paths)
				}
			default:
				walk(item, next, paths)
			}
		}
		if value["name"] == "WORKER_IMAGE" {
			check(value["value"], path+".value", paths)
		}
	case []interface{}:
		for i, item := range value {
			walk(item, fmt.Sprintf("%s[%d]", path, i), paths)
		}
	}
}

// WorkloadCount lets release gates reject empty input or mistakenly unrendered configuration.
func WorkloadCount(object map[string]interface{}) int {
	kind := selectionKind(object)
	if kind == "" {
		return 0
	}
	if kind != "list" {
		return 1
	}
	items, _ := object["items"].([]interface{})
	total := 0
	for _, item := range items {
		child, _ := item.(map[string]interface{})
		total += WorkloadCount(child)
	}
	return total
}

func selectionKind(object map[string]interface{}) string {
	kind, _ := object["kind"].(string)
	switch kind {
	case "Pod", "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob", "ReplicationController":
		return "pod"
	case "PostgresSchema":
		return "postgres"
	case "Cluster":
		version, _ := object["apiVersion"].(string)
		if strings.HasPrefix(version, "postgresql.cnpg.io/") {
			return "cnpg"
		}
	case "List":
		return "list"
	}
	return ""
}
