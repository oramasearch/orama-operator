/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	paymentConditionReady = "Ready"
	// paymentWebhookFinalizer deletes the vendor's endpoint before the object goes.
	paymentWebhookFinalizer = "orama.cloud/payment-webhook-cleanup"
	// paymentWebhookSigningSecretKey is the key of the signing Secret.
	paymentWebhookSigningSecretKey = "signingSecret"
)

// stripeWebhookEndpoint is what the controller reads of a Stripe webhook endpoint. Secret is only in
// the answer to a create: Stripe never returns it again.
type stripeWebhookEndpoint struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	EnabledEvents []string          `json:"enabled_events"`
	APIVersion    string            `json:"api_version"`
	Status        string            `json:"status"`
	Metadata      map[string]string `json:"metadata"`
	Secret        string            `json:"secret"`
}

// StripeWebhookEndpointReconciler keeps one Stripe webhook endpoint per object and writes its
// signing secret to the Secret the spec names. It is the only code that calls Stripe's webhook API.
//
// An endpoint is ours when it carries metadata[orama_managed]=true and
// metadata[orama_owner]=<namespace>/<name>, and has the spec's URL (or is the endpoint the status
// names, whose URL the spec changed). The owner tag leaves out the cluster, so a cluster rebuilt
// from the committed resources recognises its endpoints. An endpoint at the spec's URL that is not
// ours is never adopted, changed or deleted (URLTaken). The signing secret can be read only from the
// answer to the create, so an endpoint whose Secret is lost, or whose apiVersion changes (Stripe
// cannot change it), is deleted and created again.
type StripeWebhookEndpointReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// StripeAPIBase is Stripe's API; empty is https://api.stripe.com. Tests point it at a fake.
	StripeAPIBase string
	// clock throttles the pass against Stripe per object.
	clock paymentCheckClock
}

// +kubebuilder:rbac:groups=orama.cloud,resources=stripewebhookendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=stripewebhookendpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=stripewebhookendpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=paymentproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *StripeWebhookEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var endpoint oramav1alpha1.StripeWebhookEndpoint
	if err := r.Get(ctx, req.NamespacedName, &endpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !endpoint.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &endpoint)
	}
	if controllerutil.AddFinalizer(&endpoint, paymentWebhookFinalizer) {
		if err := r.Update(ctx, &endpoint); err != nil {
			return ctrl.Result{}, err
		}
	}
	status := *endpoint.Status.DeepCopy()
	condition, after, err := r.configure(ctx, &endpoint, &status)
	if condition.Reason != "" {
		condition.Type, condition.ObservedGeneration = paymentConditionReady, endpoint.Generation
		if meta.SetStatusCondition(&status.Conditions, condition) {
			logf.FromContext(ctx).Info("stripe webhook endpoint readiness changed",
				"status", condition.Status, "reason", condition.Reason, "message", condition.Message)
		}
	}
	if serr := r.saveStatus(ctx, &endpoint, status); serr != nil && err == nil {
		err = serr
	}
	return ctrl.Result{RequeueAfter: after}, err
}

// saveStatus writes status when it changed, as a merge patch so a spec change made meanwhile does
// not refuse it; endpoint then carries it.
func (r *StripeWebhookEndpointReconciler) saveStatus(ctx context.Context, endpoint *oramav1alpha1.StripeWebhookEndpoint, status oramav1alpha1.StripeWebhookEndpointStatus) error {
	if equality.Semantic.DeepEqual(endpoint.Status, status) {
		return nil
	}
	patched := endpoint.DeepCopy()
	patched.Status = status
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(endpoint)); err != nil {
		return err
	}
	endpoint.Status = status
	return nil
}

// credential is the endpoint's key once its provider's Credentials are True (paymentCredential).
func (r *StripeWebhookEndpointReconciler) credential(ctx context.Context, endpoint *oramav1alpha1.StripeWebhookEndpoint) (key, problem string, gone bool, err error) {
	return paymentCredential(ctx, r.Client, endpoint.Namespace, endpoint.Spec.ProviderRef.Name, endpoint.Spec.APIKeySecretRef)
}

