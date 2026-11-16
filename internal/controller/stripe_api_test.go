/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Nested keys are bracketed, lists repeat key[], and the body is the same whatever order the
// fields were written in.
func TestStripeFormEncodesNestedKeysInOneOrder(t *testing.T) {
	if got := stripeField("price_data", "recurring", "interval"); got != "price_data[recurring][interval]" {
		t.Errorf("stripeField = %q", got)
	}
	first := url.Values{}
	first.Set("url", "https://eu.lakelive.example/stripe?x=1")
	first.Set(stripeField("metadata", "orama_owner"), "lake-eu/lake")
	first[stripeField("enabled_events", "")] = []string{"customer.subscription.updated", "invoice.paid"}
	first.Set(stripeField("price_data", "product"), "lake_usage")
	second := url.Values{}
	second.Set(stripeField("price_data", "product"), "lake_usage")
	second[stripeField("enabled_events", "")] = []string{"customer.subscription.updated", "invoice.paid"}
	second.Set(stripeField("metadata", "orama_owner"), "lake-eu/lake")
	second.Set("url", "https://eu.lakelive.example/stripe?x=1")

	want := "enabled_events%5B%5D=customer.subscription.updated&enabled_events%5B%5D=invoice.paid" +
		"&metadata%5Borama_owner%5D=lake-eu%2Flake&price_data%5Bproduct%5D=lake_usage" +
		"&url=https%3A%2F%2Feu.lakelive.example%2Fstripe%3Fx%3D1"
	if first.Encode() != want || second.Encode() != want {
		t.Errorf("encoded\n %s\n %s\nwant\n %s", first.Encode(), second.Encode(), want)
	}
	decoded, err := url.ParseQuery(want)
	if err != nil || decoded.Get("metadata[orama_owner]") != "lake-eu/lake" || len(decoded["enabled_events[]"]) != 2 {
		t.Errorf("decoded = %v, %v", decoded, err)
	}
}

// A key is orama-<uid>-<operation>-<target>-<16 hex of the body's sha256>, the same for the same
// body, different for another body or target, with the attempt as a suffix after a 5xx.
func TestStripeIdempotencyKeyNamesOneRequest(t *testing.T) {
	form := url.Values{"url": {"https://eu.lakelive.example/stripe"}, stripeField("enabled_events", ""): {"invoice.paid"}}
	key := stripeIdempotencyKey("0d5c8e1a-uid", "create", "new", form, 0)
	sum := sha256.Sum256([]byte(form.Encode()))
	if want := fmt.Sprintf("orama-0d5c8e1a-uid-create-new-%x", sum[:8]); key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
	if !regexp.MustCompile(`^orama-0d5c8e1a-uid-create-new-[0-9a-f]{16}$`).MatchString(key) {
		t.Errorf("key %q has the wrong shape", key)
	}
	reordered := url.Values{stripeField("enabled_events", ""): {"invoice.paid"}, "url": {"https://eu.lakelive.example/stripe"}}
	if again := stripeIdempotencyKey("0d5c8e1a-uid", "create", "new", reordered, 0); again != key {
		t.Errorf("the same body gave %q, then %q", key, again)
	}
	changed := url.Values{"url": {"https://eu.lakelive.example/stripe"}, stripeField("enabled_events", ""): {"invoice.paid", "invoice.created"}}
	if other := stripeIdempotencyKey("0d5c8e1a-uid", "create", "new", changed, 0); other == key {
		t.Error("another body gave the same key")
	}
	if recreate := stripeIdempotencyKey("0d5c8e1a-uid", "create", "recreate-we_123", form, 0); recreate == key ||
		!strings.Contains(recreate, "-create-recreate-we_123-") {
		t.Errorf("recreate key = %q", recreate)
	}
	if retried := stripeIdempotencyKey("0d5c8e1a-uid", "create", "new", form, 2); retried != key+"-2" {
		t.Errorf("attempt 2 = %q, want %q", retried, key+"-2")
	}
}

