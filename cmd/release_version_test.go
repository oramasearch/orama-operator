/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReleaseVersionIsDocumented reads the release version from chart/Chart.yaml, the one place it
// is written, and checks that the version has its release notes, that the release index and the
// README name it, and that the chart's appVersion agrees. A release that moves the version and
// misses one of these fails here instead of shipping.
func TestReleaseVersionIsDocumented(t *testing.T) {
	chart, err := os.ReadFile(filepath.Join("..", "chart", "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	field := func(name string) string {
		m := regexp.MustCompile(`(?m)^` + name + `:\s*"?([^"\s]+)"?\s*$`).FindSubmatch(chart)
		if m == nil {
			t.Fatalf("chart/Chart.yaml has no %s", name)
		}
		return string(m[1])
	}
	version, appVersion := field("version"), field("appVersion")
	if version != appVersion {
		t.Errorf("chart/Chart.yaml version %s and appVersion %s differ", version, appVersion)
	}
	tag := "v" + version

	notes := filepath.Join("..", "docs", "releases", tag+".md")
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("no release notes for %s: %v", tag, err)
	}
	for _, page := range []string{
		filepath.Join("..", "docs", "releases", "README.md"),
		filepath.Join("..", "README.md"),
	} {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), tag+".md") {
			t.Errorf("%s does not link the current release's notes, %s.md", page, tag)
		}
	}
}
