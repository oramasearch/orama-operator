/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func oramaCloudAPIReader(r *OramaCloudV1Reconciler) client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func oramaCloudComponentSelector(oc *oramav1alpha1.OramaCloudV1, components ...string) metav1.LabelSelector {
	selector := metav1.LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/name":     appName,
		"app.kubernetes.io/instance": oc.Name,
	}}
	if len(components) == 1 {
		selector.MatchLabels["app.kubernetes.io/component"] = components[0]
		return selector
	}
	selector.MatchExpressions = []metav1.LabelSelectorRequirement{{
		Key:      "app.kubernetes.io/component",
		Operator: metav1.LabelSelectorOpIn,
		Values:   append([]string(nil), components...),
	}}
	return selector
}

func selectorPeer(selector metav1.LabelSelector) networkingv1.NetworkPolicyPeer {
	copy := selector.DeepCopy()
	return networkingv1.NetworkPolicyPeer{PodSelector: copy}
}

func egressTo(selector metav1.LabelSelector, ports ...int32) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{selectorPeer(selector)},
		Ports: tcpPorts(ports...),
	}
}

func ingressFrom(selector metav1.LabelSelector, ports ...int32) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{selectorPeer(selector)},
		Ports: tcpPorts(ports...),
	}
}

func oramaCloudEgressPolicy(
	name string,
	source metav1.LabelSelector,
	rules ...networkingv1.NetworkPolicyEgressRule,
) *networkingv1.NetworkPolicy {
	return networkPolicy(
		name,
		source,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		nil,
		rules,
	)
}

func oramaCloudIngressPolicy(
	name string,
	target metav1.LabelSelector,
	rules ...networkingv1.NetworkPolicyIngressRule,
) *networkingv1.NetworkPolicy {
	return networkPolicy(
		name,
		target,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		rules,
		nil,
	)
}