// configure brings the Stripe endpoint to the spec and returns the Ready condition, without its
// type and generation, and when to look again. Between rechecks it is the condition the object
// already has, and Stripe is not called.
func (r *StripeWebhookEndpointReconciler) configure(ctx context.Context, endpoint *oramav1alpha1.StripeWebhookEndpoint, status *oramav1alpha1.StripeWebhookEndpointStatus) (metav1.Condition, time.Duration, error) {
	key, problem, _, err := r.credential(ctx, endpoint)
	if err != nil {
		return metav1.Condition{}, 0, err
	}
	if problem != "" {
		r.clock.forget(endpoint.UID)
		return paymentCondition(false, "ProviderNotReady", problem), paymentRetry, nil
	}
	held, problem, err := r.signingSecret(ctx, endpoint)
	if err != nil {
		return metav1.Condition{}, 0, err
	}
	if problem != "" {
		r.clock.forget(endpoint.UID)
		return paymentCondition(false, "SigningSecretTaken", problem), paymentRetry, nil
	}

	digest := stripeWebhookDigest(endpoint, key, held)
	previous := meta.FindStatusCondition(endpoint.Status.Conditions, paymentConditionReady)
	if wait := r.clock.wait(endpoint.UID, digest, paymentRecheck); wait > 0 && previous != nil && previous.ObservedGeneration == endpoint.Generation {
		return *previous, wait, nil
	}
	pass := stripeWebhookPass{r: r, endpoint: endpoint, status: status, api: stripeAPI{Base: r.StripeAPIBase, Key: key}}
	condition, err := pass.run(ctx, held)
	if err != nil {
		return metav1.Condition{}, 0, err
	}
	ok := condition.Status == metav1.ConditionTrue
	// A pass that succeeded leaves the signing secret held.
	r.clock.record(endpoint.UID, stripeWebhookDigest(endpoint, key, held || ok), ok)
	if !ok {
		return condition, paymentRetry, nil
	}
	return condition, paymentRecheck, nil
}

// stripeWebhookDigest is what a pass against Stripe is for: a changed spec or key, or a lost
// signing secret, makes the next pass due at once.
func stripeWebhookDigest(endpoint *oramav1alpha1.StripeWebhookEndpoint, key string, held bool) string {
	spec, _ := json.Marshal(endpoint.Spec)
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%t", spec, key, held))
	return fmt.Sprintf("%x", sum[:])
}

// signingSecret is whether this object's Secret holds a signing secret. problem is set when the
// Secret belongs to another controller, so the operator never writes it.
func (r *StripeWebhookEndpointReconciler) signingSecret(ctx context.Context, endpoint *oramav1alpha1.StripeWebhookEndpoint) (held bool, problem string, err error) {
	var secret corev1.Secret
	name := endpoint.Spec.SigningSecretName
	if err := r.Get(ctx, client.ObjectKey{Namespace: endpoint.Namespace, Name: name}, &secret); err != nil {
		return false, "", client.IgnoreNotFound(err)
	}
	if owner := metav1.GetControllerOf(&secret); owner != nil && owner.UID != endpoint.UID {
		return false, fmt.Sprintf("Secret %q belongs to %s %s", name, owner.Kind, owner.Name), nil
	}
	return metav1.IsControlledBy(&secret, endpoint) && len(secret.Data[paymentWebhookSigningSecretKey]) > 0, "", nil
}

// stripeWebhookPass is one pass of an object against Stripe. It changes status as Stripe answers.
type stripeWebhookPass struct {
	r        *StripeWebhookEndpointReconciler
	endpoint *oramav1alpha1.StripeWebhookEndpoint
	status   *oramav1alpha1.StripeWebhookEndpointStatus
	api      stripeAPI
}

// owner is the metadata[orama_owner] tag: no cluster identity, so a rebuilt cluster finds its own.
func (p *stripeWebhookPass) owner() string {
	return p.endpoint.Namespace + "/" + p.endpoint.Name
}

// events are the spec's event types, sorted and deduplicated, as Stripe is sent them.
func (p *stripeWebhookPass) events() []string {
	return slices.Compact(slices.Sorted(slices.Values(p.endpoint.Spec.Events)))
}

