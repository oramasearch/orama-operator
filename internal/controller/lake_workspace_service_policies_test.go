// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestWorkspaceServiceIngressPairsWithLakeEgress(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	r, kube := newWorkspaceHarness(t, lake)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	r.syncLakeWorkspaces(ctx, lake, store, store.rows)
	var sandbox oramav1alpha1.NetworkSandbox
	key := client.ObjectKey{Namespace: testWorkspaceName, Name: networkSandboxName}
	if err := kube.Get(ctx, key, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, &sandbox)
	cases := []struct {
		kind     string
		selector metav1.LabelSelector
		pods     map[string]string
		ports    []int32
	}{
		{
			kind: "postgres",
			selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "cnpg.io/cluster", Operator: metav1.LabelSelectorOpExists},
			}},
			pods:  map[string]string{"cnpg.io/cluster": "db", "cnpg.io/podRole": "instance"},
			ports: []int32{5432},
		},
		{
			kind: "kafka",
			selector: metav1.LabelSelector{
				MatchLabels: map[string]string{"strimzi.io/kind": "Kafka"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "strimzi.io/cluster", Operator: metav1.LabelSelectorOpExists},
				},
			},
			pods:  map[string]string{"strimzi.io/kind": "Kafka", "strimzi.io/cluster": "db"},
			ports: []int32{9092},
		},
		{
			kind: "clickhouse",
			selector: metav1.LabelSelector{
				MatchLabels: map[string]string{"clickhouse.altinity.com/app": "chop"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "clickhouse.altinity.com/chi", Operator: metav1.LabelSelectorOpExists},
				},
			},
			pods:  map[string]string{"clickhouse.altinity.com/app": "chop", "clickhouse.altinity.com/chi": "db"},
			ports: []int32{8123, 9000},
		},
		{
			kind: "tidb",
			selector: metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/component": "tidb", "app.kubernetes.io/managed-by": "tidb-operator",
			}},
			pods:  map[string]string{"app.kubernetes.io/component": "tidb", "app.kubernetes.io/managed-by": "tidb-operator"},
			ports: []int32{4000},
		},
		{
			kind: "cassandra",
			selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "cassandra.datastax.com/cluster", Operator: metav1.LabelSelectorOpExists},
			}},
			pods:  map[string]string{"cassandra.datastax.com/cluster": "db"},
			ports: []int32{9042},
		},
		{
			kind: "valkey",
			selector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "valkey-operator"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "valkey.io/cluster", Operator: metav1.LabelSelectorOpExists},
				},
			},
			pods:  map[string]string{"app.kubernetes.io/managed-by": "valkey-operator", "valkey.io/cluster": "db"},
			ports: []int32{6379},
		},
	}
	namespaces := &metav1.LabelSelector{MatchLabels: map[string]string{workspaceLakeLabel: lake.Name}}
	egress := &oramav1alpha1.NetworkSandbox{Spec: oramav1alpha1.NetworkSandboxSpec{
		Policies: lakeWorkspaceServiceAllowances(lakePodSelector(lake), namespaces),
	}}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			allowance := allowanceByName(t, &sandbox, "allow-lake-service-"+tc.kind+"-ingress")
			var policy networkingv1.NetworkPolicy
			if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: allowance.Name}, &policy); err != nil {
				t.Fatal(err)
			}
			if policy.Annotations[networkSandboxCommentAnnotation] != allowance.Comment || allowance.Comment == "" || !metav1.IsControlledBy(&policy, &sandbox) {
				t.Fatal("rendered policy lost its justification or sandbox owner")
			}
			if !reflect.DeepEqual(policy.Spec.PodSelector, tc.selector) || !selectorMatches(t, policy.Spec.PodSelector, tc.pods) {
				t.Fatalf("unexpected %s destinations: %+v", tc.kind, policy.Spec.PodSelector)
			}
			if !reflect.DeepEqual(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) || len(policy.Spec.Egress) != 0 || len(policy.Spec.Ingress) != 1 {
				t.Fatalf("not a single ingress rule: %+v", policy.Spec)
			}
			rule := policy.Spec.Ingress[0]
			if !reflect.DeepEqual(policyPorts(t, rule.Ports), tc.ports) || len(rule.From) != 1 {
				t.Fatalf("unexpected ports or sources: %+v", rule)
			}
			assertOnlyOwningLakePeer(t, lake, rule.From[0])
			out := allowanceByName(t, egress, "allow-lake-to-workspace-"+tc.kind).Spec.Egress[0]
			if !reflect.DeepEqual(out.To[0].PodSelector, &policy.Spec.PodSelector) || !reflect.DeepEqual(out.Ports, rule.Ports) {
				t.Fatal("Lake egress and workspace ingress do not pair")
			}
			for _, other := range cases {
				if other.kind != tc.kind && selectorMatches(t, policy.Spec.PodSelector, other.pods) {
					t.Fatalf("%s ingress also selects %s pods", tc.kind, other.kind)
				}
			}
			for _, foreign := range []map[string]string{nil, {"app.kubernetes.io/name": "lake-app"}, {"app.kubernetes.io/component": "pd", "app.kubernetes.io/managed-by": "tidb-operator"}} {
				if selectorMatches(t, policy.Spec.PodSelector, foreign) {
					t.Fatalf("client ingress selects unrelated pod %v", foreign)
				}
			}
		})
	}
	r.syncLakeWorkspaces(ctx, lake, store, store.rows)
	var again oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, key, &again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != sandbox.ResourceVersion {
		t.Fatal("unchanged workspace sandbox was rewritten")
	}
}

