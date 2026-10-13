/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"errors"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Two outcomes of a reconcile are not errors, and logging them as errors (with a stack trace, on
// every retry) buries what is new in the operator's log.
//
//   - A write conflict: the object changed between the cached read and the write. It is ordinary
//     optimistic concurrency, and the fix is to reconcile again from a fresh read.
//   - A provider refusing a request because of configuration: a feature not enabled on the
//     account, a credential without the permission. Retrying fast cannot fix it. The reason belongs
//     on the resource's status, and the operator checks back occasionally.

// refusalRecheckInterval is how often a resource a provider refused is tried again.
const refusalRecheckInterval = 30 * time.Minute

// settle turns a reconcile's (result, error) into what the manager should see. A conflict becomes
// a quiet requeue. Any other error is returned alone: returning a result together with an error
// makes the manager log a warning and ignore the result.
func settle(result ctrl.Result, err error) (ctrl.Result, error) {
	if err == nil {
		return result, nil
	}
	if apierrors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, err
}

// providerRefused reports whether a provider's API refused a request for a reason that retrying
// will not change: a 4xx other than a timeout or a rate limit. A 5xx or a network error is the
// provider's problem and is retried with the manager's backoff.
func providerRefused(err error) bool {
	status := 0
	var cloudflare *cfAPIError
	var azure *azureError
	switch {
	case errors.As(err, &cloudflare):
		status = cloudflare.StatusCode
	case errors.As(err, &azure):
		status = azure.Status
	default:
		return false
	}
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return false
	}
	return status >= 400 && status < 500
}