// run finds the object's endpoint and creates, repairs or replaces it. held is whether the signing
// Secret holds the endpoint's secret.
func (p *stripeWebhookPass) run(ctx context.Context, held bool) (metav1.Condition, error) {
	spec := p.endpoint.Spec
	listed, err := p.list(ctx)
	if err != nil {
		return paymentCondition(false, "StripeAPIError", stripeCheckFailure("list webhook endpoints", err)), nil
	}
	ours, taken := stripeWebhookMatch(listed, p.owner(), spec.URL, p.status.EndpointID)
	if taken != nil {
		return paymentCondition(false, "URLTaken", fmt.Sprintf(
			"Stripe webhook endpoint %s already delivers to %s and is not this object's; a person decides what to do with it",
			taken.ID, spec.URL)), nil
	}

	var created *stripeWebhookEndpoint
	switch {
	case ours == nil:
		created, err = p.create(ctx, "new")
	case !held || ours.APIVersion != spec.APIVersion:
		created, err = p.replace(ctx, ours, held)
	default:
		err = p.repair(ctx, ours)
	}
	if err != nil {
		// A request that got no answer is retried in a minute under the same key.
		return paymentCondition(false, "StripeAPIError", stripeCheckFailure("configure the webhook endpoint", err)), nil
	}
	if created != nil {
		if err := p.writeSigningSecret(ctx, created); err != nil {
			return metav1.Condition{}, err
		}
	}
	return paymentCondition(true, "Configured", fmt.Sprintf("Stripe webhook endpoint %s delivers %d event types to %s",
		p.status.EndpointID, len(p.events()), spec.URL)), nil
}

// list is every webhook endpoint of the account, page by page.
func (p *stripeWebhookPass) list(ctx context.Context) ([]stripeWebhookEndpoint, error) {
	var all []stripeWebhookEndpoint
	query := url.Values{"limit": {"100"}}
	for {
		page, err := stripeDo[stripeList[stripeWebhookEndpoint]](ctx, p.api, stripeCall{
			Method: http.MethodGet, Path: "/v1/webhook_endpoints", Query: query, Version: p.endpoint.Spec.APIVersion,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.HasMore || len(page.Data) == 0 {
			return all, nil
		}
		query = url.Values{"limit": {"100"}, "starting_after": {page.Data[len(page.Data)-1].ID}}
	}
}

// stripeWebhookMatch finds ours among the listed endpoints: owner-tagged and at url, or else
// owner-tagged with knownID (the spec's URL changed). taken is an endpoint at url that is not ours.
func stripeWebhookMatch(listed []stripeWebhookEndpoint, owner, url, knownID string) (ours, taken *stripeWebhookEndpoint) {
	var byID *stripeWebhookEndpoint
	for i := range listed {
		e := &listed[i]
		tagged := e.Metadata["orama_managed"] == "true" && e.Metadata["orama_owner"] == owner
		switch {
		case e.URL == url && tagged && ours == nil:
			ours = e
		case e.URL == url && !tagged && taken == nil:
			taken = e
		case tagged && knownID != "" && e.ID == knownID:
			byID = e
		}
	}
	if ours == nil {
		ours = byID
	}
	return ours, taken
}

// post sends a create or an update under an idempotency key naming exactly this request. Once
// Stripe answers, 2xx or not, the key is spent and status.writes moves on, so the next request
// never gets this one's saved answer. A pass sends at most one post, and Reconcile saves the status
// after it, the signing secret first.
func (p *stripeWebhookPass) post(ctx context.Context, operation, target, path string, form url.Values) (stripeWebhookEndpoint, error) {
	key := stripeIdempotencyKey(p.endpoint.UID, operation, target, form, int(p.status.Writes))
	answer, err := stripeDo[stripeWebhookEndpoint](ctx, p.api, stripeCall{
		Method: http.MethodPost, Path: path, Form: form, Version: p.endpoint.Spec.APIVersion, IdempotencyKey: key,
	})
	var apiErr *stripeAPIError
	if err == nil || errors.As(err, &apiErr) {
		p.status.Writes++
	}
	return answer, err
}

// create makes the endpoint; Stripe's answer carries its signing secret, the only time Stripe
// returns it, for run to write at once. target is "new", or "recreate-<the endpoint it replaces>".
func (p *stripeWebhookPass) create(ctx context.Context, target string) (*stripeWebhookEndpoint, error) {
	spec := p.endpoint.Spec
	form := url.Values{}
	form.Set("url", spec.URL)
	form[stripeField("enabled_events", "")] = p.events()
	form.Set("api_version", spec.APIVersion)
	form.Set("description", fmt.Sprintf("orama-operator: %s %s", spec.App, p.owner()))
	form.Set(stripeField("metadata", "orama_managed"), "true")
	form.Set(stripeField("metadata", "orama_owner"), p.owner())
	form.Set(stripeField("metadata", "orama_app"), spec.App)
	created, err := p.post(ctx, "create", target, "/v1/webhook_endpoints", form)
	if err != nil {
		return nil, err
	}
	if created.ID == "" || created.Secret == "" {
		return nil, fmt.Errorf("stripe created webhook endpoint %q without a signing secret", created.ID)
	}
	p.status.EndpointID = created.ID
	logf.FromContext(ctx).Info("created Stripe webhook endpoint", "endpoint", created.ID, "url", spec.URL)
	return &created, nil
}

// writeSigningSecret writes a created endpoint's signing secret to the object's Secret, which it
// owns.
func (p *stripeWebhookPass) writeSigningSecret(ctx context.Context, created *stripeWebhookEndpoint) error {
	name := p.endpoint.Spec.SigningSecretName
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.endpoint.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, p.r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[paymentWebhookSigningSecretKey] = []byte(created.Secret)
		return controllerutil.SetControllerReference(p.endpoint, secret, p.r.Scheme)
	}); err != nil {
		return fmt.Errorf("writing the signing secret of Stripe webhook endpoint %s to Secret %q: %w", created.ID, name, err)
	}
	return nil
}

