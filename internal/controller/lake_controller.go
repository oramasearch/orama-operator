/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// LakeReconciler reconciles a Lake object into a Namespace, a Deployment, a Service, a
// control-plane Postgres (CloudNativePG Cluster, on by default), an owned Zetta data plane and,
// opt-in, an Ingress plus a Cloudflare DNS record.
type LakeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorNamespace is the namespace of the manager pod that reads Lake's control database and
	// Zetta API. Empty omits those in-cluster ingress allowances in local unit tests.
	OperatorNamespace string
	// ServiceConnectionURL is the control plane's in-cluster https base URL, where a lake image
	// that reads LAKE_SERVICE_CONNECTION_URL asks for one service connection at a time
	// (lake_service_connections.go, operator#773). Empty keeps every Lake on the aggregate mount.
	ServiceConnectionURL string
	// ServiceConnectionCAFile is the control plane's CA certificate, given to the Lake pod.
	ServiceConnectionCAFile string
	// ServiceStores opens lake's control Postgres for the managed-services poll. Nil uses pgx;
	// tests substitute a fake store.
	ServiceStores LakeServiceStoreOpener
	// SQLiteImage is the lake-sqlite image for sqlite services when a Lake sets no
	// spec.services.sqliteImage (the --lake-sqlite-image flag). Empty uses DefaultLakeSQLiteImage.
	SQLiteImage string
	// DuckDBImage is the lake-duckdb image for duckdb services when a Lake sets no
	// spec.services.duckdbImage (the --lake-duckdb-image flag). Empty uses DefaultLakeDuckDBImage.
	DuckDBImage string
	// PolarsImage is the lake-polars image for polars services when a Lake sets no
	// spec.services.polarsImage (the --lake-polars-image flag). Empty uses DefaultLakePolarsImage.
	PolarsImage string
	// ServiceEvents wakes the reconciler when lake writes a service or build row
	// (LakeServicesListener). Nil means rows are found by the poll alone.
	ServiceEvents <-chan event.GenericEvent
	// APIReader lists lake pods without a cache (no cluster-wide pod informer). Nil uses Client.
	APIReader client.Reader
	// NetworkPolicyCheck reads whether the cluster enforces network policy (operator#699,
	// lake_network_policy_enforcement.go). Nil is the Cilium check every cluster runs; it is not a
	// switch, only the point where a test supplies a fake result.
	NetworkPolicyCheck networkPolicyEnforcementCheck
	// workloadResources throttles the workload resources pass per Lake (lake#643), and the app
	// transfer pass with it (lake#661).
	workloadResources workloadResourcesClock
	// ClerkAPIBase is Clerk's Backend API; empty is https://api.clerk.com/v1. SvixAPIBase replaces
	// the Svix API address a Clerk instance's webhook portal names. Tests point both at fakes.
	ClerkAPIBase string
	SvixAPIBase  string
	// clerkClock throttles the Clerk configuration pass per Lake (lake_clerk.go).
	clerkClock lakeClerkClock
	// DomainResolver looks up a customer domain's records (lake_custom_domain.go); nil is the
	// system resolver. Tests substitute a fake.
	DomainResolver DomainResolver
	// customDomainClock throttles those lookups per Lake.
	customDomainClock lakeCustomDomainClock
	// StripeAPIBase is Stripe's API for the check of a Lake's own key (lake_payments.go); empty is
	// https://api.stripe.com. Tests point it at a fake.
	StripeAPIBase string
	// lakeAppKeyChecks throttles that check per Lake.
	lakeAppKeyChecks lakeAppKeyChecks
	// VolumeStats reads the kubelet's volume stats for a store's used space (lake#666). Nil reads
	// them through Clientset's node proxy; with neither, used space is not reported.
	VolumeStats VolumeStatsReader
	// storeUsage throttles the used-space measurement per Lake.
	storeUsage storeUsageClock
	// appTransfer holds the ingress controllers' last byte counters per Lake (lake#661).
	appTransfer appTransferState
	// AppTransferScrape substitutes reading an ingress controller pod's metrics in tests.
	AppTransferScrape func(ctx context.Context, pod *corev1.Pod) (map[ingressKey]ingressBytes, bool, error)
	// Clientset reads a completed builder pod's final stdout lines through pods/log.
	Clientset kubernetes.Interface
	// BuildLogs substitutes the pods/log read in tests.
	BuildLogs func(ctx context.Context, namespace, jobName string) (string, error)
	// LakeManifestLogs substitutes the manifest pod's pods/log read in tests (lake_manifest.go).
	// With neither it nor Clientset no image manifest is read and the vendored schema path applies.
	LakeManifestLogs func(ctx context.Context, namespace, podName string) (string, error)
	// BuildEventClient posts best-effort pod-level events to lake. Nil uses a bounded client.
	BuildEventClient *http.Client
}

const (
	lakeDefaultPort      = 4900 // lake's --listen default
	lakeServicePort      = 80
	lakeControlDBName    = "lake" // CNPG bootstrap database + owner
	lakeStatusRequeue    = 20 * time.Second
	lakeStorePath        = "/var/lib/lake/store"
	lakeStoreVolume      = "store"
	lakeZettaFinalizer   = "orama.cloud/lake-zetta"
	lakeSchemaGuardTable = "methodology_deployments_guard"
)

// lakeSchemaSQL is lake's control-plane schema. SOURCE OF TRUTH: oramasearch/lake db/schema.sql;
// this is a byte-identical vendored copy, applied per Lake through an owned PostgresSchema.
//
//go:embed assets/lake-schema.sql
var lakeSchemaSQL string

var (
	cnpgScheduledBackupGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ScheduledBackup"}
	cnpgBackupGVK          = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"}
	cnpgBackupListGVK      = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "BackupList"}
)

// lakeControlDB is the CNPG Cluster name for a Lake; CNPG derives `<name>-app` (the owner
// credentials + URI Secret) and `<name>-rw` (the primary Service) from it.
func lakeControlDB(name string) string { return "lake-" + name + "-controldb" }

func lakeControlDBScheduledBackup(name string) string { return lakeControlDB(name) + "-backup" }

// lakeAdminSecret holds the Lake's admin bearer token (key `token`), minted once by the operator and
// injected as LAKE_ADMIN_TOKEN — the same pattern as OramaZettaCluster's `<name>-zetta-admin`.
func lakeAdminSecret(name string) string { return "lake-" + name + "-admin" }

// lakeInstanceKekSecret holds the root wrapping key for lake's persisted identity and
// per-customer KEKs. It is distinct from the bearer token and is never exposed by the API.
func lakeInstanceKekSecret(name string) string { return "lake-" + name + "-instance-kek" }

func lakePostgresEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Postgres.Enabled == nil || *lake.Spec.Postgres.Enabled
}

func lakePostgresBackupEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Postgres.Backup.Enabled != nil && *lake.Spec.Postgres.Backup.Enabled
}

func lakeZettaEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Zetta.Enabled == nil || *lake.Spec.Zetta.Enabled
}

func lakeZettaName(name string) string {
	return "lake-" + name + "-zetta"
}

func lakeZettaClientService(name string) string {
	return lakeZettaName(name) + "-zetta"
}

func lakeZettaAdminSecret(name string) string {
	return lakeZettaName(name) + "-zetta-admin"
}

