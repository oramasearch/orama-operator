package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Every control DB schema's migration pod reaches the control DB: the sandbox's schema egress rule
// selects it, and the control DB's ingress admits it. On v0.6.1 only the first schema was admitted,
// so <cr>-controldb-schema-0008 waited for the database forever on every stack (operator#625).
func TestNetworkSandboxAdmitsEveryControlDBSchema(t *testing.T) {
	oc := publicStack(nil)
	h := newPublicHarness(t, oc)
	if err := h.r.reconcileNetworkSandbox(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	sandbox := &oramav1alpha1.NetworkSandbox{}
	if !h.get(networkSandboxName, sandbox) {
		t.Fatal("no NetworkSandbox")
	}
	policies := map[string]oramav1alpha1.NetworkSandboxPolicy{}
	for _, p := range sandbox.Spec.Policies {
		policies[p.Name] = p
	}
	egress, ok := policies["allow-postgres-schema-egress"]
	if !ok {
		t.Fatal("no allow-postgres-schema-egress")
	}
	ingress, ok := policies["allow-controldb-ingress"]
	if !ok {
		t.Fatal("no allow-controldb-ingress")
	}

	matches := func(t *testing.T, sel *metav1.LabelSelector, podLabels labels.Set) bool {
		t.Helper()
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			t.Fatal(err)
		}
		return s.Matches(podLabels)
	}

	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != len(controlDBSchemaSuffixes) {
		t.Fatalf("controlDBSchemas declares %d schemas, controlDBSchemaSuffixes names %d", len(schemas), len(controlDBSchemaSuffixes))
	}
	for _, schema := range schemas {
		// The labels the PostgresSchema controller puts on its migration pods.
		pod := labels.Set{
			"app.kubernetes.io/instance":   oc.Name,
			"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
			"orama.cloud/postgresschema":   oc.Name + schema.suffix,
		}
		if !matches(t, &egress.Spec.PodSelector, pod) {
			t.Errorf("allow-postgres-schema-egress does not select %s's migration pod", oc.Name+schema.suffix)
		}
		admitted := false
		for _, rule := range ingress.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.PodSelector != nil && matches(t, peer.PodSelector, pod) {
					admitted = true
				}
			}
		}
		if !admitted {
			t.Errorf("allow-controldb-ingress does not admit %s's migration pod", oc.Name+schema.suffix)
		}
	}

	// Another stack's schema Job is not admitted.
	other := labels.Set{"orama.cloud/postgresschema": "other-controldb-schema-0008"}
	if matches(t, &egress.Spec.PodSelector, other) {
		t.Error("allow-postgres-schema-egress selects another stack's schema pod")
	}
}
