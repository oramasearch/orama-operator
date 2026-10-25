/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// testLakeAppKey is Lake's key in the payments Secret; testStripeKey is the operator's.
	testLakeAppKey         = "rk_test_51LakeAppKey"
	testLakePaymentsSecret = "stripe-testing"
)

func paymentsTestLake(name string) *oramav1alpha1.Lake {
	one := int32(1)
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake-" + name, Image: "oramasearch/lake:test", Replicas: &one, Host: name + ".lake.example",
			Env: []corev1.EnvVar{
				{Name: "LAKE_STRIPE_SECRET_KEY", Value: "sk_test_handmade"},
				{Name: "LAKE_STRIPE_WEBHOOK_SECRET", Value: "whsec_handmade"},
			},
			Billing:  &oramav1alpha1.LakeBillingSpec{Enabled: true},
			Payments: &oramav1alpha1.LakePaymentsSpec{Mode: "test", SecretRef: corev1.LocalObjectReference{Name: testLakePaymentsSecret}},
		},
	}
}

// lakePaymentsTestSecret is a payments Secret with the fake Stripe's account and keys; data
// replaces or, with an empty value, removes keys.
func lakePaymentsTestSecret(ns string, data map[string]string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testLakePaymentsSecret, Namespace: ns},
		Data: map[string][]byte{
			"accountID": []byte(testStripeAccount), "operatorKey": []byte(testStripeKey), "appKey": []byte(testLakeAppKey),
		},
	}
	for key, value := range data {
		if value == "" {
			delete(secret.Data, key)
		} else {
			secret.Data[key] = []byte(value)
		}
	}
	return secret
}

// paymentsHarness runs a Lake's reconciler and the five payment reconcilers on one fake client
// against the fake Stripe, each step by hand, as the manager would in turn.
type paymentsHarness struct {
	t      *testing.T
	ctx    context.Context
	c      client.Client
	stripe *fakeStripe
	r      *LakeReconciler
	name   string
	ns     string

	provider                         *PaymentProviderReconciler
	stripeEndpoint                   *StripeWebhookEndpointReconciler
	endpoint, catalog, stripeCatalog reconcile.Reconciler
}

func newPaymentsHarness(t *testing.T, lake *oramav1alpha1.Lake, objects ...client.Object) *paymentsHarness {
	t.Helper()
	s := newLakeScheme(t)
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.acceptKey(testLakeAppKey, testStripeAccount)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objects, lake)...).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &oramav1alpha1.PaymentProvider{},
			&oramav1alpha1.PaymentWebhookEndpoint{}, &oramav1alpha1.StripeWebhookEndpoint{},
			&oramav1alpha1.PaymentCatalog{}, &oramav1alpha1.StripeCatalog{}).
		Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	return &paymentsHarness{
		t: t, ctx: context.Background(), c: c, stripe: stripe, name: lake.Name, ns: lake.Spec.Namespace,
		r:              &LakeReconciler{Client: c, Scheme: s, StripeAPIBase: stripe.url},
		provider:       &PaymentProviderReconciler{Client: c, Scheme: s, StripeAPIBase: stripe.url},
		endpoint:       &PaymentWebhookEndpointReconciler{Client: c, Scheme: s},
		stripeEndpoint: &StripeWebhookEndpointReconciler{Client: c, Scheme: s, StripeAPIBase: stripe.url},
		catalog:        &PaymentCatalogReconciler{Client: c, Scheme: s},
		stripeCatalog:  &StripeCatalogReconciler{Client: c, Scheme: s, StripeAPIBase: stripe.url},
	}
}

func (h *paymentsHarness) run(r reconcile.Reconciler, key types.NamespacedName) {
	h.t.Helper()
	if _, err := r.Reconcile(h.ctx, ctrl.Request{NamespacedName: key}); err != nil {
		h.t.Fatalf("reconcile %s: %v", key, err)
	}
}

func (h *paymentsHarness) reconcileLake() {
	h.t.Helper()
	h.run(h.r, types.NamespacedName{Name: h.name})
}

func (h *paymentsHarness) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: h.ns, Name: name}
}

// runProvider checks the provider's key.
func (h *paymentsHarness) runProvider() {
	h.t.Helper()
	h.run(h.provider, h.key("lake-"+h.name+"-payments"))
}

// runEndpoint resolves the abstract endpoint, configures the Stripe one and copies its Ready back.
func (h *paymentsHarness) runEndpoint() {
	h.t.Helper()
	key := h.key("lake-" + h.name + "-payments")
	h.run(h.endpoint, key)
	h.run(h.stripeEndpoint, key)
	h.run(h.endpoint, key)
}

// runCatalog resolves the abstract catalog, configures the Stripe one and copies its Ready back.
func (h *paymentsHarness) runCatalog() {
	h.t.Helper()
	key := h.key("lake-" + h.name + "-catalog")
	h.run(h.catalog, key)
	h.run(h.stripeCatalog, key)
	h.run(h.catalog, key)
}

