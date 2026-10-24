/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testLakeAppServiceID = "8831658d-5691-4d40-b2cd-d52c651a8087"
	testLakeAppRevisionA = "8f1d6dfdbde090ba5b293a7439f975f5564b026a"
	testLakeAppRevisionB = "bd32b0a7b90b382852e5fcabf4b8fad00caf7954"
)

type lakeAppEventRecorder struct {
	mu     sync.Mutex
	events []lakeAppEvent
	topics []string
}

func (r *lakeAppEventRecorder) publish(_ context.Context, baseURL, token, topic string, event lakeAppEvent) error {
	if baseURL != "http://zetta.lake.svc:4800" || token != "events-token" {
		return &unexpectedLakeAppEventCredential{baseURL: baseURL, token: token}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	r.topics = append(r.topics, topic)
	return nil
}

func (r *lakeAppEventRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

type unexpectedLakeAppEventCredential struct {
	baseURL string
	token   string
}

func (e *unexpectedLakeAppEventCredential) Error() string {
	return "unexpected app event credential"
}

func newLakeAppScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		oramav1alpha1.AddToScheme,
		corev1.AddToScheme,
		appsv1.AddToScheme,
		networkingv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func TestSynchronizedAppsUseCollisionFreeWorkspaceResourceNames(t *testing.T) {
	workspaceNamespace := "prod-ws-123e4567e89b"
	first := &oramav1alpha1.LakeApp{
		Spec: oramav1alpha1.LakeAppSpec{
			Sync: &oramav1alpha1.LakeAppSync{
				ID:                 "11111111-1111-4111-8111-111111111111",
				WorkspaceNamespace: workspaceNamespace,
			},
		},
	}
	second := &oramav1alpha1.LakeApp{
		Spec: oramav1alpha1.LakeAppSpec{
			Sync: &oramav1alpha1.LakeAppSync{
				ID:                 "22222222-2222-4222-8222-222222222222",
				WorkspaceNamespace: workspaceNamespace,
			},
		},
	}
	if lakeAppNamespace(first) != workspaceNamespace || lakeAppNamespace(second) != workspaceNamespace {
		t.Fatal("synchronized apps did not select their shared workspace namespace")
	}
	if lakeAppWorkloadObjectName(first) == lakeAppWorkloadObjectName(second) {
		t.Fatal("synchronized app workload names collide")
	}
	if lakeAppTokensName(first) == lakeAppTokensName(second) {
		t.Fatal("synchronized app token Secret names collide")
	}
}

func TestLakeAppEnvtestLifecycle(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for LakeApp envtest")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	testEnvironment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{crdPath},
		CRDs:                  certManagerTestCRDs(),
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

	ctx := context.Background()
	scheme := newLakeAppScheme(t)
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	createLakeAppFixture(t, ctx, kubeClient)
	recorder := &lakeAppEventRecorder{}
	reconciler := &LakeAppReconciler{
		Client:         kubeClient,
		Scheme:         scheme,
		APIReader:      kubeClient,
		EventPublisher: recorder.publish,
	}
	key := client.ObjectKey{Namespace: "lake", Name: "sales-dashboard"}
	reconcileLakeAppTest(t, ctx, reconciler, key)
	reconcileLakeAppTest(t, ctx, reconciler, key)

	var app oramav1alpha1.LakeApp
	if err := kubeClient.Get(ctx, key, &app); err != nil {
		t.Fatal(err)
	}
	if app.Spec.Mode != oramav1alpha1.LakeAppModeRuntime || app.Spec.Port != 8080 || app.Spec.Auth != oramav1alpha1.LakeAppAuthLake {
		t.Fatalf("API defaults were not applied: %+v", app.Spec)
	}
	namespace := lakeAppNamespace(&app)
	assertLakeAppCreated(t, ctx, kubeClient, &app, namespace)
	if app.Status.Phase != oramav1alpha1.LakeAppPhaseProgressing || app.Status.ObservedRevision != testLakeAppRevisionA {
		t.Fatalf("initial status = %+v", app.Status)
	}
	if recorder.count() != 1 {
		t.Fatalf("initial event count = %d, want 1", recorder.count())
	}

	setLakeAppDeploymentReady(t, ctx, kubeClient, namespace, 1)
	reconcileLakeAppTest(t, ctx, reconciler, key)
	if err := kubeClient.Get(ctx, key, &app); err != nil {
		t.Fatal(err)
	}
	if app.Status.Phase != oramav1alpha1.LakeAppPhaseReady || !app.Status.Ready {
		t.Fatalf("ready status = %+v", app.Status)
	}
	if recorder.count() != 2 {
		t.Fatalf("ready event count = %d, want 2", recorder.count())
	}
	reconcileLakeAppTest(t, ctx, reconciler, key)
	if recorder.count() != 2 {
		t.Fatalf("steady reconcile published %d events, want 2", recorder.count())
	}

	var deployment appsv1.Deployment
	deploymentKey := client.ObjectKey{Namespace: namespace, Name: lakeAppWorkloadName}
	if err := kubeClient.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	credentialHash := deployment.Spec.Template.Annotations[lakeAppCredentialHashAnnotation]
	rotateLakeAppBinding(t, ctx, kubeClient)
	reconcileLakeAppTest(t, ctx, reconciler, key)
	if err := kubeClient.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Annotations[lakeAppCredentialHashAnnotation] == credentialHash {
		t.Fatal("binding rotation did not change the pod-template credential hash")
	}
	var binding corev1.Secret
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppBindingsSecretName}, &binding); err != nil {
		t.Fatal(err)
	}
	if string(binding.Data["zetta"]) != "workspace-token-2" {
		t.Fatalf("rotated binding = %q", binding.Data["zetta"])
	}

	updateLakeAppRevision(t, ctx, kubeClient, key, testLakeAppRevisionB)
	reconcileLakeAppTest(t, ctx, reconciler, key)
	assertLakeAppRevision(t, ctx, kubeClient, key, deploymentKey, testLakeAppRevisionB)
	setLakeAppDeploymentReady(t, ctx, kubeClient, namespace, 1)
	reconcileLakeAppTest(t, ctx, reconciler, key)
	updateLakeAppRevision(t, ctx, kubeClient, key, testLakeAppRevisionA)
	reconcileLakeAppTest(t, ctx, reconciler, key)
	assertLakeAppRevision(t, ctx, kubeClient, key, deploymentKey, testLakeAppRevisionA)

	createCrashLoopingLakeAppPod(t, ctx, kubeClient, &app, namespace)
	reconcileLakeAppTest(t, ctx, reconciler, key)
	if err := kubeClient.Get(ctx, key, &app); err != nil {
		t.Fatal(err)
	}
	if app.Status.Phase != oramav1alpha1.LakeAppPhaseDegraded || !strings.Contains(app.Status.Message, "crash-looping") {
		t.Fatalf("crash-loop status = %+v", app.Status)
	}

	if err := kubeClient.Delete(ctx, &app); err != nil {
		t.Fatal(err)
	}
	reconcileLakeAppTest(t, ctx, reconciler, key)
	finalizeLakeAppNamespaceForEnvtest(t, ctx, config, namespace)
	reconcileLakeAppTest(t, ctx, reconciler, key)
	if err := kubeClient.Get(ctx, key, &app); !apierrors.IsNotFound(err) {
		t.Fatalf("LakeApp after finalization: %v", err)
	}
	if recorder.count() < 4 {
		t.Fatalf("event count after lifecycle = %d, want rollout and teardown transitions", recorder.count())
	}
}

