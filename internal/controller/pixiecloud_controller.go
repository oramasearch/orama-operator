/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// pixieAgentImportService is the Service, in each agent's namespace, that its PeerConnection
	// import of the hub's vzconn creates (peerImportServiceName of pixieAgentImport).
	pixieAgentImport        = "pixie-cloud"
	pixieAgentImportService = "peer-" + pixieAgentImport
	pixieAlertingCondition  = "Alerting"
	pixieDeployKeyLabel     = "orama.cloud/pixie-agent"
)

// PixieCloudReconciler runs self-hosted Pixie Cloud on the hub's cluster from a PixieCloud the
// managing cluster wrote (operator#15, #456): its certificates and keys, its CloudNativePG database
// (one role per service), OpenSearch, the PxL script server and the vendored services, behind a
// default-deny sandbox; then the bootstrap (pixie_cloud_bootstrap.go) and one deploy key per agent
// cluster the managing cluster accepted.
type PixieCloudReconciler struct {
	client.Client
	// APIReader reads the Kubernetes API Service and Endpoints for the sandbox, uncached.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// OperatorNamespace is the operator's own namespace, whose pod runs the bootstrap.
	OperatorNamespace string
	// Backend is injected for tests; nil connects to the hub's services.
	Backend func(pixieHubCredentials) (pixieHubBackend, error)
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=pixieclouds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=pixieclouds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=pixieclouds/finalizers,verbs=update
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules;podmonitors;alertmanagerconfigs,verbs=get;list;watch;create;update;patch;delete
// Pixie's own RBAC from the vendored bundles, scoped to its names (TestChartKarpenterRBACMatchesBundle).
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=create;update;patch;delete;bind;escalate,resourceNames={"pl-cloud-connector-role","pl-node-view","pl-vizier-metadata","plc:nats-server"}
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create;update;patch;delete,resourceNames={"pl-cloud-connector-cluster-binding","pl-node-view-cluster-binding","pl-vizier-metadata-cluster-binding","pl-vizier-metadata-node-view-cluster-binding","plc:nats-server-binding"}
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;update;patch;delete;bind;escalate,resourceNames=pl-cloud-connector-ns-role;pl-kuberesolver-role;pl-vizier-metadata-role;pl-vizier-query-broker-role
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;update;patch;delete,resourceNames=pl-cloud-connector-binding;pl-kuberesolver-role-binding;pl-vizier-metadata-binding;pl-vizier-query-broker-binding

func (r *PixieCloudReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *PixieCloudReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *PixieCloudReconciler) backend(creds pixieHubCredentials) (pixieHubBackend, error) {
	if r.Backend != nil {
		return r.Backend(creds)
	}
	return newPixieHubClient(creds)
}

func (r *PixieCloudReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cloud oramav1alpha1.PixieCloud
	if err := r.Get(ctx, req.NamespacedName, &cloud); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cloud.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&cloud, pixieFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := prunePixieObjects(ctx, r.Client, cloud.Namespace, pixieManagedCloud, nil); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&cloud, pixieFinalizer)
		return ctrl.Result{}, r.Update(ctx, &cloud)
	}
	if !controllerutil.ContainsFinalizer(&cloud, pixieFinalizer) {
		controllerutil.AddFinalizer(&cloud, pixieFinalizer)
		if err := r.Update(ctx, &cloud); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	status, alertingErr, err := r.reconcile(ctx, &cloud)
	if statusErr := r.setStatus(ctx, &cloud, status, alertingErr); statusErr != nil && err == nil {
		err = statusErr
	}
	return ctrl.Result{RequeueAfter: pixieRequeue}, err
}

