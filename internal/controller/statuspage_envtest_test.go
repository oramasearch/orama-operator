// SPDX-License-Identifier: MIT

package controller

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// statusPageDocuments reads the one status page file of the cluster resources: the bucket the
// page is served from, then the page.
func statusPageDocuments(t *testing.T) [][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(clustersDir(), "status", "*.yaml"))
	if err != nil || len(files) != 1 {
		t.Fatalf("want one status page file in %s/status, got %v (%v)", clustersDir(), files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var docs [][]byte
	for _, doc := range bytes.Split(raw, []byte("\n---\n")) {
		if len(bytes.TrimSpace(doc)) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

// The committed status page decodes strictly (no misspelt field silently dropped), runs every check
// a production gate asks of a Lake in both regions, checks only hosts of its own deployment, and
// publishes to the bucket the same file creates.
func TestCommittedStatusPage(t *testing.T) {
	docs := statusPageDocuments(t)
	if len(docs) != 2 {
		t.Fatalf("%d documents, want the bucket and the page", len(docs))
	}
	var bucket oramav1alpha1.CloudflareR2Bucket
	if err := yaml.UnmarshalStrict(docs[0], &bucket); err != nil {
		t.Fatal(err)
	}
	var page oramav1alpha1.StatusPage
	if err := yaml.UnmarshalStrict(docs[1], &page); err != nil {
		t.Fatal(err)
	}
	if bucket.Spec.Credentials == nil || page.Spec.Publish == nil || page.Spec.Publish.BucketSecretRef != bucket.Spec.Credentials.SecretName ||
		bucket.Namespace != page.Namespace || bucket.Spec.CustomDomain == nil || "https://"+bucket.Spec.CustomDomain.Domain != page.Spec.Publish.PublicURL {
		t.Fatalf("the page must publish to the bucket this file creates, on its public host: %+v %+v", bucket.Spec, page.Spec.Publish)
	}
	// The page is served on status.<apex>, and every check is of a host under that apex: a page for
	// production never checks a nonprod host.
	apex, ok := strings.CutPrefix(bucket.Spec.CustomDomain.Domain, "status.")
	if !ok {
		t.Fatalf("the page is served on status.<apex>, not %s", bucket.Spec.CustomDomain.Domain)
	}
	urls := map[string]bool{}
	for _, component := range page.Spec.Components {
		for _, check := range component.Checks {
			urls[strings.SplitN(check.URL, "?", 2)[0]] = true
			host := strings.SplitN(strings.TrimPrefix(check.URL, "https://"), "/", 2)[0]
			if host != apex && !strings.HasSuffix(host, "."+apex) {
				t.Fatalf("%s/%s checks %s, outside the page's deployment %s", component.Name, check.Name, host, apex)
			}
		}
	}
	for _, region := range []string{"eu", "us"} {
		for _, path := range []string{"/readyz", "/v1/server_info", "/v1/workspace_engine_info", "/v1/receive_agent_tasks", "/", "/sign-in"} {
			if !urls["https://"+region+"."+apex+path] {
				t.Errorf("no check of https://%s.%s%s", region, apex, path)
			}
		}
	}
	if !urls["https://"+apex+"/"] {
		t.Error("no check of the apex")
	}
}

// The CRD admits the committed file and refuses what it must.
func TestStatusPageCRDAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for status page admission envtest")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths: []string{
			"../../config/crd/bases/orama.cloud_cloudflarer2buckets.yaml",
			"../../config/crd/bases/orama.cloud_statuspages.yaml",
		},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	kube, err := client.New(config, client.Options{Scheme: backupTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}}); err != nil {
		t.Fatal(err)
	}
	docs := statusPageDocuments(t)
	var committed oramav1alpha1.StatusPage
	if err := yaml.Unmarshal(docs[1], &committed); err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		var object unstructured.Unstructured
		if err := yaml.Unmarshal(doc, &object.Object); err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(ctx, &object); err != nil {
			t.Fatalf("the committed %s was refused: %v", object.GetKind(), err)
		}
	}
	var page oramav1alpha1.StatusPage
	if err := kube.Get(ctx, client.ObjectKey{Namespace: committed.Namespace, Name: committed.Name}, &page); err != nil {
		t.Fatal(err)
	}
	if page.Spec.Components[0].Checks[0].Method != "GET" || page.Spec.Components[0].Checks[0].Expect.Status != 200 {
		t.Fatalf("defaults not applied: %+v", page.Spec.Components[0].Checks[0])
	}

	bad := []struct {
		name   string
		mutate func(*oramav1alpha1.StatusPage)
		want   string
	}{
		{"bearer without secret", func(p *oramav1alpha1.StatusPage) {
			p.Spec.Components[0].Checks[0].BearerTokenKey = "token"
		}, "bearerTokenKey needs secretRef"},
		{"interval too short", func(p *oramav1alpha1.StatusPage) { p.Spec.IntervalSeconds = 5 }, "intervalSeconds"},
		{"not a URL", func(p *oramav1alpha1.StatusPage) { p.Spec.Components[0].Checks[0].URL = "eu.lake.live/readyz" }, "url"},
		{"no components", func(p *oramav1alpha1.StatusPage) { p.Spec.Components = nil }, "components"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			p := &oramav1alpha1.StatusPage{
				ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "orama-system"},
				Spec: oramav1alpha1.StatusPageSpec{Title: "t", Components: []oramav1alpha1.StatusPageComponent{{
					Name: "a", Title: "A", Checks: []oramav1alpha1.StatusPageCheck{{Name: "c", URL: "https://example.com/"}},
				}}},
			}
			tc.mutate(p)
			if err := kube.Create(ctx, p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("admitted or wrong refusal: %v", err)
			}
		})
	}
}
