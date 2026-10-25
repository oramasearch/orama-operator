/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// testdata/lake-manifest-v1.json is `lake manifest` from lake#1147 (a5fc33cb), the first lake with
// the command. testdata/lake-manifest-unsupported.txt is what lake v0.5.2 prints for it (exit 2).
const (
	testLakeManifestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testLakeManifestImage  = "docker.io/oramasearch/lake@" + testLakeManifestDigest
)

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseLakeManifestReadsTheImagesManifest(t *testing.T) {
	manifest, err := parseLakeManifest(readTestFile(t, "testdata/lake-manifest-v1.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Supported || manifest.Version != "0.6.1" || manifest.LatestMigration != "0099_account_balances" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if want := []string{"migrate", "--wait-seconds", "600"}; !reflect.DeepEqual(manifest.MigrateArgs, want) || manifest.DatabaseEnv != "LAKE_CONTROL_DB_URI" {
		t.Fatalf("migrate = %v %s", manifest.MigrateArgs, manifest.DatabaseEnv)
	}
	// The fixture was built from the same db/schema.sql the operator vendors, so the lists the
	// operator keeps for older images must equal what the image now publishes.
	var source struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(readTestFile(t, "assets/lake-schema.source.json")), &source); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "sha256:"+source.SHA256[:12] {
		t.Fatalf("fixture schema %s is not the vendored schema %s", manifest.SchemaVersion, source.SHA256)
	}
	if !reflect.DeepEqual(manifest.StripeWebhookEvents, lakeStripeWebhookEvents) {
		t.Errorf("events differ from lakeStripeWebhookEvents:\n%v\n%v", manifest.StripeWebhookEvents, lakeStripeWebhookEvents)
	}
	if !reflect.DeepEqual(lakeCatalogProducts(manifest), lakePaymentProducts()) {
		t.Errorf("products differ from lakePaymentProducts: %v", manifest.StripeProducts)
	}
	vendoredGuards := regexp.MustCompile(`FROM (\w+_guard)\n`).FindAllStringSubmatch(postgresGuardCountQuery, -1)
	var guards []string
	for _, match := range vendoredGuards {
		guards = append(guards, match[1])
	}
	if got, want := slices.Sorted(slices.Values(manifest.GuardTables)), slices.Sorted(slices.Values(guards)); !reflect.DeepEqual(got, want) {
		t.Errorf("guard tables differ from postgresGuardCountQuery:\n%v\n%v", got, want)
	}
	for _, version := range schemaMigrationVersions(lakeSchemaSQL) {
		if !slices.Contains(manifest.Migrations, version) {
			t.Errorf("manifest lacks vendored migration %s", version)
		}
	}
	t.Logf("manifest: %d migrations, %d guards, %d events, %d products", len(manifest.Migrations),
		len(manifest.GuardTables), len(manifest.StripeWebhookEvents), len(manifest.StripeProducts))
}

func TestGuardCountQueryRebuildsTheVendoredQuery(t *testing.T) {
	var guards []string
	for _, match := range regexp.MustCompile(`FROM (\w+_guard)\n`).FindAllStringSubmatch(postgresGuardCountQuery, -1) {
		guards = append(guards, match[1])
	}
	if got := guardCountQuery(guards); got != postgresGuardCountQuery {
		t.Fatalf("guardCountQuery differs from the vendored query:\n%s", got)
	}
}

