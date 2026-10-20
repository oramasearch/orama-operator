/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testCatalogPlan    = "cloud_v1.plan.4b0c7c3e-2f7a-4c1e-9d3b-5a6f0e1d2c3b"
	testCatalogMonthly = testCatalogPlan + ".monthly"
	testCatalogYearly  = testCatalogPlan + ".yearly"
	testCatalogAddon   = "cloud_v1.addon.9e1f0a2b-7c3d-4e5f-8a9b-0c1d2e3f4a5b.once"
)

var testCatalogKey = types.NamespacedName{Namespace: "billing", Name: "cloud-v1"}

// testCatalogSpec is a Cloud v1 sandbox catalog at version 3: a plan with a monthly and a yearly
// price, and an add-on with a one-time price.
func testCatalogSpec() oramav1alpha1.PaymentCatalogSpec {
	return oramav1alpha1.PaymentCatalogSpec{
		ProviderRef: corev1.LocalObjectReference{Name: "stripe"}, App: "cloud_v1", Version: 3,
		Products: []oramav1alpha1.PaymentProduct{
			{
				ID: "cloud_v1_plan_pro", Name: "Pro", Description: "The Pro plan",
				Metadata: map[string]string{"plan_id": "4b0c7c3e-2f7a-4c1e-9d3b-5a6f0e1d2c3b"},
				Prices: []oramav1alpha1.PaymentPrice{
					{LookupKey: testCatalogMonthly, Currency: "usd", UnitAmount: 4900, Interval: "month"},
					{LookupKey: testCatalogYearly, Currency: "usd", UnitAmount: 49000, Interval: "year"},
				},
			},
			{
				ID: "cloud_v1_addon_onboarding", Name: "Onboarding",
				Prices: []oramav1alpha1.PaymentPrice{
					{LookupKey: testCatalogAddon, Currency: "usd", UnitAmount: 150000, Metadata: map[string]string{"addon_type": "onboarding"}},
				},
			},
		},
	}
}

type stripeCatalogFixture struct {
	client    client.Client
	reconcile func() (oramav1alpha1.StripeCatalog, ctrl.Result)
	// edit changes the catalog's spec, as the abstract kind would.
	edit func(func(*oramav1alpha1.StripeCatalogSpec))
	// advance moves the controller's clock forward.
	advance func(time.Duration)
}

// newStripeCatalogFixture is StripeCatalog "cloud-v1" in namespace "billing" with testCatalogSpec,
// for a stripe PaymentProvider whose Credentials are True.
func newStripeCatalogFixture(t *testing.T, stripe *fakeStripe) stripeCatalogFixture {
	t.Helper()
	s := newLakeScheme(t)
	keyRef := oramav1alpha1.PaymentSecretKeyRef{Name: "stripe-operator", Key: "apiKey"}
	provider := &oramav1alpha1.PaymentProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "stripe", Namespace: "billing", UID: "provider-uid", Generation: 1},
		Spec: oramav1alpha1.PaymentProviderSpec{Type: "stripe", Stripe: &oramav1alpha1.StripePaymentProviderSpec{
			AccountID: testStripeAccount, APIKeySecretRef: keyRef,
		}},
		Status: oramav1alpha1.PaymentProviderStatus{Conditions: []metav1.Condition{{
			Type: paymentConditionCredentials, Status: metav1.ConditionTrue, Reason: "Verified", ObservedGeneration: 1,
			LastTransitionTime: metav1.Now(),
		}}},
	}
	catalog := &oramav1alpha1.StripeCatalog{
		ObjectMeta: metav1.ObjectMeta{Name: testCatalogKey.Name, Namespace: testCatalogKey.Namespace, UID: "catalog-uid", Generation: 1},
		Spec:       oramav1alpha1.StripeCatalogSpec{PaymentCatalogSpec: testCatalogSpec(), APIKeySecretRef: keyRef},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(provider, catalog, paymentKeySecret(testStripeKey)).
		WithStatusSubresource(provider, catalog).Build()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := &StripeCatalogReconciler{Client: c, Scheme: s, StripeAPIBase: stripe.url}
	r.clock.now = func() time.Time { return now }
	ctx := context.Background()
	return stripeCatalogFixture{
		client: c,
		reconcile: func() (oramav1alpha1.StripeCatalog, ctrl.Result) {
			t.Helper()
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: testCatalogKey})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var out oramav1alpha1.StripeCatalog
			if err := c.Get(ctx, testCatalogKey, &out); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			return out, result
		},
		edit: func(change func(*oramav1alpha1.StripeCatalogSpec)) {
			t.Helper()
			var current oramav1alpha1.StripeCatalog
			if err := c.Get(ctx, testCatalogKey, &current); err != nil {
				t.Fatal(err)
			}
			change(&current.Spec)
			if err := c.Update(ctx, &current); err != nil {
				t.Fatal(err)
			}
		},
		advance: func(d time.Duration) { now = now.Add(d) },
	}
}