func createLakeAppFixture(t *testing.T, ctx context.Context, kubeClient client.Client) {
	t.Helper()
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake"}},
		&oramav1alpha1.Lake{
			ObjectMeta: metav1.ObjectMeta{Name: "prod"},
			Spec: oramav1alpha1.LakeSpec{
				Namespace:      "lake",
				Image:          "oramasearch/lake:test",
				PullSecretName: "docker-pull",
				Apps: oramav1alpha1.LakeAppsSpec{
					Wildcard:     "apps.example.test",
					IngressClass: "nginx",
					RuntimeImage: "oramasearch/app-runtime:0.1.0",
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "docker-pull", Namespace: "lake"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "dashboard-env", Namespace: "lake"},
			Data:       map[string][]byte{"FEATURE_FLAG": []byte("enabled")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "dashboard-source", Namespace: "lake"},
			Data:       map[string][]byte{"token": []byte("source-read-token"), "unrelated": []byte("must-not-copy")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "dashboard-bindings", Namespace: "lake"},
			Data: map[string][]byte{
				"zetta":              []byte("workspace-token-1"),
				testLakeAppServiceID: []byte(`{"url":"postgresql://app@service/app"}`),
				"not-requested":      []byte("must-not-copy"),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "dashboard-events", Namespace: "lake"},
			Data: map[string][]byte{
				"url":   []byte("http://zetta.lake.svc:4800"),
				"token": []byte("events-token"),
			},
		},
	}
	for _, object := range objects {
		if err := kubeClient.Create(ctx, object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	replicas := int32(1)
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "sales-dashboard", Namespace: "lake"},
		Spec: oramav1alpha1.LakeAppSpec{
			LakeRef: "prod",
			Source: &oramav1alpha1.LakeAppSource{
				Repository: "sales-analytics",
				Revision:   testLakeAppRevisionA,
				Path:       "apps/dashboard",
				Entrypoint: "main:app",
				Framework:  "fastapi",
				TokenSecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "dashboard-source"},
					Key:                  "token",
				},
			},
			Replicas:   &replicas,
			SecretRefs: []string{"dashboard-env"},
			Bindings: &oramav1alpha1.LakeAppBindings{
				Zetta: &oramav1alpha1.LakeAppZettaBinding{
					CustomerID: "50d440ef-d5e3-44df-8381-340ad2c3a0fb",
					Scopes:     []string{"datasets:read"},
				},
				Services:  []string{testLakeAppServiceID},
				SecretRef: &corev1.LocalObjectReference{Name: "dashboard-bindings"},
			},
			EventsSecretRef: &corev1.LocalObjectReference{Name: "dashboard-events"},
		},
	}
	if err := kubeClient.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
}

