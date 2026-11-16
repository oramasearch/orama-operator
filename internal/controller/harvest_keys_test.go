// SPDX-License-Identifier: MIT

package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// HARVEST's control key is one generated value: index-builder sends it as HARVEST_SIGN and
// harvest-master checks it as HARVEST_SIGNATURE, both by secretKeyRef to <cr>-harvest-sign.
// harvest-master alone holds NOTIFICATION_SIGNATURE, a second, independent generated key. Both are
// at least the 32 characters HARVEST requires, and neither is replaced by a later reconcile.
func TestHarvestKeysAreGeneratedAndShared(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()

	sign, notification := h.secret("v1-harvest-sign"), h.secret("v1-harvest-notification")
	signKey, notificationKey := string(sign.Data[harvestSignKey]), string(notification.Data[harvestNotificationKey])
	for _, sec := range []*corev1.Secret{sign, notification} {
		if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != "v1" || sec.Labels[componentLabel] != credentialsComponent {
			t.Errorf("%s owner %+v, component %q", sec.Name, sec.OwnerReferences, sec.Labels[componentLabel])
		}
	}
	if !hex64.MatchString(signKey) || !hex64.MatchString(notificationKey) {
		t.Fatal("a HARVEST key is not 64 hex characters (32 random bytes)")
	}
	if signKey == notificationKey {
		t.Error("the notification key equals the control key; HARVEST wants an independent one")
	}

	ref := func(env []corev1.EnvVar, name string) *corev1.SecretKeySelector {
		e := envNamed(env, name)
		if e == nil || e.Value != "" || e.ValueFrom == nil {
			return nil
		}
		return e.ValueFrom.SecretKeyRef
	}
	tmpls := h.templates()
	master := containerIn(t, tmpls["harvest-master"].Spec, "harvest-master").Env
	builder := containerIn(t, tmpls["index-builder"].Spec, "index-builder").Env
	for _, c := range []struct {
		who, env, secret, key string
		got                   *corev1.SecretKeySelector
	}{
		{"harvest-master", "HARVEST_SIGNATURE", "v1-harvest-sign", harvestSignKey, ref(master, "HARVEST_SIGNATURE")},
		{"harvest-master", "NOTIFICATION_SIGNATURE", "v1-harvest-notification", harvestNotificationKey, ref(master, "NOTIFICATION_SIGNATURE")},
		{"index-builder", "HARVEST_SIGN", "v1-harvest-sign", harvestSignKey, ref(builder, "HARVEST_SIGN")},
	} {
		if c.got == nil || c.got.Name != c.secret || c.got.Key != c.key {
			t.Errorf("%s %s = %+v, want a secretKeyRef to %s/%s", c.who, c.env, c.got, c.secret, c.key)
		}
	}
	if envNamed(builder, "NOTIFICATION_SIGNATURE") != nil {
		t.Error("index-builder holds the notification key; only harvest-master signs notifications")
	}
	// No Podman API runs in the cluster (operator#12), so none is invented.
	if e := envNamed(master, "PODMAN_API_HOST"); e != nil {
		t.Errorf("harvest-master PODMAN_API_HOST = %+v; the cluster has no Podman API", e)
	}
	// The dev-* placeholders are no longer seeded beside the explicit entries.
	for _, comp := range []string{"index-builder", "harvest-master"} {
		if _, ok := h.secret(componentSecretName(h.stored(), comp)).Data["HARVEST_SIGN"]; ok {
			t.Errorf("%s's component Secret still seeds HARVEST_SIGN", comp)
		}
	}

	h.mustRun()
	if string(h.secret("v1-harvest-sign").Data[harvestSignKey]) != signKey ||
		string(h.secret("v1-harvest-notification").Data[harvestNotificationKey]) != notificationKey {
		t.Error("a second reconcile changed a HARVEST key")
	}
}

// A key supplied before the first reconcile is kept; the dashboard needs the control key even
// without ingestion, while the notification key belongs only to harvest-master.
func TestHarvestKeysKeepASuppliedValueAndFollowIngestion(t *testing.T) {
	const supplied = "supplied-harvest-control-key-0123456789"
	h := newEngineHarness(t, stackOC(nil), engineSecret(), secretOf("v1-harvest-sign", map[string]string{harvestSignKey: supplied}))
	h.mustRun()
	if got := string(h.secret("v1-harvest-sign").Data[harvestSignKey]); got != supplied {
		t.Errorf("control key = %q, want the supplied value kept", got)
	}

	h = newEngineHarness(t, stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Ingestion = oramav1alpha1.IngestionSpec{}
	}), engineSecret())
	h.mustRun()
	if !h.get("v1-harvest-sign", &corev1.Secret{}) {
		t.Error("dashboard has no control key without ingestion")
	}
	dashboard := containerIn(t, h.templates()["dashboard"].Spec, "dashboard")
	if ref := secretRef(dashboard.Env, "SCRAPER_SIGNATURE"); ref == nil || ref.Name != "v1-harvest-sign" || ref.Key != harvestSignKey {
		t.Errorf("dashboard SCRAPER_SIGNATURE = %+v", ref)
	}
	if h.get("v1-harvest-notification", &corev1.Secret{}) {
		t.Error("harvest notification key exists without ingestion")
	}
	if h.get("v1-harvest-master", &appsv1.Deployment{}) {
		t.Error("harvest-master runs without ingestion")
	}
}
