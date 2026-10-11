/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Managed services (liquid-bridge decision 46; the contract is lake's
// docs/control-plane/services.md). lake writes the services table in its control Postgres and never
// talks to Kubernetes; the operator reads that table, creates one Lake-controlled
// OramaZettaCluster or CloudNativePG Cluster named <lake>-<slug> per present service, deletes absent
// ones, copies each Ready service's connection into Secret lake-<lake>-services, and writes what it
// observed to service_status. The workspace reconciler exclusively owns the Zetta service named by
// workspaces.zetta_service_id and its status, so this poll never writes that Zetta; it only copies
// its connection into the services Secret, of which it is the one writer. The other kinds of the catalog (decision
// 47: Kafka, ClickHouse, TiDB, Cassandra, Valkey) are written for their upstream operators by
// lake_service_catalog.go, and SQLite, DuckDB and Polars (decision 47, amended; lake#195), which the
// operator runs itself behind lake-sqlite, lake-duckdb and lake-polars, go through the same code
// (lake_service_wrapper.go).
const (
	ServicePhaseProvisioning = "Provisioning"
	ServicePhaseReady        = "Ready"
	ServicePhaseFailed       = "Failed"
	ServicePhaseDeleting     = "Deleting"
	ServicePhaseDeleted      = "Deleted"

	serviceKindZetta      = "zetta"
	serviceKindPostgres   = "postgres"
	serviceDesiredPresent = "present"
	serviceDesiredAbsent  = "absent"

	// Labels and annotation on every service resource. serviceIDLabel is the ownership key.
	serviceIDLabel            = "orama.cloud/service-id"
	serviceKindLabel          = "orama.cloud/service-kind"
	serviceLakeLabel          = "orama.cloud/lake"
	serviceCustomerAnnotation = "orama.cloud/customer-id"
	// serviceCustomerLabel carries the customer id verbatim when it is a valid label value and a
	// hash otherwise; the annotation always carries it verbatim.
	serviceCustomerLabel = "orama.cloud/customer-id"
	// workspaceIDLabel marks a decision-45 workspace cluster; its value is the ws- slug.
	workspaceIDLabel = "orama.cloud/workspace-id"

	lakeServicesMountPath = "/var/run/lake/services"
	lakeServicesVolume    = "services"
	lakeServiceDirEnv     = "LAKE_SERVICE_DIR"

	serviceDefaultInstances = 1
	serviceMaxInstances     = 3
	serviceDefaultStorageGi = 10
	serviceMaxStorageGi     = 100

	servicePostgresDatabase = "app"
	servicePostgresOwner    = "app"
	servicePostgresPort     = 5432

	// serviceDefaultMemoryReq and serviceDefaultMemoryLimit size a Zetta service node when
	// spec.workspaces.resources is unset; a service idles most of the time.
	serviceDefaultMemoryReq   = "64Mi"
	serviceDefaultMemoryLimit = "1Gi"
	// serviceLongestNameSuffix is the longest suffix appended to <lake>-<slug> (the Zetta headless
	// Service); the name check keeps every derived name inside the 63-character DNS label limit.
	serviceLongestNameSuffix = "-zetta-headless"
	// zettaDataClaimTemplate is the volumeClaimTemplate name of the OramaZettaCluster StatefulSet;
	// its PVCs are data-<cluster>-zetta-<ordinal>.
	zettaDataClaimTemplate = "data"
	// cnpgClusterLabel is the label CloudNativePG puts on a Cluster's pods and PVCs.
	cnpgClusterLabel = "cnpg.io/cluster"

	// servicePassTimeout bounds one poll: the database connection and every call made through it.
	servicePassTimeout = 30 * time.Second
)

var (
	serviceSlugPattern      = regexp.MustCompile(`^(svc|ws)-[0-9a-f]{12}$`)
	cnpgClusterListGVK      = cnpgClusterGVK.GroupVersion().WithKind("ClusterList")
	servicePostgresSelector = func(name string) client.MatchingLabels { return client.MatchingLabels{cnpgClusterLabel: name} }
)

// LakeService is one row of lake's services table joined to its service_status row.
type LakeService struct {
	ID                 string
	CustomerID         string
	WorkspaceID        string
	WorkspaceNamespace string
	Kind               string
	Name               string
	Slug               string
	IsDefault          bool
	// Spec is the raw jsonb spec: {"instances": 1-3, "storage_gi": 1-100}.
	Spec       string
	Desired    string
	Generation int64
	// WorkspaceZetta is true when this service is the Zetta named by its workspace's
	// zetta_service_id. LakeWorkspacesReconciler is its only resource and status writer; the
	// services poll only delivers its connection into the services Secret.
	WorkspaceZetta bool
	// Status is the current service_status row, nil when the operator has written none.
	Status *LakeServiceStatus
}

// LakeServiceStatus is one service_status row, written only by the operator.
type LakeServiceStatus struct {
	ServiceID          string
	Phase              string
	Message            string
	Endpoint           string
	ObservedGeneration int64
}

// LakeServiceStore reads the services and stores tables and upserts service_status and
// store_status. The pgx implementation talks to lake's control Postgres; tests substitute an
// in-memory fake. ListStores returns errStoresTableMissing while lake's schema is older than
// 0028_stores.
type LakeServiceStore interface {
	ListServices(ctx context.Context) ([]LakeService, error)
	UpsertServiceStatus(ctx context.Context, status LakeServiceStatus) error
	ListStores(ctx context.Context) ([]LakeStore, error)
	UpsertStoreStatus(ctx context.Context, status LakeStoreStatus) error
	ListAgentLaunches(ctx context.Context) ([]LakeAgentLaunch, error)
	UpsertAgentLaunchStatus(ctx context.Context, status LakeAgentLaunchStatus) error
	ListAgentSchedules(ctx context.Context) ([]LakeAgentSchedule, error)
	UpsertAgentScheduleStatus(ctx context.Context, status LakeAgentScheduleStatus) error
	UpsertAgentScheduleRun(ctx context.Context, run LakeAgentScheduleRun) error
	ListBuilds(ctx context.Context) ([]LakeBuild, error)
	UpdateBuildStatus(ctx context.Context, status LakeBuildStatus) error
	// The workload resources half (lake_workload_resources.go, lake#643). ListWorkloadResources
	// returns errWorkloadResourcesTableMissing while lake's schema is older than
	// 0075_workload_resources.
	ListWorkloadResources(ctx context.Context) ([]WorkloadResources, error)
	ListWorkloadObjects(ctx context.Context, ids []string, live bool) ([]WorkloadObject, error)
	// ListUnmeteredJobs is every finished build, job-mode agent launch and agent cron run that
	// ran and has no workload_resources row (lake#658), for the backfill.
	ListUnmeteredJobs(ctx context.Context) ([]WorkloadObject, error)
	UpsertWorkloadResources(ctx context.Context, row WorkloadResources) error
	DeleteWorkloadResources(ctx context.Context, kind, id string) error
	Close(ctx context.Context) error
}

// LakeServiceStoreOpener connects a LakeServiceStore with the uri from the control DB app Secret.
type LakeServiceStoreOpener func(ctx context.Context, uri string) (LakeServiceStore, error)

type lakeServiceSpec struct {
	Instances int32
	StorageGi int32
}

// serviceResult is what one pass observed for one service. credential is set only when the
// resource is ready and its connection could be read.
type serviceResult struct {
	phase         string
	message       string
	endpoint      string
	resourceReady bool
	credential    []byte
}

// lakeServicesEnabled: managed services have their own switch. Workspaces use a separate table,
// listener and reconciler.
func lakeServicesEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Services.Enabled && lakePostgresEnabled(lake)
}

