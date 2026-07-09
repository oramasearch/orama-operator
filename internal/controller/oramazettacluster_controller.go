/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// Zetta headless ports (source-verified — see docs/projects/one-orama/zetta-headless-reference.md).
const (
	zettaPeerPort = 7777 // peer replication + signal plane + blob transfer (mTLS unless plaintext)
	zettaAPIPort  = 4800 // MCP + REST + model/index serving (one HTTP server)
	zettaDataDir  = "/data"
)

// OramaZettaClusterReconciler deploys a headless Orama Zetta cluster (StatefulSet of equal-peer CRDT
// nodes) from an OramaZettaCluster CR. Step 1 of One Orama (orama-bridge#130): it only RUNS Zetta
// headless — no operator↔Zetta symbiosis and no changes to Zetta's MCP service.
type OramaZettaClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

func zettaLabels(z *oramav1alpha1.OramaZettaCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "orama-zetta",
		"app.kubernetes.io/instance":   z.Name,
		"app.kubernetes.io/component":  "zetta-node",
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
	}
}

func zettaNames(z *oramav1alpha1.OramaZettaCluster) (sts, headlessSvc, clientSvc string) {
	return z.Name + "-zetta", z.Name + "-zetta-headless", z.Name + "-zetta"
}

func (r *OramaZettaClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var z oramav1alpha1.OramaZettaCluster
	if err := r.Get(ctx, req.NamespacedName, &z); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	stsName, headlessName, clientName := zettaNames(&z)
	lbls := zettaLabels(&z)

	// 1. Headless Service — stable per-pod DNS for peer discovery (<sts>-0.<headless>.<ns>.svc:7777).
	headless := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, headless, func() error {
		headless.Labels = lbls
		headless.Spec.ClusterIP = corev1.ClusterIPNone
		headless.Spec.PublishNotReadyAddresses = true // peers must resolve each other before they're Ready
		headless.Spec.Selector = lbls
		headless.Spec.Ports = []corev1.ServicePort{
			{Name: "peer", Port: zettaPeerPort, TargetPort: intstr.FromInt32(zettaPeerPort)},
			{Name: "api", Port: zettaAPIPort, TargetPort: intstr.FromInt32(zettaAPIPort)},
		}
		return controllerutil.SetControllerReference(&z, headless, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 2. Client Service — the HTTP API (MCP + REST + serving) on 4800.
	clientSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: clientName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, clientSvc, func() error {
		clientSvc.Labels = lbls
		clientSvc.Spec.Selector = lbls
		clientSvc.Spec.Ports = []corev1.ServicePort{
			{Name: "api", Port: zettaAPIPort, TargetPort: intstr.FromInt32(zettaAPIPort)},
		}
		return controllerutil.SetControllerReference(&z, clientSvc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 3. StatefulSet — N equal-peer headless nodes, each with a per-node PVC at /data.
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		return r.mutateZettaStatefulSet(&z, sts, headlessName, stsName, lbls)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Status — readiness roll-up (write only on change; anti-flap).
	return ctrl.Result{}, r.reconcileZettaStatus(ctx, &z, stsName)
}

func (r *OramaZettaClusterReconciler) mutateZettaStatefulSet(z *oramav1alpha1.OramaZettaCluster, sts *appsv1.StatefulSet, headlessName, stsName string, lbls map[string]string) error {
	replicas := z.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}
	image := z.Spec.Image
	if image == "" {
		image = "oramasearch/zetta:headless-v1-sandbox"
	}
	storageSize := z.Spec.Storage.Size
	if storageSize == "" {
		storageSize = "10Gi"
	}

	var pull []corev1.LocalObjectReference
	if z.Spec.PullSecretName != "" {
		pull = []corev1.LocalObjectReference{{Name: z.Spec.PullSecretName}}
	}
	var resources corev1.ResourceRequirements
	if z.Spec.Resources != nil {
		resources = *z.Spec.Resources
	}

	// The seed is node-0's stable DNS. node-0 bootstraps (mints its own mTLS cluster token); peers
	// join it. A tiny sh wrapper keys off the pod ordinal ($HOSTNAME = <sts>-<ord>) so node-0 gets
	// neither --peer nor a token, while peers seed off node-0. Multi-node mTLS token-sharing is a
	// later stage — set clusterPlaintext for a multi-node cluster today.
	seed := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d", stsName, headlessName, z.Namespace, zettaPeerPort)
	plaintext := "0"
	if z.Spec.ClusterPlaintext {
		plaintext = "1"
	}
	entry := fmt.Sprintf(`set -eu
ORD="${HOSTNAME##*-}"
A="--headless --data-dir %s --listen 0.0.0.0:%d --mcp-listen 0.0.0.0:%d"
if [ "$ORD" != "0" ]; then A="$A --peer %s"; fi
if [ "$ZETTA_CLUSTER_PLAINTEXT" = "1" ]; then A="$A --cluster-plaintext"; fi
echo "zetta node $HOSTNAME (ordinal $ORD): zetta $A"
exec zetta $A`, zettaDataDir, zettaPeerPort, zettaAPIPort, seed)

	sts.Labels = lbls
	sts.Spec.ServiceName = headlessName
	sts.Spec.Replicas = &replicas
	sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
	// Bounded, ordered scale-up so node-0 is Ready before peers try to join it.
	pmp := appsv1.ParallelPodManagement
	sts.Spec.PodManagementPolicy = pmp
	sts.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: lbls},
		Spec: corev1.PodSpec{
			ImagePullSecrets: pull,
			// The image runs zetta as uid 10001; fsGroup makes the mounted PVC group-writable so the
			// node can create /data/security (mints its cluster cert/token) etc. Azure/most CSI disks
			// mount root-owned, so a non-root node needs this.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  int64Ptr(10001),
				RunAsGroup: int64Ptr(10001),
				FSGroup:    int64Ptr(10001),
			},
			Containers: []corev1.Container{{
				Name:    "zetta",
				Image:   image,
				Command: []string{"/bin/sh", "-c", entry},
				Env: []corev1.EnvVar{
					{Name: "ZETTA_CLUSTER_PLAINTEXT", Value: plaintext},
				},
				Ports: []corev1.ContainerPort{
					{Name: "peer", ContainerPort: zettaPeerPort},
					{Name: "api", ContainerPort: zettaAPIPort},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: zettaDataDir}},
				Resources:    resources,
				// Readiness: the HTTP API answers GET /v1/server_info (bound 0.0.0.0:4800).
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/v1/server_info", Port: intstr.FromInt32(zettaAPIPort)}},
					InitialDelaySeconds: 5, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 6,
				},
				// Liveness: the peer listener is up.
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
						Port: intstr.FromInt32(zettaPeerPort)}},
					InitialDelaySeconds: 15, PeriodSeconds: 20, TimeoutSeconds: 3, FailureThreshold: 3,
				},
				// Startup: the default preflight rehashes every large-object body — allow a long boot.
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
						Port: intstr.FromInt32(zettaPeerPort)}},
					PeriodSeconds: 10, FailureThreshold: 60,
				},
			}},
		},
	}
	// Per-node PVC at /data — the CRDT doc, node_id (stable identity), certs, and JSONL/large-object data.
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(storageSize)}},
		},
	}
	if z.Spec.Storage.StorageClassName != "" {
		pvc.Spec.StorageClassName = &z.Spec.Storage.StorageClassName
	}
	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{pvc}
	return controllerutil.SetControllerReference(z, sts, r.Scheme)
}

// reconcileZettaStatus rolls the StatefulSet's readiness up into the CR status, writing only on a
// real change (anti-flap — operator principle #7).
func (r *OramaZettaClusterReconciler) reconcileZettaStatus(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, stsName string) error {
	desired := z.Spec.Replicas
	if desired < 1 {
		desired = 1
	}
	var sts appsv1.StatefulSet
	ready := int32(0)
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: stsName}, &sts); err == nil {
		ready = sts.Status.ReadyReplicas
	}

	phase := "Progressing"
	msg := fmt.Sprintf("%d/%d zetta nodes ready", ready, desired)
	if ready >= desired {
		phase, msg = "Ready", fmt.Sprintf("all %d zetta nodes ready", desired)
	} else if ready == 0 {
		phase = "Progressing"
	}

	if z.Status.Phase == phase && z.Status.ReadyNodes == ready && z.Status.TotalNodes == desired && z.Status.Message == msg {
		return nil // no change — don't touch the status subresource (anti-flap)
	}
	z.Status.Phase, z.Status.ReadyNodes, z.Status.TotalNodes, z.Status.Message = phase, ready, desired, msg
	return r.Status().Update(ctx, z)
}

func (r *OramaZettaClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaZettaCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