// configure brings the Lake's payments to Configured.
func (h *paymentsHarness) configure() {
	h.t.Helper()
	h.reconcileLake()
	h.runProvider()
	h.runEndpoint()
	h.runCatalog()
	h.reconcileLake()
	if condition := h.condition(); condition == nil || condition.Reason != "Configured" {
		h.t.Fatalf("PaymentsReady = %+v, want Configured", condition)
	}
}

func (h *paymentsHarness) deployment() *appsv1.Deployment {
	h.t.Helper()
	var deployment appsv1.Deployment
	if err := h.c.Get(h.ctx, h.key("lake-"+h.name), &deployment); err != nil {
		h.t.Fatal(err)
	}
	return &deployment
}

// stripeEnv is the Deployment's entries with a Stripe name, in order.
func (h *paymentsHarness) stripeEnv() []corev1.EnvVar {
	h.t.Helper()
	var env []corev1.EnvVar
	for _, variable := range h.deployment().Spec.Template.Spec.Containers[0].Env {
		if slices.Contains(lakePaymentsEnvNames, variable.Name) {
			env = append(env, variable)
		}
	}
	return env
}

func (h *paymentsHarness) hash() string {
	h.t.Helper()
	return h.deployment().Spec.Template.Annotations[lakePaymentsSecretHashKey]
}

func (h *paymentsHarness) condition() *metav1.Condition {
	h.t.Helper()
	var lake oramav1alpha1.Lake
	if err := h.c.Get(h.ctx, types.NamespacedName{Name: h.name}, &lake); err != nil {
		h.t.Fatal(err)
	}
	return apimeta.FindStatusCondition(lake.Status.Conditions, lakePaymentsConditionType)
}

func (h *paymentsHarness) expectReason(reason string) {
	h.t.Helper()
	if condition := h.condition(); condition == nil || condition.Reason != reason {
		h.t.Fatalf("PaymentsReady = %+v, want reason %s", condition, reason)
	}
}

func (h *paymentsHarness) signingSecret() string {
	h.t.Helper()
	var secret corev1.Secret
	if err := h.c.Get(h.ctx, h.key("lake-"+h.name+"-payments-webhook"), &secret); err != nil {
		h.t.Fatal(err)
	}
	return string(secret.Data["signingSecret"])
}

// exists is whether the object at name exists, being deleted or not.
func (h *paymentsHarness) exists(name string, object client.Object) bool {
	h.t.Helper()
	err := h.c.Get(h.ctx, h.key(name), object)
	if err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatal(err)
	}
	return err == nil
}

func (h *paymentsHarness) updatePaymentsSecret(data map[string]string) {
	h.t.Helper()
	var secret corev1.Secret
	if err := h.c.Get(h.ctx, h.key(testLakePaymentsSecret), &secret); err != nil {
		h.t.Fatal(err)
	}
	secret.Data = lakePaymentsTestSecret(h.ns, data).Data
	if err := h.c.Update(h.ctx, &secret); err != nil {
		h.t.Fatal(err)
	}
}

func lakePaymentsTestEnv(signingSecretName string) []corev1.EnvVar {
	ref := func(name, key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
	}
	return []corev1.EnvVar{
		{Name: "LAKE_STRIPE_SECRET_KEY", ValueFrom: ref(testLakePaymentsSecret, "appKey")},
		{Name: "LAKE_STRIPE_WEBHOOK_SECRET", ValueFrom: ref(signingSecretName, "signingSecret")},
	}
}