// lakeServicesSecret holds one <slug>.json connection per Ready service.
func lakeServicesSecret(name string) string { return "lake-" + name + "-services" }

// lakeWorkspacesSecret is the decision-45 credential Secret the services Secret replaces.
func lakeWorkspacesSecret(name string) string { return "lake-" + name + "-workspaces" }

func serviceResourceName(lake, slug string) string { return lake + "-" + slug }

func serviceCredentialKey(slug string) string { return slug + ".json" }

// serviceZettaEndpoint is the in-cluster client URL of a Zetta service (zettaNames: <cluster>-zetta).
func serviceZettaEndpoint(lake, slug, namespace string) string {
	return fmt.Sprintf("http://%s-zetta.%s.svc.cluster.local:%d", serviceResourceName(lake, slug), namespace, zettaAPIPort)
}

func servicePostgresHost(lake, slug, namespace string) string {
	return fmt.Sprintf("%s-rw.%s.svc.cluster.local", serviceResourceName(lake, slug), namespace)
}

func servicePostgresEndpoint(lake, slug, namespace string) string {
	return fmt.Sprintf("%s:%d", servicePostgresHost(lake, slug, namespace), servicePostgresPort)
}

func serviceZettaImage(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Services.ZettaImage != "" {
		return lake.Spec.Services.ZettaImage
	}
	return lake.Spec.Workspaces.Image
}

// serviceImage is the image of a kind the Orama operator runs itself, "" for the others: the
// Lake's spec.services.<kind>Image, then the operator's --lake-<kind>-image flag, then the built-in
// default (DefaultLakeSQLiteImage, DefaultLakeDuckDBImage, DefaultLakePolarsImage).
func (r *LakeReconciler) serviceImage(lake *oramav1alpha1.Lake, kind string) string {
	var lakeImage, flagImage, fallback string
	switch kind {
	case serviceKindSQLite:
		lakeImage, flagImage, fallback = lake.Spec.Services.SQLiteImage, r.SQLiteImage, DefaultLakeSQLiteImage
	case serviceKindDuckDB:
		lakeImage, flagImage, fallback = lake.Spec.Services.DuckDBImage, r.DuckDBImage, DefaultLakeDuckDBImage
	case serviceKindPolars:
		lakeImage, flagImage, fallback = lake.Spec.Services.PolarsImage, r.PolarsImage, DefaultLakePolarsImage
	default:
		return ""
	}
	if lakeImage != "" {
		return lakeImage
	}
	if flagImage != "" {
		return flagImage
	}
	return fallback
}

// serviceQuota is spec.services.maxServices; 0 is no limit.
func serviceQuota(lake *oramav1alpha1.Lake) int {
	return int(lake.Spec.Services.MaxServices)
}

// serviceZettaResources is spec.workspaces.resources, or the small default: 64Mi memory request,
// 1Gi memory limit, no CPU request.
func serviceZettaResources(lake *oramav1alpha1.Lake) *corev1.ResourceRequirements {
	if lake.Spec.Workspaces.Resources != nil {
		return lake.Spec.Workspaces.Resources.DeepCopy()
	}
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(serviceDefaultMemoryReq)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(serviceDefaultMemoryLimit)},
	}
}

func serviceCustomerLabelValue(customerID string) string {
	if len(validation.IsValidLabelValue(customerID)) == 0 {
		return customerID
	}
	return fmt.Sprintf("sha256-%x", sha256.Sum256([]byte(customerID)))[:23]
}

// isLakeServiceCluster reports whether a Lake-controlled OramaZettaCluster is a managed service or
// workspace data plane rather than the Lake's own data plane.
func isLakeServiceCluster(cluster *oramav1alpha1.OramaZettaCluster) bool {
	return cluster.Labels[serviceIDLabel] != "" || cluster.Labels[workspaceLabel] != "" || cluster.Labels[workspaceIDLabel] != ""
}

