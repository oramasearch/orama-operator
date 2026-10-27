/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The network sandboxes of the pixie provider's namespaces (#465: default-deny for every namespace
// the operator creates). pl and plc are wholly the operator's, so each sandbox selects every pod in
// its namespace, the PeerConnection gateways included, and the PeerConnection controller adds its
// own peer-<name>-… allowances, which these renderers keep.

// pixieMonitoringNamespace is where the platform's Prometheus runs (kube-prometheus-stack).
const pixieMonitoringNamespace = "monitoring"

var everyPod = metav1.LabelSelector{}

func namespaceInternal(name, comment string) oramav1alpha1.NetworkSandboxPolicy {
	return sandboxAllowance(comment, networkPolicy(name, everyPod,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}},
		[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}},
	))
}

func pixieAPIServerAllowance(ctx context.Context, reader client.Reader, namespace, comment string) (oramav1alpha1.NetworkSandboxPolicy, error) {
	target, err := readKubernetesAPIServerTarget(ctx, reader)
	if err != nil {
		return oramav1alpha1.NetworkSandboxPolicy{}, err
	}
	apiClient := workspaceAPIServerClient{name: "allow-apiserver-egress", comment: comment, pods: everyPod}
	return sandboxAllowance(comment, workspaceAPIServerEgressPolicy(apiClient, target, namespace, nil)), nil
}

func ingressFromNamespace(name, namespace string, labels map[string]string, to metav1.LabelSelector, ports ...int32) *networkingv1.NetworkPolicy {
	return networkPolicy(name, to, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{namespacePodPeer(namespace, labels)}, Ports: tcpPorts(ports...)}}, nil)
}

// vizierSandboxPolicies are Vizier's allowances. The node agents run on the host network, which a
// NetworkPolicy cannot name, so their path to Kelvin, NATS and the query broker is a Cilium
// fromEntities rule.
func vizierSandboxPolicies(ctx context.Context, reader client.Reader, vz *oramav1alpha1.PixieVizier, localHub bool) ([]oramav1alpha1.NetworkSandboxPolicy, []oramav1alpha1.NetworkSandboxCiliumPolicy, error) {
	api, err := pixieAPIServerAllowance(ctx, reader, oramav1alpha1.PixieVizierNamespace,
		"The metadata service, cloud connector and query broker watch the Kubernetes API.")
	if err != nil {
		return nil, nil, err
	}
	policies := []oramav1alpha1.NetworkSandboxPolicy{
		namespaceInternal("allow-vizier-internal", "Vizier's services call each other and NATS inside the namespace."),
		sandboxAllowance("Vizier resolves its services and the hub's.", workspaceDNSPolicy(oramav1alpha1.PixieVizierNamespace, nil)),
		api,
	}
	if localHub {
		policies = append(policies, sandboxAllowance(
			"The cloud connector reaches the hub's vzconn on this cluster; any other cluster is reached only through a PeerConnection.",
			networkPolicy("allow-hub-egress", podSelector(map[string]string{pixieComponentLabel: "vizier-cloud-connector"}),
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil,
				[]networkingv1.NetworkPolicyEgressRule{{
					To:    []networkingv1.NetworkPolicyPeer{namespacePodPeer(oramav1alpha1.PixieCloudNamespace, map[string]string{pixieComponentLabel: "vzconn-server"})},
					Ports: tcpPorts(pixieVzconnPort),
				}}),
		))
	}
	hostSpec, err := json.Marshal(map[string]any{
		"endpointSelector": map[string]any{},
		"ingress":          []any{map[string]any{"fromEntities": []string{"host", "remote-node"}}},
	})
	if err != nil {
		return nil, nil, err
	}
	cilium := []oramav1alpha1.NetworkSandboxCiliumPolicy{{
		Name:    "allow-node-agents",
		Comment: "The node agents (PEM) run on each node's host network and send to Kelvin, NATS and the query broker; NetworkPolicy cannot select the host.",
		Spec:    apiextensionsv1.JSON{Raw: hostSpec},
	}}
	return policies, cilium, nil
}

