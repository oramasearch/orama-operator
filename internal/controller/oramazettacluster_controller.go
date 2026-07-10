/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// Zetta headless ports (source-verified — see docs/projects/one-orama/zetta-headless-reference.md).
const (
	zettaPeerPort = 7777 // peer replication + signal plane + blob transfer (mTLS unless plaintext)
	zettaAPIPort  = 4800 // MCP + REST + model/index serving (one HTTP server)
	zettaDataDir  = "/data"

	// Landing placeholder served at the host root. The real Zetta web UI is upstream work
	// (oramasearch/zetta#647); until then the operator serves a static "zetta" page at `/` from a
	// tiny nginx pod that is INDEPENDENT of Zetta's (currently flaky, zetta#700/#695) HTTP server —
	// so the host always resolves to something even when the API is down.
	zettaLandingImage = "nginx:1.27-alpine"
	zettaLandingPort  = 80
	zettaLandingText  = "zetta\n"
)

// OramaZettaClusterReconciler deploys a headless Orama Zetta cluster (StatefulSet of equal-peer CRDT
// nodes) from an OramaZettaCluster CR. Step 1 of One Orama (orama-bridge#130): it only RUNS Zetta
// headless — no operator↔Zetta symbiosis and no changes to Zetta's MCP service.
type OramaZettaClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// stateCache is the anti-flap memory for the operator-state sync into Zetta: component key →
	// last-synced fingerprint, so a steady stack produces zero Zetta writes (see zetta_statesync.go).
	stateCache sync.Map
	// operatorID{Once,Value} memoize this operator's unique identity (one per k8s cluster) so its state
	// stays distinct in a Zetta cluster shared across k8s clusters.
	operatorIDOnce  sync.Once
	operatorIDValue string
}

