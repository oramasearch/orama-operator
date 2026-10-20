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
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// stripeCatalogRecheck is how often a catalog that matches is compared with Stripe again.
const stripeCatalogRecheck = 6 * time.Hour

// stripeProduct is what the catalog reads of a Stripe product.
type stripeProduct struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Active      bool              `json:"active"`
	Metadata    map[string]string `json:"metadata"`
}

// stripePrice is what the catalog reads of a Stripe price.
type stripePrice struct {
	ID         string           `json:"id"`
	Product    stripeProductRef `json:"product"`
	Currency   string           `json:"currency"`
	UnitAmount *int64           `json:"unit_amount"`
	Recurring  *struct {
		Interval      string `json:"interval"`
		IntervalCount int64  `json:"interval_count"`
	} `json:"recurring"`
	LookupKey string            `json:"lookup_key"`
	Active    bool              `json:"active"`
	Metadata  map[string]string `json:"metadata"`
}

// stripeProductRef is a price's product ID. Stripe sends the ID, or the product itself when the
// request expanded it.
type stripeProductRef string

func (r *stripeProductRef) UnmarshalJSON(data []byte) error {
	var id string
	if json.Unmarshal(data, &id) == nil {
		*r = stripeProductRef(id)
		return nil
	}
	var product struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &product); err != nil {
		return err
	}
	*r = stripeProductRef(product.ID)
	return nil
}

// StripeCatalogReconciler keeps one catalog's products and prices in a Stripe account. It is the
// only code that writes Stripe products and prices.
//
// A product is the catalog's to write when its metadata[orama_app] is the catalog's app
// (ProductTaken otherwise) and its metadata[orama_catalog_version] is not higher than the catalog's
// version (Superseded otherwise: a newer operator's definition wins). A price holding one of the
// spec's lookup keys is the catalog's when it carries the catalog's app and belongs to one of its
// products (PriceTaken otherwise). Every product and price is read before anything is written, so a
// refused or superseded catalog changes nothing. Stripe prices cannot change, so a changed price is
// a new one that takes the lookup key over, and whatever the catalog no longer wants is archived,
// never deleted. Deleting the object changes nothing in Stripe: there is no finalizer, because
// products may have subscriptions and archiving them is a person's decision.
type StripeCatalogReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// StripeAPIBase is Stripe's API; empty is https://api.stripe.com. Tests point it at a fake.
	StripeAPIBase string
	// clock throttles the pass against Stripe per object.
	clock paymentCheckClock
}

// +kubebuilder:rbac:groups=orama.cloud,resources=stripecatalogs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=stripecatalogs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=paymentproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *StripeCatalogReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var catalog oramav1alpha1.StripeCatalog
	if err := r.Get(ctx, req.NamespacedName, &catalog); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !catalog.DeletionTimestamp.IsZero() {
		r.clock.forget(catalog.UID)
		return ctrl.Result{}, nil
	}
	status := *catalog.Status.DeepCopy()
	condition, after, err := r.configure(ctx, &catalog, &status)
	if condition.Reason != "" {
		condition.Type, condition.ObservedGeneration = paymentConditionReady, catalog.Generation
		if meta.SetStatusCondition(&status.Conditions, condition) {
			logf.FromContext(ctx).Info("stripe catalog readiness changed",
				"status", condition.Status, "reason", condition.Reason, "message", condition.Message)
		}
	}
	if serr := r.saveStatus(ctx, &catalog, status); serr != nil && err == nil {
		err = serr
	}
	return ctrl.Result{RequeueAfter: after}, err
}

// saveStatus writes status when it changed, as a merge patch so a spec change made meanwhile does
// not refuse it.
func (r *StripeCatalogReconciler) saveStatus(ctx context.Context, catalog *oramav1alpha1.StripeCatalog, status oramav1alpha1.StripeCatalogStatus) error {
	if equality.Semantic.DeepEqual(catalog.Status, status) {
		return nil
	}
	patched := catalog.DeepCopy()
	patched.Status = status
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(catalog)); err != nil {
		return err
	}
	catalog.Status = status
	return nil
}

