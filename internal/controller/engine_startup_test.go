/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestEngineStartupWorkloads(t *testing.T) {
	for _, envoy := range []bool{false, true} {
		t.Run(fmt.Sprintf("envoy=%t", envoy), func(t *testing.T) {
			ctx := context.Background()
			s := ocv1TestScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).Build()
			r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
			oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine.Envoy = envoy
			})
			writer := &appsv1.StatefulSet{}
			reader := &appsv1.Deployment{}
			for _, stage := range []string{"create", "upgrade"} {
				t.Run(stage, func(t *testing.T) {
					if err := r.reconcileWriter(ctx, oc); err != nil {
						t.Fatalf("reconcileWriter: %v", err)
					}
					if err := r.reconcileReaders(ctx, oc); err != nil {
						t.Fatalf("reconcileReaders: %v", err)
					}
					if err := cl.Get(ctx, types.NamespacedName{Name: "v1-writer", Namespace: "orama-system"}, writer); err != nil {
						t.Fatalf("get writer StatefulSet: %v", err)
					}
					if err := cl.Get(ctx, types.NamespacedName{Name: "v1-reader", Namespace: "orama-system"}, reader); err != nil {
						t.Fatalf("get reader Deployment: %v", err)
					}
					assertEngineHealthProbes(t, engineContainerForStartupTest(t, &writer.Spec.Template.Spec), 120, 20*time.Minute)
					assertEngineHealthProbes(t, engineContainerForStartupTest(t, &reader.Spec.Template.Spec), 240, 40*time.Minute)
				})
				if t.Failed() {
					t.FailNow()
				}
				if stage == "create" {
					// Seed the previous five-minute templates; the next reconcile must repair both.
					engineContainerForStartupTest(t, &writer.Spec.Template.Spec).StartupProbe.FailureThreshold = 30
					engineContainerForStartupTest(t, &reader.Spec.Template.Spec).StartupProbe.FailureThreshold = 30
					if err := cl.Update(ctx, writer); err != nil {
						t.Fatalf("seed old writer probe: %v", err)
					}
					if err := cl.Update(ctx, reader); err != nil {
						t.Fatalf("seed old reader probe: %v", err)
					}
				}
			}
		})
	}
}

func TestEngineStartupProbeRounding(t *testing.T) {
	for _, tc := range []struct {
		timeout   time.Duration
		threshold int32
	}{
		{time.Second, 1},
		{10 * time.Second, 1},
		{95 * time.Second, 10},
		{20 * time.Minute, 120},
		{40 * time.Minute, 240},
	} {
		t.Run(tc.timeout.String(), func(t *testing.T) {
			if got := engineStartupProbe(tc.timeout).FailureThreshold; got != tc.threshold {
				t.Errorf("threshold = %d, want %d", got, tc.threshold)
			}
		})
	}
}

func TestEngineStartupProbesAreIndependent(t *testing.T) {
	oc := newOCV1(nil)
	writer := oramacoreContainer(oc, writerStartupTimeout)
	reader := oramacoreContainer(oc, readerStartupTimeout)
	writer.StartupProbe.FailureThreshold = 1
	writer.StartupProbe.HTTPGet.Path = "/changed"
	assertEngineHealthProbes(t, &reader, 240, 40*time.Minute)
	nextWriter := oramacoreContainer(oc, writerStartupTimeout)
	assertEngineHealthProbes(t, &nextWriter, 120, 20*time.Minute)
}

func engineContainerForStartupTest(t *testing.T, pod *corev1.PodSpec) *corev1.Container {
	t.Helper()
	for i := range pod.Containers {
		if pod.Containers[i].Name == "oramacore" {
			return &pod.Containers[i]
		}
	}
	t.Fatal("oramacore container missing")
	return nil
}

func assertEngineHealthProbes(t *testing.T, c *corev1.Container, threshold int32, window time.Duration) {
	t.Helper()
	for _, tc := range []struct {
		name                       string
		probe                      *corev1.Probe
		period, timeout, threshold int32
	}{
		{"startup", c.StartupProbe, 10, 0, threshold},
		{"readiness", c.ReadinessProbe, 10, 3, 3},
		{"liveness", c.LivenessProbe, 15, 3, 3},
	} {
		want := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/health", Port: intstr.FromInt32(oramacorePort),
			}},
			PeriodSeconds: tc.period, TimeoutSeconds: tc.timeout, FailureThreshold: tc.threshold,
		}
		if !reflect.DeepEqual(tc.probe, want) {
			t.Errorf("%s probe = %+v, want %+v", tc.name, tc.probe, want)
		}
	}
	if c.StartupProbe != nil {
		got := time.Duration(c.StartupProbe.PeriodSeconds) * time.Duration(c.StartupProbe.FailureThreshold) * time.Second
		if got != window {
			t.Errorf("startup window = %s, want %s", got, window)
		}
	}
}
