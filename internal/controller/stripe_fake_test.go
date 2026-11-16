// SPDX-License-Identifier: MIT

package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
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
// acceptKey adds another key, of this account or of another one. A key of another account reads
// only GET /v1/account; every other route answers 404, as Stripe answers for an object of another
// account.
//
// It keeps webhook endpoints, products and prices, and honours idempotency keys as Stripe does: a
// POST with a key it has seen gets the saved answer again, a saved 500 included, and a key reused
// with other parameters is refused. A route named in drop is carried out, and its answer saved, but
// the connection is closed before the answer is sent: a timeout, to the client.
type fakeStripe struct {
	mu            sync.Mutex
	url           string
	key           string
	accountID     string
	livemode      bool
	otherKeys     map[string]string
	endpoints     []fakeStripeEndpoint
	products      []fakeStripeProduct
	prices        []fakeStripePrice
	fail          map[string]fakeStripeError
	drop          map[string]int
	idempotent    map[string]fakeStripeAnswer
	created       int
	pricesCreated int
	requests      []fakeStripeRequest
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
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	account, accepted := s.otherKeys[key]
	switch {
	case key != s.key && !accepted:
		answerStripeError(w, fakeStripeError{http.StatusUnauthorized, "invalid_request_error", "", "Invalid API Key provided: " + key})
		return
	case accepted && account != s.accountID && route == "GET /v1/account":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": account, "object": "account"})
		return
	case accepted && account != s.accountID:
		answerStripeError(w, fakeStripeError{http.StatusNotFound, "invalid_request_error", "resource_missing", "No such object in this account"})
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
	case route == "POST /v1/payment_intents":
		// The permission probe must never supply an amount or create a payment.
		if len(form) != 2 || form.Get("currency") != "usd" || form.Get("payment_method_types[]") != "card" {
			answerStripeError(w, fakeStripeError{http.StatusBadRequest, "invalid_request_error", "", "unexpected PaymentIntent probe fields"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
			"type": "invalid_request_error", "code": "parameter_missing", "param": "amount", "message": "Missing required param: amount.",
		}})
	case strings.HasPrefix(r.URL.Path, "/v1/products") || strings.HasPrefix(r.URL.Path, "/v1/prices"):
		s.routeCatalog(w, r, form)
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

// acceptKey makes the fake accept key as a key of accountID.
func (s *fakeStripe) acceptKey(key, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.otherKeys == nil {
		s.otherKeys = map[string]string{}
	}
	s.otherKeys[key] = accountID
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

// fakeStripeProduct is a product as GET /v1/products/{id} returns it.
type fakeStripeProduct struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Active      bool              `json:"active"`
	Livemode    bool              `json:"livemode"`
	Metadata    map[string]string `json:"metadata"`
}

// fakeStripePrice is a price as GET /v1/prices lists it without expand: its product is an ID.
type fakeStripePrice struct {
	ID         string               `json:"id"`
	Product    string               `json:"product"`
	Currency   string               `json:"currency"`
	UnitAmount int64                `json:"unit_amount"`
	Recurring  *fakeStripeRecurring `json:"recurring"`
	LookupKey  string               `json:"lookup_key,omitempty"`
	Active     bool                 `json:"active"`
	Metadata   map[string]string    `json:"metadata"`
}

type fakeStripeRecurring struct {
	Interval      string `json:"interval"`
	IntervalCount int64  `json:"interval_count"`
}

// fakeStripeSearchQuery is the one query GET /v1/products/search answers: metadata['<key>']:'<value>'.
var fakeStripeSearchQuery = regexp.MustCompile(`^metadata\['([^']+)'\]:'([^']*)'$`)

// routeCatalog answers the product and price routes. A product's ID is given on create. A price
// created with a lookup key another price holds is refused unless transfer_lookup_key is true,
// which takes the key from that price.
func (s *fakeStripe) routeCatalog(w http.ResponseWriter, r *http.Request, form url.Values) {
	productID, onProduct := strings.CutPrefix(r.URL.Path, "/v1/products/")
	priceID, onPrice := strings.CutPrefix(r.URL.Path, "/v1/prices/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/products/search":
		s.searchProducts(w, r.URL.Query())
	case r.Method == http.MethodPost && r.URL.Path == "/v1/products":
		if s.productIndex(form.Get("id")) >= 0 {
			answerStripeError(w, fakeStripeError{http.StatusBadRequest, "invalid_request_error", "resource_already_exists", "Product already exists."})
			return
		}
		product := fakeStripeProduct{ID: form.Get("id"), Name: form.Get("name"), Description: form.Get("description"), Active: true, Livemode: s.livemode}
		product.Metadata = setFakeMetadata(nil, form)
		s.products = append(s.products, product)
		_ = json.NewEncoder(w).Encode(product)
	case onProduct:
		i := s.productIndex(productID)
		if i < 0 {
			answerStripeError(w, fakeStripeError{http.StatusNotFound, "invalid_request_error", "resource_missing", "No such product: '" + productID + "'"})
			return
		}
		product := &s.products[i]
		if r.Method == http.MethodPost {
			if form.Has("name") {
				product.Name = form.Get("name")
			}
			if form.Has("description") {
				product.Description = form.Get("description")
			}
			if form.Has("active") {
				product.Active = form.Get("active") == "true"
			}
			product.Metadata = setFakeMetadata(product.Metadata, form)
		}
		_ = json.NewEncoder(w).Encode(product)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/prices":
		s.listPrices(w, r.URL.Query())
	case r.Method == http.MethodPost && r.URL.Path == "/v1/prices":
		s.createPrice(w, form)
	case onPrice && r.Method == http.MethodPost:
		i := s.priceIndex(priceID)
		if i < 0 {
			answerStripeError(w, fakeStripeError{http.StatusNotFound, "invalid_request_error", "resource_missing", "No such price: '" + priceID + "'"})
			return
		}
		price := &s.prices[i]
		if form.Has("active") {
			price.Active = form.Get("active") == "true"
		}
		price.Metadata = setFakeMetadata(price.Metadata, form)
		_ = json.NewEncoder(w).Encode(price)
	default:
		answerStripeError(w, fakeStripeError{http.StatusNotFound, "invalid_request_error", "resource_missing", "Unrecognized request URL"})
	}
}

func (s *fakeStripe) createPrice(w http.ResponseWriter, form url.Values) {
	if s.productIndex(form.Get("product")) < 0 {
		answerStripeError(w, fakeStripeError{http.StatusBadRequest, "invalid_request_error", "resource_missing", "No such product: '" + form.Get("product") + "'"})
		return
	}
	key := form.Get("lookup_key")
	if holder := s.lookupKeyHolder(key); holder >= 0 {
		if form.Get("transfer_lookup_key") != "true" {
			answerStripeError(w, fakeStripeError{http.StatusBadRequest, "invalid_request_error", "",
				fmt.Sprintf("A price (`%s`) already uses that lookup key.", s.prices[holder].ID)})
			return
		}
		s.prices[holder].LookupKey = ""
	}
	amount, _ := strconv.ParseInt(form.Get("unit_amount"), 10, 64)
	s.pricesCreated++
	price := fakeStripePrice{
		ID: fmt.Sprintf("price_%d", s.pricesCreated), Product: form.Get("product"), Currency: form.Get("currency"),
		UnitAmount: amount, LookupKey: key, Active: true, Metadata: setFakeMetadata(nil, form),
	}
	if interval := form.Get("recurring[interval]"); interval != "" {
		price.Recurring = &fakeStripeRecurring{Interval: interval, IntervalCount: 1}
	}
	s.prices = append(s.prices, price)
	_ = json.NewEncoder(w).Encode(price)
}

// listPrices answers one page of GET /v1/prices filtered by lookup_keys[], product and active;
// expand[]=data.product puts each price's product in place of its ID.
func (s *fakeStripe) listPrices(w http.ResponseWriter, query url.Values) {
	var matched []fakeStripePrice
	for _, price := range s.prices {
		keys := query["lookup_keys[]"]
		switch {
		case len(keys) > 0 && !slices.Contains(keys, price.LookupKey):
		case query.Has("product") && price.Product != query.Get("product"):
		case query.Has("active") && strconv.FormatBool(price.Active) != query.Get("active"):
		default:
			matched = append(matched, price)
		}
	}
	rest := matched
	if after := query.Get("starting_after"); after != "" {
		i := slices.IndexFunc(rest, func(p fakeStripePrice) bool { return p.ID == after })
		rest = rest[i+1:]
	}
	page := rest[:min(fakeStripeLimit(query), len(rest))]
	data := make([]any, len(page))
	for i, price := range page {
		data[i] = price
		if slices.Contains(query["expand[]"], "data.product") {
			product := fakeStripeProduct{ID: price.Product}
			if j := s.productIndex(price.Product); j >= 0 {
				product = s.products[j]
			}
			data[i] = struct {
				fakeStripePrice
				Product fakeStripeProduct `json:"product"`
			}{price, product}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "has_more": len(page) < len(rest)})
}

// searchProducts answers one page of GET /v1/products/search; page is the offset of the next one.
func (s *fakeStripe) searchProducts(w http.ResponseWriter, query url.Values) {
	match := fakeStripeSearchQuery.FindStringSubmatch(query.Get("query"))
	if match == nil {
		answerStripeError(w, fakeStripeError{http.StatusBadRequest, "invalid_request_error", "", "the fake does not support this query"})
		return
	}
	var found []fakeStripeProduct
	for _, product := range s.products {
		if product.Metadata[match[1]] == match[2] {
			found = append(found, product)
		}
	}
	offset, _ := strconv.Atoi(query.Get("page"))
	rest := found[min(offset, len(found)):]
	page := rest[:min(fakeStripeLimit(query), len(rest))]
	var next any
	if len(page) < len(rest) {
		next = strconv.Itoa(offset + len(page))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "search_result", "data": append([]fakeStripeProduct{}, page...), "has_more": next != nil, "next_page": next,
	})
}

func fakeStripeLimit(query url.Values) int {
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit <= 0 || limit > 100 {
		return 10
	}
	return limit
}

// setFakeMetadata applies a form's metadata[...] fields as Stripe does: an empty value removes the
// key.
func setFakeMetadata(metadata map[string]string, form url.Values) map[string]string {
	if metadata == nil {
		metadata = map[string]string{}
	}
	for field, values := range form {
		if name, ok := strings.CutPrefix(field, "metadata["); ok {
			name = strings.TrimSuffix(name, "]")
			if values[0] == "" {
				delete(metadata, name)
			} else {
				metadata[name] = values[0]
			}
		}
	}
	return metadata
}

func (s *fakeStripe) productIndex(id string) int {
	return slices.IndexFunc(s.products, func(p fakeStripeProduct) bool { return p.ID == id })
}

func (s *fakeStripe) priceIndex(id string) int {
	return slices.IndexFunc(s.prices, func(p fakeStripePrice) bool { return p.ID == id })
}

func (s *fakeStripe) lookupKeyHolder(key string) int {
	if key == "" {
		return -1
	}
	return slices.IndexFunc(s.prices, func(p fakeStripePrice) bool { return p.LookupKey == key })
}

// product is a copy of one product; the test fails without it.
func (s *fakeStripe) product(t *testing.T, id string) fakeStripeProduct {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.productIndex(id)
	if i < 0 {
		t.Fatalf("Stripe has no product %s", id)
	}
	product := s.products[i]
	product.Metadata = maps.Clone(product.Metadata)
	return product
}

// price is a copy of one price; the test fails without it.
func (s *fakeStripe) price(t *testing.T, id string) fakeStripePrice {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.priceIndex(id)
	if i < 0 {
		t.Fatalf("Stripe has no price %s", id)
	}
	price := s.prices[i]
	price.Metadata = maps.Clone(price.Metadata)
	return price
}

// priceCount is how many prices the account has, archived ones included.
func (s *fakeStripe) priceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prices)
}

// editProduct changes a product as a person in the Stripe Dashboard would.
func (s *fakeStripe) editProduct(id string, edit func(*fakeStripeProduct)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.productIndex(id); i >= 0 {
		edit(&s.products[i])
	}
}

// addPrice adds a price as if created with transfer_lookup_key=true, and returns its ID.
func (s *fakeStripe) addPrice(price fakeStripePrice) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if holder := s.lookupKeyHolder(price.LookupKey); holder >= 0 {
		s.prices[holder].LookupKey = ""
	}
	s.pricesCreated++
	price.ID = fmt.Sprintf("price_%d", s.pricesCreated)
	s.prices = append(s.prices, price)
	return price.ID
}
