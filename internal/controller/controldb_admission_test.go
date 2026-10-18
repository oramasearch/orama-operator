/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Use the API server's CEL evaluator; fake clients cannot prove admission behavior. These
// dry-runs complement the gated rollout proof on the installed nonprod chart and image.
func TestControlDBModeAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for controlDB CEL admission envtest")
	}
	server := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases", "orama.cloud_oramacloudv1s.yaml")},
		ErrorIfCRDPathMissing: true,
	}
	config, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	kube, err := client.New(config, client.Options{Scheme: ocv1TestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const namespace = "controldb-admission"
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	type admissionCase struct {
		name       string
		components map[string]interface{}
		reject     bool
	}
	tests := []admissionCase{
		{name: "components omitted"},
		{name: "controlDB omitted", components: map[string]interface{}{}},
		{name: "external broker", components: map[string]interface{}{
			"broker":    map[string]interface{}{"enabled": true, "mode": "external"},
			"controlDB": map[string]interface{}{"enabled": true, "mode": "in-cluster"},
		}},
	}
	for _, enabled := range []bool{false, true} {
		for _, mode := range []string{"", "in-cluster", "external"} {
			for _, credentials := range []bool{false, true} {
				db := map[string]interface{}{"enabled": enabled}
				if mode != "" {
					db["mode"] = mode
				}
				if credentials {
					db["endpoint"] = "postgres.example.test:5432"
					db["credentialsSecretRef"] = map[string]interface{}{"name": "any-secret"}
				}
				tests = append(tests, admissionCase{
					name:       fmt.Sprintf("enabled=%t/mode=%s/credentials=%t", enabled, mode, credentials),
					components: map[string]interface{}{"controlDB": db},
					reject:     mode == "external",
				})
			}
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := map[string]interface{}{"region": map[string]interface{}{"name": "proof", "role": "primary"}}
			if tt.components != nil {
				spec["components"] = tt.components
			}
			fixture := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "orama.cloud/v1alpha1", "kind": "OramaCloudV1",
				"metadata": map[string]interface{}{"name": "proof-controldb-mode", "namespace": namespace},
				"spec":     spec,
			}}
			err := kube.Create(ctx, fixture, client.DryRunAll)
			if tt.reject {
				const message = "controlDB.mode external is not supported: the control DB is always in-cluster"
				if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), message) {
					t.Fatalf("expected controlDB CEL rejection, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid controlDB or independent external broker refused: %v", err)
			}
			if !tt.reject && tt.components != nil && tt.components["controlDB"] != nil {
				mode, found, err := unstructured.NestedString(fixture.Object, "spec", "components", "controlDB", "mode")
				if err != nil || !found || mode != "in-cluster" {
					t.Fatalf("accepted controlDB mode must default to in-cluster: found=%t, mode=%q, error=%v", found, mode, err)
				}
			}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "proof-controldb-mode"}, fixture); !apierrors.IsNotFound(err) {
				t.Fatalf("dry-run fixture persisted: %v", err)
			}
		})
	}
}
