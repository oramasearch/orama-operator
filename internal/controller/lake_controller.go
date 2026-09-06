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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// LakeReconciler reconciles a Lake object into a Namespace, a Deployment, a Service, a
// control-plane Postgres (CloudNativePG Cluster, on by default) and, opt-in, an Ingress plus a
// Cloudflare DNS record. Zetta is lake's data plane and is provisioned separately. Hello-world stage: the Deployment runs whatever spec.image says, which
// defaults to an HTTP echo until the lake binary exists.
type LakeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	lakeDefaultPort   = 4900 // lake's --listen default
	lakeServicePort   = 80
	lakeControlDBName = "lake" // CNPG bootstrap database + owner
	lakeStatusRequeue = 20 * time.Second
	lakeStorePath     = "/var/lib/lake/store"
	lakeStoreVolume   = "store"
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
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=postgresschemas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=scheduledbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=backups,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings the cluster to the state described by the Lake.
func (r *LakeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, req.NamespacedName, &lake); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ns := lake.Spec.Namespace
	if ns == "" {
		ns = "lake-dev"
	}
	image := lake.Spec.Image
	if image == "" {
		return ctrl.Result{}, fmt.Errorf("lake %q: spec.image is required", lake.Name)
	}
	replicas := int32(1)
	if lake.Spec.Replicas != nil {
		replicas = *lake.Spec.Replicas
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
	name := "lake-" + lake.Name
	claimName, err := r.reconcileLakeStore(ctx, &lake, ns, labels)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile store: %w", err)
	}

	// Admin token: minted once, before the Deployment so its secretKeyRef resolves.
	if err := r.reconcileLakeAdminToken(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile admin token: %w", err)
	}
	if err := r.reconcileLakeInstanceKek(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile instance KEK: %w", err)
	}
	sealing, err := r.reconcileLakeSealingKey(ctx, &lake, ns, labels)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile sealing key: %w", err)
	}

	// Deployment.
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = &replicas
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deploy.Spec.Template.ObjectMeta.Labels = labels
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "lake",
			Image: image,
			Args:  lake.Spec.Args,
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			Env:   lakeEnv(&lake, sealing),
			VolumeMounts: []corev1.VolumeMount{{
				Name:      lakeStoreVolume,
				MountPath: lakeStorePath,
			}},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromString("http")}}},
		}}
		// lake runs as uid/gid 10001 in its image; fsGroup makes the store volume writable by that
		// group when it is mounted (docs/lake/README.md, the store section).
		deploy.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
			FSGroup:             ptrInt64(lakeRunAsGroup),
			FSGroupChangePolicy: ptrFSGroupChangePolicy(corev1.FSGroupChangeOnRootMismatch),
		}
		deploy.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name: lakeStoreVolume,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: claimName,
			}},
		}}
		deploy.Spec.Template.Spec.ImagePullSecrets = nil
		if lake.Spec.PullSecretName != "" {
			deploy.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: lake.Spec.PullSecretName}}
		}
		return controllerutil.SetControllerReference(&lake, deploy, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile deployment: %w", err)
	}

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
	if err := r.reconcileLakeControlDBSchema(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile control db schema: %w", err)
	}

	// Ingress + DNS (opt-in, same contract as OramaZettaCluster).
	if err := r.reconcileLakeIngress(ctx, &lake, ns, name, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ingress: %w", err)
	}

	// Status: mirror the Deployment, and only write on a real transition (anti-flap,
	// operator principle #7). Vocabulary: Progressing | Ready | Degraded.
	phase := "Progressing"
	switch {
	case replicas > 0 && deploy.Status.ReadyReplicas >= replicas:
		phase = "Ready"
	case deploy.Status.UnavailableReplicas > 0 && deploy.Status.ReadyReplicas > 0:
		phase = "Degraded"
	}
	storeStatus, storeCondition, storeReady, err := r.lakeStoreStatus(ctx, &lake, ns, claimName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read store status: %w", err)
	}
	if !storeReady && phase == "Ready" {
		phase = "Progressing"
	}
	pgReady, pgDetail := r.lakeControlDBStatus(ctx, &lake, ns)
	schemaPhase, schemaVersion := r.lakeSchemaStatus(ctx, &lake, ns)
	backupReady, lastBackupTime, backupCondition, err := r.lakeBackupStatus(ctx, &lake, ns, backup)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read control DB backup status: %w", err)
	}
	if backup.Enabled && !backup.Valid {
		phase = "Degraded"
	}
	dataServices, dataServicesSummary := lakeDataServiceStatus(&lake)
	desiredConditions := lake.Status.Conditions
	conditionsChanged := apimeta.SetStatusCondition(&desiredConditions, backupCondition)
	if apimeta.SetStatusCondition(&desiredConditions, storeCondition) {
		conditionsChanged = true
	}
	if lake.Status.ReadyReplicas != deploy.Status.ReadyReplicas || lake.Status.Phase != phase ||
		lake.Status.PostgresReady != pgReady || lake.Status.PostgresInstances != pgDetail ||
		lake.Status.SchemaPhase != schemaPhase || lake.Status.SchemaVersion != schemaVersion ||
		lake.Status.BackupReady != backupReady || !lakeBackupTimesEqual(lake.Status.LastBackupTime, lastBackupTime) ||
		!apiequality.Semantic.DeepEqual(lake.Status.DataServices, dataServices) ||
		lake.Status.DataServicesSummary != dataServicesSummary ||
		!apiequality.Semantic.DeepEqual(lake.Status.Store, storeStatus) ||
		!apiequality.Semantic.DeepEqual(lake.Status.Sealing, sealing.Status) ||
		conditionsChanged {
		lake.Status.ReadyReplicas = deploy.Status.ReadyReplicas
		lake.Status.Phase = phase
		lake.Status.PostgresReady = pgReady
		lake.Status.PostgresInstances = pgDetail
		lake.Status.SchemaPhase = schemaPhase
		lake.Status.SchemaVersion = schemaVersion
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
	// The CNPG Cluster is not watched (OramaCloudV1 does the same): poll until the DB is ready and
	// the schema applied (the PostgresSchema is owned, so its status changes re-queue us too).
	if lakePostgresEnabled(&lake) && (!pgReady || schemaPhase != "Applied") {
		return ctrl.Result{RequeueAfter: lakeStatusRequeue}, nil
	}
	// CNPG creates Backup objects outside Lake ownership. Polling captures future runs while the
	// anti-flap status guard avoids writes when the observed state is unchanged.
	if lakePostgresEnabled(&lake) && backup.Enabled {
		return ctrl.Result{RequeueAfter: lakeStatusRequeue}, nil
	}
	if !storeReady {
		return ctrl.Result{RequeueAfter: lakeStatusRequeue}, nil
	}
	return ctrl.Result{}, nil
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
		Notes:    "the Zetta data plane and its lifecycle procedures are pending #125",
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
) (string, error) {
	claimName := lakeStoreClaimName(lake)
	if lake.Spec.Store.ExistingClaim != "" {
		return claimName, nil
	}

	pvc := &corev1.PersistentVolumeClaim{}
	key := client.ObjectKey{Namespace: ns, Name: claimName}
	if err := r.Get(ctx, key, pvc); err == nil {
		if !metav1.IsControlledBy(pvc, lake) {
			return "", fmt.Errorf(
				"persistent volume claim %s/%s already exists and is not controlled by Lake %s; set spec.store.existingClaim to use it",
				ns,
				claimName,
				lake.Name,
			)
		}
		return claimName, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
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
		return "", err
	}
	if err := r.Create(ctx, pvc); err != nil {
		return "", err
	}
	return claimName, nil
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
// before the database exists and picks the value up on its next restart.
func lakeEnv(lake *oramav1alpha1.Lake, sealing lakeSealingMaterial) []corev1.EnvVar {
	reserved := map[string]struct{}{
		"LAKE_ADMIN_TOKEN":         {},
		"LAKE_CONTROL_DB_PASSWORD": {},
		"LAKE_CONTROL_DB_URI":      {},
		"LAKE_INSTANCE_KEK":        {},
		"LAKE_SEALING_KID":         {},
		"LAKE_SEALING_PUBLIC_KEY":  {},
		"LAKE_STORE_ROOT":          {},
	}
	env := []corev1.EnvVar{
		{Name: "LAKE_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: lakeAdminSecret(lake.Name)},
			Key:                  "token",
		}}},
		{Name: "LAKE_STORE_ROOT", Value: lakeStorePath},
		{Name: "LAKE_SEALING_PUBLIC_KEY", Value: base64.RawURLEncoding.EncodeToString(sealing.PublicKey)},
		{Name: "LAKE_SEALING_KID", Value: sealing.KID},
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
	for _, custom := range lake.Spec.Env {
		if _, found := reserved[custom.Name]; found {
			continue
		}
		env = append(env, custom)
	}
	return env
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
			"storage":   storage,
			"bootstrap": map[string]interface{}{
				"initdb": map[string]interface{}{"database": lakeControlDBName, "owner": lakeControlDBName},
			},
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

// reconcileLakeControlDBSchema applies lake's control-plane schema the way OramaCloudV1 applies
// the dashboard's (controldb.go): the SQL rendered into an owned ConfigMap and declared as an owned
// PostgresSchema, whose controller runs the idempotent migration Job (wait for the DB, skip if the
// guard table exists, apply in one transaction) and records the applied version. The version is a
// content hash of the SQL, so it changes exactly when the vendored schema does. With Postgres
// disabled both objects are torn down.
func (r *LakeReconciler) reconcileLakeControlDBSchema(ctx context.Context, lake *oramav1alpha1.Lake, ns string, lbls map[string]string) error {
	cmName := lakeControlDB(lake.Name) + "-migrations"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns}}
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: lakeControlDB(lake.Name) + "-schema", Namespace: ns}}
	if !lakePostgresEnabled(lake) {
		if err := deleteIfExists(ctx, r.Client, ps); err != nil {
			return err
		}
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
		ps.Labels = lbls
		ps.Spec = oramav1alpha1.PostgresSchemaSpec{
			ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: lakeControlDB(lake.Name) + "-app", Key: "uri"},
			SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: cmName, Key: "schema.sql"},
			Version:             schemaVersion(lakeSchemaSQL),
			// The guard is the table the NEWEST migration creates: with --single-transaction it
			// exists iff the whole (idempotent) file applied, and a new migration = a new guard, so
			// the next Job re-runs the file and adds only what is missing.
			GuardTable: "identity_exit_guard",
			Image:      "postgres:18-alpine",
		}
		return controllerutil.SetControllerReference(lake, ps, r.Scheme)
	})
	return err
}

