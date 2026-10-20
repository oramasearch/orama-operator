/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testWebhookURL        = "https://eu.lake.live/v1/billing/stripe/webhook"
	testWebhookAPIVersion = "2024-06-20"
	testWebhookSecretName = "lake-stripe-webhook"
)

var testWebhookKey = types.NamespacedName{Namespace: "billing", Name: "lake"}

type stripeWebhookFixture struct {
	client    client.WithWatch
	recorder  *record.FakeRecorder
	reconcile func() (oramav1alpha1.StripeWebhookEndpoint, ctrl.Result)
	// edit changes the endpoint's spec, as the abstract kind would.
	edit func(func(*oramav1alpha1.StripeWebhookEndpointSpec))
	// advance moves the controller's clock forward.
	advance func(time.Duration)
	// delete starts deletion and sets the clock to its timestamp.
	delete func()
	// logs is every line the controller logged.
	logs func() []string
}

// newStripeWebhookFixture is StripeWebhookEndpoint "lake" in namespace "billing" for a stripe
// PaymentProvider whose Credentials are True, delivering three event types (one repeated, out of
// order) to testWebhookURL. objects are added to the cluster.
func newStripeWebhookFixture(t *testing.T, stripe *fakeStripe, objects ...client.Object) stripeWebhookFixture {
	t.Helper()
	s := newLakeScheme(t)
	provider := &oramav1alpha1.PaymentProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "stripe", Namespace: "billing", UID: "provider-uid", Generation: 1},
		Spec: oramav1alpha1.PaymentProviderSpec{Type: "stripe", Stripe: &oramav1alpha1.StripePaymentProviderSpec{
			AccountID: testStripeAccount, APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: "stripe-operator", Key: "apiKey"},
		}},
		Status: oramav1alpha1.PaymentProviderStatus{Conditions: []metav1.Condition{{
			Type: paymentConditionCredentials, Status: metav1.ConditionTrue, Reason: "Verified", ObservedGeneration: 1,
			LastTransitionTime: metav1.Now(),
		}}},
	}
	endpoint := &oramav1alpha1.StripeWebhookEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", Namespace: "billing", UID: "endpoint-uid", Generation: 1},
		Spec: oramav1alpha1.StripeWebhookEndpointSpec{
			PaymentWebhookEndpointSpec: oramav1alpha1.PaymentWebhookEndpointSpec{
				ProviderRef: corev1.LocalObjectReference{Name: "stripe"}, URL: testWebhookURL,
				Events:     []string{"invoice.paid", "customer.subscription.updated", "invoice.paid"},
				APIVersion: testWebhookAPIVersion, App: "lake", SigningSecretName: testWebhookSecretName,
			},
			APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: "stripe-operator", Key: "apiKey"},
		},
	}
	objects = append([]client.Object{provider, endpoint, paymentKeySecret(testStripeKey)}, objects...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(provider, endpoint).Build()
	recorder := record.NewFakeRecorder(100)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := &StripeWebhookEndpointReconciler{Client: c, Scheme: s, Recorder: recorder, StripeAPIBase: stripe.url}
	r.clock.now = func() time.Time { return now }
	var mu sync.Mutex
	var lines []string
	ctx := logf.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 10}))
	return stripeWebhookFixture{
		client:   c,
		recorder: recorder,
		reconcile: func() (oramav1alpha1.StripeWebhookEndpoint, ctrl.Result) {
			t.Helper()
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: testWebhookKey})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var out oramav1alpha1.StripeWebhookEndpoint
			if err := c.Get(ctx, testWebhookKey, &out); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			return out, result
		},
		edit: func(change func(*oramav1alpha1.StripeWebhookEndpointSpec)) {
			t.Helper()
			var current oramav1alpha1.StripeWebhookEndpoint
			if err := c.Get(ctx, testWebhookKey, &current); err != nil {
				t.Fatal(err)
			}
			change(&current.Spec)
			if err := c.Update(ctx, &current); err != nil {
				t.Fatal(err)
			}
		},
		advance: func(d time.Duration) { now = now.Add(d) },
		delete: func() {
			t.Helper()
			var endpoint oramav1alpha1.StripeWebhookEndpoint
			if err := c.Get(ctx, testWebhookKey, &endpoint); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(ctx, &endpoint); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, testWebhookKey, &endpoint); err != nil {
				t.Fatal(err)
			}
			now = endpoint.DeletionTimestamp.Time
		},
		logs: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string{}, lines...)
		},
	}
}

