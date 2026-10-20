/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The API server's CEL evaluator holds a migration to what it declared: the target, team,
// projects, run and source never change, while step, apply and attempt do.
func TestMigrationAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for OramaCloudV1Migration CEL admission envtest")
	}
	server := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "chart", "crds", "orama.cloud_oramacloudv1migrations.yaml")},
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
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v1-dev"}}); err != nil {
		t.Fatal(err)
	}
	m := migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) { m.UID, m.Generation = "", 0 })
	if err := kube.Create(ctx, m); err != nil {
		t.Fatalf("the issue's example was refused: %v", err)
	}

	update := func(mut func(*oramav1alpha1.OramaCloudV1Migration)) error {
		current := &oramav1alpha1.OramaCloudV1Migration{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(m), current); err != nil {
			t.Fatal(err)
		}
		mut(current)
		return kube.Update(ctx, current)
	}
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1Migration){
		"teamID":     func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.TeamID = "5e01bc49-b388-43c2-8986-2395a0b8fe52" },
		"targetRef":  func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.TargetRef = "v1-prod" },
		"runID":      func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.RunID = "pilot-2" },
		"projectIDs": func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.ProjectIDs = m.Spec.ProjectIDs[:1] },
		"source": func(m *oramav1alpha1.OramaCloudV1Migration) {
			m.Spec.Source.Settings.InstanceID = "i-0b9478719a8f91e2c"
		},
	} {
		err := update(mut)
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "declare a new migration instead") {
			t.Errorf("changing %s: %v, want the immutability refusal", name, err)
		}
	}
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1Migration){
		"step":    func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Step = oramav1alpha1.MigrationStepCatchUp },
		"apply":   func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Apply = false },
		"attempt": func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Attempt = 3 },
		"projectIDs reordered": func(m *oramav1alpha1.OramaCloudV1Migration) {
			m.Spec.ProjectIDs = []string{m.Spec.ProjectIDs[1], m.Spec.ProjectIDs[0]}
		},
	} {
		if err := update(mut); err != nil {
			t.Errorf("changing %s: %v", name, err)
		}
	}

	// Field validation on create.
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1Migration){
		"an unpinned image":         func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Image = "oramasearch/cloud-v1-migration:latest" },
		"an unknown step":           func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Step = "switch" },
		"a team that is not a UUID": func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.TeamID = "team-1" },
		"no projects":               func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.ProjectIDs = nil },
		"an http origin": func(m *oramav1alpha1.OramaCloudV1Migration) {
			m.Spec.Source.WriterURL = "http://writer.prod1.example.com"
		},
		"an origin with a path": func(m *oramav1alpha1.OramaCloudV1Migration) {
			m.Spec.Source.ReaderURL = "https://reader.example.com/v1"
		},
		"a bad instance":      func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Source.Settings.InstanceID = "writer-1" },
		"a relative data dir": func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Source.Settings.DataDir = "opt/oramacore" },
		"a bad region":        func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Source.Settings.Region = "Ohio" },
		"a DSN as the host":   func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Source.DatabaseHost = "postgres://u:p@host" },
	} {
		bad := migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) {
			m.Name, m.UID, m.Generation = "bad", "", 0
			mut(m)
		})
		if err := kube.Create(ctx, bad); !apierrors.IsInvalid(err) {
			t.Errorf("%s: %v, want Invalid", name, err)
		}
	}
}
