/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/imagepolicy"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestCredentialHelpersArePinned(t *testing.T) {
	ctx := context.Background()
	for _, compare := range []bool{false, true} {
		job := &batchv1.Job{Spec: verificationJobSpec("source", "restored", "readonly", compare)}
		if err := validateWorkloadImages(job); err != nil {
			t.Error(err)
		}
	}
	for _, image := range []string{"", "postgres:16-alpine"} {
		job := &batchv1.Job{Spec: pgMigrationJobSpec(&oramav1alpha1.PostgresSchema{Spec: oramav1alpha1.PostgresSchemaSpec{Image: image}})}
		if err := validateWorkloadImages(job); err != nil {
			t.Error(err)
		}
	}
	// Exercise the actual Lake schema renderer, not just its default constant.
	scheme := newLakeScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "pin-test"}}
	lr := &LakeReconciler{Client: cl, Scheme: scheme}
	if err := lr.reconcileLakeControlDBSchema(ctx, lake, "test", nil); err != nil {
		t.Fatal(err)
	}
	ps := &oramav1alpha1.PostgresSchema{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "test", Name: lakeControlDB(lake.Name) + "-schema"}, ps); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkloadImages(&batchv1.Job{Spec: pgMigrationJobSpec(ps)}); err != nil {
		t.Error(err)
	}
	z := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "pin-test", Namespace: "test"}}
	zr := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}
	if err := zr.reconcileZettaControlPlane(ctx, z, "zetta"); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: "test", Name: z.Name + "-zetta-schema"}
	if err := cl.Get(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkloadImages(job); err != nil {
		t.Error(err)
	}
	// A pre-fix schema Job must be replaced even though its SQL is unchanged.
	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, key, cm); err != nil {
		t.Fatal(err)
	}
	oldSum := sha256.Sum256([]byte(cm.Data["apply.sh"]))
	job.Annotations[zettaSchemaVersionAnnotation] = fmt.Sprintf("sha256:%x", oldSum[:6])
	job.Spec.Template.Spec.Containers[0].Image = "curlimages/curl:8.10.1"
	if err := cl.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := zr.reconcileZettaControlPlane(ctx, z, "zetta"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete Job was not removed: %v", err)
	}
}

func TestRehearsalsUseReadOnlyCredentialsOnEveryPath(t *testing.T) {
	for _, operation := range []string{"restore", "upgrade", "failover"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newRehearsalFixture(t, operation, 2)
			if operation == "failover" {
				reconcileRehearsal(t, fixture)
				rehearsal := getRehearsal(t, fixture)
				lake := getRehearsalLake(t, fixture)
				r := &LakeRestoreRehearsalReconciler{Client: fixture.client, Scheme: fixture.scheme}
				job, err := r.ensureFailoverVerificationJob(context.Background(), rehearsal, lake)
				if err != nil {
					t.Fatal(err)
				}
				if !jobEnvSecretIs(job.Spec.Template.Spec.Containers[0].Env, "PGPASSWORD", lakeRehearsalSecretName(lake)) {
					t.Fatal("failover holds an owner credential")
				}
			} else {
				scratch := advanceScratchRehearsalToReadyCluster(t, fixture)
				secret := &corev1.Secret{}
				if err := fixture.client.Get(context.Background(), client.ObjectKey{Namespace: fixture.scratchNamespace(), Name: "source-db"}, secret); err != nil {
					t.Fatal(err)
				}
				if string(secret.Data["username"]) != lakeRehearsalRoleName {
					t.Fatal("scratch copied the database owner's credential")
				}
				role, _, _ := unstructured.NestedSlice(scratch.Object, "spec", "managed", "roles")
				if !reflect.DeepEqual(role, []interface{}{lakeRehearsalRole("source-db")}) {
					t.Fatalf("scratch roles: %v", role)
				}
			}
		})
	}
}