type lakeBackupConfig struct {
	Enabled    bool
	Valid      bool
	Reason     string
	Message    string
	Bucket     string
	Path       string
	Endpoint   string
	Schedule   string
	Retention  string
	SecretName string
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;patch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=postgresschemas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=scheduledbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=backups,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings the cluster to the state described by the Lake.
func (r *LakeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, req.NamespacedName, &lake); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ns := lakeNamespace(&lake)
	if !lake.DeletionTimestamp.IsZero() {
		// Payments first, while the Lake's finalizer holds it: the provider must outlive the
		// webhook endpoint, whose own finalizer deletes the Stripe endpoint with the provider's key.
		if done, err := r.removeLakePayments(ctx, &lake, ns); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove payments: %w", err)
		} else if !done {
			return ctrl.Result{RequeueAfter: lakeStatusRequeue}, nil
		}
		return r.finalizeLakeZetta(ctx, &lake)
	}
	if !controllerutil.ContainsFinalizer(&lake, lakeZettaFinalizer) {
		controllerutil.AddFinalizer(&lake, lakeZettaFinalizer)
		if err := r.Update(ctx, &lake); err != nil {
			return ctrl.Result{}, fmt.Errorf("add Zetta finalizer: %w", err)
		}
	}
	image := lake.Spec.Image
	if image == "" {
		return ctrl.Result{}, fmt.Errorf("lake %q: spec.image is required", lake.Name)
	}
	replicas, replicasRefused := lakeReplicas(&lake)
	// A suspended Lake runs no lake process and keeps everything (operator#862,
	// lake_suspension.go). status.suspendedSince is set in memory here, so the Zetta grace below
	// reads it, and written with the rest of the status.
	now := time.Now()
	previousSuspendedSince := lake.Status.SuspendedSince
	lake.Status.SuspendedSince = lakeSuspendedSince(&lake, now)
	if lake.Spec.Suspended {
		replicas = 0
	}
	port := int32(lakeDefaultPort)
	if lake.Spec.Port != nil {
		port = *lake.Spec.Port
	}

	// Namespace: create if missing. Not owner-ref'd — a namespace outliving the Lake
	// is safer than a Lake deletion taking a namespace with it. A blind Create (AlreadyExists
	// ignored) rather than Get-then-Create: a cached Get would start a cluster-wide Namespace
	// informer, which the operator deliberately dropped (operator#106) — the manager holds
	// only `namespaces: get, create`, no list/watch.
	if err := r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err == nil {
		log.Info("created namespace", "namespace", ns)
	} else if !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create namespace %q: %w", ns, err)
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       "lake",
		"app.kubernetes.io/instance":   lake.Name,
		"app.kubernetes.io/managed-by": "orama-operator",
	}
	if err := r.reconcileLakeNetworkSandbox(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile network sandbox: %w", err)
	}
	name := "lake-" + lake.Name
	zetta, err := r.reconcileLakeZetta(ctx, &lake, ns, labels)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Zetta data plane: %w", err)
	}
	// Stop what the Lake controls while it is suspended, or give back what a suspension took,
	// before any control row is acted on.
	if err := r.reconcileLakeSuspension(ctx, &lake, now); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile suspension: %w", err)
	}
	claimName, storeAccessModes, err := r.reconcileLakeStore(ctx, &lake, ns, labels)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile store: %w", err)
	}

	// Admin token: minted once, before the Deployment so its secretKeyRef resolves.
	if err := r.reconcileLakeAdminToken(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile admin token: %w", err)
	}
	// The credential, CA and namespace label an on-demand image uses to ask for one service
	// connection (lake_service_connections.go, operator#773).
	if err := r.reconcileLakeServiceConnectionAccess(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileLakeDeployToken(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile deployment token: %w", err)
	}
	if err := r.reconcileLakeInstanceKek(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile instance KEK: %w", err)
	}
	sealing, err := r.reconcileLakeSealingKey(ctx, &lake, ns, labels)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile sealing key: %w", err)
	}
	if err := r.reconcileLakeAgentServiceToken(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile agent service token: %w", err)
	}
	if err := r.reconcileLakeAgentBusTokens(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, err
	}

	// The Clerk instance (spec.clerk, lake_clerk.go): before the Deployment, which reads its keys.
	clerk, err := r.reconcileLakeClerk(ctx, &lake, ns, labels)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Clerk: %w", err)
	}
	if lakeSpecEnvBillingIgnored(&lake) {
		log.Info("spec.env names LAKE_BILLING but spec.billing decides it; the spec.env entry is ignored, remove it",
			"billing", lake.Spec.Billing.Enabled)
	}
	// The image's manifest (lake_manifest.go, #936): its schema, Stripe events and products.
	manifest, err := r.reconcileLakeImageManifest(ctx, &lake, ns)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read image manifest: %w", err)
	}
	// Payments (spec.payments, lake_payments.go): before the Deployment, which reads its keys.
	payments, err := r.reconcileLakePayments(ctx, &lake, ns, labels, manifest.Status)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile payments: %w", err)
	}

	// A customer's own domain (spec.customDomain, lake_custom_domain.go): before the Deployment,
	// whose LAKE_PUBLIC_HOST follows it once it is routed. lakeEnv reads the fresh state from
	// lake.Status.CustomDomain, which is written below with the rest of the status.
	customDomain, customDomainCondition, err := r.reconcileLakeCustomDomain(ctx, &lake, ns, name, labels)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile custom domain: %w", err)
	}
	previousCustomDomain := lake.Status.CustomDomain
	lake.Status.CustomDomain = customDomain

	// Service.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: "http", Port: lakeServicePort, TargetPort: intstr.FromInt(int(port)),
		}}
		return controllerutil.SetControllerReference(&lake, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}

	backup, err := r.resolveLakeBackupConfig(ctx, &lake, ns)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve control DB backup: %w", err)
	}

	// Control-plane Postgres (CNPG), on by default, and its schema (PostgresSchema).
	if err := r.reconcileLakeControlDB(ctx, &lake, ns, labels, backup); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile control db: %w", err)
	}
	// The schema follows the image's manifest; while a new image's manifest is being read the
	// PostgresSchema is left as it is, and so is the Deployment.
	desiredSchemaVersion := ""
	if manifest.Current {
		if err := r.reconcileLakeControlDBSchema(ctx, &lake, ns, labels, manifest.Status); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile control db schema: %w", err)
		}
		desiredSchemaVersion = lakeDesiredSchemaVersion(&lake, manifest.Status)
	}

	// Hold both first startup and updates until the desired schema has finished applying.
	// Keep an existing Deployment intact while the replacement migration Job runs.
	schemaPhase, schemaVersion, schemaReadiness := r.lakeSchemaStatus(ctx, &lake, ns, desiredSchemaVersion)
	schemaReady := schemaReadiness.phase == "Ready" && (manifest.Current || !lakePostgresEnabled(&lake))
	deploy := &appsv1.Deployment{}
	if schemaReady {
		deploy, err = r.reconcileLakeDeployment(
			ctx, &lake, ns, name, image, claimName, storeAccessModes, labels, replicas, port, sealing, clerk, payments, manifest.Status,
		)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile deployment: %w", err)
		}
		if err := r.reconcileLakePDB(ctx, &lake, ns, name, labels, replicas); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile pod disruption budget: %w", err)
		}
	} else if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, deploy); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("read deployment while waiting for schema: %w", err)
	}

	// Lake-owned control rows (connections, services, stores, agents, builds): best-effort, never fail the Lake.
	// The tables exist only once the schema has applied, so the poll waits for it.
	// Tenant code (workspace namespaces, services, stores, agents, builds, methodology, apps and
	// environments, all made from those rows) runs only where the cluster enforces network policy
	// (operator#699). Where it does not, the rows are not synchronized: nothing new starts and
	// nothing running is changed, and the Lake reports Degraded with the reason. What already runs
	// is left in place rather than deleted, so a transient failed read never removes tenant data.
	networkPolicy := r.lakeNetworkPolicyEnforcement(ctx, &lake)
	if !networkPolicy.Enforced {
		log.Info("tenant workloads refused: network policy is not enforced",
			"reason", networkPolicy.Condition.Reason, "message", networkPolicy.Condition.Message)
	}
	// A suspended Lake acts on no control row and runs no library agent: nothing new starts and
	// nothing missing is made again until it is not suspended (operator#862).
	controlRowsChanging := false
	if lakeControlRowsEnabled(&lake) && schemaReady && networkPolicy.Enforced && !lake.Spec.Suspended {
		controlRowsChanging = r.reconcileLakeServices(ctx, &lake, ns, labels)
	}

	// The Agent Library's specialized agents (lake_agents.go): best-effort, never fails the Lake.
	if !lake.Spec.Suspended {
		if err := r.reconcileLakeLibraryAgents(ctx, &lake, ns, labels); err != nil {
			log.Error(err, "library agents deferred")
		}
	}
	// Orama's provider key never stays in a workspace namespace (orama-operator#779).
	if err := r.removeWorkspaceProviderKeyCopies(ctx, &lake); err != nil {
		log.Error(err, "removing provider key copies from workspace namespaces deferred")
	}

	// Ingress + DNS (opt-in, same contract as OramaZettaCluster).
	if err := r.reconcileLakeIngress(ctx, &lake, ns, name, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ingress: %w", err)
	}

	// Aggregate readiness before writing status, preserving stable conditions between polls.
	var readiness []lakeComponentReadiness
	if lake.Spec.Suspended {
		readiness = append(readiness, lakeSuspensionReadiness(&lake, now))
	}
	readiness = append(readiness, lakeDeploymentReadiness(deploy, replicas))
	storeStatus, storeCondition, storeReady, err := r.lakeStoreStatus(ctx, &lake, ns, claimName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read store status: %w", err)
	}
	if !storeReady {
		readiness = append(readiness, lakeComponentReadiness{"Progressing", storeCondition.Reason, storeCondition.Message})
	}
	pgReady, pgDetail := r.lakeControlDBStatus(ctx, &lake, ns)
	if lakePostgresEnabled(&lake) && !pgReady {
		readiness = append(readiness, lakeComponentReadiness{
			"Progressing", "PostgresNotReady", "waiting for all configured Postgres instances to be ready (observed " + pgDetail + ")",
		})
	}
	readiness = append(readiness, schemaReadiness)
	if manifest.Readiness.phase != "Ready" {
		readiness = append(readiness, manifest.Readiness)
	}
	zettaReady, zettaCondition := lakeZettaStatus(&lake, zetta)
	if lakeZettaEnabled(&lake) && !zettaReady {
		zettaPhase := "Progressing"
		if zetta != nil && zetta.Status.Phase == "Degraded" {
			zettaPhase = "Degraded"
		}
		readiness = append(readiness, lakeComponentReadiness{zettaPhase, zettaCondition.Reason, zettaCondition.Message})
	}
	backupReady, lastBackupTime, backupCondition, err := r.lakeBackupStatus(ctx, &lake, ns, backup)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read control DB backup status: %w", err)
	}
	if backup.Enabled && !backup.Valid {
		readiness = append(readiness, lakeComponentReadiness{"Degraded", backupCondition.Reason, backupCondition.Message})
	}
	if replicasRefused != "" {
		readiness = append(readiness, lakeComponentReadiness{"Degraded", "ReplicasRefused", replicasRefused})
	}
	if !networkPolicy.Enforced {
		readiness = append(readiness, lakeComponentReadiness{"Degraded", networkPolicy.Condition.Reason, networkPolicy.Condition.Message})
	}
	phase, readyCondition := lakeAggregateReadiness(&lake, readiness)
	dataServices, dataServicesSummary := lakeDataServiceStatus(&lake)
	desiredConditions := lake.Status.Conditions
	conditionsChanged := apimeta.SetStatusCondition(&desiredConditions, readyCondition)
	if apimeta.SetStatusCondition(&desiredConditions, backupCondition) {
		conditionsChanged = true
	}
	if apimeta.SetStatusCondition(&desiredConditions, storeCondition) {
		conditionsChanged = true
	}
	if apimeta.SetStatusCondition(&desiredConditions, zettaCondition) {
		conditionsChanged = true
	}
	if apimeta.SetStatusCondition(&desiredConditions, lakeFeaturesCondition(&lake)) {
		conditionsChanged = true
	}
	if apimeta.SetStatusCondition(&desiredConditions, networkPolicy.Condition) {
		conditionsChanged = true
	}
	if apimeta.SetStatusCondition(&desiredConditions, lakeAppsWildcardCondition(&lake)) {
		conditionsChanged = true
	}
	// ClerkReady exists only on a Lake with spec.clerk.
	if clerk.Condition != nil {
		if apimeta.SetStatusCondition(&desiredConditions, *clerk.Condition) {
			conditionsChanged = true
		}
	} else if apimeta.RemoveStatusCondition(&desiredConditions, lakeClerkConditionType) {
		conditionsChanged = true
	}
	// CustomDomainReady exists only on a Lake with spec.customDomain.
	if customDomainCondition != nil {
		if apimeta.SetStatusCondition(&desiredConditions, *customDomainCondition) {
			conditionsChanged = true
		}
	} else if apimeta.RemoveStatusCondition(&desiredConditions, lakeCustomDomainConditionType) {
		conditionsChanged = true
	}
	// PaymentsReady exists only on a Lake with spec.payments, or while what it made is removed.
	if payments.Condition != nil {
		if apimeta.SetStatusCondition(&desiredConditions, *payments.Condition) {
			conditionsChanged = true
		}
	} else if apimeta.RemoveStatusCondition(&desiredConditions, lakePaymentsConditionType) {
		conditionsChanged = true
	}
	if lake.Status.ReadyReplicas != deploy.Status.ReadyReplicas || lake.Status.Phase != phase ||
		lake.Status.PostgresReady != pgReady || lake.Status.PostgresInstances != pgDetail ||
		lake.Status.ZettaReady != zettaReady ||
		lake.Status.SchemaPhase != schemaPhase || lake.Status.SchemaVersion != schemaVersion ||
		!apiequality.Semantic.DeepEqual(lake.Status.ImageManifest, manifest.Status) ||
		lake.Status.BackupReady != backupReady || !lakeBackupTimesEqual(lake.Status.LastBackupTime, lastBackupTime) ||
		!apiequality.Semantic.DeepEqual(lake.Status.DataServices, dataServices) ||
		lake.Status.DataServicesSummary != dataServicesSummary ||
		!apiequality.Semantic.DeepEqual(lake.Status.Store, storeStatus) ||
		!apiequality.Semantic.DeepEqual(lake.Status.Sealing, sealing.Status) ||
		!apiequality.Semantic.DeepEqual(previousCustomDomain, customDomain) ||
		!lakeBackupTimesEqual(previousSuspendedSince, lake.Status.SuspendedSince) ||
		conditionsChanged {
		lake.Status.ReadyReplicas = deploy.Status.ReadyReplicas
		lake.Status.Phase = phase
		lake.Status.PostgresReady = pgReady
		lake.Status.PostgresInstances = pgDetail
		lake.Status.ZettaReady = zettaReady
		lake.Status.SchemaPhase = schemaPhase
		lake.Status.SchemaVersion = schemaVersion
		lake.Status.ImageManifest = manifest.Status
		lake.Status.BackupReady = backupReady
		lake.Status.LastBackupTime = lastBackupTime
		lake.Status.DataServices = dataServices
		lake.Status.DataServicesSummary = dataServicesSummary
		lake.Status.Store = storeStatus
		lake.Status.Sealing = sealing.Status
		lake.Status.Conditions = desiredConditions
		if err := r.Status().Update(ctx, &lake); err != nil {
			return ctrl.Result{}, err
		}
	}
	result := lakeRequeue(&lake, controlRowsChanging, zettaReady, storeReady)
	// A suspended Lake is looked at again when its Zetta grace ends.
	if wait := lakeSuspensionRequeue(&lake, now); wait > 0 && (result.RequeueAfter == 0 || wait < result.RequeueAfter) {
		result.RequeueAfter = wait
	}
	// A manifest being read is looked at sooner.
	if manifest.Requeue > 0 && (result.RequeueAfter == 0 || manifest.Requeue < result.RequeueAfter) {
		result.RequeueAfter = manifest.Requeue
	}
	// A customer domain not yet routed is looked at again soon: nothing the operator watches
	// changes when the customer creates their records or the certificate is issued.
	if customDomain != nil && customDomain.Phase != lakeCustomDomainRouted &&
		(result.RequeueAfter == 0 || lakeCustomDomainCheckPending < result.RequeueAfter) {
		result.RequeueAfter = lakeCustomDomainCheckPending
	}
	// The next check of the Lake's Stripe key, or a payments removal, may be due sooner.
	if payments.Requeue > 0 && (result.RequeueAfter == 0 || payments.Requeue < result.RequeueAfter) {
		result.RequeueAfter = payments.Requeue
	}
	return result, nil
}

