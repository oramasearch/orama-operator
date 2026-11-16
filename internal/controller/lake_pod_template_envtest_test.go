// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// startLakeEnvtest runs a real API server, so defaulting and the Deployment generation behave
// as they do in a cluster: the generation moves only when the stored spec changes.
func startLakeEnvtest(t *testing.T) client.Client {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for Lake pod-template envtest")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	testEnvironment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}
	config, err := testEnvironment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	kubeClient, err := client.New(config, client.Options{Scheme: newLakeScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return kubeClient
}

func lakeTemplateLabels(lake *oramav1alpha1.Lake) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "lake",
		"app.kubernetes.io/instance":   lake.Name,
		"app.kubernetes.io/managed-by": "orama-operator",
	}
}

func TestLakePodTemplateEnvtest(t *testing.T) {
	kubeClient := startLakeEnvtest(t)
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-template"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:test",
			Apps: oramav1alpha1.LakeAppsSpec{
				Wildcard:     "apps.example.com",
				RuntimeImage: "oramasearch/app-runtime:0.1.0",
			},
			Builds: oramav1alpha1.LakeBuildsSpec{},
			Store: oramav1alpha1.LakeStoreSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
	if err := kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake-dev"}}); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	reconciler := &LakeReconciler{Client: kubeClient, Scheme: kubeClient.Scheme()}
	labels := lakeTemplateLabels(lake)
	sealing := lakeSealingMaterial{KID: "test-kid", PublicKey: make([]byte, 32)}
	rwo := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	if _, err := reconciler.reconcileLakeDeployment(
		ctx, lake, "lake-dev", "lake-pod-template", lake.Spec.Image, "lake-pod-template-store",
		rwo, labels, 1, lakeDefaultPort, sealing, lakeClerkState{}, lakePaymentsState{},
	); err != nil {
		t.Fatal(err)
	}

	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: "lake-dev", Name: "lake-pod-template"}
	if err := kubeClient.Get(ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	// By default a single-replica RWO Lake recreates; the API server keeps the probes and the
	// sleep preStop action as rendered.
	assertLakeRecreates(t, deployment)
	assertLakeProbesAndDrain(t, deployment)
	securityContext := deployment.Spec.Template.Spec.SecurityContext
	if securityContext == nil || securityContext.FSGroupChangePolicy == nil ||
		*securityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("pod fsGroupChangePolicy = %+v, want %q", securityContext, corev1.FSGroupChangeOnRootMismatch)
	}
	assertEnvValueOnce(t, deployment, "LAKE_APPS_WILDCARD", "apps.example.com")
	assertEnvValueOnce(t, deployment, "LAKE_APPS_RUNTIME_IMAGE", "oramasearch/app-runtime:0.1.0")
	assertEnvValueOnce(t, deployment, "LAKE_INTERNAL_URL", "http://lake-pod-template.lake-dev.svc.cluster.local:80")
	assertLakeNdots(t, "lake", deployment.Spec.Template.Spec)

	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Apps.Wildcard = "managed.example.com"
	lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_APPS_WILDCARD", Value: "override.example.com"}}
	if err := kubeClient.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.reconcileLakeDeployment(
		ctx, lake, "lake-dev", "lake-pod-template", lake.Spec.Image, "lake-pod-template-store",
		rwo, labels, 1, lakeDefaultPort, sealing, lakeClerkState{}, lakePaymentsState{},
	); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	assertEnvValueOnce(t, deployment, "LAKE_APPS_WILDCARD", "override.example.com")
	assertLakeRecreates(t, deployment)

	// The opt-in turns the same Lake into a co-located surge, and the API server keeps it.
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Rollout.OverlapOnSharedStore = true
	if err := kubeClient.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.reconcileLakeDeployment(
		ctx, lake, "lake-dev", "lake-pod-template", lake.Spec.Image, "lake-pod-template-store",
		rwo, labels, 1, lakeDefaultPort, sealing, lakeClerkState{}, lakePaymentsState{},
	); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	assertLakeSurges(t, deployment)
	assertLakeProbesAndDrain(t, deployment)
	affinity := deployment.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.PodAffinity == nil ||
		len(affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 ||
		affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].TopologyKey != "kubernetes.io/hostname" {
		t.Fatalf("opted-in single-replica RWO Lake affinity = %+v, want the surge pod on the running pod's node", affinity)
	}
}

