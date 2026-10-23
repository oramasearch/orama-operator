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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The set CRDs as the API server enforces them: a migration names exactly one member, a LakeSet
// has no migrations, a LakeSet's e2e test names exactly one member, and every committed set file
// is accepted.
func TestSetCRDsAcceptTheCommittedSetsAndRefuseMigrationsIntoNoMember(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the set CRD envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	kube, err := client.New(config, client.Options{Scheme: clusterTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	files, err := filepath.Glob(filepath.Join("..", "..", "config", "clusters", "sets", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no set files: %v", err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		set := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, &set.Object); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := kube.Create(ctx, set, client.DryRunAll); err != nil {
			t.Fatalf("%s is refused by its CRD: %v", file, err)
		}
	}

	set := func(name string, members []string, migrations ...oramav1alpha1.SetMigration) *oramav1alpha1.OramaCloudV1Set {
		s := &oramav1alpha1.OramaCloudV1Set{ObjectMeta: metav1.ObjectMeta{Name: name}}
		s.Spec.Template = *rawJSON(`{}`)
		for _, member := range members {
			s.Spec.Clusters = append(s.Spec.Clusters, oramav1alpha1.SetMember{ClusterRef: "orama-nonprod-cluster-01", Name: member})
		}
		s.Spec.Migrations = migrations
		return s
	}
	migration := func(name, member string) oramav1alpha1.SetMigration {
		return oramav1alpha1.SetMigration{Name: name, Member: member, Spec: *rawJSON(`{"step":"plan"}`)}
	}
	if err := kube.Create(ctx, set("known", []string{"v1-dev"}, migration("pilot-1", "v1-dev")), client.DryRunAll); err != nil {
		t.Fatalf("a migration into a member was refused: %v", err)
	}
	for name, refused := range map[string]*oramav1alpha1.OramaCloudV1Set{
		"unknown member": set("unknown", []string{"v1-dev"}, migration("pilot-1", "v1-staging")),
		// Two members of one name on different clusters: the migration would not say which.
		"ambiguous member": func() *oramav1alpha1.OramaCloudV1Set {
			s := set("ambiguous", []string{"v1-dev", "v1-dev"}, migration("pilot-1", "v1-dev"))
			s.Spec.Clusters[1].ClusterRef = "orama-nonprod-cluster-02"
			return s
		}(),
		"duplicate name": set("duplicate", []string{"v1-dev"}, migration("pilot-1", "v1-dev"), migration("pilot-1", "v1-dev")),
	} {
		err := kube.Create(ctx, refused, client.DryRunAll)
		if !apierrors.IsInvalid(err) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
		if name != "duplicate name" && !strings.Contains(err.Error(), "every migrations[].member must name exactly one clusters[].name") {
			t.Fatalf("%s: refused for another reason: %v", name, err)
		}
	}

	// A LakeSet's schema has no migrations: the field is pruned.
	lake := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "orama.cloud/v1alpha1", "kind": "LakeSet", "metadata": map[string]any{"name": "lake"},
		"spec": map[string]any{"template": map[string]any{}, "clusters": []any{map[string]any{"clusterRef": "c", "name": "dev"}},
			"migrations": []any{map[string]any{"name": "pilot-1", "member": "dev", "spec": map[string]any{}}}},
	}}
	if err := kube.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedSlice(lake.Object, "spec", "migrations"); found {
		t.Fatal("a LakeSet kept spec.migrations")
	}

	// A LakeSet's e2e test names exactly one member (operator#589).
	lakeSet := func(name string, members []string, tests ...oramav1alpha1.SetE2ETest) *oramav1alpha1.LakeSet {
		s := &oramav1alpha1.LakeSet{ObjectMeta: metav1.ObjectMeta{Name: name}}
		s.Spec.Template = *rawJSON(`{}`)
		for _, member := range members {
			s.Spec.Clusters = append(s.Spec.Clusters, oramav1alpha1.SetMember{ClusterRef: "orama-nonprod-cluster-01", Name: member})
		}
		s.Spec.E2ETests = tests
		return s
	}
	gate := func(name, member string) oramav1alpha1.SetE2ETest {
		return oramav1alpha1.SetE2ETest{Name: name, Member: member, Spec: *rawJSON(`{"image":"oramasearch/lake-e2e@sha256:0","runID":"1"}`)}
	}
	accepted := lakeSet("gate", []string{"testing"}, gate("testing-gate", "testing"))
	if err := kube.Create(ctx, accepted); err != nil {
		t.Fatalf("an e2e test against a member was refused: %v", err)
	}
	if tests := accepted.Spec.E2ETests; len(tests) != 1 || !strings.Contains(string(tests[0].Spec.Raw), "lake-e2e@sha256:0") {
		t.Fatalf("the e2e test's spec was not kept: %+v", tests)
	}
	for name, refused := range map[string]*oramav1alpha1.LakeSet{
		"unknown member": lakeSet("unknown", []string{"testing"}, gate("testing-gate", "staging")),
		"duplicate name": lakeSet("duplicate", []string{"testing"}, gate("testing-gate", "testing"), gate("testing-gate", "testing")),
		"bad name":       lakeSet("badname", []string{"testing"}, gate("Testing_Gate", "testing")),
	} {
		err := kube.Create(ctx, refused, client.DryRunAll)
		if !apierrors.IsInvalid(err) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
		if name == "unknown member" && !strings.Contains(err.Error(), "every e2eTests[].member must name exactly one clusters[].name") {
			t.Fatalf("%s: refused for another reason: %v", name, err)
		}
	}
}
