// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Every Job the Cloud v1 controller runs against control Postgres reaches it: a sandbox egress rule
// lets its pod out to the CNPG pods on 5432, and the control DB's ingress lets it in. Each case takes
// the pod's labels from the Job's own builder, never a copy. #616, #625 (only the first schema's
// migration was admitted) and #648 (the cluster-row Job was not admitted at all) each shipped a Job
// the sandbox blocked, so a new control DB Job needs a case here.
func TestNetworkSandboxAdmitsEveryControlDBClient(t *testing.T) {
	oc := publicStack(nil)
	other := publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Name = "other" })
	h := newPublicHarness(t, oc)
	if err := h.r.reconcileNetworkSandbox(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	sandbox := &oramav1alpha1.NetworkSandbox{}
	if !h.get(networkSandboxName, sandbox) {
		t.Fatal("no NetworkSandbox")
	}

	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != len(controlDBSchemaSuffixes) {
		t.Fatalf("controlDBSchemas declares %d schemas, controlDBSchemaSuffixes names %d", len(schemas), len(controlDBSchemaSuffixes))
	}
	type controlDBClient struct {
		name string
		pod  func(*oramav1alpha1.OramaCloudV1) labels.Set
	}
	var clients []controlDBClient
	for _, schema := range schemas {
		suffix := schema.suffix
		clients = append(clients, controlDBClient{suffix[1:], func(o *oramav1alpha1.OramaCloudV1) labels.Set {
			// The PostgresSchema reconcileControlDB creates, and the migration pod its controller runs.
			ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: o.Name + suffix, Labels: labelsFor(o, "controldb")}}
			return pgMigrationJobSpec(ps).Template.Labels
		}})
	}
	clients = append(clients, controlDBClient{"controldb-cluster-row", func(o *oramav1alpha1.OramaCloudV1) labels.Set {
		return controlDBClusterRowJob(o).Spec.Template.Labels
	}})

	// The CNPG instance pods: the Cluster's inheritedMetadata labels plus CNPG's own.
	cnpg := labels.Set(labelsFor(oc, "controldb"))
	cnpg["cnpg.io/cluster"] = oc.Name + "-controldb"

	for _, c := range clients {
		t.Run(c.name, func(t *testing.T) {
			pod := c.pod(oc)
			if !selectorMatches(t, sandbox.Spec.PodSelector, pod) {
				t.Error("the sandbox's own pod selector does not cover the pod, so none of its allowances apply")
			}
			if !sandboxEgressAllows(t, sandbox, pod, cnpg, 5432) {
				t.Error("no allowance lets the pod out to the CNPG pods on TCP 5432")
			}
			if !sandboxIngressAllows(t, sandbox, pod, cnpg, 5432) {
				t.Error("no allowance lets the pod into the CNPG pods on TCP 5432")
			}
			foreign := c.pod(other)
			if sandboxEgressAllows(t, sandbox, foreign, cnpg, 5432) {
				t.Error("an allowance lets another stack's pod out to this stack's CNPG pods")
			}
			if sandboxIngressAllows(t, sandbox, foreign, cnpg, 5432) {
				t.Error("an allowance lets another stack's pod into this stack's CNPG pods")
			}
		})
	}
}

// sandboxEgressAllows reports whether an allowance that selects src has an egress rule to dst on the
// TCP port through a namespace-local pod peer.
func sandboxEgressAllows(t *testing.T, sandbox *oramav1alpha1.NetworkSandbox, src, dst labels.Set, port int32) bool {
	t.Helper()
	for _, p := range sandbox.Spec.Policies {
		if !selectorMatches(t, p.Spec.PodSelector, src) {
			continue
		}
		for _, rule := range p.Spec.Egress {
			if localPeerMatches(t, rule.To, dst) && portsAllowTCP(rule.Ports, port) {
				return true
			}
		}
	}
	return false
}

// sandboxIngressAllows reports whether an allowance that selects dst has an ingress rule from src on
// the TCP port through a namespace-local pod peer.
func sandboxIngressAllows(t *testing.T, sandbox *oramav1alpha1.NetworkSandbox, src, dst labels.Set, port int32) bool {
	t.Helper()
	for _, p := range sandbox.Spec.Policies {
		if !selectorMatches(t, p.Spec.PodSelector, dst) {
			continue
		}
		for _, rule := range p.Spec.Ingress {
			if localPeerMatches(t, rule.From, src) && portsAllowTCP(rule.Ports, port) {
				return true
			}
		}
	}
	return false
}

// localPeerMatches counts only pod-selector peers: a namespace or IP peer is wider than any rule a
// control DB client needs.
func localPeerMatches(t *testing.T, peers []networkingv1.NetworkPolicyPeer, pod labels.Set) bool {
	t.Helper()
	for _, peer := range peers {
		if peer.PodSelector != nil && peer.NamespaceSelector == nil && peer.IPBlock == nil &&
			selectorMatches(t, *peer.PodSelector, pod) {
			return true
		}
	}
	return false
}

func portsAllowTCP(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	for _, p := range ports {
		if (p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP) && p.Port != nil && p.Port.IntValue() == int(port) {
			return true
		}
	}
	return false
}
