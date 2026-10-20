/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

//go:embed assets/cloud-v1-payment-catalog.yaml
var dashboardPaymentCatalogYAML []byte

const dashboardPaymentsCondition = "PaymentsReady"

var dashboardPaymentsEnvNames = []string{"STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET"}

// These match the dashboard's src/app/api/webhooks/stripe/route.ts and src/lib/stripe/client.ts.
var dashboardStripeWebhookEvents = []string{
	"customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted",
	"invoice.paid", "invoice.payment_failed", "payment_method.attached", "payment_method.detached",
	"checkout.session.completed",
}

func dashboardPaymentCatalog() (oramav1alpha1.PaymentCatalogSpec, error) {
	var spec oramav1alpha1.PaymentCatalogSpec
	err := yaml.UnmarshalStrict(dashboardPaymentCatalogYAML, &spec)
	return spec, err
}

func validateDashboardPayments(oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Dashboard
	if d.Payments == nil {
		return nil
	}
	if !d.Enabled || d.Host == "" {
		return fmt.Errorf("dashboard.payments requires the dashboard enabled with a host")
	}
	if d.Payments.SecretRef.Name == "" {
		return fmt.Errorf("dashboard.payments.secretRef needs a name")
	}
	prices := oc.Spec.Components.ControlDB.StripePrices
	if len(prices.Plans) != 0 || len(prices.Addons) != 0 {
		return fmt.Errorf("dashboard.payments and non-empty controlDB.stripePrices cannot be set together")
	}
	return nil
}

// readDashboardPaymentsSecret checks both keys locally before any child is written or key reaches
// the dashboard. The provider controller verifies the operator key's account against Stripe.
func (r *OramaCloudV1Reconciler) readDashboardPaymentsSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (accountID, reason, problem string, err error) {
	if err := validateDashboardPayments(oc); err != nil {
		return accountID, "InvalidSpec", err.Error(), nil
	}
	name := oc.Spec.Components.Dashboard.Payments.SecretRef.Name
	var stored corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, &stored); err != nil {
		if apierrors.IsNotFound(err) {
			return accountID, "SecretMissing", fmt.Sprintf("Secret %s/%s does not exist", oc.Namespace, name), nil
		}
		return accountID, "", "", err
	}
	value := func(key string) string { return strings.TrimSpace(string(stored.Data[key])) }
	accountID = value(lakePaymentsAccountIDKey)
	for _, key := range []string{lakePaymentsAccountIDKey, lakePaymentsOperatorKeyKey, lakePaymentsAppKeyKey} {
		if value(key) == "" {
			return accountID, "SecretMissing", fmt.Sprintf("Secret %s/%s has no %s", oc.Namespace, name, key), nil
		}
	}
	if !stripeAccountIDPattern.MatchString(accountID) {
		return accountID, "InvalidSecret", fmt.Sprintf("Secret %s/%s accountID is not a Stripe account ID (acct_...)", oc.Namespace, name), nil
	}
	for _, key := range []string{lakePaymentsOperatorKeyKey, lakePaymentsAppKeyKey} {
		switch stripeKeyMode(value(key)) {
		case "live":
			return accountID, "LiveModeRefused", fmt.Sprintf("Secret %s/%s key %s is live-mode; dashboard.payments is sandbox-only", oc.Namespace, name, key), nil
		case "test":
		default:
			return accountID, "InvalidSecret", fmt.Sprintf("Secret %s/%s key %s is not a Stripe secret or restricted key", oc.Namespace, name, key), nil
		}
	}
	return accountID, "", "", nil
}

