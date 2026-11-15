// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeEnvironmentDeliveryRevokesUnauthorizedCopies(t *testing.T) {
	for _, scenario := range []string{"missing environment", "removed reference", "wrong namespace", "foreign environment", "deleting environment"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			scheme := newLakeEnvironmentScheme(t)
			lake := &oramav1alpha1.Lake{
				ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
				Spec:       oramav1alpha1.LakeSpec{Namespace: "lake"},
			}
			environment := &oramav1alpha1.LakeEnvironment{
				ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "lake"},
				Spec:       oramav1alpha1.LakeEnvironmentSpec{LakeRef: "prod", SecretRefs: []string{"database"}},
			}
			owner := &oramav1alpha1.LakeSealedSecret{
				ObjectMeta: environmentObjectMeta(environment, "application-database", lakeEnvironmentNamespace(environment)),
				Spec: oramav1alpha1.LakeSealedSecretSpec{
					LakeRef:  corev1.LocalObjectReference{Name: "prod"},
					Template: oramav1alpha1.LakeSealedSecretTemplate{Name: "application-database"},
					Source: oramav1alpha1.LakeSealedSecretSource{
						Type:     "delivery",
						Delivery: &oramav1alpha1.LakeEnvironmentSecretDeliverySource{Environment: environment.Name, Name: "database"},
					},
				},
			}
			owner.UID = "local-owner"
			if err := controllerutil.SetControllerReference(lake, owner, scheme); err != nil {
				t.Fatal(err)
			}
			mutateUnauthorizedDelivery(scenario, environment, owner)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: owner.Spec.Template.Name, Namespace: owner.Namespace}}
			if err := controllerutil.SetControllerReference(owner, secret, scheme); err != nil {
				t.Fatal(err)
			}
			foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: owner.Namespace}}
			objects := []client.Object{lake, owner, secret, foreign}
			if scenario != "missing environment" {
				objects = append(objects, environment)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(owner).Build()
			reconciler := &LakeSealedSecretReconciler{Client: kube, Scheme: scheme}
			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)}); err != nil {
				t.Fatal(err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(secret), secret); !apierrors.IsNotFound(err) {
				t.Fatalf("unauthorized copy remains: %v", err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
				t.Fatalf("foreign Secret was changed: %v", err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(owner), owner); err != nil {
				t.Fatal(err)
			}
			if owner.Status.Phase != "Error" || owner.Status.Conditions[0].Reason != "DeliveryInvalid" {
				t.Fatalf("invalid delivery status: %+v", owner.Status)
			}
		})
	}
}

func mutateUnauthorizedDelivery(scenario string, environment *oramav1alpha1.LakeEnvironment, owner *oramav1alpha1.LakeSealedSecret) {
	switch scenario {
	case "removed reference":
		environment.Spec.SecretRefs = nil
	case "wrong namespace":
		owner.Namespace = "wrong"
	case "foreign environment":
		owner.Annotations[environmentOwnerAnnotation] = "lake/foreign"
	case "deleting environment":
		now := metav1.Now()
		environment.DeletionTimestamp = &now
		environment.Finalizers = []string{lakeEnvironmentFinalizer}
	}
}