// wantReady checks the Ready condition, and that no condition carries a key.
func wantReady(t *testing.T, endpoint oramav1alpha1.StripeWebhookEndpoint, status metav1.ConditionStatus, reason string) metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(endpoint.Status.Conditions, paymentConditionReady)
	if condition == nil {
		t.Fatalf("no Ready condition: %+v", endpoint.Status)
	}
	if testStripeKeyPattern.MatchString(condition.Message) {
		t.Errorf("the condition carries a key: %q", condition.Message)
	}
	if condition.Status != status || condition.Reason != reason {
		t.Errorf("Ready = %s %s (%q), want %s %s", condition.Status, condition.Reason, condition.Message, status, reason)
	}
	return *condition
}

// signingSecretOf is the signing Secret's value, failing the test unless the endpoint controls it.
func signingSecretOf(t *testing.T, c client.Client, endpoint oramav1alpha1.StripeWebhookEndpoint) string {
	t.Helper()
	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "billing", Name: testWebhookSecretName}, &secret); err != nil {
		t.Fatalf("signing Secret: %v", err)
	}
	if !metav1.IsControlledBy(&secret, &endpoint) {
		t.Errorf("the signing Secret is not controlled by the endpoint: %+v", secret.OwnerReferences)
	}
	return string(secret.Data[paymentWebhookSigningSecretKey])
}

func eventsOf(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// ownEndpoint is an endpoint carrying this object's tags, as the operator creates it.
func ownEndpoint(id string) fakeStripeEndpoint {
	return fakeStripeEndpoint{
		ID: id, URL: testWebhookURL, EnabledEvents: []string{"customer.subscription.updated", "invoice.paid"},
		APIVersion: testWebhookAPIVersion, Status: "enabled",
		Metadata: map[string]string{"orama_managed": "true", "orama_owner": "billing/lake", "orama_app": "lake"},
	}
}

var createKeyPattern = regexp.MustCompile(`^orama-endpoint-uid-create-new-[0-9a-f]{16}$`)

// The first pass creates one endpoint with the tags, sorted events, pinned version and a key naming
// the request, and writes its signing secret; a steady endpoint then makes no write and no status
// change, and its hourly recheck only lists.
func TestStripeWebhookEndpointCreatesOneEndpointAndWritesItsSecret(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)

	first, result := f.reconcile()
	wantReady(t, first, metav1.ConditionTrue, "Configured")
	if result.RequeueAfter != paymentRecheck || first.Status.EndpointID != "we_1" || first.Status.Writes != 1 {
		t.Errorf("requeue %v, status %+v", result.RequeueAfter, first.Status)
	}
	if !slices.Contains(first.Finalizers, paymentWebhookFinalizer) {
		t.Errorf("finalizers = %v", first.Finalizers)
	}
	writes := stripe.writes()
	if len(writes) != 1 || writes[0].Path != "/v1/webhook_endpoints" {
		t.Fatalf("writes = %+v", writes)
	}
	create := writes[0]
	want := map[string][]string{
		"url":                     {testWebhookURL},
		"enabled_events[]":        {"customer.subscription.updated", "invoice.paid"},
		"api_version":             {testWebhookAPIVersion},
		"description":             {"orama-operator: lake billing/lake"},
		"metadata[orama_managed]": {"true"},
		"metadata[orama_owner]":   {"billing/lake"},
		"metadata[orama_app]":     {"lake"},
	}
	for field, values := range want {
		if !slices.Equal(create.Form[field], values) {
			t.Errorf("form %s = %v, want %v", field, create.Form[field], values)
		}
	}
	if len(create.Form) != len(want) || create.Version != testWebhookAPIVersion || !createKeyPattern.MatchString(create.IdempotencyKey) {
		t.Errorf("create = %+v", create)
	}
	if got := signingSecretOf(t, f.client, first); got != "whsec_fake1" {
		t.Errorf("signing secret = %q", got)
	}

	second, _ := f.reconcile()
	f.advance(paymentRecheck)
	third, result := f.reconcile()
	if n := len(stripe.writes()); n != 1 || second.ResourceVersion != first.ResourceVersion || third.ResourceVersion != first.ResourceVersion {
		t.Errorf("a steady endpoint made %d writes in all or rewrote its status", n)
	}
	if n := stripe.count("GET /v1/webhook_endpoints"); n != 2 || result.RequeueAfter != paymentRecheck {
		t.Errorf("listed %d times, requeue %v; want one list per pass", n, result.RequeueAfter)
	}
}

