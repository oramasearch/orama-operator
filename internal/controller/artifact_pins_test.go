/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
)

func sha256Of(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// testChartArchive packages a chart that says it is version, with description making its bytes
// differ from another archive of the same version.
func testChartArchive(t *testing.T, version, description string) []byte {
	t.Helper()
	path, err := chartutil.Save(&chart.Chart{Metadata: &chart.Metadata{
		APIVersion: chart.APIVersionV2, Name: "pinned", Version: version, Description: description,
	}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// chartRepository serves one chart, pinned-1.0.0, whose archive the test can replace: a publisher
// moving a version to other bytes.
type chartRepository struct {
	mu      sync.Mutex
	archive []byte
}

func (r *chartRepository) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req.URL.Path {
	case "/index.yaml":
		_, _ = w.Write([]byte("apiVersion: v1\nentries:\n  pinned:\n  - apiVersion: v2\n    name: pinned\n    version: 1.0.0\n    urls: [pinned-1.0.0.tgz]\n"))
	case "/pinned-1.0.0.tgz":
		_, _ = w.Write(r.archive)
	default:
		http.NotFound(w, req)
	}
}

// The issue's check (orama-operator#808): a chart whose bytes are not the pinned ones is refused
// before it is loaded, whatever version it is published as, and a refusal does not poison the
// installer's cache for the right pin.
func TestAChartIsInstalledOnlyFromThePinnedBytes(t *testing.T) {
	original := testChartArchive(t, "1.0.0", "the release")
	repo := &chartRepository{archive: original}
	server := httptest.NewServer(http.HandlerFunc(repo.serve))
	defer server.Close()
	source := chartSource{Repository: server.URL, Name: "pinned", Version: "1.0.0", SHA256: sha256Of(original)}

	h := newHelmLeafInstaller()
	if loaded, err := h.fetchChart(source); err != nil || loaded.Metadata.Version != "1.0.0" {
		t.Fatalf("the pinned bytes: %v", err)
	}

	wrong := source
	wrong.SHA256 = "sha256:" + strings.Repeat("0", 64)
	if _, err := h.fetchChart(wrong); err == nil || !strings.Contains(err.Error(), "it is not the artifact that was pinned") {
		t.Fatalf("a wrong pin must be refused, even with the right bytes cached under another: %v", err)
	}

	// The publisher moves 1.0.0 to other bytes. A cluster reconciled by a new process, where
	// nothing is cached, is refused them.
	repo.mu.Lock()
	repo.archive = testChartArchive(t, "1.0.0", "a replacement")
	repo.mu.Unlock()
	if _, err := newHelmLeafInstaller().fetchChart(source); err == nil || !strings.Contains(err.Error(), "is refused") {
		t.Fatalf("a republished version must be refused: %v", err)
	}
	if _, err := DownloadPinnedChart(server.URL, "pinned", "1.0.0", source.SHA256); err == nil {
		t.Fatal("helmfile's staging must refuse the republished bytes too")
	}

	// Pinned bytes that are another version are refused as well.
	other := testChartArchive(t, "1.0.1", "the release")
	if err := CheckPinnedChart("pinned-1.0.0.tgz", "1.0.0", sha256Of(other), other); err == nil || !strings.Contains(err.Error(), "says it is version 1.0.1") {
		t.Fatalf("a chart that says it is another version: %v", err)
	}

	if _, err := h.fetchChart(chartSource{Repository: server.URL, Name: "pinned", Version: "1.0.0"}); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("a chart without a pin must not be downloaded: %v", err)
	}
}

// A release without pins is refused before Helm reads the cluster, so one already installed is
// not kept as Ready either.
func TestAnUnpinnedReleaseIsRefusedBeforeTheClusterIsRead(t *testing.T) {
	h := newHelmLeafInstaller()
	pinned := chartSource{Repository: "https://example.invalid", Name: "x", Version: "1.0.0", SHA256: "sha256:" + strings.Repeat("a", 64)}
	for name, req := range map[string]helmReleaseRequest{
		"no chart pin": {Release: "x", Namespace: "x", Chart: chartSource{Repository: "https://example.invalid", Name: "x", Version: "1.0.0"}},
		"no CRD pin":   {Release: "x", Namespace: "x", Chart: pinned, CRDURLs: []string{"https://example.invalid/crd.yaml"}},
	} {
		// No cluster at all: reaching for one would panic.
		if _, err := h.ensureRelease(context.Background(), nil, nil, req, false); err == nil || !strings.Contains(err.Error(), "is required") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A CRD manifest is applied only when its bytes are the pinned ones.
func TestACRDManifestIsAppliedOnlyFromThePinnedBytes(t *testing.T) {
	manifest := []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(manifest) }))
	defer server.Close()

	if data, err := DownloadPinnedManifest(context.Background(), server.URL, sha256Of(manifest)); err != nil || string(data) != string(manifest) {
		t.Fatalf("the pinned manifest: %v", err)
	}
	// The client is never reached: the manifest is refused first.
	err := applyCRDManifestURL(context.Background(), nil, server.URL, "sha256:"+strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "it is not the artifact that was pinned") {
		t.Fatalf("a manifest with other bytes must be refused: %v", err)
	}
	if _, err := DownloadPinnedManifest(context.Background(), server.URL, ""); err == nil {
		t.Fatal("a manifest without a pin must not be downloaded")
	}
}

// The pin a release records fits a Kubernetes label value, whole.
func TestTheChartPinFitsAReleaseLabel(t *testing.T) {
	labelValue := regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)
	a, b := chartPinLabelValue("sha256:"+strings.Repeat("a", 64)), chartPinLabelValue("sha256:"+strings.Repeat("a", 63)+"b")
	if len(a) > 63 || !labelValue.MatchString(a) || a == b {
		t.Fatalf("label values %q and %q", a, b)
	}
}