func assertOnlyOwningLakePeer(t *testing.T, lake *oramav1alpha1.Lake, peer networkingv1.NetworkPolicyPeer) {
	t.Helper()
	want := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": lakeNamespace(lake)}},
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": lake.Name,
		}},
	}
	if !reflect.DeepEqual(peer, want) {
		t.Fatalf("source is not only the owning Lake: %+v", peer)
	}
	if selectorMatches(t, *peer.NamespaceSelector, map[string]string{"kubernetes.io/metadata.name": "another-namespace"}) {
		t.Fatal("Lake labels copied into another namespace admitted")
	}
	for _, labels := range []map[string]string{
		nil,
		{"app.kubernetes.io/name": "orama-operator"},
		{"app.kubernetes.io/name": "lake"},
		{"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": "another-lake"},
	} {
		if selectorMatches(t, *peer.PodSelector, labels) {
			t.Fatalf("non-owner pod admitted: %v", labels)
		}
	}
}

func TestLakeAppBackendIngressPairsWithLakeEgress(t *testing.T) {
	for _, synchronized := range []bool{false, true} {
		name := "standalone"
		if synchronized {
			name = "workspace"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			lake := testWorkspaceLake()
			app := &oramav1alpha1.LakeApp{
				ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: lakeNamespace(lake)},
				Spec:       oramav1alpha1.LakeAppSpec{LakeRef: lake.Name, Port: 9123},
			}
			if synchronized {
				app.Spec.Sync = &oramav1alpha1.LakeAppSync{ID: "app-id", WorkspaceNamespace: testWorkspaceName}
			}
			scheme := newLakeAppScheme(t)
			objects := []client.Object{app}
			if synchronized {
				objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName}})
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &LakeAppReconciler{Client: kube, Scheme: scheme}
			key := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppAccessPolicyName(app)}
			for _, port := range []int32{9123, 8765} {
				app.Spec.Port = port
				if err := kube.Update(ctx, app); err != nil {
					t.Fatal(err)
				}
				if err := r.reconcileLakeAppAccessPolicy(ctx, lake, app, key.Namespace); err != nil {
					t.Fatal(err)
				}
				var policy networkingv1.NetworkPolicy
				if err := kube.Get(ctx, key, &policy); err != nil {
					t.Fatal(err)
				}
				assertLakeAppBackendPolicy(t, lake, app, &policy)
				lakeReconciler := &LakeReconciler{Client: kube, Scheme: scheme}
				out, err := lakeReconciler.lakeAppProxyAllowances(ctx, lake, lakePodSelector(lake))
				if err != nil || len(out) != 1 || len(out[0].Spec.Egress) != 1 {
					t.Fatalf("app egress = %v, error %v", out, err)
				}
				egress := out[0].Spec.Egress[0]
				if !reflect.DeepEqual(egress.Ports, policy.Spec.Ingress[1].Ports) || !reflect.DeepEqual(egress.To, []networkingv1.NetworkPolicyPeer{namespacePodPeer(key.Namespace, lakeAppLabels(app))}) {
					t.Fatal("Lake egress does not pair with this app's ingress")
				}
				if err := r.reconcileLakeAppAccessPolicy(ctx, lake, app, key.Namespace); err != nil {
					t.Fatal(err)
				}
				var again networkingv1.NetworkPolicy
				if err := kube.Get(ctx, key, &again); err != nil || again.ResourceVersion != policy.ResourceVersion {
					t.Fatalf("unchanged app policy was rewritten: %v", err)
				}
			}
		})
	}
}

