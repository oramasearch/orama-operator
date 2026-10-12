/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// renderedImportSources parses the processor config's oramacores back into sources.
func renderedImportSources(t *testing.T, config string) []oramav1alpha1.AnalyticsImportSource {
	t.Helper()
	var cfg struct {
		Oramacores []struct {
			Name    string `json:"name"`
			APIURL  string `json:"api_url"`
			Enabled bool   `json:"enabled"`
		} `json:"oramacores"`
	}
	if err := yaml.Unmarshal([]byte(config), &cfg); err != nil {
		t.Fatalf("processor config is not YAML: %v\n%s", err, config)
	}
	sources := make([]oramav1alpha1.AnalyticsImportSource, 0, len(cfg.Oramacores))
	for _, c := range cfg.Oramacores {
		sources = append(sources, oramav1alpha1.AnalyticsImportSource{Name: c.Name, URL: c.APIURL, Enabled: c.Enabled})
	}
	return sources
}

func readerSource(ordinal int, enabled bool) oramav1alpha1.AnalyticsImportSource {
	return oramav1alpha1.AnalyticsImportSource{
		Name:    fmt.Sprintf("v1-reader-%d", ordinal),
		URL:     fmt.Sprintf("http://v1-reader-%d.v1-reader-headless.orama-system.svc:8080/v1/analytics", ordinal),
		Enabled: enabled,
	}
}

// Without retained PVCs the config lists ordinals 0..replicas-1 once each, and the writer only
// when includeWriter is set.
func TestAnalyticsImportSourcesListEachReaderOnce(t *testing.T) {
	writer := oramav1alpha1.AnalyticsImportSource{Name: "v1-writer", URL: "http://v1-writer:8080/v1/analytics", Enabled: true}
	for _, includeWriter := range []bool{false, true} {
		for replicas := 1; replicas <= 4; replicas++ {
			oc := scheduledImportsOC()
			oc.Spec.Components.AnalyticsProcessor.Import.IncludeWriter = includeWriter
			var want []oramav1alpha1.AnalyticsImportSource
			for ordinal := range replicas {
				want = append(want, readerSource(ordinal, true))
			}
			if includeWriter {
				want = append(want, writer)
			}
			got := renderedImportSources(t, analyticsProcessorConfigYAML(oc, analyticsImportSources(oc, int32(replicas), 0)))
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("replicas=%d includeWriter=%t:\n got %v\nwant %v", replicas, includeWriter, got, want)
			}
		}
	}
}

// A scale-down keeps the PVCs of the removed ordinals; those ordinals stay in the config, disabled,
// and status.analyticsImport.sources matches what the processor was given.
func TestAnalyticsImportRetainsScaledDownReaders(t *testing.T) {
	oc := scheduledImportsOC()
	oc.Spec.Components.Engine.Reader.Replicas = 3
	var claims []corev1.PersistentVolumeClaim
	for ordinal := range 5 {
		claims = append(claims, corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("data-v1-reader-%d", ordinal), Namespace: "orama-system"}})
	}
	// Neither another component's claim nor a CR whose name extends this one is a reader ordinal.
	claims = append(claims,
		corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-v1-writer-0", Namespace: "orama-system"}},
		corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-v1-reader-x-7", Namespace: "orama-system"}},
	)
	cl, r := reconcileOCV1(t, oc, &claims[0], &claims[1], &claims[2], &claims[3], &claims[4], &claims[5], &claims[6])
	reconcileAgain(t, cl, r, nil)

	config := getObj(t, cl, "v1-analytics-processor-config", &corev1.ConfigMap{}).Data["config.yaml"]
	got := renderedImportSources(t, config)
	want := []oramav1alpha1.AnalyticsImportSource{
		readerSource(0, true), readerSource(1, true), readerSource(2, true), readerSource(3, false), readerSource(4, false),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("processor sources:\n got %v\nwant %v", got, want)
	}

	status := getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.AnalyticsImport
	if status == nil || fmt.Sprint(status.Sources) != fmt.Sprint(got) {
		t.Fatalf("status.analyticsImport = %+v, want sources %v", status, got)
	}
	version := getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).ResourceVersion
	reconcileAgain(t, cl, r, nil)
	if getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).ResourceVersion != version {
		t.Error("steady analytics import status rewrote the CR status")
	}

	// The CronJob's last successful run is reported.
	schedule := getObj(t, cl, "v1-analytics-import", &batchv1.CronJob{})
	succeeded := metav1.NewTime(time.Date(2026, 10, 12, 9, 5, 0, 0, time.UTC))
	schedule.Status.LastSuccessfulTime = &succeeded
	if err := cl.Status().Update(context.Background(), schedule); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, nil)
	if last := getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.AnalyticsImport.LastSuccessfulTime; last == nil || !last.Equal(&succeeded) {
		t.Errorf("status.analyticsImport.lastSuccessfulTime = %v, want %v", last, succeeded)
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Import.Enabled = false })
	if getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.AnalyticsImport != nil {
		t.Error("disabling imports left status.analyticsImport")
	}
}

// The functional e2e suite runs against live stacks; GET /v1/analytics would erase a reader's
// pending customer events, which only the analytics processor may pull.
func TestE2EScriptDoesNotDrainAnalytics(t *testing.T) {
	if strings.Contains(e2eFunctionalScript, "/v1/analytics") {
		t.Fatal("e2e-functional.sh calls /v1/analytics, which erases a reader's pending analytics events")
	}
}
