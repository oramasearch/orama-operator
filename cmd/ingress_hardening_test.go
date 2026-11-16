/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"path/filepath"
	"testing"
)

// Both installation paths explicitly disable the admission webhook and per-Ingress
// snippets while ingress-nginx remains required; chart defaults must not decide either policy.
func TestIngressDisablesAdmissionWebhooksAndSnippetAnnotations(t *testing.T) {
	check := func(where string, releases map[string]platformRelease) {
		t.Run(where, func(t *testing.T) {
			release, ok := releases["ingress-nginx"]
			if !ok {
				t.Fatal("ingress-nginx release missing; review this guard when controller replacement is complete")
			}
			controller, _ := release.Values["controller"].(map[string]any)
			webhooks, _ := controller["admissionWebhooks"].(map[string]any)
			if enabled, ok := webhooks["enabled"].(bool); !ok || enabled {
				t.Error("controller.admissionWebhooks.enabled must be explicitly false")
			}
			extraArgs, _ := controller["extraArgs"].(map[string]any)
			if _, exists := extraArgs["validating-webhook"]; exists {
				t.Error("controller.extraArgs must not restore the validating-webhook listener")
			}
			if enabled, ok := controller["allowSnippetAnnotations"].(bool); !ok || enabled {
				t.Error("controller.allowSnippetAnnotations must be explicitly false")
			}
			// Extra controller.config entries can override the ConfigMap value emitted by the chart.
			config, _ := controller["config"].(map[string]any)
			if value, exists := config["allow-snippet-annotations"]; exists && value != "false" {
				t.Error("controller.config.allow-snippet-annotations must not enable snippets")
			}
		})
	}
	check("helmfile.yaml", helmfilePlatform(t))
	paths, err := filepath.Glob("../config/clusters/*/platform.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no leaf platform files (%v)", err)
	}
	for _, path := range paths {
		check(path, leafPlatform(t, path))
	}
}
