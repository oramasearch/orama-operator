// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func assertCloudV1Identity(t *testing.T, pod corev1.PodSpec, name string) {
	t.Helper()
	if pod.ServiceAccountName != name || (pod.DeprecatedServiceAccount != "" && pod.DeprecatedServiceAccount != name) {
		t.Errorf("pod identity = %q/%q, want dedicated account %q", pod.ServiceAccountName, pod.DeprecatedServiceAccount, name)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("pod does not explicitly disable token automount")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Error("pod does not explicitly disable service links")
	}
}

func assertCloudV1Account(t *testing.T, c client.Client, owner client.Object, name string) {
	t.Helper()
	account := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, account); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(account, owner) || account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
		t.Errorf("account %s is not owned and tokenless: %+v", name, account)
	}
	for _, list := range []client.ObjectList{&rbacv1.RoleBindingList{}, &rbacv1.ClusterRoleBindingList{}} {
		if err := c.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		if meta.LenList(list) != 0 {
			t.Errorf("workload reconciliation created RBAC grants: %+v", list)
		}
	}
}

// Exercise actual builders, including updates of unsafe pre-existing templates and accounts.
func TestCloudV1IdentityWorkloadBuilders(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"app", "cache", "broker", "analytics", "search", "harvest-mongo", "embeddings", "gpu-embeddings", "writer", "writer-separated", "reader", "reader-separated", "reader-envoy"} {
		for _, upgrade := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/create", true: "/upgrade"}[upgrade], func(t *testing.T) {
				oc := engineOC(nil)
				component := path
				switch path {
				case "reader-envoy":
					component = "reader"
					oc.Spec.Components.Engine.Envoy = true
				case "gpu-embeddings":
					component = "embeddings"
					oc.Spec.Components.Engine.Writer.GPU = true
				case "writer-separated", "reader-separated":
					withSideImages(oc)
					component = path[:len(path)-len("-separated")]
				}
				name := oc.Name + "-" + component
				scheme := ocv1TestScheme(t)
				var obj client.Object
				pod := func() *corev1.PodSpec { return nil }
				if component == "app" || component == "embeddings" {
					dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
					obj, pod = dep, func() *corev1.PodSpec { return &dep.Spec.Template.Spec }
				} else {
					sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
					obj, pod = sts, func() *corev1.PodSpec { return &sts.Spec.Template.Spec }
				}
				objects := []client.Object{}
				if upgrade {
					pod().ServiceAccountName, pod().DeprecatedServiceAccount = "default", "default"
					pod().AutomountServiceAccountToken, pod().EnableServiceLinks = boolPtr(true), boolPtr(true)
					account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}, AutomountServiceAccountToken: boolPtr(true)}
					if err := controllerutil.SetControllerReference(oc, account, scheme); err != nil {
						t.Fatal(err)
					}
					objects = append(objects, obj, account)
				}
				c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
				r := &OramaCloudV1Reconciler{Client: c, Scheme: scheme}
				run := func() error {
					switch component {
					case "app":
						return r.reconcileApp(ctx, oc, appDef{component: component, image: "app:fixture", port: 8080})
					case "embeddings":
						return r.reconcileEmbeddings(ctx, oc)
					case "writer":
						return r.reconcileWriter(ctx, oc, "", "")
					case "reader":
						return r.reconcileReaders(ctx, oc, "", "", "")
					default:
						return r.provisionStatefulSet(ctx, oc, dataStoreDef{component: component, image: "store:fixture", dataPath: "/data"})
					}
				}
				if err := run(); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatal(err)
				}
				assertCloudV1Identity(t, *pod(), name)
				assertCloudV1Account(t, c, oc, name)
				before := obj.DeepCopyObject()
				// A lost account is restored without rolling an unchanged workload.
				account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
				if err := c.Delete(ctx, account); err != nil {
					t.Fatal(err)
				}
				if err := run(); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatal(err)
				}
				assertCloudV1Account(t, c, oc, name)
				if !reflect.DeepEqual(before, obj) {
					t.Error("steady reconciliation rewrote workload")
				}
			})
		}
	}
}

func TestCloudV1IdentityRejectsForeignAccount(t *testing.T) {
	oc := newOCV1(nil)
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "v1-app", Namespace: oc.Namespace}, AutomountServiceAccountToken: boolPtr(true)}
	scheme := ocv1TestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()
	r := &OramaCloudV1Reconciler{Client: c, Scheme: scheme}
	if err := r.reconcileApp(context.Background(), oc, appDef{component: "app", image: "fixture", port: 8080}); err == nil {
		t.Fatal("workload inherited a foreign account")
	}
	var deployments appsv1.DeploymentList
	if err := c.List(context.Background(), &deployments); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) != 0 {
		t.Fatal("workload created despite account conflict")
	}
}