func (r *OramaCloudV1Reconciler) reconcileNetworkSandbox(
	ctx context.Context,
	oc *oramav1alpha1.OramaCloudV1,
) error {
	reader := oramaCloudAPIReader(r)
	apiTarget, apiErr := readKubernetesAPIServerTarget(ctx, reader)
	publicPeers, internetErr := publicInternetPeers(ctx, reader)
	migrations, err := targetingMigrations(ctx, r.Client, oc)
	if err != nil {
		return err
	}

	writer := oramaCloudComponentSelector(oc, "writer")
	readerPods := oramaCloudComponentSelector(oc, "reader")
	engine := oramaCloudComponentSelector(oc, "writer", "reader")
	embeddings := oramaCloudComponentSelector(oc, "embeddings")
	broker := oramaCloudComponentSelector(oc, "broker")
	controlDB := oramaCloudComponentSelector(oc, "controldb")
	cache := oramaCloudComponentSelector(oc, "cache")
	redisHTTP := oramaCloudComponentSelector(oc, redisHTTPComponent)
	analytics := oramaCloudComponentSelector(oc, "analytics")
	search := oramaCloudComponentSelector(oc, "search")
	dashboard := oramaCloudComponentSelector(oc, "dashboard")
	docs := oramaCloudComponentSelector(oc, "docs")
	workers := oramaCloudComponentSelector(oc, "workers")
	indexBuilder := oramaCloudComponentSelector(oc, "index-builder")
	harvestMaster := oramaCloudComponentSelector(oc, "harvest-master")
	harvestMongo := oramaCloudComponentSelector(oc, "harvest-mongo")
	selenium := oramaCloudComponentSelector(oc, "selenium")
	disco := oramaCloudComponentSelector(oc, "disco")
	pgadmin := oramaCloudComponentSelector(oc, "pgadmin")
	keycloak := oramaCloudComponentSelector(oc, "keycloak")
	analyticsProcessor := oramaCloudComponentSelector(oc, "analytics-processor")
	// #421's Jobs: the import CronJob calls the processor; the retention Job sets ClickHouse's TTL over
	// the native protocol (#616).
	analyticsImport := oramaCloudComponentSelector(oc, analyticsImportComponent)
	analyticsRetention := oramaCloudComponentSelector(oc, "analytics-retention")
	searchWorkers := oramaCloudComponentSelector(
		oc,
		"index-shared-worker",
		"large-index-worker",
		"openai-decrypter",
		"telemetry-worker",
	)
	openAIDecrypter := oramaCloudComponentSelector(oc, "openai-decrypter")
	// OramaCloudV1Migration Jobs (operator#653).
	migration := oramaCloudComponentSelector(oc, migrationComponent)
	// Every control DB schema's migration Job, not only the first: each PostgresSchema labels its pods
	// with its own name (operator#625).
	schemaNames := make([]string, 0, len(controlDBSchemaSuffixes))
	for _, suffix := range controlDBSchemaSuffixes {
		schemaNames = append(schemaNames, oc.Name+suffix)
	}
	postgresSchema := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "orama.cloud/postgresschema", Operator: metav1.LabelSelectorOpIn, Values: schemaNames},
	}}
	// The cluster-row Job inserts the stack's c1 row (operator#648). It is not a PostgresSchema Job,
	// so it has its own label; the instance label keeps another stack's Job out.
	clusterRowJob := metav1.LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/name":     appName,
		"app.kubernetes.io/instance": oc.Name,
		controlDBClientLabel:         controlDBClusterRowClient,
	}}
	cnpgPods := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{
			Key:      "cnpg.io/cluster",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{oc.Name + "-controldb"},
		},
	}}

	allowances := []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(
			"Every Cloud v1 workload needs cluster DNS over UDP and TCP for Service discovery.",
			workspaceDNSPolicy(oc.Namespace, labelsFor(oc, "network-sandbox")),
		),
		sandboxAllowance(
			"OramaCore writers and readers use RabbitMQ streams for replication and the shared embeddings gRPC service.",
			oramaCloudEgressPolicy(
				"allow-engine-egress",
				engine,
				egressTo(broker, 5552),
				egressTo(embeddings, 50051),
			),
		),
		sandboxAllowance(
			"RabbitMQ accepts stream replication from OramaCore and AMQP jobs from workers and HARVEST.",
			oramaCloudIngressPolicy(
				"allow-broker-ingress",
				broker,
				ingressFrom(engine, 5552),
				ingressFrom(oramaCloudComponentSelector(oc, "workers", "harvest-master"), 5672),
			),
		),
		sandboxAllowance(
			"The shared embeddings service accepts gRPC from OramaCore and DISCO.",
			oramaCloudIngressPolicy(
				"allow-embeddings-ingress",
				embeddings,
				ingressFrom(engine, 50051),
				ingressFrom(disco, 50051),
			),
		),
		sandboxAllowance(
			"The dashboard uses control Postgres, cache, its REST shim, analytics, search, OramaCore and Keycloak.",
			oramaCloudEgressPolicy(
				"allow-dashboard-egress",
				dashboard,
				egressTo(controlDB, 5432),
				egressTo(cache, 6379),
				egressTo(redisHTTP, redisHTTPPort),
				egressTo(analytics, 8123),
				egressTo(search, 8108),
				egressTo(writer, 8080),
				egressTo(readerPods, 80, 8080),
				egressTo(keycloak, 8080),
			),
		),
		sandboxAllowance(
			"The bundled workers use RabbitMQ, cache and OramaCore for queued application work.",
			oramaCloudEgressPolicy(
				"allow-workers-egress",
				workers,
				egressTo(broker, 5672),
				egressTo(cache, 6379),
				egressTo(writer, 8080),
				egressTo(readerPods, 80, 8080),
			),
		),
		sandboxAllowance(
			"The analytics processor pulls search events from every reader (#419, #421), writes ClickHouse and control Postgres.",
			oramaCloudEgressPolicy(
				"allow-analytics-processor-egress",
				analyticsProcessor,
				egressTo(analytics, 8123),
				egressTo(writer, 8080),
				egressTo(readerPods, 8080),
				egressTo(controlDB, 5432),
			),
		),
		sandboxAllowance(
			"The analytics import CronJob calls the analytics processor's import and aggregate endpoints (#421).",
			oramaCloudEgressPolicy("allow-analytics-import-egress", analyticsImport, egressTo(analyticsProcessor, 8080)),
		),
		sandboxAllowance(
			"The analytics processor accepts the import CronJob's calls (#421).",
			oramaCloudIngressPolicy("allow-analytics-processor-ingress", analyticsProcessor, ingressFrom(analyticsImport, 8080)),
		),
		sandboxAllowance(
			"The analytics retention Job sets ClickHouse's 30-day TTL with clickhouse-client (#421, #616).",
			oramaCloudEgressPolicy("allow-analytics-retention-egress", analyticsRetention, egressTo(analytics, 9000)),
		),
		sandboxAllowance(
			"Index builder uses dashboard, cache, control Postgres, DISCO, search, decrypter and HARVEST APIs.",
			oramaCloudEgressPolicy(
				"allow-index-builder-egress",
				indexBuilder,
				egressTo(dashboard, 3000),
				egressTo(cache, 6379),
				egressTo(controlDB, 5432),
				egressTo(disco, 8080, 50051),
				egressTo(search, 8108),
				egressTo(openAIDecrypter, 8080),
				egressTo(harvestMaster, 8080),
			),
		),
		sandboxAllowance(
			"HARVEST master uses its Mongo record store, RabbitMQ, cache and Selenium browser service.",
			oramaCloudEgressPolicy(
				"allow-harvest-egress",
				harvestMaster,
				egressTo(harvestMongo, 27017),
				egressTo(broker, 5672),
				egressTo(cache, 6379),
				egressTo(selenium, 4444),
			),
		),
		sandboxAllowance(
			"DISCO calls the shared embeddings gRPC service.",
			oramaCloudEgressPolicy("allow-disco-egress", disco, egressTo(embeddings, 50051)),
		),
		sandboxAllowance(
			"pgAdmin connects to the control Postgres service.",
			oramaCloudEgressPolicy("allow-pgadmin-egress", pgadmin, egressTo(controlDB, 5432)),
		),
		sandboxAllowance(
			"Hosted-search workers use Typesense, OramaCore and ClickHouse.",
			oramaCloudEgressPolicy(
				"allow-search-workers-egress",
				searchWorkers,
				egressTo(search, 8108),
				egressTo(writer, 8080),
				egressTo(readerPods, 80, 8080),
				egressTo(analytics, 8123),
			),
		),
		sandboxAllowance(
			"Control Postgres accepts application, migration, cluster-row Job and replication clients declared by this stack.",
			oramaCloudIngressPolicy(
				"allow-controldb-ingress",
				cnpgPods,
				ingressFrom(oramaCloudComponentSelector(oc, "dashboard", "index-builder", "analytics-processor", "pgadmin"), 5432),
				ingressFrom(postgresSchema, 5432),
				ingressFrom(clusterRowJob, 5432),
				ingressFrom(cnpgPods, 5432, 8000),
			),
		),
		sandboxAllowance(
			"PostgresSchema applies the dashboard schema to control Postgres.",
			oramaCloudEgressPolicy("allow-postgres-schema-egress", postgresSchema, egressTo(cnpgPods, 5432)),
		),
		sandboxAllowance(
			"The cluster-row Job inserts the stack's c1 oramacore_clusters row into control Postgres.",
			oramaCloudEgressPolicy("allow-controldb-cluster-row-egress", clusterRowJob, egressTo(cnpgPods, 5432)),
		),
		sandboxAllowance(
			"Cache accepts Redis clients from dashboard, index builder, workers, HARVEST and the REST shim.",
			oramaCloudIngressPolicy(
				"allow-cache-ingress",
				cache,
				ingressFrom(oramaCloudComponentSelector(oc, "dashboard", "index-builder", "workers", "harvest-master", redisHTTPComponent), 6379),
			),
		),
		sandboxAllowance(
			"The serverless-redis-http shim connects to the cache (#562).",
			oramaCloudEgressPolicy("allow-redis-http-egress", redisHTTP, egressTo(cache, 6379)),
		),
		sandboxAllowance(
			"The serverless-redis-http shim accepts the dashboard's Upstash REST calls, and no one else's (#562).",
			oramaCloudIngressPolicy("allow-redis-http-ingress", redisHTTP, ingressFrom(dashboard, redisHTTPPort)),
		),
		sandboxAllowance(
			"ClickHouse accepts analytics writes and reads from dashboard, analytics processor, DISCO and hosted-search workers.",
			oramaCloudIngressPolicy(
				"allow-analytics-ingress",
				analytics,
				ingressFrom(oramaCloudComponentSelector(oc, "dashboard", "analytics-processor", "disco", "index-shared-worker", "large-index-worker", "telemetry-worker"), 8123),
				ingressFrom(analyticsRetention, 9000),
			),
		),
		sandboxAllowance(
			"Typesense accepts hosted-search traffic from dashboard, index builder and search workers.",
			oramaCloudIngressPolicy(
				"allow-search-ingress",
				search,
				ingressFrom(oramaCloudComponentSelector(oc, "dashboard", "index-builder", "index-shared-worker", "large-index-worker", "telemetry-worker"), 8108),
			),
		),
		sandboxAllowance(
			"OramaCore writer accepts API calls from dashboard, workers, analytics processor and hosted-search workers.",
			oramaCloudIngressPolicy(
				"allow-writer-ingress",
				writer,
				ingressFrom(oramaCloudComponentSelector(oc, "dashboard", "workers", "analytics-processor", "index-shared-worker", "large-index-worker", "telemetry-worker"), 8080),
			),
		),
		sandboxAllowance(
			"OramaCore readers accept queries from dashboard, workers and hosted-search workers, and the analytics processor's pulls (#419).",
			oramaCloudIngressPolicy(
				"allow-reader-ingress",
				readerPods,
				ingressFrom(oramaCloudComponentSelector(oc, "dashboard", "workers", "index-shared-worker", "large-index-worker", "telemetry-worker"), 80, 8080),
				ingressFrom(analyticsProcessor, 8080),
			),
		),
		sandboxAllowance(
			"Dashboard accepts index-builder deployment API calls.",
			oramaCloudIngressPolicy("allow-dashboard-internal-ingress", dashboard, ingressFrom(indexBuilder, 3000)),
		),
		sandboxAllowance(
			"DISCO accepts HTTP and embeddings requests from index builder.",
			oramaCloudIngressPolicy("allow-disco-ingress", disco, ingressFrom(indexBuilder, 8080, 50051)),
		),
		sandboxAllowance(
			"OpenAI decrypter accepts index-builder requests.",
			oramaCloudIngressPolicy("allow-openai-decrypter-ingress", openAIDecrypter, ingressFrom(indexBuilder, 8080)),
		),
		sandboxAllowance(
			"HARVEST master accepts crawl requests from index builder.",
			oramaCloudIngressPolicy("allow-harvest-master-ingress", harvestMaster, ingressFrom(indexBuilder, 8080)),
		),
		sandboxAllowance(
			"HARVEST Mongo accepts record-store traffic from HARVEST master.",
			oramaCloudIngressPolicy("allow-harvest-mongo-ingress", harvestMongo, ingressFrom(harvestMaster, 27017)),
		),
		sandboxAllowance(
			"Selenium accepts browser sessions from HARVEST master.",
			oramaCloudIngressPolicy("allow-selenium-ingress", selenium, ingressFrom(harvestMaster, 4444)),
		),
		sandboxAllowance(
			"Keycloak accepts identity requests from dashboard.",
			oramaCloudIngressPolicy("allow-keycloak-ingress", keycloak, ingressFrom(dashboard, 8080)),
		),
		sandboxAllowance(
			"OramaCloudV1Migration Jobs copy a Prod 1 team into control Postgres, the writer and the readers (operator#653); DNS is allow-dns-egress.",
			oramaCloudEgressPolicy(
				"allow-migration-egress",
				migration,
				egressTo(cnpgPods, 5432),
				egressTo(writer, oramacorePort),
				egressTo(readerPods, readerPodPort(oc)),
			),
		),
		sandboxAllowance(
			"Control Postgres accepts the migration role from OramaCloudV1Migration Jobs (operator#653).",
			oramaCloudIngressPolicy("allow-controldb-migration-ingress", cnpgPods, ingressFrom(migration, 5432)),
		),
		sandboxAllowance(
			"OramaCore writer accepts OramaCloudV1Migration Jobs' collection and document writes (operator#653).",
			oramaCloudIngressPolicy("allow-writer-migration-ingress", writer, ingressFrom(migration, oramacorePort)),
		),
		sandboxAllowance(
			"OramaCore readers accept OramaCloudV1Migration Jobs' verification reads (operator#653).",
			oramaCloudIngressPolicy("allow-reader-migration-ingress", readerPods, ingressFrom(migration, readerPodPort(oc))),
		),
		sandboxAllowance(
			"CloudNativePG instances replicate and exchange health traffic within this stack.",
			networkPolicy(
				"allow-controldb-internal",
				cnpgPods,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				[]networkingv1.NetworkPolicyIngressRule{ingressFrom(cnpgPods, 5432, 8000)},
				[]networkingv1.NetworkPolicyEgressRule{egressTo(cnpgPods, 5432, 8000)},
			),
		),
	}

	cnpgOperatorPods := workspaceCNPGOperator.pods
	allowances = append(allowances, sandboxAllowance(
		"The CloudNativePG operator needs the instance-manager status and Postgres ports to manage control Postgres.",
		oramaCloudIngressPolicy(
			"allow-cnpg-operator-ingress",
			cnpgPods,
			networkingv1.NetworkPolicyIngressRule{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceNameSelector(workspaceCNPGOperator.namespace),
					PodSelector:       &cnpgOperatorPods,
				}},
				Ports: tcpPorts(8000, 5432),
			},
		),
	))
	if apiErr == nil {
		apiClient := workspaceAPIServerClient{
			name:    "allow-cnpg-apiserver-egress",
			comment: "CloudNativePG instances and bootstrap jobs need the Kubernetes API for their Cluster, leases and status.",
			pods:    cnpgPods,
		}
		allowances = append(
			allowances,
			sandboxAllowance(apiClient.comment, workspaceAPIServerEgressPolicy(apiClient, apiTarget, oc.Namespace, nil)),
		)
	} else {
		preserved, preserveErr := existingSandboxAllowances(ctx, r.Client, oc.Namespace, "allow-cnpg-apiserver-egress")
		allowances = append(allowances, preserved...)
		apiErr = errors.Join(apiErr, preserveErr)
	}
	if writerJWKSFromDashboard(oc) {
		allowances = append(
			allowances,
			sandboxAllowance(
				"The OramaCore writer fetches the dashboard's JWKS to verify write tokens.",
				oramaCloudEgressPolicy("allow-writer-jwks-egress", writer, egressTo(dashboard, 3000)),
			),
			sandboxAllowance(
				"Dashboard serves its JWKS to the OramaCore writer for write-token verification.",
				oramaCloudIngressPolicy("allow-dashboard-jwks-ingress", dashboard, ingressFrom(writer, 3000)),
			),
		)
	}
	if r.OperatorNamespace != "" {
		allowances = append(allowances, sandboxAllowance(
			"RabbitMQTopology in the Orama operator calls the broker management API to reconcile streams, users and permissions.",
			oramaCloudIngressPolicy(
				"allow-operator-to-broker-management",
				broker,
				networkingv1.NetworkPolicyIngressRule{
					From:  []networkingv1.NetworkPolicyPeer{namespacePodPeer(r.OperatorNamespace, map[string]string{"app.kubernetes.io/name": "orama-operator"})},
					Ports: tcpPorts(15672),
				},
			),
		))
	}
	allowances = append(
		allowances,
		sandboxAllowance(
			"ingress-nginx and zetta-tunnel's ingress gateway need the dashboard HTTP port for its Ingress backend.",
			oramaCloudIngressPolicy("allow-dashboard-ingress-nginx", dashboard, ingressGatewayRule(3000)),
		),
		sandboxAllowance(
			"ingress-nginx and zetta-tunnel's ingress gateway need the docs HTTP port for its Ingress backend.",
			oramaCloudIngressPolicy("allow-docs-ingress-nginx", docs, ingressGatewayRule(3000)),
		),
	)
	// The public engine endpoints (cloudv1_networking.go): only while each is published, only to this
	// stack's writer or reader pods on the port behind its Ingress backend, and only from ingress-nginx:
	// Cloud v1 is never served by the zetta-tunnel gateway (bridge decisions of 2026-10-17, 19).
	if writerPublicHost(oc) != "" {
		allowances = append(allowances, sandboxAllowance(
			"ingress-nginx needs the writer API port for its public Ingress backend.",
			oramaCloudIngressPolicy("allow-writer-ingress-nginx", writer, ingressNginxRule(oramacorePort)),
		))
	}
	if readerPublicHost(oc) != "" {
		allowances = append(allowances, sandboxAllowance(
			"ingress-nginx needs the reader pod port behind the reader Service for its public Ingress backend.",
			oramaCloudIngressPolicy("allow-reader-ingress-nginx", readerPods, ingressNginxRule(readerPodPort(oc))),
		))
	}
	// Prod 1, only while a migration targets this stack (operator#653).
	prod1Hosts := migrationProd1Hosts(migrations)
	if internetErr == nil {
		allowances = append(allowances, oramaCloudPublicInternetAllowances(
			publicPeers,
			engine,
			dashboard,
			workers,
			indexBuilder,
			harvestMaster,
			selenium,
			embeddings,
			searchWorkers,
			cnpgPods,
		)...)
		if len(prod1Hosts) > 0 {
			allowances = append(allowances, migrationProd1Allowance(publicPeers, migration, prod1Hosts))
		}
	} else {
		publicPolicyNames := []string{
			"allow-engine-public-internet",
			"allow-dashboard-public-https",
			"allow-workers-public-https",
			"allow-index-builder-public-https",
			"allow-harvest-public-https",
			"allow-selenium-public-internet",
			"allow-embeddings-public-https",
			"allow-search-workers-public-https",
			"allow-controldb-primary-replication",
		}
		if len(prod1Hosts) > 0 {
			publicPolicyNames = append(publicPolicyNames, migrationProd1PolicyName)
		}
		preserved, preserveErr := existingSandboxAllowances(
			ctx,
			r.Client,
			oc.Namespace,
			publicPolicyNames...,
		)
		allowances = append(allowances, preserved...)
		internetErr = errors.Join(internetErr, preserveErr)
	}

	labels := labelsFor(oc, "network-sandbox")
	_, reconcileErr := reconcileOwnedNetworkSandbox(
		ctx,
		r.Client,
		r.Scheme,
		oc,
		oc.Namespace,
		labels,
		metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/instance":   oc.Name,
			"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		}},
		allowances,
	)
	// The manager always provides APIReader. Older unit fixtures omit it and have no synthetic
	// default/kubernetes objects; keep those fixtures focused on their existing stack behavior.
	if r.APIReader == nil && apierrors.IsNotFound(apiErr) {
		apiErr = nil
	}
	if r.APIReader == nil && apierrors.IsNotFound(internetErr) {
		internetErr = nil
	}
	return errors.Join(apiErr, internetErr, reconcileErr)
}

