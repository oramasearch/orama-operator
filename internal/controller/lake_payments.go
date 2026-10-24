/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// A Lake's payments (operator#639, docs/lake/payments.md).
//
// Lake.spec.payments names a Secret holding one Stripe account or sandbox (accountID), the
// operator's restricted key (operatorKey) and Lake's (appKey), and the mode the Lake must bill in.
// From it the operator keeps, owned by the Lake in its namespace:
//
//   - a PaymentProvider lake-<name>-payments, with the operator key;
//   - a PaymentWebhookEndpoint of the same name at https://<host>/billing/stripe/webhook for the
//     events Lake handles, whose signing secret lands in Secret lake-<name>-payments-webhook;
//   - Lake's PaymentCatalog lake-<name>-catalog: its three products, without prices.
//
// It checks that Lake's own key sees the catalog's account and mode and can create card
// PaymentIntents, then gives the Lake LAKE_STRIPE_SECRET_KEY and LAKE_STRIPE_WEBHOOK_SECRET.
// A payments failure never fails the Lake: without the variables Lake takes payments through its manual provider.
// Condition PaymentsReady says where it stands.
//
// A Lake without spec.payments is not touched: no request to Stripe, no new object, no new env var.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=orama.cloud,resources=paymentproviders;paymentwebhookendpoints;paymentcatalogs,verbs=get;list;watch;create;update;patch;delete

const (
	lakePaymentsConditionType = "PaymentsReady"
	// lakePaymentsSecretHashKey is the pod-template annotation holding the digest of Lake's key and
	// the webhook signing secret, so a new value under the same names rolls the Lake.
	lakePaymentsSecretHashKey = "orama.cloud/payments-secret-hash"

	// The keys of spec.payments.secretRef.
	lakePaymentsAccountIDKey   = "accountID"
	lakePaymentsOperatorKeyKey = "operatorKey"
	lakePaymentsAppKeyKey      = "appKey"

	// lakeStripeWebhookPath is Lake's route for Stripe's events.
	lakeStripeWebhookPath = "/billing/stripe/webhook"
	// lakeStripeAPIVersion is the Stripe-Version Lake pins (lake include/lake/api/stripe.hpp), so
	// its endpoint delivers payloads of that version.
	lakeStripeAPIVersion = "2024-06-20"

	// lakePaymentCatalogVersion is the version of lakePaymentProducts: bump it with every change.
	lakePaymentCatalogVersion = 1
	// lakeUsageProduct is the product Lake's key is checked against.
	lakeUsageProduct = "lake_usage"
)

// lakeStripeWebhookEvents are the Stripe event types Lake's webhook handler takes (lake
// src/api/stripe.cpp): the invoice events, the Checkout events of lake#833's order sheet, and the
// subscription and invoice lifecycle that sets a cloud order's paid-through date (lake#972). Lake acknowledges a type it does not handle, so subscribing early is harmless.
// Change this list together with Lake's handler.
// Since lake v0.6.0 (lake#1114) also the account balance's events: a top-up's Checkout session, the
// card saved for auto top-up (setup_intent.succeeded) and each auto top-up charge (payment_intent.*).
var lakeStripeWebhookEvents = []string{
	"checkout.session.async_payment_failed",
	"checkout.session.async_payment_succeeded",
	"checkout.session.completed",
	"checkout.session.expired",
	"customer.subscription.created",
	"customer.subscription.deleted",
	"customer.subscription.paused",
	"customer.subscription.resumed",
	"customer.subscription.updated",
	"invoice.marked_uncollectible",
	"invoice.paid",
	"invoice.payment_action_required",
	"invoice.payment_failed",
	"invoice.updated",
	"invoice.voided",
	"payment_intent.payment_failed",
	"payment_intent.succeeded",
	"setup_intent.succeeded",
}