func TestCloudV1IdentityCronJobs(t *testing.T) {
	oc := newOCV1(nil)
	scheme := ocv1TestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &OramaCloudV1Reconciler{Client: c, Scheme: scheme}
	// The callback cannot accidentally restore default identity or token settings.
	if err := r.upsertCronJob(context.Background(), oc, "v1-cron", "cron", "* * * * *", false, 60, func(pod *corev1.PodSpec) {
		pod.ServiceAccountName = "default"
		pod.AutomountServiceAccountToken, pod.EnableServiceLinks = boolPtr(true), boolPtr(true)
		pod.Containers = []corev1.Container{{Name: "cron", Image: "fixture"}}
	}); err != nil {
		t.Fatal(err)
	}
	var cron batchv1.CronJob
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: oc.Namespace, Name: "v1-cron"}, &cron); err != nil {
		t.Fatal(err)
	}
	assertCloudV1Identity(t, cron.Spec.JobTemplate.Spec.Template.Spec, cron.Name)
	assertCloudV1Account(t, c, oc, cron.Name)
}

func TestCloudV1IdentityOneShotJobs(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.DemoSeed = true })
	scheme := ocv1TestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &OramaCloudV1Reconciler{Client: c, Scheme: scheme}
	if err := r.reconcileDemoSeed(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	var seed batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: oc.Namespace, Name: "v1-demo-seed"}, &seed); err != nil {
		t.Fatal(err)
	}
	for _, job := range []*batchv1.Job{&seed, controlDBClusterRowJob(oc), analyticsRetentionJob(oc, "true", "fixture")} {
		t.Run(job.Name, func(t *testing.T) { assertCloudV1Identity(t, job.Spec.Template.Spec, job.Name) })
	}
	assertCloudV1Account(t, c, oc, seed.Name)
	m := migrationTestResource(nil)
	h := newMigrationHarness(t, m, migrationStack(t, nil)...)
	h.reconcile()
	job := h.job("team-7e57a000-seed-0")
	assertCloudV1Identity(t, job.Spec.Template.Spec, job.Name)
	assertCloudV1Account(t, h.kube, m, job.Name)
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: "schema"}}
	assertCloudV1Identity(t, pgMigrationJobSpec(ps).Template.Spec, "schema-migrate")

	test := &oramav1alpha1.OramaCloudV1E2ETest{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: oc.Namespace, UID: "probe"}, Spec: oramav1alpha1.OramaCloudV1E2ETestSpec{TargetRef: oc.Name}}
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(test, oc).WithStatusSubresource(test).Build()
	e2e := &OramaCloudV1E2ETestReconciler{Client: c, Scheme: scheme}
	if _, err := e2e.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(test)}); err != nil {
		t.Fatal(err)
	}
	var probe batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: oc.Namespace, Name: "probe-e2e"}, &probe); err != nil {
		t.Fatal(err)
	}
	assertCloudV1Identity(t, probe.Spec.Template.Spec, probe.Name)
	assertCloudV1Account(t, c, test, probe.Name)
}

// Check admission with real ServiceAccount defaults and verify that a steady pass writes nothing.
func TestCloudV1IdentityAgainstAPIServer(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for pod identity admission proof")
	}
	server := &envtest.Environment{BinaryAssetsDirectory: assets}
	cfg, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := ocv1TestScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Namespace = "identity"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: oc.Namespace}}); err != nil {
		t.Fatal(err)
	}
	r := &OramaCloudV1Reconciler{Client: c, Scheme: scheme}
	run := func() {
		t.Helper()
		if err := r.upsertCronJob(ctx, oc, "v1-cron", "cron", "* * * * *", false, 60, func(pod *corev1.PodSpec) {
			ctr := onlyContainer(pod, "cron")
			ctr.Image = "fixture:local"
		}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	var cron batchv1.CronJob
	key := client.ObjectKey{Namespace: oc.Namespace, Name: "v1-cron"}
	if err := c.Get(ctx, key, &cron); err != nil {
		t.Fatal(err)
	}
	before := cron.ResourceVersion
	run()
	if err := c.Get(ctx, key, &cron); err != nil {
		t.Fatal(err)
	}
	if cron.ResourceVersion != before {
		t.Error("steady reconciliation rewrote admitted CronJob")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "identity", Namespace: oc.Namespace}, Spec: cron.Spec.JobTemplate.Spec.Template.Spec}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	assertCloudV1Identity(t, pod.Spec, key.Name)
	for _, volume := range pod.Spec.Volumes {
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil {
					t.Error("admission projected a service-account token")
				}
			}
		}
	}
}

func TestCloudV1IdentityPreservesExistingJobs(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.DemoSeed = true })
	scheme := ocv1TestScheme(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "v1-demo-seed", Namespace: oc.Namespace}}
	job.Spec.Template.Spec = corev1.PodSpec{ServiceAccountName: "default", AutomountServiceAccountToken: boolPtr(true)}
	if err := controllerutil.SetControllerReference(oc, job, scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	var before batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &before); err != nil {
		t.Fatal(err)
	}
	r := &OramaCloudV1Reconciler{Client: c, Scheme: scheme}
	if err := r.reconcileDemoSeed(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), job); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&before, job) {
		t.Fatal("existing one-shot Job changed during identity rollout")
	}

	// The cluster-row Job's existing fingerprint must survive an identity-only rollout, too.
	row := controlDBClusterRowJob(oc)
	old := row.Spec.Template.DeepCopy()
	old.Spec.ServiceAccountName = ""
	old.Spec.EnableServiceLinks = nil
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if row.Annotations[clusterRowHashKey] != hex.EncodeToString(sum[:]) {
		t.Fatal("identity-only change would replace and replay a cluster-row Job")
	}
}