// lakeSchemaStatus mirrors the owned PostgresSchema's phase and applied version, best-effort.
func (r *LakeReconciler) lakeSchemaStatus(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (string, string) {
	if !lakePostgresEnabled(lake) {
		return "", ""
	}
	var ps oramav1alpha1.PostgresSchema
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lakeControlDB(lake.Name) + "-schema"}, &ps); err != nil {
		return "Pending", ""
	}
	if ps.Status.Phase == "" {
		return "Pending", ps.Status.AppliedVersion
	}
	return ps.Status.Phase, ps.Status.AppliedVersion
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
	return ri >= 1, fmt.Sprintf("%d/%d", ri, inst)
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
	if !lake.Spec.IngressEnabled || lake.Spec.Host == "" {
		if err := deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: ns}}); err != nil {
			return err
		}
		if err := deleteIfExists(ctx, r.Client, dnsRecord()); err != nil {
			return err
		}
		return r.deleteLakePreviewDNSRecords(ctx, lake, ns, "")
	}
	prefix := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = lbls
		ing.Spec.IngressClassName = strPtr("nginx")
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: lake.Spec.Host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/", PathType: &prefix,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: svcName, Port: networkingv1.ServiceBackendPort{Number: lakeServicePort}}},
				}},
			}},
		}}
		return controllerutil.SetControllerReference(lake, ing, r.Scheme)
	}); err != nil {
		return err
	}
	if lake.Spec.DNSSecretRef == nil || lake.Spec.DNSSecretRef.Name == "" {
		if err := deleteIfExists(ctx, r.Client, dnsRecord()); err != nil {
			return err
		}
		return r.deleteLakePreviewDNSRecords(ctx, lake, ns, "")
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
	if lake.Spec.PreviewWildcard == "" {
		return nil
	}
	preview := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: previewDNSName, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, preview, func() error {
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Lake{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(
			&corev1.PersistentVolumeClaim{},
			handler.EnqueueRequestsFromMapFunc(r.lakeRequestsForStoreClaim),
		).
		Owns(&networkingv1.Ingress{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&oramav1alpha1.LakeSealedSecret{}).
		Owns(&oramav1alpha1.PostgresSchema{}).
		Owns(newCNPGScheduledBackup(nil, "")).
		Named("lake").
		Complete(r)
}

// lakeRunAsGroup is the gid of the lake user in the oramasearch/lake image.
const lakeRunAsGroup int64 = 10001

func ptrInt64(v int64) *int64 { return &v }

func ptrFSGroupChangePolicy(v corev1.PodFSGroupChangePolicy) *corev1.PodFSGroupChangePolicy {
	return &v
}