func (r *PixieCloudReconciler) reconcile(ctx context.Context, cloud *oramav1alpha1.PixieCloud) (oramav1alpha1.PixieCloudStatus, error, error) {
	refuse := func(phase, message string) (oramav1alpha1.PixieCloudStatus, error, error) {
		return oramav1alpha1.PixieCloudStatus{Phase: phase, Message: message}, nil, nil
	}
	if cloud.Namespace != oramav1alpha1.PixieCloudNamespace {
		return refuse(oramav1alpha1.ObservabilityPhaseRefused, "a PixieCloud runs in namespace "+oramav1alpha1.PixieCloudNamespace)
	}
	if problems := pixieReleaseProblems(&cloud.Spec.Release); len(problems) > 0 {
		return refuse(oramav1alpha1.ObservabilityPhaseDegraded, problems[0])
	}
	var pull corev1.Secret
	if found, err := getIfExists(ctx, r.Client, cloud.Namespace, cloud.Spec.PullSecretName, &pull); err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	} else if !found {
		return refuse(oramav1alpha1.ObservabilityPhasePending, "waiting for the registry credentials (Secret "+cloud.Spec.PullSecretName+"), which the managing cluster writes")
	}
	material, err := r.ensureSecrets(ctx, cloud)
	if err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	}
	vendored, err := renderPixieCloud(pixieCloudRender{Cloud: cloud, CertsHash: material.certsHash, DatabaseHost: pixieDBHost()})
	if err != nil {
		return refuse(oramav1alpha1.ObservabilityPhaseDegraded, err.Error())
	}
	policies, err := pixieCloudSandboxPolicies(ctx, r.reader(), r.OperatorNamespace)
	if err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	}
	if err := reconcilePixieSandbox(ctx, r.Client, r.Scheme, cloud, policies, nil); err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	}
	// The stores before the services that need them.
	own := []client.Object{pixieDBClusterObject(cloud)}
	for _, database := range pixieDatabaseObjects() {
		own = append(own, database)
	}
	search := pixieSearchObjects(cloud, material.searchSecurity)
	scripts := pixieScriptsObjects(cloud)
	own = append(own, search...)
	own = append(own, scripts...)
	for _, object := range own {
		stampPixieObject(object, pixieManagedCloud, cloud.Namespace, cloud.Spec.PullSecretName)
	}
	if err := applyPixieObjects(ctx, r.Client, r.Scheme, cloud, pixieManagedCloud, append(own, vendored...)); err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	}
	alertingErr := reconcileObservabilityAlerting(ctx, r.Client, r.Scheme, cloud, cloud.Spec.Hub, cloud.Spec.AlertRoutes, pixieCloudAlertRules(cloud))
	if alertingErr != nil && !errors.Is(alertingErr, errMonitoringCRDsMissing) {
		return oramav1alpha1.PixieCloudStatus{}, nil, alertingErr
	}

	workloads := append(append([]client.Object{}, search...), scripts...)
	workloads = append(workloads, vendored...)
	components, err := pixieWorkloadStatus(ctx, r.Client, cloud.Namespace, workloads)
	if err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	}
	database, err := r.databaseStatus(ctx)
	if err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	}
	components = append(components, database)
	sort.Slice(components, func(i, j int) bool { return components[i].Name < components[j].Name })
	phase, ready, message := summarizeComponents(components)
	status := oramav1alpha1.PixieCloudStatus{Phase: phase, Ready: ready, Components: components, Message: message, CACertificate: string(material.cloudCA)}

	agents, bootstrapMessage, err := r.bootstrap(ctx, cloud, material, phase == oramav1alpha1.ObservabilityPhaseReady)
	if err != nil {
		return oramav1alpha1.PixieCloudStatus{}, nil, err
	}
	status.Agents = agents
	if status.Phase == oramav1alpha1.ObservabilityPhaseReady && bootstrapMessage != "" {
		status.Phase, status.Message = oramav1alpha1.ObservabilityPhaseProgressing, bootstrapMessage
	}
	return status, alertingErr, nil
}

// pixieCloudMaterial is what ensureSecrets minted and the rest of the pass needs.
type pixieCloudMaterial struct {
	certsHash      string
	cloudCA        []byte
	serviceCA      []byte
	searchSecurity map[string]string
	creds          pixieHubCredentials
}