// The three children appear with the issue's fields at once, and the Lake gets no Stripe variable
// (spec.env's are dropped) until the provider, the endpoint and its signing Secret, the catalog and
// the check of Lake's own key are all in place; then it gets both from the right Secrets. A steady
// Lake then asks Stripe nothing and writes nothing.
func TestLakePaymentsGiveTheStripeEnvOnlyOnceEverythingIsInPlace(t *testing.T) {
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", nil))
	h.reconcileLake()

	var provider oramav1alpha1.PaymentProvider
	if !h.exists("lake-testing-payments", &provider) {
		t.Fatal("no PaymentProvider")
	}
	wantProvider := oramav1alpha1.PaymentProviderSpec{Type: "stripe", Stripe: &oramav1alpha1.StripePaymentProviderSpec{
		AccountID: testStripeAccount, Livemode: false,
		APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: testLakePaymentsSecret, Key: "operatorKey"},
	}}
	if !reflect.DeepEqual(provider.Spec, wantProvider) || provider.OwnerReferences[0].Name != "testing" {
		t.Errorf("provider = %+v owners %v", provider.Spec, provider.OwnerReferences)
	}
	var endpoint oramav1alpha1.PaymentWebhookEndpoint
	if !h.exists("lake-testing-payments", &endpoint) {
		t.Fatal("no PaymentWebhookEndpoint")
	}
	wantEndpoint := oramav1alpha1.PaymentWebhookEndpointSpec{
		ProviderRef: corev1.LocalObjectReference{Name: "lake-testing-payments"},
		URL:         "https://testing.lake.example/billing/stripe/webhook",
		Events: []string{
			"checkout.session.async_payment_failed", "checkout.session.async_payment_succeeded", "checkout.session.completed",
			"checkout.session.expired", "customer.subscription.created", "customer.subscription.deleted",
			"customer.subscription.paused", "customer.subscription.resumed", "customer.subscription.updated",
			"invoice.marked_uncollectible", "invoice.paid", "invoice.payment_action_required", "invoice.payment_failed",
			"invoice.updated", "invoice.voided", "payment_intent.payment_failed", "payment_intent.succeeded",
			"setup_intent.succeeded",
		},
		APIVersion: "2024-06-20", App: "lake", SigningSecretName: "lake-testing-payments-webhook",
	}
	if !reflect.DeepEqual(endpoint.Spec, wantEndpoint) || endpoint.OwnerReferences[0].Name != "testing" {
		t.Errorf("endpoint = %+v owners %v", endpoint.Spec, endpoint.OwnerReferences)
	}
	var catalog oramav1alpha1.PaymentCatalog
	if !h.exists("lake-testing-catalog", &catalog) {
		t.Fatal("no PaymentCatalog")
	}
	wantCatalog := oramav1alpha1.PaymentCatalogSpec{
		ProviderRef: corev1.LocalObjectReference{Name: "lake-testing-payments"}, App: "lake", Version: 1,
		Products: []oramav1alpha1.PaymentProduct{
			{ID: "lake_usage", Name: "Lake usage"},
			{ID: "lake_private_warehouse", Name: "Lake Private Warehouse"},
			{ID: "lake_dedicated_cluster", Name: "Lake Dedicated Cluster"},
		},
	}
	if !reflect.DeepEqual(catalog.Spec, wantCatalog) || catalog.OwnerReferences[0].Name != "testing" {
		t.Errorf("catalog = %+v owners %v", catalog.Spec, catalog.OwnerReferences)
	}

	noEnv := func(when string) {
		t.Helper()
		if env := h.stripeEnv(); len(env) != 0 || h.hash() != "" {
			t.Fatalf("%s: Stripe env %+v, hash %q", when, env, h.hash())
		}
	}
	noEnv("before the provider checked its key")
	h.expectReason("ProviderNotReady")

	h.runProvider()
	h.reconcileLake()
	noEnv("before the endpoint exists")
	h.expectReason("Progressing")

	h.runEndpoint()
	h.reconcileLake()
	if h.signingSecret() == "" {
		t.Fatal("the endpoint wrote no signing secret")
	}
	noEnv("with the signing Secret but no catalog")
	h.expectReason("Progressing")
	if n := h.stripe.count("GET /v1/products/lake_usage"); n != 0 {
		t.Fatalf("Lake's key was checked before the catalog was Ready (%d requests)", n)
	}

	h.runCatalog()
	checks := h.stripe.count("GET /v1/products/lake_usage")
	h.reconcileLake()
	if got := h.stripe.count("GET /v1/products/lake_usage") - checks; got != 1 {
		t.Errorf("Lake's key was checked %d times, want once", got)
	}
	if env, want := h.stripeEnv(), lakePaymentsTestEnv("lake-testing-payments-webhook"); !reflect.DeepEqual(env, want) {
		t.Errorf("Stripe env = %+v, want %+v (spec.env's values dropped)", env, want)
	}
	if got, want := h.hash(), lakePaymentsHash(testLakeAppKey, h.signingSecret()); got != want {
		t.Errorf("payments-secret-hash = %q, want %q", got, want)
	}
	h.expectReason("Configured")
	if condition := h.condition(); condition.Status != metav1.ConditionTrue {
		t.Errorf("PaymentsReady = %+v", condition)
	}

	// Steady: no request to Stripe and no write.
	requests, deployment := len(h.stripe.received()), h.deployment().ResourceVersion
	var before oramav1alpha1.Lake
	if err := h.c.Get(h.ctx, types.NamespacedName{Name: "testing"}, &before); err != nil {
		t.Fatal(err)
	}
	h.reconcileLake()
	var after oramav1alpha1.Lake
	if err := h.c.Get(h.ctx, types.NamespacedName{Name: "testing"}, &after); err != nil {
		t.Fatal(err)
	}
	if len(h.stripe.received()) != requests || h.deployment().ResourceVersion != deployment || after.ResourceVersion != before.ResourceVersion {
		t.Errorf("a steady Lake asked Stripe %d times or wrote (Deployment %s→%s, Lake %s→%s)", len(h.stripe.received())-requests,
			deployment, h.deployment().ResourceVersion, before.ResourceVersion, after.ResourceVersion)
	}
}