func TestParseLakeManifestRecognisesAnOlderImage(t *testing.T) {
	manifest, err := parseLakeManifest(readTestFile(t, "testdata/lake-manifest-unsupported.txt"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Supported {
		t.Fatal("an image answering with its usage text is not supported")
	}
}

func TestParseLakeManifestRefusesWhatItCannotTrust(t *testing.T) {
	valid := readTestFile(t, "testdata/lake-manifest-v1.json")
	edit := func(change func(map[string]any)) string {
		var document map[string]any
		if err := json.Unmarshal([]byte(valid), &document); err != nil {
			t.Fatal(err)
		}
		change(document)
		data, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	schema := func(document map[string]any) map[string]any { return document["schema"].(map[string]any) }
	cases := []struct {
		name     string
		log      string
		exitCode int32
		want     string
	}{
		{"usage text with exit 0", readTestFile(t, "testdata/lake-manifest-unsupported.txt"), 0, "no manifest_version document"},
		{"exit 1", "lake: something broke", 1, "exited 1"},
		{"a manifest with exit 2", valid, 2, "exited 2"},
		{"no output", "", 0, "no output"},
		{"a newer manifest version", edit(func(d map[string]any) { d["manifest_version"] = 2 }), 0, "manifest_version 2"},
		{"another program", edit(func(d map[string]any) { d["name"] = "zetta" }), 0, "not lake"},
		{"a guard that is not a table name", edit(func(d map[string]any) {
			schema(d)["guard_tables"] = []any{"x_guard; DROP TABLE users; --"}
		}), 0, "schema.guard_tables"},
		{"a migration name with a quote", edit(func(d map[string]any) {
			schema(d)["migrations"] = []any{"0001_a'b"}
		}), 0, "schema.migrations"},
		{"no events", edit(func(d map[string]any) {
			d["stripe"].(map[string]any)["webhook_events"] = []any{}
		}), 0, "stripe.webhook_events is empty"},
		{"a database variable that is not a name", edit(func(d map[string]any) {
			d["migrate"].(map[string]any)["database_env"] = "LAKE=1"
		}), 0, "migrate.database_env"},
		{"a product without a name", edit(func(d map[string]any) {
			d["stripe"].(map[string]any)["products"] = []any{map[string]any{"id": "lake_usage", "name": ""}}
		}), 0, "stripe.products"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLakeManifest(tc.log, tc.exitCode)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLakeImageRefFromID(t *testing.T) {
	for imageID, want := range map[string]string{
		testLakeManifestImage:                        testLakeManifestImage,
		"docker-pullable://" + testLakeManifestImage: testLakeManifestImage,
		"sha256:" + strings.Repeat("a", 64):          "",
		"docker.io/oramasearch/lake:v0.6.1":          "",
		"":                                           "",
		"docker.io/oramasearch/lake@sha256:short":    "",
		"docker.io/oramasearch/lake @" + testLakeManifestDigest: "",
	} {
		if got := lakeImageRefFromID(imageID); got != want {
			t.Errorf("lakeImageRefFromID(%q) = %q, want %q", imageID, got, want)
		}
	}
}

// manifestHarness runs a Lake reconciler that reads manifest pods from a fake client.
type manifestHarness struct {
	t    *testing.T
	ctx  context.Context
	c    client.Client
	s    *runtime.Scheme
	r    *LakeReconciler
	name string
	ns   string
	logs map[string]string
}

func newManifestHarness(t *testing.T, lake *oramav1alpha1.Lake) *manifestHarness {
	t.Helper()
	s := newLakeScheme(t)
	// The API server gives every object a UID; the fake client does not, and the manifest pod is
	// matched to its Job by UID.
	created := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &appsv1.Deployment{}, &batchv1.Job{}, &corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			created++
			obj.SetUID(types.UID(fmt.Sprintf("uid-%d", created)))
			return c.Create(ctx, obj, opts...)
		}}).Build()
	h := &manifestHarness{t: t, ctx: context.Background(), c: c, s: s, name: lake.Name, ns: lakeNamespace(lake), logs: map[string]string{}}
	h.r = &LakeReconciler{Client: c, Scheme: s, LakeManifestLogs: func(_ context.Context, namespace, pod string) (string, error) {
		return h.logs[namespace+"/"+pod], nil
	}}
	return h
}

func (h *manifestHarness) reconcile() ctrl.Result {
	h.t.Helper()
	result, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: h.name}})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return result
}

func (h *manifestHarness) lake() *oramav1alpha1.Lake {
	h.t.Helper()
	var lake oramav1alpha1.Lake
	if err := h.c.Get(h.ctx, types.NamespacedName{Name: h.name}, &lake); err != nil {
		h.t.Fatal(err)
	}
	return &lake
}