// lakePaymentProducts are Lake's products. Their IDs are a contract with Lake (lake#855): its
// invoice and Checkout lines name them with an inline price from Lake's own price list or order
// sheet, so none has a price here.
func lakePaymentProducts() []oramav1alpha1.PaymentProduct {
	return []oramav1alpha1.PaymentProduct{
		{ID: lakeUsageProduct, Name: "Lake usage"},
		{ID: "lake_private_warehouse", Name: "Lake Private Warehouse"},
		{ID: "lake_dedicated_cluster", Name: "Lake Dedicated Cluster"},
	}
}

var lakePaymentsEnvNames = []string{"LAKE_STRIPE_SECRET_KEY", "LAKE_STRIPE_WEBHOOK_SECRET"}

// stripeAccountIDPattern is PaymentProvider's spec.stripe.accountId pattern, checked before the
// provider is written so a malformed Secret is reported instead of failing the Lake's reconcile.
var stripeAccountIDPattern = regexp.MustCompile(`^acct_[A-Za-z0-9]+$`)

// lakePaymentsName names the Lake's PaymentProvider and PaymentWebhookEndpoint.
func lakePaymentsName(lake *oramav1alpha1.Lake) string { return "lake-" + lake.Name + "-payments" }

func lakePaymentsCatalogName(lake *oramav1alpha1.Lake) string {
	return "lake-" + lake.Name + "-catalog"
}

// lakePaymentsWebhookSecretName is the Secret the webhook endpoint writes its signing secret to.
func lakePaymentsWebhookSecretName(lake *oramav1alpha1.Lake) string {
	return lakePaymentsName(lake) + "-webhook"
}

// lakePaymentsState is what the Deployment and status need from a Lake's payments.
type lakePaymentsState struct {
	// Env gives the Lake LAKE_STRIPE_SECRET_KEY and LAKE_STRIPE_WEBHOOK_SECRET.
	Env bool
	// Hash is the payments-secret-hash annotation, set while Env is.
	Hash string
	// Condition is PaymentsReady; nil when the Lake has no spec.payments and nothing is left to remove.
	Condition *metav1.Condition
	// Requeue is when to look again: the next check of Lake's key, or a removal in progress.
	Requeue time.Duration
}

// lakePaymentsSecret is what spec.payments.secretRef holds.
type lakePaymentsSecret struct {
	accountID, operatorKey, appKey string
}

