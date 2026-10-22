/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package platformcharts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// helmfile's path installs only what was staged with the pinned bytes: a missing file is
// downloaded and checked, a staged one is checked again on every run, and one that is not the
// pinned bytes is never left for helmfile.
func TestStagingKeepsOnlyThePinnedBytes(t *testing.T) {
	saved, err := chartutil.Save(&chart.Chart{Metadata: &chart.Metadata{APIVersion: chart.APIVersionV2, Name: "op", Version: "1.0.0"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte("kind: CustomResourceDefinition\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			_, _ = w.Write([]byte("apiVersion: v1\nentries:\n  op:\n  - apiVersion: v2\n    name: op\n    version: 1.0.0\n    urls: [op-1.0.0.tgz]\n"))
		case "/op-1.0.0.tgz":
			_, _ = w.Write(archive)
		case "/crd.yaml":
			_, _ = w.Write(manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pins := &Pins{Charts: []Chart{{Name: "op", Repository: server.URL, Version: "1.0.0", SHA256: digestOf(archive),
		CRDs: []CRD{{URL: server.URL + "/crd.yaml", SHA256: digestOf(manifest)}}}}}
	dir := t.TempDir()
	var lines []string
	report := func(line string) { lines = append(lines, line) }
	ctx := context.Background()

	if err := Stage(ctx, pins, dir, true, report); err == nil || !strings.Contains(err.Error(), "is not staged") {
		t.Fatalf("offline, with nothing staged: %v", err)
	}
	if err := Stage(ctx, pins, dir, false, report); err != nil {
		t.Fatal(err)
	}
	if err := Stage(ctx, pins, dir, true, report); err != nil {
		t.Fatalf("offline, after staging: %v", err)
	}
	if got := strings.Join(lines, "\n"); strings.Count(got, "downloaded") != 2 || strings.Count(got, "are the pinned ones") != 4 {
		t.Fatalf("report:\n%s", got)
	}

	// A staged archive that is not the pinned bytes: refused offline, replaced online.
	staged := filepath.Join(dir, "op-1.0.0.tgz")
	if err := os.WriteFile(staged, []byte("not the chart"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Stage(ctx, pins, dir, true, report); err == nil || !strings.Contains(err.Error(), "not the artifact that was pinned") {
		t.Fatalf("offline, with other bytes staged: %v", err)
	}
	if err := Stage(ctx, pins, dir, false, report); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(staged); digestOf(data) != digestOf(archive) {
		t.Fatal("the staged archive was not replaced by the pinned bytes")
	}

	// A pin the upstream bytes do not match: nothing is written for helmfile to find.
	pins.Charts[0].CRDs[0].SHA256 = "sha256:" + strings.Repeat("0", 64)
	if err := os.Remove(filepath.Join(dir, "op-1.0.0-crd.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := Stage(ctx, pins, dir, false, report); err == nil {
		t.Fatal("a manifest that is not the pinned bytes was staged")
	}
	if _, err := os.Stat(filepath.Join(dir, "op-1.0.0-crd.yaml")); !os.IsNotExist(err) {
		t.Fatalf("the refused manifest is on disk: %v", err)
	}
}

func TestAPinsFileWithoutAPinIsRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pins.yaml")
	if err := os.WriteFile(file, []byte("charts:\n  - name: op\n    repository: https://example.invalid\n    version: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil || !strings.Contains(err.Error(), "has no sha256 pin") {
		t.Fatalf("got %v", err)
	}
}