func TestRehearsalRoleIsManagedAndStable(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "test"}, Spec: oramav1alpha1.LakeSpec{Postgres: oramav1alpha1.LakePostgresSpec{Image: rehearsalTestSourceImage}}}
	r := &LakeReconciler{Client: cl, Scheme: scheme}
	if err := r.reconcileLakeControlDB(ctx, lake, "test", nil, lakeBackupConfig{}); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: "test", Name: lakeRehearsalSecretName(lake)}
	if err := cl.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	before := secret.DeepCopy()
	if secret.Type != corev1.SecretTypeBasicAuth || string(secret.Data["username"]) != lakeRehearsalRoleName {
		t.Fatal("not a CNPG read-only login secret")
	}
	if err := r.reconcileLakeControlDB(ctx, lake, "test", nil, lakeBackupConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Data, secret.Data) || before.ResourceVersion != secret.ResourceVersion {
		t.Fatal("steady reconcile changed the credential")
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "test", Name: lakeControlDB(lake.Name)}, cluster); err != nil {
		t.Fatal(err)
	}
	roles, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "managed", "roles")
	role := roles[0].(map[string]interface{})
	for _, field := range []string{"superuser", "createdb", "createrole", "replication", "bypassrls"} {
		if role[field] != false {
			t.Errorf("role grants %s", field)
		}
	}
	if !reflect.DeepEqual(role["inRoles"], []interface{}{"pg_read_all_data"}) {
		t.Fatal("unexpected role memberships")
	}
	image, _, _ := unstructured.NestedString(cluster.Object, "spec", "imageName")
	if image != lake.Spec.Postgres.Image {
		t.Fatal("source image pin was not applied")
	}
}

func TestRehearsalRejectsMutableScratchImages(t *testing.T) {
	for _, operation := range []string{"restore", "upgrade"} {
		t.Run(operation, func(t *testing.T) {
			f := newRehearsalFixture(t, operation, 2)
			source := getSourceCluster(t, f)
			if err := unstructured.SetNestedField(source.Object, "postgres:18-alpine", "status", "image"); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Status().Update(context.Background(), source); err != nil {
				t.Fatal(err)
			}
			reconcileRehearsal(t, f)
			reconcileRehearsal(t, f)
			reconcileRehearsal(t, f)
			rehearsal := getRehearsal(t, f)
			if rehearsal.Status.Phase != "Failed" || !strings.Contains(rehearsal.Status.Message, "immutable SHA-256") {
				t.Fatalf("mutable restore source accepted: %s", rehearsal.Status.Phase)
			}
			var secrets corev1.SecretList
			if err := f.client.List(context.Background(), &secrets, client.InNamespace(f.scratchNamespace())); err != nil {
				t.Fatal(err)
			}
			if len(secrets.Items) != 0 {
				t.Fatal("copied credentials before validating source image")
			}
		})
	}
	f := newRehearsalFixture(t, "upgrade", 2)
	rehearsal := getRehearsal(t, f)
	rehearsal.Spec.TargetImage = "postgres:19"
	if got := validateRehearsal(rehearsal, getRehearsalLake(t, f)); !strings.Contains(got, "immutable SHA-256") {
		t.Fatal("mutable upgrade target accepted")
	}
}

