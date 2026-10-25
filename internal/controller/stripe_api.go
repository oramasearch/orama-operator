/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	stripeAPIBase = "https://api.stripe.com"
	// stripeAPIVersion is the Stripe-Version of the operator's own calls: account, products and
	// prices. It matches Lake's pin, keeping requests predictable and independent of the account
	// default; the operator reads stable fields whose shape is unchanged across the apps' versions.
	// Change it only with a test against the fake for every field the operator reads.
	// A webhook endpoint's calls carry that endpoint's apiVersion instead.
	stripeAPIVersion = "2024-06-20"
	// stripeTimeout bounds one attempt of a request.
	stripeTimeout = 15 * time.Second
	// stripeAttempts is how often a request is sent while no answer arrives.
	stripeAttempts = 3
)

// stripeAPI is one key against Stripe's form-encoded REST API.
type stripeAPI struct {
	// Base is the API's address; empty is https://api.stripe.com. Tests point it at a fake.
	Base string
	Key  string
}

// stripeCall is one request. A POST sends Form as its body; Query is added to any method's URL.
// Build both as url.Values with stripeField keys: Encode sorts by key, so one request always has
// one body, and so one idempotency key.
type stripeCall struct {
	Method string
	// Path is the resource, such as /v1/webhook_endpoints/we_123, with IDs path-escaped.
	Path  string
	Query url.Values
	Form  url.Values
	// Version is the Stripe-Version header; empty is stripeAPIVersion.
	Version string
	// IdempotencyKey (stripeIdempotencyKey) is sent with a POST only; Stripe ignores it on GET and
	// DELETE.
	IdempotencyKey string
}

// stripeList is one page of a Stripe list, such as GET /v1/webhook_endpoints.
type stripeList[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// stripeSearchPage is one page of a Stripe search, such as GET /v1/products/search. NextPage is the
// next request's page parameter.
type stripeSearchPage[T any] struct {
	Data     []T    `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

// stripeField names a nested form key in Stripe's encoding: stripeField("metadata", "orama_app")
// is metadata[orama_app], stripeField("price_data", "product") is price_data[product], and an
// empty name makes a list, stripeField("enabled_events", "") is enabled_events[].
func stripeField(name string, nested ...string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, n := range nested {
		b.WriteString("[" + n + "]")
	}
	return b.String()
}

// stripeIdempotencyKey names one request exactly, because Stripe replays the saved answer to a key
// it has seen in the last 24 hours (a saved 500 too) and refuses a key reused with other parameters:
// orama-<object UID>-<operation>-<target>-<first 16 hex of sha256(form body)>.
//
// target is "new" for a create and the Stripe object's ID for an update; a recreate names the ID
// it replaces ("recreate-we_123"), so it never replays the create of the object it deleted.
// attempt is how many writes Stripe has answered for the object, 2xx or not (suffix -<attempt>;
// 0 adds none). A request Stripe answered is never sent again under its key: after a 5xx it would
// get the saved 5xx back, and the same request needed again later would get the saved success
// and change nothing. A request that got no answer keeps its key.
func stripeIdempotencyKey(uid types.UID, operation, target string, form url.Values, attempt int) string {
	sum := sha256.Sum256([]byte(form.Encode()))
	key := fmt.Sprintf("orama-%s-%s-%s-%x", uid, operation, target, sum[:8])
	if attempt > 0 {
		key += fmt.Sprintf("-%d", attempt)
	}
	return key
}

// stripeAPIError is a non-2xx answer, without the key. Stripe's error message can echo the key it
// was sent, so Message is empty on 401 and 403 and has the key replaced by <redacted> otherwise
// (lake does the same, src/api/stripe.cpp). Conditions and logs use only this.
type stripeAPIError struct {
	Method     string
	Path       string
	StatusCode int
	// Type and Code are Stripe's error.type (invalid_request_error, ...) and error.code
	// (resource_missing, ...); Param names a rejected or missing request field.
	Type    string
	Code    string
	Param   string
	Message string
}

func (e *stripeAPIError) Error() string {
	text := fmt.Sprintf("stripe %s %s: HTTP %d", e.Method, e.Path, e.StatusCode)
	for _, part := range []string{e.Type, e.Code} {
		if part != "" {
			text += " " + part
		}
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

// stripeSecretPattern is anything shaped like a Stripe secret, restricted or signing secret, so a
// message never carries one, even one other than the key that was sent.
var stripeSecretPattern = regexp.MustCompile(`(sk|rk)_(live|test)_[A-Za-z0-9]+|whsec_[A-Za-z0-9]+`)

func stripeRedact(text, key string) string {
	if key != "" {
		text = strings.ReplaceAll(text, key, "<redacted>")
	}
	return stripeSecretPattern.ReplaceAllString(text, "<redacted>")
}

// stripeDo sends call and decodes a 2xx answer into T.
//
// A request that gets no answer (a timeout, a dropped connection) is sent again unchanged, with the
// same idempotency key, so Stripe answers the repeat of a request it already carried out with the
// saved result instead of carrying it out twice. GET and DELETE are repeated the same way; a POST
// without a key is not. Any answer, a 5xx too, is returned as it is: Stripe saved it against the
// key, so the caller builds the next attempt with stripeIdempotencyKey's attempt.
func stripeDo[T any](ctx context.Context, api stripeAPI, call stripeCall) (T, error) {
	var out T
	repeatable := call.Method != http.MethodPost || call.IdempotencyKey != ""
	var err error
	for attempt := 1; attempt <= stripeAttempts; attempt++ {
		var answered bool
		answered, err = stripeSend(ctx, api, call, &out)
		if answered || !repeatable || attempt == stripeAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
		}
	}
	return out, err
}

// stripeSend makes one attempt; answered is false when no complete answer arrived.
func stripeSend(ctx context.Context, api stripeAPI, call stripeCall, out any) (bool, error) {
	base := stripeAPIBase
	if api.Base != "" {
		base = strings.TrimSuffix(api.Base, "/")
	}
	target := base + call.Path
	if len(call.Query) > 0 {
		target += "?" + call.Query.Encode()
	}
	var body io.Reader
	if call.Method == http.MethodPost {
		body = strings.NewReader(call.Form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, call.Method, target, body)
	if err != nil {
		return true, err
	}
	req.Header.Set("Authorization", "Bearer "+api.Key)
	version := call.Version
	if version == "" {
		version = stripeAPIVersion
	}
	req.Header.Set("Stripe-Version", version)
	if call.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if call.IdempotencyKey != "" {
			req.Header.Set("Idempotency-Key", call.IdempotencyKey)
		}
	}

	resp, err := (&http.Client{Timeout: stripeTimeout}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &stripeAPIError{Method: call.Method, Path: call.Path, StatusCode: resp.StatusCode}
		var answer struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Param   string `json:"param"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &answer) == nil {
			apiErr.Type = stripeRedact(answer.Error.Type, api.Key)
			apiErr.Code = stripeRedact(answer.Error.Code, api.Key)
			apiErr.Param = stripeRedact(answer.Error.Param, api.Key)
			if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
				apiErr.Message = stripeRedact(answer.Error.Message, api.Key)
			}
		}
		return true, apiErr
	}
	if err := json.Unmarshal(data, out); err != nil {
		return true, fmt.Errorf("stripe %s %s: decoding the answer: %w", call.Method, call.Path, err)
	}
	return true, nil
}

// stripeForbidden is whether Stripe refused the key what it asked (403, or a permission_error):
// a restricted key without that permission.
func stripeForbidden(err error) bool {
	var apiErr *stripeAPIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.Type == "permission_error")
}