func (h *manifestHarness) key(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: h.ns, Name: name}
}

func (h *manifestHarness) manifestJob() *batchv1.Job {
	h.t.Helper()
	var job batchv1.Job
	if err := h.c.Get(h.ctx, h.key("lake-"+h.name+"-manifest"), &job); err != nil {
		h.t.Fatal(err)
	}
	return &job
}

// finishManifestPod is the kubelet: the Job's pod ran and exited with the given log.
func (h *manifestHarness) finishManifestPod(exitCode int32, imageID, log string) {
	h.t.Helper()
	job := h.manifestJob()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", job.Name, len(h.logs)),
			Namespace: h.ns, Labels: map[string]string{"job-name": job.Name}, CreationTimestamp: metav1.Now()},
		Spec: job.Spec.Template.Spec,
	}
	if err := controllerutil.SetControllerReference(job, pod, h.s); err != nil {
		h.t.Fatal(err)
	}
	if err := h.c.Create(h.ctx, pod); err != nil {
		h.t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: lakeManifestContainer, ImageID: imageID,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
	}}
	if err := h.c.Status().Update(h.ctx, pod); err != nil {
		h.t.Fatal(err)
	}
	h.logs[h.ns+"/"+pod.Name] = log
	now := metav1.Now()
	if exitCode == 0 {
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now}}
	}
	if err := h.c.Status().Update(h.ctx, job); err != nil {
		h.t.Fatal(err)
	}
}

func (h *manifestHarness) schema() *oramav1alpha1.PostgresSchema {
	h.t.Helper()
	var ps oramav1alpha1.PostgresSchema
	if err := h.c.Get(h.ctx, h.key(lakeControlDB(h.name)+"-schema"), &ps); err != nil {
		h.t.Fatal(err)
	}
	return &ps
}