// With spec.payments both Stripe names belong to the operator and spec.env's entries are dropped;
// without it spec.env keeps them.
func TestLakeEnvDropsSpecEnvStripeVariablesOnlyWithPayments(t *testing.T) {
	lake := paymentsTestLake("testing")
	lake.Spec.Env = append(lake.Spec.Env, corev1.EnvVar{Name: "LAKE_LOG", Value: "debug"})
	names := func() []string {
		var found []string
		for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
			if slices.Contains(lakePaymentsEnvNames, variable.Name) || variable.Name == "LAKE_LOG" {
				found = append(found, variable.Name+"="+variable.Value)
			}
		}
		return found
	}
	if got := names(); !reflect.DeepEqual(got, []string{"LAKE_LOG=debug"}) {
		t.Errorf("with spec.payments: %v", got)
	}
	lake.Spec.Payments = nil
	if got, want := names(), []string{"LAKE_STRIPE_SECRET_KEY=sk_test_handmade", "LAKE_STRIPE_WEBHOOK_SECRET=whsec_handmade", "LAKE_LOG=debug"}; !reflect.DeepEqual(got, want) {
		t.Errorf("without spec.payments: %v, want %v", got, want)
	}
}

// Without its Secret a Lake with spec.payments reports SecretMissing, makes no payment object and
// asks Stripe nothing, and stays Ready, writing no status while it waits.
func TestLakePaymentsWithoutTheSecretWaitAndTheLakeStaysReady(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.database(1, 1)
	f.reconcile()
	f.expectState("Ready", "LakeReady")

	f.lake.Spec.Billing = &oramav1alpha1.LakeBillingSpec{Enabled: true}
	f.lake.Spec.Payments = &oramav1alpha1.LakePaymentsSpec{Mode: "test", SecretRef: corev1.LocalObjectReference{Name: "stripe-testing"}}
	if err := f.client.Update(f.ctx, f.lake); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	condition := apimeta.FindStatusCondition(f.lake.Status.Conditions, lakePaymentsConditionType)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "SecretMissing" {
		t.Fatalf("PaymentsReady = %+v", condition)
	}
	for _, list := range []client.ObjectList{
		&oramav1alpha1.PaymentProviderList{}, &oramav1alpha1.PaymentWebhookEndpointList{}, &oramav1alpha1.PaymentCatalogList{},
	} {
		if err := f.client.List(f.ctx, list); err != nil {
			t.Fatal(err)
		}
		if n := apimeta.LenList(list); n != 0 {
			t.Errorf("%T has %d items without the Secret", list, n)
		}
	}
	f.expectQuiet(lakeControlRowsRequeue)
}

// A key whose prefix disagrees with spec.payments.mode, or a Secret without what it must hold, is
// refused before anything is created or sent to Stripe.
func TestLakePaymentsRefuseASecretOfTheWrongModeOrShape(t *testing.T) {
	for name, tc := range map[string]struct {
		data   map[string]string
		reason string
	}{
		"live operator key": {map[string]string{"operatorKey": "rk_live_51OperatorKey"}, "ModeMismatch"},
		"live app key":      {map[string]string{"appKey": "sk_live_51LakeAppKey"}, "ModeMismatch"},
		"not a Stripe key":  {map[string]string{"appKey": "pk_test_51LakeAppKey"}, "ModeMismatch"},
		"no app key":        {map[string]string{"appKey": ""}, "SecretMissing"},
		"not an account ID": {map[string]string{"accountID": "1Orama"}, "SecretMissing"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", tc.data))
			h.reconcileLake()
			h.expectReason(tc.reason)
			if h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) ||
				h.exists("lake-testing-payments", &oramav1alpha1.PaymentWebhookEndpoint{}) ||
				h.exists("lake-testing-catalog", &oramav1alpha1.PaymentCatalog{}) {
				t.Error("a payment object was created")
			}
			if got := h.stripe.received(); len(got) != 0 {
				t.Errorf("Stripe was called: %+v", got)
			}
			if env := h.stripeEnv(); len(env) != 0 {
				t.Errorf("Stripe env = %+v", env)
			}
		})
	}
}

// A Lake key of another account than the operator key cannot see the catalog's product: the Lake
// reports AppKeyMismatch and gets no Stripe variable.
func TestLakePaymentsRefuseAnAppKeyOfAnotherAccount(t *testing.T) {
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", map[string]string{"appKey": "rk_test_51OtherAccount"}))
	h.stripe.acceptKey("rk_test_51OtherAccount", "acct_1Other")
	h.reconcileLake()
	h.runProvider()
	h.runEndpoint()
	h.runCatalog()
	h.reconcileLake()
	h.expectReason("AppKeyMismatch")
	if env := h.stripeEnv(); len(env) != 0 || h.hash() != "" {
		t.Errorf("Stripe env %+v, hash %q", env, h.hash())
	}
}

