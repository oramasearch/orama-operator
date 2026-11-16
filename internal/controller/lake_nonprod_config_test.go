// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Each nonprod Lake must use its own test account and operator-managed webhook, including after
// member overrides are merged. A shared signing secret cannot authenticate three endpoints.
func TestCommittedNonprodLakesUseSeparateManagedPayments(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(clustersDir(), "sets", "lake-nonprod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var set oramav1alpha1.LakeSet
	if err := yaml.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	template, err := jsonObject(set.Spec.Template.Raw, "LakeSet template")
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range set.Spec.Clusters {
		t.Run(member.Name, func(t *testing.T) {
			merged, err := memberSpec(template, resolvedMember{member: member})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(merged)
			if err != nil {
				t.Fatal(err)
			}
			var lake oramav1alpha1.LakeSpec
			if err := json.Unmarshal(raw, &lake); err != nil {
				t.Fatal(err)
			}
			if lake.Billing == nil || !lake.Billing.Enabled || lake.Payments == nil || lake.Payments.Mode != "test" || lake.Payments.SecretRef.Name != "lake-prod-stripe" {
				t.Fatalf("nonprod billing must use managed test payments: billing=%+v payments=%+v", lake.Billing, lake.Payments)
			}
			refs := append(append([]oramav1alpha1.SetSecret{}, set.Spec.Secrets...), member.Secrets...)
			var source oramav1alpha1.SetSecret
			for _, ref := range refs {
				if secretTarget(ref) == lake.Payments.SecretRef.Name {
					source = ref
				}
			}
			if source.Namespace != "lake" || source.Name != "stripe-"+member.Name {
				t.Fatalf("payments must use the member's own sandbox Secret, got %+v", source)
			}
			for _, env := range lake.Env {
				switch env.Name {
				case "LAKE_BILLING", "LAKE_STRIPE_SECRET_KEY", "LAKE_STRIPE_WEBHOOK_SECRET":
					t.Errorf("legacy %s overrides must be removed when payments are managed", env.Name)
				}
			}
		})
	}
}

// The old keys belong to the same field manager on the live members. Exercise the actual remote
// apply path against an API server: replacing the source must remove the legacy credential keys.
func TestManagedSetPaymentSecretReplacesLegacyDataEnvtest(t *testing.T) {
	kube := startLakeEnvtest(t)
	ctx := context.Background()
	req := memberRequest{
		Kind: lakeSetKind.instance, CRD: lakeSetKind.crd,
		Name: "dev", Namespace: "lake-dev", Owner: "LakeSet.lake-nonprod", Purpose: "nonprod",
		Spec: map[string]any{"namespace": "lake-dev", "image": "oramasearch/lake:v0.4.0"},
		Secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-stripe"},
			Type: corev1.SecretTypeOpaque, Data: map[string][]byte{
				"secret_key": []byte("legacy-key"), "webhook_secret": []byte("legacy-signing-secret"),
			}}},
	}
	if _, err := applyMemberWith(ctx, kube, req); err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{
		"accountID": []byte("acct_nonprod"), "appKey": []byte("test-app-key"), "operatorKey": []byte("test-operator-key"),
	}
	req.Secrets[0].Data = want
	if _, err := applyMemberWith(ctx, kube, req); err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "lake-dev", Name: "lake-prod-stripe"}, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Data, want) {
		t.Fatal("the copied Secret data does not exactly match the new source")
	}
}