// parseServiceSpec reads spec {instances, storage_gi}, applying the contract's defaults and ranges.
func parseServiceSpec(raw string) (lakeServiceSpec, error) {
	spec := lakeServiceSpec{Instances: serviceDefaultInstances, StorageGi: serviceDefaultStorageGi}
	if strings.TrimSpace(raw) == "" {
		return spec, nil
	}
	var fields struct {
		Instances *int64 `json:"instances"`
		StorageGi *int64 `json:"storage_gi"`
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return spec, fmt.Errorf("spec is not a JSON object with integer instances and storage_gi: %v", err)
	}
	if fields.Instances != nil {
		if *fields.Instances < 1 || *fields.Instances > serviceMaxInstances {
			return spec, fmt.Errorf("spec.instances must be between 1 and %d", serviceMaxInstances)
		}
		spec.Instances = int32(*fields.Instances)
	}
	if fields.StorageGi != nil {
		if *fields.StorageGi < 1 || *fields.StorageGi > serviceMaxStorageGi {
			return spec, fmt.Errorf("spec.storage_gi must be between 1 and %d", serviceMaxStorageGi)
		}
		spec.StorageGi = int32(*fields.StorageGi)
	}
	return spec, nil
}

// validateService rejects a row the operator will not act on. The table's own constraints make
// most of these unreachable; they are checked again because a wrong name here would touch another
// resource.
func validateService(lake *oramav1alpha1.Lake, svc *LakeService) (lakeServiceSpec, error) {
	switch {
	case !serviceSlugPattern.MatchString(svc.Slug):
		return lakeServiceSpec{}, fmt.Errorf("slug must match %s", serviceSlugPattern.String())
	case !knownServiceKind(svc.Kind):
		return lakeServiceSpec{}, fmt.Errorf("%s", unknownServiceKindMessage(svc.Kind))
	case svc.IsDefault && svc.Kind != serviceKindZetta:
		return lakeServiceSpec{}, fmt.Errorf("only a zetta service can be the default")
	case svc.CustomerID == "":
		return lakeServiceSpec{}, fmt.Errorf("customer_id is required")
	case svc.WorkspaceID == "" || svc.WorkspaceNamespace == "":
		return lakeServiceSpec{}, fmt.Errorf("workspace_id and its reconciled namespace are required")
	case len(validation.IsDNS1123Label(svc.WorkspaceNamespace)) != 0:
		return lakeServiceSpec{}, fmt.Errorf("workspace namespace is invalid")
	case len(serviceResourceName(lake.Name, svc.Slug))+serviceNameOverhead(svc.Kind) > validation.DNS1035LabelMaxLength:
		return lakeServiceSpec{}, fmt.Errorf("lake name %q is too long for service resource names", lake.Name)
	}
	spec, err := parseServiceSpec(svc.Spec)
	if err == nil && svc.Kind == serviceKindSQLite && spec.Instances != 1 {
		return spec, fmt.Errorf("a sqlite service has exactly one instance, because SQLite has one writer; spec.instances is %d", spec.Instances)
	}
	if err == nil && svc.Kind == serviceKindDuckDB && spec.Instances != 1 {
		return spec, fmt.Errorf("a duckdb service has exactly one instance, because one DuckDB process owns its database file; spec.instances is %d", spec.Instances)
	}
	return spec, err
}

// serviceOwnership is the rule behind every write and deletion decision.
type serviceOwnership int

const (
	serviceForeign serviceOwnership = iota
	// serviceOwned: controlled by this Lake, labelled with this Lake and this service's id.
	serviceOwned
)

func serviceOwnershipOf(lake *oramav1alpha1.Lake, obj metav1.Object, svc *LakeService) serviceOwnership {
	labels := obj.GetLabels()
	if !metav1.IsControlledBy(obj, lake) || labels[serviceLakeLabel] != lake.Name {
		return serviceForeign
	}
	if id := labels[serviceIDLabel]; id != "" {
		if id == svc.ID {
			return serviceOwned
		}
		return serviceForeign
	}
	return serviceForeign
}

func foreignServiceMessage(kind, namespace, name string) string {
	return fmt.Sprintf("%s %s/%s exists and is not this Lake's service", kind, namespace, name)
}

// reconcileLakeServices is one poll of Lake-owned control rows: services, stores, agents, and
// builds. It is best-effort: a Secret that is not there yet, an unreachable database, or a failed
// Kubernetes call is logged and retried on the Lake's next requeue and never fails Lake reconcile.
// It reports whether any row is still changing, so the Lake is polled fast until it settles.
func (r *LakeReconciler) reconcileLakeServices(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	var app corev1.Secret
	secretName := lakeControlDB(lake.Name) + "-app"
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, &app); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("control rows deferred: control DB app Secret not created yet", "secret", secretName)
		} else {
			log.Error(err, "control rows deferred: cannot read the control DB app Secret", "secret", secretName)
		}
		return false
	}
	uri := string(app.Data["uri"])
	if uri == "" {
		log.Info("control rows deferred: control DB app Secret has no uri yet", "secret", secretName)
		return false
	}
	passCtx, cancel := context.WithTimeout(ctx, servicePassTimeout)
	defer cancel()
	open := r.ServiceStores
	if open == nil {
		open = openPgxServiceStore
	}
	store, err := open(passCtx, uri)
	if err != nil {
		log.Info("control rows deferred: cannot connect to the control DB", "err", err.Error())
		return false
	}
	defer func() { _ = store.Close(context.Background()) }()
	changing := false
	if lakeServicesEnabled(lake) {
		services, err := store.ListServices(passCtx)
		if err != nil {
			log.Info("services deferred: cannot read services", "err", err.Error())
		} else {
			// Stores (lake#196) ride the same connection. A schema without the stores table, or a
			// failed read, skips them for this pass and leaves the services alone.
			stores, storesErr := store.ListStores(passCtx)
			storesRead := storesErr == nil
			if errors.Is(storesErr, errStoresTableMissing) {
				log.Info("stores skipped: " + storesErr.Error())
			} else if storesErr != nil {
				log.Info("stores deferred: cannot read stores", "err", storesErr.Error())
			}
			changing = r.syncLakeServices(passCtx, lake, namespace, labels, store, services, stores, storesRead)
		}
		// Agent launches (lake_agents.go, lake#231) ride the same connection. A schema without
		// the table, or a failed read, skips them for this pass.
		launches, err := store.ListAgentLaunches(passCtx)
		if errors.Is(err, errAgentLaunchesTableMissing) {
			log.Info("agent launches skipped: " + err.Error())
		} else if err != nil {
			log.Info("agent launches deferred: cannot read agent launches", "err", err.Error())
		} else if r.reconcileLakeAgentLaunches(passCtx, lake, labels, store, launches) {
			changing = true
		}
		// Agent cron schedules (lake_agent_schedules.go, lake#244): one CronJob each.
		schedules, err := store.ListAgentSchedules(passCtx)
		if errors.Is(err, errAgentSchedulesTableMissing) {
			log.Info("agent schedules skipped: " + err.Error())
		} else if err != nil {
			log.Info("agent schedules deferred: cannot read agent schedules", "err", err.Error())
		} else if r.reconcileLakeAgentSchedules(passCtx, lake, labels, store, schedules) {
			changing = true
		}
	}
	if lakeBuildsEnabled(lake) {
		builds, err := store.ListBuilds(passCtx)
		if errors.Is(err, errBuildsTableMissing) {
			log.Info("builds skipped: " + err.Error())
		} else if err != nil {
			log.Info("builds deferred: cannot read builds", "err", err.Error())
		} else if r.reconcileLakeBuilds(passCtx, lake, store, builds) {
			changing = true
		}
	}
	// Workload resources (lake_workload_resources.go, lake#643) ride the same connection, at most
	// once per workloadResourcesInterval. A schema without the table skips them.
	if r.workloadResources.due(lake.UID, time.Now()) {
		err := r.reconcileWorkloadResources(passCtx, lake, store)
		if errors.Is(err, errWorkloadResourcesTableMissing) {
			log.Info("workload resources skipped: " + err.Error())
		} else if err != nil {
			log.Info("workload resources deferred", "err", err.Error())
		}
	}
	return changing
}