// Product read alone cannot prove that the restricted key can charge. A Dashboard permission
// edit keeps the same key, so the failed check retries after a minute without a Secret change.
func TestLakePaymentsRequirePaymentIntentWriteAndRecoverAfterAPermissionEdit(t *testing.T) {
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", nil))
	now := time.Now()
	h.r.lakeAppKeyChecks.clock.now = func() time.Time { return now }
	h.stripe.setFailure("POST /v1/payment_intents", &fakeStripeError{
		status: http.StatusForbidden, errType: "invalid_request_error", message: "refused " + testLakeAppKey,
	})
	h.reconcileLake()
	h.runProvider()
	h.runEndpoint()
	h.runCatalog()
	h.reconcileLake()
	h.expectReason("AppKeyPermissionsMissing")
	if h.stripe.count("GET /v1/products/lake_usage") < 1 || h.stripe.count("POST /v1/payment_intents") != 1 {
		t.Fatal("the product read and charge permission probe must both run")
	}
	if env := h.stripeEnv(); len(env) != 0 || h.hash() != "" {
		t.Errorf("an unchargeable key was given to the Lake: env %+v hash %q", env, h.hash())
	}
	if message := h.condition().Message; strings.Contains(message, testLakeAppKey) || !strings.Contains(message, "Payment Intents write") {
		t.Errorf("permission failure message = %q", message)
	}
	h.stripe.setFailure("POST /v1/payment_intents", nil)
	h.reconcileLake()
	if got := h.stripe.count("POST /v1/payment_intents"); got != 1 {
		t.Errorf("a cached failure made %d probes, want 1", got)
	}
	now = now.Add(paymentRetry)
	h.reconcileLake()
	h.expectReason("Configured")
	if got := h.stripe.count("POST /v1/payment_intents"); got != 2 {
		t.Errorf("after a permission edit: %d probes, want 2", got)
	}
	if len(h.stripeEnv()) != 2 || h.hash() == "" {
		t.Fatal("a repaired key was not given to the Lake")
	}
	for _, request := range h.stripe.received() {
		if request.Path == "/v1/payment_intents" && (len(request.Form) != 2 ||
			request.Form.Get("currency") != "usd" || request.Form.Get("payment_method_types[]") != "card" ||
			request.Version != lakeStripeAPIVersion || request.IdempotencyKey != "") {
			t.Errorf("unsafe or unexpected permission probe: %+v", request)
		}
	}
}

// Only Stripe's precise validation error passes; unrelated 4xx errors, outages, malformed
// answers and unexpected success must never mark a key chargeable.
func TestLakePaymentIntentPermissionProbeRequiresTheExpectedValidationError(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		status             int
	}{
		{"missing amount", `{"error":{"type":"invalid_request_error","code":"parameter_missing","param":"amount"}}`, "", 400},
		{"wrong parameter", `{"error":{"type":"invalid_request_error","code":"parameter_missing","param":"currency"}}`, "StripeAPIError", 400},
		{"wrong code", `{"error":{"type":"invalid_request_error","code":"parameter_invalid_integer","param":"amount"}}`, "StripeAPIError", 400},
		{"wrong type", `{"error":{"type":"permission_error","code":"parameter_missing","param":"amount"}}`, "AppKeyPermissionsMissing", 400},
		{"forbidden", `{"error":{"type":"invalid_request_error"}}`, "AppKeyPermissionsMissing", 403},
		{"revoked", `{"error":{"type":"invalid_request_error"}}`, "StripeAPIError", 401},
		{"rate limited", `{"error":{"type":"rate_limit_error"}}`, "StripeAPIError", 429},
		{"unavailable", `{"error":{"type":"api_error"}}`, "StripeAPIError", 500},
		{"malformed", `{`, "StripeAPIError", 400},
		{"unexpected success", `{}`, "StripeAPIError", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			r := LakeReconciler{StripeAPIBase: server.URL}
			failure := r.lakeAppKeyChargeFailure(context.Background(), testLakeAppKey)
			if tc.reason == "" {
				if failure != nil {
					t.Fatalf("expected validation error failed the probe: %+v", failure)
				}
			} else if failure == nil || failure.Reason != tc.reason {
				t.Fatalf("failure = %+v, want %s", failure, tc.reason)
			}
		})
	}
}

