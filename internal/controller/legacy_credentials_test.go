// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestRemainingCredentialPlaceholdersAreReplacedOnce(t *testing.T) {
	oc := stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Disco.Enabled = true
		o.Spec.Components.PgAdmin.Enabled = true
	})
	legacy := []*corev1.Secret{
		secretOf("v1-disco-secret", map[string]string{"DISCO_SECRET": "dev-disco-secret"}),
		secretOf("v1-workers-secret", map[string]string{"DISCO_SECRET": "dev-disco-secret", "QSTASH_TOKEN": "keep-qstash"}),
		secretOf("v1-index-builder-secret", map[string]string{"DISCO_SECRET_KEY": "dev-disco-secret"}),
		secretOf("v1-pgadmin-secret", map[string]string{"PGADMIN_DEFAULT_PASSWORD": "dev-pgadmin-password"}),
		secretOf("v1-broker-admin", map[string]string{"username": "custom-admin", "password": "dev-rabbitmq-password", "unrelated": "keep-setting"}),
	}
	objects := []client.Object{oc, engineSecret()}
	for _, sec := range legacy {
		objects = append(objects, sec)
	}
	h := newEngineHarness(t, objects...)
	h.mustRun()
	stable := map[string]*corev1.Secret{}
	for name, key := range map[string]string{"v1-disco-auth": "DISCO_SECRET", "v1-pgadmin-auth": "PGADMIN_DEFAULT_PASSWORD", "v1-broker-admin": "password"} {
		sec := h.secret(name)
		if !hex48.Match(sec.Data[key]) {
			t.Errorf("%s/%s was not generated", name, key)
		}
		stable[name] = sec.DeepCopy()
		if reqs := h.r.requestsForSecret(context.Background(), sec); len(reqs) != 1 || reqs[0].Name != oc.Name {
			t.Errorf("%s event did not enqueue its stack", name)
		}
	}
	for _, sec := range legacy[:4] {
		for key, value := range h.secret(sec.Name).Data {
			if (key == "DISCO_SECRET" || key == "DISCO_SECRET_KEY" || key == "PGADMIN_DEFAULT_PASSWORD") && strings.HasPrefix(string(value), "dev-") {
				t.Errorf("%s still holds placeholder %s", sec.Name, key)
			}
		}
	}
	broker := h.secret("v1-broker-admin")
	if string(broker.Data["username"]) != "custom-admin" || string(broker.Data["unrelated"]) != "keep-setting" || len(broker.OwnerReferences) != 0 {
		t.Fatal("broker migration changed supplied fields or adopted the Secret")
	}
	if string(h.secret("v1-workers-secret").Data["QSTASH_TOKEN"]) != "keep-qstash" {
		t.Fatal("migration changed an unrelated component setting")
	}
	h.mustRun()
	for name, before := range stable {
		if !reflect.DeepEqual(h.secret(name), before) {
			t.Errorf("second reconcile wrote %s", name)
		}
	}
	// A retained credential survives teardown and re-enable, including pgAdmin's emptyDir DB.
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Disco.Enabled = false
		o.Spec.Components.PgAdmin.Enabled = false
	})
	h.mustRun()
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Disco.Enabled = true
		o.Spec.Components.PgAdmin.Enabled = true
	})
	h.mustRun()
	for name, before := range stable {
		if !reflect.DeepEqual(h.secret(name), before) {
			t.Errorf("disable/re-enable wrote %s", name)
		}
	}
}

