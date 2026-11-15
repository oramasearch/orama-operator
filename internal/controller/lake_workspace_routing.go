/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Workspace routing is checked by the operator itself, not only by lake (operator#781).
//
// Every row intake that places objects in a workspace namespace takes that namespace from lake's
// workspaces table. Lake keeps a row's workspace_id within its customer with triggers, and the
// operator writes workspaces.namespace itself, so nothing is exploitable through lake today; the
// operator still refuses on its own, in two places:
//
//   - every listing joins workspaces with w.customer_id = <row>.customer_id (and a methodology row's
//     build with b.customer_id = <row>.customer_id), so a row naming another customer's workspace
//     reaches no namespace at all;
//   - before writing into the namespace, workspaceNamespaceRefusal checks the namespace on the
//     cluster: it must not be the Lake namespace, and when it exists it must carry the labels the
//     workspaces reconciler put on it for this workspace, this customer and this Lake.
//
// A namespace that does not exist is not refused here: no intake creates a workspace namespace, so
// a write into it fails at the API server, and the row waits as it did before.

// workspaceNamespaceRefusal returns why the operator will not write a row's objects into namespace,
// or "" when it may.
func workspaceNamespaceRefusal(ctx context.Context, reader client.Reader, lake *oramav1alpha1.Lake, namespace, workspaceID, customerID string) (string, error) {
	var existing corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("read workspace namespace %q: %w", namespace, err)
	}
	if namespace == lakeNamespace(lake) {
		return fmt.Sprintf("namespace %q is the Lake's own namespace, not a workspace's", namespace), nil
	}
	labels := existing.Labels
	if labels[workspaceLabel] != workspaceID || labels[workspaceCustomerLabel] != customerID || labels[workspaceLakeLabel] != lake.Name {
		return fmt.Sprintf("namespace %q is not workspace %s of customer %s in Lake %s", namespace, workspaceID, customerID, lake.Name), nil
	}
	return "", nil
}