// Once given, the variables survive a failed hourly check and a rotated key that fails its check,
// without rolling the Lake; a key of the wrong mode withdraws them.
func TestLakePaymentsKeepTheEnvThroughCheckFailuresButNotAWrongMode(t *testing.T) {
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", nil))
	h.configure()
	env, hash := h.stripeEnv(), h.hash()

	later := time.Now().Add(2 * time.Hour)
	h.r.lakeAppKeyChecks.clock.now = func() time.Time { return later }
	h.stripe.setFailure("GET /v1/products/lake_usage", &fakeStripeError{status: http.StatusInternalServerError, errType: "api_error"})
	h.reconcileLake()
	h.expectReason("StripeAPIError")
	if !reflect.DeepEqual(h.stripeEnv(), env) || h.hash() != hash {
		t.Errorf("a failed hourly check changed the env or the hash: %+v %q", h.stripeEnv(), h.hash())
	}
	h.stripe.setFailure("GET /v1/products/lake_usage", nil)
	later = later.Add(2 * time.Hour)
	h.stripe.setFailure("POST /v1/payment_intents", &fakeStripeError{status: http.StatusForbidden, errType: "invalid_request_error"})
	h.reconcileLake()
	h.expectReason("AppKeyPermissionsMissing")
	if !reflect.DeepEqual(h.stripeEnv(), env) || h.hash() != hash {
		t.Error("a missing charge permission changed the existing env or rolled the Lake")
	}
	h.stripe.setFailure("POST /v1/payment_intents", nil)

	h.stripe.acceptKey("rk_test_51OtherAccount", "acct_1Other")
	h.updatePaymentsSecret(map[string]string{"appKey": "rk_test_51OtherAccount"})
	h.reconcileLake()
	h.expectReason("AppKeyMismatch")
	if !reflect.DeepEqual(h.stripeEnv(), env) || h.hash() != hash {
		t.Errorf("a rotated key that failed its check rolled the Lake: %+v %q", h.stripeEnv(), h.hash())
	}

	h.updatePaymentsSecret(map[string]string{"appKey": "rk_live_51LakeAppKey"})
	h.reconcileLake()
	h.expectReason("ModeMismatch")
	if env := h.stripeEnv(); len(env) != 0 || h.hash() != "" {
		t.Errorf("a live key in a test Lake kept the env: %+v %q", env, h.hash())
	}
}

// A new value under the same names, of the signing secret or of Lake's key, changes the
// payments-secret-hash and so rolls the pod.
func TestLakePaymentsRollTheLakeWhenASecretValueChanges(t *testing.T) {
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", nil))
	h.configure()
	first := h.hash()

	var signing corev1.Secret
	if err := h.c.Get(h.ctx, h.key("lake-testing-payments-webhook"), &signing); err != nil {
		t.Fatal(err)
	}
	signing.Data["signingSecret"] = []byte("whsec_replaced")
	if err := h.c.Update(h.ctx, &signing); err != nil {
		t.Fatal(err)
	}
	h.reconcileLake()
	second := h.hash()
	if second == first || second != lakePaymentsHash(testLakeAppKey, "whsec_replaced") {
		t.Errorf("hash after a new signing secret = %q (was %q)", second, first)
	}

	h.stripe.acceptKey("rk_test_51LakeAppKeyRotated", testStripeAccount)
	h.updatePaymentsSecret(map[string]string{"appKey": "rk_test_51LakeAppKeyRotated"})
	h.reconcileLake()
	h.expectReason("Configured")
	if third := h.hash(); third == second || third != lakePaymentsHash("rk_test_51LakeAppKeyRotated", "whsec_replaced") {
		t.Errorf("hash after a rotated appKey = %q (was %q)", third, second)
	}
	if env := h.stripeEnv(); !reflect.DeepEqual(env, lakePaymentsTestEnv("lake-testing-payments-webhook")) {
		t.Errorf("Stripe env = %+v", env)
	}
}

// Removing spec.payments withdraws the variables and deletes the catalog and the endpoint at once,
// and the provider only once the endpoint is gone, so the endpoint's finalizer still has the key
// and deletes the Stripe endpoint. The test finalizer stands in for the garbage collector holding a
// foreground deletion until the StripeWebhookEndpoint is gone.
func TestLakePaymentsRemovalDeletesTheEndpointBeforeTheProvider(t *testing.T) {
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", nil))
	h.configure()
	if n := len(h.stripe.webhookEndpoints()); n != 1 {
		t.Fatalf("Stripe has %d endpoints", n)
	}
	var endpoint oramav1alpha1.PaymentWebhookEndpoint
	h.exists("lake-testing-payments", &endpoint)
	controllerutil.AddFinalizer(&endpoint, "test.orama.cloud/foreground")
	if err := h.c.Update(h.ctx, &endpoint); err != nil {
		t.Fatal(err)
	}

	var lake oramav1alpha1.Lake
	if err := h.c.Get(h.ctx, types.NamespacedName{Name: "testing"}, &lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Payments = nil
	if err := h.c.Update(h.ctx, &lake); err != nil {
		t.Fatal(err)
	}
	h.reconcileLake()
	env := h.stripeEnv()
	if len(env) != 2 || env[0].Value != "sk_test_handmade" || env[1].Value != "whsec_handmade" || h.hash() != "" {
		t.Errorf("after removal: Stripe env %+v (want spec.env's back), hash %q", env, h.hash())
	}
	if h.exists("lake-testing-catalog", &oramav1alpha1.PaymentCatalog{}) {
		t.Error("the catalog was kept")
	}
	if !h.exists("lake-testing-payments", &endpoint) || endpoint.DeletionTimestamp.IsZero() {
		t.Fatalf("the endpoint is not being deleted: %+v", endpoint.ObjectMeta)
	}
	if !h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) {
		t.Fatal("the provider went before the endpoint")
	}
	h.expectReason("Removing")

	// The garbage collector deletes the StripeWebhookEndpoint, whose finalizer deletes the Stripe
	// endpoint with the provider's key, and then releases the PaymentWebhookEndpoint.
	var concrete oramav1alpha1.StripeWebhookEndpoint
	h.exists("lake-testing-payments", &concrete)
	if err := h.c.Delete(h.ctx, &concrete); err != nil {
		t.Fatal(err)
	}
	h.run(h.stripeEndpoint, h.key("lake-testing-payments"))
	if got := h.stripe.webhookEndpoints(); len(got) != 0 {
		t.Errorf("the Stripe endpoint was left behind: %+v", got)
	}
	controllerutil.RemoveFinalizer(&endpoint, "test.orama.cloud/foreground")
	if err := h.c.Update(h.ctx, &endpoint); err != nil {
		t.Fatal(err)
	}

	h.reconcileLake()
	if h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) {
		t.Error("the provider was kept after the endpoint went")
	}
	if condition := h.condition(); condition != nil {
		t.Errorf("PaymentsReady kept after removal: %+v", condition)
	}
}