func reconcileLakeAppTest(t *testing.T, ctx context.Context, reconciler *LakeAppReconciler, key client.ObjectKey) {
	t.Helper()
	if _, err := reconciler.Reconcile(ctx, requestFor(key)); err != nil {
		t.Fatal(err)
	}
}

func requestFor(key client.ObjectKey) clientRequest {
	return ctrl.Request{NamespacedName: key}
}

type clientRequest = ctrl.Request

func assertLakeAppCreated(t *testing.T, ctx context.Context, kubeClient client.Client, app *oramav1alpha1.LakeApp, namespace string) {
	t.Helper()
	var workloadNamespace corev1.Namespace
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: namespace}, &workloadNamespace); err != nil {
		t.Fatal(err)
	}
	if workloadNamespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" || workloadNamespace.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
		t.Fatalf("app namespace metadata = labels %#v annotations %#v", workloadNamespace.Labels, workloadNamespace.Annotations)
	}

	var deployment appsv1.Deployment
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppWorkloadName}, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	container := pod.Containers[0]
	if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != lakeAppRunAsUser || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("pod hardening = pod %#v container %#v", pod.SecurityContext, container.SecurityContext)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != lakeAppPullSecretName {
		t.Fatalf("pod identity/pull settings = automount %v pull %#v", pod.AutomountServiceAccountToken, pod.ImagePullSecrets)
	}
	assertLakeAppEnv(t, container.Env, "APP_REVISION", testLakeAppRevisionA)
	assertLakeAppEnv(t, container.Env, "APP_TOKEN", "")
	if len(container.VolumeMounts) != 2 || container.VolumeMounts[1].MountPath != lakeAppBindingsPath {
		t.Fatalf("binding mounts = %#v", container.VolumeMounts)
	}

	var binding corev1.Secret
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppBindingsSecretName}, &binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Data) != 2 || string(binding.Data["zetta"]) != "workspace-token-1" || len(binding.Data[testLakeAppServiceID]) == 0 {
		t.Fatalf("binding delivery = %#v", binding.Data)
	}
	if _, leaked := binding.Data["not-requested"]; leaked {
		t.Fatal("binding delivery copied an unrequested key")
	}
	var sourceToken corev1.Secret
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppSourceSecretName}, &sourceToken); err != nil {
		t.Fatal(err)
	}
	if len(sourceToken.Data) != 1 || string(sourceToken.Data["token"]) != "source-read-token" {
		t.Fatalf("source token delivery = %#v", sourceToken.Data)
	}

	var access networkingv1.NetworkPolicy
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppNetworkAccessName}, &access); err != nil {
		t.Fatal(err)
	}
	if !lakeAppPolicyHasPodSelector(access.Spec.Egress, map[string]string{serviceIDLabel: testLakeAppServiceID}) {
		t.Fatalf("app access policy has no service-id target: %#v", access.Spec.Egress)
	}
	customer := serviceCustomerLabelValue(app.Spec.Bindings.Zetta.CustomerID)
	if !lakeAppPolicyHasPodSelector(access.Spec.Egress, map[string]string{serviceCustomerLabel: customer, serviceKindLabel: serviceKindZetta}) {
		t.Fatalf("app access policy has no scoped Zetta target: %#v", access.Spec.Egress)
	}

	var ingress networkingv1.Ingress
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppWorkloadName}, &ingress); err != nil {
		t.Fatal(err)
	}
	// lake#1132: the auth subrequest comes from ingress-nginx, so it uses Lake's proxy port.
	wantAuthURL := "http://lake-prod.lake.svc.cluster.local:8080/v1/apps/auth"
	if ingress.Spec.Rules[0].Host != "sales-dashboard.apps.example.test" || ingress.Annotations["nginx.ingress.kubernetes.io/auth-url"] != wantAuthURL {
		t.Fatalf("app ingress = host %q annotations %#v", ingress.Spec.Rules[0].Host, ingress.Annotations)
	}
}