// lakeRequeue is when a reconciled Lake is looked at again.
func lakeRequeue(lake *oramav1alpha1.Lake, controlRowsChanging, zettaReady, storeReady bool) ctrl.Result {
	// Control rows are polled, and their observed Kubernetes state can change without updating the
	// Lake, so they run at their own cadence; unchanged polls write nothing.
	if lakeControlRowsEnabled(lake) {
		// Starting or deleting work is followed closely so its status tracks the cluster rather
		// than the regular poll.
		if controlRowsChanging {
			return ctrl.Result{RequeueAfter: lakeServicesChangingPoll}
		}
		return ctrl.Result{RequeueAfter: min(lakeStatusRequeue, lakeServicesPollInterval(lake))}
	}
	// CNPG Cluster and Backup resources are not watched. Keep polling after success so
	// readiness loss and future backups are observed; unchanged polls write no status.
	if lakePostgresEnabled(lake) {
		return ctrl.Result{RequeueAfter: lakeStatusRequeue}
	}
	if lakeZettaEnabled(lake) && !zettaReady {
		return ctrl.Result{RequeueAfter: lakeStatusRequeue}
	}
	if !storeReady {
		return ctrl.Result{RequeueAfter: lakeStatusRequeue}
	}
	return ctrl.Result{}
}

func (r *LakeReconciler) finalizeLakeZetta(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(lake, lakeZettaFinalizer) {
		return ctrl.Result{}, nil
	}

	children, err := r.lakeZettaChildren(ctx, lake)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list Zetta data planes during finalization: %w", err)
	}
	for i := range children {
		if children[i].DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &children[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete Zetta data plane: %w", err)
			}
		}
	}
	if len(children) > 0 {
		return ctrl.Result{RequeueAfter: lakeStatusRequeue}, nil
	}

	controllerutil.RemoveFinalizer(lake, lakeZettaFinalizer)
	if err := r.Update(ctx, lake); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove Zetta finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *LakeReconciler) reconcileLakeZetta(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	labels map[string]string,
) (*oramav1alpha1.OramaZettaCluster, error) {
	children, err := r.lakeZettaChildren(ctx, lake)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKey{Namespace: namespace, Name: lakeZettaName(lake.Name)}
	for i := range children {
		if client.ObjectKeyFromObject(&children[i]) == key && lakeZettaEnabled(lake) {
			continue
		}
		// Service clusters are also Lake-controlled but are never removed here: only the Lake's
		// own data plane follows spec.zetta. The services poll deletes an absent service; the rest
		// go with the Lake at finalization.
		if isLakeServiceCluster(&children[i]) {
			continue
		}
		if err := r.Delete(ctx, &children[i]); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	if !lakeZettaEnabled(lake) {
		return nil, nil
	}

	zetta := &oramav1alpha1.OramaZettaCluster{}
	if err := r.Get(ctx, key, zetta); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		zetta = &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      key.Name,
				Namespace: key.Namespace,
			},
		}
	} else if !metav1.IsControlledBy(zetta, lake) {
		return nil, fmt.Errorf(
			"OramaZettaCluster %s already exists and is not controlled by Lake %s",
			key,
			lake.Name,
		)
	}

	replicas := int32(1)
	if lake.Spec.Zetta.Replicas != nil {
		replicas = *lake.Spec.Zetta.Replicas
	}
	// The Lake's Zetta is also its deployment control plane (operator#557): it carries the
	// operator schema, so the namespace has exactly the one operator-schema Zetta that the
	// deployment REST/MCP contract and the deploy sync look for, and lake reads and appends
	// orama_control through LAKE_OPERATOR_ZETTA_URL with the admin token it already holds for this
	// cluster. Managed services are still requested through lake's control Postgres, and service
	// and workspace Zettas stay plain.
	operatorSchema := true
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, zetta, func() error {
		zetta.Labels = withLabels(labels, lakeInstancePodLabels(lake))
		zetta.Spec = oramav1alpha1.OramaZettaClusterSpec{
			Replicas:                replicas,
			Image:                   lakeDataPlaneZettaImage(lake),
			PullSecretName:          lake.Spec.PullSecretName,
			Storage:                 lake.Spec.Zetta.Storage,
			Env:                     lakeDataPlaneZettaEnv(lake),
			Reachability:            lake.Spec.Zetta.Reachability,
			ProvisionOperatorSchema: &operatorSchema,
			// Stopped once a suspended Lake's grace has passed (operator#862).
			Suspended: lakeZettasSuspended(lake, time.Now()),
		}
		return controllerutil.SetControllerReference(lake, zetta, r.Scheme)
	})
	return zetta, err
}

func (r *LakeReconciler) lakeZettaChildren(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
) ([]oramav1alpha1.OramaZettaCluster, error) {
	var clusters oramav1alpha1.OramaZettaClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return nil, err
	}
	children := make([]oramav1alpha1.OramaZettaCluster, 0, len(clusters.Items))
	for i := range clusters.Items {
		if metav1.IsControlledBy(&clusters.Items[i], lake) {
			children = append(children, clusters.Items[i])
		}
	}
	return children, nil
}

func lakeZettaStatus(
	lake *oramav1alpha1.Lake,
	zetta *oramav1alpha1.OramaZettaCluster,
) (bool, metav1.Condition) {
	condition := metav1.Condition{
		Type:               "ZettaReady",
		ObservedGeneration: lake.Generation,
	}
	if !lakeZettaEnabled(lake) {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ZettaDisabled"
		condition.Message = "Zetta data plane is disabled"
		return false, condition
	}
	if zetta == nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ZettaNotFound"
		condition.Message = "waiting for the owned OramaZettaCluster"
		return false, condition
	}

	desired := zetta.Spec.Replicas
	if desired < 1 {
		desired = 1
	}
	ready := zetta.Status.Phase == "Ready" && zetta.Status.ReadyNodes >= desired
	condition.Status = metav1.ConditionFalse
	condition.Reason = "ZettaProgressing"
	condition.Message = fmt.Sprintf("%d/%d Zetta nodes ready", zetta.Status.ReadyNodes, desired)
	if zetta.Status.Phase == "Degraded" {
		condition.Reason = "ZettaDegraded"
	}
	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "ZettaReady"
		condition.Message = fmt.Sprintf("all %d Zetta nodes ready", desired)
	}
	return ready, condition
}