// reconcileLakePayments keeps the Lake's payment objects and returns what the Lake reads from
// them. Without spec.payments it removes what the field made.
func (r *LakeReconciler) reconcileLakePayments(ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string) (lakePaymentsState, error) {
	if lake.Spec.Payments == nil {
		done, err := r.removeLakePayments(ctx, lake, ns)
		if err != nil || done {
			return lakePaymentsState{}, err
		}
		return lakePaymentsState{Requeue: lakeStatusRequeue, Condition: &metav1.Condition{
			Type: lakePaymentsConditionType, Status: metav1.ConditionFalse, Reason: "Removing", ObservedGeneration: lake.Generation,
			Message: fmt.Sprintf("waiting for PaymentWebhookEndpoint %q to delete its endpoint before PaymentProvider %q goes",
				lakePaymentsName(lake), lakePaymentsName(lake)),
		}}, nil
	}
	condition := metav1.Condition{
		Type: lakePaymentsConditionType, Status: metav1.ConditionTrue, Reason: "Configured", ObservedGeneration: lake.Generation,
	}
	state := lakePaymentsState{Condition: &condition}
	fail := func(reason, message string) {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, reason, message
	}

	secret, reason, problem, err := r.readLakePaymentsSecret(ctx, lake, ns)
	if err != nil {
		return state, err
	}
	if problem != "" {
		// Nothing is created or changed, and the variables are withdrawn: a reference to a Secret
		// or key that is missing stops the pod from starting, and a key of the wrong mode must
		// never reach it.
		r.lakeAppKeyChecks.forget(lake.UID)
		fail(reason, problem)
		return state, nil
	}
	provider, endpoint, catalog, err := r.applyLakePayments(ctx, lake, ns, labels, secret)
	if err != nil {
		return state, err
	}
	signing, err := r.lakePaymentsSigningSecret(ctx, lake, ns)
	if err != nil {
		return state, err
	}

	credentials := currentCondition(provider.Status.Conditions, paymentConditionCredentials, provider.Generation)
	endpointReady := currentCondition(endpoint.Status.Conditions, paymentConditionReady, endpoint.Generation)
	catalogReady := currentCondition(catalog.Status.Conditions, paymentConditionReady, catalog.Generation)
	var appKeyFailure *metav1.Condition
	if catalogReady != nil && catalogReady.Status == metav1.ConditionTrue {
		appKeyFailure, state.Requeue = r.checkLakeAppKey(ctx, lake, secret)
	}
	switch {
	case credentials == nil:
		fail("ProviderNotReady", fmt.Sprintf("PaymentProvider %q has not checked its key yet", provider.Name))
	case credentials.Status != metav1.ConditionTrue:
		fail("ProviderNotReady", fmt.Sprintf("PaymentProvider %q: %s: %s", provider.Name, credentials.Reason, credentials.Message))
	case endpointReady == nil:
		fail("Progressing", fmt.Sprintf("waiting for PaymentWebhookEndpoint %q", endpoint.Name))
	case endpointReady.Status != metav1.ConditionTrue:
		fail(endpointReady.Reason, fmt.Sprintf("PaymentWebhookEndpoint %q: %s", endpoint.Name, endpointReady.Message))
	case catalogReady == nil:
		fail("Progressing", fmt.Sprintf("waiting for PaymentCatalog %q", catalog.Name))
	case catalogReady.Status != metav1.ConditionTrue:
		fail(catalogReady.Reason, fmt.Sprintf("PaymentCatalog %q: %s", catalog.Name, catalogReady.Message))
	case appKeyFailure != nil:
		fail(appKeyFailure.Reason, appKeyFailure.Message)
	case signing == "":
		fail("Progressing", fmt.Sprintf("waiting for Secret %q to hold the webhook signing secret", lakePaymentsWebhookSecretName(lake)))
	default:
		condition.Message = fmt.Sprintf("%s mode, account %s: webhook endpoint %s, %d products; the Lake has its Stripe keys",
			lake.Spec.Payments.Mode, secret.accountID, endpoint.Status.EndpointID, len(catalog.Spec.Products))
	}

	given, givenHash, err := r.lakePaymentsGiven(ctx, lake, ns)
	if err != nil {
		return state, err
	}
	switch {
	case condition.Status == metav1.ConditionTrue:
		state.Env, state.Hash = true, lakePaymentsHash(secret.appKey, signing)
	case given:
		// Given before: a later failure is reported but keeps the variables and the hash, so a
		// Stripe outage never takes the Lake back to manual payments, and a value not yet proven
		// (a rotated appKey, a signing secret being replaced) does not roll it.
		state.Env, state.Hash = true, givenHash
		condition.Message += "; the Lake keeps the Stripe keys it was given"
	}
	return state, nil
}

// currentCondition is the condition of conditionType when it was written for generation.
func currentCondition(conditions []metav1.Condition, conditionType string, generation int64) *metav1.Condition {
	condition := apimeta.FindStatusCondition(conditions, conditionType)
	if condition == nil || condition.ObservedGeneration != generation {
		return nil
	}
	return condition
}