// configure brings the account to the catalog and returns the Ready condition, without its type and
// generation, and when to look again. Between rechecks it is the condition the object already has,
// and Stripe is not called.
func (r *StripeCatalogReconciler) configure(ctx context.Context, catalog *oramav1alpha1.StripeCatalog, status *oramav1alpha1.StripeCatalogStatus) (metav1.Condition, time.Duration, error) {
	if problem := paymentCatalogProblem(catalog.Spec.PaymentCatalogSpec); problem != "" {
		r.clock.forget(catalog.UID)
		return paymentCondition(false, "InvalidSpec", problem), 0, nil
	}
	key, problem, _, err := paymentCredential(ctx, r.Client, catalog.Namespace, catalog.Spec.ProviderRef.Name, catalog.Spec.APIKeySecretRef)
	if err != nil {
		return metav1.Condition{}, 0, err
	}
	if problem != "" {
		r.clock.forget(catalog.UID)
		return paymentCondition(false, "ProviderNotReady", problem), paymentRetry, nil
	}

	spec, _ := json.Marshal(catalog.Spec)
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s", spec, key))
	digest := fmt.Sprintf("%x", sum[:])
	previous := meta.FindStatusCondition(catalog.Status.Conditions, paymentConditionReady)
	if wait := r.clock.wait(catalog.UID, digest, stripeCatalogRecheck); wait > 0 && previous != nil && previous.ObservedGeneration == catalog.Generation {
		return *previous, wait, nil
	}
	pass := stripeCatalogPass{catalog: catalog, status: status, api: stripeAPI{Base: r.StripeAPIBase, Key: key}}
	condition := pass.run(ctx)
	ok := condition.Status == metav1.ConditionTrue
	r.clock.record(catalog.UID, digest, ok)
	if !ok {
		return condition, paymentRetry, nil
	}
	return condition, stripeCatalogRecheck, nil
}

// paymentCatalogProblem is what the CRD does not refuse and no pass could keep: a lookup key under
// two products, which would move between them on every pass, or a metadata key starting with
// orama_, which would overwrite the operator's tags. (A CEL rule for the second would exceed the
// API server's cost budget inside the unbounded product and price lists.)
func paymentCatalogProblem(spec oramav1alpha1.PaymentCatalogSpec) string {
	reserved := func(metadata map[string]string) string {
		for _, key := range slices.Sorted(maps.Keys(metadata)) {
			if strings.HasPrefix(key, "orama_") {
				return key
			}
		}
		return ""
	}
	products := map[string]string{}
	for _, product := range spec.Products {
		if key := reserved(product.Metadata); key != "" {
			return fmt.Sprintf("product %s sets metadata key %q: keys starting with orama_ are the operator's", product.ID, key)
		}
		for _, price := range product.Prices {
			if key := reserved(price.Metadata); key != "" {
				return fmt.Sprintf("price %s sets metadata key %q: keys starting with orama_ are the operator's", price.LookupKey, key)
			}
			if other, taken := products[price.LookupKey]; taken && other != product.ID {
				return fmt.Sprintf("lookup key %q is under products %s and %s", price.LookupKey, other, product.ID)
			}
			products[price.LookupKey] = product.ID
		}
	}
	return ""
}

// stripeCatalogPass is one pass of a catalog against Stripe. It changes status as Stripe answers.
type stripeCatalogPass struct {
	catalog *oramav1alpha1.StripeCatalog
	status  *oramav1alpha1.StripeCatalogStatus
	api     stripeAPI
}

// stripeCatalogState is what the account holds of the catalog before a pass writes anything.
type stripeCatalogState struct {
	// products has every spec product's ID; the value is nil when Stripe has no such product.
	products map[string]*stripeProduct
	// holders has every spec lookup key; the value is nil when no price holds it.
	holders map[string]*stripePrice
	// removed are the active products this catalog created that the spec no longer lists.
	removed []stripeProduct
}

