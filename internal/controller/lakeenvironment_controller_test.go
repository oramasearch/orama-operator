/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const testEnvironmentImage = "registry.example.com/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newLakeEnvironmentScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		networkingv1.AddToScheme,
		oramav1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return scheme
}

func TestLakeEnvironmentNamespacePreservesLegacyOwnership(t *testing.T) {
	spec := oramav1alpha1.LakeEnvironmentSpec{
		LakeRef:     "prod",
		Repository:  "repo-1",
		Environment: "production",
	}
	legacy := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "production-legacy",
			Namespace:   "lake",
			Annotations: map[string]string{deploymentIDAnnotation: "deploy-1"},
		},
		Spec: spec,
	}
	stable := legacy.DeepCopy()
	stable.Name = lakeEnvironmentResourceName(spec.LakeRef, spec.Repository, spec.Environment)
	legacyNamespace := lakeEnvironmentNamespace(legacy)
	stableNamespace := lakeEnvironmentNamespace(stable)
	if legacyNamespace == stableNamespace {
		t.Fatalf("legacy and stable resources share namespace %q", stableNamespace)
	}
	legacyDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("deploy-1")))[:10]
	if !strings.HasSuffix(legacyNamespace, legacyDigest) {
		t.Errorf("legacy namespace = %q, want deployment digest suffix %q", legacyNamespace, legacyDigest)
	}
}

func TestLakeEnvironmentCopiesDeliveredSealedSecrets(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	controller := true
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake", Image: "oramasearch/lake:test"},
	}
	environment := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "lake",
			UID:       "environment-uid",
			Annotations: map[string]string{
				deploymentIDAnnotation: "deploy-secret",
			},
		},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef:     "prod",
			Repository:  "repo-1",
			Environment: "production",
			Kind:        oramav1alpha1.LakeEnvironmentKindNamed,
			Change:      "release-1",
			Image:       testEnvironmentImage,
			Replicas:    1,
			Port:        8080,
			SecretRefs:  []string{"secret://database"},
		},
	}
	sealed := &oramav1alpha1.LakeSealedSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "database",
			Namespace:  "lake",
			UID:        "sealed-uid",
			Generation: 2,
		},
		Spec: oramav1alpha1.LakeSealedSecretSpec{
			LakeRef:  corev1.LocalObjectReference{Name: "prod"},
			Template: oramav1alpha1.LakeSealedSecretTemplate{Name: "application-database"},
			Source:   oramav1alpha1.LakeSealedSecretSource{Type: "literal"},
		},
		Status: oramav1alpha1.LakeSealedSecretStatus{
			Phase:              "Ready",
			ObservedGeneration: 2,
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Delivered",
				ObservedGeneration: 2,
			}},
		},
	}
	produced := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "application-database",
			Namespace: "lake",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: oramav1alpha1.GroupVersion.String(),
				Kind:       "LakeSealedSecret",
				Name:       sealed.Name,
				UID:        sealed.UID,
				Controller: &controller,
			}},
		},
		Data: map[string][]byte{"DATABASE_URL": []byte("postgresql://application")},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lake, environment, sealed, produced).
		WithStatusSubresource(environment, sealed, &appsv1.Deployment{}).
		Build()
	reconciler := &LakeEnvironmentReconciler{Client: kubeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "lake", Name: environment.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	workloadNamespace := lakeEnvironmentNamespace(environment)
	var copied corev1.Secret
	key := types.NamespacedName{Namespace: workloadNamespace, Name: produced.Name}
	if err := kubeClient.Get(ctx, key, &copied); err != nil {
		t.Fatalf("copied Secret: %v", err)
	}
	if string(copied.Data["DATABASE_URL"]) != "postgresql://application" {
		t.Errorf("copied data = %#v", copied.Data)
	}
	var deployment appsv1.Deployment
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: "environment"}, &deployment); err != nil {
		t.Fatal(err)
	}
	envFrom := deployment.Spec.Template.Spec.Containers[0].EnvFrom
	if len(envFrom) != 1 || envFrom[0].SecretRef == nil || envFrom[0].SecretRef.Name != produced.Name {
		t.Errorf("envFrom = %+v", envFrom)
	}

	if err := kubeClient.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	environment.Spec.SecretRefs = []string{"secret://unknown"}
	if err := kubeClient.Update(ctx, environment); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	if environment.Status.Phase != oramav1alpha1.LakeEnvironmentPhaseDegraded ||
		environment.Status.Conditions[0].Reason != "SecretReferenceNotFound" {
		t.Errorf("unknown reference status = %+v", environment.Status)
	}
	if err := kubeClient.Get(ctx, key, &copied); !apierrors.IsNotFound(err) {
		t.Errorf("stale copied Secret remains: %v", err)
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: "environment"}, &deployment); !apierrors.IsNotFound(err) {
		t.Errorf("workload remains with an unresolved secret: %v", err)
	}
}