// wantCatalogReady checks the Ready condition, and that it carries no key.
func wantCatalogReady(t *testing.T, catalog oramav1alpha1.StripeCatalog, status metav1.ConditionStatus, reason string) metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(catalog.Status.Conditions, paymentConditionReady)
	if condition == nil {
		t.Fatalf("no Ready condition: %+v", catalog.Status)
	}
	if testStripeKeyPattern.MatchString(condition.Message) {
		t.Errorf("the condition carries a key: %q", condition.Message)
	}
	if condition.Status != status || condition.Reason != reason {
		t.Errorf("Ready = %s %s (%q), want %s %s", condition.Status, condition.Reason, condition.Message, status, reason)
	}
	return *condition
}

// wantForm checks that a request's body is exactly want.
func wantForm(t *testing.T, request fakeStripeRequest, want map[string][]string) {
	t.Helper()
	for field, values := range want {
		if !slices.Equal(request.Form[field], values) {
			t.Errorf("%s %s: %s = %v, want %v", request.Method, request.Path, field, request.Form[field], values)
		}
	}
	if len(request.Form) != len(want) {
		t.Errorf("%s %s: form %v has fields beyond %v", request.Method, request.Path, request.Form, want)
	}
}

// writesSince are the writes after the first n.
func writesSince(stripe *fakeStripe, n int) []fakeStripeRequest {
	return stripe.writes()[n:]
}

// ownPrice is a price as the catalog creates it.
func ownPrice(product, lookupKey string, amount int64, interval string) fakeStripePrice {
	price := fakeStripePrice{
		Product: product, Currency: "usd", UnitAmount: amount, LookupKey: lookupKey, Active: true,
		Metadata: map[string]string{"orama_app": "cloud_v1", "orama_owner": "billing/cloud-v1"},
	}
	if interval != "" {
		price.Recurring = &fakeStripeRecurring{Interval: interval, IntervalCount: 1}
	}
	return price
}

var catalogKeyPattern = regexp.MustCompile(`^orama-catalog-uid-([a-z]+-[a-z]+)-(.+)-[0-9a-f]{16}(-[0-9]+)?$`)