func (h *manifestHarness) deploymentImage() string {
	h.t.Helper()
	var deployment appsv1.Deployment
	err := h.c.Get(h.ctx, h.key("lake-"+h.name), &deployment)
	if apierrors.IsNotFound(err) {
		return ""
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return deployment.Spec.Template.Spec.Containers[0].Image
}

func (h *manifestHarness) setImage(image string) {
	h.t.Helper()
	lake := h.lake()
	lake.Spec.Image = image
	if err := h.c.Update(h.ctx, lake); err != nil {
		h.t.Fatal(err)
	}
}

func (h *manifestHarness) readyReason() string {
	h.t.Helper()
	condition := apimeta.FindStatusCondition(h.lake().Status.Conditions, "Ready")
	if condition == nil {
		return ""
	}
	return condition.Reason
}

// A Lake on an image older than `lake manifest` keeps the vendored path; moving it to an image
// with the command reads the manifest once, runs `lake migrate` from that image by digest before
// the Deployment rolls, and holds the old Deployment until the migration applies. Moving back runs
// the vendored path again.
func TestLakeImageManifestDecidesTheSchemaPathAndHoldsTheRollout(t *testing.T) {
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "manifest"},
		Spec:       oramav1alpha1.LakeSpec{Image: "oramasearch/lake:v0.6.1", PullSecretName: "dockerhub"},
	}
	h := newManifestHarness(t, lake)

	// 1. A new Lake on an old image: the manifest Job runs first, nothing else waits on a guess.
	if result := h.reconcile(); result.RequeueAfter == 0 || result.RequeueAfter > lakeManifestPoll {
		t.Fatalf("requeue = %v while reading the manifest", result.RequeueAfter)
	}
	job := h.manifestJob()
	pod := job.Spec.Template.Spec
	if job.Annotations[lakeManifestImageAnnotation] != "oramasearch/lake:v0.6.1" || pod.Containers[0].Image != "oramasearch/lake:v0.6.1" ||
		!reflect.DeepEqual(pod.Containers[0].Args, []string{"manifest"}) || pod.Containers[0].Command != nil ||
		len(pod.Containers[0].Env) != 0 || *pod.AutomountServiceAccountToken || *job.Spec.BackoffLimit != 0 ||
		len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "dockerhub" ||
		*pod.SecurityContext.RunAsUser != lakeRunAsUser || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatalf("manifest Job = %+v", job.Spec)
	}
	if job.Spec.Template.Labels["app.kubernetes.io/name"] == "lake" {
		t.Fatal("the manifest pod would match the Lake Service")
	}
	if h.deploymentImage() != "" || h.lake().Status.ImageManifest != nil || h.lake().Status.Phase != "Progressing" {
		t.Fatalf("deployment %q, phase %s while reading the manifest", h.deploymentImage(), h.lake().Status.Phase)
	}
	if err := h.c.Get(h.ctx, h.key("lake-manifest-controldb-schema"), &oramav1alpha1.PostgresSchema{}); !apierrors.IsNotFound(err) {
		t.Fatalf("PostgresSchema made before the manifest was read: %v", err)
	}
	t.Log("new Lake: manifest Job created, Deployment and schema wait")

	h.finishManifestPod(2, "docker.io/oramasearch/lake@sha256:"+strings.Repeat("0", 64), readTestFile(t, "testdata/lake-manifest-unsupported.txt"))
	h.reconcile()
	if m := h.lake().Status.ImageManifest; m == nil || m.Supported || m.Image != "oramasearch/lake:v0.6.1" {
		t.Fatalf("status.imageManifest = %+v", m)
	}
	ps := h.schema()
	if ps.Spec.Migrate != nil || ps.Spec.SchemaConfigMapRef.Name != "lake-manifest-controldb-migrations" || ps.Spec.Version != lakeSchemaVersion() ||
		ps.Spec.Image != pinnedPostgres18Image {
		t.Fatalf("an older image must use the vendored schema: %+v", ps.Spec)
	}
	setLakeSchemaAppliedForTest(t, h.c, h.key(ps.Name))
	h.reconcile()
	if h.deploymentImage() != "oramasearch/lake:v0.6.1" {
		t.Fatalf("deployment image = %q", h.deploymentImage())
	}
	t.Log("older image: vendored schema applied, Deployment created")

	// 2. The release that carries `lake manifest`: only the image changes.
	h.setImage("oramasearch/lake:v0.7.0")
	h.reconcile() // the old manifest Job is replaced
	h.reconcile()
	if job := h.manifestJob(); job.Annotations[lakeManifestImageAnnotation] != "oramasearch/lake:v0.7.0" {
		t.Fatalf("manifest Job annotation = %s", job.Annotations[lakeManifestImageAnnotation])
	}
	if h.deploymentImage() != "oramasearch/lake:v0.6.1" || h.schema().Spec.Migrate != nil {
		t.Fatalf("the Deployment (%s) or schema (%+v) moved before the new image manifest was read", h.deploymentImage(), h.schema().Spec.Migrate)
	}
	h.finishManifestPod(0, "docker-pullable://"+testLakeManifestImage, readTestFile(t, "testdata/lake-manifest-v1.json"))
	h.reconcile()
	status := h.lake().Status.ImageManifest
	if status == nil || !status.Supported || status.Image != "oramasearch/lake:v0.7.0" || status.ImageRef != testLakeManifestImage {
		t.Fatalf("status.imageManifest = %+v", status)
	}
	ps = h.schema()
	wantMigrate := &oramav1alpha1.PostgresSchemaMigrate{
		Args: []string{"migrate", "--wait-seconds", "600"}, DatabaseEnv: "LAKE_CONTROL_DB_URI",
		FailOnExitCodes: []int32{1, 2}, RunAsUser: ptrInt64(lakeRunAsUser), RunAsGroup: ptrInt64(lakeRunAsGroup),
	}
	if !reflect.DeepEqual(ps.Spec.Migrate, wantMigrate) || ps.Spec.Image != testLakeManifestImage ||
		ps.Spec.Version != "image:"+testLakeManifestDigest || ps.Spec.ConnectionSecretRef.Name != "lake-manifest-controldb-app" ||
		len(ps.Spec.ImagePullSecrets) != 1 || ps.Spec.ImagePullSecrets[0].Name != "dockerhub" {
		t.Fatalf("PostgresSchema = %+v", ps.Spec)
	}
	if err := h.c.Get(h.ctx, h.key("lake-manifest-controldb-migrations"), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the vendored schema ConfigMap stays: %v", err)
	}
	if h.deploymentImage() != "oramasearch/lake:v0.6.1" {
		t.Fatal("the new image rolled before `lake migrate` applied")
	}
	t.Log("new image: lake migrate from the image by digest, old Deployment held")

	// The migration Job the PostgresSchema controller renders for it.
	migrateJob := pgMigrationJobSpec(ps)
	container := migrateJob.Template.Spec.Containers[0]
	if container.Image != testLakeManifestImage || !reflect.DeepEqual(container.Args, wantMigrate.Args) || container.Command != nil ||
		len(container.Env) != 1 || container.Env[0].Name != "LAKE_CONTROL_DB_URI" ||
		container.Env[0].ValueFrom.SecretKeyRef.Name != "lake-manifest-controldb-app" || container.Env[0].ValueFrom.SecretKeyRef.Key != "uri" ||
		len(container.VolumeMounts) != 0 || len(migrateJob.Template.Spec.Volumes) != 0 {
		t.Fatalf("migrate container = %+v", container)
	}
	rule := migrateJob.PodFailurePolicy.Rules[0]
	if rule.Action != batchv1.PodFailurePolicyActionFailJob || !reflect.DeepEqual(rule.OnExitCodes.Values, []int32{1, 2}) {
		t.Fatalf("pod failure policy = %+v", migrateJob.PodFailurePolicy)
	}
	if migrateJob.Template.Labels[lakeInstanceLabel] != "manifest" {
		t.Fatal("the migrate pod lacks the Lake's sandbox identity, so it cannot reach the database")
	}

	setLakeSchemaAppliedForTest(t, h.c, h.key(ps.Name))
	h.reconcile()
	if h.deploymentImage() != "oramasearch/lake:v0.7.0" {
		t.Fatalf("deployment image = %q after the migration applied", h.deploymentImage())
	}
	if got := h.lake().Status.SchemaVersion; got != "image:"+testLakeManifestDigest {
		t.Fatalf("status.schemaVersion = %s", got)
	}
	t.Log("new image rolled after lake migrate applied")

	// 3. A steady Lake reads nothing again: the cached manifest is used and no Job is made.
	if err := h.c.Delete(h.ctx, h.manifestJob()); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if err := h.c.Get(h.ctx, h.key("lake-manifest-manifest"), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a cached manifest ran the Job again: %v", err)
	}
	t.Log("steady: manifest cached in status, no Job")
}