func assertLakeAppEnv(t *testing.T, env []corev1.EnvVar, name, value string) {
	t.Helper()
	for _, variable := range env {
		if variable.Name != name {
			continue
		}
		if value != "" && variable.Value != value {
			t.Fatalf("%s = %q, want %q", name, variable.Value, value)
		}
		if value == "" && variable.ValueFrom == nil {
			t.Fatalf("%s is not sourced from a Secret", name)
		}
		return
	}
	t.Fatalf("environment variable %s not found", name)
}

func lakeAppPolicyHasPodSelector(rules []networkingv1.NetworkPolicyEgressRule, wanted map[string]string) bool {
	for _, rule := range rules {
		for _, peer := range rule.To {
			if peer.PodSelector == nil {
				continue
			}
			matches := true
			for key, value := range wanted {
				if peer.PodSelector.MatchLabels[key] != value {
					matches = false
				}
			}
			if matches {
				return true
			}
		}
	}
	return false
}

func setLakeAppDeploymentReady(t *testing.T, ctx context.Context, kubeClient client.Client, namespace string, replicas int32) {
	setNamedLakeAppDeploymentReady(t, ctx, kubeClient, namespace, lakeAppWorkloadName, replicas)
}

func setNamedLakeAppDeploymentReady(t *testing.T, ctx context.Context, kubeClient client.Client, namespace, name string, replicas int32) {
	t.Helper()
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := kubeClient.Get(ctx, key, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas = replicas
	deployment.Status.ReadyReplicas = replicas
	deployment.Status.UpdatedReplicas = replicas
	deployment.Status.AvailableReplicas = replicas
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
}

func rotateLakeAppBinding(t *testing.T, ctx context.Context, kubeClient client.Client) {
	t.Helper()
	var source corev1.Secret
	key := client.ObjectKey{Namespace: "lake", Name: "dashboard-bindings"}
	if err := kubeClient.Get(ctx, key, &source); err != nil {
		t.Fatal(err)
	}
	source.Data["zetta"] = []byte("workspace-token-2")
	if err := kubeClient.Update(ctx, &source); err != nil {
		t.Fatal(err)
	}
}

func updateLakeAppRevision(t *testing.T, ctx context.Context, kubeClient client.Client, key client.ObjectKey, revision string) {
	t.Helper()
	var app oramav1alpha1.LakeApp
	if err := kubeClient.Get(ctx, key, &app); err != nil {
		t.Fatal(err)
	}
	app.Spec.Source.Revision = revision
	if err := kubeClient.Update(ctx, &app); err != nil {
		t.Fatal(err)
	}
}

func assertLakeAppRevision(t *testing.T, ctx context.Context, kubeClient client.Client, appKey, deploymentKey client.ObjectKey, revision string) {
	t.Helper()
	var app oramav1alpha1.LakeApp
	if err := kubeClient.Get(ctx, appKey, &app); err != nil {
		t.Fatal(err)
	}
	if app.Status.ObservedRevision != revision {
		t.Fatalf("observed revision = %q, want %q", app.Status.ObservedRevision, revision)
	}
	var deployment appsv1.Deployment
	if err := kubeClient.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	assertLakeAppEnv(t, deployment.Spec.Template.Spec.Containers[0].Env, "APP_REVISION", revision)
}

func createCrashLoopingLakeAppPod(t *testing.T, ctx context.Context, kubeClient client.Client, app *oramav1alpha1.LakeApp, namespace string) {
	t.Helper()
	noEscalation := false
	nonRoot := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "app-crash",
			Namespace:   namespace,
			Labels:      lakeAppLabels(app),
			Annotations: map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)},
		},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &nonRoot,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "example.test/app:test",
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	if err := kubeClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff",
		}},
	}}
	if err := kubeClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func finalizeLakeAppNamespaceForEnvtest(t *testing.T, ctx context.Context, config *rest.Config, namespace string) {
	t.Helper()
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	ns.Spec.Finalizers = nil
	if _, err := clientset.CoreV1().Namespaces().Finalize(ctx, ns, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestPublishLakeAppEvent(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotAuth = request.Header.Get("Authorization")
		if request.URL.Path != "/v1/publish_event" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&gotBody); err != nil {
			t.Error(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"value":{"sequence":1}}`))
	}))
	defer server.Close()
	event := lakeAppEvent{Phase: oramav1alpha1.LakeAppPhaseReady, Ready: true, Revision: testLakeAppRevisionA, Message: "ready"}
	if err := publishLakeAppEvent(context.Background(), server.URL, "scoped-token", "app-events.sales-dashboard", event); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer scoped-token" || gotBody["topic"] != "app-events.sales-dashboard" || gotBody["encoding"] != "json" {
		t.Fatalf("publish request = auth %q body %#v", gotAuth, gotBody)
	}
	payload, ok := gotBody["payload"].(string)
	if !ok || !bytes.Contains([]byte(payload), []byte(`"phase":"Ready"`)) {
		t.Fatalf("publish payload = %#v", gotBody["payload"])
	}
}