// ensureSecrets mints the hub's CAs, certificates, signing keys, per-service database
// credentials, search users and administrator password.
func (r *PixieCloudReconciler) ensureSecrets(ctx context.Context, cloud *oramav1alpha1.PixieCloud) (pixieCloudMaterial, error) {
	labels := map[string]string{pixieManagedLabel: pixieManagedCloud}
	now := r.now()
	ns := cloud.Namespace
	serviceCA, err := ensurePixieCA(ctx, r.Client, r.Scheme, cloud, pixieServiceCASecret, "Pixie Cloud service CA "+cloud.Spec.Hub, labels, now)
	if err != nil {
		return pixieCloudMaterial{}, err
	}
	cloudCA, err := ensurePixieCA(ctx, r.Client, r.Scheme, cloud, pixieCloudCASecret, "Pixie Cloud CA "+cloud.Spec.Hub, labels, now)
	if err != nil {
		return pixieCloudMaterial{}, err
	}
	names := pixieCloudDNSNames(ns)
	var certs []*corev1.Secret
	for _, leaf := range []pixieLeaf{
		{Secret: pixieServerCertsSecret, CertKey: corev1.TLSCertKey, KeyKey: corev1.TLSPrivateKeyKey, CAKey: "ca.crt",
			Request: tunnelLeafRequest{CommonName: "pixie-cloud", DNSNames: names, Server: true, Client: true}},
		{Secret: pixieClientCertsSecret, CertKey: corev1.TLSCertKey, KeyKey: corev1.TLSPrivateKeyKey, CAKey: "ca.crt",
			Request: tunnelLeafRequest{CommonName: "pixie-cloud-client", DNSNames: names, Server: true, Client: true}},
		{Secret: pixieSearchNodeCerts, CertKey: corev1.TLSCertKey, KeyKey: corev1.TLSPrivateKeyKey, CAKey: "ca.crt", PKCS8: true,
			Request: tunnelLeafRequest{CommonName: "pixie-search", DNSNames: []string{
				pixieSearchService, pixieSearchService + "." + ns + ".svc", pixieSearchService + "." + ns + ".svc.cluster.local",
				"*." + pixieSearchService + "." + ns + ".svc.cluster.local",
			}, Server: true, Client: true}},
	} {
		secret, err := ensurePixieLeaf(ctx, r.Client, r.Scheme, cloud, serviceCA, leaf, labels, now)
		if err != nil {
			return pixieCloudMaterial{}, err
		}
		certs = append(certs, secret)
	}
	// What agents and people reach: the UI and vzconn. Every agent dials the same import name in
	// its own namespace, so one name covers them all.
	proxy, err := ensurePixieLeaf(ctx, r.Client, r.Scheme, cloud, cloudCA, pixieLeaf{
		Secret: pixieProxyCertsSecret, CertKey: corev1.TLSCertKey, KeyKey: corev1.TLSPrivateKeyKey,
		Request: tunnelLeafRequest{CommonName: cloud.Spec.Domain, DNSNames: pixieProxyDNSNames(cloud.Spec.Domain), Server: true},
	}, labels, now)
	if err != nil {
		return pixieCloudMaterial{}, err
	}
	certs = append(certs, proxy)

	for _, minted := range []struct {
		name string
		keys []pixieSecretKey
	}{
		{pixieAuthSecret, []pixieSecretKey{randomKey("jwt-signing-key", 32)}},
		{pixieSessionSecret, []pixieSecretKey{randomKey("session-key", 32)}},
		{pixieHydraSecret, []pixieSecretKey{randomKey("CLIENT_SECRET", 32), randomKey("OIDC_SUBJECT_IDENTIFIERS_PAIRWISE_SALT", 32), randomKey("SECRETS_SYSTEM", 32)}},
		// No outside identity provider: the keys exist, empty, because the auth service reads them.
		{pixieOIDCSecret, []pixieSecretKey{fixedKey("oidc-client-id", ""), fixedKey("oidc-client-secret", "")}},
		{pixieAdminSecret, []pixieSecretKey{fixedKey("email", cloud.Spec.AdminEmail), randomKey("password", 24)}},
		{pixieSearchSecret, []pixieSecretKey{
			randomKey("indexer-password", 24), bcryptOf("indexer-hash", "indexer-password"),
			randomKey("api-password", 24), bcryptOf("api-hash", "api-password"),
		}},
	} {
		if _, err := ensureMintedSecret(ctx, r.Client, r.Scheme, cloud, minted.name, corev1.SecretTypeOpaque, labels, minted.keys...); err != nil {
			return pixieCloudMaterial{}, err
		}
	}
	databases := map[string]*corev1.Secret{}
	for _, db := range pixieDatabases() {
		secret, err := ensureMintedSecret(ctx, r.Client, r.Scheme, cloud, pixieDatabaseSecret(db), corev1.SecretTypeBasicAuth,
			withLabels(labels, map[string]string{"cnpg.io/reload": "true"}),
			fixedKey(corev1.BasicAuthUsernameKey, db), randomKey(corev1.BasicAuthPasswordKey, 24), randomKey("database-key", 32))
		if err != nil {
			return pixieCloudMaterial{}, err
		}
		databases[db] = secret
	}
	bootstrapSecret, err := ensureMintedSecret(ctx, r.Client, r.Scheme, cloud, pixieDatabaseSecret(pixieBootstrapRole), corev1.SecretTypeBasicAuth,
		withLabels(labels, map[string]string{"cnpg.io/reload": "true"}),
		fixedKey(corev1.BasicAuthUsernameKey, pixieBootstrapRole), randomKey(corev1.BasicAuthPasswordKey, 24))
	if err != nil {
		return pixieCloudMaterial{}, err
	}
	var auth corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: pixieAuthSecret}, &auth); err != nil {
		return pixieCloudMaterial{}, err
	}
	var search corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: pixieSearchSecret}, &search); err != nil {
		return pixieCloudMaterial{}, err
	}
	return pixieCloudMaterial{
		certsHash:      secretsHash(certs...),
		cloudCA:        cloudCA.CertPEM,
		serviceCA:      serviceCA.CertPEM,
		searchSecurity: pixieSearchSecurityConfig(string(search.Data["indexer-hash"]), string(search.Data["api-hash"])),
		creds: pixieHubCredentials{
			ProfilePassword:   string(databases["profile"].Data[corev1.BasicAuthPasswordKey]),
			BootstrapPassword: string(bootstrapSecret.Data[corev1.BasicAuthPasswordKey]),
			SearchUser:        "pixie-indexer",
			SearchPassword:    string(search.Data["indexer-password"]),
			SigningKey:        string(auth.Data["jwt-signing-key"]),
			Domain:            cloud.Spec.Domain,
			ServiceCA:         serviceCA.CertPEM,
		},
	}, nil
}