func TestStripeCatalogRefusesCloudV1InLiveModeAndAllowsLake(t *testing.T) {
	for _, key := range []string{"sk_live_51OperatorKey", "rk_live_51OperatorKey"} {
		for _, app := range []string{"cloud_v1", "lake"} {
			t.Run(key[:7]+"/"+app, func(t *testing.T) {
				stripe := newFakeStripe(t, key, testStripeAccount)
				f := newStripeCatalogFixture(t, stripe)
				ctx := context.Background()
				var provider oramav1alpha1.PaymentProvider
				if err := f.client.Get(ctx, types.NamespacedName{Namespace: "billing", Name: "stripe"}, &provider); err != nil {
					t.Fatal(err)
				}
				provider.Spec.Stripe.Livemode = true
				if err := f.client.Update(ctx, &provider); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Update(ctx, paymentKeySecret(key)); err != nil {
					t.Fatal(err)
				}
				f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) {
					spec.App = app
					if app == "lake" {
						spec.Products = []oramav1alpha1.PaymentProduct{{ID: "lake_usage", Name: "Lake usage"}}
					}
				})

				catalog, _ := f.reconcile()
				if app == "lake" {
					wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
					if writes := stripe.writes(); len(writes) != 1 || writes[0].Form.Get("id") != "lake_usage" {
						t.Errorf("writes = %+v", writes)
					}
					return
				}
				condition := wantCatalogReady(t, catalog, metav1.ConditionFalse, "LiveModeRefused")
				if condition.ObservedGeneration != catalog.Generation || !strings.Contains(condition.Message, "Prod 1") {
					t.Errorf("Ready = %+v", condition)
				}
				if catalog.Status.Writes != 0 || len(catalog.Status.Prices) != 0 {
					t.Errorf("refused catalog status = %+v", catalog.Status)
				}
				second, _ := f.reconcile()
				if second.ResourceVersion != catalog.ResourceVersion {
					t.Error("an unchanged refusal was written again")
				}
				if requests := stripe.received(); len(requests) != 0 {
					t.Errorf("refused catalog sent %d requests to Stripe", len(requests))
				}
			})
		}
	}
}

// The first pass creates each product and its prices, in order, tagged with the app, the owner and
// (on products) the catalog version, under keys naming each request and ended by the count of
// writes before it; status maps every lookup key to the price now holding it.
func TestStripeCatalogCreatesProductsAndPricesFromEmptyAndReportsTheirIDs(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeCatalogFixture(t, stripe)

	catalog, result := f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	if result.RequeueAfter != stripeCatalogRecheck || len(catalog.Finalizers) != 0 {
		t.Errorf("requeue %v, finalizers %v; want a 6-hour recheck and no finalizer", result.RequeueAfter, catalog.Finalizers)
	}
	writes := stripe.writes()
	if len(writes) != 5 {
		t.Fatalf("writes = %+v", writes)
	}
	wantForm(t, writes[0], map[string][]string{
		"id": {"cloud_v1_plan_pro"}, "name": {"Pro"}, "description": {"The Pro plan"},
		"metadata[plan_id]":               {"4b0c7c3e-2f7a-4c1e-9d3b-5a6f0e1d2c3b"},
		"metadata[orama_app]":             {"cloud_v1"},
		"metadata[orama_owner]":           {"billing/cloud-v1"},
		"metadata[orama_catalog_version]": {"3"},
	})
	wantForm(t, writes[1], map[string][]string{
		"product": {"cloud_v1_plan_pro"}, "currency": {"usd"}, "unit_amount": {"4900"}, "recurring[interval]": {"month"},
		"lookup_key": {testCatalogMonthly}, "metadata[orama_app]": {"cloud_v1"}, "metadata[orama_owner]": {"billing/cloud-v1"},
	})
	wantForm(t, writes[3], map[string][]string{
		"id": {"cloud_v1_addon_onboarding"}, "name": {"Onboarding"},
		"metadata[orama_app]": {"cloud_v1"}, "metadata[orama_owner]": {"billing/cloud-v1"}, "metadata[orama_catalog_version]": {"3"},
	})
	wantForm(t, writes[4], map[string][]string{
		"product": {"cloud_v1_addon_onboarding"}, "currency": {"usd"}, "unit_amount": {"150000"}, "lookup_key": {testCatalogAddon},
		"metadata[addon_type]": {"onboarding"}, "metadata[orama_app]": {"cloud_v1"}, "metadata[orama_owner]": {"billing/cloud-v1"},
	})
	wantKeys := []struct{ operation, target, attempt string }{
		{"create-product", "cloud_v1_plan_pro", ""},
		{"create-price", testCatalogMonthly, "-1"},
		{"create-price", testCatalogYearly, "-2"},
		{"create-product", "cloud_v1_addon_onboarding", "-3"},
		{"create-price", testCatalogAddon, "-4"},
	}
	for i, want := range wantKeys {
		got := catalogKeyPattern.FindStringSubmatch(writes[i].IdempotencyKey)
		if got == nil || got[1] != want.operation || got[2] != want.target || got[3] != want.attempt || writes[i].Version != stripeAPIVersion {
			t.Errorf("write %d: key %q, version %q; want %+v", i, writes[i].IdempotencyKey, writes[i].Version, want)
		}
	}

	wantPrices := map[string]string{testCatalogMonthly: "price_1", testCatalogYearly: "price_2", testCatalogAddon: "price_3"}
	if !maps.Equal(catalog.Status.Prices, wantPrices) || catalog.Status.Writes != 5 {
		t.Errorf("status prices %v, writes %d; want %v and 5", catalog.Status.Prices, catalog.Status.Writes, wantPrices)
	}
	if price := stripe.price(t, "price_2"); price.Recurring == nil || price.Recurring.Interval != "year" || price.LookupKey != testCatalogYearly {
		t.Errorf("price_2 = %+v", price)
	}
}

