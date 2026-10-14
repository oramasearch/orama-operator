/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNamespaceReadsAreUncached reads every controller source file. The operator may get
// Namespaces but never list or watch them (operator#106), so a Namespace read through the
// manager's cached client starts an informer that can never sync, and the manager exits on its
// cache-sync timeout (dev-0e9f4a4 and dev-469b078 on 2026-10-14). The envtest suites run as
// cluster-admin and cannot see that, so this test holds the rule: every Get of a variable declared
// as corev1.Namespace goes through an uncached reader, and nothing lists Namespaces.
func TestNamespaceReadsAreUncached(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`var (\w+) corev1\.Namespace\b`)
	listed := regexp.MustCompile(`corev1\.NamespaceList\b`)
	uncached := regexp.MustCompile(`(reader\(\)|apiReader\(\)|APIReader|\breader)\.Get\(`)
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		source := string(data)
		if listed.MatchString(source) {
			t.Errorf("%s lists Namespaces; the operator may only get them", file)
		}
		lines := strings.Split(source, "\n")
		for i, line := range lines {
			match := declared.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			target := "&" + match[1] + ")"
			for j := i + 1; j < len(lines) && j < i+12; j++ {
				if !strings.Contains(lines[j], ".Get(") || !strings.Contains(lines[j], target) {
					continue
				}
				checked++
				if !uncached.MatchString(lines[j]) {
					t.Errorf("%s:%d reads Namespace %s through a client that may be cached; use the uncached APIReader:\n%s",
						file, j+1, match[1], strings.TrimSpace(lines[j]))
				}
				break
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no Namespace reads; the test no longer recognises the code it guards")
	}
}
