/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestDashboardPaymentCatalogReferenceRows(t *testing.T) {
	spec, err := dashboardPaymentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if problem := paymentCatalogProblem(spec); problem != "" {
		t.Fatal(problem)
	}
	if spec.App != "cloud_v1" || spec.Version < 1 || len(spec.Products) != 17 {
		t.Fatalf("unexpected catalog definition: %+v", spec)
	}
	rows := map[string]string{}
	for _, match := range regexp.MustCompile(`INSERT INTO reference_(subscription_plans|stripe_addon_prices).*'({.*})'`).FindAllStringSubmatch(dashboardReferenceRowsSQL, -1) {
		var row struct {
			ID        string `json:"id"`
			AddonType string `json:"addon_type"`
		}
		if err := json.Unmarshal([]byte(match[2]), &row); err != nil {
			t.Fatal(err)
		}
		if match[1] == "subscription_plans" {
			rows["cloud_v1.plan."+row.ID+".monthly"] = ""
			rows["cloud_v1.plan."+row.ID+".yearly"] = ""
		} else {
			rows["cloud_v1.addon."+row.ID] = row.AddonType
		}
	}
	if len(rows) != 35 {
		t.Fatalf("parsed %d reference price slots, want 35", len(rows))
	}
	omitted := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?m)^# (plan|addon) ([0-9a-f-]{36}): (.+)$`).FindAllStringSubmatch(string(dashboardPaymentCatalogYAML), -1) {
		keys := []string{"cloud_v1.addon." + match[2]}
		if match[1] == "plan" {
			keys = []string{"cloud_v1.plan." + match[2] + ".monthly", "cloud_v1.plan." + match[2] + ".yearly"}
		}
		for _, key := range keys {
			if _, ok := rows[key]; !ok {
				t.Errorf("omission names an unknown reference row: %s", key)
			}
			if omitted[key] {
				t.Errorf("duplicate omission: %s", key)
			}
			omitted[key] = true
		}
	}
	seen := map[string]bool{}
	for _, product := range spec.Products {
		if !strings.HasPrefix(product.ID, "cloud_v1_") {
			t.Errorf("unprefixed product: %s", product.ID)
		}
		for _, price := range product.Prices {
			addonType, ok := rows[price.LookupKey]
			if !ok {
				t.Errorf("lookup key names no reference row: %s", price.LookupKey)
			}
			if seen[price.LookupKey] || omitted[price.LookupKey] {
				t.Errorf("duplicate or omitted lookup key: %s", price.LookupKey)
			}
			seen[price.LookupKey] = true
			if addonType != "" && product.Metadata["addon_type"] != addonType {
				t.Errorf("%s lost addon_type %s", product.ID, addonType)
			}
			if price.Currency != "usd" || price.UnitAmount <= 0 {
				t.Errorf("invalid price: %+v", price)
			}
			interval := "month"
			if strings.HasSuffix(price.LookupKey, ".yearly") {
				interval = "year"
			}
			if price.Interval != interval {
				t.Errorf("wrong interval: %+v", price)
			}
		}
	}
	for key := range rows {
		if !seen[key] && !omitted[key] {
			t.Errorf("reference row has no price or header omission: %s", key)
		}
	}
	if len(seen) != 12 || len(omitted) != 23 {
		t.Errorf("price/omission slots = %d/%d, want 12/23", len(seen), len(omitted))
	}
	// These are the flat live amounts, including Build Full Analytics' $10 (the plan column says $9.99).
	amounts := map[string]int64{
		"cloud_v1.plan.28c01974-de01-40a5-be0b-d640f060bbfe.monthly": 10000,
		"cloud_v1.plan.28c01974-de01-40a5-be0b-d640f060bbfe.yearly":  100000,
		"cloud_v1.plan.46246ab8-4389-418f-a7f5-fb629cc659a6.monthly": 145000,
		"cloud_v1.plan.46246ab8-4389-418f-a7f5-fb629cc659a6.yearly":  1450000,
		"cloud_v1.addon.ff6f4595-ad00-4f22-8c96-cee67c93306e":        500,
		"cloud_v1.addon.3c3b49f4-c664-4273-8d85-08d140e84e1a":        500,
		"cloud_v1.addon.fb125974-3077-4c83-b9c9-c6f1735873b9":        500,
		"cloud_v1.addon.3a71548d-ccd5-4f94-a1e1-10e2a3d3a112":        1000,
		"cloud_v1.addon.6daeda62-cbb6-47ee-9e65-41e09ba363e7":        1000,
		"cloud_v1.addon.4d15ac28-b018-4dba-82fd-48e15d4080f5":        500,
		"cloud_v1.addon.c36ae842-1925-4c86-a2af-7ed590fba1be":        10000,
		"cloud_v1.addon.71bc6838-1321-4c1d-af2a-dc732c1bb6c6":        10000,
	}
	for _, product := range spec.Products {
		for _, price := range product.Prices {
			if price.UnitAmount != amounts[price.LookupKey] {
				t.Errorf("%s amount = %d, want %d", price.LookupKey, price.UnitAmount, amounts[price.LookupKey])
			}
		}
	}
}

