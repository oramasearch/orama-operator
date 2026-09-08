/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeSchemaPinnedBytes(t *testing.T) {
	data, err := os.ReadFile("assets/lake-schema.source.json")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Commit string `json:"commit"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(lakeSchemaSQL))
	if hex.EncodeToString(digest[:]) != source.SHA256 || len(source.Commit) != 40 {
		t.Fatal("vendored Lake schema differs from pinned source; run hack/check-lake-schema.py")
	}
	t.Logf("vendored source: %s, SHA-256: %s", source.Commit, source.SHA256)
}

func TestLakeSchemaVersionIncludesGuard(t *testing.T) {
	oldGuardVersion := schemaVersion(lakeSchemaSQL + "\nidentity_exit_guard")
	if lakeSchemaVersion() == oldGuardVersion || lakeSchemaVersion() == schemaVersion(lakeSchemaSQL) {
		t.Fatal("Lake schema version must change when its guard changes")
	}
	t.Logf("same SQL, old guard=%s, 0021 guard=%s", oldGuardVersion, lakeSchemaVersion())
}

// Other workload tests explicitly start from an applied schema. The migration and rollout tests
// below exercise the waiting path independently, without this fixture.
func applyLakeSchemaForWorkloadTest(t *testing.T, c client.Client, s *runtime.Scheme, lake *oramav1alpha1.Lake) {
	t.Helper()
	ns := lake.Spec.Namespace
	if ns == "" {
		ns = "lake-dev"
	}
	r := &LakeReconciler{Client: c, Scheme: s}
	if err := r.reconcileLakeControlDBSchema(context.Background(), lake, ns, nil); err != nil {
		t.Fatal(err)
	}
	setLakeSchemaAppliedForTest(t, c, client.ObjectKey{Namespace: ns, Name: lakeControlDB(lake.Name) + "-schema"})
}

func setLakeSchemaAppliedForTest(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()
	var ps oramav1alpha1.PostgresSchema
	if err := c.Get(context.Background(), key, &ps); err != nil {
		t.Fatal(err)
	}
	ps.Status.Phase = "Applied"
	ps.Status.AppliedVersion = ps.Spec.Version
	ps.Status.ObservedGeneration = ps.Generation
	apimeta.SetStatusCondition(&ps.Status.Conditions, metav1.Condition{
		Type: "Applied", Status: metav1.ConditionTrue, Reason: "Applied", Message: "test migration completed",
		ObservedGeneration: ps.Generation,
	})
	if err := c.Status().Update(context.Background(), &ps); err != nil {
		t.Fatal(err)
	}
}

func TestLakeSchemaGatesWorkloadRollout(t *testing.T) {
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout"},
		Spec:       oramav1alpha1.LakeSpec{Image: "oramasearch/lake:old"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &appsv1.Deployment{}).Build()
	ctx := context.Background()
	deploymentKey := client.ObjectKey{Namespace: "lake-dev", Name: "lake-rollout"}
	schemaKey := client.ObjectKey{Namespace: "lake-dev", Name: "lake-rollout-controldb-schema"}
	reconcileLake(t, c, scheme, lake.Name)
	if err := c.Get(ctx, deploymentKey, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Deployment exists before schema applies: %v", err)
	}
	t.Log("first startup: Deployment absent while schema is Pending")
	setLakeSchemaAppliedForTest(t, c, schemaKey)
	reconcileLake(t, c, scheme, lake.Name)
	var deployment appsv1.Deployment
	if err := c.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas = 1
	if err := c.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Image = "oramasearch/lake:new"
	if err := c.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	assertLakeSchemaBlocksUpdate(t, c, scheme, lake, schemaKey, deploymentKey)
	setLakeSchemaAppliedForTest(t, c, schemaKey)
	reconcileLake(t, c, scheme, lake.Name)
	if err := c.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != lake.Spec.Image {
		t.Fatal("new image did not roll out after the desired schema applied")
	}
	t.Log("upgrade: new image released only after the desired schema reports Applied")
}

func assertLakeSchemaBlocksUpdate(t *testing.T, c client.Client, s *runtime.Scheme, lake *oramav1alpha1.Lake, schemaKey, deploymentKey client.ObjectKey) {
	t.Helper()
	for _, state := range []string{"Pending", "Applying", "Failed", "stale-version", "stale-generation", "false-condition"} {
		var ps oramav1alpha1.PostgresSchema
		if err := c.Get(context.Background(), schemaKey, &ps); err != nil {
			t.Fatal(err)
		}
		ps.Status.Phase = state
		ps.Status.AppliedVersion = ps.Spec.Version
		ps.Status.ObservedGeneration = ps.Generation
		if state == "stale-version" {
			ps.Status.Phase = "Applied"
			ps.Status.AppliedVersion = "sha256:old"
		}
		if state == "stale-generation" {
			ps.Status.Phase = "Applied"
			ps.Status.ObservedGeneration = ps.Generation - 1
		}
		if state == "false-condition" {
			ps.Status.Phase = "Applied"
			ps.Status.Conditions = nil
		}
		if err := c.Status().Update(context.Background(), &ps); err != nil {
			t.Fatal(err)
		}
		var before appsv1.Deployment
		if err := c.Get(context.Background(), deploymentKey, &before); err != nil {
			t.Fatal(err)
		}
		reconcileLake(t, c, s, lake.Name)
		var after appsv1.Deployment
		if err := c.Get(context.Background(), deploymentKey, &after); err != nil {
			t.Fatal(err)
		}
		if before.ResourceVersion != after.ResourceVersion || after.Spec.Template.Spec.Containers[0].Image != "oramasearch/lake:old" {
			t.Fatalf("Deployment changed while schema was %s", state)
		}
		t.Logf("upgrade held: schema=%s, existing Deployment unchanged", state)
	}
}
