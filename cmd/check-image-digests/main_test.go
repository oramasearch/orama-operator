package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReleaseCheckRejectsMutableRenderedWorkloads(t *testing.T) {
	digest := "image@sha256:" + strings.Repeat("a", 64)
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod", "PostgresSchema"} {
		t.Run(kind, func(t *testing.T) {
			for _, image := range []string{digest, "image:tag"} {
				var manifest string
				if kind == "PostgresSchema" {
					manifest = "kind: PostgresSchema\nspec:\n  image: " + image + "\n"
				} else {
					manifest = "kind: " + kind + "\nspec:\n  template:\n    spec:\n      containers:\n      - name: credential-consumer\n        image: " + image + "\n"
				}
				var output bytes.Buffer
				err := check(strings.NewReader(manifest), &output)
				if (err == nil) != (image == digest) {
					t.Fatalf("image %s: err=%v, output=%s", image, err, output.String())
				}
			}
		})
	}
	// Missing CNPG imageName delegates image selection to mutable external defaults.
	if err := check(strings.NewReader("apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nspec: {}\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted implicit CNPG image")
	}
	if err := check(strings.NewReader("kind: Job\nspec: [invalid"), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted invalid YAML")
	}
}

func TestReleaseCheckRejectsEmptyOrUnrenderedInput(t *testing.T) {
	for _, input := range []string{"", "---\n", "kind: ConfigMap\ndata: {}\n", "kind: OramaCloudV1Set\nspec: {}\n", "kind: List\nitems: []\n"} {
		if err := check(strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Errorf("accepted input without executable workloads: %q", input)
		}
	}
}
