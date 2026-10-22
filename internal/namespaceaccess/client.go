/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// Package namespaceaccess bootstraps the manager's workload RoleBinding only in opted-in
// namespaces. API admission independently enforces this boundary for a stolen credential.
package namespaceaccess

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const RoleName = "orama-operator-workloads"
const ManagedLabel = "app.kubernetes.io/managed-by"
const ManagedValue = "orama-operator"

type scopedClient struct {
	client.Client
	reader             client.Reader
	namespace, account string
}

// Wrap uses the uncached reader for namespace and binding checks, so revocation and deletion
// repair take effect on the next write. No successful authorization is cached indefinitely.
func Wrap(c client.Client, reader client.Reader, namespace, account string) client.Client {
	return &scopedClient{Client: c, reader: reader, namespace: namespace, account: account}
}

// EnsureNamespace verifies opt-in and repairs the fixed workload grant before reconciliation.
func EnsureNamespace(ctx context.Context, c client.Client, namespace string) error {
	if scoped, ok := c.(interface {
		EnsureNamespace(context.Context, string) error
	}); ok {
		return scoped.EnsureNamespace(ctx, namespace)
	}
	return nil
}

func (c *scopedClient) prepare(ctx context.Context, obj client.Object) error {
	return c.EnsureNamespace(ctx, obj.GetNamespace())
}

func (c *scopedClient) EnsureNamespace(ctx context.Context, name string) error {
	if name == "" || name == "kube-system" {
		return nil
	}
	var namespace corev1.Namespace
	if err := c.reader.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
		return err
	}
	if namespace.Labels[ManagedLabel] != ManagedValue && namespace.Name != c.namespace {
		return fmt.Errorf("namespace %q requires %s=%s in its committed namespace manifest before the operator can manage workloads", namespace.Name, ManagedLabel, ManagedValue)
	}
	var binding rbacv1.RoleBinding
	key := client.ObjectKey{Namespace: namespace.Name, Name: RoleName}
	err := c.reader.Get(ctx, key, &binding)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil &&
		binding.RoleRef == (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: RoleName}) &&
		len(binding.Subjects) == 1 && binding.Subjects[0] == (rbacv1.Subject{Kind: "ServiceAccount", Name: c.account, Namespace: c.namespace}) {
		return nil
	}
	binding = rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: RoleName, Namespace: namespace.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: RoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: c.account, Namespace: c.namespace}},
	}
	return c.Client.Patch(ctx, &binding, client.Apply, client.FieldOwner(RoleName), client.ForceOwnership)
}

func (c *scopedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if ns, ok := obj.(*corev1.Namespace); ok && ns.ResourceVersion == "" && !strings.HasPrefix(ns.Name, "kube-") {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels[ManagedLabel] = ManagedValue
	}
	if err := c.prepare(ctx, obj); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}
func (c *scopedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.prepare(ctx, obj); err != nil {
		return err
	}
	return c.Client.Update(ctx, obj, opts...)
}
func (c *scopedClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.prepare(ctx, obj); err != nil {
		return err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}
func (c *scopedClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.prepare(ctx, obj); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}
func (c *scopedClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	options := &client.DeleteAllOfOptions{}
	options.ApplyOptions(opts)
	if options.Namespace == "" {
		return fmt.Errorf("namespace is required for collection deletion")
	}
	target := obj.DeepCopyObject().(client.Object)
	target.SetNamespace(options.Namespace)
	if err := c.prepare(ctx, target); err != nil {
		return err
	}
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}