// owner is the metadata[orama_owner] tag: no cluster identity, so a rebuilt cluster finds its own.
func (p *stripeCatalogPass) owner() string {
	return p.catalog.Namespace + "/" + p.catalog.Name
}

// run reads the catalog's products and prices, stops when one is not the catalog's to write, then
// writes what differs and archives what the catalog no longer wants.
func (p *stripeCatalogPass) run(ctx context.Context) metav1.Condition {
	state, refusal, err := p.read(ctx)
	if err != nil {
		return paymentCondition(false, "StripeAPIError", stripeCheckFailure("read the catalog", err))
	}
	if refusal.Reason != "" {
		return refusal
	}
	prices, err := p.apply(ctx, state)
	if err != nil {
		// A request that got no answer is retried in a minute under the same key.
		return paymentCondition(false, "StripeAPIError", stripeCheckFailure("write the catalog", err))
	}
	p.status.Prices = prices
	return paymentCondition(true, "Configured", fmt.Sprintf("%d products and %d prices match catalog version %d",
		len(state.products), len(prices), p.catalog.Spec.Version))
}

// read finds every spec product, every product this catalog created, and the price holding each
// spec lookup key. refusal is set when one of them is not the catalog's to write.
func (p *stripeCatalogPass) read(ctx context.Context) (stripeCatalogState, metav1.Condition, error) {
	spec := p.catalog.Spec
	state := stripeCatalogState{products: map[string]*stripeProduct{}, holders: map[string]*stripePrice{}}
	for _, want := range spec.Products {
		product, err := stripeDo[stripeProduct](ctx, p.api, stripeCall{Method: http.MethodGet, Path: "/v1/products/" + url.PathEscape(want.ID)})
		var apiErr *stripeAPIError
		switch {
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
			state.products[want.ID] = nil
			continue
		case err != nil:
			return state, metav1.Condition{}, err
		}
		if refusal := p.refusal(product); refusal.Reason != "" {
			return state, refusal, nil
		}
		state.products[want.ID] = &product
	}

	owned, err := p.ownedProducts(ctx)
	if err != nil {
		return state, metav1.Condition{}, err
	}
	for _, product := range owned {
		if _, listed := state.products[product.ID]; listed || !product.Active || product.Metadata["orama_app"] != spec.App {
			continue
		}
		if refusal := p.refusal(product); refusal.Reason != "" {
			return state, refusal, nil
		}
		state.removed = append(state.removed, product)
	}

	for _, want := range spec.Products {
		for _, price := range want.Prices {
			query := url.Values{stripeField("lookup_keys", ""): {price.LookupKey}, stripeField("expand", ""): {"data.product"}}
			page, err := stripeDo[stripeList[stripePrice]](ctx, p.api, stripeCall{Method: http.MethodGet, Path: "/v1/prices", Query: query})
			if err != nil {
				return state, metav1.Condition{}, err
			}
			if len(page.Data) == 0 {
				state.holders[price.LookupKey] = nil
				continue
			}
			holder := page.Data[0]
			if _, ours := state.products[string(holder.Product)]; !ours || holder.Metadata["orama_app"] != spec.App {
				return state, paymentCondition(false, "PriceTaken", fmt.Sprintf(
					"Stripe price %s holds lookup key %q and is not this catalog's (product %s, metadata[orama_app]=%q); a person decides what to do with it",
					holder.ID, price.LookupKey, holder.Product, holder.Metadata["orama_app"])), nil
			}
			state.holders[price.LookupKey] = &holder
		}
	}
	return state, metav1.Condition{}, nil
}