// Deleting a Lake removes its payments in the same order before its finalizer is released.
func TestDeletingALakeRemovesItsPaymentsFirst(t *testing.T) {
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", nil))
	h.configure()
	var lake oramav1alpha1.Lake
	if err := h.c.Get(h.ctx, types.NamespacedName{Name: "testing"}, &lake); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(h.ctx, &lake); err != nil {
		t.Fatal(err)
	}
	h.reconcileLake()
	if !h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) {
		t.Fatal("the provider went before the endpoint")
	}
	if h.exists("lake-testing-payments", &oramav1alpha1.PaymentWebhookEndpoint{}) || h.exists("lake-testing-catalog", &oramav1alpha1.PaymentCatalog{}) {
		t.Fatal("the endpoint or the catalog was kept")
	}
	if err := h.c.Get(h.ctx, types.NamespacedName{Name: "testing"}, &lake); err != nil || !controllerutil.ContainsFinalizer(&lake, lakeZettaFinalizer) {
		t.Fatalf("the Lake was released before its payments were removed: %v", err)
	}
	h.reconcileLake()
	if h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) {
		t.Error("the provider was kept after the endpoint went")
	}
}

func TestDeletingALakeWithARefusedStripeKeyCompletes(t *testing.T) {
	testDeletingALakeWithARefusedStripeKey(t, false)
}

func TestDeletingALakeWithARevokedStripeKeyCompletesAfterGrace(t *testing.T) {
	testDeletingALakeWithARefusedStripeKey(t, true)
}