// syncLakeServices applies one read of the services and stores tables: absent services first, so
// a deletion frees its quota slot in the same pass; then present services oldest first; then the
// stores (lake_stores.go); then the credential Secret, which holds both; then the status of every
// present service and store, each Ready only once its connection is in the Secret. storesRead is
// false when the stores table could not be read: the Secret then keeps every store key it has.
func (r *LakeReconciler) syncLakeServices(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, store LakeServiceStore, services []LakeService, stores []LakeStore, storesRead bool) (changing bool) {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	absentIDs := map[string]bool{}
	workspaceZettaIDs := map[string]bool{}
	preservedSlugs := map[string]bool{}
	preservedAliases := map[string]string{}
	var present, workspaceZettas []*LakeService
	workspaceNamespaces := map[string]bool{}
	for i := range services {
		svc := &services[i]
		if svc.WorkspaceNamespace != "" {
			workspaceNamespaces[svc.WorkspaceNamespace] = true
		}
		if svc.Kind == serviceKindZetta && svc.WorkspaceZetta {
			workspaceZettaIDs[svc.ID] = true
			preservedSlugs[svc.Slug] = true
			preservedAliases[svc.ID] = svc.Slug
			workspaceZettas = append(workspaceZettas, svc)
			continue
		}
		switch svc.Desired {
		case serviceDesiredAbsent:
			absentIDs[svc.ID] = true
			// History: lake hides it and the operator skips it.
			if svc.Status != nil && svc.Status.Phase == ServicePhaseDeleted {
				continue
			}
			changing = true
			if svc.WorkspaceID == "" || len(validation.IsDNS1123Label(svc.WorkspaceNamespace)) != 0 {
				result := serviceResult{phase: ServicePhaseFailed, message: "workspace_id and its reconciled namespace are required"}
				if err := writeServiceStatus(ctx, store, svc, result); err != nil {
					log.Error(err, "service status write deferred", "service", svc.ID)
				}
				continue
			}
			result, err := r.deleteLakeService(ctx, lake, svc.WorkspaceNamespace, namespace, svc)
			if err != nil {
				log.Error(err, "service deletion deferred", "service", svc.ID, "slug", svc.Slug)
				continue
			}
			if err := writeServiceStatus(ctx, store, svc, result); err != nil {
				log.Error(err, "service status write deferred", "service", svc.ID)
			}
		case serviceDesiredPresent:
			present = append(present, svc)
		default:
			result := serviceResult{phase: ServicePhaseFailed, message: fmt.Sprintf("desired must be %s or %s", serviceDesiredPresent, serviceDesiredAbsent)}
			if err := writeServiceStatus(ctx, store, svc, result); err != nil {
				log.Error(err, "service status write deferred", "service", svc.ID)
			}
		}
	}

	live := 0
	if len(present) > 0 {
		var err error
		live, err = r.liveServiceCount(ctx, lake, workspaceNamespaces, absentIDs, workspaceZettaIDs)
		if err != nil {
			log.Error(err, "services deferred: cannot count live services")
			return false
		}
	}
	results := make(map[string]serviceResult, len(present))
	presentSlugs := preservedSlugs
	credentials := map[string][]byte{}
	for _, svc := range present {
		presentSlugs[svc.Slug] = true
		result, err := r.reconcileLakeService(ctx, lake, svc.WorkspaceNamespace, svc, &live)
		if err != nil {
			log.Error(err, "service reconcile deferred", "service", svc.ID, "slug", svc.Slug)
			continue
		}
		results[svc.ID] = result
		if result.credential != nil {
			credentials[svc.Slug] = result.credential
		}
	}

	// The workspace reconciler owns a workspace's Zetta and its status, but this poll is the one
	// writer of the services Secret, so it delivers that Zetta's connection too: read-only, from the
	// cluster and admin Secret in the workspace namespace (operator#264). The workspace reconciler
	// reports the workspace Ready only once the connection is here.
	for _, svc := range workspaceZettas {
		credential, pending, err := workspaceZettaCredential(ctx, r.Client, lake, svc.WorkspaceID, svc.WorkspaceNamespace)
		if err != nil {
			log.Error(err, "workspace Zetta connection deferred", "service", svc.ID, "workspace", svc.WorkspaceID)
			changing = true
			continue
		}
		if pending {
			changing = true
		}
		if credential != nil {
			credentials[svc.Slug] = credential
		}
	}

	storesPass := r.reconcileLakeStores(ctx, lake, namespace, labels, store, stores, storesRead)
	for slug, credential := range storesPass.credentials {
		credentials[slug] = credential
	}
	keep := func(slug string) bool {
		if storeSlugPattern.MatchString(slug) {
			return !storesPass.read || storesPass.present[slug]
		}
		return presentSlugs[slug]
	}

	aliases := preservedAliases
	for _, svc := range present {
		aliases[svc.ID] = svc.Slug
	}
	written, err := r.reconcileLakeServicesSecret(ctx, lake, namespace, labels, keep, credentials, aliases)
	if err != nil {
		log.Error(err, "service credentials deferred")
	}
	if err := r.removeLakeWorkspacesSecret(ctx, lake, namespace); err != nil {
		log.Error(err, "cannot remove the superseded workspace credential Secret")
	}
	for _, svc := range present {
		result, found := results[svc.ID]
		if !found {
			continue
		}
		if result.resourceReady {
			if written[svc.Slug] {
				result.phase, result.message = ServicePhaseReady, "service ready"
			} else {
				result.phase, result.message = ServicePhaseProvisioning, "delivering the connection"
			}
		}
		if result.phase == ServicePhaseProvisioning {
			changing = true
		}
		if err := writeServiceStatus(ctx, store, svc, result); err != nil {
			log.Error(err, "service status write deferred", "service", svc.ID)
		}
	}
	finishLakeStores(ctx, lake, store, &storesPass, written)
	return changing || storesPass.changing
}