// New events are one update carrying only the sorted list; a new URL is one update carrying only
// the URL, found through the endpoint ID in status; a new apiVersion, which Stripe cannot change,
// deletes the endpoint and creates it again.
func TestStripeWebhookEndpointUpdatesDriftAndReplacesOnANewAPIVersion(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	f.reconcile()

	f.edit(func(spec *oramav1alpha1.StripeWebhookEndpointSpec) {
		spec.Events = []string{"invoice.payment_failed", "invoice.paid"}
	})
	endpoint, _ := f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 2 || writes[1].Path != "/v1/webhook_endpoints/we_1" || len(writes[1].Form) != 1 ||
		!slices.Equal(writes[1].Form["enabled_events[]"], []string{"invoice.paid", "invoice.payment_failed"}) {
		t.Fatalf("writes = %+v", writes)
	}
	if !strings.HasPrefix(writes[1].IdempotencyKey, "orama-endpoint-uid-update-we_1-") {
		t.Errorf("update key = %q", writes[1].IdempotencyKey)
	}

	f.edit(func(spec *oramav1alpha1.StripeWebhookEndpointSpec) { spec.URL = "https://eu.lake.live/stripe" })
	f.reconcile()
	if writes := stripe.writes(); len(writes) != 3 || len(writes[2].Form) != 1 || writes[2].Form.Get("url") != "https://eu.lake.live/stripe" {
		t.Fatalf("writes = %+v", writes)
	}

	f.edit(func(spec *oramav1alpha1.StripeWebhookEndpointSpec) { spec.APIVersion = "2025-03-31.basil" })
	endpoint, _ = f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	writes = stripe.writes()
	if len(writes) != 5 || writes[3].Method != http.MethodDelete || writes[3].Path != "/v1/webhook_endpoints/we_1" ||
		writes[4].Form.Get("api_version") != "2025-03-31.basil" || writes[4].Version != "2025-03-31.basil" {
		t.Fatalf("writes = %+v", writes)
	}
	if left := stripe.webhookEndpoints(); len(left) != 1 || left[0].ID != "we_2" || endpoint.Status.EndpointID != "we_2" {
		t.Errorf("endpoints %+v, status %+v", left, endpoint.Status)
	}
	if got := signingSecretOf(t, f.client, endpoint); got != "whsec_fake2" {
		t.Errorf("signing secret = %q", got)
	}
	if events := eventsOf(f.recorder); len(events) != 1 || !strings.Contains(events[0], "APIVersionChanged") || !strings.Contains(events[0], "we_1") {
		t.Errorf("events = %v", events)
	}
}

// A lost signing Secret cannot be read back: the endpoint is deleted and created again under a key
// naming the endpoint it replaces, so Stripe makes a new endpoint instead of replaying the first
// create, and the new secret is written.
func TestStripeWebhookEndpointRecreatesWhenItsSecretIsLost(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	f.reconcile()
	if err := f.client.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testWebhookSecretName, Namespace: "billing"}}); err != nil {
		t.Fatal(err)
	}

	endpoint, _ := f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 3 || writes[1].Method != http.MethodDelete || writes[1].Path != "/v1/webhook_endpoints/we_1" {
		t.Fatalf("writes = %+v", writes)
	}
	if recreate := writes[2].IdempotencyKey; recreate == writes[0].IdempotencyKey || !strings.Contains(recreate, "-create-recreate-we_1-") {
		t.Errorf("recreate key %q, first create key %q", recreate, writes[0].IdempotencyKey)
	}
	if left := stripe.webhookEndpoints(); len(left) != 1 || left[0].ID != "we_2" || endpoint.Status.EndpointID != "we_2" {
		t.Errorf("endpoints %+v, status %+v", left, endpoint.Status)
	}
	if got := signingSecretOf(t, f.client, endpoint); got != "whsec_fake2" {
		t.Errorf("signing secret = %q", got)
	}
	if events := eventsOf(f.recorder); len(events) != 1 || !strings.HasPrefix(events[0], "Warning SigningSecretLost") || !strings.Contains(events[0], "we_1") {
		t.Errorf("events = %v", events)
	}
}

// An endpoint at the spec's URL without this object's tags is never adopted, changed or deleted.
func TestStripeWebhookEndpointNeverTakesAnotherEndpointAtItsURL(t *testing.T) {
	for name, metadata := range map[string]map[string]string{
		"made by hand":     nil,
		"another object's": {"orama_managed": "true", "orama_owner": "billing/other", "orama_app": "lake"},
	} {
		t.Run(name, func(t *testing.T) {
			stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
			stripe.endpoints = []fakeStripeEndpoint{{ID: "we_hand", URL: testWebhookURL, Status: "disabled", Metadata: metadata}}
			f := newStripeWebhookFixture(t, stripe)

			endpoint, result := f.reconcile()
			if condition := wantReady(t, endpoint, metav1.ConditionFalse, "URLTaken"); !strings.Contains(condition.Message, "we_hand") {
				t.Errorf("message %q does not name the endpoint", condition.Message)
			}
			if n := len(stripe.writes()); n != 0 || result.RequeueAfter != paymentRetry {
				t.Errorf("%d writes, requeue %v", n, result.RequeueAfter)
			}
			if left := stripe.webhookEndpoints(); len(left) != 1 || left[0].Status != "disabled" {
				t.Errorf("the other endpoint changed: %+v", left)
			}
		})
	}
}