// A catalog that matches makes no write and no status change: between rechecks no request at all,
// and its 6-hourly recheck only reads.
func TestStripeCatalogMakesNoWriteWhenTheAccountMatches(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeCatalogFixture(t, stripe)
	first, _ := f.reconcile()
	requests := len(stripe.received())

	second, result := f.reconcile()
	if n := len(stripe.received()); n != requests || second.ResourceVersion != first.ResourceVersion || result.RequeueAfter != stripeCatalogRecheck {
		t.Errorf("between rechecks: %d requests, status rewritten %t, requeue %v", n-requests, second.ResourceVersion != first.ResourceVersion, result.RequeueAfter)
	}
	f.advance(stripeCatalogRecheck)
	third, result := f.reconcile()
	wantCatalogReady(t, third, metav1.ConditionTrue, "Configured")
	if n := len(stripe.writes()); n != 5 || third.ResourceVersion != first.ResourceVersion || result.RequeueAfter != stripeCatalogRecheck {
		t.Errorf("the recheck made %d writes in all, status rewritten %t, requeue %v", n, third.ResourceVersion != first.ResourceVersion, result.RequeueAfter)
	}
	if len(stripe.received()) == requests {
		t.Error("the recheck did not read the account")
	}
}

// A changed amount is a new price that takes the lookup key over, and the old price is archived,
// not deleted.
func TestStripeCatalogReplacesAChangedPriceAndArchivesTheOldOne(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeCatalogFixture(t, stripe)
	f.reconcile()

	f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) { spec.Products[0].Prices[0].UnitAmount = 5900 })
	catalog, _ := f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	writes := writesSince(stripe, 5)
	if len(writes) != 2 || writes[0].Path != "/v1/prices" || writes[1].Path != "/v1/prices/price_1" {
		t.Fatalf("writes = %+v", writes)
	}
	wantForm(t, writes[0], map[string][]string{
		"product": {"cloud_v1_plan_pro"}, "currency": {"usd"}, "unit_amount": {"5900"}, "recurring[interval]": {"month"},
		"lookup_key": {testCatalogMonthly}, "transfer_lookup_key": {"true"},
		"metadata[orama_app]": {"cloud_v1"}, "metadata[orama_owner]": {"billing/cloud-v1"},
	})
	wantForm(t, writes[1], map[string][]string{"active": {"false"}})
	if !strings.Contains(writes[0].IdempotencyKey, "-replace-price-price_1-") || !strings.Contains(writes[1].IdempotencyKey, "-archive-price-price_1-") {
		t.Errorf("keys %q, %q", writes[0].IdempotencyKey, writes[1].IdempotencyKey)
	}
	if old := stripe.price(t, "price_1"); old.Active || old.LookupKey != "" {
		t.Errorf("old price = %+v", old)
	}
	if replacement := stripe.price(t, "price_4"); !replacement.Active || replacement.LookupKey != testCatalogMonthly || replacement.UnitAmount != 5900 {
		t.Errorf("replacement = %+v", replacement)
	}
	if got := catalog.Status.Prices[testCatalogMonthly]; got != "price_4" || stripe.priceCount() != 4 {
		t.Errorf("status price %q, %d prices", got, stripe.priceCount())
	}
}