// Every request carries the key as a Bearer token and a Stripe-Version; only a POST carries an
// idempotency key and a form body.
func TestStripeRequestsCarryAuthVersionAndKeyOnlyOnPost(t *testing.T) {
	stripe := newFakeStripe(t, "rk_test_operator", "acct_1Orama")
	api := stripeAPI{Base: stripe.url, Key: "rk_test_operator"}
	ctx := context.Background()
	if _, err := stripeDo[stripeAccount](ctx, api, stripeCall{Method: http.MethodGet, Path: "/v1/account", IdempotencyKey: "ignored"}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{stripeField("metadata", "orama_app"): {"lake"}}
	_, _ = stripeDo[stripeAccount](ctx, api, stripeCall{
		Method: http.MethodPost, Path: "/v1/webhook_endpoints", Form: form, Version: "2025-03-31.basil", IdempotencyKey: "orama-k",
	})
	_, _ = stripeDo[stripeAccount](ctx, api, stripeCall{Method: http.MethodDelete, Path: "/v1/webhook_endpoints/we_1", IdempotencyKey: "ignored"})

	requests := stripe.received()
	if len(requests) != 3 {
		t.Fatalf("requests = %+v", requests)
	}
	get, post, del := requests[0], requests[1], requests[2]
	if get.Version != stripeAPIVersion || get.IdempotencyKey != "" {
		t.Errorf("GET version %q key %q, want %q and none", get.Version, get.IdempotencyKey, stripeAPIVersion)
	}
	if post.Version != "2025-03-31.basil" || post.IdempotencyKey != "orama-k" || post.Form.Get("metadata[orama_app]") != "lake" {
		t.Errorf("POST = %+v", post)
	}
	if del.IdempotencyKey != "" {
		t.Errorf("DELETE carried idempotency key %q", del.IdempotencyKey)
	}
}

// Stripe's message is kept, with the key replaced, except on 401 and 403, where it is dropped;
// the status, type and code always remain.
func TestStripeErrorsNeverCarryTheKey(t *testing.T) {
	const key = "rk_live_51SecretValue"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bad":
			answerStripeError(w, fakeStripeError{http.StatusBadRequest, "invalid_request_error", "url_invalid",
				"Invalid URL for key " + key + " (signing secret whsec_abc123)"})
		case "/v1/unauthorized":
			answerStripeError(w, fakeStripeError{http.StatusUnauthorized, "invalid_request_error", "", "Invalid API Key provided: " + key})
		case "/v1/forbidden":
			answerStripeError(w, fakeStripeError{http.StatusForbidden, "invalid_request_error", "", "The provided key '" + key + "' does not have the required permissions"})
		}
	}))
	t.Cleanup(server.Close)
	api := stripeAPI{Base: server.URL, Key: key}

	_, err := stripeDo[stripeAccount](context.Background(), api, stripeCall{Method: http.MethodGet, Path: "/v1/bad"})
	var apiErr *stripeAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || apiErr.Type != "invalid_request_error" || apiErr.Code != "url_invalid" {
		t.Fatalf("err = %#v", err)
	}
	if strings.Contains(err.Error(), "rk_live_") || strings.Contains(err.Error(), "whsec_abc") || !strings.Contains(err.Error(), "Invalid URL for key <redacted>") {
		t.Errorf("400 error = %q", err)
	}
	for _, path := range []string{"/v1/unauthorized", "/v1/forbidden"} {
		_, err := stripeDo[stripeAccount](context.Background(), api, stripeCall{Method: http.MethodGet, Path: path})
		if !errors.As(err, &apiErr) || apiErr.Message != "" || apiErr.Type != "invalid_request_error" || strings.Contains(err.Error(), "rk_live_") {
			t.Errorf("%s error = %q (%#v)", path, err, apiErr)
		}
	}
}

// A request that gets no answer is sent again with the same idempotency key; a POST without a key,
// or a request that got a 5xx, is not.
func TestStripeRepeatsOnlyAnUnansweredRequestWithItsKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	answer500 := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		first, fail := len(keys) == 1, answer500
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case fail:
			answerStripeError(w, fakeStripeError{http.StatusInternalServerError, "api_error", "", "try again"})
		case first:
			// Carried out, but the connection drops before the answer: a timeout, to the client.
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		default:
			_, _ = w.Write([]byte(`{"id":"we_1"}`))
		}
	}))
	t.Cleanup(server.Close)
	api := stripeAPI{Base: server.URL, Key: "rk_test_operator"}
	ctx := context.Background()
	reset := func(fail bool) {
		mu.Lock()
		defer mu.Unlock()
		keys, answer500 = nil, fail
	}
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, keys...)
	}

	got, err := stripeDo[stripeAccount](ctx, api, stripeCall{Method: http.MethodPost, Path: "/v1/webhook_endpoints", IdempotencyKey: "orama-k"})
	if keys := sent(); err != nil || got.ID != "we_1" || len(keys) != 2 || keys[0] != "orama-k" || keys[1] != "orama-k" {
		t.Errorf("keyed POST: %+v, %v, keys %v", got, err, keys)
	}
	reset(false)
	if _, err := stripeDo[stripeAccount](ctx, api, stripeCall{Method: http.MethodPost, Path: "/v1/webhook_endpoints"}); err == nil || len(sent()) != 1 {
		t.Errorf("POST without a key: %v, sent %d times", err, len(sent()))
	}
	reset(true)
	_, err = stripeDo[stripeAccount](ctx, api, stripeCall{Method: http.MethodPost, Path: "/v1/webhook_endpoints", IdempotencyKey: "orama-k"})
	var apiErr *stripeAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 || len(sent()) != 1 {
		t.Errorf("5xx: %v, sent %d times", err, len(sent()))
	}
}