// An endpoint with this object's tags is ours whoever created it: a rebuilt cluster, with no status
// and no signing Secret, replaces it; an operator that lost only the status repairs it.
func TestStripeWebhookEndpointRecognisesItsEndpointFromAnEarlierCluster(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.endpoints = []fakeStripeEndpoint{ownEndpoint("we_earlier")}
	f := newStripeWebhookFixture(t, stripe)
	endpoint, _ := f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 2 || writes[0].Path != "/v1/webhook_endpoints/we_earlier" || !strings.Contains(writes[1].IdempotencyKey, "-recreate-we_earlier-") {
		t.Fatalf("writes = %+v", writes)
	}
	if left := stripe.webhookEndpoints(); len(left) != 1 || left[0].ID != "we_1" {
		t.Errorf("endpoints = %+v", left)
	}

	stripe = newFakeStripe(t, testStripeKey, testStripeAccount)
	disabled := ownEndpoint("we_earlier")
	disabled.Status = "disabled"
	stripe.endpoints = []fakeStripeEndpoint{disabled}
	held := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testWebhookSecretName, Namespace: "billing", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "StripeWebhookEndpoint", Name: "lake", UID: "endpoint-uid",
			Controller: ptrTo(true),
		}}},
		Data: map[string][]byte{paymentWebhookSigningSecretKey: []byte("whsec_earlier")},
	}
	f = newStripeWebhookFixture(t, stripe, held)
	endpoint, _ = f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	if writes := stripe.writes(); len(writes) != 1 || writes[0].Path != "/v1/webhook_endpoints/we_earlier" || writes[0].Form.Get("disabled") != "false" {
		t.Fatalf("writes = %+v", writes)
	}
	if endpoint.Status.EndpointID != "we_earlier" || signingSecretOf(t, f.client, endpoint) != "whsec_earlier" {
		t.Errorf("status %+v", endpoint.Status)
	}
}

// A signing Secret another controller owns is never written, so no endpoint is made for it.
func TestStripeWebhookEndpointRefusesASigningSecretItDoesNotOwn(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	taken := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testWebhookSecretName, Namespace: "billing", OwnerReferences: []metav1.OwnerReference{{
		APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "Lake", Name: "lake", UID: "lake-uid", Controller: ptrTo(true),
	}}}}
	f := newStripeWebhookFixture(t, stripe, taken)
	endpoint, _ := f.reconcile()
	wantReady(t, endpoint, metav1.ConditionFalse, "SigningSecretTaken")
	if n := len(stripe.received()); n != 0 {
		t.Errorf("%d requests to Stripe", n)
	}
}

// A request that got no answer is sent again under its key, and Stripe's saved answer to it
// creates one endpoint.
func TestStripeWebhookEndpointRetriesATimeoutUnderTheSameKey(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.dropAnswers("POST /v1/webhook_endpoints", 1)
	f := newStripeWebhookFixture(t, stripe)

	endpoint, _ := f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 2 || writes[0].IdempotencyKey != writes[1].IdempotencyKey {
		t.Fatalf("writes = %+v", writes)
	}
	if left := stripe.webhookEndpoints(); len(left) != 1 || endpoint.Status.EndpointID != "we_1" || endpoint.Status.Writes != 1 {
		t.Errorf("endpoints %+v, status %+v", left, endpoint.Status)
	}
	if got := signingSecretOf(t, f.client, endpoint); got != "whsec_fake1" {
		t.Errorf("signing secret = %q", got)
	}

	// Every attempt loses its answer: all under one key, and StripeAPIError. Stripe carried the
	// create out, so the next pass, a minute later, finds the endpoint without its secret and
	// replaces it rather than leaving it behind.
	stripe = newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.dropAnswers("POST /v1/webhook_endpoints", 100)
	f = newStripeWebhookFixture(t, stripe)
	endpoint, result := f.reconcile()
	wantReady(t, endpoint, metav1.ConditionFalse, "StripeAPIError")
	if endpoint.Status.Writes != 0 || result.RequeueAfter != paymentRetry {
		t.Errorf("requeue %v, status %+v", result.RequeueAfter, endpoint.Status)
	}
	lost := stripe.writes()
	for _, write := range lost {
		if write.IdempotencyKey != lost[0].IdempotencyKey {
			t.Errorf("keys differ across attempts: %q, %q", lost[0].IdempotencyKey, write.IdempotencyKey)
		}
	}
	stripe.dropAnswers("POST /v1/webhook_endpoints", 0)
	f.advance(paymentRetry)
	endpoint, _ = f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	if left := stripe.webhookEndpoints(); len(left) != 1 || left[0].ID != "we_2" || signingSecretOf(t, f.client, endpoint) != left[0].Secret {
		t.Errorf("endpoints %+v", left)
	}
}