// readLakePaymentsSecret reads spec.payments.secretRef. reason and problem say why it cannot be
// used: SecretMissing (no Secret, a key missing, an account ID that is not one) or ModeMismatch (a
// key that is not of spec.payments.mode).
func (r *LakeReconciler) readLakePaymentsSecret(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (secret lakePaymentsSecret, reason, problem string, err error) {
	name := lake.Spec.Payments.SecretRef.Name
	var stored corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &stored); err != nil {
		if apierrors.IsNotFound(err) {
			return secret, "SecretMissing", fmt.Sprintf("Secret %s/%s does not exist", ns, name), nil
		}
		return secret, "", "", err
	}
	value := func(key string) string { return strings.TrimSpace(string(stored.Data[key])) }
	secret = lakePaymentsSecret{
		accountID: value(lakePaymentsAccountIDKey), operatorKey: value(lakePaymentsOperatorKeyKey), appKey: value(lakePaymentsAppKeyKey),
	}
	for _, key := range []string{lakePaymentsAccountIDKey, lakePaymentsOperatorKeyKey, lakePaymentsAppKeyKey} {
		if value(key) == "" {
			return secret, "SecretMissing", fmt.Sprintf("Secret %s/%s has no %s", ns, name, key), nil
		}
	}
	if !stripeAccountIDPattern.MatchString(secret.accountID) {
		return secret, "SecretMissing", fmt.Sprintf("Secret %s/%s key %s is not a Stripe account ID (acct_...)", ns, name, lakePaymentsAccountIDKey), nil
	}
	for _, key := range []struct{ name, value string }{
		{lakePaymentsOperatorKeyKey, secret.operatorKey}, {lakePaymentsAppKeyKey, secret.appKey},
	} {
		switch mode := stripeKeyMode(key.value); {
		case mode == "":
			return secret, "ModeMismatch", fmt.Sprintf("Secret %s/%s key %s is not a Stripe secret or restricted key", ns, name, key.name), nil
		case mode != lake.Spec.Payments.Mode:
			return secret, "ModeMismatch", fmt.Sprintf("spec.payments.mode is %s but Secret %s/%s key %s is a %s-mode key",
				lake.Spec.Payments.Mode, ns, name, key.name, mode), nil
		}
	}
	return secret, "", "", nil
}