func dashboardPaymentsOC() *oramav1alpha1.OramaCloudV1 {
	oc := dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.Host = "app.example.com"
		d.Payments = &oramav1alpha1.DashboardPaymentsSpec{SecretRef: corev1.LocalObjectReference{Name: testLakePaymentsSecret}}
	})
	oc.Spec.IDP.Provider = "none"
	return oc
}

// Run against the real API server; payment-controller status is supplied explicitly so readiness
// failures and last-known prices can be exercised without making any real Stripe request.
func TestDashboardPaymentsEnvtest(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	ctx, kube := startPaymentEnvtest(t, stripe, func(ctrl.Manager, *runtime.Scheme) []paymentReconciler { return nil })
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: kube.Scheme()}
	oc := dashboardPaymentsOC()
	oc.Namespace, oc.UID = "billing", ""
	oc.Spec.Cloudflare.APITokenSecretRef = &corev1.LocalObjectReference{Name: "cloudflare"}
	for _, obj := range []client.Object{oc, lakePaymentsTestSecret(oc.Namespace, nil), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testMasterSecret, Namespace: oc.Namespace}, Data: map[string][]byte{"MASTER_API_KEY": []byte(testMasterKey)}}} {
		if err := kube.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	reconcilePayments := func() *metav1.Condition {
		t.Helper()
		condition, err := r.reconcileDashboardPayments(ctx, oc)
		if err != nil {
			t.Fatal(err)
		}
		return condition
	}
	get := func(name string, obj client.Object) {
		t.Helper()
		if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, obj); err != nil {
			t.Fatal(err)
		}
	}
	dashboard := func() *appsv1.Deployment {
		t.Helper()
		if err := r.reconcileDashboard(ctx, oc); err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		get(oc.Name+"-dashboard", &dep)
		return &dep
	}
	assertStripeEnv := func(dep *appsv1.Deployment, want bool) {
		t.Helper()
		for _, name := range dashboardPaymentsEnvNames {
			var found []corev1.EnvVar
			for _, env := range dep.Spec.Template.Spec.Containers[0].Env {
				if env.Name == name {
					found = append(found, env)
				}
			}
			if want {
				if len(found) != 1 || found[0].ValueFrom == nil || found[0].ValueFrom.SecretKeyRef == nil {
					t.Fatalf("explicit %s = %+v", name, found)
				}
			} else if len(found) != 0 {
				t.Fatalf("premature %s = %+v", name, found)
			}
		}
	}
	t.Run("children and gated env", func(t *testing.T) {
		reconcilePayments()
		var provider oramav1alpha1.PaymentProvider
		var endpoint oramav1alpha1.PaymentWebhookEndpoint
		var catalog oramav1alpha1.PaymentCatalog
		get("v1-payments", &provider)
		get("v1-payments", &endpoint)
		get("v1-catalog", &catalog)
		for _, child := range []client.Object{&provider, &endpoint, &catalog} {
			if !metav1.IsControlledBy(child, oc) {
				t.Errorf("%T is not instance-owned", child)
			}
		}
		if provider.Spec.Stripe.Livemode || provider.Spec.Stripe.AccountID != testStripeAccount || provider.Spec.Stripe.APIKeySecretRef.Key != "operatorKey" {
			t.Errorf("wrong provider: %+v", provider.Spec)
		}
		wantEvents := []string{"customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted", "invoice.paid", "invoice.payment_failed", "payment_method.attached", "payment_method.detached", "checkout.session.completed"}
		if endpoint.Spec.URL != "https://app.example.com/api/webhooks/stripe" || endpoint.Spec.APIVersion != "2025-10-29.clover" || endpoint.Spec.App != "cloud_v1" || endpoint.Spec.SigningSecretName != "v1-payments-webhook" || !slices.Equal(endpoint.Spec.Events, wantEvents) {
			t.Errorf("wrong endpoint: %+v", endpoint.Spec)
		}
		want, err := dashboardPaymentCatalog()
		if err != nil {
			t.Fatal(err)
		}
		want.ProviderRef.Name = "v1-payments"
		if !reflect.DeepEqual(catalog.Spec, want) {
			t.Error("catalog differs from asset")
		}
		// Reserved explicit entries are ignored even when their references are missing.
		oc.Spec.Components.Dashboard.Env = []corev1.EnvVar{secretKeyEnv("STRIPE_SECRET_KEY", "absent", "appKey"), {Name: "STRIPE_WEBHOOK_SECRET", Value: "handmade"}}
		assertStripeEnv(dashboard(), false)
		catalog.Status.Prices = map[string]string{
			"cloud_v1.plan.28c01974-de01-40a5-be0b-d640f060bbfe.monthly": "price_build_monthly",
			"cloud_v1.plan.28c01974-de01-40a5-be0b-d640f060bbfe.yearly":  "price_build_yearly",
			"cloud_v1.addon.3c3b49f4-c664-4273-8d85-08d140e84e1a":        "price_extra_project",
		}
		catalog.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Configured", LastTransitionTime: metav1.Now(), ObservedGeneration: catalog.Generation}}
		if err := kube.Status().Update(ctx, &catalog); err != nil {
			t.Fatal(err)
		}
		assertStripeEnv(dashboard(), false)
	})
	t.Run("envFrom shadowing and secret rotations", func(t *testing.T) {
		source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-nonprod-dashboard-env", Namespace: oc.Namespace}, Data: map[string][]byte{"STRIPE_SECRET_KEY": []byte("handmade"), "STRIPE_WEBHOOK_SECRET": []byte("old_signing")}}
		signing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-payments-webhook", Namespace: oc.Namespace}, Data: map[string][]byte{"signingSecret": []byte("whsec_first")}}
		if err := kube.Create(ctx, source); err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(ctx, signing); err != nil {
			t.Fatal(err)
		}
		oc.Spec.Components.Dashboard.EnvFrom = []corev1.EnvFromSource{secretSource(source.Name)}
		dep := dashboard()
		assertStripeEnv(dep, true)
		// Resolve the container with the same precedence as Kubernetes; this Secret carries both names.
		effective := map[string]string{}
		container := dep.Spec.Template.Spec.Containers[0]
		for _, source := range container.EnvFrom {
			kind, name, _ := envFromRef(source)
			data, _, err := r.readEnvObject(ctx, oc.Namespace, kind, name)
			if err != nil {
				t.Fatal(err)
			}
			maps.Copy(effective, data)
		}
		for _, env := range container.Env {
			if kind, name, key, _, ok := envKeyRef(env); ok {
				data, _, err := r.readEnvObject(ctx, oc.Namespace, kind, name)
				if err != nil {
					t.Fatal(err)
				}
				effective[env.Name] = data[key]
			} else if env.ValueFrom == nil {
				effective[env.Name] = env.Value
			}
		}
		if effective["STRIPE_SECRET_KEY"] != testLakeAppKey || effective["STRIPE_WEBHOOK_SECRET"] != "whsec_first" {
			t.Fatalf("wrong effective env: Stripe sandbox keys did not win")
		}
		hash := dep.Spec.Template.Annotations[dashboardEnvHashAnnotation]
		signing.Data["signingSecret"] = []byte("whsec_rotated")
		if err := kube.Update(ctx, signing); err != nil {
			t.Fatal(err)
		}
		next := dashboard().Spec.Template.Annotations[dashboardEnvHashAnnotation]
		if next == "" || next == hash {
			t.Error("signing-secret rotation did not change dashboard env hash")
		}
		var keys corev1.Secret
		get(testLakePaymentsSecret, &keys)
		keys.Data["appKey"] = []byte("sk_test_rotated")
		if err := kube.Update(ctx, &keys); err != nil {
			t.Fatal(err)
		}
		if got := dashboard().Spec.Template.Annotations[dashboardEnvHashAnnotation]; got == next {
			t.Error("app-key rotation did not change dashboard env hash")
		}
		for _, name := range []string{testLakePaymentsSecret, signing.Name} {
			if !dashboardReferences(oc, "Secret", name) || len(r.requestsForDashboardEnv(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: oc.Namespace, Name: name}})) != 1 {
				t.Errorf("Secret %s does not enqueue dashboard", name)
			}
		}
	})
	t.Run("reference SQL keeps prices through catalog failures", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
				t.Fatal(err)
			}
			if i < 2 {
				var ps oramav1alpha1.PostgresSchema
				get(oc.Name+controlDBSchemaSuffixes[i], &ps)
				setSchemaStatus(t, kube, &ps, "Applied", ps.Spec.Version, ps.Generation)
			}
		}
		var cm corev1.ConfigMap
		get("v1-controldb-migrations", &cm)
		sql := cm.Data["reference-data.sql"]
		for _, price := range []string{"price_build_monthly", "price_build_yearly", "price_extra_project"} {
			if !strings.Contains(sql, price) {
				t.Errorf("reference SQL lacks %s", price)
			}
		}
		var reference oramav1alpha1.PostgresSchema
		get("v1-controldb-reference-data", &reference)
		version := reference.Spec.Version
		var catalog oramav1alpha1.PaymentCatalog
		get("v1-catalog", &catalog)
		catalog.Status.Conditions[0].Status, catalog.Status.Conditions[0].Reason = metav1.ConditionFalse, "StripeAPIError"
		if err := kube.Status().Update(ctx, &catalog); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
			t.Fatal(err)
		}
		get("v1-controldb-migrations", &cm)
		get("v1-controldb-reference-data", &reference)
		if cm.Data["reference-data.sql"] != sql || reference.Spec.Version != version {
			t.Error("transient catalog failure rewrote the reference SQL")
		}
		assertStripeEnv(dashboard(), true)
		catalog.Status.Prices["cloud_v1.plan.28c01974-de01-40a5-be0b-d640f060bbfe.monthly"] = "price_new_monthly"
		if err := kube.Status().Update(ctx, &catalog); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
			t.Fatal(err)
		}
		get("v1-controldb-reference-data", &reference)
		if reference.Spec.Version == version {
			t.Error("new price did not change the reference-data version")
		}
	})
	t.Run("live keys and conflicting explicit prices refused", func(t *testing.T) {
		for _, mutate := range []struct {
			name   string
			reason string
			apply  func(*oramav1alpha1.OramaCloudV1, *corev1.Secret)
		}{
			{"live-operator", "LiveModeRefused", func(_ *oramav1alpha1.OramaCloudV1, s *corev1.Secret) {
				s.Data["operatorKey"] = []byte("rk_live_refused")
			}},
			{"live-app", "LiveModeRefused", func(_ *oramav1alpha1.OramaCloudV1, s *corev1.Secret) { s.Data["appKey"] = []byte("sk_live_refused") }},
			{"explicit-prices", "InvalidSpec", func(o *oramav1alpha1.OramaCloudV1, _ *corev1.Secret) {
				o.Spec.Components.ControlDB.StripePrices.Plans = []oramav1alpha1.StripePlanPrice{{ID: "28c01974-de01-40a5-be0b-d640f060bbfe", Monthly: "manual"}}
			}},
		} {
			t.Run(mutate.name, func(t *testing.T) {
				other := dashboardPaymentsOC()
				other.Name, other.Namespace, other.UID = mutate.name, oc.Namespace, ""
				keys := lakePaymentsTestSecret(oc.Namespace, nil)
				keys.Name = mutate.name + "-keys"
				other.Spec.Components.Dashboard.Payments.SecretRef.Name = keys.Name
				mutate.apply(other, keys)
				if err := kube.Create(ctx, other); err != nil {
					t.Fatal(err)
				}
				if err := kube.Create(ctx, keys); err != nil {
					t.Fatal(err)
				}
				condition, err := r.reconcileDashboardPayments(ctx, other)
				if err != nil {
					t.Fatal(err)
				}
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != mutate.reason {
					t.Fatalf("refusal = %+v, want %s", condition, mutate.reason)
				}
				if mutate.reason == "InvalidSpec" {
					if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)}); err != nil {
						t.Fatal(err)
					}
					get(other.Name, other)
					stored := apimeta.FindStatusCondition(other.Status.Conditions, dashboardPaymentsCondition)
					if stored == nil || stored.Reason != "InvalidSpec" {
						t.Fatalf("controller did not publish refusal: %+v", other.Status.Conditions)
					}
				}
				for _, child := range []client.Object{&oramav1alpha1.PaymentProvider{}, &oramav1alpha1.PaymentWebhookEndpoint{}, &oramav1alpha1.PaymentCatalog{}} {
					name := other.Name + "-payments"
					if _, ok := child.(*oramav1alpha1.PaymentCatalog); ok {
						name = other.Name + "-catalog"
					}
					if err := kube.Get(ctx, client.ObjectKey{Namespace: other.Namespace, Name: name}, child); !apierrors.IsNotFound(err) {
						t.Errorf("refused configuration created %T: %v", child, err)
					}
				}
			})
		}
	})
	t.Run("removal orders env endpoint provider", func(t *testing.T) {
		var endpoint oramav1alpha1.PaymentWebhookEndpoint
		get("v1-payments", &endpoint)
		endpoint.Finalizers = []string{"test.orama.cloud/hold-endpoint"}
		if err := kube.Update(ctx, &endpoint); err != nil {
			t.Fatal(err)
		}
		oc.Spec.Components.Dashboard.Payments = nil
		reconcilePayments()
		get("v1-payments", &endpoint)
		if !endpoint.DeletionTimestamp.IsZero() {
			t.Fatal("endpoint deleted before env was withdrawn")
		}
		oc.Spec.Components.Dashboard.Env, oc.Spec.Components.Dashboard.EnvFrom = nil, nil
		assertStripeEnv(dashboard(), false)
		reconcilePayments()
		get("v1-payments", &endpoint)
		if endpoint.DeletionTimestamp.IsZero() {
			t.Fatal("endpoint was not deleted")
		}
		get("v1-payments", &oramav1alpha1.PaymentProvider{})
		// envtest has no garbage collector: remove the test hold and foreground finalizer explicitly.
		endpoint.Finalizers = nil
		if err := kube.Update(ctx, &endpoint); err != nil {
			t.Fatal(err)
		}
		reconcilePayments()
		if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: "v1-payments"}, &oramav1alpha1.PaymentProvider{}); !apierrors.IsNotFound(err) {
			t.Fatalf("provider remains after endpoint: %v", err)
		}
		if condition := reconcilePayments(); condition != nil {
			t.Errorf("condition after cleanup: %+v", condition)
		}
	})
}