// A refused replacement key reaches Stripe directly while the provider's check is still cached;
// a previously valid key that fails its next check holds deletion only for the grace window.
func testDeletingALakeWithARefusedStripeKey(t *testing.T, checkProvider bool) {
	t.Helper()
	h := newPaymentsHarness(t, paymentsTestLake("testing"), lakePaymentsTestSecret("lake-testing", nil))
	h.configure()
	recorder := record.NewFakeRecorder(10)
	h.stripeEndpoint.Recorder = recorder
	if checkProvider {
		for _, route := range []string{"GET /v1/account", "GET /v1/webhook_endpoints"} {
			h.stripe.setFailure(route, &fakeStripeError{status: http.StatusUnauthorized, errType: "invalid_request_error"})
		}
		h.provider.clock.now = func() time.Time { return time.Now().Add(2 * paymentRecheck) }
		h.runProvider()
		var provider oramav1alpha1.PaymentProvider
		h.exists("lake-testing-payments", &provider)
		credentials := apimeta.FindStatusCondition(provider.Status.Conditions, paymentConditionCredentials)
		if credentials == nil || credentials.Status != metav1.ConditionFalse || !strings.Contains(credentials.Message, "HTTP 401") {
			t.Fatalf("revoked key Credentials = %+v", credentials)
		}
	} else {
		h.updatePaymentsSecret(map[string]string{"operatorKey": "rk_test_51RefusedOperatorKey"})
	}

	// The fake client has no garbage collector. Hold the abstract endpoint until its concrete
	// child's finalizer releases it, as Kubernetes foreground deletion does.
	var endpoint oramav1alpha1.PaymentWebhookEndpoint
	if !h.exists("lake-testing-payments", &endpoint) {
		t.Fatal("no PaymentWebhookEndpoint")
	}
	controllerutil.AddFinalizer(&endpoint, "test.orama.cloud/foreground")
	if err := h.c.Update(h.ctx, &endpoint); err != nil {
		t.Fatal(err)
	}
	var lake oramav1alpha1.Lake
	lakeKey := types.NamespacedName{Name: "testing"}
	if err := h.c.Get(h.ctx, lakeKey, &lake); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(h.ctx, &lake); err != nil {
		t.Fatal(err)
	}
	h.reconcileLake()
	if !h.exists("lake-testing-payments", &endpoint) || endpoint.DeletionTimestamp.IsZero() {
		t.Fatal("the PaymentWebhookEndpoint is not being deleted")
	}
	if !h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) {
		t.Fatal("the provider went before the endpoint")
	}
	var concrete oramav1alpha1.StripeWebhookEndpoint
	if !h.exists("lake-testing-payments", &concrete) || concrete.Status.EndpointID == "" {
		t.Fatal("no configured StripeWebhookEndpoint")
	}
	endpointID := concrete.Status.EndpointID
	if err := h.c.Delete(h.ctx, &concrete); err != nil {
		t.Fatal(err)
	}
	h.exists("lake-testing-payments", &concrete)
	now := concrete.DeletionTimestamp.Time
	h.stripeEndpoint.clock.now = func() time.Time { return now }
	requests := h.stripe.count("GET /v1/webhook_endpoints")
	result, err := h.stripeEndpoint.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.key("lake-testing-payments")})
	if err != nil {
		t.Fatal(err)
	}
	if checkProvider {
		if !h.exists("lake-testing-payments", &concrete) || !controllerutil.ContainsFinalizer(&concrete, paymentWebhookFinalizer) || result.RequeueAfter != paymentRetry {
			t.Fatalf("within grace: endpoint %+v, requeue %v", concrete.ObjectMeta, result.RequeueAfter)
		}
		wantReady(t, concrete, metav1.ConditionFalse, "ProviderNotReady")
		h.reconcileLake()
		if err := h.c.Get(h.ctx, lakeKey, &lake); err != nil || !controllerutil.ContainsFinalizer(&lake, lakeZettaFinalizer) {
			t.Fatalf("the Lake was released within grace: %v", err)
		}
		if !h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) {
			t.Fatal("the provider went within grace")
		}
		if events := eventsOf(recorder); len(events) != 0 {
			t.Fatalf("events within grace = %v", events)
		}
		if h.stripe.count("GET /v1/webhook_endpoints") != requests {
			t.Fatal("called Stripe with Credentials=False")
		}
		now = now.Add(paymentDeleteGrace + time.Second)
		h.run(h.stripeEndpoint, h.key("lake-testing-payments"))
	} else if result.RequeueAfter != 0 || h.stripe.count("GET /v1/webhook_endpoints") != requests+1 {
		t.Fatalf("Stripe 401: requeue %v, list calls %d", result.RequeueAfter, h.stripe.count("GET /v1/webhook_endpoints")-requests)
	}
	if h.exists("lake-testing-payments", &oramav1alpha1.StripeWebhookEndpoint{}) {
		t.Fatal("the StripeWebhookEndpoint was kept after the refused key")
	}
	events := eventsOf(recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning EndpointLeftBehind") || !strings.Contains(events[0], "HTTP 401") ||
		!strings.Contains(events[0], "Stripe webhook endpoint "+endpointID+" was not deleted: delete it in the Stripe Dashboard") {
		t.Fatalf("events = %v", events)
	}
	if remaining := h.stripe.webhookEndpoints(); len(remaining) != 1 || remaining[0].ID != endpointID {
		t.Fatalf("Stripe endpoints left behind = %+v", remaining)
	}
	controllerutil.RemoveFinalizer(&endpoint, "test.orama.cloud/foreground")
	if err := h.c.Update(h.ctx, &endpoint); err != nil {
		t.Fatal(err)
	}
	h.reconcileLake()
	h.reconcileLake()
	if err := h.c.Get(h.ctx, lakeKey, &lake); !apierrors.IsNotFound(err) {
		t.Fatalf("the Lake remains after payment cleanup: %v", err)
	}
	if h.exists("lake-testing-payments", &oramav1alpha1.PaymentWebhookEndpoint{}) ||
		h.exists("lake-testing-payments", &oramav1alpha1.PaymentProvider{}) {
		t.Fatal("the PaymentWebhookEndpoint or PaymentProvider was kept")
	}
}

// The Secret watch maps the payments Secret and the signing Secret to their Lake, and nothing else.
func TestLakePaymentsSecretsMapToTheirLake(t *testing.T) {
	s := newLakeScheme(t)
	plain := paymentsTestLake("plain")
	plain.Spec.Payments = nil
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(paymentsTestLake("testing"), plain).Build()
	r := &LakeReconciler{Client: c, Scheme: s}
	for _, tc := range []struct {
		namespace, name string
		want            []reconcile.Request
	}{
		{"lake-testing", testLakePaymentsSecret, []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "testing"}}}},
		{"lake-testing", "lake-testing-payments-webhook", []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "testing"}}}},
		{"lake-testing", "lake-testing-admin", nil},
		{"lake-plain", testLakePaymentsSecret, nil},
		{"lake-other", testLakePaymentsSecret, nil},
	} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tc.namespace, Name: tc.name}}
		if got := r.lakeRequestsForPaymentsSecret(context.Background(), secret); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s/%s maps to %v, want %v", tc.namespace, tc.name, got, tc.want)
		}
	}
}