func (r *LakeReconciler) reconcileLakeDeployment(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	name string,
	image string,
	claimName string,
	storeAccessModes []corev1.PersistentVolumeAccessMode,
	labels map[string]string,
	replicas int32,
	port int32,
	sealing lakeSealingMaterial,
	clerk lakeClerkState,
	payments lakePaymentsState,
	manifest *oramav1alpha1.LakeImageManifest,
) (*appsv1.Deployment, error) {
	terminationGracePeriodSeconds := int64(lakeTerminationGracePeriodSeconds)
	strategy, affinity := lakeRollout(replicas, storeAccessModes, lake.Spec.Rollout.OverlapOnSharedStore, labels)
	resources := resourceReqs("128Mi", "50m", "512Mi")
	if lake.Spec.Resources != nil {
		resources = lake.Spec.Resources.DeepCopy()
	}

	containerSecurityContext := hardenedSecurityContext()
	containerSecurityContext.RunAsNonRoot = boolPtr(true)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = labels
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deployment.Spec.Strategy = strategy
		deployment.Spec.Template.ObjectMeta.Labels = withLabels(labels, lakeInstancePodLabels(lake))
		setLakePaymentsSecretHash(&deployment.Spec.Template.ObjectMeta, payments.Hash)
		deployment.Spec.Template.Spec = corev1.PodSpec{
			// ndots:2, so the full Service names lake dials resolve on the first query.
			DNSConfig: lakePodDNSConfig(),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:        boolPtr(true),
				RunAsUser:           ptrInt64(lakeRunAsUser),
				RunAsGroup:          ptrInt64(lakeRunAsGroup),
				FSGroup:             ptrInt64(lakeRunAsGroup),
				FSGroupChangePolicy: ptrFSGroupChangePolicy(corev1.FSGroupChangeOnRootMismatch),
			},
			TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
			TopologySpreadConstraints:     lakeTopologySpreadConstraints(lake, replicas, labels),
			Affinity:                      affinity,
			Containers: []corev1.Container{{
				Name:            "lake",
				Image:           image,
				Args:            lake.Spec.Args,
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
				Env:             slices.Concat(lakeEnv(lake, sealing), lakeClerkEnv(lake, clerk), lakePaymentsEnv(lake, payments)),
				Resources:       *resources,
				SecurityContext: containerSecurityContext,
				Lifecycle:       lakeDrainPreStop(),
				VolumeMounts: []corev1.VolumeMount{{
					Name:      lakeStoreVolume,
					MountPath: lakeStorePath,
				}},
				// /readyz answers 200 only once lake's control store answers a ping, so a new pod
				// receives traffic only when it can serve it. /healthz is process liveness; the
				// startup probe holds liveness off while a cold start opens the store.
				StartupProbe:   lakeHTTPProbe("/healthz", 5, 3, 60),
				ReadinessProbe: lakeHTTPProbe("/readyz", 5, 2, 3),
				LivenessProbe:  lakeHTTPProbe("/healthz", 20, 3, 3),
			}},
			Volumes: []corev1.Volume{{
				Name: lakeStoreVolume,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claimName,
					},
				},
			}},
		}
		if lakeServicesEnabled(lake) {
			// Optional, so lake starts before the first service exists. kubelet refreshes a
			// mounted Secret in place, so adding a service key never restarts lake.
			optional := true
			mode := int32(0o440) // readable by lake through its fsGroup
			spec := &deployment.Spec.Template.Spec
			spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      lakeServicesVolume,
				MountPath: lakeServicesMountPath,
				ReadOnly:  true,
			})
			spec.Volumes = append(spec.Volumes, corev1.Volume{
				Name: lakeServicesVolume,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  lakeServicesSecret(lake.Name),
					Optional:    &optional,
					DefaultMode: &mode,
				}},
			})
		}
		if lakeServicesEnabled(lake) && lakeServiceConnectionsOnDemand(lake, manifest, r.ServiceConnectionURL) {
			// The image asks the operator for one connection when it needs it (operator#773): no
			// service credential is mounted.
			applyLakeServiceConnectionsOnDemand(lake, &deployment.Spec.Template, r.ServiceConnectionURL)
		}
		if len(lakeAgentLibrary(lake)) > 0 {
			// The library agents' bus credentials (lake docs/api/agent-bus.md), one file each.
			mode := int32(0o440)
			spec := &deployment.Spec.Template.Spec
			spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      lakeAgentBusVolume,
				MountPath: lakeAgentBusMountPath,
				ReadOnly:  true,
			})
			spec.Volumes = append(spec.Volumes, corev1.Volume{
				Name: lakeAgentBusVolume,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  lakeAgentBusSecret(lake.Name),
					DefaultMode: &mode,
				}},
			})
		}
		if lake.Spec.PullSecretName != "" {
			deployment.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{
				Name: lake.Spec.PullSecretName,
			}}
		}
		return controllerutil.SetControllerReference(lake, deployment, r.Scheme)
	})
	return deployment, err
}

const (
	// lakePreStopDrainSeconds keeps a terminating lake serving while the endpoint controller and
	// the ingress drop it, and while requests already routed to it finish. lake stops its HTTP
	// server immediately on SIGTERM, so this delay is the drain.
	lakePreStopDrainSeconds = 15
	// lakeTerminationGracePeriodSeconds covers the drain plus lake's own shutdown.
	lakeTerminationGracePeriodSeconds = 45
)

// lakeDrainPreStop uses the kubelet's built-in sleep action, so the drain does not depend on a
// shell in the lake image.
func lakeDrainPreStop() *corev1.Lifecycle {
	return &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			Sleep: &corev1.SleepAction{Seconds: lakePreStopDrainSeconds},
		},
	}
}

// lakeReplicas is the Deployment's replica count: spec.replicas, default 1, never more than 1
// (operator#698). Lake runs one process per store root; a second one reports Ready while it waits
// for the root, takes half the traffic and fails it. A CR stored before the CRD's maximum gets one
// replica and the reason it was refused, for a Degraded Ready condition.
func lakeReplicas(lake *oramav1alpha1.Lake) (int32, string) {
	if lake.Spec.Replicas == nil {
		return 1, ""
	}
	if requested := *lake.Spec.Replicas; requested > 1 {
		return 1, fmt.Sprintf("spec.replicas is %d; lake runs one process per store root (operator#698), so the Deployment keeps one replica", requested)
	}
	return *lake.Spec.Replicas, ""
}

// lakeRollout chooses how a pod-template change replaces lake pods.
//
// A ReadWriteMany store, and any multi-replica Lake (which needs one), surges: the replacement
// must pass /readyz before an old pod stops (maxSurge 1, maxUnavailable 0), so a change shows no
// gap in service.
//
// A single-replica Lake on a ReadWriteOnce claim (or a claim that does not exist yet) defaults to
// Recreate. Surging there means two lake processes open the same store root for the overlap, and
// that store holds data with no other copy; until that is proven safe, it is opt-in through
// spec.rollout.overlapOnSharedStore. With the opt-in the Lake surges too, and the new pod gets
// required pod affinity to its own pods on kubernetes.io/hostname, so it lands on the node with the
// volume attached and mounts it beside the old pod. The first pod matches its own affinity term,
// so it schedules anywhere when no lake pod exists. If that node cannot fit the surge pod, the
// rollout waits with the old pod still serving.
//
// A ReadWriteOncePod claim admits one pod only, so it always recreates: a surge pod could never
// mount it and the rollout would never finish.
//
// The result depends only on the spec and the claim's access modes, which are immutable on a
// PersistentVolumeClaim, never on observed pod state, so a second reconcile renders the same
// template.
func lakeRollout(
	replicas int32,
	accessModes []corev1.PersistentVolumeAccessMode,
	overlapOnSharedStore bool,
	labels map[string]string,
) (appsv1.DeploymentStrategy, *corev1.Affinity) {
	recreate := appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	if hasAccessMode(accessModes, corev1.ReadWriteOncePod) {
		return recreate, nil
	}
	maxSurge := intstr.FromInt32(1)
	maxUnavailable := intstr.FromInt32(0)
	surge := appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
	}
	if replicas > 1 || hasAccessMode(accessModes, corev1.ReadWriteMany) {
		return surge, nil
	}
	if !overlapOnSharedStore {
		return recreate, nil
	}
	selector := map[string]string{}
	for key, value := range labels {
		selector[key] = value
	}
	return surge, &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: selector},
				TopologyKey:   "kubernetes.io/hostname",
			}},
		},
	}
}

func hasAccessMode(modes []corev1.PersistentVolumeAccessMode, mode corev1.PersistentVolumeAccessMode) bool {
	for _, candidate := range modes {
		if candidate == mode {
			return true
		}
	}
	return false
}

func lakeHTTPProbe(path string, periodSeconds, timeoutSeconds, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromString("http"),
			},
		},
		PeriodSeconds:    periodSeconds,
		TimeoutSeconds:   timeoutSeconds,
		FailureThreshold: failureThreshold,
	}
}

func lakeTopologySpreadConstraints(
	lake *oramav1alpha1.Lake,
	replicas int32,
	labels map[string]string,
) []corev1.TopologySpreadConstraint {
	if lake.Spec.TopologySpreadConstraints != nil {
		return append([]corev1.TopologySpreadConstraint(nil), lake.Spec.TopologySpreadConstraints...)
	}
	if replicas <= 1 {
		return nil
	}
	return spreadConstraints(labels)
}

func (r *LakeReconciler) reconcileLakePDB(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	name string,
	labels map[string]string,
	replicas int32,
) error {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	if replicas <= 1 {
		return deleteIfExists(ctx, r.Client, pdb)
	}

	maxUnavailable := intstr.FromInt(1)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = labels
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.MinAvailable = nil
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		return controllerutil.SetControllerReference(lake, pdb, r.Scheme)
	})
	return err
}

func lakeDataServiceStatus(lake *oramav1alpha1.Lake) ([]oramav1alpha1.LakeDataServiceStatus, string) {
	previous := make(map[string]oramav1alpha1.LakeDataServiceStatus, len(lake.Status.DataServices))
	for _, service := range lake.Status.DataServices {
		previous[service.Name] = service
	}

	postgres := previous["postgres"]
	postgres.Name = "postgres"
	postgres.Kind = "postgres"
	postgres.Backup = untestedUnlessTested(postgres.Backup)
	postgres.Restore = untestedUnlessTested(postgres.Restore)
	postgres.Upgrade = untestedUnlessTested(postgres.Upgrade)
	postgres.Failover = untestedUnlessTested(postgres.Failover)
	if !lakePostgresEnabled(lake) {
		postgres.Backup = "none"
		postgres.Restore = "none"
		postgres.Upgrade = "none"
		postgres.Failover = "none"
		postgres.Notes = "control-plane Postgres is disabled"
	} else if !lakePostgresBackupEnabled(lake) {
		postgres.Backup = "none"
		postgres.Restore = "none"
	}
	instances := int32(1)
	if lake.Spec.Postgres.Instances != nil {
		instances = *lake.Spec.Postgres.Instances
	}
	if lakePostgresEnabled(lake) && instances < 2 {
		postgres.Failover = "none"
		if postgres.Notes == "" || postgres.Notes == postgresLifecycleGapNote {
			postgres.Notes = postgresSingleInstanceNote
		} else if !strings.Contains(postgres.Notes, postgresSingleInstanceNote) {
			postgres.Notes += "; " + postgresSingleInstanceNote
		}
	} else {
		postgres.Notes = removeDataServiceNote(postgres.Notes, postgresSingleInstanceNote)
	}
	postgres.Managed = postgresDataServiceManaged(&postgres, lake)
	if !postgres.Managed && postgres.Notes == "" {
		postgres.Notes = postgresLifecycleGapNote
	} else if postgres.Managed && postgres.Notes == postgresLifecycleGapNote {
		postgres.Notes = ""
	}

	zetta := oramav1alpha1.LakeDataServiceStatus{
		Name:     "zetta",
		Kind:     "zetta",
		Managed:  false,
		Backup:   "none",
		Restore:  "none",
		Upgrade:  "none",
		Failover: "none",
		Notes:    "Zetta lifecycle procedures have not been rehearsed",
	}
	services := []oramav1alpha1.LakeDataServiceStatus{postgres, zetta}
	return services, dataServiceSummary(services)
}