// replace deletes ours and creates it again: its signing secret is lost, or its apiVersion changed,
// and Stripe can neither return the one nor change the other.
func (p *stripeWebhookPass) replace(ctx context.Context, ours *stripeWebhookEndpoint, held bool) (*stripeWebhookEndpoint, error) {
	if !held {
		p.r.event(p.endpoint, corev1.EventTypeWarning, "SigningSecretLost",
			"Secret %q has no signing secret for Stripe webhook endpoint %s, which Stripe cannot return again: deleting the endpoint to create it again",
			p.endpoint.Spec.SigningSecretName, ours.ID)
	} else {
		p.r.event(p.endpoint, corev1.EventTypeNormal, "APIVersionChanged",
			"Stripe webhook endpoint %s has API version %q, not %q, which Stripe cannot change: deleting the endpoint to create it again",
			ours.ID, ours.APIVersion, p.endpoint.Spec.APIVersion)
	}
	if err := stripeDeleteWebhookEndpoint(ctx, p.api, p.endpoint.Spec.APIVersion, ours.ID); err != nil {
		return nil, err
	}
	return p.create(ctx, "recreate-"+ours.ID)
}

// repair sends only what differs from the spec: the URL, the sorted events, or a disabled endpoint.
func (p *stripeWebhookPass) repair(ctx context.Context, ours *stripeWebhookEndpoint) error {
	p.status.EndpointID = ours.ID
	form := url.Values{}
	if ours.URL != p.endpoint.Spec.URL {
		form.Set("url", p.endpoint.Spec.URL)
	}
	if events := p.events(); !slices.Equal(slices.Compact(slices.Sorted(slices.Values(ours.EnabledEvents))), events) {
		form[stripeField("enabled_events", "")] = events
	}
	if ours.Status != "enabled" {
		form.Set("disabled", "false")
	}
	if len(form) == 0 {
		return nil
	}
	if _, err := p.post(ctx, "update", ours.ID, "/v1/webhook_endpoints/"+url.PathEscape(ours.ID), form); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("repaired Stripe webhook endpoint", "endpoint", ours.ID, "fields", slices.Sorted(maps.Keys(form)))
	return nil
}