// pixieCloudSandboxPolicies are the hub's allowances.
func pixieCloudSandboxPolicies(ctx context.Context, reader client.Reader, operatorNamespace string) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	api, err := pixieAPIServerAllowance(ctx, reader, oramav1alpha1.PixieCloudNamespace,
		"Pixie's services resolve each other through the Kubernetes API (kuberesolver), and CloudNativePG's instances read their Cluster.")
	if err != nil {
		return nil, err
	}
	cnpgOperator := workspaceCNPGOperator.pods
	policies := []oramav1alpha1.NetworkSandboxPolicy{
		namespaceInternal("allow-cloud-internal", "Pixie Cloud's services, NATS, OpenSearch and Postgres call each other inside the namespace."),
		sandboxAllowance("Pixie Cloud resolves its services.", workspaceDNSPolicy(oramav1alpha1.PixieCloudNamespace, nil)),
		api,
		sandboxAllowance("The CloudNativePG operator needs the instance-manager status and Postgres ports.",
			networkPolicy("allow-cnpg-operator-ingress", podSelector(map[string]string{"cnpg.io/cluster": pixieDBCluster}),
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceNameSelector(workspaceCNPGOperator.namespace), PodSelector: &cnpgOperator}},
					Ports: tcpPorts(8000, 5432),
				}}, nil)),
		sandboxAllowance("A Vizier on the hub's own cluster registers with vzconn; other clusters arrive through their PeerConnection.",
			ingressFromNamespace("allow-local-agent-ingress", oramav1alpha1.PixieVizierNamespace,
				map[string]string{pixieComponentLabel: "vizier-cloud-connector"},
				podSelector(map[string]string{pixieComponentLabel: "vzconn-server"}), pixieVzconnPort)),
	}
	if operatorNamespace != "" {
		operator := map[string]string{"app.kubernetes.io/name": "orama-operator"}
		policies = append(policies,
			sandboxAllowance("The operator bootstraps the hub: the administrator in Kratos, the organisation in Postgres, deploy keys through the API, the metadata index in OpenSearch.",
				networkPolicy("allow-operator-bootstrap", everyPod, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					[]networkingv1.NetworkPolicyIngressRule{
						{From: []networkingv1.NetworkPolicyPeer{namespacePodPeer(operatorNamespace, operator)}, Ports: tcpPorts(4434, 5432, pixieSearchPort, 51200)},
					}, nil)))
	}
	return policies, nil
}

func podSelector(labels map[string]string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: labels}
}

// reconcilePixieSandbox writes the namespace's sandbox, keeping the allowances its PeerConnections
// added.
func reconcilePixieSandbox(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	policies []oramav1alpha1.NetworkSandboxPolicy,
	cilium []oramav1alpha1.NetworkSandboxCiliumPolicy,
) error {
	names, err := peerAllowanceNames(ctx, c, owner.GetNamespace())
	if err != nil {
		return err
	}
	kept, err := existingSandboxAllowances(ctx, c, owner.GetNamespace(), names...)
	if err != nil {
		return err
	}
	policies = append(policies, kept...)
	sandbox := &oramav1alpha1.NetworkSandbox{ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: owner.GetNamespace()}}
	_, err = controllerutil.CreateOrUpdate(ctx, c, sandbox, func() error {
		if current := metav1.GetControllerOf(sandbox); current != nil && !metav1.IsControlledBy(sandbox, owner) {
			return fmt.Errorf("NetworkSandbox %s/%s is controlled by %s %s", sandbox.Namespace, networkSandboxName, current.Kind, current.Name)
		}
		sandbox.Spec = oramav1alpha1.NetworkSandboxSpec{
			PodSelector:    everyPod,
			PolicyLabels:   map[string]string{pixieManagedLabel: owner.GetLabels()[pixieManagedLabel]},
			Policies:       policies,
			CiliumPolicies: cilium,
		}
		return controllerutil.SetControllerReference(owner, sandbox, scheme)
	})
	if err != nil {
		return fmt.Errorf("reconcile NetworkSandbox %s/%s: %w", sandbox.Namespace, networkSandboxName, err)
	}
	_, _, err = reconcileNetworkSandboxPolicies(ctx, c, scheme, sandbox)
	return err
}
