/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

// A write conflict is ordinary optimistic concurrency: reconcile again, and log nothing. Found on
// the management cluster on 2026-10-13, where the first status write of a new
// KubernetesClusterProvider raced its own finalizer update and was logged as an error.
func TestSettleTurnsAConflictIntoAQuietRequeueAndNeverReturnsAResultWithAnError(t *testing.T) {
	wanted := ctrl.Result{RequeueAfter: 30 * time.Second}
	if got, err := settle(wanted, nil); err != nil || got != wanted {
		t.Fatalf("no error: want the result unchanged, got %+v, %v", got, err)
	}

	conflict := apierrors.NewConflict(schema.GroupResource{Group: "orama.cloud", Resource: "kubernetesclusterproviders"},
		"azure-orama", errors.New("the object has been modified"))
	got, err := settle(wanted, conflict)
	if err != nil || !got.Requeue || got.RequeueAfter != 0 {
		t.Fatalf("a conflict: want a plain requeue and no error, got %+v, %v", got, err)
	}
	// Wrapped, as a helper might return it.
	if got, err := settle(wanted, fmt.Errorf("write status: %w", conflict)); err != nil || !got.Requeue {
		t.Fatalf("a wrapped conflict: got %+v, %v", got, err)
	}

	other := errors.New("connection refused")
	got, err = settle(wanted, other)
	if !errors.Is(err, other) || got != (ctrl.Result{}) {
		t.Fatalf("another error: want it returned with an empty result, got %+v, %v", got, err)
	}
}

// A provider refusing because of configuration is not retried fast. The R2 bucket of the v1
// sandbox was refused with 403 "Please enable R2" on every retry from July to October 2026.
func TestProviderRefusedIsAFourHundredThatRetryingWillNotChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"cloudflare 403, R2 not enabled", &cfAPIError{Method: "POST", StatusCode: 403, Body: `{"errors":[{"code":10042}]}`}, true},
		{"cloudflare 400", &cfAPIError{StatusCode: 400}, true},
		{"cloudflare 429 rate limit", &cfAPIError{StatusCode: 429}, false},
		{"cloudflare 408 timeout", &cfAPIError{StatusCode: 408}, false},
		{"cloudflare 500", &cfAPIError{StatusCode: 500}, false},
		{"azure 403", &azureError{Status: 403, Code: "AuthorizationFailed"}, true},
		{"azure 409 conflict", &azureError{Status: 409}, true},
		{"azure 503", &azureError{Status: 503}, false},
		{"wrapped cloudflare 403", fmt.Errorf("ensure bucket: %w", &cfAPIError{StatusCode: 403}), true},
		{"a network error", errors.New("dial tcp: i/o timeout"), false},
		{"nil", nil, false},
	} {
		if got := providerRefused(tc.err); got != tc.want {
			t.Errorf("%s: providerRefused = %t, want %t", tc.name, got, tc.want)
		}
	}
}