// TestLakePodsNdotsEnvtest is operator#276: lake's, agents' and apps' pods resolve with ndots:2, so
// a Service's full name is tried as given first, and the API server stores the option as rendered.
func TestLakePodsNdotsEnvtest(t *testing.T) {
	kubeClient := startLakeEnvtest(t)
	ctx := context.Background()
	if err := kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake-dns"}}); err != nil {
		t.Fatal(err)
	}
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "dns"},
		Spec: oramav1alpha1.LakeSpec{
			Image:  "oramasearch/lake:test",
			Agents: oramav1alpha1.LakeAgentsSpec{RuntimeImage: "oramasearch/agent-runtime:test"},
			Apps:   oramav1alpha1.LakeAppsSpec{RuntimeImage: "oramasearch/app-runtime:test"},
		},
	}
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "dns-app", Namespace: "lake-dns"},
		Spec:       oramav1alpha1.LakeAppSpec{Port: 8080},
	}
	pods := map[string]corev1.PodSpec{
		"agent-service": agentPodSpec(lake, "oramasearch/agent-ask-orama:test", agentBaseEnv(lake, "lake-dns"), true),
		"agent-job":     agentJobPod(lake, agentBaseEnv(lake, "lake-dns")),
		"app":           lakeAppPodSpec(lake, app, lakeAppSecrets{}),
	}
	for name, pod := range pods {
		labels := map[string]string{"app.kubernetes.io/name": name}
		pod.RestartPolicy = corev1.RestartPolicyAlways
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lake-dns"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: pod},
			},
		}
		if err := kubeClient.Create(ctx, deployment); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		stored := &appsv1.Deployment{}
		if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(deployment), stored); err != nil {
			t.Fatal(err)
		}
		assertLakeNdots(t, name, stored.Spec.Template.Spec)
	}
	for _, env := range agentBaseEnv(lake, "lake-dns") {
		if env.Name == "LAKE_URL" && env.Value != "http://lake-dns.lake-dns.svc.cluster.local:80" {
			t.Fatalf("agent LAKE_URL = %q, want the Service's full name", env.Value)
		}
	}
}

func assertLakeNdots(t *testing.T, name string, pod corev1.PodSpec) {
	t.Helper()
	if pod.DNSPolicy != "" && pod.DNSPolicy != corev1.DNSClusterFirst {
		t.Fatalf("%s dnsPolicy = %q, want ClusterFirst", name, pod.DNSPolicy)
	}
	config := pod.DNSConfig
	if config == nil || len(config.Nameservers) != 0 || len(config.Searches) != 0 || len(config.Options) != 1 ||
		config.Options[0].Name != "ndots" || config.Options[0].Value == nil || *config.Options[0].Value != "2" {
		t.Fatalf("%s dnsConfig = %+v, want only options [{ndots 2}]", name, config)
	}
}

// TestLakeSpecChangeRollsOnce is operator#243: patching spec.apps.runtimeImage must change the
// Deployment once. The store is created through reconcileLakeStore, so the rollout choice comes
// from the real claim, as in Reconcile.
func TestLakeSpecChangeRollsOnce(t *testing.T) {
	kubeClient := startLakeEnvtest(t)
	if err := kubeClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake-dev"}}); err != nil {
		t.Fatal(err)
	}
	t.Run("default Recreate", func(t *testing.T) {
		lakeSpecChangeRollsOnce(t, kubeClient, "one-rollout", false)
	})
	t.Run("opt-in overlap", func(t *testing.T) {
		lakeSpecChangeRollsOnce(t, kubeClient, "one-rollout-overlap", true)
	})
}