// pixieProxyDNSNames are the names the hub's proxy and vzconn answer to: the UI's domain and its
// work. subdomain, vzconn's own Service, and the import Service agents on other clusters dial.
func pixieProxyDNSNames(domain string) []string {
	return []string{
		domain,
		"work." + domain,
		pixieVzconnService + "." + oramav1alpha1.PixieCloudNamespace + ".svc",
		pixieAgentImportService + "." + oramav1alpha1.PixieVizierNamespace + ".svc",
	}
}

// databaseStatus is the hub's Postgres as a component.
func (r *PixieCloudReconciler) databaseStatus(ctx context.Context) (oramav1alpha1.ObservabilityComponentStatus, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	component := oramav1alpha1.ObservabilityComponentStatus{Name: pixieDBCluster}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oramav1alpha1.PixieCloudNamespace, Name: pixieDBCluster}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			component.Message = "not created yet"
			return component, nil
		}
		return component, err
	}
	readyInstances, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	if readyInstances >= pixieDBInstances {
		component.Ready = true
	} else {
		component.Message = "instances starting"
	}
	return component, nil
}

// bootstrap runs once the hub is Ready, and keeps one deploy key per accepted agent through
// Pixie's API. It returns each agent's state and, while it cannot finish, why.
func (r *PixieCloudReconciler) bootstrap(ctx context.Context, cloud *oramav1alpha1.PixieCloud, material pixieCloudMaterial, hubReady bool) ([]oramav1alpha1.PixieCloudAgentStatus, string, error) {
	statuses := make([]oramav1alpha1.PixieCloudAgentStatus, 0, len(cloud.Spec.Agents))
	for _, agent := range cloud.Spec.Agents {
		statuses = append(statuses, oramav1alpha1.PixieCloudAgentStatus{Name: agent.Name})
	}
	if !hubReady {
		return statuses, "", nil
	}
	backend, err := r.backend(material.creds)
	if err != nil {
		return statuses, "", err
	}
	defer backend.Close()
	if ok, why, err := backend.Ready(ctx); err != nil || !ok {
		return statuses, firstNonEmpty(why, "waiting for Pixie's databases"), err
	}
	if err := backend.EnsureIndex(ctx, pixieMetadataIndex); err != nil {
		return statuses, "", err
	}
	var admin corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: cloud.Namespace, Name: pixieAdminSecret}, &admin); err != nil {
		return statuses, "", err
	}
	identityID, err := backend.EnsureAdmin(ctx, cloud.Spec.AdminEmail, string(admin.Data["password"]))
	if err != nil {
		return statuses, "", err
	}
	as, err := backend.EnsureOrg(ctx, cloud.Spec.Organization, cloud.Spec.AdminEmail, identityID)
	if err != nil {
		return statuses, "", err
	}
	known, err := backend.DeployKeyIDs(ctx, as)
	if err != nil {
		return statuses, "", err
	}
	var existing corev1.SecretList
	if err := r.List(ctx, &existing, client.InNamespace(cloud.Namespace), client.HasLabels{pixieDeployKeyLabel}); err != nil {
		return statuses, "", err
	}
	wanted := map[string]bool{}
	for _, agent := range cloud.Spec.Agents {
		wanted[agent.Name] = true
	}
	// Agents the managing cluster no longer lists: the key goes from Pixie, then the Secret.
	for i := range existing.Items {
		secret := &existing.Items[i]
		if wanted[secret.Labels[pixieDeployKeyLabel]] || !metav1.IsControlledBy(secret, cloud) {
			continue
		}
		if id := string(secret.Data["id"]); known[id] {
			if err := backend.DeleteDeployKey(ctx, as, id); err != nil {
				return statuses, "", err
			}
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return statuses, "", err
		}
	}
	clusters, err := backend.Clusters(ctx, as)
	if err != nil {
		return statuses, "", err
	}
	for i, agent := range cloud.Spec.Agents {
		if err := r.ensureDeployKey(ctx, cloud, backend, as, agent.Name, known); err != nil {
			return statuses, "", err
		}
		statuses[i].DeployKeySecret = pixieDeployKeyPrefix + agent.Name
		state := clusters[agent.Cluster]
		statuses[i].Registered, statuses[i].Connected = state.Registered, state.Connected
	}
	return statuses, "", nil
}