// Stripe saves a 500 against its key, so the next attempt, a minute later, is sent under a new one
// and succeeds; the answered write's count is saved in status before it is built.
func TestStripeWebhookEndpointUsesANewKeyAfterA5xx(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.setFailure("POST /v1/webhook_endpoints", &fakeStripeError{http.StatusInternalServerError, "api_error", "", "Something went wrong"})
	f := newStripeWebhookFixture(t, stripe)

	endpoint, result := f.reconcile()
	if condition := wantReady(t, endpoint, metav1.ConditionFalse, "StripeAPIError"); !strings.Contains(condition.Message, "HTTP 500") {
		t.Errorf("message = %q", condition.Message)
	}
	if result.RequeueAfter != paymentRetry || endpoint.Status.Writes != 1 {
		t.Errorf("requeue %v, status %+v", result.RequeueAfter, endpoint.Status)
	}
	stripe.setFailure("POST /v1/webhook_endpoints", nil)
	f.reconcile()
	if n := len(stripe.writes()); n != 1 {
		t.Errorf("retried before a minute passed: %d writes", n)
	}

	f.advance(paymentRetry)
	endpoint, _ = f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 2 || writes[1].IdempotencyKey != writes[0].IdempotencyKey+"-1" {
		t.Fatalf("writes = %+v", writes)
	}
	if left := stripe.webhookEndpoints(); len(left) != 1 || endpoint.Status.EndpointID != "we_1" {
		t.Errorf("endpoints %+v, status %+v", left, endpoint.Status)
	}
}

// The same repair twice in one generation, the endpoint disabled in the Dashboard and then again,
// is two requests under two keys: the second is not answered with the first's saved answer.
func TestStripeWebhookEndpointRepairsTheSameDriftTwice(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	f.reconcile()
	disable := func(e *fakeStripeEndpoint) { e.Status = "disabled" }

	var keys []string
	for range 2 {
		stripe.editEndpoint("we_1", disable)
		f.advance(paymentRecheck)
		endpoint, _ := f.reconcile()
		wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
		if left := stripe.webhookEndpoints(); left[0].Status != "enabled" {
			t.Fatalf("repair %d left the endpoint %s", len(keys)+1, left[0].Status)
		}
		writes := stripe.writes()
		keys = append(keys, writes[len(writes)-1].IdempotencyKey)
	}
	if n := len(stripe.writes()); n != 3 || keys[0] == keys[1] {
		t.Errorf("%d writes, repair keys %v", n, keys)
	}
}

