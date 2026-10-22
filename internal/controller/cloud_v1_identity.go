/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// cloudV1PodIdentity makes token projection and service discovery independent of namespace defaults.
// Each workload uses its own account; these workloads need no Kubernetes API permissions.
func cloudV1PodIdentity(pod *corev1.PodSpec, workload string) {
	pod.ServiceAccountName = workload
	pod.DeprecatedServiceAccount = ""
	pod.AutomountServiceAccountToken = boolPtr(false)
	pod.EnableServiceLinks = boolPtr(false)
}

// ensureCloudV1ServiceAccount creates a tokenless account owned by the workload's CR, without any
// RBAC grants. A foreign account is a conflict, so a workload cannot inherit its permissions.
func ensureCloudV1ServiceAccount(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, workload string, labels map[string]string) error {
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: workload, Namespace: owner.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, account, func() error {
		if account.ResourceVersion != "" && !metav1.IsControlledBy(account, owner) {
			return fmt.Errorf("ServiceAccount %s/%s is not controlled by %s", account.Namespace, account.Name, owner.GetName())
		}
		account.Labels = labels
		account.AutomountServiceAccountToken = boolPtr(false)
		return controllerutil.SetControllerReference(owner, account, scheme)
	})
	return err
}
