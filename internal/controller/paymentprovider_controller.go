/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	paymentProviderStripe       = "stripe"
	paymentConditionCredentials = "Credentials"

	// A checked key is checked again this often, so a key revoked or narrowed in the vendor's
	// dashboard is reported; a failed check, or a missing Secret, is looked at again sooner.
	paymentRecheck = time.Hour
	paymentRetry   = time.Minute
)

// paymentCheckClock remembers, per object, the last check against the vendor and what it was for,
// so a steady object makes no vendor request between rechecks (lakeClerkClock's rule). now
// replaces the time in tests.
type paymentCheckClock struct {
	mu   sync.Mutex
	last map[types.UID]paymentCheck
	now  func() time.Time
}

type paymentCheck struct {
	digest string
	at     time.Time
	ok     bool
}

func (c *paymentCheckClock) time() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// wait is how long until the object's next check: zero when it was never checked or was checked
// for something else, else what is left of recheck, or of paymentRetry after a failure.
func (c *paymentCheckClock) wait(uid types.UID, digest string, recheck time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	check, found := c.last[uid]
	if !found || check.digest != digest {
		return 0
	}
	interval := paymentRetry
	if check.ok {
		interval = recheck
	}
	return max(0, check.at.Add(interval).Sub(c.time()))
}

func (c *paymentCheckClock) record(uid types.UID, digest string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[types.UID]paymentCheck{}
	}
	c.last[uid] = paymentCheck{digest: digest, at: c.time(), ok: ok}
}

func (c *paymentCheckClock) forget(uid types.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.last, uid)
}

// paymentSecretValue reads one key of a Secret; problem says why there is none.
func paymentSecretValue(ctx context.Context, c client.Client, namespace string, ref oramav1alpha1.PaymentSecretKeyRef) (value, problem string, err error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("Secret %q does not exist", ref.Name), nil
		}
		return "", "", err
	}
	value = strings.TrimSpace(string(secret.Data[ref.Key]))
	if value == "" {
		return "", fmt.Sprintf("Secret %q has no key %q", ref.Name, ref.Key), nil
	}
	return value, "", nil
}

// paymentCredential is a concrete payment kind's key once its provider's Credentials are True for
// the provider's current spec. Otherwise problem says why not, and gone whether the provider or the
// key's Secret no longer exists.
func paymentCredential(ctx context.Context, c client.Client, namespace, providerName string, keyRef oramav1alpha1.PaymentSecretKeyRef) (key, problem string, gone bool, err error) {
	var provider oramav1alpha1.PaymentProvider
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: providerName}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("PaymentProvider %q does not exist", providerName), true, nil
		}
		return "", "", false, err
	}
	key, problem, err = paymentSecretValue(ctx, c, namespace, keyRef)
	if err != nil || problem != "" {
		return "", problem, problem != "", err
	}
	credentials := meta.FindStatusCondition(provider.Status.Conditions, paymentConditionCredentials)
	switch {
	case credentials == nil || credentials.ObservedGeneration != provider.Generation:
		return "", fmt.Sprintf("PaymentProvider %q has not checked its key yet", providerName), false, nil
	case credentials.Status != metav1.ConditionTrue:
		return "", fmt.Sprintf("PaymentProvider %q: Credentials %s (%s)", providerName, credentials.Reason, credentials.Message), false, nil
	}
	return key, "", false, nil
}

// PaymentProviderReconciler checks a provider's key against its vendor and reports the result in
// condition Credentials. For Stripe: the key's prefix against spec.stripe.livemode, then
// GET /v1/account against spec.stripe.accountId. A restricted key that may not read the account is
// checked by listing webhook endpoints instead, which proves the key and its mode but not the
// account (ModeVerified). Nothing here changes anything in Stripe.
type PaymentProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// StripeAPIBase is Stripe's API; empty is https://api.stripe.com. Tests point it at a fake.
	StripeAPIBase string
	// clock throttles the check per provider.
	clock paymentCheckClock
}

