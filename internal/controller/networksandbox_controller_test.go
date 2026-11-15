// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestNetworkSandboxRendersDefaultDenyAndExplicitAllowances(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	tcp := corev1.ProtocolTCP
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: "isolated", UID: "sandbox-uid"},
		Spec: oramav1alpha1.NetworkSandboxSpec{
			PolicyLabels: map[string]string{"orama.cloud/workspace": "workspace-id"},
			Policies: []oramav1alpha1.NetworkSandboxPolicy{
				{
					Name:    "allow-dns-egress",
					Comment: "Every workload needs cluster DNS for Service discovery.",
					Spec: networkingv1.NetworkPolicySpec{
						PodSelector: metav1.LabelSelector{},
						PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
						Egress: []networkingv1.NetworkPolicyEgressRule{{
							To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceNameSelector("kube-system")}},
							Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: intstrPtr(intstr.FromInt(53))}},
						}},
					},
				},
			},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	count, ciliumCount, err := reconcileNetworkSandboxPolicies(ctx, kube, scheme, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || ciliumCount != 1 {
		t.Fatalf("rendered counts = %d, %d", count, ciliumCount)
	}

	var deny networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "isolated", Name: networkSandboxDefaultDenyName}, &deny); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deny.Spec.PolicyTypes, []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	}) || len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
		t.Fatalf("default deny = %#v", deny.Spec)
	}
	if !metav1.IsControlledBy(&deny, sandbox) {
		t.Fatal("default deny is not controlled by the NetworkSandbox")
	}

	var dns networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "isolated", Name: "allow-dns-egress"}, &dns); err != nil {
		t.Fatal(err)
	}
	if dns.Annotations[networkSandboxCommentAnnotation] != sandbox.Spec.Policies[0].Comment {
		t.Fatalf("policy comment = %q", dns.Annotations[networkSandboxCommentAnnotation])
	}
	if !reflect.DeepEqual(dns.Spec, sandbox.Spec.Policies[0].Spec) {
		t.Fatalf("rendered spec = %#v, want %#v", dns.Spec, sandbox.Spec.Policies[0].Spec)
	}
	if dns.Labels["orama.cloud/workspace"] != "workspace-id" {
		t.Fatalf("rendered labels = %v", dns.Labels)
	}
}

func TestNetworkSandboxAdoptsAndPrunesPoliciesWithoutTouchingForeignPolicies(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: "isolated", UID: "sandbox-uid"},
		Spec: oramav1alpha1.NetworkSandboxSpec{Policies: []oramav1alpha1.NetworkSandboxPolicy{{
			Name:    "allow-needed",
			Comment: "A component needs this path.",
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "source"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		}}},
	}
	adopt := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-needed", Namespace: "isolated"}}
	stale := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-stale", Namespace: "isolated"}}
	if err := controllerutil.SetControllerReference(sandbox, stale, scheme); err != nil {
		t.Fatal(err)
	}
	foreign := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "tenant-policy", Namespace: "isolated"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, adopt, stale, foreign).Build()

	if _, _, err := reconcileNetworkSandboxPolicies(ctx, kube, scheme, sandbox); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(stale), &networkingv1.NetworkPolicy{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("stale controlled policy remains: %v", err)
	}
	for _, object := range []client.Object{adopt, foreign} {
		var got networkingv1.NetworkPolicy
		if err := kube.Get(ctx, client.ObjectKeyFromObject(object), &got); err != nil {
			t.Fatalf("policy %s was removed: %v", object.GetName(), err)
		}
		if object.GetName() == adopt.Name && !metav1.IsControlledBy(&got, sandbox) {
			t.Fatal("existing policy was not adopted")
		}
	}
}

func TestNetworkSandboxRefusesReservedAndDuplicatePolicyNames(t *testing.T) {
	scheme := newLakeScheme(t)
	for _, policies := range [][]oramav1alpha1.NetworkSandboxPolicy{
		{{Name: networkSandboxDefaultDenyName, Comment: "invalid", Spec: networkingv1.NetworkPolicySpec{}}},
		{
			{Name: "duplicate", Comment: "first", Spec: networkingv1.NetworkPolicySpec{}},
			{Name: "duplicate", Comment: "second", Spec: networkingv1.NetworkPolicySpec{}},
		},
	} {
		sandbox := &oramav1alpha1.NetworkSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: "isolated", UID: "sandbox-uid"},
			Spec:       oramav1alpha1.NetworkSandboxSpec{Policies: policies},
		}
		kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
		if _, _, err := reconcileNetworkSandboxPolicies(context.Background(), kube, scheme, sandbox); err == nil {
			t.Fatalf("policies %#v were accepted", policies)
		}
	}
}