func postgresDataServiceManaged(service *oramav1alpha1.LakeDataServiceStatus, lake *oramav1alpha1.Lake) bool {
	return lakePostgresEnabled(lake) &&
		service.Backup == "tested" &&
		service.Restore == "tested" &&
		service.Upgrade == "tested" &&
		(service.Failover == "tested" || service.Failover == "none")
}

func dataServiceSummary(services []oramav1alpha1.LakeDataServiceStatus) string {
	managed := 0
	for _, service := range services {
		if service.Managed {
			managed++
		}
	}
	return fmt.Sprintf("%d managed, %d unmanaged", managed, len(services)-managed)
}

func removeDataServiceNote(notes string, remove string) string {
	parts := strings.Split(notes, "; ")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != remove {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

func untestedUnlessTested(value string) string {
	if value == "tested" {
		return value
	}
	return "untested"
}

func lakeStoreClaimName(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Store.ExistingClaim != "" {
		return lake.Spec.Store.ExistingClaim
	}
	return "lake-" + lake.Name + "-store"
}

// reconcileLakeStore creates the managed claim once. Its storage fields are deliberately not
// reconciled afterward because access mode and storage class are immutable and replacement would
// destroy the data boundary. An existingClaim is mounted without adoption or mutation.
func (r *LakeReconciler) reconcileLakeStore(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	ns string,
	lbls map[string]string,
) (string, []corev1.PersistentVolumeAccessMode, error) {
	claimName := lakeStoreClaimName(lake)
	pvc := &corev1.PersistentVolumeClaim{}
	key := client.ObjectKey{Namespace: ns, Name: claimName}
	if lake.Spec.Store.ExistingClaim != "" {
		// The rollout follows the claim's real access modes. A missing claim reports none; no
		// lake pod can start until it exists.
		if err := r.Get(ctx, key, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				return claimName, nil, nil
			}
			return "", nil, err
		}
		return claimName, pvc.Spec.AccessModes, nil
	}

	if err := r.Get(ctx, key, pvc); err == nil {
		if !metav1.IsControlledBy(pvc, lake) {
			return "", nil, fmt.Errorf(
				"persistent volume claim %s/%s already exists and is not controlled by Lake %s; set spec.store.existingClaim to use it",
				ns,
				claimName,
				lake.Name,
			)
		}
		return claimName, pvc.Spec.AccessModes, nil
	} else if !apierrors.IsNotFound(err) {
		return "", nil, err
	}

	size := lake.Spec.Store.Size.DeepCopy()
	if size.IsZero() {
		size = resource.MustParse("100Gi")
	}
	accessModes := append([]corev1.PersistentVolumeAccessMode(nil), lake.Spec.Store.AccessModes...)
	if len(accessModes) == 0 {
		accessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: ns,
			Labels:    lbls,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: accessModes,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: size,
			}},
		},
	}
	if lake.Spec.Store.StorageClassName != "" {
		pvc.Spec.StorageClassName = &lake.Spec.Store.StorageClassName
	}
	if err := controllerutil.SetControllerReference(lake, pvc, r.Scheme); err != nil {
		return "", nil, err
	}
	if err := r.Create(ctx, pvc); err != nil {
		return "", nil, err
	}
	return claimName, pvc.Spec.AccessModes, nil
}

func (r *LakeReconciler) lakeStoreStatus(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	ns string,
	claimName string,
) (*oramav1alpha1.LakeStoreStatus, metav1.Condition, bool, error) {
	status := &oramav1alpha1.LakeStoreStatus{ClaimName: claimName}
	condition := metav1.Condition{
		Type:               "StoreReady",
		ObservedGeneration: lake.Generation,
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: claimName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "StoreClaimNotFound"
			condition.Message = fmt.Sprintf("waiting for PersistentVolumeClaim %s/%s", ns, claimName)
			return status, condition, false, nil
		}
		return nil, condition, false, err
	}

	status.Phase = pvc.Status.Phase
	if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		copy := capacity.DeepCopy()
		status.Capacity = &copy
	}
	if pvc.Status.Phase == corev1.ClaimBound {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "StoreBound"
		condition.Message = fmt.Sprintf("PersistentVolumeClaim %s/%s is bound", ns, claimName)
		return status, condition, true, nil
	}
	condition.Status = metav1.ConditionFalse
	condition.Reason = "StoreUnbound"
	condition.Message = fmt.Sprintf(
		"PersistentVolumeClaim %s/%s is not bound (phase %q)",
		ns,
		claimName,
		pvc.Status.Phase,
	)
	return status, condition, false, nil
}

// lakeEnv is what the lake container learns from the operator. The control-DB Secret is
// generated by CNPG after the Cluster bootstraps, so the reference is optional: the pod starts
// before the database exists and picks the value up on its next restart. The Zetta token is a
// required Secret reference, so kubelet waits for the child controller to mint it before starting
// lake with an incomplete data-plane configuration.
func lakeEnv(lake *oramav1alpha1.Lake, sealing lakeSealingMaterial) []corev1.EnvVar {
	reserved := map[string]struct{}{
		"LAKE_ADMIN_TOKEN":               {},
		"LAKE_AGENT_BUS_DIR":             {},
		"LAKE_AGENT_ENDPOINTS":           {},
		"LAKE_AGENT_MODEL":               {},
		"LAKE_AGENT_SERVICE_TOKEN":       {},
		"LAKE_APPS_RUNTIME_IMAGE":        {},
		"LAKE_METHODOLOGY_RUNTIME_IMAGE": {},
		"LAKE_INTERNAL_URL":              {},
		"LAKE_CONTROL_DB_PASSWORD":       {},
		"LAKE_CONTROL_DB_URI":            {},
		"LAKE_DATA_PLANE_TOKEN":          {},
		"LAKE_DATA_PLANE_URL":            {},
		"LAKE_INSTANCE_KEK":              {},
		"LAKE_OPERATOR_ZETTA_TOKEN":      {},
		"LAKE_OPERATOR_ZETTA_URL":        {},
		"LAKE_SEALING_KID":               {},
		"LAKE_SEALING_PUBLIC_KEY":        {},
		"LAKE_SERVICE_DIR":               {},
		"LAKE_STORE_ROOT":                {},
		zettaEgressAllowEnv:              {},
	}
	var explicitAppsWildcard *corev1.EnvVar
	for i := range lake.Spec.Env {
		if lake.Spec.Env[i].Name == "LAKE_APPS_WILDCARD" {
			explicitAppsWildcard = &lake.Spec.Env[i]
		}
	}
	env := []corev1.EnvVar{
		{Name: "LAKE_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: lakeAdminSecret(lake.Name)},
			Key:                  "token",
		}}},
		{Name: "LAKE_STORE_ROOT", Value: lakeStorePath},
		{Name: "LAKE_SEALING_PUBLIC_KEY", Value: base64.RawURLEncoding.EncodeToString(sealing.PublicKey)},
		{Name: "LAKE_SEALING_KID", Value: sealing.KID},
		// lake's own allowlist for zetta's client (lake#1097), which refuses all but loopback without
		// one: lake reaches its bus Zetta, its workspace Zettas, agent services and service wrappers
		// through it, all in the cluster. The namespace's network policy decides which of them.
		{Name: zettaEgressAllowEnv, Value: lakeDeploymentEgressAllow},
	}
	if explicitAppsWildcard != nil {
		env = append(env, *explicitAppsWildcard.DeepCopy())
		reserved["LAKE_APPS_WILDCARD"] = struct{}{}
	} else {
		env = append(env, corev1.EnvVar{Name: "LAKE_APPS_WILDCARD", Value: lakeAppsWildcard(lake)})
		reserved["LAKE_APPS_WILDCARD"] = struct{}{}
	}
	env = append(env, corev1.EnvVar{Name: "LAKE_APPS_RUNTIME_IMAGE", Value: lakeAppsRuntimeImage(lake)})
	// The base of every methodology's image build and the stage and collect pods' image (lake#655).
	env = append(env, corev1.EnvVar{Name: "LAKE_METHODOLOGY_RUNTIME_IMAGE", Value: lakeMethodologyRuntimeImage(lake)})
	// The image lake runs MCP servers on: the default, unless spec.env names the variable, which
	// then stays where spec.env puts it.
	if runner, fromSpec := lakeMCPRunnerEnv(lake); !fromSpec {
		env = append(env, runner)
	}
	if lakePostgresEnabled(lake) {
		env = append(env, corev1.EnvVar{
			Name: "LAKE_INSTANCE_KEK",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: lakeInstanceKekSecret(lake.Name)},
				Key:                  "key",
			}},
		})
		optional := true
		secret := lakeControlDB(lake.Name) + "-app"
		ref := func(key string) *corev1.EnvVarSource {
			return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
				Optional:             &optional,
			}}
		}
		env = append(
			env,
			corev1.EnvVar{Name: "LAKE_CONTROL_DB_URI", ValueFrom: ref("uri")},
			corev1.EnvVar{Name: "LAKE_CONTROL_DB_PASSWORD", ValueFrom: ref("password")},
		)
	}
	// LAKE_PUBLIC_HOST names this Lake on every orama_control row lake reads and writes, and lake
	// refuses to start the operator Zetta client without it. spec.env keeps a value it states;
	// otherwise the operator gives spec.host. A routed customer domain (#947) replaces it with the
	// customer's host and names the control rows with LAKE_CONTROL_HOST instead; CEL refuses those
	// variables in spec.env alongside spec.customDomain.
	if custom := lakeCustomDomainEnv(lake); custom != nil {
		env = append(env, custom...)
		for _, variable := range custom {
			reserved[variable.Name] = struct{}{}
		}
	} else if !lakeSpecEnvSetsPublicHost(lake) {
		reserved["LAKE_PUBLIC_HOST"] = struct{}{}
		if lake.Spec.Host != "" {
			env = append(env, corev1.EnvVar{Name: "LAKE_PUBLIC_HOST", Value: lake.Spec.Host})
		}
	}
	if lakeZettaEnabled(lake) {
		zettaURL := fmt.Sprintf(
			"http://%s.%s.svc.cluster.local:%d",
			lakeZettaClientService(lake.Name),
			lakeNamespace(lake),
			zettaAPIPort,
		)
		zettaToken := func() *corev1.EnvVarSource {
			return &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: lakeZettaAdminSecret(lake.Name),
					},
					Key: "token",
				},
			}
		}
		env = append(
			env,
			corev1.EnvVar{Name: "LAKE_DATA_PLANE_URL", Value: zettaURL},
			corev1.EnvVar{Name: "LAKE_DATA_PLANE_TOKEN", ValueFrom: zettaToken()},
		)
		// Deployments (operator#557): the same Zetta carries orama_control, and the token is the
		// one lake already holds for it, so this grants lake nothing new. Given only when the Lake
		// has a public host, because lake exits at startup with the pair and no LAKE_PUBLIC_HOST;
		// FeaturesReady says so when it has none.
		if lakeHasPublicHost(lake) {
			env = append(
				env,
				corev1.EnvVar{Name: "LAKE_OPERATOR_ZETTA_URL", Value: zettaURL},
				corev1.EnvVar{Name: "LAKE_OPERATOR_ZETTA_TOKEN", ValueFrom: zettaToken()},
			)
		}
	}
	if lakeServicesEnabled(lake) {
		// Service endpoints are in service_status; this directory holds each Ready service's
		// connection as <slug>.json.
		env = append(env, corev1.EnvVar{Name: lakeServiceDirEnv, Value: lakeServicesMountPath})
	}
	// Its agents (lake_agents.go): where the library agents are, lake's own in-cluster address,
	// the model, and the token lake presents to agent services.
	env = append(env, lakeAgentEnv(lake, lakeNamespace(lake))...)
	env = append(env, lakeRegionEnv(lake)...)
	for _, name := range lakeRegionEnvNames {
		reserved[name] = struct{}{}
	}
	// With spec.clerk the operator gives the Clerk variables (lakeClerkEnv); spec.env cannot.
	if lake.Spec.Clerk != nil {
		for _, name := range lakeClerkEnvNames {
			reserved[name] = struct{}{}
		}
	}
	// With spec.billing the operator gives LAKE_BILLING (lake_billing.go); spec.env cannot.
	if billing, owned := lakeBillingEnv(lake); owned {
		env = append(env, billing...)
		reserved[lakeBillingEnvName] = struct{}{}
	}
	// With spec.payments the operator gives the Stripe variables (lakePaymentsEnv); spec.env cannot.
	if lake.Spec.Payments != nil {
		for _, name := range lakePaymentsEnvNames {
			reserved[name] = struct{}{}
		}
	}
	// The per-tenant limits (lake_limits.go, operator#782): fixed, the same on every cluster.
	env = append(env, lakeLimitEnv()...)
	for name := range lakeLimitEnvNames() {
		reserved[name] = struct{}{}
	}
	for _, custom := range lake.Spec.Env {
		if _, found := reserved[custom.Name]; found {
			continue
		}
		env = append(env, custom)
	}
	return env
}