// ensureDeployKey keeps an agent's key: the Secret's key is kept while Pixie still knows its id;
// otherwise (a new agent, or a rebuilt hub) Pixie creates a new one and the Secret takes it. The
// Secret is written only then.
func (r *PixieCloudReconciler) ensureDeployKey(ctx context.Context, cloud *oramav1alpha1.PixieCloud, backend pixieHubBackend, as pixieIdentity, agent string, known map[string]bool) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pixieDeployKeyPrefix + agent, Namespace: cloud.Namespace}}
	found, err := getIfExists(ctx, r.Client, secret.Namespace, secret.Name, secret)
	if err != nil {
		return err
	}
	if found {
		if !metav1.IsControlledBy(secret, cloud) {
			return fmt.Errorf("Secret %s/%s exists and is not this hub's", secret.Namespace, secret.Name)
		}
		if known[string(secret.Data["id"])] && len(secret.Data["deploy-key"]) > 0 {
			return nil
		}
	}
	key, err := backend.CreateDeployKey(ctx, as)
	if err != nil {
		return err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{pixieManagedLabel: pixieManagedCloud, pixieDeployKeyLabel: agent}
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{"deploy-key": []byte(key.Key), "id": []byte(key.ID)}
		return controllerutil.SetControllerReference(cloud, secret, r.Scheme)
	})
	return err
}

// setStatus writes status only when it changed (principle 7).
func (r *PixieCloudReconciler) setStatus(ctx context.Context, cloud *oramav1alpha1.PixieCloud, observed oramav1alpha1.PixieCloudStatus, alertingErr error) error {
	if observed.Phase == "" {
		return nil
	}
	desired := observed
	if desired.CACertificate == "" {
		desired.CACertificate = cloud.Status.CACertificate
	}
	desired.ObservedGeneration = cloud.Generation
	desired.Conditions = append([]metav1.Condition(nil), cloud.Status.Conditions...)
	apimeta.SetStatusCondition(&desired.Conditions, pixieCondition(observed.Phase, observed.Message, cloud.Generation))
	apimeta.SetStatusCondition(&desired.Conditions, alertingCondition(alertingErr, cloud.Generation))
	if apiequality.Semantic.DeepEqual(cloud.Status, desired) {
		return nil
	}
	cloud.Status = desired
	return r.Status().Update(ctx, cloud)
}

// alertingCondition says whether the cluster's rules and routing are written.
func alertingCondition(err error, generation int64) metav1.Condition {
	if err != nil {
		return metav1.Condition{Type: pixieAlertingCondition, Status: metav1.ConditionFalse, Reason: "MonitoringCRDsMissing", Message: err.Error(), ObservedGeneration: generation}
	}
	return metav1.Condition{Type: pixieAlertingCondition, Status: metav1.ConditionTrue, Reason: "RulesWritten", Message: "alert rules and routing are written", ObservedGeneration: generation}
}

func (r *PixieCloudReconciler) cloudsInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	if object.GetNamespace() != oramav1alpha1.PixieCloudNamespace {
		return nil
	}
	var list oramav1alpha1.PixieCloudList
	if err := r.List(ctx, &list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}

func (r *PixieCloudReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("pixiecloud").
		For(&oramav1alpha1.PixieCloud{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.cloudsInNamespace)).
		Complete(r)
}