// refusal is why the catalog may not write product, or an empty condition: another app's product,
// or one a newer catalog definition wrote.
func (p *stripeCatalogPass) refusal(product stripeProduct) metav1.Condition {
	spec := p.catalog.Spec
	if app := product.Metadata["orama_app"]; app != spec.App {
		return paymentCondition(false, "ProductTaken", fmt.Sprintf(
			"Stripe product %s carries metadata[orama_app]=%q, not %q; a person decides what to do with it", product.ID, app, spec.App))
	}
	if version := stripeCatalogVersion(product); version > spec.Version {
		return paymentCondition(true, "Superseded", fmt.Sprintf(
			"Stripe product %s carries catalog version %d, newer than this catalog's %d: the newer definition is kept and this catalog changes nothing",
			product.ID, version, spec.Version))
	}
	return metav1.Condition{}
}

// stripeCatalogVersion is a product's metadata[orama_catalog_version]; 0 when it has none.
func stripeCatalogVersion(product stripeProduct) int64 {
	version, _ := strconv.ParseInt(product.Metadata["orama_catalog_version"], 10, 64)
	return version
}

// ownedProducts are the products whose metadata[orama_owner] is this catalog, page by page.
func (p *stripeCatalogPass) ownedProducts(ctx context.Context) ([]stripeProduct, error) {
	var all []stripeProduct
	query := url.Values{"query": {fmt.Sprintf("metadata['orama_owner']:'%s'", p.owner())}, "limit": {"100"}}
	for {
		page, err := stripeDo[stripeSearchPage[stripeProduct]](ctx, p.api, stripeCall{Method: http.MethodGet, Path: "/v1/products/search", Query: query})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.HasMore || page.NextPage == "" {
			return all, nil
		}
		query.Set("page", page.NextPage)
	}
}

// apply brings every spec product and price to the spec, then archives what the catalog no longer
// wants, and returns each lookup key's price. Archiving comes last, so a pass that stops between a
// replacement and the archive of the price it replaced leaves that price for the next pass.
func (p *stripeCatalogPass) apply(ctx context.Context, state stripeCatalogState) (map[string]string, error) {
	prices := map[string]string{}
	for _, want := range p.catalog.Spec.Products {
		if err := p.writeProduct(ctx, want, state.products[want.ID]); err != nil {
			return nil, err
		}
		for _, price := range want.Prices {
			id, err := p.writePrice(ctx, want.ID, price, state.holders[price.LookupKey])
			if err != nil {
				return nil, err
			}
			prices[price.LookupKey] = id
		}
	}
	held := map[string]bool{}
	for _, id := range prices {
		held[id] = true
	}
	for _, want := range p.catalog.Spec.Products {
		if err := p.archiveStalePrices(ctx, want.ID, held); err != nil {
			return nil, err
		}
	}
	for _, product := range state.removed {
		if err := p.archiveProduct(ctx, product); err != nil {
			return nil, err
		}
	}
	return prices, nil
}