// writeServiceStatus upserts service_status only when phase, message, endpoint or the observed
// generation differ from the row already there, so a steady service produces no database writes.
func writeServiceStatus(ctx context.Context, store LakeServiceStore, svc *LakeService, result serviceResult) error {
	next := LakeServiceStatus{
		ServiceID:          svc.ID,
		Phase:              result.phase,
		Message:            result.message,
		Endpoint:           result.endpoint,
		ObservedGeneration: svc.Generation,
	}
	if svc.Status != nil && *svc.Status == next {
		return nil
	}
	if err := store.UpsertServiceStatus(ctx, next); err != nil {
		return err
	}
	svc.Status = &next
	return nil
}

// liveServiceCount counts the resources that count against the service quota: owned by this Lake,
// labelled with a service id, not being deleted, not absent in the current read, and not the Zetta
// owned by a workspace reconciler.
func (r *LakeReconciler) liveServiceCount(ctx context.Context, lake *oramav1alpha1.Lake, namespaces map[string]bool, absentIDs, workspaceZettaIDs map[string]bool) (int, error) {
	selector := client.MatchingLabels{serviceLakeLabel: lake.Name}
	objects := []metav1.Object{}
	for namespace := range namespaces {
		var clusters oramav1alpha1.OramaZettaClusterList
		if err := r.List(ctx, &clusters, client.InNamespace(namespace), selector); err != nil {
			return 0, err
		}
		for i := range clusters.Items {
			objects = append(objects, &clusters.Items[i])
		}
		postgres := &unstructured.UnstructuredList{}
		postgres.SetGroupVersionKind(cnpgClusterListGVK)
		if err := r.List(ctx, postgres, client.InNamespace(namespace), selector); err != nil {
			return 0, err
		}
		for i := range postgres.Items {
			objects = append(objects, &postgres.Items[i])
		}
		catalog, err := r.liveCatalogObjects(ctx, lake, namespace)
		if err != nil {
			return 0, err
		}
		objects = append(objects, catalog...)
	}
	live := 0
	for _, obj := range objects {
		if !metav1.IsControlledBy(obj, lake) || obj.GetDeletionTimestamp() != nil {
			continue
		}
		labels := obj.GetLabels()
		if id := labels[serviceIDLabel]; id != "" {
			if !absentIDs[id] && !workspaceZettaIDs[id] {
				live++
			}
		}
	}
	return live, nil
}

// zettaDataClaims lists the data PVCs a Zetta cluster's StatefulSet created from its
// volumeClaimTemplate: named data-<cluster>-zetta-<ordinal> and carrying the StatefulSet's selector
// labels, which the StatefulSet controller copies onto every claim. The StatefulSet never deletes
// these claims itself.
func (r *LakeReconciler) zettaDataClaims(ctx context.Context, namespace, clusterName string) ([]corev1.PersistentVolumeClaim, error) {
	owner := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace}}
	statefulSet, _, _ := zettaNames(owner)
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(zettaDataClaimTemplate+"-"+statefulSet+"-") + "[0-9]+$")
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(namespace), client.MatchingLabels(zettaLabels(owner))); err != nil {
		return nil, err
	}
	matched := make([]corev1.PersistentVolumeClaim, 0, len(claims.Items))
	for _, claim := range claims.Items {
		if pattern.MatchString(claim.Name) {
			matched = append(matched, claim)
		}
	}
	return matched, nil
}

// setServiceMetadata writes the labels and annotation the contract puts on every service resource,
// leaving any other label (a workspace's workspace-id, for one) in place.
func setServiceMetadata(obj metav1.Object, lake *oramav1alpha1.Lake, svc *LakeService) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app.kubernetes.io/name"] = "lake-service"
	labels["app.kubernetes.io/instance"] = lake.Name
	labels["app.kubernetes.io/managed-by"] = "orama-operator"
	labels[serviceLakeLabel] = lake.Name
	labels[serviceIDLabel] = svc.ID
	labels[serviceKindLabel] = svc.Kind
	labels[serviceCustomerLabel] = serviceCustomerLabelValue(svc.CustomerID)
	labels[workspaceLabel] = svc.WorkspaceID
	obj.SetLabels(labels)
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[serviceCustomerAnnotation] = svc.CustomerID
	obj.SetAnnotations(annotations)
}