// reconcileDashboardPayments owns the sandbox objects and reports progress separately from Ready.
// Removing payments waits for the dashboard to withdraw its env before deleting the endpoint, then
// waits for the endpoint's finalizer before deleting the provider that supplies its credentials.
func (r *OramaCloudV1Reconciler) reconcileDashboardPayments(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (*metav1.Condition, error) {
	condition := &metav1.Condition{Type: dashboardPaymentsCondition, Status: metav1.ConditionFalse, Reason: "Progressing", ObservedGeneration: oc.Generation}
	if oc.Spec.Components.Dashboard.Payments == nil {
		done, err := r.removeDashboardPayments(ctx, oc)
		if done || err != nil {
			return nil, err
		}
		condition.Reason, condition.Message = "Removing", "withdrawing dashboard keys, then removing the webhook endpoint before its provider"
		return condition, nil
	}
	accountID, reason, problem, err := r.readDashboardPaymentsSecret(ctx, oc)
	if err != nil {
		return nil, err
	}
	if problem != "" {
		condition.Reason, condition.Message = reason, problem
		return condition, nil
	}
	name := oc.Name + "-payments"
	provider := &oramav1alpha1.PaymentProvider{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, provider, func() error {
		provider.Labels = labelsFor(oc, "dashboard")
		provider.Spec = oramav1alpha1.PaymentProviderSpec{Type: paymentProviderStripe, Stripe: &oramav1alpha1.StripePaymentProviderSpec{
			AccountID: accountID, APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: oc.Spec.Components.Dashboard.Payments.SecretRef.Name, Key: lakePaymentsOperatorKeyKey},
		}}
		return controllerutil.SetControllerReference(oc, provider, r.Scheme)
	}); err != nil {
		return nil, err
	}
	spec, err := dashboardPaymentCatalog()
	if err != nil {
		return nil, err
	}
	spec.ProviderRef.Name = name
	catalog := &oramav1alpha1.PaymentCatalog{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-catalog", Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, catalog, func() error {
		catalog.Labels, catalog.Spec = labelsFor(oc, "dashboard"), spec
		return controllerutil.SetControllerReference(oc, catalog, r.Scheme)
	}); err != nil {
		return nil, err
	}
	endpoint := &oramav1alpha1.PaymentWebhookEndpoint{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, endpoint, func() error {
		endpoint.Labels = labelsFor(oc, "dashboard")
		endpoint.Spec = oramav1alpha1.PaymentWebhookEndpointSpec{
			ProviderRef: corev1.LocalObjectReference{Name: name}, URL: "https://" + oc.Spec.Components.Dashboard.Host + "/api/webhooks/stripe",
			APIVersion: "2025-10-29.clover", App: "cloud_v1", Events: slices.Clone(dashboardStripeWebhookEvents), SigningSecretName: name + "-webhook",
		}
		return controllerutil.SetControllerReference(oc, endpoint, r.Scheme)
	}); err != nil {
		return nil, err
	}
	for _, child := range []struct {
		name      string
		condition *metav1.Condition
	}{
		{provider.Name, currentCondition(provider.Status.Conditions, paymentConditionCredentials, provider.Generation)},
		{catalog.Name, currentCondition(catalog.Status.Conditions, paymentConditionReady, catalog.Generation)},
		{endpoint.Name, currentCondition(endpoint.Status.Conditions, paymentConditionReady, endpoint.Generation)},
	} {
		if child.condition == nil {
			condition.Message = fmt.Sprintf("waiting for %s", child.name)
			return condition, nil
		}
		if child.condition.Status != metav1.ConditionTrue {
			condition.Reason, condition.Message = child.condition.Reason, fmt.Sprintf("%s: %s", child.name, child.condition.Message)
			return condition, nil
		}
	}
	env, err := r.dashboardPaymentsEnv(ctx, oc)
	if err != nil {
		return nil, err
	}
	if len(env) == 0 {
		condition.Message = "waiting for catalog prices and webhook signing Secret"
		return condition, nil
	}
	condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "Configured", "sandbox catalog, webhook and dashboard Stripe keys configured"
	return condition, nil
}

// dashboardPaymentsEnv gives explicit references only once both Secrets and the catalog's prices
// exist. A transient catalog failure keeps its last prices and therefore keeps the keys available.
func (r *OramaCloudV1Reconciler) dashboardPaymentsEnv(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]corev1.EnvVar, error) {
	if oc.Spec.Components.Dashboard.Payments == nil {
		return nil, nil
	}
	_, _, problem, err := r.readDashboardPaymentsSecret(ctx, oc)
	if err != nil || problem != "" {
		return nil, err
	}
	var catalog oramav1alpha1.PaymentCatalog
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-catalog"}, &catalog); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if len(catalog.Status.Prices) == 0 {
		return nil, nil
	}
	var signing corev1.Secret
	name := oc.Name + "-payments-webhook"
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, &signing); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if len(signing.Data[paymentWebhookSigningSecretKey]) == 0 {
		return nil, nil
	}
	ref := func(name, key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
	}
	return []corev1.EnvVar{
		{Name: "STRIPE_SECRET_KEY", ValueFrom: ref(oc.Spec.Components.Dashboard.Payments.SecretRef.Name, lakePaymentsAppKeyKey)},
		{Name: "STRIPE_WEBHOOK_SECRET", ValueFrom: ref(name, paymentWebhookSigningSecretKey)},
	}, nil
}

func dashboardUserEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	env := slices.Clone(oc.Spec.Components.Dashboard.Env)
	if oc.Spec.Components.Dashboard.Payments != nil {
		env = slices.DeleteFunc(env, func(e corev1.EnvVar) bool { return slices.Contains(dashboardPaymentsEnvNames, e.Name) })
	}
	return env
}

// dashboardStripePrices uses the last catalog status independently of Ready. Sorted lookup keys
// make the generated SQL stable across reconciles; an unknown lookup key is an error, never a row.
func (r *OramaCloudV1Reconciler) dashboardStripePrices(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (oramav1alpha1.ControlDBStripePrices, error) {
	if oc.Spec.Components.Dashboard.Payments == nil {
		return oc.Spec.Components.ControlDB.StripePrices, nil
	}
	var prices oramav1alpha1.ControlDBStripePrices
	var catalog oramav1alpha1.PaymentCatalog
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-catalog"}, &catalog); err != nil {
		return prices, client.IgnoreNotFound(err)
	}
	plans := map[string]oramav1alpha1.StripePlanPrice{}
	for _, key := range slices.Sorted(maps.Keys(catalog.Status.Prices)) {
		parts := strings.Split(key, ".")
		if len(parts) == 3 && parts[0] == "cloud_v1" && parts[1] == "addon" {
			prices.Addons = append(prices.Addons, oramav1alpha1.StripeAddonPrice{ID: parts[2], PriceID: catalog.Status.Prices[key]})
		} else if len(parts) == 4 && parts[0] == "cloud_v1" && parts[1] == "plan" && (parts[3] == "monthly" || parts[3] == "yearly") {
			plan := plans[parts[2]]
			plan.ID = parts[2]
			if parts[3] == "monthly" {
				plan.Monthly = catalog.Status.Prices[key]
			} else {
				plan.Yearly = catalog.Status.Prices[key]
			}
			plans[parts[2]] = plan
		} else {
			return prices, fmt.Errorf("PaymentCatalog %s has an invalid dashboard lookup key %q", catalog.Name, key)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(plans)) {
		prices.Plans = append(prices.Plans, plans[id])
	}
	return prices, nil
}

func (r *OramaCloudV1Reconciler) removeDashboardPayments(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (bool, error) {
	var endpoint oramav1alpha1.PaymentWebhookEndpoint
	endpointErr := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-payments"}, &endpoint)
	if endpointErr != nil && !apierrors.IsNotFound(endpointErr) && !apimeta.IsNoMatchError(endpointErr) {
		return false, endpointErr
	}
	if endpointErr == nil && metav1.IsControlledBy(&endpoint, oc) {
		var deployment appsv1.Deployment
		if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-dashboard"}, &deployment); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, err
			}
		} else {
			for _, container := range deployment.Spec.Template.Spec.Containers {
				for _, env := range container.Env {
					if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == oc.Name+"-payments-webhook" {
						return false, nil
					}
				}
			}
		}
	}
	remove := func(name string, child client.Object, opts ...client.DeleteOption) (bool, error) {
		if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, child); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				return false, nil
			}
			return false, err
		}
		if !metav1.IsControlledBy(child, oc) {
			return false, nil
		}
		if child.GetDeletionTimestamp().IsZero() {
			if err := r.Delete(ctx, child, opts...); err != nil {
				return false, client.IgnoreNotFound(err)
			}
		}
		return true, nil
	}
	if _, err := remove(oc.Name+"-catalog", &oramav1alpha1.PaymentCatalog{}); err != nil {
		return false, err
	}
	left, err := remove(oc.Name+"-payments", &oramav1alpha1.PaymentWebhookEndpoint{}, client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err != nil || left {
		return false, err
	}
	left, err = remove(oc.Name+"-payments", &oramav1alpha1.PaymentProvider{})
	return !left && err == nil, err
}
