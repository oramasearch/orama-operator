/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for the user-supplied dashboard environment (operator#311).

const userDashboardEnv = "prod2-dashboard-env"

func userEnvSecret(data map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: userDashboardEnv, Namespace: "orama-system"}, StringData: data}
}

func userEnvFrom() []corev1.EnvFromSource {
	return []corev1.EnvFromSource{secretEnvFrom(userDashboardEnv)}
}

// dashboardWithUserEnv is a dashboard-only CR with ClickHouse Cloud analytics and the given user env.
func dashboardWithUserEnv(envFrom []corev1.EnvFromSource, env []corev1.EnvVar) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard = oramav1alpha1.DashboardSpec{
			ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true}, EnvFrom: envFrom, Env: env}
		o.Spec.Components.Analytics = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "external",
			Endpoint: "https://clickhouse.example:8443", CredentialsSecretRef: &corev1.LocalObjectReference{Name: "prod2-clickhouse"}}
	})
}

// reconcileDashboardOnly runs reconcileDashboard against a fake client holding objs.
func reconcileDashboardOnly(t *testing.T, oc *oramav1alpha1.OramaCloudV1, objs ...client.Object) (*corev1.Container, error) {
	t.Helper()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	if err := r.reconcileDashboard(context.Background(), oc); err != nil {
		return nil, err
	}
	return &getObj(t, cl, "v1-dashboard", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0], nil
}

func TestDashboardUserEnvFromIsLastEnvFromEntry(t *testing.T) {
	c, err := reconcileDashboardOnly(t, dashboardWithUserEnv(userEnvFrom(), nil), userEnvSecret(map[string]string{"CRON_SECRET": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	last := c.EnvFrom[len(c.EnvFrom)-1]
	if last.SecretRef == nil || last.SecretRef.Name != userDashboardEnv {
		t.Fatalf("last envFrom = %+v, want secretRef %s", last, userDashboardEnv)
	}
	if !slices.ContainsFunc(c.EnvFrom[:len(c.EnvFrom)-1], func(e corev1.EnvFromSource) bool {
		return e.SecretRef != nil && e.SecretRef.Name == "v1-dashboard-secret"
	}) {
		t.Errorf("operator envFrom sources missing before the user source: %+v", c.EnvFrom)
	}
}

func TestDashboardUserEnvComesLastAndReplacesClickHouseHost(t *testing.T) {
	proxy := corev1.EnvVar{Name: "CLICKHOUSE_HOST", Value: "https://ch-proxy.example"}
	extra := corev1.EnvVar{Name: "DATA_SOURCE_REQUEST_URL", Value: "https://data.example"}
	c, err := reconcileDashboardOnly(t, dashboardWithUserEnv(nil, []corev1.EnvVar{proxy, extra}))
	if err != nil {
		t.Fatal(err)
	}
	var hosts []corev1.EnvVar
	for _, e := range c.Env {
		if e.Name == "CLICKHOUSE_HOST" {
			hosts = append(hosts, e)
		}
	}
	if len(hosts) != 1 || hosts[0].Value != proxy.Value {
		t.Fatalf("CLICKHOUSE_HOST entries = %+v, want one with the user value", hosts)
	}
	if n := len(c.Env); n < 2 || c.Env[n-2] != proxy || c.Env[n-1] != extra {
		t.Errorf("user env is not last, in order: %+v", c.Env)
	}
	if !slices.ContainsFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "CLICKHOUSE_USER" }) {
		t.Error("operator env CLICKHOUSE_USER dropped")
	}
}

func TestDashboardUserEnvRefusesOperatorOwnedKeys(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "JWT_PRIVATE_KEY"} {
		_, err := reconcileDashboardOnly(t, dashboardWithUserEnv(nil, []corev1.EnvVar{{Name: name, Value: "x"}}))
		if err == nil || !strings.Contains(err.Error(), "dashboard.env sets "+name) || !strings.Contains(err.Error(), "operator owns") {
			t.Errorf("dashboard.env %s: err = %v, want a refusal naming it", name, err)
		}
	}
	// The same keys in an envFrom source are refused too, without echoing the value.
	_, err := reconcileDashboardOnly(t, dashboardWithUserEnv(userEnvFrom(), nil),
		userEnvSecret(map[string]string{"JWT_ISSUER": "https://app.orama.com", "CRON_SECRET": "x"}))
	if err == nil || !strings.Contains(err.Error(), userDashboardEnv) || !strings.Contains(err.Error(), "JWT_ISSUER") ||
		strings.Contains(err.Error(), "app.orama.com") {
		t.Errorf("envFrom with JWT_ISSUER: err = %v, want a refusal naming the Secret and key", err)
	}
}

