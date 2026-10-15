/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A wrapper kind's Service (SQLite, DuckDB, Polars) sends 8080 to the named port "http". A tunnel
// to it failed on Lake/prod (operator#508); the name is resolved from the pods the Service selects.
func TestNamedTargetPortIsResolvedFromTheServicesPods(t *testing.T) {
	ctx := context.Background()
	selector := map[string]string{"app.kubernetes.io/name": "lake-sqlite", "app.kubernetes.io/instance": "prod-svc-6642e20610ec"}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ws", Name: "prod-svc-6642e20610ec"}, Spec: corev1.ServiceSpec{Selector: selector}}
	pod := func(portName string, port int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ws", Name: "prod-svc-6642e20610ec-0", Labels: selector},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sqlite", Ports: []corev1.ContainerPort{
				{Name: portName, ContainerPort: port, Protocol: corev1.ProtocolTCP},
			}}}},
		}
	}
	scheme := newLakeScheme(t)

	resolved, err := namedTargetPort(ctx, fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod("http", 8080)).Build(), "ws", service, "http")
	if err != nil || resolved != 8080 {
		t.Fatalf("named port http = %d, %v; want 8080", resolved, err)
	}

	_, err = namedTargetPort(ctx, fake.NewClientBuilder().WithScheme(scheme).Build(), "ws", service, "http")
	if _, waiting := err.(connectionPendingError); !waiting {
		t.Fatalf("with no pod yet the target should wait, got %v", err)
	}

	_, err = namedTargetPort(ctx, fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod("metrics", 9090)).Build(), "ws", service, "http")
	if err == nil {
		t.Fatal("a pod with no container port named http resolved")
	}
	if _, waiting := err.(connectionPendingError); waiting {
		t.Fatal("a pod without the named port should fail, not wait")
	}

	if _, err := namedTargetPort(ctx, nil, "ws", service, "http"); err == nil {
		t.Fatal("without an uncached pod reader the name must not resolve")
	}
}