// A manifest Job that cannot run (a pull failure: the Job fails with no terminated container), or
// prints something the operator cannot trust, holds the image with a Degraded reason, and is tried
// again after lakeManifestRetry.
func TestLakeImageManifestFailureHoldsTheImageAndRetries(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "broken"}, Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:missing"}}
	h := newManifestHarness(t, lake)
	h.reconcile()
	job := h.manifestJob()
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded",
		Message: "Job was active longer than specified deadline", LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute))}}
	if err := h.c.Status().Update(h.ctx, job); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if reason := h.readyReason(); reason != "ImageManifestFailed" || h.lake().Status.Phase != "Degraded" {
		t.Fatalf("Ready reason %s, phase %s", reason, h.lake().Status.Phase)
	}
	if h.deploymentImage() != "" {
		t.Fatal("deployment created without a manifest")
	}
	h.manifestJob() // still there: not yet retried
	job = h.manifestJob()
	job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-lakeManifestRetry - time.Minute))
	if err := h.c.Status().Update(h.ctx, job); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if err := h.c.Get(h.ctx, h.key("lake-broken-manifest"), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("failed manifest Job not replaced after %s: %v", lakeManifestRetry, err)
	}
	h.reconcile()
	h.manifestJob()
	t.Log("failed manifest Job: Degraded, held, replaced after the retry interval")

	h.finishManifestPod(0, testLakeManifestImage, "{\"manifest_version\": 1, \"name\": \"lake\"}\n")
	h.reconcile()
	if reason := h.readyReason(); reason != "ImageManifestInvalid" || h.lake().Status.ImageManifest != nil {
		t.Fatalf("Ready reason %s, manifest %+v", reason, h.lake().Status.ImageManifest)
	}
	t.Log("invalid manifest: held with ImageManifestInvalid, nothing cached")
}

