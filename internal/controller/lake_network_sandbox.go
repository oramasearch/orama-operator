/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const lakeInstanceLabel = "orama.cloud/lake-instance"

func lakeInstancePodLabels(lake *oramav1alpha1.Lake) map[string]string {
	return map[string]string{lakeInstanceLabel: lake.Name}
}

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
	agentSelector := metav1.LabelSelector{MatchLabels: lakeInstancePodLabels(lake), MatchExpressions: []metav1.LabelSelectorRequirement{
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
			"Every Lake-owned workload needs cluster DNS over UDP and TCP for Service discovery.",
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
			"Lake reaches workspace Zetta on 4800 and agents plus SQLite, DuckDB and Polars HTTP wrappers on 8080.",
			networkPolicy(
				"allow-lake-to-workspaces",
				lakeSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: workspaceNamespaces, PodSelector: &metav1.LabelSelector{}}}, Ports: tcpPorts(zettaAPIPort, agentPort)}},
			),
		),
		sandboxAllowance(
			"Lake sends predictions to its served methodology models' serve pods, the other half of methodology-serve-ingress (lake#690, lake#802).",
			networkPolicy(
				"allow-lake-to-methodology-serve",
				lakeSelector,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceNameSelector(methodologyRunsNamespace(lake.Name)),
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						methodologyRunLakeLabel: lake.Name, methodologyRunRoleLabel: methodologyRoleServe,
					}},
				}}, Ports: tcpPorts(methodologyServePort)}},
			),
		),
	}
	allowances = append(allowances, lakeInternalIngressAllowances(lake, lakeSelector, cnpgSelector, zettaSelector, agentSelector, schemaSelector)...)
	allowances = append(allowances, lakeWorkspaceServiceAllowances(lakeSelector, workspaceNamespaces)...)
	appAllowances, appErr := r.lakeAppProxyAllowances(ctx, lake, lakeSelector)
	if appErr != nil {
		preserved, preserveErr := existingSandboxAllowances(ctx, r.Client, namespace, "allow-lake-to-apps")
		appAllowances = preserved
		appErr = errors.Join(appErr, preserveErr)
	}
	allowances = append(allowances, appAllowances...)

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
		"ingress-nginx and zetta-tunnel's ingress gateway need Lake's HTTP port for the Lake Ingress backend; ingress-nginx also for LakeApp auth subrequests.",
		networkPolicy(
			"allow-ingress-nginx",
			lakeSelector,
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			[]networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{namespacePodPeer("ingress-nginx", map[string]string{"app.kubernetes.io/name": "ingress-nginx"}), zettaTunnelIngressPeer()},
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
		metav1.LabelSelector{MatchLabels: lakeInstancePodLabels(lake)},
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
	return errors.Join(apiErr, internetErr, appErr, reconcileErr)
}

