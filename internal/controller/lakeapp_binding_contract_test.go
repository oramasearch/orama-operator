// SPDX-License-Identifier: MIT

package controller

import (
	"encoding/json"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The Zetta binding file is a contract with app-runtime (lake#1205). app-runtime's
// tests/fixtures/operator-zetta-binding.json holds these same bytes and parses them with
// orama_app.bindings; a change here must change that fixture in the same release.
const lakeAppZettaBindingFixture = "testdata/app-binding-zetta.json"

// lakeAppZettaBindingToken parses a delivered binding the way app-runtime does (one JSON object
// with lake_url, customer_id and token) and returns its token.
func lakeAppZettaBindingToken(t *testing.T, data []byte) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("zetta binding is not one JSON document: %v", err)
	}
	if len(document) != 3 {
		t.Fatalf("zetta binding keys = %d, want lake_url, customer_id and token", len(document))
	}
	for _, key := range []string{"lake_url", "customer_id", "token"} {
		if value, ok := document[key].(string); !ok || value == "" {
			t.Fatalf("zetta binding %s is not a non-empty string", key)
		}
	}
	return document["token"].(string)
}

func TestLakeAppZettaBindingMatchesTheAppRuntimeFixture(t *testing.T) {
	want, err := os.ReadFile(lakeAppZettaBindingFixture)
	if err != nil {
		t.Fatal(err)
	}
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "dev"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake-dev"},
	}
	app := &oramav1alpha1.LakeApp{Spec: oramav1alpha1.LakeAppSpec{
		Bindings: &oramav1alpha1.LakeAppBindings{Zetta: &oramav1alpha1.LakeAppZettaBinding{
			CustomerID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", Scopes: []string{"read"},
		}},
		Sync: &oramav1alpha1.LakeAppSync{WorkspaceID: "11111111-2222-4333-8444-555555555555"},
	}}
	token := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	got, err := lakeAppZettaBinding(lake, app, token)
	if err != nil {
		t.Fatal(err)
	}
	if string(got)+"\n" != string(want) {
		t.Fatalf("zetta binding bytes changed:\n got %s\nwant %s", got, want)
	}
}

// A synchronized app's binding Secret carries the JSON document, never the bare token the
// token Secret holds: the bare token is what crash-looped segmentation-dashboard on lake-dev.
func TestSyncedLakeAppZettaBindingIsTheJSONDocument(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.LakeApp{}).Build()
	h := newAppsSyncHarness(t, c)
	h.converge()
	app := h.app()
	namespace := lakeAppNamespace(app)
	var tokens, binding corev1.Secret
	if err := c.Get(h.ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppTokensName(app)}, &tokens); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(h.ctx, client.ObjectKey{Namespace: namespace, Name: lakeAppBindingsName(app)}, &binding); err != nil {
		t.Fatal(err)
	}
	if len(tokens.Data["zetta"]) != 64 {
		t.Fatal("the synchronized app has no minted zetta token")
	}
	if token := lakeAppZettaBindingToken(t, binding.Data["zetta"]); token != string(tokens.Data["zetta"]) {
		t.Fatal("the binding does not carry the token whose hash Lake registered")
	}
	var document lakeAppZettaBindingDocument
	if err := json.Unmarshal(binding.Data["zetta"], &document); err != nil {
		t.Fatal(err)
	}
	if document.LakeURL != workspaceLakeInternalURL(h.lake, testWorkspaceID) || document.CustomerID != testServiceCustomer {
		t.Fatalf("binding addresses %q for customer %q", document.LakeURL, document.CustomerID)
	}
}