// With a manifest, the Lake's Stripe endpoint subscribes to the image's events and its catalog
// holds the image's products: adding either is a lake change only.
func TestLakePaymentsFollowTheImageManifest(t *testing.T) {
	lake := paymentsTestLake("manifest")
	manifest, err := parseLakeManifest(readTestFile(t, "testdata/lake-manifest-v1.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Image = lake.Spec.Image
	manifest.StripeWebhookEvents = append(manifest.StripeWebhookEvents, "charge.refunded")
	manifest.StripeProducts = append(manifest.StripeProducts, oramav1alpha1.LakeStripeProduct{ID: "lake_support", Name: "Lake Support"})
	lake.Status.ImageManifest = manifest
	h := newPaymentsHarness(t, lake, lakePaymentsTestSecret("lake-manifest", nil))
	h.reconcileLake()
	var endpoint oramav1alpha1.PaymentWebhookEndpoint
	if !h.exists("lake-manifest-payments", &endpoint) {
		t.Fatal("no PaymentWebhookEndpoint")
	}
	if !reflect.DeepEqual(endpoint.Spec.Events, manifest.StripeWebhookEvents) {
		t.Fatalf("endpoint events = %v", endpoint.Spec.Events)
	}
	var catalog oramav1alpha1.PaymentCatalog
	if !h.exists("lake-manifest-catalog", &catalog) {
		t.Fatal("no PaymentCatalog")
	}
	if last := catalog.Spec.Products[len(catalog.Spec.Products)-1]; len(catalog.Spec.Products) != 4 || last.ID != "lake_support" {
		t.Fatalf("catalog products = %v", catalog.Spec.Products)
	}
	t.Logf("endpoint: %d events from the manifest; catalog: %d products", len(endpoint.Spec.Events), len(catalog.Spec.Products))
}

// The restore rehearsal counts the image's guard tables and requires its migrations.
func TestRehearsalQueriesFollowTheImageManifest(t *testing.T) {
	lake := &oramav1alpha1.Lake{}
	if query, missing := lakeVerificationQueries(lake); query != postgresVerificationQuery || missing != postgresMissingMigrationsQuery {
		t.Fatal("a Lake without a manifest must use the vendored queries")
	}
	lake.Status.ImageManifest = &oramav1alpha1.LakeImageManifest{
		Supported: true, GuardTables: []string{"identity_core_guard", "new_feature_guard"}, Migrations: []string{"0001_identity", "0100_new_feature"},
	}
	query, missing := lakeVerificationQueries(lake)
	if !strings.Contains(query, "SELECT 'new_feature_guard=' || count(*) FROM new_feature_guard") ||
		strings.Contains(query, "account_balances_guard") || !strings.Contains(missing, "('0100_new_feature')") ||
		!strings.Contains(query, "'missing_migrations=' || ("+missing+")") {
		t.Fatalf("query = %s\nmissing = %s", query, missing)
	}
	job := verificationJobSpec(lake, "source", "", "secret", false)
	for _, env := range job.Template.Spec.Containers[0].Env {
		if (env.Name == "QUERY" && env.Value != query) || (env.Name == "MIGRATIONS_QUERY" && env.Value != missing) {
			t.Fatalf("verification Job %s = %s", env.Name, env.Value)
		}
	}
}
