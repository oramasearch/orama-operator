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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// productionAuth is a complete set of production authentication values, none of them a placeholder.
var productionAuth = map[string]string{
	"CRON_SECRET":           "prod-cron-0001",
	"STRIPE_WEBHOOK_SECRET": "whsec_prod0001",
	"STRIPE_SECRET_KEY":     "rk_live_prod0001",
}

// withAuth is productionAuth with changes; an empty value deletes the key.
func withAuth(changes map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range productionAuth {
		out[k] = v
	}
	for k, v := range changes {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

// labelProduction marks oc as its set marks a member on a production cluster.
func labelProduction(o *oramav1alpha1.OramaCloudV1) {
	if o.Labels == nil {
		o.Labels = map[string]string{}
	}
	o.Labels[instancePurposeLabel] = productionPurpose
}

// prodDashOC is dashOC on a production instance.
func prodDashOC(mut func(*oramav1alpha1.DashboardSpec)) *oramav1alpha1.OramaCloudV1 {
	oc := dashOC(mut)
	labelProduction(oc)
	return oc
}

// LSR-132: a production dashboard whose authentication settings are sandbox defaults, missing, or
// not from the user's Secrets is never rendered. A fresh stack gets no Deployment, a running one keeps
// its last template, the stack is Ready=False naming each setting, and no value is printed.
func TestProductionDashboardIsNotRenderedWithSandboxAuthentication(t *testing.T) {
	type tc struct {
		dash    func(*oramav1alpha1.DashboardSpec)
		objects []client.Object
		want    []string
	}
	envFrom := func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("dashboard-env")}
	}
	cases := map[string]tc{
		// The committed v1-us member: a public dashboard with no dashboard env at all, so the
		// operator's seeded placeholders and no CRON_SECRET.
		"no dashboard env (the v1-us member)": {
			want: []string{
				"CRON_SECRET (the cron routes' bearer) is not set by a dashboard.envFrom Secret or a dashboard.env secretKeyRef",
				"STRIPE_WEBHOOK_SECRET (the Stripe webhook signature) is not set",
				"STRIPE_SECRET_KEY (the Stripe API key) is not set",
			}},
		"seeded placeholders copied into the user's Secret": {dash: envFrom,
			objects: []client.Object{secretOf("dashboard-env", withAuth(map[string]string{
				"STRIPE_WEBHOOK_SECRET": "dev-stripe-webhook-secret", "STRIPE_SECRET_KEY": "dev-stripe-secret-key"}))},
			want: []string{
				"STRIPE_WEBHOOK_SECRET (the Stripe webhook signature) is an operator placeholder",
				"STRIPE_SECRET_KEY (the Stripe API key) is an operator placeholder",
			}},
		"another component's placeholder": {dash: envFrom,
			objects: []client.Object{secretOf("dashboard-env", withAuth(map[string]string{"CRON_SECRET": "dev-harvest-signature"}))},
			want:    []string{"CRON_SECRET (the cron routes' bearer) is an operator placeholder"}},
		// The crons are off, and CRON_SECRET is still required: the routes are served either way.
		"no CRON_SECRET with the crons disabled": {dash: envFrom,
			objects: []client.Object{secretOf("dashboard-env", withAuth(map[string]string{"CRON_SECRET": ""}))},
			want:    []string{"CRON_SECRET (the cron routes' bearer) is not set"}},
		"empty CRON_SECRET": {dash: envFrom,
			objects: []client.Object{secretOf("dashboard-env", map[string]string{"CRON_SECRET": "",
				"STRIPE_WEBHOOK_SECRET": productionAuth["STRIPE_WEBHOOK_SECRET"], "STRIPE_SECRET_KEY": productionAuth["STRIPE_SECRET_KEY"]})},
			want: []string{"CRON_SECRET (the cron routes' bearer) is empty"}},
		"a literal in dashboard.env": {dash: func(d *oramav1alpha1.DashboardSpec) {
			envFrom(d)
			d.Env = []corev1.EnvVar{{Name: "STRIPE_WEBHOOK_SECRET", Value: "whsec_literal"}}
		}, objects: []client.Object{secretOf("dashboard-env", productionAuth)},
			want: []string{"STRIPE_WEBHOOK_SECRET (the Stripe webhook signature): dashboard.env sets it, but not from a Secret key"}},
		"a ConfigMap source": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{secretSource("dashboard-env"), configMapSource("plain")}
		}, objects: []client.Object{secretOf("dashboard-env", productionAuth), configMapOf("plain", map[string]string{"CRON_SECRET": "x"})},
			want: []string{"CRON_SECRET (the cron routes' bearer): dashboard.envFrom[1] gives it from ConfigMap plain, not a Secret"}},
		// A value written into the operator's own component Secret by hand does not count: it is
		// where the placeholders live, and ensureComponentSecret keeps whatever is there.
		"values only in the operator's component Secret": {
			objects: []client.Object{secretOf("v1-dashboard-secret", productionAuth)},
			want:    []string{"CRON_SECRET (the cron routes' bearer) is not set", "STRIPE_SECRET_KEY (the Stripe API key) is not set"}},
		"optional selector to a missing key": {dash: func(d *oramav1alpha1.DashboardSpec) {
			envFrom(d)
			sel := secretKeyEnv("CRON_SECRET", "cron", "token")
			sel.ValueFrom.SecretKeyRef.Optional = optional()
			d.Env = []corev1.EnvVar{sel}
		}, objects: []client.Object{secretOf("dashboard-env", productionAuth), secretOf("cron", nil)},
			want: []string{"CRON_SECRET (the cron routes' bearer) is not set"}},
	}
	for name, c := range cases {
		for _, existing := range []bool{false, true} {
			label := name + " on a fresh stack"
			if existing {
				label = name + " on a running stack"
			}
			t.Run(label, func(t *testing.T) {
				// A running stack: rendered before it was marked production, as v1-us is today.
				objs := append([]client.Object{dashOC(nil)}, c.objects...)
				h := newEngineHarness(t, objs...)
				var dep *appsv1.Deployment
				if existing {
					h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Env = nil })
					h.mustRun()
					dep = h.dashboard()
					if dep == nil {
						t.Fatal("the nonprod dashboard was not rendered")
					}
				}
				h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
					labelProduction(o)
					if c.dash != nil {
						c.dash(&o.Spec.Components.Dashboard)
					}
				})
				err := h.run()
				if err == nil {
					t.Fatal("Reconcile rendered a production dashboard without its production authentication settings")
				}
				ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
				if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReconcileError" || !strings.HasPrefix(ready.Message, "dashboard: ") {
					t.Fatalf("Ready = %+v, want False/ReconcileError from the dashboard step", ready)
				}
				for _, want := range c.want {
					if !strings.Contains(ready.Message, want) {
						t.Errorf("Ready message %q does not contain %q", ready.Message, want)
					}
				}
				for _, v := range []string{"prod-cron-0001", "whsec_prod0001", "rk_live_prod0001", "dev-stripe", "whsec_literal"} {
					if strings.Contains(ready.Message, v) || strings.Contains(err.Error(), v) {
						t.Errorf("the error holds the value %q", v)
					}
				}
				got := h.dashboard()
				switch {
				case !existing && got != nil:
					t.Error("a refused production reconcile created the dashboard Deployment")
				case existing && got.ResourceVersion != dep.ResourceVersion:
					t.Error("a refused production reconcile changed the dashboard Deployment")
				}
			})
		}
	}
}

