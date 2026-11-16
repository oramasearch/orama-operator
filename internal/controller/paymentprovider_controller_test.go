/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testStripeKey     = "rk_test_51OperatorKey"
	testStripeAccount = "acct_1Orama"
)

// testStripeKeyPattern is any key value; a message may name a prefix such as sk_live_, never a key.
var testStripeKeyPattern = regexp.MustCompile(`(sk|rk|pk)_(live|test)_[A-Za-z0-9]`)

type paymentProviderFixture struct {
	client    client.Client
	reconcile func() (oramav1alpha1.PaymentProvider, ctrl.Result)
	// advance moves the provider's clock forward.
	advance func(time.Duration)
}

// newPaymentProviderFixture is a stripe PaymentProvider "stripe" in namespace "billing" for
// account acct_1Orama, reading key "apiKey" of Secret "stripe-operator". secretKey "" leaves the
// Secret out.
func newPaymentProviderFixture(t *testing.T, stripe *fakeStripe, livemode bool, secretKey string) paymentProviderFixture {
	t.Helper()
	s := newLakeScheme(t)
	provider := &oramav1alpha1.PaymentProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "stripe", Namespace: "billing", UID: "provider-uid", Generation: 1},
		Spec: oramav1alpha1.PaymentProviderSpec{Type: "stripe", Stripe: &oramav1alpha1.StripePaymentProviderSpec{
			AccountID: testStripeAccount, Livemode: livemode,
			APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: "stripe-operator", Key: "apiKey"},
		}},
	}
	objects := []client.Object{provider}
	if secretKey != "" {
		objects = append(objects, paymentKeySecret(secretKey))
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(provider).Build()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := &PaymentProviderReconciler{Client: c, Scheme: s, StripeAPIBase: stripe.url}
	r.clock.now = func() time.Time { return now }
	return paymentProviderFixture{
		client: c,
		reconcile: func() (oramav1alpha1.PaymentProvider, ctrl.Result) {
			t.Helper()
			key := types.NamespacedName{Namespace: "billing", Name: "stripe"}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var out oramav1alpha1.PaymentProvider
			if err := c.Get(context.Background(), key, &out); err != nil {
				t.Fatal(err)
			}
			return out, result
		},
		advance: func(d time.Duration) { now = now.Add(d) },
	}
}

func paymentKeySecret(key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "stripe-operator", Namespace: "billing"},
		Data:       map[string][]byte{"apiKey": []byte(key)},
	}
}

// credentialsOf is the provider's Credentials condition, failing the test when it is missing or
// carries a key.
func credentialsOf(t *testing.T, provider oramav1alpha1.PaymentProvider) metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(provider.Status.Conditions, paymentConditionCredentials)
	if condition == nil {
		t.Fatalf("no Credentials condition: %+v", provider.Status)
	}
	if testStripeKeyPattern.MatchString(condition.Message) {
		t.Errorf("the condition carries a key: %q", condition.Message)
	}
	return *condition
}

func wantCredentials(t *testing.T, provider oramav1alpha1.PaymentProvider, status metav1.ConditionStatus, reason string) metav1.Condition {
	t.Helper()
	condition := credentialsOf(t, provider)
	if condition.Status != status || condition.Reason != reason {
		t.Errorf("Credentials = %s %s (%q), want %s %s", condition.Status, condition.Reason, condition.Message, status, reason)
	}
	return condition
}

// A key of the named account and mode is Verified; a steady provider makes no request and writes
// no status until the hourly recheck, which writes nothing either.
func TestPaymentProviderVerifiesTheKeysAccount(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newPaymentProviderFixture(t, stripe, false, testStripeKey)

	first, result := f.reconcile()
	wantCredentials(t, first, metav1.ConditionTrue, "Verified")
	if result.RequeueAfter != paymentRecheck {
		t.Errorf("requeue = %v, want %v", result.RequeueAfter, paymentRecheck)
	}
	if requests := stripe.received(); len(requests) != 1 || requests[0].Version != stripeAPIVersion {
		t.Fatalf("requests = %+v", requests)
	}

	second, _ := f.reconcile()
	if stripe.count("GET /v1/account") != 1 || second.ResourceVersion != first.ResourceVersion {
		t.Errorf("a steady provider called Stripe %d times or rewrote its status", stripe.count("GET /v1/account"))
	}
	f.advance(paymentRecheck)
	third, _ := f.reconcile()
	if stripe.count("GET /v1/account") != 2 || third.ResourceVersion != first.ResourceVersion {
		t.Errorf("the recheck called Stripe %d times in all or rewrote an unchanged status", stripe.count("GET /v1/account"))
	}
}

func TestPaymentProviderRefusesAKeyOfAnotherAccount(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, "acct_2Other")
	f := newPaymentProviderFixture(t, stripe, false, testStripeKey)
	provider, _ := f.reconcile()
	condition := wantCredentials(t, provider, metav1.ConditionFalse, "AccountMismatch")
	if !strings.Contains(condition.Message, "acct_2Other") || !strings.Contains(condition.Message, testStripeAccount) {
		t.Errorf("message %q does not name both accounts", condition.Message)
	}
}

// The key's prefix must agree with livemode; a key that disagrees is never sent to Stripe.
func TestPaymentProviderChecksTheKeysModeFirst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		livemode bool
		key      string
	}{
		{"live spec, test key", true, "rk_test_51OperatorKey"},
		{"test spec, live key", false, "sk_live_51OperatorKey"},
		{"publishable key", false, "pk_test_51OperatorKey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stripe := newFakeStripe(t, tc.key, testStripeAccount)
			f := newPaymentProviderFixture(t, stripe, tc.livemode, tc.key)
			provider, _ := f.reconcile()
			wantCredentials(t, provider, metav1.ConditionFalse, "ModeMismatch")
			if n := len(stripe.received()); n != 0 {
				t.Errorf("the key was sent to Stripe %d times", n)
			}
		})
	}
}