// migrationProd1PolicyName is the migration pods' egress to Prod 1.
const migrationProd1PolicyName = "allow-migration-prod1-egress"

// migrationProd1Allowance lets migration pods reach Prod 1: the database on 5432 and the engine
// origins and the SSM endpoint on 443. It is enforced by port and public address range, not by
// name, since Cilium's FQDN policy needs AKS Advanced Container Networking Services, which these
// clusters do not run; the comment names the hosts it is for.
func migrationProd1Allowance(publicPeers []networkingv1.NetworkPolicyPeer, migration metav1.LabelSelector, hosts []string) oramav1alpha1.NetworkSandboxPolicy {
	return sandboxAllowance(
		"OramaCloudV1Migration Jobs read Prod 1 at "+strings.Join(hosts, ", ")+
			" (operator#653), enforced by port and public range, not by name; private, metadata, API, pod and node ranges stay excluded.",
		oramaCloudEgressPolicy(migrationProd1PolicyName, migration, publicEgressRule(publicPeers, 443, 5432)),
	)
}

// ingressGatewayRule admits ingress-nginx and zetta-tunnel's ingress gateway, the two controllers
// an Ingress of this stack can be served by, on the given pod ports.
func ingressGatewayRule(ports ...int32) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{namespacePodPeer("ingress-nginx", map[string]string{"app.kubernetes.io/name": "ingress-nginx"}), zettaTunnelIngressPeer()},
		Ports: tcpPorts(ports...),
	}
}