// A price the spec removes is archived and keeps its lookup key, so adding it back reactivates it;
// a product the catalog created and the spec removes is archived, with the catalog's version.
func TestStripeCatalogArchivesWhatTheSpecRemoves(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeCatalogFixture(t, stripe)
	f.reconcile()

	f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) { spec.Products[0].Prices = spec.Products[0].Prices[:1] })
	catalog, _ := f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	writes := writesSince(stripe, 5)
	if len(writes) != 1 || writes[0].Path != "/v1/prices/price_2" {
		t.Fatalf("writes = %+v", writes)
	}
	wantForm(t, writes[0], map[string][]string{"active": {"false"}})
	if _, listed := catalog.Status.Prices[testCatalogYearly]; listed || stripe.price(t, "price_2").Active {
		t.Errorf("status prices %v, price_2 %+v", catalog.Status.Prices, stripe.price(t, "price_2"))
	}

	f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) {
		spec.Products[0].Prices = testCatalogSpec().Products[0].Prices
	})
	catalog, _ = f.reconcile()
	writes = writesSince(stripe, 6)
	if len(writes) != 1 || writes[0].Path != "/v1/prices/price_2" || catalog.Status.Prices[testCatalogYearly] != "price_2" {
		t.Fatalf("writes = %+v, status %v", writes, catalog.Status.Prices)
	}
	wantForm(t, writes[0], map[string][]string{"active": {"true"}})

	f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) { spec.Products = spec.Products[:1] })
	catalog, _ = f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	writes = writesSince(stripe, 7)
	if len(writes) != 1 || writes[0].Path != "/v1/products/cloud_v1_addon_onboarding" {
		t.Fatalf("writes = %+v", writes)
	}
	wantForm(t, writes[0], map[string][]string{"active": {"false"}, "metadata[orama_catalog_version]": {"3"}})
	if stripe.product(t, "cloud_v1_addon_onboarding").Active || len(stripe.products) != 2 {
		t.Errorf("products = %+v", stripe.products)
	}
}

// A pass that created a replacement price, which took the lookup key, and stopped before archiving
// the old one: the next pass creates nothing and archives the old price.
func TestStripeCatalogArchivesThePriceAnInterruptedReplacementLeftBehind(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeCatalogFixture(t, stripe)
	f.reconcile()
	replacement := stripe.addPrice(ownPrice("cloud_v1_plan_pro", testCatalogMonthly, 5900, "month"))
	f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) { spec.Products[0].Prices[0].UnitAmount = 5900 })

	catalog, _ := f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	writes := writesSince(stripe, 5)
	if len(writes) != 1 || writes[0].Path != "/v1/prices/price_1" {
		t.Fatalf("writes = %+v", writes)
	}
	wantForm(t, writes[0], map[string][]string{"active": {"false"}})
	if catalog.Status.Prices[testCatalogMonthly] != replacement || stripe.price(t, "price_1").Active {
		t.Errorf("status prices %v, price_1 %+v", catalog.Status.Prices, stripe.price(t, "price_1"))
	}
}

