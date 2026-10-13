/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// deleteCloudV1Owned removes only this CR's resource; foreign objects are never adopted on disable.
func (r *OramaCloudV1Reconciler) deleteCloudV1Owned(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, oc) {
		return fmt.Errorf("refusing to delete unowned %T %s", obj, obj.GetName())
	}
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	return client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}))
}
