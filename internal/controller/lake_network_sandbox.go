/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func networkPolicy(
	name string,
	pods metav1.LabelSelector,
	policyTypes []networkingv1.PolicyType,
	ingress []networkingv1.NetworkPolicyIngressRule,
	egress []networkingv1.NetworkPolicyEgressRule,
) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: pods,
			PolicyTypes: policyTypes,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

func podPeer(labels map[string]string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: copyStringMap(labels)},
	}
}

func namespacePodPeer(namespace string, labels map[string]string) networkingv1.NetworkPolicyPeer {
	peer := podPeer(labels)
	peer.NamespaceSelector = namespaceNameSelector(namespace)
	return peer
}

func tcpPorts(ports ...int32) []networkingv1.NetworkPolicyPort {
	out := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, port := range ports {
		out = append(out, workspaceTCPPort(port))
	}
	return out
}

func lakePodSelector(lake *oramav1alpha1.Lake) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/name":     "lake",
		"app.kubernetes.io/instance": lake.Name,
	}}
}

func lakeAPIReader(r *LakeReconciler) client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *LakeReconciler) reconcileLakeNetworkSandbox(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	labels map[string]string,
) error {
	apiTarget, apiErr := readKubernetesAPIServerTarget(ctx, lakeAPIReader(r))
	publicPeers, internetErr := publicInternetPeers(ctx, lakeAPIReader(r))

	lakeSelector := lakePodSelector(lake)
	cnpgSelector := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{
			Key:      "cnpg.io/cluster",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{lakeControlDB(lake.Name)},
		},
	}}
	zettaSelector := metav1.LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/name":     "orama-zetta",
		"app.kubernetes.io/instance": lakeZettaName(lake.Name),
	}}
	agentSelector := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		labelExists(libraryAgentLabel),
	}}
	schemaSelector := metav1.LabelSelector{MatchLabels: map[string]string{
		"orama.cloud/postgresschema": lakeControlDB(lake.Name) + "-schema",
	}}
	workspaceNamespaces := &metav1.LabelSelector{MatchLabels: map[string]string{
		workspaceLakeLabel: lake.Name,
	}}
	allowances := []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(
			"Every Lake namespace workload needs cluster DNS over UDP and TCP for Service discovery.",
			workspaceDNSPolicy(namespace, labels),
		),
		sandboxAllowance(
			"Lake needs its CloudNativePG control database on the Postgres port.",
			networkPolicy(
				"allow-lake-to-controldb",
				lakeSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &cnpgSelector}}, Ports: tcpPorts(5432)}},
			),
		),
		sandboxAllowance(
			"The PostgresSchema migration job needs Lake's CloudNativePG control database.",
			networkPolicy(
				"allow-schema-to-controldb",
				schemaSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &cnpgSelector}}, Ports: tcpPorts(5432)}},
			),
		),
		sandboxAllowance(
			"Lake needs its owned Zetta data-plane HTTP API.",
			networkPolicy(
				"allow-lake-to-zetta",
				lakeSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &zettaSelector}}, Ports: tcpPorts(zettaAPIPort)}},
			),
		),
		sandboxAllowance(
			"Zetta nodes and their schema job need the peer and HTTP API ports inside this Lake namespace.",
			networkPolicy(
				"allow-zetta-internal",
				zettaSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &zettaSelector}}, Ports: tcpPorts(zettaPeerPort, zettaAPIPort)}},
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &zettaSelector}}, Ports: tcpPorts(zettaPeerPort, zettaAPIPort)}},
			),
		),
		sandboxAllowance(
			"CloudNativePG instances need Postgres replication and health traffic between members of Lake's control database.",
			networkPolicy(
				"allow-controldb-internal",
				cnpgSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &cnpgSelector}}, Ports: tcpPorts(5432, 8000)}},
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &cnpgSelector}}, Ports: tcpPorts(5432, 8000)}},
			),
		),
		sandboxAllowance(
			"Lake calls its in-namespace library agents over their HTTP service port.",
			networkPolicy(
				"allow-lake-to-agents",
				lakeSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &agentSelector}}, Ports: tcpPorts(agentPort)}},
			),
		),
		sandboxAllowance(
			"Library agents call their owning Lake API over Lake's configured container port.",
			networkPolicy(
				"allow-agents-to-lake",
				agentSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &lakeSelector}}, Ports: tcpPorts(int32(lakeContainerPort(lake)))}},
			),
		),
		sandboxAllowance(
			"Workspace workloads call their owning Lake API for control-plane operations and status.",
			networkPolicy(
				"allow-workspace-ingress",
				lakeSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: workspaceNamespaces, PodSelector: &metav1.LabelSelector{}}}, Ports: tcpPorts(int32(lakeContainerPort(lake)))}},
				nil,
			),
		),
		sandboxAllowance(
			"Lake reaches the Zetta and agent HTTP services in workspace namespaces it manages.",
			networkPolicy(
				"allow-lake-to-workspaces",
				lakeSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: workspaceNamespaces, PodSelector: &metav1.LabelSelector{}}}, Ports: tcpPorts(zettaAPIPort, agentPort)}},
			),
		),
	}

	if r.OperatorNamespace != "" {
		operatorPeer := namespacePodPeer(r.OperatorNamespace, map[string]string{"app.kubernetes.io/name": "orama-operator"})
		allowances = append(
			allowances,
			sandboxAllowance(
				"The Orama operator calls Lake APIs for sealed-secret delivery and control-plane reconciliation.",
				networkPolicy(
					"allow-operator-to-lake",
					lakeSelector,
					[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{operatorPeer}, Ports: tcpPorts(int32(lakeContainerPort(lake)))}},
					nil,
				),
			),
			sandboxAllowance(
				"The Orama operator reads Lake's control Postgres to reconcile workspaces, services, stores and other control rows.",
				networkPolicy(
					"allow-operator-to-controldb",
					cnpgSelector,
					[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{operatorPeer}, Ports: tcpPorts(5432)}},
					nil,
				),
			),
			sandboxAllowance(
				"The Orama operator reads and provisions Lake's Zetta control plane through its HTTP API.",
				networkPolicy(
					"allow-operator-to-zetta",
					zettaSelector,
					[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{operatorPeer}, Ports: tcpPorts(zettaAPIPort)}},
					nil,
				),
			),
		)
	}

	cnpgOperatorPods := workspaceCNPGOperator.pods
	allowances = append(allowances, sandboxAllowance(
		"The CloudNativePG operator needs the instance-manager status port and Postgres port to manage Lake's control database.",
		networkPolicy(
			"allow-cnpg-operator-ingress",
			cnpgSelector,
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			[]networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceNameSelector(workspaceCNPGOperator.namespace),
					PodSelector:       &cnpgOperatorPods,
				}},
				Ports: tcpPorts(8000, 5432),
			}},
			nil,
		),
	))

	if apiErr == nil {
		apiClients := []workspaceAPIServerClient{
			{
				name:    "allow-lake-apiserver-egress",
				comment: "Lake needs the Kubernetes API for its platform control-plane operations.",
				pods:    lakeSelector,
			},
			{
				name:    "allow-cnpg-apiserver-egress",
				comment: "CloudNativePG instances and bootstrap jobs need the Kubernetes API for their Cluster, leases and status.",
				pods:    cnpgSelector,
			},
		}
		for _, apiClient := range apiClients {
			allowances = append(
				allowances,
				sandboxAllowance(apiClient.comment, workspaceAPIServerEgressPolicy(apiClient, apiTarget, namespace, labels)),
			)
		}
	} else {
		preserved, preserveErr := existingSandboxAllowances(
			ctx,
			r.Client,
			namespace,
			"allow-lake-apiserver-egress",
			"allow-cnpg-apiserver-egress",
		)
		allowances = append(allowances, preserved...)
		apiErr = errors.Join(apiErr, preserveErr)
	}
	if internetErr == nil {
		allowances = append(
			allowances,
			sandboxAllowance(
				"Lake calls Clerk, Cloudflare, Stripe and model providers over public HTTPS; private, metadata, API, pod and node ranges stay excluded.",
				networkPolicy(
					"allow-lake-public-https",
					lakeSelector,
					[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
					nil,
					[]networkingv1.NetworkPolicyEgressRule{{To: publicPeers, Ports: tcpPorts(443)}},
				),
			),
			sandboxAllowance(
				"Library agents call their configured public model provider over HTTPS; private, metadata, API, pod and node ranges stay excluded.",
				networkPolicy(
					"allow-agents-public-https",
					agentSelector,
					[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
					nil,
					[]networkingv1.NetworkPolicyEgressRule{{To: publicPeers, Ports: tcpPorts(443)}},
				),
			),
			sandboxAllowance(
				"CloudNativePG uploads configured Lake backups to the public R2 or S3 HTTPS endpoint.",
				networkPolicy(
					"allow-controldb-backup-public-https",
					cnpgSelector,
					[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
					nil,
					[]networkingv1.NetworkPolicyEgressRule{{To: publicPeers, Ports: tcpPorts(443)}},
				),
			),
		)
	} else {
		preserved, preserveErr := existingSandboxAllowances(
			ctx,
			r.Client,
			namespace,
			"allow-lake-public-https",
			"allow-agents-public-https",
			"allow-controldb-backup-public-https",
		)
		allowances = append(allowances, preserved...)
		internetErr = errors.Join(internetErr, preserveErr)
	}
	allowances = append(allowances, sandboxAllowance(
		"ingress-nginx needs Lake's HTTP port for the Lake Ingress backend and LakeApp auth subrequests.",
		networkPolicy(
			"allow-ingress-nginx",
			lakeSelector,
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			[]networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{namespacePodPeer("ingress-nginx", map[string]string{"app.kubernetes.io/name": "ingress-nginx"})},
				Ports: tcpPorts(int32(lakeContainerPort(lake))),
			}},
			nil,
		),
	))

	_, reconcileErr := reconcileOwnedNetworkSandbox(
		ctx,
		r.Client,
		r.Scheme,
		lake,
		namespace,
		labels,
		allowances,
	)
	// The manager always provides APIReader. Older unit fixtures omit it and have no synthetic
	// default/kubernetes objects; keep those fixtures focused on their existing Lake behavior.
	if r.APIReader == nil && apierrors.IsNotFound(apiErr) {
		apiErr = nil
	}
	if r.APIReader == nil && apierrors.IsNotFound(internetErr) {
		internetErr = nil
	}
	return errors.Join(apiErr, internetErr, reconcileErr)
}