// productMetadata is the metadata a spec product carries: its own and the catalog's app and version.
// orama_owner is added on create only.
func (p *stripeCatalogPass) productMetadata(want oramav1alpha1.PaymentProduct) map[string]string {
	metadata := maps.Clone(want.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["orama_app"] = p.catalog.Spec.App
	metadata["orama_catalog_version"] = strconv.FormatInt(p.catalog.Spec.Version, 10)
	return metadata
}

// priceMetadata is the metadata a spec price carries: its own and the catalog's app. orama_owner is
// added on create only.
func (p *stripeCatalogPass) priceMetadata(want oramav1alpha1.PaymentPrice) map[string]string {
	metadata := maps.Clone(want.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["orama_app"] = p.catalog.Spec.App
	return metadata
}

// writeProduct creates the product, or sends only what differs: name, description, metadata, or
// active=true for an archived one.
func (p *stripeCatalogPass) writeProduct(ctx context.Context, want oramav1alpha1.PaymentProduct, existing *stripeProduct) error {
	form := url.Values{}
	if existing == nil {
		form.Set("id", want.ID)
		form.Set("name", want.Name)
		if want.Description != "" {
			form.Set("description", want.Description)
		}
		stripeMetadataForm(form, p.productMetadata(want))
		form.Set(stripeField("metadata", "orama_owner"), p.owner())
		if _, err := p.post(ctx, "create-product", want.ID, "/v1/products", form); err != nil {
			return err
		}
		logf.FromContext(ctx).Info("created Stripe product", "product", want.ID)
		return nil
	}
	if existing.Name != want.Name {
		form.Set("name", want.Name)
	}
	if existing.Description != want.Description {
		form.Set("description", want.Description)
	}
	if !existing.Active {
		form.Set("active", "true")
	}
	stripeMetadataDiff(form, existing.Metadata, p.productMetadata(want))
	if len(form) == 0 {
		return nil
	}
	if _, err := p.post(ctx, "update-product", want.ID, "/v1/products/"+url.PathEscape(want.ID), form); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("updated Stripe product", "product", want.ID, "fields", slices.Sorted(maps.Keys(form)))
	return nil
}

// writePrice makes the price holding want's lookup key match it and returns that price's ID. A
// holder that differs in product, currency, amount or interval cannot be changed: a new price takes
// the lookup key over, and archiveStalePrices archives the holder.
func (p *stripeCatalogPass) writePrice(ctx context.Context, productID string, want oramav1alpha1.PaymentPrice, holder *stripePrice) (string, error) {
	switch {
	case holder == nil:
		return p.createPrice(ctx, "create-price", want.LookupKey, productID, want, false)
	case !stripePriceMatches(*holder, productID, want):
		return p.createPrice(ctx, "replace-price", holder.ID, productID, want, true)
	}
	form := url.Values{}
	if !holder.Active {
		form.Set("active", "true")
	}
	stripeMetadataDiff(form, holder.Metadata, p.priceMetadata(want))
	if len(form) == 0 {
		return holder.ID, nil
	}
	if _, err := p.post(ctx, "update-price", holder.ID, "/v1/prices/"+url.PathEscape(holder.ID), form); err != nil {
		return "", err
	}
	logf.FromContext(ctx).Info("updated Stripe price", "price", holder.ID, "lookupKey", want.LookupKey, "fields", slices.Sorted(maps.Keys(form)))
	return holder.ID, nil
}

// createPrice creates want's price; transfer takes the lookup key from the price target names.
func (p *stripeCatalogPass) createPrice(ctx context.Context, operation, target, productID string, want oramav1alpha1.PaymentPrice, transfer bool) (string, error) {
	form := url.Values{}
	form.Set("product", productID)
	form.Set("currency", want.Currency)
	form.Set("unit_amount", strconv.FormatInt(want.UnitAmount, 10))
	if want.Interval != "" {
		form.Set(stripeField("recurring", "interval"), want.Interval)
	}
	form.Set("lookup_key", want.LookupKey)
	if transfer {
		form.Set("transfer_lookup_key", "true")
	}
	stripeMetadataForm(form, p.priceMetadata(want))
	form.Set(stripeField("metadata", "orama_owner"), p.owner())
	id, err := p.post(ctx, operation, target, "/v1/prices", form)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("stripe created the price for lookup key %q without an id", want.LookupKey)
	}
	log := logf.FromContext(ctx).WithValues("price", id, "lookupKey", want.LookupKey)
	if transfer {
		log = log.WithValues("replaces", target)
	}
	log.Info("created Stripe price")
	return id, nil
}

// stripePriceMatches is whether price is want under productID in what Stripe cannot change:
// product, currency, amount and interval.
func stripePriceMatches(price stripePrice, productID string, want oramav1alpha1.PaymentPrice) bool {
	if string(price.Product) != productID || price.Currency != want.Currency || price.UnitAmount == nil || *price.UnitAmount != want.UnitAmount {
		return false
	}
	if price.Recurring == nil {
		return want.Interval == ""
	}
	return price.Recurring.Interval == want.Interval && price.Recurring.IntervalCount == 1
}

// archiveStalePrices archives every active price of the product that carries the catalog's app and
// holds none of the spec's lookup keys: a price the spec removed, or one a changed price replaced,
// even when the pass that replaced it stopped before archiving it.
func (p *stripeCatalogPass) archiveStalePrices(ctx context.Context, productID string, held map[string]bool) error {
	listed, err := p.activePrices(ctx, productID)
	if err != nil {
		return err
	}
	for _, price := range listed {
		if held[price.ID] || price.Metadata["orama_app"] != p.catalog.Spec.App {
			continue
		}
		form := url.Values{"active": {"false"}}
		if _, err := p.post(ctx, "archive-price", price.ID, "/v1/prices/"+url.PathEscape(price.ID), form); err != nil {
			return err
		}
		logf.FromContext(ctx).Info("archived Stripe price", "price", price.ID, "product", productID)
	}
	return nil
}

// activePrices are the product's active prices, page by page.
func (p *stripeCatalogPass) activePrices(ctx context.Context, productID string) ([]stripePrice, error) {
	var all []stripePrice
	query := url.Values{"product": {productID}, "active": {"true"}, "limit": {"100"}}
	for {
		page, err := stripeDo[stripeList[stripePrice]](ctx, p.api, stripeCall{Method: http.MethodGet, Path: "/v1/prices", Query: query})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.HasMore || len(page.Data) == 0 {
			return all, nil
		}
		query.Set("starting_after", page.Data[len(page.Data)-1].ID)
	}
}

// archiveProduct archives a product this catalog created and no longer lists. It stamps the
// catalog's version, so an older catalog that still lists the product stands down (Superseded)
// instead of reactivating it.
func (p *stripeCatalogPass) archiveProduct(ctx context.Context, product stripeProduct) error {
	form := url.Values{}
	form.Set("active", "false")
	form.Set(stripeField("metadata", "orama_catalog_version"), strconv.FormatInt(p.catalog.Spec.Version, 10))
	if _, err := p.post(ctx, "archive-product", product.ID, "/v1/products/"+url.PathEscape(product.ID), form); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("archived Stripe product", "product", product.ID)
	return nil
}

// post sends a create or an update under an idempotency key naming exactly this request, and
// returns the ID Stripe answers with. Once Stripe answers, 2xx or not, the key is spent and
// status.writes moves on, so the next request never gets this one's saved answer (see
// stripeIdempotencyKey). Reconcile saves the status after the pass.
func (p *stripeCatalogPass) post(ctx context.Context, operation, target, path string, form url.Values) (string, error) {
	key := stripeIdempotencyKey(p.catalog.UID, operation, target, form, int(p.status.Writes))
	answer, err := stripeDo[struct {
		ID string `json:"id"`
	}](ctx, p.api, stripeCall{Method: http.MethodPost, Path: path, Form: form, IdempotencyKey: key})
	var apiErr *stripeAPIError
	if err == nil || errors.As(err, &apiErr) {
		p.status.Writes++
	}
	return answer.ID, err
}

// stripeMetadataForm sets each metadata key on a create; an empty value is left out.
func stripeMetadataForm(form url.Values, metadata map[string]string) {
	for key, value := range metadata {
		if value != "" {
			form.Set(stripeField("metadata", key), value)
		}
	}
}

// stripeMetadataDiff adds to form what turns have into want: each key whose value differs, and an
// empty value, which Stripe takes as "remove", for each key want does not have. orama_owner is never
// changed: it names the catalog that created the object.
func stripeMetadataDiff(form url.Values, have, want map[string]string) {
	for key, value := range want {
		if have[key] != value {
			form.Set(stripeField("metadata", key), value)
		}
	}
	for key := range have {
		if _, kept := want[key]; !kept && key != "orama_owner" {
			form.Set(stripeField("metadata", key), "")
		}
	}
}

func (r *StripeCatalogReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.StripeCatalog{}).
		Watches(&oramav1alpha1.PaymentProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("stripe-catalog").
		Complete(r)
}

func (r *StripeCatalogReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	var catalogs oramav1alpha1.StripeCatalogList
	if err := r.List(ctx, &catalogs, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range catalogs.Items {
		if catalogs.Items[i].Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&catalogs.Items[i])})
		}
	}
	return requests
}