// ingressNginxRule admits ingress-nginx alone on the given pod ports: the public engine hosts.
func ingressNginxRule(ports ...int32) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{namespacePodPeer("ingress-nginx", map[string]string{"app.kubernetes.io/name": "ingress-nginx"})},
		Ports: tcpPorts(ports...),
	}
}

func publicEgressRule(peers []networkingv1.NetworkPolicyPeer, ports ...int32) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To:    append([]networkingv1.NetworkPolicyPeer(nil), peers...),
		Ports: tcpPorts(ports...),
	}
}

func publicTCPAllPortsEgressRule(peers []networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: append([]networkingv1.NetworkPolicyPeer(nil), peers...),
		Ports: []networkingv1.NetworkPolicyPort{{
			Protocol: protocolPtr(corev1.ProtocolTCP),
		}},
	}
}

func oramaCloudPublicInternetAllowances(
	publicPeers []networkingv1.NetworkPolicyPeer,
	engine metav1.LabelSelector,
	dashboard metav1.LabelSelector,
	workers metav1.LabelSelector,
	indexBuilder metav1.LabelSelector,
	harvestMaster metav1.LabelSelector,
	selenium metav1.LabelSelector,
	embeddings metav1.LabelSelector,
	searchWorkers metav1.LabelSelector,
	cnpgPods metav1.LabelSelector,
) []oramav1alpha1.NetworkSandboxPolicy {
	return []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(
			"Customer JavaScript hooks in OramaCore may reach public addresses on any TCP port; private, metadata, API, pod and node ranges remain excluded.",
			oramaCloudEgressPolicy("allow-engine-public-internet", engine, publicTCPAllPortsEgressRule(publicPeers)),
		),
		sandboxAllowance(
			"Dashboard calls Clerk, Stripe, Resend and Slack over public HTTPS.",
			oramaCloudEgressPolicy("allow-dashboard-public-https", dashboard, publicEgressRule(publicPeers, 443)),
		),
		sandboxAllowance(
			"The workers bundle calls its configured public APIs over HTTPS.",
			oramaCloudEgressPolicy("allow-workers-public-https", workers, publicEgressRule(publicPeers, 443)),
		),
		sandboxAllowance(
			"Index builder calls Cloudflare, Apify and Ably over public HTTPS.",
			oramaCloudEgressPolicy("allow-index-builder-public-https", indexBuilder, publicEgressRule(publicPeers, 443)),
		),
		sandboxAllowance(
			"HARVEST master pulls crawl worker sources and images over public HTTPS.",
			oramaCloudEgressPolicy("allow-harvest-public-https", harvestMaster, publicEgressRule(publicPeers, 443)),
		),
		sandboxAllowance(
			"Selenium visits customer-requested public HTTP and HTTPS pages for HARVEST crawls.",
			oramaCloudEgressPolicy("allow-selenium-public-internet", selenium, publicEgressRule(publicPeers, 80, 443)),
		),
		sandboxAllowance(
			"Embeddings downloads configured public model artifacts over HTTPS.",
			oramaCloudEgressPolicy("allow-embeddings-public-https", embeddings, publicEgressRule(publicPeers, 443)),
		),
		sandboxAllowance(
			"Hosted-search workers call their configured public providers over HTTPS.",
			oramaCloudEgressPolicy("allow-search-workers-public-https", searchWorkers, publicEgressRule(publicPeers, 443)),
		),
		sandboxAllowance(
			"A read-region CloudNativePG replica reaches its configured primary Postgres endpoint on the public network; this path is inferred from spec.peers.postgresEndpoint.",
			oramaCloudEgressPolicy("allow-controldb-primary-replication", cnpgPods, publicEgressRule(publicPeers, 5432)),
		),
	}
}