// reconcileLakeService creates or updates one present service and reports what it observed. live
// is the Lake's live service count: it is compared with the quota before a new resource is created
// and incremented after one is. An existing service is never refused by the quota.
func (r *LakeReconciler) reconcileLakeService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService, live *int) (serviceResult, error) {
	spec, err := validateService(lake, svc)
	if err != nil {
		return serviceResult{phase: ServicePhaseFailed, message: err.Error()}, nil
	}
	switch svc.Kind {
	case serviceKindZetta:
		return r.reconcileZettaService(ctx, lake, namespace, svc, spec, live)
	case serviceKindPostgres:
		return r.reconcilePostgresService(ctx, lake, namespace, svc, spec, live)
	}
	return r.reconcileCatalogService(ctx, lake, namespace, svc, spec, live, catalogKinds[svc.Kind])
}

// knownServiceKind reports whether the operator runs kind: Zetta, Postgres or a catalog kind.
func knownServiceKind(kind string) bool {
	_, catalog := catalogKinds[kind]
	return kind == serviceKindZetta || kind == serviceKindPostgres || catalog
}

func quotaRefusal(lake *oramav1alpha1.Lake, live int) (serviceResult, bool) {
	if limit := serviceQuota(lake); limit > 0 && live >= limit {
		return serviceResult{phase: ServicePhaseFailed, message: fmt.Sprintf("service quota reached (%d)", limit)}, true
	}
	return serviceResult{}, false
}

func (r *LakeReconciler) reconcileZettaService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService, spec lakeServiceSpec, live *int) (serviceResult, error) {
	name := serviceResourceName(lake.Name, svc.Slug)
	key := client.ObjectKey{Namespace: namespace, Name: name}
	endpoint := serviceZettaEndpoint(lake.Name, svc.Slug, namespace)
	cluster := &oramav1alpha1.OramaZettaCluster{}
	creating := false
	if err := r.Get(ctx, key, cluster); err == nil {
		if serviceOwnershipOf(lake, cluster, svc) != serviceOwned {
			return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("OramaZettaCluster", namespace, name)}, nil
		}
		if cluster.DeletionTimestamp != nil {
			return serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: "waiting for the previous Zetta cluster to be deleted"}, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, err
	} else {
		// A new cluster must not take over an old data volume: its StatefulSet would bind a claim
		// of the same name. Wait for a terminating one; refuse a live one nothing proves is ours.
		claims, err := r.zettaDataClaims(ctx, namespace, name)
		if err != nil {
			return serviceResult{}, err
		}
		for _, claim := range claims {
			if claim.DeletionTimestamp != nil {
				return serviceResult{phase: ServicePhaseProvisioning, message: "waiting for the previous Zetta data volume to be deleted"}, nil
			}
			return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("PersistentVolumeClaim", namespace, claim.Name)}, nil
		}
		if refusal, refused := quotaRefusal(lake, *live); refused {
			return refusal, nil
		}
		cluster = &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		creating = true
	}

	operatorSchema := false
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		setServiceMetadata(cluster, lake, svc)
		// Only the fields the service contract owns are written, so server-side defaults on the
		// rest of the spec do not turn every poll into an update. The data volume is sized once
		// because a StatefulSet's volumeClaimTemplates are immutable.
		if creating {
			cluster.Spec.Storage = oramav1alpha1.OramaZettaStorage{
				Size:             fmt.Sprintf("%dGi", spec.StorageGi),
				StorageClassName: lake.Spec.Workspaces.Storage.StorageClassName,
			}
		}
		cluster.Spec.Replicas = spec.Instances
		cluster.Spec.Image = serviceZettaImage(lake)
		cluster.Spec.PullSecretName = lake.Spec.PullSecretName
		cluster.Spec.Resources = serviceZettaResources(lake)
		cluster.Spec.Env = append([]corev1.EnvVar(nil), lake.Spec.Services.ZettaEnv...)
		cluster.Spec.IngressEnabled = false
		cluster.Spec.ProvisionOperatorSchema = &operatorSchema
		return controllerutil.SetControllerReference(lake, cluster, r.Scheme)
	}); err != nil {
		return serviceResult{}, err
	}
	if creating {
		*live++
	}

	result := serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: "waiting for the Zetta nodes"}
	if cluster.Status.Phase != "Ready" || cluster.Status.ReadyNodes < spec.Instances {
		return result, nil
	}
	result.resourceReady = true
	var admin corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-zetta-admin"}, &admin); err != nil {
		if apierrors.IsNotFound(err) {
			return result, nil
		}
		return serviceResult{}, err
	}
	token := string(admin.Data["token"])
	if token == "" {
		return result, nil
	}
	credential, err := zettaCredential(endpoint, token)
	if err != nil {
		return serviceResult{}, err
	}
	result.credential = credential
	return result, nil
}

// zettaCredential is the <slug>.json connection lake reads for a Zetta: its in-cluster endpoint
// and admin bearer token.
func zettaCredential(endpoint, token string) ([]byte, error) {
	return json.Marshal(struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}{serviceKindZetta, endpoint, token})
}

func newCNPGCluster(namespace, name string) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetNamespace(namespace)
	cluster.SetName(name)
	return cluster
}