// applyLakePayments writes the Lake's PaymentProvider, PaymentWebhookEndpoint and PaymentCatalog
// and returns them as stored, status included.
func (r *LakeReconciler) applyLakePayments(
	ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string, secret lakePaymentsSecret,
) (*oramav1alpha1.PaymentProvider, *oramav1alpha1.PaymentWebhookEndpoint, *oramav1alpha1.PaymentCatalog, error) {
	name := lakePaymentsName(lake)
	providerRef := corev1.LocalObjectReference{Name: name}
	provider := &oramav1alpha1.PaymentProvider{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, provider, func() error {
		provider.Labels = labels
		provider.Spec = oramav1alpha1.PaymentProviderSpec{Type: paymentProviderStripe, Stripe: &oramav1alpha1.StripePaymentProviderSpec{
			AccountID: secret.accountID,
			Livemode:  lake.Spec.Payments.Mode == "live",
			APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{
				Name: lake.Spec.Payments.SecretRef.Name, Key: lakePaymentsOperatorKeyKey,
			},
		}}
		return controllerutil.SetControllerReference(lake, provider, r.Scheme)
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("payment provider: %w", err)
	}
	endpoint := &oramav1alpha1.PaymentWebhookEndpoint{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, endpoint, func() error {
		endpoint.Labels = labels
		endpoint.Spec = oramav1alpha1.PaymentWebhookEndpointSpec{
			ProviderRef:       providerRef,
			URL:               "https://" + lake.Spec.Host + lakeStripeWebhookPath,
			Events:            slices.Clone(lakeStripeWebhookEvents),
			APIVersion:        lakeStripeAPIVersion,
			App:               "lake",
			SigningSecretName: lakePaymentsWebhookSecretName(lake),
		}
		return controllerutil.SetControllerReference(lake, endpoint, r.Scheme)
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("payment webhook endpoint: %w", err)
	}
	catalog := &oramav1alpha1.PaymentCatalog{ObjectMeta: metav1.ObjectMeta{Name: lakePaymentsCatalogName(lake), Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, catalog, func() error {
		catalog.Labels = labels
		catalog.Spec = oramav1alpha1.PaymentCatalogSpec{
			ProviderRef: providerRef, App: "lake", Version: lakePaymentCatalogVersion, Products: lakePaymentProducts(),
		}
		return controllerutil.SetControllerReference(lake, catalog, r.Scheme)
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("payment catalog: %w", err)
	}
	return provider, endpoint, catalog, nil
}

// lakePaymentsSigningSecret is the webhook signing secret, or "" while its Secret does not hold one.
func (r *LakeReconciler) lakePaymentsSigningSecret(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lakePaymentsWebhookSecretName(lake)}, &secret); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return string(secret.Data[paymentWebhookSigningSecretKey]), nil
}

// lakePaymentsGiven is whether the Lake's Deployment already has the Stripe variables as this spec
// renders them, and its payments-secret-hash.
func (r *LakeReconciler) lakePaymentsGiven(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (bool, string, error) {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: "lake-" + lake.Name}, &deployment); err != nil {
		return false, "", client.IgnoreNotFound(err)
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != "lake" {
			continue
		}
		for _, want := range lakePaymentsEnv(lake, lakePaymentsState{Env: true}) {
			i := slices.IndexFunc(container.Env, func(have corev1.EnvVar) bool { return have.Name == want.Name })
			if i < 0 || !apiequality.Semantic.DeepEqual(container.Env[i], want) {
				return false, "", nil
			}
		}
		return true, deployment.Spec.Template.Annotations[lakePaymentsSecretHashKey], nil
	}
	return false, "", nil
}

// lakePaymentsHash is the payments-secret-hash: Lake's key and the signing secret, the two values
// the Lake reads under names that do not change.
func lakePaymentsHash(appKey, signingSecret string) string {
	sum := sha256.Sum256([]byte(appKey + "\x00" + signingSecret))
	return fmt.Sprintf("%x", sum[:])
}

// lakePaymentsEnv is the Lake's Stripe variables, given once state.Env. Both names are reserved
// whenever spec.payments is set (lakeEnv drops them from spec.env).
func lakePaymentsEnv(lake *oramav1alpha1.Lake, state lakePaymentsState) []corev1.EnvVar {
	if lake.Spec.Payments == nil || !state.Env {
		return nil
	}
	ref := func(secret, key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
		}}
	}
	return []corev1.EnvVar{
		{Name: "LAKE_STRIPE_SECRET_KEY", ValueFrom: ref(lake.Spec.Payments.SecretRef.Name, lakePaymentsAppKeyKey)},
		{Name: "LAKE_STRIPE_WEBHOOK_SECRET", ValueFrom: ref(lakePaymentsWebhookSecretName(lake), paymentWebhookSigningSecretKey)},
	}
}

// setLakePaymentsSecretHash writes the payments-secret-hash pod-template annotation, or removes it
// while the Lake has no Stripe variables.
func setLakePaymentsSecretHash(template *metav1.ObjectMeta, hash string) {
	if hash == "" {
		delete(template.Annotations, lakePaymentsSecretHashKey)
		return
	}
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[lakePaymentsSecretHashKey] = hash
}

// ── Lake's own key ───────────────────────────────────────────────────────────

// lakeAppKeyChecks throttles the check of each Lake's key by paymentCheckClock's rule (hourly, and
// a minute after Stripe did not answer; a changed key at once) and remembers what the last check
// found, which the Lake reports between checks.
type lakeAppKeyChecks struct {
	clock    paymentCheckClock
	mu       sync.Mutex
	failures map[types.UID]*metav1.Condition
}

// check returns what is wrong with the key digest names (nil when nothing is) and when to check
// again. run asks Stripe, only when a check is due.
func (c *lakeAppKeyChecks) check(uid types.UID, digest string, run func() *metav1.Condition) (*metav1.Condition, time.Duration) {
	if wait := c.clock.wait(uid, digest, paymentRecheck); wait > 0 {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.failures[uid], wait
	}
	failure := run()
	c.mu.Lock()
	if c.failures == nil {
		c.failures = map[types.UID]*metav1.Condition{}
	}
	c.failures[uid] = failure
	c.mu.Unlock()
	// Recheck missing permissions soon too: a Dashboard edit does not change the key's digest.
	c.clock.record(uid, digest, failure == nil ||
		(failure.Reason != "StripeAPIError" && failure.Reason != "AppKeyPermissionsMissing"))
	return failure, c.clock.wait(uid, digest, paymentRecheck)
}