// A missing Secret or key is SecretMissing, looked at again in a minute; once it exists, the key
// is checked at once, even when it is the key checked before it went missing.
func TestPaymentProviderWaitsForItsSecret(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newPaymentProviderFixture(t, stripe, false, "")
	ctx := context.Background()

	provider, result := f.reconcile()
	wantCredentials(t, provider, metav1.ConditionFalse, "SecretMissing")
	if result.RequeueAfter != paymentRetry || len(stripe.received()) != 0 {
		t.Errorf("requeue %v, %d requests", result.RequeueAfter, len(stripe.received()))
	}
	empty := paymentKeySecret("")
	empty.Data = map[string][]byte{"other": []byte("x")}
	if err := f.client.Create(ctx, empty); err != nil {
		t.Fatal(err)
	}
	provider, _ = f.reconcile()
	if condition := wantCredentials(t, provider, metav1.ConditionFalse, "SecretMissing"); !strings.Contains(condition.Message, `no key "apiKey"`) {
		t.Errorf("message = %q", condition.Message)
	}

	empty.Data = map[string][]byte{"apiKey": []byte(testStripeKey)}
	if err := f.client.Update(ctx, empty); err != nil {
		t.Fatal(err)
	}
	provider, _ = f.reconcile()
	wantCredentials(t, provider, metav1.ConditionTrue, "Verified")

	if err := f.client.Delete(ctx, empty); err != nil {
		t.Fatal(err)
	}
	provider, _ = f.reconcile()
	wantCredentials(t, provider, metav1.ConditionFalse, "SecretMissing")
	if err := f.client.Create(ctx, paymentKeySecret(testStripeKey)); err != nil {
		t.Fatal(err)
	}
	provider, _ = f.reconcile()
	wantCredentials(t, provider, metav1.ConditionTrue, "Verified")
	if n := stripe.count("GET /v1/account"); n != 2 {
		t.Errorf("account read %d times, want 2", n)
	}
}

// A Stripe error is StripeAPIError, retried after a minute; a refused key is reported without the
// key Stripe's answer echoes.
func TestPaymentProviderReportsStripeErrors(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.setFailure("GET /v1/account", &fakeStripeError{http.StatusInternalServerError, "api_error", "", "Something went wrong"})
	f := newPaymentProviderFixture(t, stripe, false, testStripeKey)

	provider, result := f.reconcile()
	if condition := wantCredentials(t, provider, metav1.ConditionFalse, "StripeAPIError"); !strings.Contains(condition.Message, "HTTP 500") {
		t.Errorf("message = %q", condition.Message)
	}
	if result.RequeueAfter != paymentRetry {
		t.Errorf("requeue = %v, want %v", result.RequeueAfter, paymentRetry)
	}
	f.reconcile()
	if n := stripe.count("GET /v1/account"); n != 1 {
		t.Errorf("retried before a minute passed: %d requests", n)
	}
	stripe.setFailure("GET /v1/account", nil)
	f.advance(paymentRetry)
	provider, _ = f.reconcile()
	wantCredentials(t, provider, metav1.ConditionTrue, "Verified")

	refused := newFakeStripe(t, "rk_test_51AnotherKey", testStripeAccount)
	g := newPaymentProviderFixture(t, refused, false, testStripeKey)
	provider, _ = g.reconcile()
	if condition := wantCredentials(t, provider, metav1.ConditionFalse, "StripeAPIError"); !strings.Contains(condition.Message, "HTTP 401") {
		t.Errorf("message = %q", condition.Message)
	}
}

// A restricted key that may not read the account is checked by listing webhook endpoints:
// ModeVerified, saying the account is not checked, unless a listed endpoint is of the other mode.
func TestPaymentProviderFallsBackToTheModeWhenTheAccountIsUnreadable(t *testing.T) {
	forbidden := &fakeStripeError{http.StatusForbidden, "invalid_request_error", "",
		"The provided key '" + testStripeKey + "' does not have the required permissions for this endpoint"}

	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.setFailure("GET /v1/account", forbidden)
	stripe.endpoints = []fakeStripeEndpoint{{ID: "we_1", URL: "https://eu.lakelive.example/stripe", Livemode: false}}
	f := newPaymentProviderFixture(t, stripe, false, testStripeKey)
	provider, result := f.reconcile()
	condition := wantCredentials(t, provider, metav1.ConditionTrue, "ModeVerified")
	if !strings.Contains(condition.Message, "accountId is not checked") || result.RequeueAfter != paymentRecheck {
		t.Errorf("message %q, requeue %v", condition.Message, result.RequeueAfter)
	}
	requests := stripe.received()
	if len(requests) != 2 || requests[1].Path != "/v1/webhook_endpoints" || requests[1].Query.Get("limit") != "1" {
		t.Errorf("requests = %+v", requests)
	}

	stripe.endpoints[0].Livemode = true
	f.advance(paymentRecheck)
	provider, _ = f.reconcile()
	wantCredentials(t, provider, metav1.ConditionFalse, "ModeMismatch")

	stripe.setFailure("GET /v1/webhook_endpoints", forbidden)
	f.advance(paymentRecheck)
	provider, _ = f.reconcile()
	wantCredentials(t, provider, metav1.ConditionFalse, "StripeAPIError")
}