func (r *LakeReconciler) reconcilePostgresService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService, spec lakeServiceSpec, live *int) (serviceResult, error) {
	name := serviceResourceName(lake.Name, svc.Slug)
	endpoint := servicePostgresEndpoint(lake.Name, svc.Slug, namespace)
	cluster := newCNPGCluster(namespace, name)
	creating := false
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err == nil {
		if serviceOwnershipOf(lake, cluster, svc) != serviceOwned {
			return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("CloudNativePG Cluster", namespace, name)}, nil
		}
		if cluster.GetDeletionTimestamp() != nil {
			return serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: "waiting for the previous Postgres cluster to be deleted"}, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, err
	} else {
		if refusal, refused := quotaRefusal(lake, *live); refused {
			return refusal, nil
		}
		cluster = newCNPGCluster(namespace, name)
		creating = true
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		setServiceMetadata(cluster, lake, svc)
		// Individual fields rather than a whole spec, so CloudNativePG's defaults survive and a
		// steady cluster is never rewritten. Storage and bootstrap are set once, at creation.
		if err := unstructured.SetNestedField(cluster.Object, int64(spec.Instances), "spec", "instances"); err != nil {
			return err
		}
		if creating {
			storage := map[string]interface{}{"size": fmt.Sprintf("%dGi", spec.StorageGi)}
			if class := lake.Spec.Postgres.StorageClassName; class != "" {
				storage["storageClass"] = class
			}
			if err := unstructured.SetNestedMap(cluster.Object, storage, "spec", "storage"); err != nil {
				return err
			}
			initdb := map[string]interface{}{"database": servicePostgresDatabase, "owner": servicePostgresOwner}
			if err := unstructured.SetNestedMap(cluster.Object, initdb, "spec", "bootstrap", "initdb"); err != nil {
				return err
			}
		}
		return controllerutil.SetControllerReference(lake, cluster, r.Scheme)
	}); err != nil {
		return serviceResult{}, err
	}
	if creating {
		*live++
	}

	result := serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: "waiting for the Postgres instances"}
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	if ready != int64(spec.Instances) {
		return result, nil
	}
	result.resourceReady = true
	var app corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-app"}, &app); err != nil {
		if apierrors.IsNotFound(err) {
			return result, nil
		}
		return serviceResult{}, err
	}
	password, uri := string(app.Data["password"]), string(app.Data["uri"])
	if password == "" || uri == "" {
		return result, nil
	}
	database, user := string(app.Data["dbname"]), string(app.Data["username"])
	if database == "" {
		database = servicePostgresDatabase
	}
	if user == "" {
		user = servicePostgresOwner
	}
	credential, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Database string `json:"database"`
		User     string `json:"user"`
		Password string `json:"password"`
		URI      string `json:"uri"`
	}{serviceKindPostgres, servicePostgresHost(lake.Name, svc.Slug, namespace), servicePostgresPort, database, user, password, uri})
	if err != nil {
		return serviceResult{}, err
	}
	result.credential = credential
	return result, nil
}

// deleteLakeService deletes an absent service: for Zetta its data PVCs, then its cluster; for
// Postgres its CloudNativePG Cluster, whose PVCs it owns; then its credential key. It reports
// Deleted only once none of them remains; until then every poll repeats the same steps. Only a
// resource that passes the ownership rule is ever deleted.
func (r *LakeReconciler) deleteLakeService(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	credentialNamespace string,
	svc *LakeService,
) (serviceResult, error) {
	if !serviceSlugPattern.MatchString(svc.Slug) {
		return serviceResult{phase: ServicePhaseFailed, message: fmt.Sprintf("slug must match %s", serviceSlugPattern.String())}, nil
	}
	previousEndpoint := ""
	if svc.Status != nil {
		previousEndpoint = svc.Status.Endpoint
	}
	var remaining bool
	var result serviceResult
	var err error
	switch svc.Kind {
	case serviceKindZetta:
		result, remaining, err = r.deleteZettaService(ctx, lake, namespace, svc, previousEndpoint)
	case serviceKindPostgres:
		result, remaining, err = r.deletePostgresService(ctx, lake, namespace, svc)
	default:
		kind, found := catalogKinds[svc.Kind]
		if !found {
			return serviceResult{phase: ServicePhaseFailed, message: unknownServiceKindMessage(svc.Kind)}, nil
		}
		result, remaining, err = r.deleteCatalogService(ctx, lake, namespace, svc, previousEndpoint, kind)
	}
	if err != nil || result.phase == ServicePhaseFailed {
		return result, err
	}
	if err := r.removeServiceCredential(ctx, lake, credentialNamespace, svc.Slug); err != nil {
		return serviceResult{}, err
	}
	if remaining {
		endpoint := previousEndpoint
		if result.endpoint != "" {
			endpoint = result.endpoint
		}
		return serviceResult{phase: ServicePhaseDeleting, endpoint: endpoint, message: result.message}, nil
	}
	return serviceResult{phase: ServicePhaseDeleted, message: "service deleted"}, nil
}

func (r *LakeReconciler) deleteZettaService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService, previousEndpoint string) (serviceResult, bool, error) {
	name := serviceResourceName(lake.Name, svc.Slug)
	key := client.ObjectKey{Namespace: namespace, Name: name}
	cluster := &oramav1alpha1.OramaZettaCluster{}
	clusterFound := true
	if err := r.Get(ctx, key, cluster); apierrors.IsNotFound(err) {
		clusterFound = false
	} else if err != nil {
		return serviceResult{}, false, err
	}
	if clusterFound && serviceOwnershipOf(lake, cluster, svc) == serviceForeign {
		return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("OramaZettaCluster", namespace, name)}, false, nil
	}

	claims, err := r.zettaDataClaims(ctx, namespace, name)
	if err != nil {
		return serviceResult{}, false, err
	}
	// Without the cluster, a live claim is deleted only when service_status proves this operator
	// materialized the service: an endpoint is written only for a resource it owns.
	claimsProven := clusterFound || previousEndpoint != ""
	for i := range claims {
		claim := &claims[i]
		if claim.DeletionTimestamp != nil {
			continue
		}
		if !claimsProven {
			return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("PersistentVolumeClaim", namespace, claim.Name)}, false, nil
		}
		if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
			return serviceResult{}, false, fmt.Errorf("delete service data volume %s/%s: %w", namespace, claim.Name, err)
		}
	}
	// Data volumes before the cluster, so an interrupted pass never leaves live volumes behind a
	// cluster that is already gone.
	if clusterFound && cluster.DeletionTimestamp == nil {
		if err := r.Delete(ctx, cluster, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return serviceResult{}, false, fmt.Errorf("delete service cluster %s: %w", key, err)
		}
	}

	if err := r.Get(ctx, key, &oramav1alpha1.OramaZettaCluster{}); err == nil {
		return serviceResult{message: "deleting the Zetta cluster"}, true, nil
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, false, err
	}
	remaining, err := r.zettaDataClaims(ctx, namespace, name)
	if err != nil {
		return serviceResult{}, false, err
	}
	if len(remaining) > 0 {
		return serviceResult{message: "deleting the Zetta data volumes"}, true, nil
	}
	return serviceResult{}, false, nil
}