func TestDashboardPaymentsMissingSecretDoesNotBlockReady(t *testing.T) {
	ctx := context.Background()
	oc := dashboardPaymentsOC()
	scheme := ocv1TestScheme(t)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v1-dashboard", Namespace: oc.Namespace, Labels: labelsFor(oc, "dashboard")},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1, Replicas: 1}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, deployment).WithStatusSubresource(oc).Build()
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
	condition, err := r.reconcileDashboardPayments(ctx, oc)
	if err != nil || condition == nil || condition.Reason != "SecretMissing" {
		t.Fatalf("missing secret: %+v, %v", condition, err)
	}
	r.setStatus(ctx, oc, oc.Status.Phase, oc.Status.ComponentsReady, oc.Status.Components, nil, *condition)
	r.applyReadiness(ctx, oc, nil)
	if !apimeta.IsStatusConditionTrue(oc.Status.Conditions, "Ready") || apimeta.IsStatusConditionTrue(oc.Status.Conditions, dashboardPaymentsCondition) {
		t.Fatalf("payments affected Ready: %+v", oc.Status.Conditions)
	}
}

func TestDashboardPaymentsUnsetPreservesExplicitPrices(t *testing.T) {
	ctx := context.Background()
	oc := dashOC(nil)
	oc.Spec.Components.ControlDB.StripePrices = oramav1alpha1.ControlDBStripePrices{Plans: []oramav1alpha1.StripePlanPrice{{ID: "28c01974-de01-40a5-be0b-d640f060bbfe", Monthly: "price_live_copied"}}}
	// A manually supplied Secret may share the generated name without opting into payments.
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v1-dashboard", Namespace: oc.Namespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "dashboard", Env: []corev1.EnvVar{secretKeyEnv("STRIPE_WEBHOOK_SECRET", "v1-payments-webhook", "signingSecret")},
		}}}}}}
	r, kube := newControlDBReconciler(t, oc, deployment)
	prices, err := r.dashboardStripePrices(ctx, oc)
	if err != nil || !reflect.DeepEqual(prices, oc.Spec.Components.ControlDB.StripePrices) {
		t.Fatalf("explicit prices changed: %+v, %v", prices, err)
	}
	condition, err := r.reconcileDashboardPayments(ctx, oc)
	if err != nil || condition != nil {
		t.Fatalf("unset payments: %+v, %v", condition, err)
	}
	for _, list := range []client.ObjectList{&oramav1alpha1.PaymentProviderList{}, &oramav1alpha1.PaymentCatalogList{}, &oramav1alpha1.PaymentWebhookEndpointList{}} {
		if err := kube.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		switch items := list.(type) {
		case *oramav1alpha1.PaymentProviderList:
			if len(items.Items) != 0 {
				t.Error("created provider without payments")
			}
		case *oramav1alpha1.PaymentCatalogList:
			if len(items.Items) != 0 {
				t.Error("created catalog without payments")
			}
		case *oramav1alpha1.PaymentWebhookEndpointList:
			if len(items.Items) != 0 {
				t.Error("created endpoint without payments")
			}
		}
	}
}
