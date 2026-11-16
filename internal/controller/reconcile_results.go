// SPDX-License-Identifier: MIT

package controller

import (
	ctrl "sigs.k8s.io/controller-runtime"
)

// settle turns a reconcile's (result, error) into what the manager should see: the result when
// there is no error, and the error alone when there is one. Returning a result together with an
// error makes the manager warn and ignore the result.
//
// It hides nothing. An error is returned, logged and retried. The log is kept readable by fixing
// what causes errors, never by not reporting them.
func settle(result ctrl.Result, err error) (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}
