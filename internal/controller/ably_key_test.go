// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func ablyStack() *oramav1alpha1.OramaCloudV1 {
	return stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Ingestion.AblyKeySecretRef = &oramav1alpha1.SecretKeyRef{Name: "progress-key", Key: "api-key"}
	})
}

func TestAblyKeyReferencePrecedenceAndRotation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "existing component Secret"}[legacy], func(t *testing.T) {
			h := newEngineHarness(t, ablyStack(), engineSecret(), secretOf("progress-key", map[string]string{"api-key": "test-ably-key"}))
			if legacy {
				h.put(secretOf("v1-index-builder-secret", map[string]string{"ABLY_API_KEY": "old-component-key"}))
			}
			h.mustRun()
			before := h.templates()
			builder := containerIn(t, before["index-builder"].Spec, "index-builder")
			ref := secretRef(builder.Env, "ABLY_API_KEY")
			if ref == nil || ref.Name != "progress-key" || ref.Key != "api-key" || ref.Optional != nil {
				t.Fatalf("ABLY_API_KEY = %+v, want required progress-key/api-key", ref)
			}
			if !slices.ContainsFunc(builder.EnvFrom, func(e corev1.EnvFromSource) bool {
				return e.SecretRef != nil && e.SecretRef.Name == "v1-index-builder-secret"
			}) || h.effectiveEnv(builder)["ABLY_API_KEY"] != "test-ably-key" {
				t.Fatal("the explicit reference must win over the retained component envFrom")
			}
			seed, seeded := h.secret("v1-index-builder-secret").Data["ABLY_API_KEY"]
			if seeded != legacy || (legacy && string(seed) != "old-component-key") {
				t.Fatal("reference must suppress new seeds and preserve existing component Secret data")
			}
			if _, ok := h.secret("v1-index-builder-secret").Data["APIFY_TOKEN"]; !ok {
				t.Fatal("the undecided Apify token must keep its historical seed")
			}
			master := containerIn(t, before["harvest-master"].Spec, "harvest-master")
			if envNamed(master.Env, "ABLY_API_KEY") != nil || len(componentKeyRefs(h.stored(), "harvest")) != 0 {
				t.Fatal("do not expose the key to HARVEST before its worker launcher exists (#12)")
			}

			// A copied external Secret has no owner: its event must still enqueue the stack.
			requests := h.r.requestsForSecret(context.Background(), h.secret("progress-key"))
			if len(requests) != 1 || requests[0].Name != "v1" || requests[0].Namespace != "orama-system" {
				t.Fatalf("referenced Secret watch = %v", requests)
			}
			otherNamespace := h.secret("progress-key").DeepCopy()
			otherNamespace.Namespace = "other"
			if requests := h.r.requestsForSecret(context.Background(), otherNamespace); len(requests) != 0 {
				t.Fatalf("cross-namespace Secret event enqueued %v", requests)
			}

			h.setSecret("progress-key", "api-key", "rotated-test-ably-key")
			h.mustRun()
			after := h.templates()
			for component, template := range before {
				changed := template.Annotations[credentialsHashKey] != after[component].Annotations[credentialsHashKey]
				if changed != (component == "index-builder") {
					t.Errorf("%s credentials hash changed = %v", component, changed)
				}
			}
			if h.effectiveEnv(containerIn(t, after["index-builder"].Spec, "index-builder"))["ABLY_API_KEY"] != "rotated-test-ably-key" {
				t.Fatal("index-builder did not resolve the rotated Ably key")
			}
			h.mustRun()
			if !reflect.DeepEqual(after["index-builder"], h.templates()["index-builder"]) {
				t.Fatal("a steady reconcile changed the index-builder template")
			}
			if got := string(h.secret("progress-key").Data["api-key"]); got != "rotated-test-ably-key" {
				t.Fatal("operator changed the external key")
			}
		})
	}
}

func TestAblyKeyReferenceFailurePreservesRunningTemplate(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		data       map[string]string
	}{
		{"missing Secret", "does not exist", nil},
		{"missing key", "has no key api-key", map[string]string{"another-key": "unused"}},
		{"empty value", "key api-key is empty", map[string]string{"api-key": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngineHarness(t, ablyStack(), engineSecret(), secretOf("progress-key", map[string]string{"api-key": "test-ably-key"}))
			h.mustRun()
			before := h.templates()["index-builder"]
			if err := h.cl.Delete(context.Background(), h.secret("progress-key")); err != nil {
				t.Fatal(err)
			}
			if tc.data != nil {
				h.put(secretOf("progress-key", tc.data))
			}
			want := "ABLY_API_KEY: Secret orama-system/progress-key (spec.components.ingestion.ablyKeySecretRef) " + tc.want
			h.mustFail(want)
			if !reflect.DeepEqual(before, h.templates()["index-builder"]) {
				t.Fatal("a broken reference changed the running index-builder template")
			}
			if c := h.readyCondition(); c == nil || c.Status != "False" || !strings.Contains(c.Message, "ingestion: "+want) {
				t.Fatalf("Ready condition does not name the broken reference: %+v", c)
			}
			h.put(secretOf("progress-key", map[string]string{"api-key": "test-ably-key"}))
			h.mustRun()
			if !reflect.DeepEqual(before, h.templates()["index-builder"]) {
				t.Fatal("restoring the same key should recover without a rollout")
			}
		})
	}

	h := newEngineHarness(t, ablyStack(), engineSecret())
	h.mustFail("ABLY_API_KEY: Secret orama-system/progress-key")
	if h.get("v1-index-builder", &appsv1.Deployment{}) {
		t.Fatal("a missing reference created an index-builder Deployment")
	}
}

func TestAblyWithoutReferencePreservesSandbox(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	b := containerIn(t, h.templates()["index-builder"].Spec, "index-builder")
	if envNamed(b.Env, "ABLY_API_KEY") != nil || h.effectiveEnv(b)["ABLY_API_KEY"] != componentSecretData("index-builder")["ABLY_API_KEY"] {
		t.Fatal("an unset reference changed the historical index-builder sandbox environment")
	}
	if componentSecretData("harvest")["ABLY_API_KEY"] != componentSecretData("index-builder")["ABLY_API_KEY"] {
		t.Fatal("HARVEST's historical sandbox seed changed")
	}
	oc := ablyStack()
	oc.Spec.Components.Ingestion.Enabled = false
	if slices.Contains(secretsReadBy(oc), "progress-key") {
		t.Fatal("disabled ingestion watches an unused Ably Secret")
	}
}