func (c *lakeAppKeyChecks) forget(uid types.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock.forget(uid)
	delete(c.failures, uid)
}

// checkLakeAppKey checks the catalog's account and mode and permission to charge saved cards.
// It returns what is wrong (nil when nothing is) and when to check again. Between checks it
// returns the last result without asking Stripe.
func (r *LakeReconciler) checkLakeAppKey(ctx context.Context, lake *oramav1alpha1.Lake, secret lakePaymentsSecret) (*metav1.Condition, time.Duration) {
	sum := sha256.Sum256([]byte(strings.Join([]string{secret.appKey, secret.accountID, lake.Spec.Payments.Mode}, "\x00")))
	return r.lakeAppKeyChecks.check(lake.UID, fmt.Sprintf("%x", sum[:]), func() *metav1.Condition {
		failure := r.lakeAppKeyFailure(ctx, lake, secret.appKey)
		if failure != nil {
			logf.FromContext(ctx).Info("Lake's Stripe key check failed", "reason", failure.Reason, "message", failure.Message)
		}
		return failure
	})
}

// lakeAppKeyFailure asks Stripe for lake_usage with Lake's key (it has Products read). The catalog
// made the product with the operator's key, so a 404 means the two keys belong to different
// accounts or sandboxes.
func (r *LakeReconciler) lakeAppKeyFailure(ctx context.Context, lake *oramav1alpha1.Lake, appKey string) *metav1.Condition {
	product, err := stripeDo[stripeLivemode](ctx, stripeAPI{Base: r.StripeAPIBase, Key: appKey}, stripeCall{
		Method: http.MethodGet, Path: "/v1/products/" + lakeUsageProduct, Version: lakeStripeAPIVersion,
	})
	var apiErr *stripeAPIError
	var failure metav1.Condition
	switch {
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		failure = paymentCondition(false, "AppKeyMismatch", fmt.Sprintf(
			"%s cannot see product %s, which the catalog keeps with %s: the two keys belong to different accounts or sandboxes",
			lakePaymentsAppKeyKey, lakeUsageProduct, lakePaymentsOperatorKeyKey))
	case err != nil:
		failure = paymentCondition(false, "StripeAPIError", "checking "+lakePaymentsAppKeyKey+": "+
			stripeCheckFailure("read product "+lakeUsageProduct, err))
	case stripeModeName(product.Livemode) != lake.Spec.Payments.Mode:
		failure = paymentCondition(false, "ModeMismatch", fmt.Sprintf("spec.payments.mode is %s but %s sees a %s-mode product %s",
			lake.Spec.Payments.Mode, lakePaymentsAppKeyKey, stripeModeName(product.Livemode), lakeUsageProduct))
	default:
		return r.lakeAppKeyChargeFailure(ctx, appKey)
	}
	return &failure
}