func TestLakeEnvironmentReconcileCreatesProfiledPreview(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:       "lake",
			Image:           "oramasearch/lake:test",
			IngressEnabled:  true,
			PreviewWildcard: "preview.example.com",
		},
	}
	environment := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-preview-7ac4c59b",
			Namespace: "lake",
			UID:       "environment-uid",
			Annotations: map[string]string{
				deploymentIDAnnotation: "deploy-1",
			},
		},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef:     "prod",
			Repository:  "repo-1",
			Environment: "checkout-preview",
			Kind:        oramav1alpha1.LakeEnvironmentKindPreview,
			Change:      "7ac4c59b92e46235a6f2",
			Image:       testEnvironmentImage,
			Replicas:    1,
			Port:        8080,
			Env: []oramav1alpha1.LakeEnvironmentVariable{{
				Name:  "APP_ENV",
				Value: "preview",
			}},
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lake, environment).
		WithStatusSubresource(environment, &appsv1.Deployment{}, &networkingv1.Ingress{}).
		Build()
	reconciler := &LakeEnvironmentReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "lake", Name: environment.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if err := client.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	namespaceName := lakeEnvironmentNamespace(environment)
	var namespace corev1.Namespace
	if err := client.Get(ctx, types.NamespacedName{Name: namespaceName}, &namespace); err != nil {
		t.Fatalf("environment namespace: %v", err)
	}
	if namespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" ||
		namespace.Labels[environmentManagedByLabel] != environmentManagedByValue ||
		namespace.Annotations[environmentOwnerAnnotation] != "lake/checkout-preview-7ac4c59b" {
		t.Errorf("namespace profile = labels %#v annotations %#v", namespace.Labels, namespace.Annotations)
	}

	var deployment appsv1.Deployment
	workloadKey := types.NamespacedName{Namespace: namespaceName, Name: "environment"}
	if err := client.Get(ctx, workloadKey, &deployment); err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if deployment.Spec.Template.Spec.RuntimeClassName == nil || *deployment.Spec.Template.Spec.RuntimeClassName != "untrusted" {
		t.Errorf("runtimeClassName = %v", deployment.Spec.Template.Spec.RuntimeClassName)
	}
	if container.Image != testEnvironmentImage || container.Ports[0].ContainerPort != 8080 ||
		container.SecurityContext == nil || container.SecurityContext.RunAsNonRoot == nil ||
		!*container.SecurityContext.RunAsNonRoot {
		t.Errorf("container = %+v", container)
	}
	if deployment.Spec.RevisionHistoryLimit == nil || *deployment.Spec.RevisionHistoryLimit != 10 {
		t.Errorf("revisionHistoryLimit = %v", deployment.Spec.RevisionHistoryLimit)
	}

	var service corev1.Service
	if err := client.Get(ctx, workloadKey, &service); err != nil || service.Spec.Ports[0].Port != 80 || service.Spec.Ports[0].TargetPort.IntValue() != 8080 {
		t.Errorf("Service = %+v, err %v", service.Spec, err)
	}
	if err := client.Get(ctx, types.NamespacedName{Namespace: namespaceName, Name: environmentProfileName}, &corev1.ResourceQuota{}); err != nil {
		t.Errorf("ResourceQuota: %v", err)
	}
	if err := client.Get(ctx, types.NamespacedName{Namespace: namespaceName, Name: environmentProfileName}, &corev1.LimitRange{}); err != nil {
		t.Errorf("LimitRange: %v", err)
	}
	var policies networkingv1.NetworkPolicyList
	if err := client.List(ctx, &policies); err != nil || len(policies.Items) != 2 {
		t.Errorf("NetworkPolicies = %d, err %v", len(policies.Items), err)
	}
	var ingress networkingv1.Ingress
	if err := client.Get(ctx, workloadKey, &ingress); err != nil {
		t.Fatalf("Ingress: %v", err)
	}
	if ingress.Spec.Rules[0].Host != "7ac4c59b92e4.preview.example.com" {
		t.Errorf("preview host = %q", ingress.Spec.Rules[0].Host)
	}
	var dnsRecords oramav1alpha1.CloudflareDNSRecordList
	if err := client.List(ctx, &dnsRecords); err != nil || len(dnsRecords.Items) != 0 {
		t.Errorf("preview DNS records = %d, err %v", len(dnsRecords.Items), err)
	}
	if environment.Status.Phase != oramav1alpha1.LakeEnvironmentPhaseProgressing ||
		environment.Status.URL != "https://7ac4c59b92e4.preview.example.com" ||
		environment.Status.DeploymentID != "deploy-1" {
		t.Errorf("initial status = %+v", environment.Status)
	}

	deployment.Status.ReadyReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	deployment.Status.ObservedGeneration = deployment.Generation
	if err := client.Status().Update(ctx, &deployment); err != nil {
		t.Fatalf("update Deployment status: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("ready reconcile: %v", err)
	}
	if err := client.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	if environment.Status.Phase != oramav1alpha1.LakeEnvironmentPhaseReady || environment.Status.ReadyReplicas != 1 {
		t.Errorf("ready status = %+v", environment.Status)
	}
	resourceVersion := environment.ResourceVersion
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if err := client.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	if environment.ResourceVersion != resourceVersion {
		t.Errorf("steady status write changed resourceVersion from %s to %s", resourceVersion, environment.ResourceVersion)
	}
}

func TestLakeEnvironmentRefusesForeignNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake", Image: "oramasearch/lake:test"},
	}
	environment := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "preview",
			Namespace: "lake",
			Annotations: map[string]string{
				deploymentIDAnnotation: "deploy-foreign",
			},
		},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef:     "prod",
			Repository:  "repo-1",
			Environment: "preview",
			Kind:        oramav1alpha1.LakeEnvironmentKindPreview,
			Change:      "change",
			Image:       testEnvironmentImage,
			Replicas:    1,
			Port:        8080,
		},
	}
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lakeEnvironmentNamespace(environment)}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, environment, foreign).
		WithStatusSubresource(environment).
		Build()
	reconciler := &LakeEnvironmentReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "lake", Name: "preview"}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := client.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	if environment.Status.Phase != oramav1alpha1.LakeEnvironmentPhaseDegraded ||
		environment.Status.Conditions[0].Reason != "NamespaceConflict" {
		t.Errorf("status = %+v", environment.Status)
	}
	if foreign.Labels != nil || foreign.Annotations != nil {
		t.Errorf("foreign namespace was modified: %+v", foreign)
	}
}

func TestNamedEnvironmentRotatesDNSRecordWithHost(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake",
			Image:          "oramasearch/lake:test",
			IngressEnabled: true,
			DNSSecretRef:   &corev1.LocalObjectReference{Name: "cloudflare"},
		},
	}
	environment := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "lake",
			UID:       "environment-uid",
			Annotations: map[string]string{
				deploymentIDAnnotation: "deploy-named",
			},
		},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef:     "prod",
			Repository:  "repo-1",
			Environment: "production",
			Kind:        oramav1alpha1.LakeEnvironmentKindNamed,
			Change:      "release-1",
			Image:       testEnvironmentImage,
			Replicas:    1,
			Port:        8080,
			Host:        "app.example.com",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, environment).
		WithStatusSubresource(environment, &appsv1.Deployment{}, &networkingv1.Ingress{}).
		Build()
	reconciler := &LakeEnvironmentReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "lake", Name: environment.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	workloadKey := types.NamespacedName{Namespace: lakeEnvironmentNamespace(environment), Name: "environment"}
	var ingress networkingv1.Ingress
	if err := client.Get(ctx, workloadKey, &ingress); err != nil {
		t.Fatal(err)
	}
	ingress.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := client.Status().Update(ctx, &ingress); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	oldName := environmentDNSRecordName(environment, "app.example.com")
	var record oramav1alpha1.CloudflareDNSRecord
	if err := client.Get(ctx, types.NamespacedName{Namespace: "lake", Name: oldName}, &record); err != nil {
		t.Fatalf("initial DNS record: %v", err)
	}
	if record.Spec.Name != "app.example.com" || record.Spec.Content != "192.0.2.10" {
		t.Errorf("initial DNS record = %+v", record.Spec)
	}

	if err := client.Get(ctx, request.NamespacedName, environment); err != nil {
		t.Fatal(err)
	}
	environment.Spec.Host = "app-new.example.com"
	if err := client.Update(ctx, environment); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := client.List(ctx, &records); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 1 || records.Items[0].Spec.Name != "app-new.example.com" {
		t.Errorf("rotated DNS records = %+v", records.Items)
	}
}
