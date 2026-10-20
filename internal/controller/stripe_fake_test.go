/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeStripe is a Stripe API double for the payment controllers' tests. It accepts one key and
// answers any other with a 401 whose message echoes it, as Stripe's can; it records every
// request; and a route named in fail ("GET /v1/account") gets that canned error instead.
//
// It keeps webhook endpoints and honours idempotency keys as Stripe does: a POST with a key it has
// seen gets the saved answer again, a saved 500 included, and a key reused with other parameters
// is refused. A route named in drop is carried out, and its answer saved, but the connection is
// closed before the answer is sent: a timeout, to the client.
type fakeStripe struct {
	mu         sync.Mutex
	url        string
	key        string
	accountID  string
	endpoints  []fakeStripeEndpoint
	fail       map[string]fakeStripeError
	drop       map[string]int
	idempotent map[string]fakeStripeAnswer
	created    int
	requests   []fakeStripeRequest
}

// fakeStripeEndpoint is a webhook endpoint as GET /v1/webhook_endpoints lists it. Secret is only in
// the answer to its create.
type fakeStripeEndpoint struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Livemode      bool              `json:"livemode"`
	EnabledEvents []string          `json:"enabled_events"`
	APIVersion    string            `json:"api_version,omitempty"`
	Status        string            `json:"status"`
	Description   string            `json:"description"`
	Metadata      map[string]string `json:"metadata"`
	Secret        string            `json:"-"`
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

// fakeStripeAnswer is an answer saved against an idempotency key, with the request it answered.
type fakeStripeAnswer struct {
	request string
	status  int
	body    []byte
}

func newFakeStripe(t *testing.T, key, accountID string) *fakeStripe {
	t.Helper()
	s := &fakeStripe{
		key: key, accountID: accountID,
		fail: map[string]fakeStripeError{}, drop: map[string]int{}, idempotent: map[string]fakeStripeAnswer{},
	}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(server.Close)
	s.url = server.URL
	return s
}

func (s *fakeStripe) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	idempotencyKey := r.Header.Get("Idempotency-Key")
	s.requests = append(s.requests, fakeStripeRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Form: form,
		Version: r.Header.Get("Stripe-Version"), IdempotencyKey: idempotencyKey,
	})
	route := r.Method + " " + r.URL.Path
	if key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); key != s.key {
		answerStripeError(w, fakeStripeError{http.StatusUnauthorized, "invalid_request_error", "", "Invalid API Key provided: " + key})
		return
	}
	keyed := r.Method == http.MethodPost && idempotencyKey != ""
	request := route + "?" + string(body)
	answer := httptest.NewRecorder()
	saved, seen := s.idempotent[idempotencyKey]
	switch {
	case keyed && seen && saved.request != request:
		answerStripeError(answer, fakeStripeError{http.StatusBadRequest, "idempotency_error", "",
			"Keys for idempotent requests can only be used with the same parameters they were first used with."})
	case keyed && seen:
		answer.Header().Set("Content-Type", "application/json")
		answer.Header().Set("Idempotent-Replayed", "true")
		answer.WriteHeader(saved.status)
		_, _ = answer.Write(saved.body)
	default:
		s.route(answer, r, route, form)
		if keyed {
			s.idempotent[idempotencyKey] = fakeStripeAnswer{request: request, status: answer.Code, body: answer.Body.Bytes()}
		}
	}
	if s.drop[route] > 0 {
		s.drop[route]--
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
		return
	}
	for name, values := range answer.Header() {
		w.Header()[name] = values
	}
	w.WriteHeader(answer.Code)
	_, _ = w.Write(answer.Body.Bytes())
}