// lakeSpecEnvSetsPublicHost is whether spec.env gives LAKE_PUBLIC_HOST a value or a source. An
// empty entry counts as unset, so spec.host replaces it.
func lakeSpecEnvSetsPublicHost(lake *oramav1alpha1.Lake) bool {
	for _, variable := range lake.Spec.Env {
		if variable.Name == "LAKE_PUBLIC_HOST" && (variable.Value != "" || variable.ValueFrom != nil) {
			return true
		}
	}
	return false
}

// lakeHasPublicHost is whether lake starts with a LAKE_PUBLIC_HOST, from spec.env or spec.host.
func lakeHasPublicHost(lake *oramav1alpha1.Lake) bool {
	return lakeSpecEnvSetsPublicHost(lake) || lake.Spec.Host != ""
}

// reconcileLakeInstanceKek mints the non-exported 32-byte root wrapping key once. Disabling the
// owned Postgres removes it with the database; re-enabling creates a new database and key.
func (r *LakeReconciler) reconcileLakeInstanceKek(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	ns string,
	lbls map[string]string,
) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeInstanceKekSecret(lake.Name), Namespace: ns}}
	if !lakePostgresEnabled(lake) {
		return deleteIfExists(ctx, r.Client, sec)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = lbls
		if len(sec.Data["key"]) == 0 {
			key, generateErr := randHex(32)
			if generateErr != nil {
				return generateErr
			}
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data["key"] = []byte(key)
		}
		return controllerutil.SetControllerReference(lake, sec, r.Scheme)
	})
	return err
}

// reconcileLakeAdminToken ensures the Secret holding the Lake's admin bearer token exists: 32
// random bytes as 64 hex chars, minted once and preserved on later reconciles, owned by the Lake.
// lake reads it as LAKE_ADMIN_TOKEN and requires it as `Authorization: Bearer` on every /v1
// capability except server_info, and on POST /mcp tool calls. Never logged.
func (r *LakeReconciler) reconcileLakeAdminToken(ctx context.Context, lake *oramav1alpha1.Lake, ns string, lbls map[string]string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeAdminSecret(lake.Name), Namespace: ns}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = lbls
		if len(sec.Data["token"]) == 0 {
			token, gerr := randHex(32)
			if gerr != nil {
				return gerr
			}
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data["token"] = []byte(token)
		}
		return controllerutil.SetControllerReference(lake, sec, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		logf.FromContext(ctx).Info("provisioned admin token", "secret", lakeAdminSecret(lake.Name),
			"retrieve", fmt.Sprintf("kubectl get secret %s -n %s -o jsonpath='{.data.token}' | base64 -d", lakeAdminSecret(lake.Name), ns))
	}
	return nil
}

// reconcileLakeControlDB provisions lake's control plane as a CloudNativePG Cluster in the lake
// namespace, the same unstructured path OramaCloudV1 uses (controldb.go): bootstrap a database
// and owner named "lake", per-instance storage, owned by the Lake. Disabled tears it down.
func (r *LakeReconciler) reconcileLakeControlDB(ctx context.Context, lake *oramav1alpha1.Lake, ns string, lbls map[string]string, backup lakeBackupConfig) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(lakeControlDB(lake.Name))
	cluster.SetNamespace(ns)
	scheduledBackup := newCNPGScheduledBackup(lake, ns)
	if !lakePostgresEnabled(lake) {
		if err := deleteIfExists(ctx, r.Client, scheduledBackup); err != nil {
			return err
		}
		return deleteIfExists(ctx, r.Client, cluster)
	}
	if err := r.ensureLakeRehearsalSecret(ctx, lake, ns, lbls); err != nil {
		return err
	}
	instances := int64(1)
	if lake.Spec.Postgres.Instances != nil {
		instances = int64(*lake.Spec.Postgres.Instances)
	}
	size := lake.Spec.Postgres.StorageSize
	if size == "" {
		size = "50Gi"
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		cluster.SetLabels(lbls)
		storage := map[string]interface{}{"size": size}
		if sc := lake.Spec.Postgres.StorageClassName; sc != "" {
			storage["storageClass"] = sc
		}
		spec := map[string]interface{}{
			"instances": instances,
			"managed":   map[string]interface{}{"roles": []interface{}{lakeRehearsalRole(lakeRehearsalSecretName(lake))}},
			"storage":   storage,
			"bootstrap": map[string]interface{}{
				"initdb": map[string]interface{}{"database": lakeControlDBName, "owner": lakeControlDBName},
			},
		}
		if lake.Spec.Postgres.Image != "" {
			spec["imageName"] = lake.Spec.Postgres.Image
		}
		if backup.Enabled && backup.Valid {
			spec["backup"] = map[string]interface{}{
				"barmanObjectStore": map[string]interface{}{
					"destinationPath": fmt.Sprintf("s3://%s/%s", strings.Trim(backup.Bucket, "/"), strings.Trim(backup.Path, "/")),
					"endpointURL":     backup.Endpoint,
					"s3Credentials": map[string]interface{}{
						"accessKeyId":     map[string]interface{}{"name": backup.SecretName, "key": "accessKeyId"},
						"secretAccessKey": map[string]interface{}{"name": backup.SecretName, "key": "secretAccessKey"},
					},
					"wal": map[string]interface{}{"compression": "gzip"},
				},
				"retentionPolicy": backup.Retention,
			}
		}
		cluster.Object["spec"] = spec
		// CNPG propagates inherited metadata to instances and bootstrap Jobs. Do not copy Lake's
		// application labels: its Service must not select the database pods.
		if err := unstructured.SetNestedStringMap(cluster.Object, lakeInstancePodLabels(lake), "spec", "inheritedMetadata", "labels"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(lake, cluster, r.Scheme)
	})
	if err != nil {
		return err
	}
	if !backup.Enabled || !backup.Valid {
		return deleteIfExists(ctx, r.Client, scheduledBackup)
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, scheduledBackup, func() error {
		scheduledBackup.SetLabels(lbls)
		scheduledBackup.Object["spec"] = map[string]interface{}{
			"schedule":             backup.Schedule,
			"backupOwnerReference": "self",
			"cluster":              map[string]interface{}{"name": lakeControlDB(lake.Name)},
			"immediate":            true,
		}
		return controllerutil.SetControllerReference(lake, scheduledBackup, r.Scheme)
	})
	return err
}

func newCNPGScheduledBackup(lake *oramav1alpha1.Lake, ns string) *unstructured.Unstructured {
	backup := &unstructured.Unstructured{}
	backup.SetGroupVersionKind(cnpgScheduledBackupGVK)
	if lake != nil {
		backup.SetName(lakeControlDBScheduledBackup(lake.Name))
		backup.SetNamespace(ns)
	}
	return backup
}

// resolveLakeBackupConfig validates enabled backup settings without an admission webhook. Invalid
// user configuration is reflected in Lake status rather than causing a panic or reconcile error.
func (r *LakeReconciler) resolveLakeBackupConfig(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (lakeBackupConfig, error) {
	config := lakeBackupConfig{Enabled: lakePostgresBackupEnabled(lake)}
	if !config.Enabled {
		return config, nil
	}
	backup := lake.Spec.Postgres.Backup
	if backup.Destination.Bucket == "" {
		config.Reason = "BackupBucketRequired"
		config.Message = "spec.postgres.backup.destination.bucket is required when backups are enabled"
		return config, nil
	}
	if backup.SecretRef.Name == "" {
		config.Reason = "BackupSecretRequired"
		config.Message = "spec.postgres.backup.secretRef is required when backups are enabled"
		return config, nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: backup.SecretRef.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			config.Reason = "BackupSecretNotFound"
			config.Message = fmt.Sprintf("backup Secret %q was not found in namespace %q", backup.SecretRef.Name, ns)
			return config, nil
		}
		return config, err
	}
	for _, key := range []string{"s3ApiEndpoint", "accessKeyId", "secretAccessKey"} {
		if len(secret.Data[key]) == 0 {
			config.Reason = "InvalidBackupSecret"
			config.Message = fmt.Sprintf("backup Secret %q must contain a non-empty %s key", backup.SecretRef.Name, key)
			return config, nil
		}
	}

	config.Valid = true
	config.Bucket = backup.Destination.Bucket
	config.Path = backup.Destination.Path
	if config.Path == "" {
		config.Path = "lake-" + lake.Name + "/controldb"
	}
	config.Endpoint = backup.Destination.EndpointURL
	if config.Endpoint == "" {
		config.Endpoint = string(secret.Data["s3ApiEndpoint"])
	}
	config.Schedule = backup.Schedule
	if config.Schedule == "" {
		config.Schedule = "0 0 3 * * *"
	}
	config.Retention = backup.Retention
	if config.Retention == "" {
		config.Retention = "30d"
	}
	config.SecretName = backup.SecretRef.Name
	return config, nil
}