// With every setting from the user's own Secrets, a production dashboard renders, and the operator
// seeds none of the placeholders into its component Secret.
func TestProductionDashboardRendersWithItsProductionAuthentication(t *testing.T) {
	for name, dash := range map[string]func(*oramav1alpha1.DashboardSpec){
		"from an envFrom Secret": func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{secretSource("dashboard-env")}
		},
		"from secretKeyRefs": func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{
				secretKeyEnv("CRON_SECRET", "dashboard-env", "CRON_SECRET"),
				secretKeyEnv("STRIPE_WEBHOOK_SECRET", "dashboard-env", "STRIPE_WEBHOOK_SECRET"),
				secretKeyEnv("STRIPE_SECRET_KEY", "dashboard-env", "STRIPE_SECRET_KEY"),
			}
		},
		"a later Secret replaces an earlier placeholder": func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{secretSource("old"), secretSource("dashboard-env")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, prodDashOC(dash), secretOf("dashboard-env", productionAuth),
				secretOf("old", map[string]string{"STRIPE_WEBHOOK_SECRET": "dev-stripe-webhook-secret"}))
			h.mustRun()
			dep := h.dashboard()
			if dep == nil {
				t.Fatal("the production dashboard was not rendered")
			}
			env := h.effectiveEnv(dep.Spec.Template.Spec.Containers[0])
			for k, want := range productionAuth {
				if env[k] != want {
					t.Errorf("effective %s is not the production value", k)
				}
			}
			sec := &corev1.Secret{}
			if !h.get("v1-dashboard-secret", sec) {
				t.Fatal("the dashboard component Secret was not created")
			}
			for _, v := range dashboardProductionAuthEnv {
				if _, seeded := sec.Data[v.name]; seeded {
					t.Errorf("a production stack's v1-dashboard-secret was seeded with %s", v.name)
				}
			}
		})
	}
}