func TestDashboardUserEnvSecretChangeRollsTemplate(t *testing.T) {
	ctx := context.Background()
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.EnvFrom = userEnvFrom() })
	cl, r := reconcileOCV1(t, oc, userEnvSecret(map[string]string{"STRIPE_SECRET_KEY": "sk_live_first"}))
	annotation := func() string {
		return getObj(t, cl, "v1-dashboard", &appsv1.Deployment{}).Spec.Template.Annotations[dashboardEnvHashAnnotation]
	}
	before := annotation()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(before) {
		t.Fatalf("annotation %q is not a sha256 hex hash", before)
	}

	secret := getObj(t, cl, userDashboardEnv, &corev1.Secret{})
	secret.StringData = nil
	secret.Data = map[string][]byte{"STRIPE_SECRET_KEY": []byte("sk_live_second")}
	if err := cl.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if reqs := r.requestsForDashboardEnv(ctx, secret); len(reqs) != 1 || reqs[0].Name != "v1" {
		t.Errorf("Secret change enqueues %v, want v1", reqs)
	}
	reconcileAgain(t, cl, r, nil)
	after := annotation()
	if after == before {
		t.Fatal("changing a key in the referenced Secret did not change the pod template annotation")
	}
	if strings.Contains(after, "sk_live") {
		t.Errorf("annotation carries a value: %q", after)
	}

	// A CR without user env gets no annotation (existing dashboards do not roll).
	plain, _ := reconcileOCV1(t, fullStackOCV1(nil))
	if _, has := getObj(t, plain, "v1-dashboard", &appsv1.Deployment{}).Spec.Template.Annotations[dashboardEnvHashAnnotation]; has {
		t.Error("dashboard without user env has an env-hash annotation")
	}
}

func TestDashboardMissingUserEnvSecretFailsReconcile(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.EnvFrom = userEnvFrom() })
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc).WithStatusSubresource(oc).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err == nil ||
		!strings.Contains(err.Error(), userDashboardEnv) {
		t.Fatalf("Reconcile err = %v, want one naming %s", err, userDashboardEnv)
	}
	ready := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, userDashboardEnv) {
		t.Fatalf("Ready = %+v, want False naming %s", ready, userDashboardEnv)
	}
}

// A seeded <cr>-dashboard-secret key that the user env provides is checked where the dashboard reads it.
func TestCredentialsReadyFollowsDashboardUserEnv(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.EnvFrom = userEnvFrom()
		o.Spec.Components.Dashboard.Env = []corev1.EnvVar{{Name: "SLACK_TOKEN", Value: "literal"}}
	})
	cl, _ := reconcileOCV1(t, oc, userEnvSecret(map[string]string{"STRIPE_SECRET_KEY": "sk_live_real", "RESEND_API_KEY": "dev-unset"}))
	cond := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, credentialsConditionType)
	if cond == nil {
		t.Fatal("no CredentialsReady condition")
	}
	for _, gone := range []string{"v1-dashboard-secret/STRIPE_SECRET_KEY", "v1-dashboard-secret/RESEND_API_KEY", "v1-dashboard-secret/SLACK_TOKEN"} {
		if strings.Contains(cond.Message, gone) {
			t.Errorf("CredentialsReady still names %s although the user env provides it: %s", gone, cond.Message)
		}
	}
	for _, want := range []string{userDashboardEnv + "/RESEND_API_KEY", "v1-dashboard-secret/STRIPE_WEBHOOK_SECRET"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("CredentialsReady does not name %s: %s", want, cond.Message)
		}
	}
}