func TestCloudV1ImageWarningsDoNotBlockOrFlap(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Status.Phase = "Ready"
	r, writes := storageFake(t, oc)
	recorder := record.NewFakeRecorder(10)
	r.Recorder = recorder
	if err := r.reconcileApp(ctx, oc, appDef{component: "test", image: "app:mutable", port: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := r.reportCloudV1Images(ctx, oc); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(oc.Status.Conditions, "ImagesPinned")
	if oc.Status.Phase != "Ready" || condition == nil || condition.Reason != "ImagesNotPinned" {
		t.Fatalf("warning changed readiness or is missing: %+v", oc.Status)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning ImagesNotPinned") {
			t.Fatal(event)
		}
	default:
		t.Fatal("missing warning event")
	}
	before := writes.status
	if err := r.reportCloudV1Images(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if writes.status != before || len(recorder.Events) != 0 {
		t.Fatal("steady image warning flaps")
	}
	oc.Spec.Images.RequireDigests = true
	if err := r.reportCloudV1Images(ctx, oc); err == nil {
		t.Fatal("enforcement accepted existing tagged workload")
	}
	if err := r.reconcileApp(ctx, oc, appDef{component: "test", image: pinnedCurlImage, port: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := r.reportCloudV1Images(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(oc.Status.Conditions, "ImagesPinned") {
		t.Fatal("warning did not clear after pinning")
	}
}

func TestImageEnforcementChecksCreatesUpdatesAndRawPatches(t *testing.T) {
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	guarded := imageEnforcingClient{Client: base}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:tag"}}}}}}
	if err := guarded.Create(ctx, dep); err == nil {
		t.Fatal("mutable create accepted")
	}
	dep.Spec.Template.Spec.Containers[0].Image = pinnedCurlImage
	if err := guarded.Create(ctx, dep); err != nil {
		t.Fatal(err)
	}
	dep.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "new-init", Image: "init:tag"}}
	if err := guarded.Update(ctx, dep); err == nil {
		t.Fatal("mutable init update accepted")
	}
	dep.Spec.Template.Spec.InitContainers = nil
	for _, patch := range []client.Patch{
		client.RawPatch(types.MergePatchType, []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"app","image":"app:tag"}]}}}}`)),
		client.RawPatch(types.JSONPatchType, []byte(`[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"app:tag"}]`)),
	} {
		if err := guarded.Patch(ctx, dep, patch); err == nil {
			t.Fatal("raw patch bypassed image enforcement")
		}
	}
	fresh := &appsv1.Deployment{}
	if err := base.Get(ctx, client.ObjectKeyFromObject(dep), fresh); err != nil {
		t.Fatal(err)
	}
	if !imagepolicy.Pinned(fresh.Spec.Template.Spec.Containers[0].Image) {
		t.Fatal("rejected patch changed the stored workload")
	}
}

func TestCloudV1RenderedFixedDefaultsArePinned(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Cache.Enabled = true
		o.Spec.Components.Broker.Enabled = true
		o.Spec.Components.Analytics.Enabled = true
		o.Spec.Components.Search.Enabled = true
		o.Spec.Components.Ingestion.Enabled = true
		o.Spec.Components.Ingestion.Harvest = true
		o.Spec.Components.PgAdmin.Enabled = true
		o.Spec.Components.ControlDB.Enabled = true
		o.Spec.Components.Engine.Envoy = true
		o.Spec.IDP.Provider = "keycloak"
		o.Spec.DemoSeed = true
		o.Spec.Images.Overrides = map[string]string{"oramacore": pinnedCurlImage, "embeddings": pinnedCurlImage, "controldb": rehearsalTestSourceImage, "index-builder": pinnedCurlImage}
	})
	r, _ := storageFake(t, oc)
	steps := []func() error{
		func() error { return r.reconcileCache(ctx, oc) },
		func() error { return r.reconcileBroker(ctx, oc) },
		func() error { return r.reconcileAnalytics(ctx, oc) },
		func() error { return r.reconcileSearch(ctx, oc) },
		func() error { return r.reconcileIngestion(ctx, oc) },
		func() error { return r.reconcileIdP(ctx, oc) },
		func() error { return r.reconcileDemoSeed(ctx, oc) },
		func() error { return r.reconcilePgAdmin(ctx, oc) },
		func() error { return r.reconcileControlDB(ctx, oc) },
		func() error { return r.reconcileEmbeddings(ctx, oc) },
		func() error { return r.reconcileWriter(ctx, oc, "", "") },
		func() error { return r.reconcileReaders(ctx, oc, "", "", "") },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("render step %d: %v", i, err)
		}
	}
	if err := r.reportCloudV1Images(ctx, oc); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(oc.Status.Conditions, "ImagesPinned")
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("rendered a mutable default: %+v", condition)
	}
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, client.InNamespace(oc.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(deps.Items) < 4 {
		t.Fatal("did not render app/helper deployments")
	}
	var stores appsv1.StatefulSetList
	if err := r.List(ctx, &stores, client.InNamespace(oc.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(stores.Items) < 7 {
		t.Fatalf("did not render every store and engine side: %d", len(stores.Items))
	}
}

func TestCredentialJobsReplaceObsoleteImageOrOwnerCredentials(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	ps := pgSchemaCR()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ps).Build()
	schemas := &PostgresSchemaReconciler{Client: cl, Scheme: scheme}
	job, err := schemas.ensureMigrationJob(ctx, ps)
	if err != nil {
		t.Fatal(err)
	}
	job.Spec.Template.Spec.Containers[0].Image = "postgres:16-alpine"
	if err := cl.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if current, err := schemas.ensureMigrationJob(ctx, ps); err != nil || current != nil {
		t.Fatalf("obsolete schema image retained: job=%v err=%v", current != nil, err)
	}
	for _, operation := range []string{"restore", "failover"} {
		t.Run(operation, func(t *testing.T) {
			f := newRehearsalFixture(t, operation, 2)
			reconcileRehearsal(t, f)
			rehearsal, lake := getRehearsal(t, f), getRehearsalLake(t, f)
			r := &LakeRestoreRehearsalReconciler{Client: f.client, Scheme: f.scheme}
			ensure := func() (*batchv1.Job, error) {
				if operation == "failover" {
					return r.ensureFailoverVerificationJob(ctx, rehearsal, lake)
				}
				return r.ensureScratchVerificationJob(ctx, rehearsal, lake)
			}
			verifier, err := ensure()
			if err != nil {
				t.Fatal(err)
			}
			verifier.Spec.Template.Spec.Containers[0].Image = "postgres:18-alpine"
			for i := range verifier.Spec.Template.Spec.Containers[0].Env {
				env := &verifier.Spec.Template.Spec.Containers[0].Env[i]
				if env.Name == "PGPASSWORD" {
					env.ValueFrom = rehearsalSecretKey(lakeControlDB(lake.Name)+"-app", "password")
				}
			}
			if err := f.client.Update(ctx, verifier); err != nil {
				t.Fatal(err)
			}
			if current, err := ensure(); err != nil || current != nil {
				t.Fatalf("obsolete verifier retained: job=%v err=%v", current != nil, err)
			}
			current, err := ensure()
			if err != nil {
				t.Fatal(err)
			}
			expectedSecret := "source-db"
			if operation == "failover" {
				expectedSecret = lakeRehearsalSecretName(lake)
			}
			if !rehearsalVerifierCurrent(current, expectedSecret) {
				t.Fatal("replacement verifier is unsafe")
			}
		})
	}
}

func TestCloudV1ReconcileEnforcesImagePolicyOnlyWhenRequested(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Cache.Enabled = true
		o.Spec.Images.RequireDigests = true
		o.Spec.Images.Overrides = map[string]string{"cache": "redis:mutable"}
	})
	r, _ := storageFake(t, oc)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("enforcing reconcile accepted mutable cache")
	}
	var cache appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-cache"}, &cache); !apierrors.IsNotFound(err) {
		t.Fatalf("cache was written before policy check: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(oc), oc); err != nil {
		t.Fatal(err)
	}
	oc.Spec.Images.RequireDigests = false
	if err := r.Update(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("warning-only reconcile blocked: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-cache"}, &cache); err != nil {
		t.Fatal(err)
	}
	if cache.Spec.Template.Spec.Containers[0].Image != "redis:mutable" {
		t.Fatal("warning mode changed live image selection")
	}
}

func TestCloudV1E2EDefaultAndAdmissionImageArePinned(t *testing.T) {
	for _, image := range []string{"", "alpine:3.20", "custom:mutable"} {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			scheme := ocv1TestScheme(t)
			target := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.RequireDigests = true })
			test := &oramav1alpha1.OramaCloudV1E2ETest{ObjectMeta: metav1.ObjectMeta{Name: "image-test", Namespace: target.Namespace}, Spec: oramav1alpha1.OramaCloudV1E2ETestSpec{TargetRef: target.Name, Image: image}}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, test).WithStatusSubresource(test).Build()
			r := &OramaCloudV1E2ETestReconciler{Client: cl, Scheme: scheme}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(test)})
			if image == "custom:mutable" {
				if err == nil {
					t.Fatal("E2E runner bypassed target image policy")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			job := &batchv1.Job{}
			if err := cl.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: test.Name + "-e2e"}, job); err != nil {
				t.Fatal(err)
			}
			if err := validateWorkloadImages(job); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnforcedExistingImageWarningDoesNotFlapReadiness(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.RequireDigests = true })
	r, writes := storageFake(t, oc)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "legacy-completed", Namespace: oc.Namespace}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "legacy", Image: "helper:mutable"}}}}}, Status: batchv1.JobStatus{Succeeded: 1}}
	if err := controllerutil.SetControllerReference(oc, job, r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("legacy image was not reported")
	}
	before := writes.status
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("legacy image disappeared from enforcement")
	}
	if writes.status != before {
		t.Fatalf("unchanged enforced warning rewrote status %d times", writes.status-before)
	}
}