// A product of another app, or one made by hand, is never adopted or changed, and the catalog
// writes nothing at all: not even the products before it.
func TestStripeCatalogNeverWritesAnotherAppsProduct(t *testing.T) {
	for name, metadata := range map[string]map[string]string{
		"made by hand": nil,
		"lake's":       {"orama_app": "lake", "lake_instance": "eu.lake.live"},
	} {
		t.Run(name, func(t *testing.T) {
			stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
			stripe.products = []fakeStripeProduct{{ID: "cloud_v1_addon_onboarding", Name: "By hand", Metadata: metadata}}
			f := newStripeCatalogFixture(t, stripe)

			catalog, result := f.reconcile()
			if condition := wantCatalogReady(t, catalog, metav1.ConditionFalse, "ProductTaken"); !strings.Contains(condition.Message, "cloud_v1_addon_onboarding") {
				t.Errorf("message %q does not name the product", condition.Message)
			}
			if n := len(stripe.writes()); n != 0 || result.RequeueAfter != paymentRetry {
				t.Errorf("%d writes, requeue %v", n, result.RequeueAfter)
			}
			if product := stripe.product(t, "cloud_v1_addon_onboarding"); product.Active || product.Name != "By hand" {
				t.Errorf("the product changed: %+v", product)
			}
		})
	}
}

// A lookup key held by another app's price, or by a price on a product that is not the catalog's,
// is left alone: never transferred, never archived.
func TestStripeCatalogLeavesALookupKeyAnotherAppHolds(t *testing.T) {
	ownProduct := fakeStripeProduct{ID: "cloud_v1_plan_pro", Name: "Pro", Description: "The Pro plan", Active: true, Metadata: map[string]string{
		"plan_id": "4b0c7c3e-2f7a-4c1e-9d3b-5a6f0e1d2c3b", "orama_app": "cloud_v1", "orama_owner": "billing/cloud-v1", "orama_catalog_version": "3",
	}}
	lakeProduct := fakeStripeProduct{ID: "lake_usage", Name: "Lake usage", Active: true, Metadata: map[string]string{"orama_app": "lake"}}
	for name, tc := range map[string]struct {
		product  string
		metadata map[string]string
	}{
		"another app's price on its own product":       {"lake_usage", map[string]string{"orama_app": "lake"}},
		"another app's price on the catalog's product": {"cloud_v1_plan_pro", map[string]string{"orama_app": "lake"}},
		"this app's price on another product":          {"lake_usage", map[string]string{"orama_app": "cloud_v1"}},
	} {
		t.Run(name, func(t *testing.T) {
			stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
			stripe.products = []fakeStripeProduct{ownProduct, lakeProduct}
			stripe.prices = []fakeStripePrice{{
				ID: "price_taken", Product: tc.product, Currency: "usd", UnitAmount: 100, LookupKey: testCatalogYearly,
				Recurring: &fakeStripeRecurring{Interval: "year", IntervalCount: 1}, Active: true, Metadata: tc.metadata,
			}}
			f := newStripeCatalogFixture(t, stripe)

			catalog, _ := f.reconcile()
			if condition := wantCatalogReady(t, catalog, metav1.ConditionFalse, "PriceTaken"); !strings.Contains(condition.Message, "price_taken") {
				t.Errorf("message %q does not name the price", condition.Message)
			}
			if n := len(stripe.writes()); n != 0 {
				t.Errorf("%d writes", n)
			}
			if price := stripe.price(t, "price_taken"); !price.Active || price.LookupKey != testCatalogYearly {
				t.Errorf("the price changed: %+v", price)
			}
		})
	}
}

