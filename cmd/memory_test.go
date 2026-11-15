// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// The Go runtime's soft limit is memoryLimitFraction of the container's limit, and nothing when
// GOMEMLIMIT is set or there is no container limit.
func TestGoMemoryLimitComesFromTheContainerLimit(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	got, err := goMemoryLimit(env(map[string]string{memoryLimitEnv: "268435456"}))
	if err != nil || got != 228170137 {
		t.Errorf("256Mi container limit: %d, %v; want 228170137 (85%%)", got, err)
	}
	if got, err := goMemoryLimit(env(map[string]string{memoryLimitEnv: "268435456", "GOMEMLIMIT": "200MiB"})); err != nil || got != 0 {
		t.Errorf("with GOMEMLIMIT set: %d, %v; want 0 (the runtime already has it)", got, err)
	}
	if got, err := goMemoryLimit(env(nil)); err != nil || got != 0 {
		t.Errorf("no container limit: %d, %v; want 0", got, err)
	}
	for _, bad := range []string{"256Mi", "-1", "0"} {
		if _, err := goMemoryLimit(env(map[string]string{memoryLimitEnv: bad})); err == nil {
			t.Errorf("%s=%q was accepted", memoryLimitEnv, bad)
		}
	}
}

// The chart gives the manager its own memory limit, unconditionally, so every cluster's operator
// sets its Go limit from the same number as its container limit.
func TestTheChartGivesTheManagerItsMemoryLimit(t *testing.T) {
	chart, err := loader.Load(filepath.Join("..", "chart"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValues(chart, map[string]interface{}{},
		chartutil.ReleaseOptions{Name: "orama-operator", Namespace: "orama-system"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chart, values)
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(rendered["orama-operator/templates/operator-deployment.yaml"]), &deployment); err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Resources.Limits.Memory().IsZero() {
		t.Fatal("the chart gives the manager no memory limit")
	}
	for _, variable := range container.Env {
		if variable.Name != memoryLimitEnv {
			continue
		}
		ref := variable.ValueFrom
		if ref == nil || ref.ResourceFieldRef == nil || ref.ResourceFieldRef.Resource != "limits.memory" ||
			ref.ResourceFieldRef.ContainerName != container.Name {
			t.Errorf("%s = %+v, want the %s container's limits.memory", memoryLimitEnv, variable, container.Name)
		}
		return
	}
	t.Errorf("the manager container has no %s", memoryLimitEnv)
}

// The cache holds every Secret but Helm's release revisions, and strips managedFields, against a
// real API server.
func TestTheCacheLeavesOutHelmReleaseSecretsEnvtest(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the cache envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	direct, err := client.New(config, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	secrets := []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.cert-manager.v1", Namespace: "default",
			Labels: map[string]string{"owner": "helm", "name": "cert-manager", "status": "deployed", "version": "1"}},
			Type: "helm.sh/release.v1", Data: map[string][]byte{"release": []byte("H4sI")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "stripe-testing", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "chart-made", Namespace: "default",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm", "owner": "someone"}}},
	}
	for _, secret := range secrets {
		if err := direct.Create(ctx, secret); err != nil {
			t.Fatal(err)
		}
	}

	options := managerCacheOptions("")
	options.Scheme = clientgoscheme.Scheme
	cached, err := cache.New(config, options)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = cached.Start(ctx) }()
	if _, err := cached.GetInformer(ctx, &corev1.Secret{}); err != nil {
		t.Fatal(err)
	}
	if !cached.WaitForCacheSync(ctx) {
		t.Fatal("the cache did not sync")
	}
	var got corev1.Secret
	for _, name := range []string{"stripe-testing", "chart-made"} {
		if err := cached.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
			t.Errorf("Secret %s is not in the cache: %v", name, err)
		} else if len(got.ManagedFields) != 0 {
			t.Errorf("Secret %s kept its managedFields in the cache", name)
		}
	}
	err = cached.Get(ctx, types.NamespacedName{Namespace: "default", Name: secrets[0].Name}, &got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("a Helm release Secret is in the cache: %v", err)
	}
	var list corev1.SecretList
	if err := cached.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	for _, secret := range list.Items {
		if secret.Labels["owner"] == "helm" {
			t.Errorf("the cache lists Helm release Secret %s", secret.Name)
		}
	}
}

// A local run confined to one namespace keeps the same Secret and managedFields rules.
func TestWatchNamespaceKeepsTheCacheRules(t *testing.T) {
	options := managerCacheOptions("demo")
	if _, ok := options.DefaultNamespaces["demo"]; !ok || len(options.DefaultNamespaces) != 1 {
		t.Errorf("DefaultNamespaces = %v, want only demo", options.DefaultNamespaces)
	}
	if options.DefaultTransform == nil || len(options.ByObject) != 1 {
		t.Errorf("WATCH_NAMESPACE dropped the Secret selector or the managedFields transform: %+v", options)
	}
	if cluster := managerCacheOptions(""); cluster.DefaultNamespaces != nil {
		t.Errorf("without WATCH_NAMESPACE the cache is confined to %v", cluster.DefaultNamespaces)
	}
}