func (r *LakeReconciler) deletePostgresService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService) (serviceResult, bool, error) {
	name := serviceResourceName(lake.Name, svc.Slug)
	cluster := newCNPGCluster(namespace, name)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err == nil {
		if serviceOwnershipOf(lake, cluster, svc) != serviceOwned {
			return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("CloudNativePG Cluster", namespace, name)}, false, nil
		}
		if cluster.GetDeletionTimestamp() == nil {
			if err := r.Delete(ctx, cluster, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return serviceResult{}, false, fmt.Errorf("delete service cluster %s/%s: %w", namespace, name, err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, false, err
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), newCNPGCluster(namespace, name)); err == nil {
		return serviceResult{message: "deleting the Postgres cluster"}, true, nil
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, false, err
	}
	// CloudNativePG owns its PVCs, so the garbage collector removes them after the Cluster. The
	// service is Deleted only once they are gone too.
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(namespace), servicePostgresSelector(name)); err != nil {
		return serviceResult{}, false, err
	}
	if len(claims.Items) > 0 {
		return serviceResult{message: "deleting the Postgres data volumes"}, true, nil
	}
	return serviceResult{}, false, nil
}

// removeServiceCredential drops a service's key from lake-<lake>-services.
func (r *LakeReconciler) removeServiceCredential(ctx context.Context, lake *oramav1alpha1.Lake, namespace, slug string) error {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeServicesSecret(lake.Name)}, &secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&secret, lake) {
		return nil
	}
	if _, found := secret.Data[serviceCredentialKey(slug)]; !found {
		return nil
	}
	delete(secret.Data, serviceCredentialKey(slug))
	return r.Update(ctx, &secret)
}

// reconcileLakeServicesSecret maintains lake-<lake>-services: one <slug>.json per present service
// or store whose connection is known (service slugs are svc- and ws-, store slugs st-, so they
// never collide). A present one keeps its key while its connection cannot be read (a transient
// read never removes a file lake depends on); a key keep refuses (absent, or no longer listed) is
// dropped. It returns the slugs whose fresh credential is now in the Secret.
func (r *LakeReconciler) reconcileLakeServicesSecret(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, keep func(slug string) bool, credentials map[string][]byte, aliases map[string]string) (map[string]bool, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeServicesSecret(lake.Name), Namespace: namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); err == nil && !metav1.IsControlledBy(secret, lake) {
		return nil, fmt.Errorf("secret %s/%s exists and is not controlled by Lake %s", namespace, secret.Name, lake.Name)
	} else if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labels
		secret.Type = corev1.SecretTypeOpaque
		data := map[string][]byte{}
		for key, value := range secret.Data {
			if slug, ok := strings.CutSuffix(key, ".json"); ok && keep(slug) {
				data[key] = value
			}
		}
		for slug, credential := range credentials {
			data[serviceCredentialKey(slug)] = credential
		}
		for id, slug := range aliases {
			if credential := data[serviceCredentialKey(slug)]; len(credential) > 0 {
				data[id] = credential
			}
		}
		secret.Data = data
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	if op != controllerutil.OperationResultNone {
		r.refreshLakeServiceMounts(ctx, namespace, labels, secret.ResourceVersion)
	}
	written := make(map[string]bool, len(credentials))
	for slug := range credentials {
		written[slug] = true
	}
	return written, nil
}

// lakeServicesSecretVersionAnnotation carries the services Secret's resourceVersion on each lake
// pod. Kubelet refreshes a mounted Secret only when it syncs the pod, about once a minute, so a
// new credential reached lake up to a minute after the operator wrote it (lake#197). Changing
// the pod's annotations makes kubelet sync the pod, and so the volume, now. It restarts nothing.
const lakeServicesSecretVersionAnnotation = "orama.cloud/services-secret-version"

// refreshLakeServiceMounts patches that annotation onto every lake pod. Best-effort: a failure
// only means the credential arrives on kubelet's own schedule.
func (r *LakeReconciler) refreshLakeServiceMounts(ctx context.Context, namespace string, labels map[string]string, version string) {
	if len(labels) == 0 || version == "" {
		return
	}
	log := logf.FromContext(ctx)
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		log.Info("cannot list lake pods to refresh the services mount", "err", err.Error())
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Annotations[lakeServicesSecretVersionAnnotation] == version {
			continue
		}
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[lakeServicesSecretVersionAnnotation] = version
		if err := r.Patch(ctx, pod, patch); err != nil {
			log.Info("cannot refresh the services mount", "pod", pod.Name, "err", err.Error())
		}
	}
}

// removeLakeWorkspacesSecret deletes the decision-45 lake-<lake>-workspaces Secret, which nothing
// mounts any more; its tokens now live in lake-<lake>-services.
func (r *LakeReconciler) removeLakeWorkspacesSecret(ctx context.Context, lake *oramav1alpha1.Lake, namespace string) error {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeWorkspacesSecret(lake.Name)}, &secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&secret, lake) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &secret))
}

// lakeServicesChangingPoll is the poll while any service is starting or being deleted.
const lakeServicesChangingPoll = 2 * time.Second

// lakeServicesPollInterval is the services poll: the Lake's deploySync interval (default 15s,
// floor 1s), the cadence the decision-45 workspace poll ran at.
func lakeServicesPollInterval(lake *oramav1alpha1.Lake) time.Duration {
	interval := lake.Spec.DeploySync.Interval.Duration
	if interval <= 0 {
		interval = defaultDeploymentSyncInterval
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}