func (r *LakeReconciler) lakeBackupStatus(ctx context.Context, lake *oramav1alpha1.Lake, ns string, config lakeBackupConfig) (bool, *metav1.Time, metav1.Condition, error) {
	condition := metav1.Condition{Type: "BackupReady", ObservedGeneration: lake.Generation}
	if !config.Enabled {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BackupDisabled", "control-plane backups are disabled"
		return false, nil, condition, nil
	}
	if !config.Valid {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, config.Reason, config.Message
		return false, nil, condition, nil
	}
	if !lakePostgresEnabled(lake) {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "PostgresDisabled", "control-plane Postgres is disabled"
		return false, nil, condition, nil
	}

	scheduledBackup := newCNPGScheduledBackup(lake, ns)
	if err := r.Get(ctx, client.ObjectKeyFromObject(scheduledBackup), scheduledBackup); err != nil {
		if apierrors.IsNotFound(err) {
			condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "ScheduledBackupPending", "waiting for the CNPG ScheduledBackup"
			return false, nil, condition, nil
		}
		return false, nil, condition, err
	}

	backups := &unstructured.UnstructuredList{}
	backups.SetGroupVersionKind(cnpgBackupListGVK)
	if err := r.List(ctx, backups, client.InNamespace(ns), client.MatchingLabels{"cnpg.io/cluster": lakeControlDB(lake.Name)}); err != nil {
		return false, nil, condition, err
	}
	if len(backups.Items) == 0 {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BackupPending", "waiting for the first CNPG Backup"
		return false, nil, condition, nil
	}
	newest := &backups.Items[0]
	for i := 1; i < len(backups.Items); i++ {
		if backups.Items[i].GetCreationTimestamp().After(newest.GetCreationTimestamp().Time) {
			newest = &backups.Items[i]
		}
	}
	phase, _, _ := unstructured.NestedString(newest.Object, "status", "phase")
	lastBackupTime, err := cnpgBackupCompletionTime(newest)
	if err != nil {
		return false, nil, condition, err
	}
	if phase == "completed" {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "BackupCompleted", "newest CNPG Backup completed"
		return true, lastBackupTime, condition, nil
	}
	condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BackupNotCompleted", fmt.Sprintf("newest CNPG Backup phase is %q", phase)
	return false, lastBackupTime, condition, nil
}

func cnpgBackupCompletionTime(backup *unstructured.Unstructured) (*metav1.Time, error) {
	stoppedAt, found, err := unstructured.NestedString(backup.Object, "status", "stoppedAt")
	if err != nil || !found || stoppedAt == "" {
		return nil, err
	}
	parsed, err := time.Parse(time.RFC3339, stoppedAt)
	if err != nil {
		return nil, fmt.Errorf("parse CNPG Backup status.stoppedAt: %w", err)
	}
	return &metav1.Time{Time: parsed}, nil
}

func lakeBackupTimesEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Time.Equal(b.Time)
}

// reconcileLakeControlDBSchema declares lake's control-plane schema as an owned PostgresSchema.
//
// For an image whose manifest is supported (lake_manifest.go, #936) the PostgresSchema runs
// `lake migrate --wait-seconds 600` from the image itself (by digest when known), with
// LAKE_CONTROL_DB_URI from the CNPG app Secret. Its version is the image digest, so the migration
// runs once per digest, and the Deployment waits for it.
//
// For an image that predates `lake manifest` (manifest nil or unsupported) it applies the vendored
// schema the way OramaCloudV1 applies the dashboard's (controldb.go): the SQL rendered into an
// owned ConfigMap, whose idempotent psql Job waits for the DB, skips if the guard table and every
// recorded migration exist, and applies in one transaction. The version is a content hash of the
// SQL and guard, so changing either starts a new apply.
//
// With Postgres disabled both objects are torn down.
func (r *LakeReconciler) reconcileLakeControlDBSchema(
	ctx context.Context, lake *oramav1alpha1.Lake, ns string, lbls map[string]string, manifest *oramav1alpha1.LakeImageManifest,
) error {
	cmName := lakeControlDB(lake.Name) + "-migrations"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns}}
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: lakeControlDB(lake.Name) + "-schema", Namespace: ns}}
	if !lakePostgresEnabled(lake) {
		if err := deleteIfExists(ctx, r.Client, ps); err != nil {
			return err
		}
		return deleteIfExists(ctx, r.Client, cm)
	}
	if lakeManifestSupported(manifest) {
		image := manifest.ImageRef
		if image == "" {
			image = lake.Spec.Image
		}
		var pullSecrets []corev1.LocalObjectReference
		if lake.Spec.PullSecretName != "" {
			pullSecrets = []corev1.LocalObjectReference{{Name: lake.Spec.PullSecretName}}
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ps, func() error {
			ps.Labels = withLabels(lbls, lakeInstancePodLabels(lake))
			ps.Spec = oramav1alpha1.PostgresSchemaSpec{
				ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: lakeControlDB(lake.Name) + "-app", Key: "uri"},
				Version:             lakeImageSchemaVersion(lake, manifest),
				// Informational here: lake migrate checks the newest guard itself.
				GuardTable: manifest.GuardTable,
				Image:      image,
				Migrate: &oramav1alpha1.PostgresSchemaMigrate{
					Args:            slices.Clone(manifest.MigrateArgs),
					DatabaseEnv:     manifest.DatabaseEnv,
					FailOnExitCodes: slices.Clone(lakeMigrateFailOnExitCodes),
					RunAsUser:       ptrInt64(lakeRunAsUser),
					RunAsGroup:      ptrInt64(lakeRunAsGroup),
				},
				ImagePullSecrets: pullSecrets,
			}
			return controllerutil.SetControllerReference(lake, ps, r.Scheme)
		}); err != nil {
			return err
		}
		// The image carries its schema: the vendored copy is no longer mounted.
		return deleteIfExists(ctx, r.Client, cm)
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = lbls
		cm.Data = map[string]string{"schema.sql": lakeSchemaSQL}
		return controllerutil.SetControllerReference(lake, cm, r.Scheme)
	}); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ps, func() error {
		ps.Labels = withLabels(lbls, lakeInstancePodLabels(lake))
		ps.Spec = oramav1alpha1.PostgresSchemaSpec{
			ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: lakeControlDB(lake.Name) + "-app", Key: "uri"},
			SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: cmName, Key: "schema.sql"},
			Version:             lakeSchemaVersion(),
			// The guard is the table the NEWEST migration creates: with --single-transaction it
			// exists iff the whole (idempotent) file applied, and a new migration = a new guard, so
			// the next Job re-runs the file and adds only what is missing.
			GuardTable: lakeSchemaGuardTable,
			Image:      pinnedPostgres18Image,
		}
		return controllerutil.SetControllerReference(lake, ps, r.Scheme)
	})
	return err
}

// lakeSchemaVersion binds the SQL bytes and idempotency guard to the same apply attempt.
func lakeSchemaVersion() string {
	return schemaVersion(lakeSchemaSQL + "\n" + lakeSchemaGuardTable)
}

// lakeControlDBStatus reads CNPG readiness best-effort; not-found (or no CNPG) counts as not ready.
func (r *LakeReconciler) lakeControlDBStatus(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (bool, string) {
	if !lakePostgresEnabled(lake) {
		return false, ""
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lakeControlDB(lake.Name)}, cluster); err != nil {
		return false, "0/?"
	}
	ri, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	inst, _, _ := unstructured.NestedInt64(cluster.Object, "status", "instances")
	desired := int64(1)
	if lake.Spec.Postgres.Instances != nil {
		desired = int64(*lake.Spec.Postgres.Instances)
	}
	return desired > 0 && ri >= desired, fmt.Sprintf("%d/%d", ri, inst)
}

