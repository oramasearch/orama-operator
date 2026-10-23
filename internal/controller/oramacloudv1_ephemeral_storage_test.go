/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestV1StorageOverridesCannotRemoveBounds(t *testing.T) {
	for _, tc := range []struct{ name, request, limit, wantRequest, wantLimit string }{
		{"missing", "", "", "100Mi", "1Gi"},
		{"zero", "0", "0", "100Mi", "1Gi"},
		{"negative", "-1Gi", "-1Gi", "100Mi", "1Gi"},
		{"small-limit", "", "1Mi", "1Mi", "1Mi"},
		{"large-request", "2Gi", "", "2Gi", "2Gi"},
		{"explicit", "256Mi", "4Gi", "256Mi", "4Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}
			if tc.request != "" {
				in.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse(tc.request)
			}
			if tc.limit != "" {
				in.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse(tc.limit)
			}
			before := in.DeepCopy()
			out := v1StorageResources(in, "app")
			request, limit := out.Requests[corev1.ResourceEphemeralStorage], out.Limits[corev1.ResourceEphemeralStorage]
			if request.Cmp(resource.MustParse(tc.wantRequest)) != 0 || limit.Cmp(resource.MustParse(tc.wantLimit)) != 0 {
				t.Fatalf("request=%s limit=%s", request.String(), limit.String())
			}
			if !reflect.DeepEqual(*before, in) {
				t.Fatal("mutated resource override")
			}
			if out.Requests.Cpu().Cmp(in.Requests.Cpu().DeepCopy()) != 0 || out.Limits.Memory().Cmp(in.Limits.Memory().DeepCopy()) != 0 {
				t.Fatal("changed CPU or memory")
			}
		})
	}
	for _, name := range []string{"embeddings", hpesComponent} {
		got := v1StorageResources(corev1.ResourceRequirements{}, name).Limits[corev1.ResourceEphemeralStorage]
		if got.Cmp(resource.MustParse("16Gi")) != 0 {
			t.Fatalf("%s model cache limit=%s", name, got.String())
		}
	}
}

func TestV1StorageDefaultsOwnershipAndRecovery(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	scheme := ocv1TestScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
	if err := r.reconcileV1StorageDefaults(ctx, oc); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-ephemeral-storage"}
	var limits corev1.LimitRange
	if err := kube.Get(ctx, key, &limits); err != nil {
		t.Fatal(err)
	}
	version := limits.ResourceVersion
	if err := r.reconcileV1StorageDefaults(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, key, &limits); err != nil {
		t.Fatal(err)
	}
	if limits.ResourceVersion != version {
		t.Fatal("steady reconcile rewrote LimitRange")
	}
	limits.Spec.Limits = nil
	if err := kube.Update(ctx, &limits); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileV1StorageDefaults(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, key, &limits); err != nil {
		t.Fatal(err)
	}
	if len(limits.Spec.Limits) != 1 {
		t.Fatal("drift not repaired")
	}
	limits.OwnerReferences = nil
	if err := kube.Update(ctx, &limits); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileV1StorageDefaults(ctx, oc); err == nil {
		t.Fatal("adopted a foreign LimitRange")
	}
}

func TestV1StorageDefaultsGateWorkloads(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	scheme := ocv1TestScheme(t)
	denied := errors.New("storage admission policy denied")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc).WithStatusSubresource(oc).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.LimitRange); ok {
				return denied
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); !errors.Is(err, denied) {
		t.Fatalf("reconcile did not fail on the missing storage policy: %v", err)
	}
	// Bootstrap must not run until the namespace's storage admission defaults exist.
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: configMapName(oc)}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("bootstrap ran without storage policy: %v", err)
	}
}

// Native LimitRanger admission proves defaults cover future main and init containers, including
// child-operator pods. envtest has no kubelet, so this does not claim to measure eviction timing.
func TestV1StorageAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for v1 storage admission")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := ocv1TestScheme(t)
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Namespace = "v1-storage-admission"
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: oc.Namespace}}); err != nil {
		t.Fatal(err)
	}
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
	if err := r.reconcileV1StorageDefaults(ctx, oc); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, request, limit string
		admitted             bool
	}{
		{"default", "", "", true},
		{"explicit", "256Mi", "2Gi", true},
		{"zero", "0", "0", false},
		{"zero-request", "0", "1Gi", false},
		{"zero-limit", "1Mi", "0", false},
		{"request-exceeds-limit", "2Gi", "1Gi", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := corev1.Container{Name: "app", Image: "example/app:test"}
			if tc.request != "" {
				c.Resources.Requests = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse(tc.request)}
			}
			if tc.limit != "" {
				c.Resources.Limits = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse(tc.limit)}
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: oc.Namespace}, Spec: corev1.PodSpec{Containers: []corev1.Container{c}, InitContainers: []corev1.Container{{Name: "init", Image: "example/init:test"}}}}
			err := kube.Create(ctx, pod)
			if !tc.admitted {
				if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
					t.Fatalf("expected admission refusal, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertV1StorageBounds(t, pod.Spec)
			if tc.request == "" {
				for _, c := range []corev1.Container{pod.Spec.Containers[0], pod.Spec.InitContainers[0]} {
					request, limit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]
					if request.Cmp(resource.MustParse("100Mi")) != 0 || limit.Cmp(resource.MustParse("1Gi")) != 0 {
						t.Fatalf("incorrect defaults: %+v", c.Resources)
					}
				}
			}
		})
	}
}
