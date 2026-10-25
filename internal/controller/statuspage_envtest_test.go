/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"os"
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

const lakeLiveStatusFile = "../../config/clusters/status/lake-live.yaml"

func lakeLiveStatusDocuments(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(lakeLiveStatusFile)
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

// The committed lake.live page decodes strictly (no misspelt field silently dropped), runs every
// check the umbrella's gate asks of a production Lake in both regions, and publishes to the bucket
// the same file creates.
func TestCommittedLakeLiveStatusPage(t *testing.T) {
	docs := lakeLiveStatusDocuments(t)
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
	if !strings.HasSuffix(bucket.Spec.CustomDomain.Domain, ".lake.live") {
		t.Fatal("lake.live's page is a production host, under lake.live")
	}
	urls := map[string]bool{}
	for _, component := range page.Spec.Components {
		for _, check := range component.Checks {
			urls[strings.SplitN(check.URL, "?", 2)[0]] = true
			if strings.Contains(check.URL, "oramadev.net") {
				t.Fatalf("%s/%s: a nonprod host on the production page", component.Name, check.Name)
			}
		}
	}
	for _, region := range []string{"eu", "us"} {
		for _, path := range []string{"/readyz", "/v1/server_info", "/v1/workspace_engine_info", "/v1/receive_agent_tasks", "/", "/sign-in"} {
			if !urls["https://"+region+".lake.live"+path] {
				t.Errorf("no check of https://%s.lake.live%s", region, path)
			}
		}
	}
	if !urls["https://lake.live/"] {
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
	for _, doc := range lakeLiveStatusDocuments(t) {
		var object unstructured.Unstructured
		if err := yaml.Unmarshal(doc, &object.Object); err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(ctx, &object); err != nil {
			t.Fatalf("the committed %s was refused: %v", object.GetKind(), err)
		}
	}
	var page oramav1alpha1.StatusPage
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "orama-system", Name: "lake-live"}, &page); err != nil {
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