// +kubebuilder:rbac:groups=orama.cloud,resources=paymentproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=paymentproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *PaymentProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var provider oramav1alpha1.PaymentProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	condition, after, err := r.credentials(ctx, &provider)
	if err != nil {
		return ctrl.Result{}, err
	}
	condition.Type, condition.ObservedGeneration = paymentConditionCredentials, provider.Generation
	status := *provider.Status.DeepCopy()
	meta.SetStatusCondition(&status.Conditions, condition)
	if !equality.Semantic.DeepEqual(provider.Status, status) {
		logf.FromContext(ctx).Info("payment provider credentials changed", "status", condition.Status, "reason", condition.Reason)
		provider.Status = status
		if err := r.Status().Update(ctx, &provider); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func paymentCondition(ok bool, reason, message string) metav1.Condition {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{Status: status, Reason: reason, Message: message}
}

// credentials is the Credentials condition, without its type and generation, and when to look
// again. Between rechecks it is the condition the provider already has.
func (r *PaymentProviderReconciler) credentials(ctx context.Context, provider *oramav1alpha1.PaymentProvider) (metav1.Condition, time.Duration, error) {
	spec := provider.Spec.Stripe
	if provider.Spec.Type != paymentProviderStripe || spec == nil {
		return paymentCondition(false, "UnsupportedType", fmt.Sprintf("provider type %q is not supported", provider.Spec.Type)), 0, nil
	}
	key, problem, err := paymentSecretValue(ctx, r.Client, provider.Namespace, spec.APIKeySecretRef)
	if err != nil {
		return metav1.Condition{}, 0, err
	}
	if problem != "" {
		// The Secret's watch brings it back at once; the retry is for a Secret the cache missed.
		r.clock.forget(provider.UID)
		return paymentCondition(false, "SecretMissing", problem), paymentRetry, nil
	}

	sum := sha256.Sum256([]byte(strings.Join([]string{key, spec.AccountID, strconv.FormatBool(spec.Livemode)}, "\x00")))
	digest := fmt.Sprintf("%x", sum[:])
	previous := meta.FindStatusCondition(provider.Status.Conditions, paymentConditionCredentials)
	if wait := r.clock.wait(provider.UID, digest, paymentRecheck); wait > 0 && previous != nil {
		return *previous, wait, nil
	}
	condition := r.checkStripe(ctx, spec, key)
	ok := condition.Reason != "StripeAPIError"
	r.clock.record(provider.UID, digest, ok)
	if !ok {
		return condition, paymentRetry, nil
	}
	return condition, paymentRecheck, nil
}

// stripeKeyMode is "live" or "test" from a secret or restricted key's prefix, or "" for anything
// else.
func stripeKeyMode(key string) string {
	for _, kind := range []string{"sk_", "rk_"} {
		for _, mode := range []string{"live", "test"} {
			if strings.HasPrefix(key, kind+mode+"_") {
				return mode
			}
		}
	}
	return ""
}

func stripeModeName(livemode bool) string {
	if livemode {
		return "live"
	}
	return "test"
}

// stripeCheckFailure describes a failed Stripe call for a condition: the helper's error, which
// never carries the key.
func stripeCheckFailure(action string, err error) string {
	var apiErr *stripeAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		return fmt.Sprintf("Stripe does not accept the key (HTTP 401) to %s: it is revoked, expired or mistyped", action)
	}
	return fmt.Sprintf("cannot %s: %v", action, err)
}

// stripeAccount is what the provider reads of GET /v1/account.
type stripeAccount struct {
	ID string `json:"id"`
}

// stripeLivemode is what the provider reads of a listed Stripe object.
type stripeLivemode struct {
	Livemode bool `json:"livemode"`
}

// checkStripe checks the key's mode, then the account it belongs to.
func (r *PaymentProviderReconciler) checkStripe(ctx context.Context, spec *oramav1alpha1.StripePaymentProviderSpec, key string) metav1.Condition {
	want := stripeModeName(spec.Livemode)
	switch mode := stripeKeyMode(key); {
	case mode == "":
		return paymentCondition(false, "ModeMismatch", "the key is not a Stripe secret or restricted key (sk_live_, rk_live_, sk_test_ or rk_test_)")
	case mode != want:
		return paymentCondition(false, "ModeMismatch", fmt.Sprintf("spec.stripe.livemode is %t but the key is a %s-mode key", spec.Livemode, mode))
	}

	api := stripeAPI{Base: r.StripeAPIBase, Key: key}
	account, err := stripeDo[stripeAccount](ctx, api, stripeCall{Method: http.MethodGet, Path: "/v1/account"})
	switch {
	case err == nil && account.ID == spec.AccountID:
		return paymentCondition(true, "Verified", fmt.Sprintf("the key is a %s-mode key of account %s", want, spec.AccountID))
	case err == nil:
		return paymentCondition(false, "AccountMismatch", fmt.Sprintf("the key belongs to account %s, not spec.stripe.accountId %s", account.ID, spec.AccountID))
	case stripeForbidden(err):
		return r.checkStripeMode(ctx, api, spec)
	}
	return paymentCondition(false, "StripeAPIError", stripeCheckFailure("read the account", err))
}

// checkStripeMode checks a restricted key that may not read the account: a page of webhook
// endpoints proves the key works, and a listed endpoint's livemode must agree with the spec. The
// account is not checked on this path.
func (r *PaymentProviderReconciler) checkStripeMode(ctx context.Context, api stripeAPI, spec *oramav1alpha1.StripePaymentProviderSpec) metav1.Condition {
	page, err := stripeDo[stripeList[stripeLivemode]](ctx, api,
		stripeCall{Method: http.MethodGet, Path: "/v1/webhook_endpoints", Query: url.Values{"limit": {"1"}}})
	if err != nil {
		return paymentCondition(false, "StripeAPIError",
			"the key may not read the account, and "+stripeCheckFailure("list webhook endpoints", err))
	}
	if len(page.Data) > 0 && page.Data[0].Livemode != spec.Livemode {
		return paymentCondition(false, "ModeMismatch", fmt.Sprintf(
			"spec.stripe.livemode is %t but the account's webhook endpoints are %s-mode", spec.Livemode, stripeModeName(page.Data[0].Livemode)))
	}
	return paymentCondition(true, "ModeVerified", fmt.Sprintf(
		"the key is a %s-mode key and may list webhook endpoints; it may not read the account, so spec.stripe.accountId is not checked",
		stripeModeName(spec.Livemode)))
}

func (r *PaymentProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.PaymentProvider{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		Named("payment-provider").
		Complete(r)
}

func (r *PaymentProviderReconciler) requestsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	var providers oramav1alpha1.PaymentProviderList
	if err := r.List(ctx, &providers, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range providers.Items {
		if s := providers.Items[i].Spec.Stripe; s != nil && s.APIKeySecretRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&providers.Items[i])})
		}
	}
	return requests
}