func TestSynchronizedAppServiceEgressComesOnlyFromConnectionsAfterActivation(t *testing.T) {
	lake := testWorkspaceLake()
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: lakeNamespace(lake)},
		Spec: oramav1alpha1.LakeAppSpec{
			LakeRef: lake.Name,
			Sync: &oramav1alpha1.LakeAppSync{
				ID:                 "app-id",
				WorkspaceNamespace: testWorkspaceName,
			},
			Bindings: &oramav1alpha1.LakeAppBindings{
				Services: []string{testPostgresID},
			},
		},
	}
	rules := lakeAppEgressRules(lake, app, true)
	if len(rules) != 2 {
		t.Fatalf("active app egress has %d rules, want DNS and Lake only: %#v", len(rules), rules)
	}
	for _, rule := range rules {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels[serviceIDLabel] != "" {
				t.Fatalf("legacy service binding still opens egress: %#v", rules)
			}
		}
	}
}

func assertLakeAppBackendPolicy(t *testing.T, lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	if !reflect.DeepEqual(policy.Spec.PodSelector, metav1.LabelSelector{MatchLabels: lakeAppLabels(app)}) || len(policy.Spec.Ingress) != 2 {
		t.Fatalf("unexpected app selector or ingress: %+v", policy.Spec)
	}
	if policy.Annotations[networkSandboxCommentAnnotation] == "" || policy.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
		t.Fatal("app policy lost its justification or ownership annotation")
	}
	wantNginx := networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceNameSelector("ingress-nginx")}},
		Ports: tcpPorts(app.Spec.Port),
	}
	if !reflect.DeepEqual(policy.Spec.Ingress[0], wantNginx) || !reflect.DeepEqual(policy.Spec.Egress, lakeAppEgressRules(lake, app, false)) {
		t.Fatal("existing nginx ingress or app egress changed")
	}
	rule := policy.Spec.Ingress[1]
	if len(rule.From) != 1 || !reflect.DeepEqual(policyPorts(t, rule.Ports), []int32{app.Spec.Port}) {
		t.Fatalf("unexpected backend ingress: %+v", rule)
	}
	assertOnlyOwningLakePeer(t, lake, rule.From[0])
	otherApp := app.DeepCopy()
	otherApp.Name = "unrelated"
	if selectorMatches(t, policy.Spec.PodSelector, lakeAppLabels(otherApp)) || selectorMatches(t, policy.Spec.PodSelector, nil) {
		t.Fatal("backend ingress selects an unrelated or unlabelled app")
	}
}
