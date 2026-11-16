// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// lake#1228: Lake's restart_app bumped only the row's generation, the operator rendered the same
// pod template, and no pod was replaced. A restart now writes apps.restart_generation, which the
// sync copies to spec.sync.restartGeneration and the workload writes on the pod template.
func TestARestartChangesThePodTemplateAndNothingElseDoes(t *testing.T) {
	ctx := context.Background()
	lake := rowTrustLake("apps.example.test")
	row := testLakeAppRow()
	spec, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Sync.RestartGeneration != 0 {
		t.Fatalf("a row never restarted carries restartGeneration %d", spec.Sync.RestartGeneration)
	}
	app := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "sales", Namespace: "lake"}, Spec: spec}
	scheme := newLakeAppScheme(t)
	reconciler := &LakeAppReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	secrets := lakeAppSecrets{Digest: "credential-digest"}
	const namespace = "app-namespace"
	template := func() map[string]string {
		t.Helper()
		if err := reconciler.reconcileLakeAppWorkload(ctx, lake, app, namespace, secrets); err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		key := client.ObjectKey{Namespace: namespace, Name: lakeAppWorkloadObjectName(app)}
		if err := reconciler.Get(ctx, key, &deployment); err != nil {
			t.Fatal(err)
		}
		return deployment.Spec.Template.Annotations
	}

	never := template()
	if _, ok := never[lakeAppRestartAnnotation]; ok {
		t.Fatalf("an app never restarted has a restart annotation, so an operator upgrade would roll it: %v", never)
	}
	if never[lakeAppCredentialHashAnnotation] != "credential-digest" || never[lakeAppOwnerAnnotation] == "" {
		t.Fatalf("the template lost its other annotations: %v", never)
	}

	// Lake restarts the app: generation 3, restart_generation 3.
	row.Generation, row.RestartGeneration = 3, 3
	if app.Spec, err = lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err != nil {
		t.Fatal(err)
	}
	restarted := template()
	if restarted[lakeAppRestartAnnotation] != "3" {
		t.Fatalf("restart annotation = %q, want 3", restarted[lakeAppRestartAnnotation])
	}

	// Another edit (a scale, generation 4) keeps the template, so it replaces no pod.
	row.Generation = 4
	if app.Spec, err = lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err != nil {
		t.Fatal(err)
	}
	if again := template(); again[lakeAppRestartAnnotation] != "3" {
		t.Fatalf("a non-restart edit changed the restart annotation to %q", again[lakeAppRestartAnnotation])
	}

	// A second restart is a new template.
	row.Generation, row.RestartGeneration = 5, 5
	if app.Spec, err = lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err != nil {
		t.Fatal(err)
	}
	if second := template(); second[lakeAppRestartAnnotation] != "5" {
		t.Fatalf("second restart annotation = %q, want 5", second[lakeAppRestartAnnotation])
	}

	row.RestartGeneration = -1
	if _, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err == nil {
		t.Fatal("a negative restart generation was accepted")
	}
}