// A product a newer catalog version wrote is left as it is, and so is everything else in the
// catalog: Ready, Superseded. That holds for a listed product and for one the catalog created and
// no longer lists, which a newer definition lists again.
func TestStripeCatalogChangesNothingAfterANewerVersion(t *testing.T) {
	newer := map[string]string{"orama_app": "cloud_v1", "orama_owner": "billing/cloud-v1", "orama_catalog_version": "4"}
	for name, product := range map[string]fakeStripeProduct{
		"a listed product":  {ID: "cloud_v1_addon_onboarding", Name: "Onboarding, renamed by version 4", Active: true, Metadata: newer},
		"a removed product": {ID: "cloud_v1_plan_team", Name: "Team, added by version 4", Active: true, Metadata: newer},
	} {
		t.Run(name, func(t *testing.T) {
			stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
			stripe.products = []fakeStripeProduct{product}
			f := newStripeCatalogFixture(t, stripe)

			catalog, result := f.reconcile()
			if condition := wantCatalogReady(t, catalog, metav1.ConditionTrue, "Superseded"); !strings.Contains(condition.Message, "version 4") {
				t.Errorf("message = %q", condition.Message)
			}
			if n := len(stripe.writes()); n != 0 || result.RequeueAfter != stripeCatalogRecheck {
				t.Errorf("%d writes, requeue %v", n, result.RequeueAfter)
			}
			if got := stripe.product(t, product.ID); got.Name != product.Name || !got.Active {
				t.Errorf("the product changed: %+v", got)
			}
		})
	}
}

// An existing product of this app gets only what differs: an older catalog version is brought to
// this one, an archived product is reactivated, a metadata key the spec does not have is removed,
// and orama_owner, which names the catalog that created it, is kept.
func TestStripeCatalogUpdatesOnlyWhatDiffers(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.products = []fakeStripeProduct{{ID: "cloud_v1_plan_pro", Name: "Pro", Description: "Old", Active: false, Metadata: map[string]string{
		"orama_app": "cloud_v1", "orama_owner": "billing/other", "orama_catalog_version": "2", "note": "by hand",
	}}}
	f := newStripeCatalogFixture(t, stripe)

	catalog, _ := f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) == 0 || writes[0].Path != "/v1/products/cloud_v1_plan_pro" {
		t.Fatalf("writes = %+v", writes)
	}
	wantForm(t, writes[0], map[string][]string{
		"description": {"The Pro plan"}, "active": {"true"}, "metadata[orama_catalog_version]": {"3"}, "metadata[note]": {""},
		"metadata[plan_id]": {"4b0c7c3e-2f7a-4c1e-9d3b-5a6f0e1d2c3b"},
	})
	product := stripe.product(t, "cloud_v1_plan_pro")
	if !product.Active || product.Metadata["orama_owner"] != "billing/other" || product.Metadata["note"] != "" {
		t.Errorf("product = %+v", product)
	}
}

// A request that got no answer is sent again under its key, and Stripe's saved answer to it makes
// one price.
func TestStripeCatalogRetriesATimeoutUnderTheSameKey(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.dropAnswers("POST /v1/prices", 1)
	f := newStripeCatalogFixture(t, stripe)

	catalog, _ := f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 6 || writes[1].Path != "/v1/prices" || writes[1].IdempotencyKey != writes[2].IdempotencyKey {
		t.Fatalf("writes = %+v", writes)
	}
	if n := stripe.priceCount(); n != 3 || catalog.Status.Prices[testCatalogMonthly] != "price_1" || catalog.Status.Writes != 5 {
		t.Errorf("%d prices, status %+v", n, catalog.Status)
	}
}

// Stripe saves a 500 against its key, so the next attempt, a minute later, is sent under a new one
// and is carried out.
func TestStripeCatalogUsesANewKeyAfterA5xx(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	stripe.setFailure("POST /v1/products", &fakeStripeError{http.StatusInternalServerError, "api_error", "", "Something went wrong"})
	f := newStripeCatalogFixture(t, stripe)

	catalog, result := f.reconcile()
	if condition := wantCatalogReady(t, catalog, metav1.ConditionFalse, "StripeAPIError"); !strings.Contains(condition.Message, "HTTP 500") {
		t.Errorf("message = %q", condition.Message)
	}
	if result.RequeueAfter != paymentRetry || catalog.Status.Writes != 1 {
		t.Errorf("requeue %v, status %+v", result.RequeueAfter, catalog.Status)
	}
	stripe.setFailure("POST /v1/products", nil)
	f.advance(paymentRetry)
	catalog, _ = f.reconcile()
	wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
	writes := stripe.writes()
	if len(writes) != 6 || writes[1].IdempotencyKey != writes[0].IdempotencyKey+"-1" {
		t.Fatalf("writes = %+v", writes)
	}
}

