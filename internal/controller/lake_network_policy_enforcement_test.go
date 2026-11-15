package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func networkPolicyConditionForTest(enforced bool, reason string) metav1.Condition {
	status := metav1.ConditionFalse
	if enforced {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{Type: lakeNetworkPolicyConditionType, Status: status, Reason: reason, Message: "test: " + reason}
}

func ciliumAgentForTest(desired, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ciliumAgentNamespace, Name: ciliumAgentName},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: desired, NumberReady: ready},
	}
}

func ciliumCRDForTest() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: ciliumPolicyCRDName}}
}

// The check reads the cluster as it is: the Cilium policy kind and a running Cilium agent, and
// names what is missing otherwise (operator#699).
func TestCiliumNetworkPolicyEnforcement(t *testing.T) {
	s := newLakeScheme(t)
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		objects  []client.Object
		enforced bool
		reason   string
	}{
		{"nothing installed", nil, false, reasonCiliumPolicyKindMissing},
		{"no Cilium policy kind", []client.Object{ciliumAgentForTest(2, 2)}, false, reasonCiliumPolicyKindMissing},
		{"no Cilium agent", []client.Object{ciliumCRDForTest()}, false, reasonCiliumAgentMissing},
		{"agent schedules nothing", []client.Object{ciliumCRDForTest(), ciliumAgentForTest(0, 0)}, false, reasonCiliumAgentNotReady},
		{"no agent ready", []client.Object{ciliumCRDForTest(), ciliumAgentForTest(2, 0)}, false, reasonCiliumAgentNotReady},
		// A node joining starts its agent after it; AKS taints the node until then.
		{"a new node's agent starting", []client.Object{ciliumCRDForTest(), ciliumAgentForTest(3, 2)}, true, reasonNetworkPolicyEnforced},
		{"every agent ready", []client.Object{ciliumCRDForTest(), ciliumAgentForTest(2, 2)}, true, reasonNetworkPolicyEnforced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objects...).Build()
			got := ciliumNetworkPolicyEnforcement(context.Background(), c, 7)
			if got.Enforced != tc.enforced || got.Condition.Reason != tc.reason {
				t.Fatalf("enforced=%t reason=%q; want %t %q (%s)", got.Enforced, got.Condition.Reason, tc.enforced, tc.reason, got.Condition.Message)
			}
			wantStatus := metav1.ConditionFalse
			if tc.enforced {
				wantStatus = metav1.ConditionTrue
			}
			if got.Condition.Type != lakeNetworkPolicyConditionType || got.Condition.Status != wantStatus ||
				got.Condition.ObservedGeneration != 7 || got.Condition.Message == "" {
				t.Fatalf("condition %+v", got.Condition)
			}
			if !tc.enforced && !strings.Contains(got.Condition.Message, "tenant workloads are not started") {
				t.Fatalf("refusal does not say what it stops: %q", got.Condition.Message)
			}
		})
	}
}

// A Lake on a cluster that does not enforce network policy opens no control rows (so starts no
// tenant workload) and reports Degraded with the reason; once Cilium runs, both recover.
func TestLakeRefusesTenantWorkloadsWithoutNetworkPolicy(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.database(1, 1)
	f.reconcile()
	f.expectState("Ready", "LakeReady")

	store := &fakeServiceStore{}
	f.reconciler.ServiceStores = store.opener()
	secret := controlDBAppSecretForTest(f.lake.Name, "postgres://test")
	secret.Namespace = lakeNamespace(f.lake)
	if err := f.client.Create(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	refused := networkPolicyEnforcement{Condition: networkPolicyConditionForTest(false, reasonCiliumAgentMissing)}
	f.reconciler.NetworkPolicyCheck = func(context.Context, client.Reader, int64) networkPolicyEnforcement { return refused }
	f.reconcile()
	f.expectState("Degraded", reasonCiliumAgentMissing)
	if store.opened != 0 {
		t.Fatalf("control rows were read %d times on a cluster without network policy", store.opened)
	}
	condition := apimeta.FindStatusCondition(f.lake.Status.Conditions, lakeNetworkPolicyConditionType)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonCiliumAgentMissing ||
		condition.ObservedGeneration != f.lake.Generation {
		t.Fatalf("NetworkPolicyEnforced condition %+v", condition)
	}

	f.reconciler.NetworkPolicyCheck = func(context.Context, client.Reader, int64) networkPolicyEnforcement { return enforcedForTest }
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	if store.opened == 0 {
		t.Fatal("control rows were not read once network policy was enforced")
	}
	if !apimeta.IsStatusConditionTrue(f.lake.Status.Conditions, lakeNetworkPolicyConditionType) {
		t.Fatal("NetworkPolicyEnforced did not return to True")
	}
}