func (s *fakeStripe) route(w http.ResponseWriter, r *http.Request, route string, form url.Values) {
	if failure, ok := s.fail[route]; ok {
		answerStripeError(w, failure)
		return
	}
	id, onEndpoint := strings.CutPrefix(r.URL.Path, "/v1/webhook_endpoints/")
	switch {
	case route == "GET /v1/account":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": s.accountID, "object": "account"})
	case route == "GET /v1/webhook_endpoints":
		s.listEndpoints(w, r.URL.Query())
	case route == "POST /v1/webhook_endpoints":
		s.created++
		endpoint := fakeStripeEndpoint{
			ID: fmt.Sprintf("we_%d", s.created), URL: form.Get("url"), EnabledEvents: form["enabled_events[]"],
			APIVersion: form.Get("api_version"), Status: "enabled", Description: form.Get("description"),
			Metadata: map[string]string{}, Secret: fmt.Sprintf("whsec_fake%d", s.created),
		}
		for field, values := range form {
			if name, ok := strings.CutPrefix(field, "metadata["); ok {
				endpoint.Metadata[strings.TrimSuffix(name, "]")] = values[0]
			}
		}
		s.endpoints = append(s.endpoints, endpoint)
		_ = json.NewEncoder(w).Encode(struct {
			fakeStripeEndpoint
			Secret string `json:"secret"`
		}{endpoint, endpoint.Secret})
	case onEndpoint && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
		i := s.endpointIndex(id)
		if i < 0 {
			answerStripeError(w, fakeStripeError{http.StatusNotFound, "invalid_request_error", "resource_missing", "No such webhook endpoint: '" + id + "'"})
			return
		}
		if r.Method == http.MethodDelete {
			s.endpoints = append(s.endpoints[:i], s.endpoints[i+1:]...)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "webhook_endpoint", "deleted": true})
			return
		}
		endpoint := &s.endpoints[i]
		if form.Has("url") {
			endpoint.URL = form.Get("url")
		}
		if form.Has("enabled_events[]") {
			endpoint.EnabledEvents = form["enabled_events[]"]
		}
		if form.Has("disabled") {
			endpoint.Status = map[bool]string{true: "disabled", false: "enabled"}[form.Get("disabled") == "true"]
		}
		_ = json.NewEncoder(w).Encode(endpoint)
	default:
		answerStripeError(w, fakeStripeError{http.StatusNotFound, "invalid_request_error", "resource_missing", "Unrecognized request URL"})
	}
}

// listEndpoints answers one page, after starting_after when it is given.
func (s *fakeStripe) listEndpoints(w http.ResponseWriter, query url.Values) {
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit <= 0 || limit > 100 {
		limit = 10
	}
	rest := s.endpoints
	if after := query.Get("starting_after"); after != "" {
		rest = rest[s.endpointIndex(after)+1:]
	}
	page := rest[:min(limit, len(rest))]
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "data": append([]fakeStripeEndpoint{}, page...), "has_more": len(page) < len(rest),
	})
}

func (s *fakeStripe) endpointIndex(id string) int {
	for i := range s.endpoints {
		if s.endpoints[i].ID == id {
			return i
		}
	}
	return -1
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

// writes are the requests that may change something: every POST and DELETE.
func (s *fakeStripe) writes() []fakeStripeRequest {
	var writes []fakeStripeRequest
	for _, request := range s.received() {
		if request.Method != http.MethodGet {
			writes = append(writes, request)
		}
	}
	return writes
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

// dropAnswers makes the next n requests for route lose their answer after being carried out.
func (s *fakeStripe) dropAnswers(route string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop[route] = n
}

// webhookEndpoints is a copy of the endpoints the account has, sorted by ID.
func (s *fakeStripe) webhookEndpoints() []fakeStripeEndpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	endpoints := append([]fakeStripeEndpoint{}, s.endpoints...)
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].ID < endpoints[j].ID })
	return endpoints
}

// editEndpoint changes an endpoint as a person in the Stripe Dashboard would.
func (s *fakeStripe) editEndpoint(id string, edit func(*fakeStripeEndpoint)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.endpointIndex(id); i >= 0 {
		edit(&s.endpoints[i])
	}
}

// deleteEndpoint deletes an endpoint as a person in the Stripe Dashboard would.
func (s *fakeStripe) deleteEndpoint(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.endpointIndex(id); i >= 0 {
		s.endpoints = append(s.endpoints[:i], s.endpoints[i+1:]...)
	}
}