// The same repair twice in one generation, the product archived in the Dashboard and then again, is
// two requests under two keys: the second is not answered with the first's saved answer.
func TestStripeCatalogRepairsTheSameDriftTwice(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeCatalogFixture(t, stripe)
	f.reconcile()

	var keys []string
	for range 2 {
		stripe.editProduct("cloud_v1_plan_pro", func(p *fakeStripeProduct) { p.Active = false })
		f.advance(stripeCatalogRecheck)
		catalog, _ := f.reconcile()
		wantCatalogReady(t, catalog, metav1.ConditionTrue, "Configured")
		if !stripe.product(t, "cloud_v1_plan_pro").Active {
			t.Fatalf("repair %d left the product archived", len(keys)+1)
		}
		writes := stripe.writes()
		last := writes[len(writes)-1]
		wantForm(t, last, map[string][]string{"active": {"true"}})
		keys = append(keys, last.IdempotencyKey)
	}
	if n := len(stripe.writes()); n != 7 || keys[0] == keys[1] {
		t.Errorf("%d writes, repair keys %v", n, keys)
	}
}

// A lookup key under two products would move between them on every pass, and an orama_ metadata
// key would overwrite the operator's tags: both refused before Stripe is called. So is everything
// while the provider's credentials are not verified.
func TestStripeCatalogCallsNothingForAnInvalidSpecOrAnUnverifiedProvider(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	f := newStripeCatalogFixture(t, stripe)
	for problem, change := range map[string]func(*oramav1alpha1.StripeCatalogSpec){
		testCatalogMonthly: func(spec *oramav1alpha1.StripeCatalogSpec) {
			spec.Products[1].Prices = append(spec.Products[1].Prices, spec.Products[0].Prices[0])
		},
		"orama_owner": func(spec *oramav1alpha1.StripeCatalogSpec) {
			spec.Products[1].Prices[0].Metadata = map[string]string{"orama_owner": "billing/other"}
		},
	} {
		f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) { spec.PaymentCatalogSpec = testCatalogSpec(); change(spec) })
		catalog, _ := f.reconcile()
		if condition := wantCatalogReady(t, catalog, metav1.ConditionFalse, "InvalidSpec"); !strings.Contains(condition.Message, problem) {
			t.Errorf("message = %q", condition.Message)
		}
	}

	f.edit(func(spec *oramav1alpha1.StripeCatalogSpec) { spec.PaymentCatalogSpec = testCatalogSpec() })
	var provider oramav1alpha1.PaymentProvider
	ctx := context.Background()
	if err := f.client.Get(ctx, types.NamespacedName{Namespace: "billing", Name: "stripe"}, &provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.Conditions[0].Status, provider.Status.Conditions[0].Reason = metav1.ConditionFalse, "AccountMismatch"
	if err := f.client.Status().Update(ctx, &provider); err != nil {
		t.Fatal(err)
	}
	catalog, result := f.reconcile()
	if condition := wantCatalogReady(t, catalog, metav1.ConditionFalse, "ProviderNotReady"); !strings.Contains(condition.Message, "AccountMismatch") {
		t.Errorf("message = %q", condition.Message)
	}
	if n := len(stripe.received()); n != 0 || result.RequeueAfter != paymentRetry {
		t.Errorf("%d requests, requeue %v", n, result.RequeueAfter)
	}
}
