/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for spec.images.requireDigests (operator#317).

const testDigest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// pinnedOverrides pins every component fullStackOCV1 enables by digest.
func pinnedOverrides() map[string]string {
	o := map[string]string{}
	for _, key := range append([]string{"oramacore-writer", "oramacore-reader", "dashboard", "docs",
		"index-builder", "harvest-master", "harvest", "disco", "workers", "analytics-processor"}, searchWorkerNames...) {
		o[key] = "oramasearch/" + key + testDigest
	}
	return o
}

func TestRequireDigestsRefusesAFloatingImage(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.RequireDigests = true
		o.Spec.Images.Overrides = pinnedOverrides()
		delete(o.Spec.Images.Overrides, "disco") // resolves to oramasearch/disco:latest
	})
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc).WithStatusSubresource(oc).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)})
	if err == nil || !strings.Contains(err.Error(), "disco (oramasearch/disco:latest)") {
		t.Fatalf("Reconcile error = %v, want one naming disco", err)
	}
	if strings.Contains(err.Error(), "dashboard") {
		t.Errorf("error names a pinned component: %v", err)
	}
	got := getObj(t, cl, oc.Name, &oramav1alpha1.OramaCloudV1{})
	ready := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Status != "False" || !strings.Contains(ready.Message, "disco") {
		t.Errorf("Ready = %+v, want False naming disco", ready)
	}
	if !absent(t, cl, "v1-dashboard", &appsv1.Deployment{}) {
		t.Error("nothing may be rolled out while an image is floating")
	}
}

func TestRequireDigestsPassesWhenEveryComponentIsPinned(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.RequireDigests = true
		o.Spec.Images.Overrides = pinnedOverrides()
	}))
	if img := getObj(t, cl, "v1-disco", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].Image; img != "oramasearch/disco"+testDigest {
		t.Errorf("disco image %q, want the digest override", img)
	}
}

func TestRequireDigestsUnsetAllowsLatest(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))
	if img := getObj(t, cl, "v1-disco", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].Image; img != "oramasearch/disco:latest" {
		t.Errorf("disco image %q, want oramasearch/disco:latest as before", img)
	}
}

// A disabled component's image is not checked, and a writer-less (read) region checks no writer.
func TestRequireDigestsChecksOnlyEnabledComponents(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Region.Role = "read"
		o.Spec.Images.RequireDigests = true
		o.Spec.Components.Engine.Enabled = true
		o.Spec.Images.Overrides = map[string]string{"oramacore": "oramasearch/oramacore" + testDigest}
	})
	if err := validateImageDigests(oc); err != nil {
		t.Errorf("validateImageDigests = %v, want nil", err)
	}
	oc.Spec.Images.Overrides = map[string]string{"oramacore-writer": "oramasearch/w:1", "oramacore-reader": "oramasearch/r" + testDigest}
	if err := validateImageDigests(oc); err != nil {
		t.Errorf("read region checked the writer image: %v", err)
	}
}