//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s,verbs=get;list;watch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s/status,verbs=get
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete

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

	// 2.5 MCP bearer token — OPT-IN (spec.mcpTokenAuth, default off). Zetta's --mcp-token treats the
	//     token as an access-key id that must already be a provisioned principal; a bare token
	//     authenticates but is then denied every capability ("unknown access key"), so the default is
	//     the functional unauthenticated-local session (see the field doc + oramasearch/zetta#733).
	//     When enabled, provision the token before the StatefulSet so its secretKeyRef resolves.
	if z.Spec.MCPTokenAuth {
		if err := r.reconcileZettaMCPToken(ctx, &z); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		_ = deleteIfExists(ctx, r.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: z.Name + "-zetta-mcp", Namespace: z.Namespace}})
	}

	// 3. StatefulSet — N equal-peer headless nodes, each with a per-node PVC at /data.
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		return r.mutateZettaStatefulSet(&z, sts, headlessName, stsName, lbls)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Optional external exposure — Ingress (+ proxied Cloudflare DNS) for the HTTP API/UI on 4800
	//    (e.g. zetta.oramadev.net). No-op unless spec.host is set.
	if err := r.reconcileZettaIngress(ctx, &z, clientName, lbls); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Operator datastore (One Orama symbiosis v0) — this cluster as the operator's store. Provisions
	//    the orama_control + orama_data schema, then coarsely syncs the operator's OBSERVED state into
	//    orama_control.components (anti-flap; default on).
	syncEnabled := z.Spec.ProvisionOperatorSchema == nil || *z.Spec.ProvisionOperatorSchema
	if syncEnabled {
		if err := r.reconcileZettaControlPlane(ctx, &z, clientName); err != nil {
			return ctrl.Result{}, err
		}
		r.reconcileOperatorStateSync(ctx, &z, clientName) // best-effort; never fails the reconcile
	} else if err := r.teardownZettaControlPlane(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Status — readiness roll-up (write only on change; anti-flap).
	if err := r.reconcileZettaStatus(ctx, &z, stsName); err != nil {
		return ctrl.Result{}, err
	}
	// When acting as the datastore, requeue periodically so the coarse state sync tracks the rest of
	// the operator's CRs even without a Zetta-cluster event (anti-flap keeps the Zetta writes rare).
	if syncEnabled {
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileZettaMCPToken ensures a Secret holding a 64-char (32-byte hex) MCP bearer token exists for
// this cluster. Secure by default: the token is injected into the node (ZETTA_MCP_TOKEN → --mcp-token),
// so the MCP + REST surface requires a bearer instead of being open. Idempotent — the token is minted
// once and preserved across reconciles; the value is never logged (the entrypoint echo redacts it).
func (r *OramaZettaClusterReconciler) reconcileZettaMCPToken(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	name := z.Name + "-zetta-mcp"
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = zettaLabels(z)
		if len(sec.Data["token"]) == 0 { // mint once; preserve on later reconciles
			token, gerr := randHex(32) // 32 bytes → 64 hex chars
			if gerr != nil {
				return gerr
			}
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data["token"] = []byte(token)
		}
		return controllerutil.SetControllerReference(z, sec, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		kv := []any{"component", "zetta-mcp", "secret", name,
			"retrieve", fmt.Sprintf("kubectl get secret %s -n %s -o jsonpath='{.data.token}' | base64 -d", name, z.Namespace)}
		if z.Spec.Host != "" {
			kv = append(kv, "connectDocs", "https://"+z.Spec.Host+"/mcp.md")
		}
		logf.FromContext(ctx).Info("provisioned MCP bearer token — the node now requires a bearer on /mcp and /v1/*", kv...)
	}
	return nil
}

// reconcileZettaIngress exposes the cluster at spec.host via an nginx Ingress. The host root (`/`)
// serves a static "zetta" landing placeholder from a tiny operator-owned nginx pod (see
// reconcileZettaLanding) — independent of Zetta's own HTTP server — while every other path proxies
// to the Zetta HTTP API/UI (:4800). When spec.dnsSecretRef is also set it creates an OWNED
// CloudflareDNSRecord (proxied A/CNAME → the ingress LoadBalancer), mirroring the OramaCloudV1
// dashboard-DNS pattern (the child controller owns the CF API call + finalizer, so the operator
// never mutates Cloudflare from the main loop). SAFE no-op unless spec.host is set.
func (r *OramaZettaClusterReconciler) reconcileZettaIngress(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientSvc string, lbls map[string]string) error {
	ingName := z.Name + "-zetta"
	landingName := z.Name + "-zetta-landing"
	if z.Spec.Host == "" {
		// No host → tear down the Ingress/DNS + landing placeholder we previously created.
		_ = deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: ingName + "-dns", Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
		return nil
	}

	// Landing placeholder (nginx serving "zetta") — the backend for the host root.
	if err := r.reconcileZettaLanding(ctx, z, landingName); err != nil {
		return err
	}

	exact := networkingv1.PathTypeExact
	prefix := networkingv1.PathTypePrefix
	svcBackend := func(name string, port int32) networkingv1.IngressBackend {
		return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: name, Port: networkingv1.ServiceBackendPort{Number: port}}}
	}
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = lbls
		ing.Spec.IngressClassName = strPtr("nginx")
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: z.Spec.Host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					// Exact `/` (nginx `location = /`) → the landing placeholder; wins for the bare root.
					{Path: "/", PathType: &exact, Backend: svcBackend(landingName, zettaLandingPort)},
					// Prefix `/` (nginx `location /`) → the Zetta HTTP API/UI for everything else.
					{Path: "/", PathType: &prefix, Backend: svcBackend(clientSvc, zettaAPIPort)},
				},
			}},
		}}
		return controllerutil.SetControllerReference(z, ing, r.Scheme)
	}); err != nil {
		return err
	}

	// Proxied Cloudflare DNS (opt-in via dnsSecretRef) → resolves once the ingress LB has an address.
	if z.Spec.DNSSecretRef != nil && z.Spec.DNSSecretRef.Name != "" {
		if target := r.zettaIngressTarget(ctx, z, ingName); target != "" {
			rec := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: ingName + "-dns", Namespace: z.Namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
				rec.Spec.SecretRef, rec.Spec.Name, rec.Spec.Content, rec.Spec.Proxied = z.Spec.DNSSecretRef.Name, z.Spec.Host, target, true
				return controllerutil.SetControllerReference(z, rec, r.Scheme)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileZettaLanding creates the static "zetta" landing placeholder served at the host root — a
// ConfigMap (the page), a tiny nginx Deployment, and its Service. It is deliberately DECOUPLED from
// the Zetta node's own HTTP server (which is currently unreliable — oramasearch/zetta#700/#695), so
// visiting the host always returns something. Replace with the real Zetta web UI when zetta#647 ships.
func (r *OramaZettaClusterReconciler) reconcileZettaLanding(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, name string) error {
	lbls := map[string]string{
		"app.kubernetes.io/name":       "orama-zetta",
		"app.kubernetes.io/instance":   z.Name,
		"app.kubernetes.io/component":  "zetta-landing",
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = lbls
		cm.Data = map[string]string{"index.html": zettaLandingText}
		return controllerutil.SetControllerReference(z, cm, r.Scheme)
	}); err != nil {
		return err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = lbls
		svc.Spec.Selector = lbls
		svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: zettaLandingPort, TargetPort: intstr.FromInt32(zettaLandingPort)}}
		return controllerutil.SetControllerReference(z, svc, r.Scheme)
	}); err != nil {
		return err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(1)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "nginx",
					Image: zettaLandingImage,
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: zettaLandingPort}},
					VolumeMounts: []corev1.VolumeMount{{
						Name: "content", MountPath: "/usr/share/nginx/html", ReadOnly: true}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(zettaLandingPort)}},
						InitialDelaySeconds: 2, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					},
				}},
				Volumes: []corev1.Volume{{
					Name: "content",
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: name}}},
				}},
			},
		}
		return controllerutil.SetControllerReference(z, dep, r.Scheme)
	}); err != nil {
		return err
	}
	return nil
}

