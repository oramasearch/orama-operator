// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// namespaceCacheGuard is the manager's cached client as the operator's RBAC makes it on a real
// cluster: it may get Namespaces only uncached (operator#106), so a Namespace read through the
// cache would start an informer that can never sync. dev-0e9f4a4 crash-looped on exactly that.
type namespaceCacheGuard struct {
	client.Client
}

func (g namespaceCacheGuard) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Namespace); ok {
		return fmt.Errorf("Namespace %q read through the cache; read it through the uncached APIReader", key.Name)
	}
	return g.Client.Get(ctx, key, obj, opts...)
}

func (g namespaceCacheGuard) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.NamespaceList); ok {
		return fmt.Errorf("Namespaces listed through the cache; the operator may not list them")
	}
	return g.Client.List(ctx, list, opts...)
}

func TestEnsureOperatorNamespaceReadsUncached(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).Build()
	cached := namespaceCacheGuard{kube}
	labels := tunnelNamespaceLabels()
	for i := 0; i < 2; i++ { // creates, then confirms the existing namespace
		if _, err := ensureOperatorNamespace(ctx, kube, cached, zettaTunnelNamespace, labels); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if _, err := ensureOperatorNamespace(ctx, cached, cached, zettaTunnelNamespace, labels); err == nil {
		t.Fatal("the guard did not refuse a cached Namespace read; this test would not catch the regression")
	}
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "foreign"}}
	if err := kube.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureOperatorNamespace(ctx, kube, cached, "foreign", labels); err == nil {
		t.Fatal("adopted a namespace the operator did not create")
	}
}