// reconcileLakeIngress exposes the lake at spec.host via an nginx Ingress that proxies the whole host
// to the lake Service and, when spec.dnsSecretRef is set, an OWNED CloudflareDNSRecord (proxied,
// Host -> the ingress LoadBalancer address). Same contract as OramaZettaCluster: the child controller
// owns the Cloudflare API call and its finalizer, so this loop never touches Cloudflare. SAFE no-op
// unless spec.ingressEnabled AND spec.host; when gated off, an Ingress or DNS record created earlier
// is torn down. The DNS record waits until the ingress controller has assigned an address; the
// owned Ingress's status change re-queues the Lake, so no polling is needed.
func (r *LakeReconciler) reconcileLakeIngress(ctx context.Context, lake *oramav1alpha1.Lake, ns, svcName string, lbls map[string]string) error {
	ingName := svcName
	dnsName := svcName + "-dns"
	dnsRecord := func() *oramav1alpha1.CloudflareDNSRecord {
		return &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: dnsName, Namespace: ns}}
	}
	// The apex host's record (decision 64): the region picker's host, beside the Lake's own.
	apexRecord := func() *oramav1alpha1.CloudflareDNSRecord {
		return &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: svcName + "-apex-dns", Namespace: ns}}
	}
	apex := lakeApexHost(lake)
	// Origin TLS (operator#578, #632): the Issuer always exists, because the Lake's apps and
	// environments use it even without the Lake's own Ingress; the zone policy exists whenever the
	// Lake has a DNS credential. The Issuer reads the zone's name from the policy, to solve DNS-01
	// for names in the zone and HTTP-01 for the rest.
	if err := reconcileLakeEdgePolicy(ctx, r.Client, r.Scheme, lake, lbls); err != nil {
		return fmt.Errorf("edge zone policy: %w", err)
	}
	edge, err := readLakeEdge(ctx, r.Client, lake)
	if err != nil {
		return err
	}
	if err := reconcileLakeOriginIssuer(ctx, r.Client, r.Scheme, lake, lbls, edge.dnsZone); err != nil {
		return fmt.Errorf("origin TLS issuer: %w", err)
	}
	caSecret := svcName + "-edge-client-ca"
	if !lake.Spec.IngressEnabled || lake.Spec.Host == "" {
		if err := deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: ns}}); err != nil {
			return err
		}
		if err := r.reconcileLakeWildcardIngresses(ctx, lake, ns, svcName, lbls, "", lakeEdge{}, ""); err != nil {
			return err
		}
		if err := deleteIfExists(ctx, r.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: caSecret, Namespace: ns}}); err != nil {
			return err
		}
		if err := deleteIfExists(ctx, r.Client, dnsRecord()); err != nil {
			return err
		}
		if err := deleteIfExists(ctx, r.Client, apexRecord()); err != nil {
			return err
		}
		return r.deleteLakePreviewDNSRecords(ctx, lake, ns, "")
	}
	hosts := []string{lake.Spec.Host}
	if apex != "" {
		hosts = append(hosts, apex)
	}
	// No HTTP-only Ingress: without a hand-supplied certificate every host is issued by the Lake's
	// Issuer, through DNS-01 in the credential's zone and HTTP-01 otherwise.
	issuer := ""
	if lake.Spec.TLS.SecretName == "" {
		issuer = lakeOriginIssuerName(lake)
	}
	caRef, err := reconcileEdgeClientCA(ctx, r.Client, ns, caSecret, edge, func(secret *corev1.Secret) error {
		secret.Labels = lbls
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	if err != nil {
		return err
	}
	prefix := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = lbls
		ing.Annotations = lakeIngressAnnotations()
		ing.Spec.IngressClassName = strPtr(ingressClassOrDefault(lake.Spec.IngressClassName))
		ing.Spec.Rules = nil
		for _, host := range hosts {
			ing.Spec.Rules = append(ing.Spec.Rules, networkingv1.IngressRule{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &prefix,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: svcName, Port: networkingv1.ServiceBackendPort{Number: lakeServicePort}}},
					}},
				}},
			})
		}
		if lake.Spec.TLS.SecretName != "" {
			ing.Spec.TLS = []networkingv1.IngressTLS{{
				Hosts:      hosts,
				SecretName: lake.Spec.TLS.SecretName,
			}}
		} else if err := applyIngressTLS(ing, issuer); err != nil {
			return err
		}
		applyLakeEdgeAnnotations(ing, edge, caRef)
		return controllerutil.SetControllerReference(lake, ing, r.Scheme)
	}); err != nil {
		return err
	}
	if err := r.reconcileLakeWildcardIngresses(ctx, lake, ns, svcName, lbls, issuer, edge, caRef); err != nil {
		return err
	}
	if lake.Spec.DNSSecretRef == nil || lake.Spec.DNSSecretRef.Name == "" {
		if err := deleteIfExists(ctx, r.Client, dnsRecord()); err != nil {
			return err
		}
		if err := deleteIfExists(ctx, r.Client, apexRecord()); err != nil {
			return err
		}
		return r.deleteLakePreviewDNSRecords(ctx, lake, ns, "")
	}
	if apex == "" {
		if err := deleteIfExists(ctx, r.Client, apexRecord()); err != nil {
			return err
		}
	}
	previewDNSName := ""
	if lake.Spec.PreviewWildcard == "" {
		previewDNSName = ""
	} else {
		previewDNSName = lakePreviewDNSRecordName(svcName, lake.Spec.PreviewWildcard)
	}
	if err := r.deleteLakePreviewDNSRecords(ctx, lake, ns, previewDNSName); err != nil {
		return err
	}
	target := ingressLoadBalancerTarget(ing)
	if target == "" {
		return nil // address still pending; the Ingress status update re-queues us
	}
	rec := dnsRecord()
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
		rec.Labels = lbls
		rec.Spec.SecretRef, rec.Spec.Name, rec.Spec.Content, rec.Spec.Proxied = lake.Spec.DNSSecretRef.Name, lake.Spec.Host, target, true
		return controllerutil.SetControllerReference(lake, rec, r.Scheme)
	}); err != nil {
		return err
	}
	if apex != "" {
		apexRec := apexRecord()
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, apexRec, func() error {
			apexRec.Labels = lbls
			apexRec.Spec.SecretRef, apexRec.Spec.Name, apexRec.Spec.Content, apexRec.Spec.Proxied = lake.Spec.DNSSecretRef.Name, apex, target, true
			return controllerutil.SetControllerReference(lake, apexRec, r.Scheme)
		}); err != nil {
			return err
		}
	}
	if lake.Spec.PreviewWildcard == "" {
		return nil
	}
	preview := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: previewDNSName, Namespace: ns}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, preview, func() error {
		preview.Labels = make(map[string]string, len(lbls)+1)
		for key, value := range lbls {
			preview.Labels[key] = value
		}
		preview.Labels["orama.cloud/dns-purpose"] = "preview-wildcard"
		preview.Spec.SecretRef = lake.Spec.DNSSecretRef.Name
		preview.Spec.Name = "*." + strings.TrimPrefix(lake.Spec.PreviewWildcard, "*.")
		preview.Spec.Content = target
		preview.Spec.Proxied = true
		return controllerutil.SetControllerReference(lake, preview, r.Scheme)
	})
	return err
}

// lakeIngressAnnotations are the Lake Ingress's own nginx settings. TLS, the redirect and the
// client-certificate requirement are added by applyIngressTLS and applyLakeEdgeAnnotations.
func lakeIngressAnnotations() map[string]string {
	return map[string]string{
		"nginx.ingress.kubernetes.io/proxy-body-size":         "64m",
		"nginx.ingress.kubernetes.io/proxy-buffering":         "off",
		"nginx.ingress.kubernetes.io/proxy-request-buffering": "off",
	}
}

// reconcileLakeWildcardIngresses keeps one Ingress per wildcard the Lake's apps and previews use
// (*.<apps wildcard>, *.<preview wildcard>) while the Lake serves them (lakeServesWildcards). Each
// requests the wildcard's certificate, which the apps and environments under it copy, and makes the
// origin present that certificate for any name under the wildcard, so the zone's origin check
// passes for the wildcard DNS record. A name with no app or environment is sent to the Lake's host
// (a permanent redirect), never to a workload. An empty issuer removes them.
func (r *LakeReconciler) reconcileLakeWildcardIngresses(ctx context.Context, lake *oramav1alpha1.Lake, ns, svcName string, lbls map[string]string, issuer string, edge lakeEdge, caRef string) error {
	type wildcard struct{ name, suffix, class string }
	appsClass := lake.Spec.Apps.IngressClass
	if appsClass == "" {
		appsClass = "nginx"
	}
	apps := lakeAppsWildcard(lake)
	preview := strings.TrimPrefix(lake.Spec.PreviewWildcard, "*.")
	wildcards := []wildcard{
		{name: svcName + "-apps-wildcard", suffix: apps, class: appsClass},
		{name: svcName + "-preview-wildcard", suffix: preview, class: ingressClassOrDefault(lake.Spec.IngressClassName)},
	}
	if preview == apps {
		wildcards[1].suffix = ""
	}
	prefix := networkingv1.PathTypePrefix
	for _, w := range wildcards {
		ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: w.name, Namespace: ns}}
		if issuer == "" || w.suffix == "" || !lakeServesWildcards(lake) {
			if err := deleteIfExists(ctx, r.Client, ing); err != nil {
				return err
			}
			continue
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
			ing.Labels = lbls
			ing.Annotations = map[string]string{annotationNginxPermanentRedirect: "https://" + lake.Spec.Host + "/"}
			ing.Spec.IngressClassName = strPtr(w.class)
			ing.Spec.Rules = []networkingv1.IngressRule{{
				Host: "*." + w.suffix,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &prefix,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: svcName, Port: networkingv1.ServiceBackendPort{Number: lakeServicePort}}},
					}},
				}},
			}}
			if err := applyIngressTLS(ing, issuer); err != nil {
				return err
			}
			applyLakeEdgeAnnotations(ing, edge, caRef)
			return controllerutil.SetControllerReference(lake, ing, r.Scheme)
		}); err != nil {
			return fmt.Errorf("wildcard ingress %s: %w", w.name, err)
		}
	}
	return nil
}

func lakePreviewDNSRecordName(serviceName, wildcard string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimPrefix(wildcard, "*."))))[:8]
	return serviceName + "-preview-" + hash
}

func (r *LakeReconciler) deleteLakePreviewDNSRecords(ctx context.Context, lake *oramav1alpha1.Lake, namespace, keepName string) error {
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := r.List(ctx, &records, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/instance": lake.Name,
		"orama.cloud/dns-purpose":    "preview-wildcard",
	}); err != nil {
		return err
	}
	for i := range records.Items {
		if records.Items[i].Name == keepName {
			continue
		}
		if err := r.Delete(ctx, &records.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// lakeRequestsForStoreClaim maps both managed and existing claim changes back to cluster-scoped
// Lakes. Listing uses the manager cache, so a steady claim creates no polling traffic.
func (r *LakeReconciler) lakeRequestsForStoreClaim(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	pvc, ok := object.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	var lakes oramav1alpha1.LakeList
	if err := r.List(ctx, &lakes); err != nil {
		logf.FromContext(ctx).Error(err, "list Lakes for store claim", "claim", client.ObjectKeyFromObject(pvc))
		return nil
	}
	requests := make([]reconcile.Request, 0, len(lakes.Items))
	for i := range lakes.Items {
		lake := &lakes.Items[i]
		ns := lake.Spec.Namespace
		if ns == "" {
			ns = "lake-dev"
		}
		if ns != pvc.Namespace || lakeStoreClaimName(lake) != pvc.Name {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: lake.Name}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *LakeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Lake{}).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(
			&corev1.PersistentVolumeClaim{},
			handler.EnqueueRequestsFromMapFunc(r.lakeRequestsForStoreClaim),
		).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Owns(&oramav1alpha1.EdgeZonePolicy{}).
		Owns(&oramav1alpha1.PaymentProvider{}).
		Owns(&oramav1alpha1.PaymentWebhookEndpoint{}).
		Owns(&oramav1alpha1.PaymentCatalog{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.lakeRequestsForPaymentsSecret)).
		Owns(&oramav1alpha1.LakeSealedSecret{}).
		Owns(&oramav1alpha1.NetworkSandbox{}).
		Watches(&oramav1alpha1.LakeApp{}, handler.EnqueueRequestsFromMapFunc(lakeRequestsForAppSandbox)).
		Owns(&oramav1alpha1.OramaZettaCluster{}).
		Owns(&oramav1alpha1.PostgresSchema{}).
		Owns(newCNPGScheduledBackup(nil, ""))
	if r.ServiceEvents != nil {
		builder = builder.WatchesRawSource(source.Channel(r.ServiceEvents, &handler.EnqueueRequestForObject{}))
	}
	return builder.Named("lake").Complete(r)
}

// lakeRunAsUser and lakeRunAsGroup are the uid and gid in the oramasearch/lake image.
const (
	lakeRunAsUser  int64 = 10001
	lakeRunAsGroup int64 = 10001
)

func ptrInt64(v int64) *int64 { return &v }

func ptrFSGroupChangePolicy(v corev1.PodFSGroupChangePolicy) *corev1.PodFSGroupChangePolicy {
	return &v
}