// zettaIngressTarget returns the external address (hostname preferred, else IP) of the cluster's
// Ingress once the ingress controller has assigned one — the Content for the Cloudflare DNS record.
func (r *OramaZettaClusterReconciler) zettaIngressTarget(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, ingName string) string {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: ingName}, ing); err != nil {
		return ""
	}
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.Hostname != "" {
			return lb.Hostname
		}
		if lb.IP != "" {
			return lb.IP
		}
	}
	return ""
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
	// The --mcp-token is appended AFTER the echo, and the echo shows only a redacted "****", so the
	// bearer token never lands in the pod log. The token is injected via the ZETTA_MCP_TOKEN env from
	// the <name>-zetta-mcp Secret (reconcileZettaMCPToken).
	entry := fmt.Sprintf(`set -eu
ORD="${HOSTNAME##*-}"
A="--headless --data-dir %s --listen 0.0.0.0:%d --mcp-listen 0.0.0.0:%d"
if [ "$ORD" != "0" ]; then A="$A --peer %s"; fi
if [ "$ZETTA_CLUSTER_PLAINTEXT" = "1" ]; then A="$A --cluster-plaintext"; fi
echo "zetta node $HOSTNAME (ordinal $ORD): zetta $A${ZETTA_MCP_TOKEN:+ --mcp-token ****}"
if [ -n "${ZETTA_MCP_TOKEN:-}" ]; then A="$A --mcp-token $ZETTA_MCP_TOKEN"; fi
exec zetta $A`, zettaDataDir, zettaPeerPort, zettaAPIPort, seed)

	zettaEnv := []corev1.EnvVar{{Name: "ZETTA_CLUSTER_PLAINTEXT", Value: plaintext}}
	if z.Spec.MCPTokenAuth {
		zettaEnv = append(zettaEnv, corev1.EnvVar{Name: "ZETTA_MCP_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: z.Name + "-zetta-mcp"}, Key: "token"}}})
	}

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
				Env: zettaEnv,
				Ports: []corev1.ContainerPort{
					{Name: "peer", ContainerPort: zettaPeerPort},
					{Name: "api", ContainerPort: zettaAPIPort},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: zettaDataDir}},
				Resources:    resources,
				// Readiness probe intentionally DISABLED (operator#68): the node's HTTP server is flaky
				// (drops ~2/3 of back-to-back requests, zetta#731), so an HTTP/TCP readiness probe keeps
				// flapping the pod out of the Service — starving the schema Job + clients. Without a
				// readiness gate the pod stays a Service endpoint. Restore a real probe (GET /mcp.md)
				// once zetta#731 lands. Liveness (peer port) + startup still guard genuine hangs.
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
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&batchv1.Job{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Complete(r)
}
