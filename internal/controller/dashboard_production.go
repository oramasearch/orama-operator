/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A production dashboard never runs on sandbox authentication defaults (operator#794, LSR-132).
//
// The dashboard is internet-facing, and some of its routes are authenticated only by a shared value:
// the cron routes by `Authorization: Bearer $CRON_SECRET`, the Stripe webhook by a signature made
// with STRIPE_WEBHOOK_SECRET. componentSecretData seeds `dev-*` placeholders for the Stripe pair into
// <cr>-dashboard-secret, ensureComponentSecret keeps whatever an existing Secret holds, and CRON_SECRET
// was checked only when the crons were enabled. A stack without its production values could therefore
// serve those routes with a value anyone can read in this repository, or with none.
//
// Whether a stack is production is decided by the set that created it, never by a field of its own:
// OramaCloudV1Set writes its member cluster's `purpose` tag as the instancePurposeLabel on every pass
// (managedset_controller.go memberLabels). On a production instance:
//
//   - every variable in dashboardProductionAuthEnv must come from the user's own sources, a
//     dashboard.envFrom Secret or a dashboard.env secretKeyRef, the same way dashboardCronSecret finds
//     CRON_SECRET; a literal, a ConfigMap or the operator's <cr>-dashboard-secret does not count;
//   - its value must be non-empty and must not be one of the operator's seeded placeholders;
//   - CRON_SECRET is required whether or not the crons are enabled;
//   - STRIPE_SECRET_KEY comes from spec.components.dashboard.stripeSecretKeySecretRef, and its value
//     is a live-mode restricted key (rk_live_), never the account's full secret key (sk_) nor a
//     test-mode key (#802, LSR-140): the dashboard holds only the Stripe permissions it uses;
//   - the operator seeds no placeholder for any of them into <cr>-dashboard-secret.
//
// Otherwise the dashboard step fails before the Deployment is written, naming every missing setting
// and never a value: a fresh stack gets no dashboard, a running one keeps its last template, and the
// stack reports Ready=False until the settings are there.

// productionPurpose (the purpose tag of a production cluster) is declared with the set copy's
// scope, managedset_secret_scope.go.

// isProduction reports whether oc was created by a set whose clusters are production.
func isProduction(oc *oramav1alpha1.OramaCloudV1) bool {
	return oc.Labels[instancePurposeLabel] == productionPurpose
}

// dashboardProductionAuthEnv is each variable a production dashboard must take from the user's own
// Secrets, with what it protects.
var dashboardProductionAuthEnv = []struct{ name, protects string }{
	{"CRON_SECRET", "the cron routes' bearer"},
	{"STRIPE_WEBHOOK_SECRET", "the Stripe webhook signature"},
}

// dashboardProductionUnseeded is every dashboard variable the operator never seeds a placeholder for
// on a production instance: dashboardProductionAuthEnv and the Stripe key.
func dashboardProductionUnseeded() []string {
	names := []string{"STRIPE_SECRET_KEY"}
	for _, v := range dashboardProductionAuthEnv {
		names = append(names, v.name)
	}
	return names
}

// stripeRestrictedLivePrefix begins every live-mode Stripe restricted key. A full secret key begins
// sk_live_, a test-mode one sk_test_ or rk_test_.
const stripeRestrictedLivePrefix = "rk_live_"

// secretComponents is every component componentSecretData seeds a Secret for.
var secretComponents = []string{
	"engine", "search", "cache", "broker", "analytics", "analytics-processor", "disco", "keycloak",
	"pgadmin", "index-builder", "harvest", "harvest-master", "dashboard", "workers",
}

// seededPlaceholder reports whether value is one the operator seeds into a component Secret.
func seededPlaceholder(value string) bool {
	for _, c := range secretComponents {
		for _, v := range componentSecretData(c) {
			if v == value {
				return true
			}
		}
	}
	return false
}

// dashboardUserSecretValue finds where the dashboard's own name comes from among the user's sources
// and returns its value. found is false when neither dashboard.env nor a dashboard.envFrom source
// provides it. A winner that is not a Secret (a literal, a ConfigMap, a fieldRef) is reported in
// problem. sources are dashboardEnvSources' result.
func (r *OramaCloudV1Reconciler) dashboardUserSecretValue(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource, name string) (value string, found bool, problem string, err error) {
	for _, e := range overrideEnv(nil, oc.Spec.Components.Dashboard.Env) {
		if e.Name != name {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			return "", true, "dashboard.env sets it, but not from a Secret key", nil
		}
		ref := e.ValueFrom.SecretKeyRef
		data, _, err := r.readEnvObject(ctx, oc.Namespace, "Secret", ref.Name)
		if err != nil {
			return "", false, "", fmt.Errorf("dashboard.env %s: %w", name, err)
		}
		v, has := data[ref.Key]
		if !has {
			return "", false, "", nil
		}
		return v, true, "", nil
	}
	for i := len(sources) - 1; i >= 0; i-- {
		src := sources[i]
		key, ok := strings.CutPrefix(name, src.Prefix)
		if !ok {
			continue
		}
		v, has := src.Data[key]
		if !has {
			continue
		}
		if src.Kind != "Secret" {
			return "", true, fmt.Sprintf("dashboard.envFrom[%d] gives it from %s %s, not a Secret", i, src.Kind, src.Name), nil
		}
		return v, true, "", nil
	}
	return "", false, "", nil
}

// checkDashboardProductionAuth fails unless every variable in dashboardProductionAuthEnv comes from
// the user's own Secrets with a value that is neither empty nor a seeded placeholder. It does nothing
// on an instance that is not production. The error names every problem and no value.
func (r *OramaCloudV1Reconciler) checkDashboardProductionAuth(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource) error {
	if !isProduction(oc) {
		return nil
	}
	var problems []string
	for _, v := range dashboardProductionAuthEnv {
		value, found, problem, err := r.dashboardUserSecretValue(ctx, oc, sources, v.name)
		switch {
		case err != nil:
			return err
		case problem != "":
			problems = append(problems, fmt.Sprintf("%s (%s): %s", v.name, v.protects, problem))
		case !found:
			problems = append(problems, fmt.Sprintf("%s (%s) is not set by a dashboard.envFrom Secret or a dashboard.env secretKeyRef", v.name, v.protects))
		case value == "":
			problems = append(problems, fmt.Sprintf("%s (%s) is empty", v.name, v.protects))
		case seededPlaceholder(value):
			problems = append(problems, fmt.Sprintf("%s (%s) is an operator placeholder, not a production value", v.name, v.protects))
		}
	}
	problem, err := r.dashboardStripeKeyProblem(ctx, oc)
	if err != nil {
		return err
	}
	if problem != "" {
		problems = append(problems, problem)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the instance is production (label %s=%s, written by its set), and the dashboard is not rendered without its production authentication settings: %s",
		instancePurposeLabel, productionPurpose, strings.Join(problems, "; "))
}

// dashboardStripeKeyProblem is what is wrong with a production dashboard's Stripe key, or "". It
// names the field and never the value. checkKeyRefs has already refused a missing Secret, key or value.
func (r *OramaCloudV1Reconciler) dashboardStripeKeyProblem(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (string, error) {
	ref := oc.Spec.Components.Dashboard.StripeSecretKeySecretRef
	if ref == nil {
		return "STRIPE_SECRET_KEY (the Stripe API key) is not set by spec.components.dashboard.stripeSecretKeySecretRef: a production dashboard holds a restricted live key (" + stripeRestrictedLivePrefix + ") with only the permissions it uses, never the account's full secret key", nil
	}
	sec := &corev1.Secret{}
	if err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: ref.Name}, sec); err != nil {
		return "", fmt.Errorf("STRIPE_SECRET_KEY: read Secret %s/%s: %w", oc.Namespace, ref.Name, err)
	}
	if !strings.HasPrefix(string(sec.Data[ref.Key]), stripeRestrictedLivePrefix) {
		return fmt.Sprintf("STRIPE_SECRET_KEY (the Stripe API key) in Secret %s/%s key %s is not a live-mode restricted key (%s): a full secret key or a test-mode key is refused", oc.Namespace, ref.Name, ref.Key, stripeRestrictedLivePrefix), nil
	}
	return "", nil
}

// dashboardSeedsAuthPlaceholders reports whether ensureComponentSecret may seed the placeholders for
// dashboardProductionUnseeded into <cr>-dashboard-secret: never on a production instance.
func dashboardSeedsAuthPlaceholders(oc *oramav1alpha1.OramaCloudV1) bool {
	return !isProduction(oc)
}