// lakeAppKeyChargeFailure probes Payment Intents write without creating a payment: amount is
// required and deliberately absent. An explicit card type scopes Stripe's permission check to
// the saved cards Lake charges. Only the expected missing-amount validation error passes.
func (r *LakeReconciler) lakeAppKeyChargeFailure(ctx context.Context, appKey string) *metav1.Condition {
	_, err := stripeDo[struct{}](ctx, stripeAPI{Base: r.StripeAPIBase, Key: appKey}, stripeCall{
		Method: http.MethodPost, Path: "/v1/payment_intents", Version: lakeStripeAPIVersion,
		Form: url.Values{"currency": {"usd"}, stripeField("payment_method_types", ""): {"card"}},
	})
	var apiErr *stripeAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		apiErr.Type == "invalid_request_error" && apiErr.Code == "parameter_missing" && apiErr.Param == "amount" {
		return nil
	}
	if stripeForbidden(err) {
		failure := paymentCondition(false, "AppKeyPermissionsMissing",
			"appKey cannot create card PaymentIntents for auto top-up; grant Payment Intents write in the Stripe Dashboard")
		return &failure
	}
	message := "checking appKey: unexpected success from the PaymentIntent permission probe without an amount"
	if err != nil {
		message = "checking appKey: " + stripeCheckFailure("create card PaymentIntents for auto top-up", err)
	}
	failure := paymentCondition(false, "StripeAPIError", message)
	return &failure
}

// ── Removal ──────────────────────────────────────────────────────────────────

// removeLakePayments deletes what spec.payments made, when the field goes or the Lake is deleted.
// The caller already renders no Stripe variables. The catalog goes (Stripe's products stay) and the
// webhook endpoint is deleted in the foreground, so it is gone only once its finalizer has deleted
// the endpoint in Stripe with the provider's key; the provider goes after that. done is false while
// the endpoint is still being deleted. A Lake that never had payments finds nothing in the cache
// and writes nothing.
func (r *LakeReconciler) removeLakePayments(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (done bool, err error) {
	r.lakeAppKeyChecks.forget(lake.UID)
	if _, err := r.deleteLakePaymentsChild(ctx, lake, client.ObjectKey{Namespace: ns, Name: lakePaymentsCatalogName(lake)},
		&oramav1alpha1.PaymentCatalog{}); err != nil {
		return false, err
	}
	key := client.ObjectKey{Namespace: ns, Name: lakePaymentsName(lake)}
	endpointLeft, err := r.deleteLakePaymentsChild(ctx, lake, key, &oramav1alpha1.PaymentWebhookEndpoint{},
		client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err != nil || endpointLeft {
		return false, err
	}
	_, err = r.deleteLakePaymentsChild(ctx, lake, key, &oramav1alpha1.PaymentProvider{})
	return err == nil, err
}

// deleteLakePaymentsChild deletes the object at key when the Lake controls it, unless it is being
// deleted already. left is whether it still existed: the next reconcile, which its deletion
// triggers, finds it gone.
func (r *LakeReconciler) deleteLakePaymentsChild(
	ctx context.Context, lake *oramav1alpha1.Lake, key client.ObjectKey, object client.Object, opts ...client.DeleteOption,
) (left bool, err error) {
	if err := r.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	if !metav1.IsControlledBy(object, lake) {
		return false, nil
	}
	if object.GetDeletionTimestamp().IsZero() {
		if err := r.Delete(ctx, object, opts...); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		logf.FromContext(ctx).Info("deleting payment object", "kind", fmt.Sprintf("%T", object), "name", key.Name)
	}
	return true, nil
}

// lakeRequestsForPaymentsSecret maps a Lake's payments Secret and its webhook signing Secret to the
// Lake, so a new value under the same name rolls it: the signing Secret belongs to the webhook
// endpoint, not to the Lake, and the payments Secret to a person.
func (r *LakeReconciler) lakeRequestsForPaymentsSecret(ctx context.Context, object client.Object) []reconcile.Request {
	var lakes oramav1alpha1.LakeList
	if err := r.List(ctx, &lakes); err != nil {
		logf.FromContext(ctx).Error(err, "list Lakes for a payments Secret", "secret", client.ObjectKeyFromObject(object))
		return nil
	}
	var requests []reconcile.Request
	for i := range lakes.Items {
		lake := &lakes.Items[i]
		if lake.Spec.Payments == nil || lakeNamespace(lake) != object.GetNamespace() {
			continue
		}
		if name := object.GetName(); name == lake.Spec.Payments.SecretRef.Name || name == lakePaymentsWebhookSecretName(lake) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: lake.Name}})
		}
	}
	return requests
}
