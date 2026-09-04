/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
	lakeDBRequeue     = 20 * time.Second
)

// lakeSchemaSQL is lake's control-plane schema. SOURCE OF TRUTH: oramasearch/lake db/schema.sql;
// this is a byte-identical vendored copy, applied per Lake through an owned PostgresSchema.
//
//go:embed assets/lake-schema.sql
var lakeSchemaSQL string

// lakeControlDB is the CNPG Cluster name for a Lake; CNPG derives `<name>-app` (the owner
// credentials + URI Secret) and `<name>-rw` (the primary Service) from it.
func lakeControlDB(name string) string { return "lake-" + name + "-controldb" }

// lakeAdminSecret holds the Lake's admin bearer token (key `token`), minted once by the operator and
// injected as LAKE_ADMIN_TOKEN — the same pattern as OramaZettaCluster's `<name>-zetta-admin`.
func lakeAdminSecret(name string) string { return "lake-" + name + "-admin" }

func lakePostgresEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Postgres.Enabled == nil || *lake.Spec.Postgres.Enabled
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=postgresschemas,verbs=get;list;watch;create;update;patch;delete

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

	// Admin token: minted once, before the Deployment so its secretKeyRef resolves.
	if err := r.reconcileLakeAdminToken(ctx, &lake, ns, labels); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile admin token: %w", err)
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
			Env:   lakeEnv(&lake),
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromString("http")}}},
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

	// Control-plane Postgres (CNPG), on by default, and its schema (PostgresSchema).
	if err := r.reconcileLakeControlDB(ctx, &lake, ns, labels); err != nil {
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
	pgReady, pgDetail := r.lakeControlDBStatus(ctx, &lake, ns)
	schemaPhase, schemaVersion := r.lakeSchemaStatus(ctx, &lake, ns)
	if lake.Status.ReadyReplicas != deploy.Status.ReadyReplicas || lake.Status.Phase != phase ||
		lake.Status.PostgresReady != pgReady || lake.Status.PostgresInstances != pgDetail ||
		lake.Status.SchemaPhase != schemaPhase || lake.Status.SchemaVersion != schemaVersion {
		lake.Status.ReadyReplicas = deploy.Status.ReadyReplicas
		lake.Status.Phase = phase
		lake.Status.PostgresReady = pgReady
		lake.Status.PostgresInstances = pgDetail
		lake.Status.SchemaPhase = schemaPhase
		lake.Status.SchemaVersion = schemaVersion
		if err := r.Status().Update(ctx, &lake); err != nil {
			return ctrl.Result{}, err
		}
	}
	// The CNPG Cluster is not watched (OramaCloudV1 does the same): poll until the DB is ready and
	// the schema applied (the PostgresSchema is owned, so its status changes re-queue us too).
	if lakePostgresEnabled(&lake) && (!pgReady || schemaPhase != "Applied") {
		return ctrl.Result{RequeueAfter: lakeDBRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// lakeEnv is what the lake container learns from the operator. The control-DB Secret is
// generated by CNPG after the Cluster bootstraps, so the reference is optional: the pod starts
// before the database exists and picks the value up on its next restart.
func lakeEnv(lake *oramav1alpha1.Lake) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "LAKE_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: lakeAdminSecret(lake.Name)}, Key: "token"}}}}
	if !lakePostgresEnabled(lake) {
		return env
	}
	optional := true
	secret := lakeControlDB(lake.Name) + "-app"
	ref := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key, Optional: &optional}}
	}
	return append(env,
		corev1.EnvVar{Name: "LAKE_CONTROL_DB_URI", ValueFrom: ref("uri")},
		corev1.EnvVar{Name: "LAKE_CONTROL_DB_PASSWORD", ValueFrom: ref("password")},
	)
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
func (r *LakeReconciler) reconcileLakeControlDB(ctx context.Context, lake *oramav1alpha1.Lake, ns string, lbls map[string]string) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(lakeControlDB(lake.Name))
	cluster.SetNamespace(ns)
	if !lakePostgresEnabled(lake) {
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
		cluster.Object["spec"] = map[string]interface{}{
			"instances": instances,
			"storage":   storage,
			"bootstrap": map[string]interface{}{
				"initdb": map[string]interface{}{"database": lakeControlDBName, "owner": lakeControlDBName},
			},
		}
		return controllerutil.SetControllerReference(lake, cluster, r.Scheme)
	})
	return err
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
			GuardTable: "offline_recovery_guard",
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
		return deleteIfExists(ctx, r.Client, dnsRecord())
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
		return deleteIfExists(ctx, r.Client, dnsRecord())
	}
	target := ""
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.Hostname != "" {
			target = lb.Hostname
			break
		}
		if lb.IP != "" {
			target = lb.IP
			break
		}
	}
	if target == "" {
		return nil // address still pending; the Ingress status update re-queues us
	}
	rec := dnsRecord()
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
		rec.Labels = lbls
		rec.Spec.SecretRef, rec.Spec.Name, rec.Spec.Content, rec.Spec.Proxied = lake.Spec.DNSSecretRef.Name, lake.Spec.Host, target, true
		return controllerutil.SetControllerReference(lake, rec, r.Scheme)
	})
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *LakeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Lake{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&oramav1alpha1.PostgresSchema{}).
		Named("lake").
		Complete(r)
}
