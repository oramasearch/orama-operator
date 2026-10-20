/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeStripe is a Stripe API double for the payment controllers' tests. It accepts one key and
// answers any other with a 401 whose message echoes it, as Stripe's can; it records every
// request; and a route named in fail ("GET /v1/account") gets that canned error instead.
type fakeStripe struct {
	mu        sync.Mutex
	url       string
	key       string
	accountID string
	endpoints []fakeStripeEndpoint
	fail      map[string]fakeStripeError
	requests  []fakeStripeRequest
}

// fakeStripeEndpoint is a webhook endpoint as GET /v1/webhook_endpoints lists it.
type fakeStripeEndpoint struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Livemode bool   `json:"livemode"`
}

type fakeStripeError struct {
	status  int
	errType string
	code    string
	message string
}

// fakeStripeRequest is one request as the fake received it; Form is a POST's body.
type fakeStripeRequest struct {
	Method         string
	Path           string
	Query          url.Values
	Form           url.Values
	Version        string
	IdempotencyKey string
}

func newFakeStripe(t *testing.T, key, accountID string) *fakeStripe {
	t.Helper()
	s := &fakeStripe{key: key, accountID: accountID, fail: map[string]fakeStripeError{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(server.Close)
	s.url = server.URL
	return s
}

func (s *fakeStripe) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = r.ParseForm()
	s.requests = append(s.requests, fakeStripeRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Form: r.PostForm,
		Version: r.Header.Get("Stripe-Version"), IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	route := r.Method + " " + r.URL.Path
	if key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); key != s.key {
		answerStripeError(w, fakeStripeError{http.StatusUnauthorized, "invalid_request_error", "", "Invalid API Key provided: " + key})
		return
	}
	if failure, ok := s.fail[route]; ok {
		answerStripeError(w, failure)
		return
	}
	switch route {
	case "GET /v1/account":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": s.accountID, "object": "account"})
	case "GET /v1/webhook_endpoints":
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 || limit > 100 {
			limit = 10
		}
		page := s.endpoints[:min(limit, len(s.endpoints))]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "data": append([]fakeStripeEndpoint{}, page...), "has_more": len(page) < len(s.endpoints),
		})
	default:
		answerStripeError(w, fakeStripeError{http.StatusNotFound, "invalid_request_error", "resource_missing", "Unrecognized request URL"})
	}
}

func answerStripeError(w http.ResponseWriter, failure fakeStripeError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(failure.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"type": failure.errType, "code": failure.code, "message": failure.message,
	}})
}

// count is how many requests the fake received for route ("GET /v1/account").
func (s *fakeStripe) count(route string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, request := range s.requests {
		if request.Method+" "+request.Path == route {
			n++
		}
	}
	return n
}

func (s *fakeStripe) received() []fakeStripeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeStripeRequest{}, s.requests...)
}

func (s *fakeStripe) setFailure(route string, failure *fakeStripeError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if failure == nil {
		delete(s.fail, route)
		return
	}
	s.fail[route] = *failure
}
