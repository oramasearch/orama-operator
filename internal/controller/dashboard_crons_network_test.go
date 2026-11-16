// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestDashboardCronsNetworkAllowsOnlyTheirDashboard(t *testing.T) {
	ctx := context.Background()
	oc := cronOC(nil)
	h := newPublicHarness(t, oc, secretOf(cronSecretName(oc), map[string]string{"CRON_SECRET": cronValue}))
	if err := h.r.reconcileDashboardCrons(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := h.r.reconcileNetworkSandbox(ctx, oc); err != nil {
		t.Fatal(err)
	}
	sandbox := &oramav1alpha1.NetworkSandbox{}
	if err := h.cl.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: networkSandboxName}, sandbox); err != nil {
		t.Fatal(err)
	}
	var crons batchv1.CronJobList
	if err := h.cl.List(ctx, &crons, client.InNamespace(oc.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(crons.Items) != 3 {
		t.Fatalf("CronJobs = %d, want 3", len(crons.Items))
	}
	dashboard := labels.Set(labelsFor(oc, "dashboard"))
	dns := allowanceByName(t, sandbox, "allow-dns-egress")
	for _, cron := range crons.Items {
		t.Run(cron.Name, func(t *testing.T) {
			pod := labels.Set(cron.Spec.JobTemplate.Spec.Template.Labels)
			if !selectorMatches(t, sandbox.Spec.PodSelector, pod) || !selectorMatches(t, dns.Spec.PodSelector, pod) {
				t.Fatal("cron pod must be covered by default-deny and the shared DNS allowance")
			}
			for _, check := range []struct {
				name     string
				src, dst labels.Set
				port     int32
				want     bool
			}{
				{"dashboard", pod, dashboard, 3000, true},
				{"other port", pod, dashboard, 3001, false},
				{"other service", pod, labels.Set(labelsFor(oc, "reader")), 3000, false},
				{"other component", labels.Set(labelsFor(oc, "e2e-canary")), dashboard, 3000, false},
				{"other stack source", labels.Merge(pod, labels.Set{"app.kubernetes.io/instance": "other"}), dashboard, 3000, false},
				{"other stack target", pod, labels.Merge(dashboard, labels.Set{"app.kubernetes.io/instance": "other"}), 3000, false},
			} {
				t.Run(check.name, func(t *testing.T) {
					if got := sandboxEgressAllows(t, sandbox, check.src, check.dst, check.port); got != check.want {
						t.Errorf("egress allowed = %v, want %v", got, check.want)
					}
					if got := sandboxIngressAllows(t, sandbox, check.src, check.dst, check.port); got != check.want {
						t.Errorf("ingress allowed = %v, want %v", got, check.want)
					}
				})
			}
		})
	}
	for _, name := range []string{"allow-dashboard-crons-egress", "allow-dashboard-crons-ingress"} {
		policy := allowanceByName(t, sandbox, name)
		for _, rule := range policy.Spec.Egress {
			if len(rule.To) != 1 || rule.To[0].NamespaceSelector != nil || rule.To[0].IPBlock != nil || len(rule.Ports) != 1 || rule.Ports[0].Protocol == nil || *rule.Ports[0].Protocol != corev1.ProtocolTCP {
				t.Errorf("%s must allow one local pod peer on one TCP port: %+v", name, rule)
			}
		}
		for _, rule := range policy.Spec.Ingress {
			if len(rule.From) != 1 || rule.From[0].NamespaceSelector != nil || rule.From[0].IPBlock != nil || len(rule.Ports) != 1 || rule.Ports[0].Protocol == nil || *rule.Ports[0].Protocol != corev1.ProtocolTCP {
				t.Errorf("%s must allow one local pod peer on one TCP port: %+v", name, rule)
			}
		}
	}
}