// stripeDeleteWebhookEndpoint deletes one endpoint; one already gone counts as deleted.
func stripeDeleteWebhookEndpoint(ctx context.Context, api stripeAPI, version, id string) error {
	_, err := stripeDo[stripeWebhookEndpoint](ctx, api, stripeCall{
		Method: http.MethodDelete, Path: "/v1/webhook_endpoints/" + url.PathEscape(id), Version: version,
	})
	var apiErr *stripeAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

// finalize deletes ours in Stripe, only while it carries the owner tag, then releases the object.
// A working key still looks for our endpoint when its ID was not saved. Cleanup failures retry for
// paymentDeleteGrace only with a saved ID, unless Stripe refuses the key or credentials are gone.
// A Warning event names any saved endpoint left behind for a person to delete in the Dashboard.
func (r *StripeWebhookEndpointReconciler) finalize(ctx context.Context, endpoint *oramav1alpha1.StripeWebhookEndpoint) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(endpoint, paymentWebhookFinalizer) {
		return ctrl.Result{}, nil
	}
	key, problem, gone, err := r.credential(ctx, endpoint)
	withinGrace := r.clock.time().Sub(endpoint.DeletionTimestamp.Time) < paymentDeleteGrace
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case gone:
	case problem != "":
		problem = "deleting, waiting for the key: " + problem
		if endpoint.Status.EndpointID != "" && withinGrace {
			status := *endpoint.Status.DeepCopy()
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type: paymentConditionReady, Status: metav1.ConditionFalse, Reason: "ProviderNotReady",
				Message: problem, ObservedGeneration: endpoint.Generation,
			})
			return ctrl.Result{RequeueAfter: paymentRetry}, r.saveStatus(ctx, endpoint, status)
		}
	default:
		pass := stripeWebhookPass{r: r, endpoint: endpoint, status: &endpoint.Status, api: stripeAPI{Base: r.StripeAPIBase, Key: key}}
		listed, err := pass.list(ctx)
		if err == nil {
			if ours, _ := stripeWebhookMatch(listed, pass.owner(), endpoint.Spec.URL, endpoint.Status.EndpointID); ours != nil {
				err = stripeDeleteWebhookEndpoint(ctx, pass.api, endpoint.Spec.APIVersion, ours.ID)
				if err == nil {
					logf.FromContext(ctx).Info("deleted Stripe webhook endpoint", "endpoint", ours.ID)
				}
			}
		}
		if err != nil {
			logf.FromContext(ctx).Info("could not delete Stripe webhook endpoint", "endpoint", endpoint.Status.EndpointID, "error", err.Error())
			var apiErr *stripeAPIError
			refused := errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
			if endpoint.Status.EndpointID != "" && withinGrace && !refused {
				return ctrl.Result{RequeueAfter: paymentRetry}, nil
			}
			problem = err.Error()
		}
	}
	if problem != "" && endpoint.Status.EndpointID != "" {
		r.event(endpoint, corev1.EventTypeWarning, "EndpointLeftBehind",
			"%s, so Stripe webhook endpoint %s was not deleted: delete it in the Stripe Dashboard", problem, endpoint.Status.EndpointID)
	}
	r.clock.forget(endpoint.UID)
	controllerutil.RemoveFinalizer(endpoint, paymentWebhookFinalizer)
	return ctrl.Result{}, r.Update(ctx, endpoint)
}

func (r *StripeWebhookEndpointReconciler) event(object runtime.Object, eventType, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, eventType, reason, format, args...)
	}
}

func (r *StripeWebhookEndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.StripeWebhookEndpoint{}).
		Owns(&corev1.Secret{}).
		Watches(&oramav1alpha1.PaymentProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("stripe-webhook-endpoint").
		Complete(r)
}

func (r *StripeWebhookEndpointReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	var endpoints oramav1alpha1.StripeWebhookEndpointList
	if err := r.List(ctx, &endpoints, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range endpoints.Items {
		if endpoints.Items[i].Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&endpoints.Items[i])})
		}
	}
	return requests
}