func lakeSpecChangeRollsOnce(t *testing.T, kubeClient client.Client, name string, overlap bool) {
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:test",
			Apps: oramav1alpha1.LakeAppsSpec{
				Wildcard:     "apps.example.com",
				RuntimeImage: "oramasearch/app-runtime:first",
			},
			Store: oramav1alpha1.LakeStoreSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
			Rollout: oramav1alpha1.LakeRolloutSpec{OverlapOnSharedStore: overlap},
		},
	}
	if err := kubeClient.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	reconciler := &LakeReconciler{Client: kubeClient, Scheme: kubeClient.Scheme()}
	labels := lakeTemplateLabels(lake)
	sealing := lakeSealingMaterial{KID: "test-kid", PublicKey: make([]byte, 32)}
	key := client.ObjectKey{Namespace: "lake-dev", Name: "lake-" + name}

	// reconcile runs the store and Deployment steps the way Reconcile does, from a fresh read of
	// the Lake, and returns the stored Deployment.
	reconcile := func() *appsv1.Deployment {
		t.Helper()
		current := &oramav1alpha1.Lake{}
		if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), current); err != nil {
			t.Fatal(err)
		}
		claimName, accessModes, err := reconciler.reconcileLakeStore(ctx, current, "lake-dev", labels)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.reconcileLakeDeployment(
			ctx, current, "lake-dev", key.Name, current.Spec.Image, claimName,
			accessModes, labels, 1, lakeDefaultPort, sealing, lakeClerkState{}, lakePaymentsState{},
		); err != nil {
			t.Fatal(err)
		}
		deployment := &appsv1.Deployment{}
		if err := kubeClient.Get(ctx, key, deployment); err != nil {
			t.Fatal(err)
		}
		return deployment
	}

	initial := reconcile()
	if overlap {
		assertLakeSurges(t, initial)
	} else {
		assertLakeRecreates(t, initial)
	}
	steady := reconcile()
	if steady.Generation != initial.Generation ||
		!apiequality.Semantic.DeepEqual(steady.Spec.Template, initial.Spec.Template) {
		t.Fatalf("a reconcile with no spec change moved the Deployment: generation %d -> %d",
			initial.Generation, steady.Generation)
	}

	current := &oramav1alpha1.Lake{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Apps.RuntimeImage = "oramasearch/app-runtime:second"
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}

	changed := reconcile()
	if changed.Generation != initial.Generation+1 {
		t.Fatalf("the spec change moved the Deployment generation %d -> %d, want exactly one step",
			initial.Generation, changed.Generation)
	}
	assertEnvValueOnce(t, changed, "LAKE_APPS_RUNTIME_IMAGE", "oramasearch/app-runtime:second")
	expected := initial.Spec.Template.DeepCopy()
	for i := range expected.Spec.Containers[0].Env {
		if expected.Spec.Containers[0].Env[i].Name == "LAKE_APPS_RUNTIME_IMAGE" {
			expected.Spec.Containers[0].Env[i].Value = "oramasearch/app-runtime:second"
		}
	}
	if !apiequality.Semantic.DeepEqual(*expected, changed.Spec.Template) {
		t.Fatalf("the spec change altered more of the pod template than LAKE_APPS_RUNTIME_IMAGE:\nwant %+v\ngot  %+v",
			expected, changed.Spec.Template)
	}

	// The second and later reconciles after the change leave the template alone: one rollout.
	for attempt := 0; attempt < 3; attempt++ {
		again := reconcile()
		if again.Generation != changed.Generation ||
			!apiequality.Semantic.DeepEqual(again.Spec.Template, changed.Spec.Template) {
			t.Fatalf("reconcile %d after the change moved the Deployment: generation %d -> %d",
				attempt+2, changed.Generation, again.Generation)
		}
	}
}

func assertEnvValueOnce(t *testing.T, deployment *appsv1.Deployment, name, want string) {
	t.Helper()
	count := 0
	value := ""
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		if variable.Name == name {
			count++
			value = variable.Value
		}
	}
	if count != 1 || value != want {
		t.Fatalf("%s count = %d, value = %q, want one value %q", name, count, value, want)
	}
}