// A stack that is not production is unchanged: the placeholders are seeded and the dashboard renders
// without the user's Secrets. Only the label its set writes makes it production.
func TestNonproductionDashboardIsUnchanged(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"no set":         nil,
		"a nonprod set":  {instancePurposeLabel: "nonprod", managedBySetLabel: "OramaCloudV1Set.v1-nonprod"},
		"another label":  {"purpose": productionPurpose},
		"different case": {instancePurposeLabel: "Production"},
	} {
		t.Run(name, func(t *testing.T) {
			oc := dashOC(nil)
			oc.Labels = labels
			h := newEngineHarness(t, oc)
			h.mustRun()
			if h.dashboard() == nil {
				t.Fatal("the dashboard was not rendered")
			}
			sec := &corev1.Secret{}
			h.get("v1-dashboard-secret", sec)
			if string(sec.Data["STRIPE_WEBHOOK_SECRET"]) != "dev-stripe-webhook-secret" {
				t.Error("a nonproduction stack lost its seeded placeholder")
			}
		})
	}
}

// Every placeholder componentSecretData seeds is recognised, so none can pass as a production value.
func TestSeededPlaceholderCoversEveryComponentSecret(t *testing.T) {
	for _, c := range secretComponents {
		if componentSecretData(c) == nil {
			t.Errorf("componentSecretData(%q) is nil: remove it from secretComponents", c)
		}
		for k, v := range componentSecretData(c) {
			if !seededPlaceholder(v) {
				t.Errorf("%s %s's placeholder is not recognised", c, k)
			}
		}
	}
	for _, c := range []string{"selenium", "harvest-mongo", "docs"} {
		if componentSecretData(c) != nil {
			t.Errorf("componentSecretData(%q) seeds a Secret: add it to secretComponents", c)
		}
	}
	if seededPlaceholder(productionAuth["CRON_SECRET"]) {
		t.Error("a production value was taken for a placeholder")
	}
}

// The set writes its member cluster's purpose on the instance, which is what makes it production.
func TestAnOramaCloudV1SetLabelsEachInstanceWithItsClusterPurpose(t *testing.T) {
	for _, purpose := range []string{"production", "nonprod"} {
		set := &oramav1alpha1.OramaCloudV1Set{
			ObjectMeta: metav1.ObjectMeta{Name: "v1-set"},
			Spec: oramav1alpha1.OramaCloudV1SetSpec{ManagedSetSpec: oramav1alpha1.ManagedSetSpec{
				Template: *rawJSON(`{"profile":"light"}`),
				Clusters: []oramav1alpha1.SetMember{{ClusterRef: "leaf", Name: "member"}},
			}},
		}
		members := &fakeMembers{}
		c := setTestClient(t, append(managedCluster("leaf", "eastus", purpose), set)...)
		r := &OramaCloudV1SetReconciler{Client: c, Scheme: clusterTestScheme(t), Applier: members}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1-set"}}); err != nil {
			t.Fatal(err)
		}
		if len(members.applied) != 1 {
			t.Fatalf("%s: applied %d members", purpose, len(members.applied))
		}
		labels := memberLabels(members.applied[0])
		if labels[instancePurposeLabel] != purpose || labels[managedBySetLabel] != "OramaCloudV1Set.v1-set" {
			t.Errorf("%s: labels = %v", purpose, labels)
		}
	}
	if _, ok := memberLabels(memberRequest{Owner: "LakeSet.x"})[instancePurposeLabel]; ok {
		t.Error("a request with no purpose wrote the label")
	}
}
