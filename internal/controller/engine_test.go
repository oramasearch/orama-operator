/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestReaderProbesWaitForCatchUp: a reader is Ready only through /health/ready (it has caught up with
// the stream), while liveness and startup stay on /health so a reader that refuses on expired history
// is never Ready but is not killed. Its termination message falls back to the log so the refusal code
// reaches the pod status. The writer is unchanged.
func TestReaderProbesWaitForCatchUp(t *testing.T) {
	ctx := context.Background()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	oc := newOCV1(nil)
	if err := r.reconcileReaders(ctx, oc, "", "", envoyConfig); err != nil {
		t.Fatalf("reconcileReaders: %v", err)
	}
	if err := r.reconcileWriter(ctx, oc, "", ""); err != nil {
		t.Fatalf("reconcileWriter: %v", err)
	}

	var reader appsv1.StatefulSet
	if err := cl.Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: "v1-reader"}, &reader); err != nil {
		t.Fatalf("get reader: %v", err)
	}
	c := oramacoreOf(t, reader.Spec.Template.Spec)
	if got := c.ReadinessProbe.HTTPGet.Path; got != "/health/ready" {
		t.Errorf("reader readiness path = %q, want /health/ready", got)
	}
	if got := c.LivenessProbe.HTTPGet.Path; got != "/health" {
		t.Errorf("reader liveness path = %q, want /health", got)
	}
	if got := c.StartupProbe.HTTPGet.Path; got != "/health" {
		t.Errorf("reader startup path = %q, want /health", got)
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("reader TerminationMessagePolicy = %q, want %q", c.TerminationMessagePolicy, corev1.TerminationMessageFallbackToLogsOnError)
	}

	var writer appsv1.StatefulSet
	if err := cl.Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: "v1-writer"}, &writer); err != nil {
		t.Fatalf("get writer: %v", err)
	}
	w := oramacoreOf(t, writer.Spec.Template.Spec)
	for name, p := range map[string]*corev1.Probe{"readiness": w.ReadinessProbe, "liveness": w.LivenessProbe, "startup": w.StartupProbe} {
		if got := p.HTTPGet.Path; got != "/health" {
			t.Errorf("writer %s path = %q, want /health", name, got)
		}
	}
	if w.TerminationMessagePolicy != "" {
		t.Errorf("writer TerminationMessagePolicy = %q, want unset", w.TerminationMessagePolicy)
	}
}

func oramacoreOf(t *testing.T, spec corev1.PodSpec) corev1.Container {
	t.Helper()
	for _, c := range spec.Containers {
		if c.Name == "oramacore" {
			return c
		}
	}
	t.Fatal("no oramacore container")
	return corev1.Container{}
}