// An endpoint deleted in the Dashboard soon after its create is created again under a new key:
// the first create's saved answer would name the deleted endpoint.
func TestStripeWebhookEndpointRecreatesAnEndpointDeletedInTheDashboard(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	f.reconcile()
	stripe.deleteEndpoint("we_1")

	f.advance(paymentRecheck)
	endpoint, _ := f.reconcile()
	wantReady(t, endpoint, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 2 || writes[1].IdempotencyKey != writes[0].IdempotencyKey+"-1" {
		t.Fatalf("writes = %+v", writes)
	}
	if left := stripe.webhookEndpoints(); len(left) != 1 || left[0].ID != "we_2" || endpoint.Status.EndpointID != "we_2" {
		t.Errorf("endpoints %+v, status %+v", left, endpoint.Status)
	}
	if got := signingSecretOf(t, f.client, endpoint); got != "whsec_fake2" {
		t.Errorf("signing secret = %q", got)
	}
}

// Stripe's error can echo the key; neither the condition, the log nor an event carries it.
func TestStripeWebhookEndpointErrorsNeverCarryTheKey(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.setFailure("POST /v1/webhook_endpoints", &fakeStripeError{http.StatusBadRequest, "invalid_request_error", "url_invalid",
		"Invalid URL for key " + testStripeKey})
	f := newStripeWebhookFixture(t, stripe)

	endpoint, _ := f.reconcile()
	if condition := wantReady(t, endpoint, metav1.ConditionFalse, "StripeAPIError"); !strings.Contains(condition.Message, "Invalid URL for key <redacted>") {
		t.Errorf("message = %q", condition.Message)
	}
	logs := f.logs()
	if !slices.ContainsFunc(logs, func(line string) bool { return strings.Contains(line, "url_invalid") }) {
		t.Errorf("the failure was not logged: %v", logs)
	}
	for _, line := range append(logs, eventsOf(f.recorder)...) {
		if strings.Contains(line, testStripeKey) {
			t.Errorf("the key was logged: %q", line)
		}
	}
}

// Without verified credentials nothing is sent to Stripe.
func TestStripeWebhookEndpointWaitsForItsProvider(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	var provider oramav1alpha1.PaymentProvider
	if err := f.client.Get(context.Background(), types.NamespacedName{Namespace: "billing", Name: "stripe"}, &provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.Conditions[0].Status, provider.Status.Conditions[0].Reason = metav1.ConditionFalse, "AccountMismatch"
	if err := f.client.Status().Update(context.Background(), &provider); err != nil {
		t.Fatal(err)
	}
	endpoint, result := f.reconcile()
	if condition := wantReady(t, endpoint, metav1.ConditionFalse, "ProviderNotReady"); !strings.Contains(condition.Message, "AccountMismatch") {
		t.Errorf("message = %q", condition.Message)
	}
	if err := f.client.Delete(context.Background(), &provider); err != nil {
		t.Fatal(err)
	}
	endpoint, _ = f.reconcile()
	wantReady(t, endpoint, metav1.ConditionFalse, "ProviderNotReady")
	if n := len(stripe.received()); n != 0 || result.RequeueAfter != paymentRetry {
		t.Errorf("%d requests, requeue %v", n, result.RequeueAfter)
	}
}

// Deleting the object deletes our endpoint, or finds it already gone, and releases the object.
func TestStripeWebhookEndpointDeletesItsEndpoint(t *testing.T) {
	for name, deleteAnswer := range map[string]*fakeStripeError{
		"deleted":      nil,
		"already gone": {http.StatusNotFound, "invalid_request_error", "resource_missing", "No such webhook endpoint: 'we_1'"},
	} {
		t.Run(name, func(t *testing.T) {
			stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
			f := newStripeWebhookFixture(t, stripe)
			endpoint, _ := f.reconcile()
			stripe.setFailure("DELETE /v1/webhook_endpoints/we_1", deleteAnswer)
			if err := f.client.Delete(context.Background(), &endpoint); err != nil {
				t.Fatal(err)
			}

			if gone, _ := f.reconcile(); gone.Name != "" {
				t.Errorf("the object remains: finalizers %v", gone.Finalizers)
			}
			if writes := stripe.writes(); len(writes) != 2 || writes[1].Method != http.MethodDelete || writes[1].Path != "/v1/webhook_endpoints/we_1" {
				t.Errorf("writes = %+v", writes)
			}
		})
	}
}

// With the provider gone the finalizer does not block: it names the endpoint left behind in a
// Warning event and calls nothing in Stripe.
func TestStripeWebhookEndpointLeavesItsEndpointWhenTheProviderIsGone(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	endpoint, _ := f.reconcile()
	ctx := context.Background()
	if err := f.client.Delete(ctx, &oramav1alpha1.PaymentProvider{ObjectMeta: metav1.ObjectMeta{Name: "stripe", Namespace: "billing"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, &endpoint); err != nil {
		t.Fatal(err)
	}
	requests := len(stripe.received())

	if gone, _ := f.reconcile(); gone.Name != "" {
		t.Errorf("the object remains: finalizers %v", gone.Finalizers)
	}
	if n := len(stripe.received()); n != requests {
		t.Errorf("%d requests to Stripe after the provider went", n-requests)
	}
	events := eventsOf(f.recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning EndpointLeftBehind") || !strings.Contains(events[0], "we_1") {
		t.Errorf("events = %v", events)
	}
}

func TestStripeWebhookEndpointDeletionWaitsForCredentialsWithinGrace(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	f.reconcile()
	ctx := context.Background()
	var provider oramav1alpha1.PaymentProvider
	if err := f.client.Get(ctx, types.NamespacedName{Namespace: "billing", Name: "stripe"}, &provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.Conditions[0].Status = metav1.ConditionFalse
	provider.Status.Conditions[0].Reason = "StripeAPIError"
	provider.Status.Conditions[0].Message = "stripe GET /v1/account: HTTP 401"
	if err := f.client.Status().Update(ctx, &provider); err != nil {
		t.Fatal(err)
	}
	f.delete()
	requests := len(stripe.received())
	for _, elapsed := range []time.Duration{0, paymentDeleteGrace - time.Second} {
		f.advance(elapsed)
		endpoint, result := f.reconcile()
		condition := wantReady(t, endpoint, metav1.ConditionFalse, "ProviderNotReady")
		if !strings.Contains(condition.Message, "HTTP 401") || !slices.Contains(endpoint.Finalizers, paymentWebhookFinalizer) || result.RequeueAfter != paymentRetry {
			t.Errorf("condition %+v, finalizers %v, requeue %v", condition, endpoint.Finalizers, result.RequeueAfter)
		}
		if events := eventsOf(f.recorder); len(events) != 0 {
			t.Errorf("events within grace = %v", events)
		}
	}
	f.advance(2 * time.Second)
	if gone, result := f.reconcile(); gone.Name != "" || result.RequeueAfter != 0 {
		t.Errorf("object %q remains, requeue %v", gone.Name, result.RequeueAfter)
	}
	if len(stripe.received()) != requests {
		t.Error("called Stripe without verified credentials")
	}
	events := eventsOf(f.recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning EndpointLeftBehind") ||
		!strings.Contains(events[0], "HTTP 401") ||
		!strings.Contains(events[0], ", so Stripe webhook endpoint we_1 was not deleted: delete it in the Stripe Dashboard") {
		t.Errorf("events = %v", events)
	}
}

func TestStripeWebhookEndpointDeletionBoundsStripeFailures(t *testing.T) {
	for _, route := range []string{"GET /v1/webhook_endpoints", "DELETE /v1/webhook_endpoints/we_1"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
			t.Run(route+"/"+http.StatusText(status), func(t *testing.T) {
				stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
				f := newStripeWebhookFixture(t, stripe)
				f.reconcile()
				stripe.setFailure(route, &fakeStripeError{status, "api_error", "", "Error for key " + testStripeKey})
				f.delete()
				if status == http.StatusInternalServerError {
					for _, elapsed := range []time.Duration{0, paymentDeleteGrace - time.Second} {
						f.advance(elapsed)
						endpoint, result := f.reconcile()
						if !slices.Contains(endpoint.Finalizers, paymentWebhookFinalizer) || result.RequeueAfter != paymentRetry {
							t.Errorf("finalizers %v, requeue %v", endpoint.Finalizers, result.RequeueAfter)
						}
						if events := eventsOf(f.recorder); len(events) != 0 {
							t.Errorf("events within grace = %v", events)
						}
						if n := stripe.count(route); n == 0 {
							t.Error("did not attempt Stripe cleanup")
						}
						if !slices.ContainsFunc(f.logs(), func(line string) bool {
							return strings.Contains(line, "could not delete Stripe webhook endpoint") && strings.Contains(line, "HTTP 500")
						}) {
							t.Error("the cleanup retry was not logged")
						}
					}
					f.advance(time.Second)
				}
				if gone, result := f.reconcile(); gone.Name != "" || result.RequeueAfter != 0 {
					t.Errorf("object %q remains, requeue %v", gone.Name, result.RequeueAfter)
				}
				events := eventsOf(f.recorder)
				if len(events) != 1 || !strings.HasPrefix(events[0], "Warning EndpointLeftBehind stripe "+route+": HTTP ") ||
					!strings.Contains(events[0], ", so Stripe webhook endpoint we_1 was not deleted: delete it in the Stripe Dashboard") {
					t.Errorf("events = %v", events)
				}
				for _, line := range append(f.logs(), events...) {
					if strings.Contains(line, testStripeKey) {
						t.Errorf("the key was logged: %q", line)
					}
				}
				if len(stripe.webhookEndpoints()) != 1 {
					t.Error("expected the Stripe endpoint to remain")
				}
			})
		}
	}
}

func TestStripeWebhookEndpointDeletionWithoutAnEndpointID(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.setFailure("GET /v1/webhook_endpoints", &fakeStripeError{http.StatusInternalServerError, "api_error", "", "unavailable"})
	f := newStripeWebhookFixture(t, stripe)
	endpoint, _ := f.reconcile()
	if endpoint.Status.EndpointID != "" {
		t.Fatalf("endpoint ID = %q", endpoint.Status.EndpointID)
	}
	f.delete()
	if gone, result := f.reconcile(); gone.Name != "" || result.RequeueAfter != 0 {
		t.Errorf("object %q remains, requeue %v", gone.Name, result.RequeueAfter)
	}
	if events := eventsOf(f.recorder); len(events) != 0 {
		t.Errorf("events = %v", events)
	}
	if !slices.ContainsFunc(f.logs(), func(line string) bool {
		return strings.Contains(line, "could not delete Stripe webhook endpoint") && strings.Contains(line, "HTTP 500")
	}) {
		t.Error("the failed cleanup attempt was not logged")
	}
}

func TestStripeWebhookEndpointDeletionFindsItsEndpointWithoutAnID(t *testing.T) {
	for name, deleteAnswer := range map[string]*fakeStripeError{
		"deleted":       nil,
		"delete failed": {http.StatusInternalServerError, "api_error", "", "Error for key " + testStripeKey},
	} {
		t.Run(name, func(t *testing.T) {
			stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
			f := newStripeWebhookFixture(t, stripe)
			endpoint, _ := f.reconcile()
			// Stripe created the endpoint, but Kubernetes did not save its ID.
			endpoint.Status.EndpointID = ""
			if err := f.client.Status().Update(context.Background(), &endpoint); err != nil {
				t.Fatal(err)
			}
			stripe.setFailure("DELETE /v1/webhook_endpoints/we_1", deleteAnswer)
			f.delete()
			if gone, result := f.reconcile(); gone.Name != "" || result.RequeueAfter != 0 {
				t.Errorf("object %q remains, requeue %v", gone.Name, result.RequeueAfter)
			}
			if n := stripe.count("DELETE /v1/webhook_endpoints/we_1"); n != 1 {
				t.Errorf("delete attempts = %d, want 1", n)
			}
			if deleteAnswer == nil && len(stripe.webhookEndpoints()) != 0 {
				t.Error("the Stripe endpoint remains")
			}
			if events := eventsOf(f.recorder); len(events) != 0 {
				t.Errorf("events = %v", events)
			}
			if deleteAnswer != nil && !slices.ContainsFunc(f.logs(), func(line string) bool {
				return strings.Contains(line, "could not delete Stripe webhook endpoint") && strings.Contains(line, "HTTP 500")
			}) {
				t.Error("the failed cleanup attempt was not logged")
			}
			for _, line := range f.logs() {
				if strings.Contains(line, testStripeKey) {
					t.Errorf("the key was logged: %q", line)
				}
			}
		})
	}
}

func TestStripeWebhookEndpointDeletionWithoutAnIDOrCredentials(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeWebhookFixture(t, stripe)
	ctx := context.Background()
	var provider oramav1alpha1.PaymentProvider
	if err := f.client.Get(ctx, types.NamespacedName{Namespace: "billing", Name: "stripe"}, &provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.Conditions[0].Status = metav1.ConditionFalse
	provider.Status.Conditions[0].Reason = "StripeAPIError"
	if err := f.client.Status().Update(ctx, &provider); err != nil {
		t.Fatal(err)
	}
	endpoint, _ := f.reconcile()
	if endpoint.Status.EndpointID != "" {
		t.Fatalf("endpoint ID = %q", endpoint.Status.EndpointID)
	}
	f.delete()
	if gone, result := f.reconcile(); gone.Name != "" || result.RequeueAfter != 0 {
		t.Errorf("object %q remains, requeue %v", gone.Name, result.RequeueAfter)
	}
	if events := eventsOf(f.recorder); len(events) != 0 {
		t.Errorf("events = %v", events)
	}
	if n := len(stripe.received()); n != 0 {
		t.Errorf("Stripe calls = %d, want 0", n)
	}
}

func TestStripeWebhookEndpointDeletionRetriesKubernetesReadErrors(t *testing.T) {
	for _, resource := range []string{"provider", "secret"} {
		t.Run(resource, func(t *testing.T) {
			stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
			f := newStripeWebhookFixture(t, stripe)
			f.reconcile()
			f.delete()
			readError := errors.New("Kubernetes read failed")
			r := &StripeWebhookEndpointReconciler{
				Client: interceptor.NewClient(f.client, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						switch object.(type) {
						case *oramav1alpha1.PaymentProvider:
							if resource == "provider" {
								return readError
							}
						case *corev1.Secret:
							if resource == "secret" {
								return readError
							}
						}
						return c.Get(ctx, key, object, opts...)
					},
				}), Recorder: f.recorder, StripeAPIBase: stripe.url,
			}
			r.clock.now = func() time.Time { return time.Now().Add(paymentDeleteGrace + time.Minute) }
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: testWebhookKey}); !errors.Is(err, readError) {
				t.Errorf("reconcile error = %v, want %v", err, readError)
			}
			var endpoint oramav1alpha1.StripeWebhookEndpoint
			if err := f.client.Get(context.Background(), testWebhookKey, &endpoint); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(endpoint.Finalizers, paymentWebhookFinalizer) {
				t.Error("released the finalizer after a Kubernetes read error")
			}
			if events := eventsOf(f.recorder); len(events) != 0 {
				t.Errorf("events = %v", events)
			}
		})
	}
}