// Both ends of a connection need an allowance when the source and destination are isolated.
func lakeInternalIngressAllowances(
	lake *oramav1alpha1.Lake,
	lakePods, databasePods, zettaPods, agentPods, schemaPods metav1.LabelSelector,
) []oramav1alpha1.NetworkSandboxPolicy {
	paths := []struct {
		name    string
		comment string
		pods    metav1.LabelSelector
		from    []networkingv1.NetworkPolicyPeer
		port    int32
	}{
		{"allow-lake-clients-to-controldb", "Lake and its PostgresSchema Job need ingress to their control database.", databasePods,
			[]networkingv1.NetworkPolicyPeer{{PodSelector: &lakePods}, {PodSelector: &schemaPods}}, 5432},
		{"allow-lake-ingress-to-zetta", "Lake needs ingress to its owned Zetta data-plane API.", zettaPods,
			[]networkingv1.NetworkPolicyPeer{{PodSelector: &lakePods}}, zettaAPIPort},
		{"allow-lake-ingress-to-agents", "Library agents need to accept calls from their owning Lake.", agentPods,
			[]networkingv1.NetworkPolicyPeer{{PodSelector: &lakePods}}, agentPort},
		{"allow-agent-ingress-to-lake", "Lake needs to accept API calls from its library agents.", lakePods,
			[]networkingv1.NetworkPolicyPeer{{PodSelector: &agentPods}}, int32(lakeContainerPort(lake))},
		{"allow-build-ingress-to-lake", "BuildKit Jobs fetch source, push registry images and publish build events to their owning Lake; Lake does not dial BuildKit.", lakePods,
			[]networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{buildLakeLabel: lake.Name}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{buildLakeLabel: lake.Name, "app.kubernetes.io/name": "lake-builder"}},
			}}, int32(lakeContainerPort(lake))},
		// The other half of methodology-transfer, the egress policy in the methodology-runs
		// namespace (lake#802): without it the stage pod's calls time out and every run fails.
		{"allow-methodology-ingress-to-lake", "Methodology stage and collect pods (runs and served models) page data in from, and report results to, their owning Lake; Lake never dials them.", lakePods,
			[]networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: namespaceNameSelector(methodologyRunsNamespace(lake.Name)),
				PodSelector:       methodologyTransferPods(lake),
			}}, int32(lakeContainerPort(lake))},
		// The LakeE2ETest runner (orama-operator#537, #589) is a Job in this Lake's namespace. It
		// is not one of the Lake's pods, so its own egress is not sandboxed; the Lake's ingress is.
		{"allow-e2e-ingress-to-lake", "The LakeE2ETest runner Job in this Lake's namespace calls the Lake API (REST, MCP and liquid sync) as a freshly signed-up test person.", lakePods,
			[]networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(lakeE2ELabel)}}}},
			int32(lakeContainerPort(lake))},
		// A LakeRestoreRehearsal's read-only verifier compares the live control database with the
		// restored one (a Job in the rehearsal's scratch namespace) or checks it after a failover (a
		// Job in this namespace). Without this every Postgres rehearsal on a sandboxed Lake fails at
		// "read-only rehearsal login is unavailable". Only verifier pods of this Lake are let in,
		// from this namespace or from a scratch namespace the operator labelled for this Lake.
		{"allow-rehearsal-ingress-to-controldb", "LakeRestoreRehearsal verifier Jobs read the control database with the read-only rehearsal login, from this namespace or from this Lake's scratch namespace.", databasePods,
			[]networkingv1.NetworkPolicyPeer{
				{PodSelector: &metav1.LabelSelector{MatchLabels: lakeRehearsalPodLabels(lake.Name)}},
				{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{lakeRehearsalLakeLabel: lake.Name}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: lakeRehearsalPodLabels(lake.Name)},
				},
			}, 5432},
	}
	policies := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(paths))
	for _, path := range paths {
		policies = append(policies, sandboxAllowance(path.comment, networkPolicy(
			path.name, path.pods, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			[]networkingv1.NetworkPolicyIngressRule{{From: path.from, Ports: tcpPorts(path.port)}}, nil,
		)))
	}
	return policies
}

// Lake receives these connection endpoints in LAKE_SERVICE_DIR (Data / Connections and
// service_connection checks). Select vendor pod identities only inside this Lake's workspaces.
func lakeWorkspaceServiceAllowances(lakePods metav1.LabelSelector, namespaces *metav1.LabelSelector) []oramav1alpha1.NetworkSandboxPolicy {
	policies := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(workspaceServicePaths))
	for _, path := range workspaceServicePaths {
		policies = append(policies, sandboxAllowance(
			"Lake uses workspace "+path.kind+" connection endpoints from its services Secret for Data / Connections and service_connection checks.",
			networkPolicy("allow-lake-to-workspace-"+path.kind, lakePods, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil,
				[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaces, PodSelector: &path.pods}}, Ports: tcpPorts(path.ports...)}},
			),
		))
	}
	return policies
}

func lakeRequestsForAppSandbox(_ context.Context, object client.Object) []reconcile.Request {
	app, ok := object.(*oramav1alpha1.LakeApp)
	if !ok || app.Spec.LakeRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: app.Spec.LakeRef}}}
}

func (r *LakeReconciler) lakeAppProxyAllowances(ctx context.Context, lake *oramav1alpha1.Lake, pods metav1.LabelSelector) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	var apps oramav1alpha1.LakeAppList
	if err := r.List(ctx, &apps, client.InNamespace(lakeNamespace(lake))); err != nil {
		return nil, err
	}
	sort.Slice(apps.Items, func(i, j int) bool {
		return apps.Items[i].Name < apps.Items[j].Name
	})
	var rules []networkingv1.NetworkPolicyEgressRule
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.LakeRef != lake.Name || app.Spec.Port < 1 || app.Spec.Port > 65535 {
			continue
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{namespacePodPeer(lakeAppNamespace(app), lakeAppLabels(app))},
			Ports: tcpPorts(app.Spec.Port),
		})
	}
	if len(rules) == 0 {
		return nil, nil
	}
	return []oramav1alpha1.NetworkSandboxPolicy{sandboxAllowance(
		"Lake may proxy its LakeApp backends; inferred from each app's declared backend namespace, pod identity and container port.",
		networkPolicy("allow-lake-to-apps", pods, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil, rules),
	)}, nil
}