func TestLegacyRealCredentialsAreRetained(t *testing.T) {
	for _, tc := range []struct {
		name      string
		secrets   []*corev1.Secret
		discoWant string
		conflict  bool
	}{
		{name: "agreeing legacy values", discoWant: "supplied-disco-key", secrets: []*corev1.Secret{
			secretOf("v1-disco-secret", map[string]string{"DISCO_SECRET": "supplied-disco-key"}),
			secretOf("v1-workers-secret", map[string]string{"DISCO_SECRET": "supplied-disco-key"}),
			secretOf("v1-index-builder-secret", map[string]string{"DISCO_SECRET_KEY": "supplied-disco-key"}),
		}},
		{name: "one real value replaces placeholders", discoWant: "dev-custom-real-key", secrets: []*corev1.Secret{
			secretOf("v1-disco-secret", map[string]string{"DISCO_SECRET": "dev-disco-secret"}),
			secretOf("v1-index-builder-secret", map[string]string{"DISCO_SECRET_KEY": "dev-custom-real-key"}),
		}},
		{name: "real values keep their character set", discoWant: "supplied+/disco=key!", secrets: []*corev1.Secret{
			secretOf("v1-workers-secret", map[string]string{"DISCO_SECRET": "supplied+/disco=key!"}),
		}},
		{name: "shared value wins after migration", discoWant: "supplied-shared-key", secrets: []*corev1.Secret{
			secretOf("v1-disco-auth", map[string]string{"DISCO_SECRET": "supplied-shared-key"}),
			secretOf("v1-workers-secret", map[string]string{"DISCO_SECRET": "older-supplied-key"}),
		}},
		{name: "shared placeholder adopts real legacy value", discoWant: "supplied-disco-key", secrets: []*corev1.Secret{
			secretOf("v1-disco-auth", map[string]string{"DISCO_SECRET": "dev-disco-secret"}),
			secretOf("v1-workers-secret", map[string]string{"DISCO_SECRET": "supplied-disco-key"}),
		}},
		{name: "conflicting legacy values stop bootstrap", conflict: true, secrets: []*corev1.Secret{
			secretOf("v1-disco-secret", map[string]string{"DISCO_SECRET": "supplied-one"}),
			secretOf("v1-workers-secret", map[string]string{"DISCO_SECRET": "supplied-two"}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := stackOC(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.PgAdmin.Enabled = true
			}) // DISCO itself is disabled; its callers still need the shared key.
			broker := secretOf("v1-broker-admin", map[string]string{"username": "orama", "password": "dev-custom-broker-password"})
			pgadmin := secretOf("v1-pgadmin-secret", map[string]string{"PGADMIN_DEFAULT_PASSWORD": "supplied-pgadmin-password!+/="})
			objects := []client.Object{oc, engineSecret(), broker, pgadmin}
			for _, sec := range tc.secrets {
				objects = append(objects, sec)
			}
			h := newEngineHarness(t, objects...)
			if tc.conflict {
				err := h.mustFail("legacy credentials in")
				if !strings.Contains(err.Error(), "disagree") || strings.Contains(err.Error(), "supplied-one") || strings.Contains(err.Error(), "supplied-two") {
					t.Fatal("conflict must identify locations without printing credential values")
				}
				if h.get("v1-disco-auth", &corev1.Secret{}) || len(h.templates()) != 0 {
					t.Fatal("conflict created a shared credential or changed consumers")
				}
				return
			}
			h.mustRun()
			if string(h.secret("v1-disco-auth").Data["DISCO_SECRET"]) != tc.discoWant || string(h.secret("v1-pgadmin-auth").Data["PGADMIN_DEFAULT_PASSWORD"]) != string(pgadmin.Data["PGADMIN_DEFAULT_PASSWORD"]) {
				t.Fatal("real legacy credential was not retained")
			}
			if !reflect.DeepEqual(h.secret(broker.Name).Data, broker.Data) || !reflect.DeepEqual(h.secret(pgadmin.Name).Data, pgadmin.Data) {
				t.Fatal("migration changed a real credential in its original Secret")
			}
			// Subsequent explicit changes to the shared value must not re-adopt stale copies.
			h.setSecret("v1-disco-auth", "DISCO_SECRET", "updated-shared-key")
			h.mustRun()
			if string(h.secret("v1-disco-auth").Data["DISCO_SECRET"]) != "updated-shared-key" {
				t.Fatal("reconcile restored a stale legacy credential")
			}
		})
	}
}

func TestDisabledCredentialConsumersDoNotSeedComponentSecrets(t *testing.T) {
	oc := engineOC(nil)
	h := newEngineHarness(t, oc, engineSecret())
	if err := h.r.reconcileBootstrap(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"v1-disco-secret", "v1-pgadmin-secret", "v1-disco-auth", "v1-pgadmin-auth"} {
		if h.get(name, &corev1.Secret{}) {
			t.Errorf("disabled consumers created %s", name)
		}
	}
}
