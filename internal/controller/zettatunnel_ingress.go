/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// zetta-tunnel's ingress gateway (decision D8), on every cluster: the operator's own platform
// component, rendered by the operator that every cluster runs identically.
const (
	zettaTunnelIngressNamespace  = "zetta-tunnel-ingress"
	zettaTunnelIngressName       = "zetta-tunnel-ingress"
	zettaTunnelIngressRoutes     = "zetta-tunnel-ingress-routes"
	zettaTunnelIngressReport     = "zetta-tunnel-ingress-report"
	zettaTunnelIngressClass      = "zetta-tunnel"
	zettaTunnelIngressController = "orama.cloud/zetta-tunnel-ingress"
	zettaTunnelIngressPort       = 8443
	zettaTunnelIngressPublic     = 443
	zettaTunnelIngressReplicas   = 2
	zettaTunnelRoutesKey         = "routes.json"
	// nginxDefaultBodyBytes is ingress-nginx's default client_max_body_size (1m): a route without a
	// body-size annotation keeps the limit it has today.
	nginxDefaultBodyBytes = 1 << 20
)

func zettaTunnelIngressLabels() map[string]string {
	return map[string]string{zettaTunnelNameKey: zettaTunnelAppName, zettaTunnelComponentKey: "ingress"}
}

func zettaTunnelIngressObjectLabels() map[string]string {
	return withLabels(zettaTunnelIngressLabels(), map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy})
}

// zettaTunnelIngressPeer is the gateway as a NetworkPolicy peer, for the backends that admit it.
func zettaTunnelIngressPeer() networkingv1.NetworkPolicyPeer {
	return namespacePodPeer(zettaTunnelIngressNamespace, zettaTunnelIngressLabels())
}

// zettaIngressDocument is zetta-tunnel's ingress projection (docs/ingress.md).
type zettaIngressDocument struct {
	Hosts []zettaIngressHost `json:"hosts"`
}

type zettaIngressHost struct {
	Name        string              `json:"name"`
	Certificate string              `json:"certificate"`
	Key         string              `json:"key"`
	Routes      []zettaIngressRoute `json:"routes"`
}

type zettaIngressRoute struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	PathType     string `json:"path_type"`
	Target       string `json:"target"`
	MaxBodyBytes int64  `json:"max_body_bytes"`
}

// zettaIngressProjection is what the gateway serves, what it refuses to serve and why, and which
// backends it must reach.
type zettaIngressProjection struct {
	Document zettaIngressDocument
	// Files are the certificate and key files beside routes.json.
	Files map[string][]byte
	// Served are the Ingresses (namespace/name) whose every host is served.
	Served map[string]bool
	// Skipped is "namespace/name host: reason", sorted.
	Skipped []string
	// Backends are the Services the gateway sends to.
	Backends []zettaIngressBackend
	// OnNginx are operator Ingresses still on class nginx.
	OnNginx []string
}

type zettaIngressBackend struct {
	Namespace string
	Selector  map[string]string
	Port      intstr.IntOrString
}

// operatorIngress reports whether the operator wrote ingress: an orama.cloud resource controls it.
func operatorIngress(ingress *networkingv1.Ingress) bool {
	owner := metav1.GetControllerOf(ingress)
	return owner != nil && owner.APIVersion == oramav1alpha1.GroupVersion.String()
}

// ingressRefusal is why the gateway cannot serve an Ingress at all, or empty.
func ingressRefusal(ingress *networkingv1.Ingress) string {
	if owner := metav1.GetControllerOf(ingress); owner != nil && owner.Kind == "LakeApp" {
		return "Lake apps stay on ingress-nginx: their authentication is an nginx subrequest to Lake and their transfer is metered from nginx's per-Ingress counters"
	}
	for key := range ingress.Annotations {
		switch key {
		case "nginx.ingress.kubernetes.io/auth-url", "nginx.ingress.kubernetes.io/auth-signin":
			return "it needs nginx's external authentication subrequest, which the gateway does not make"
		case "nginx.ingress.kubernetes.io/backend-protocol":
			if strings.ToUpper(ingress.Annotations[key]) != "HTTP" {
				return "its backend protocol needs a backend CA the projection does not have"
			}
		case "nginx.ingress.kubernetes.io/rewrite-target", "nginx.ingress.kubernetes.io/configuration-snippet",
			"nginx.ingress.kubernetes.io/server-snippet":
			return "it uses an nginx rewrite or snippet the gateway does not implement"
		}
	}
	return ""
}

// ingressBodyLimit maps nginx's proxy-body-size to max_body_bytes: 0 is unlimited, as in nginx.
func ingressBodyLimit(ingress *networkingv1.Ingress) (int64, error) {
	value, ok := ingress.Annotations["nginx.ingress.kubernetes.io/proxy-body-size"]
	if !ok {
		return nginxDefaultBodyBytes, nil
	}
	value = strings.ToLower(strings.TrimSpace(value))
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(value, "k"):
		multiplier, value = 1<<10, strings.TrimSuffix(value, "k")
	case strings.HasSuffix(value, "m"):
		multiplier, value = 1<<20, strings.TrimSuffix(value, "m")
	case strings.HasSuffix(value, "g"):
		multiplier, value = 1<<30, strings.TrimSuffix(value, "g")
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("proxy-body-size %q is not a size", ingress.Annotations["nginx.ingress.kubernetes.io/proxy-body-size"])
	}
	return number * multiplier, nil
}

func validIngressHost(host string) bool {
	return host != "" && !strings.Contains(host, "*") && peerServiceNamePattern.MatchString(host) && strings.Contains(host, ".")
}

// sourceDeniedHosts is every host (lower case) of an operator Ingress that carries nginx's
// denylist-source-range, such as Cloud v1's /metrics deny companions (#410). The gateway cannot
// refuse a source, so it serves none of these hosts on any Ingress: the host's other routes would
// otherwise forward the denied path too (a Prefix / reaches /metrics). An Ingress being deleted
// still counts, so a companion on its way out never opens its host here before its main Ingress goes.
// This is an interim guard until #575 keeps the gateway from serving Cloud v1 at all; for the engine
// hosts, which the sandbox already admits from ingress-nginx only (bridge decision 19), it is a
// second safeguard.
func sourceDeniedHosts(ingresses []networkingv1.Ingress) map[string]bool {
	denied := map[string]bool{}
	for i := range ingresses {
		ingress := &ingresses[i]
		if !operatorIngress(ingress) || ingress.Annotations[annotationNginxDenylistSourceRange] == "" {
			continue
		}
		for _, rule := range ingress.Spec.Rules {
			denied[strings.ToLower(rule.Host)] = true
		}
	}
	return denied
}

// projectIngresses builds the gateway's projection from every Ingress the operator wrote, whatever
// its class: the gateway serves every host it can, so a cutover moves only DNS. A host it cannot
// serve with its own certificate, and an Ingress needing a feature it lacks, is skipped and reported.
func projectIngresses(ctx context.Context, c client.Client) (zettaIngressProjection, error) {
	projection := zettaIngressProjection{Files: map[string][]byte{}, Served: map[string]bool{}}
	var ingresses networkingv1.IngressList
	if err := c.List(ctx, &ingresses); err != nil {
		return projection, err
	}
	sort.Slice(ingresses.Items, func(i, j int) bool {
		a, b := &ingresses.Items[i], &ingresses.Items[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	denied := sourceDeniedHosts(ingresses.Items)
	hosts := map[string]*zettaIngressHost{}
	hostOwner := map[string]string{}
	paths := map[string]bool{}
	backends := map[string]zettaIngressBackend{}
	for i := range ingresses.Items {
		ingress := &ingresses.Items[i]
		if !operatorIngress(ingress) || !ingress.DeletionTimestamp.IsZero() {
			continue
		}
		key := ingress.Namespace + "/" + ingress.Name
		if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != zettaTunnelIngressClass {
			projection.OnNginx = append(projection.OnNginx, key)
		}
		skip := func(host, reason string) {
			projection.Skipped = append(projection.Skipped, fmt.Sprintf("%s %s: %s", key, host, reason))
		}
		if reason := ingressRefusal(ingress); reason != "" {
			skip("*", reason)
			continue
		}
		maxBody, err := ingressBodyLimit(ingress)
		if err != nil {
			skip("*", err.Error())
			continue
		}
		secretFor := map[string]string{}
		for _, tls := range ingress.Spec.TLS {
			for _, host := range tls.Hosts {
				secretFor[host] = tls.SecretName
			}
		}
		served := len(ingress.Spec.Rules) > 0
		for _, rule := range ingress.Spec.Rules {
			host := strings.ToLower(rule.Host)
			if !validIngressHost(host) {
				skip(host, "the gateway serves only exact host names")
				served = false
				continue
			}
			if denied[host] {
				skip(host, "an Ingress for this host refuses sources with nginx's denylist-source-range, which the gateway does not implement")
				served = false
				continue
			}
			secretName := secretFor[rule.Host]
			if secretName == "" {
				skip(host, "no TLS certificate Secret (cert-manager, operator#457); the gateway serves HTTPS only")
				served = false
				continue
			}
			if owner, taken := hostOwner[host]; taken && owner != ingress.Namespace+"/"+secretName {
				skip(host, "another Ingress serves this host with another certificate")
				served = false
				continue
			}
			var secret corev1.Secret
			if err := c.Get(ctx, client.ObjectKey{Namespace: ingress.Namespace, Name: secretName}, &secret); err != nil {
				if !apierrors.IsNotFound(err) {
					return projection, err
				}
				skip(host, fmt.Sprintf("TLS Secret %s does not exist yet", secretName))
				served = false
				continue
			}
			if len(secret.Data[corev1.TLSCertKey]) == 0 || len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
				skip(host, fmt.Sprintf("TLS Secret %s has no certificate or key yet", secretName))
				served = false
				continue
			}
			if _, err := pemCertificates(secret.Data[corev1.TLSCertKey]); err != nil {
				skip(host, fmt.Sprintf("TLS Secret %s holds no PEM certificate", secretName))
				served = false
				continue
			}
			var routes []zettaIngressRoute
			if rule.HTTP == nil {
				skip(host, "no HTTP paths")
				served = false
				continue
			}
			for _, path := range rule.HTTP.Paths {
				value := path.Path
				if value == "" {
					value = "/"
				}
				pathType := "Prefix"
				if path.PathType != nil && *path.PathType == networkingv1.PathTypeExact {
					pathType = "Exact"
				}
				backend := path.Backend.Service
				if backend == nil {
					skip(host, "a path's backend is not a Service")
					served = false
					continue
				}
				var service corev1.Service
				if err := c.Get(ctx, client.ObjectKey{Namespace: ingress.Namespace, Name: backend.Name}, &service); err != nil {
					if !apierrors.IsNotFound(err) {
						return projection, err
					}
					skip(host, fmt.Sprintf("backend Service %s does not exist", backend.Name))
					served = false
					continue
				}
				port, targetPort, ok := ingressBackendPort(&service, backend.Port)
				if !ok {
					skip(host, fmt.Sprintf("backend Service %s has no port %s", backend.Name, backendPortText(backend.Port)))
					served = false
					continue
				}
				routeKey := host + " " + pathType + " " + value
				if paths[routeKey] {
					skip(host, fmt.Sprintf("path %s is already routed by another Ingress", value))
					served = false
					continue
				}
				paths[routeKey] = true
				routes = append(routes, zettaIngressRoute{
					ID:           "r-" + sha256Hex([]byte(key), []byte(routeKey))[:24],
					Path:         value,
					PathType:     pathType,
					Target:       fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service.Name, service.Namespace, port),
					MaxBodyBytes: maxBody,
				})
				if len(service.Spec.Selector) > 0 {
					backendKey := service.Namespace + "/" + service.Name + "/" + targetPort.String()
					backends[backendKey] = zettaIngressBackend{Namespace: service.Namespace, Selector: copyStringMap(service.Spec.Selector), Port: targetPort}
				}
			}
			if len(routes) == 0 {
				continue
			}
			entry, ok := hosts[host]
			if !ok {
				entry = &zettaIngressHost{Name: host, Certificate: host + ".crt", Key: host + ".key"}
				hosts[host] = entry
				hostOwner[host] = ingress.Namespace + "/" + secretName
				projection.Files[host+".crt"] = secret.Data[corev1.TLSCertKey]
				projection.Files[host+".key"] = secret.Data[corev1.TLSPrivateKeyKey]
			}
			entry.Routes = append(entry.Routes, routes...)
		}
		if served {
			projection.Served[key] = true
		}
	}
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := hosts[name]
		sort.Slice(entry.Routes, func(i, j int) bool { return entry.Routes[i].ID < entry.Routes[j].ID })
		projection.Document.Hosts = append(projection.Document.Hosts, *entry)
	}
	backendKeys := make([]string, 0, len(backends))
	for key := range backends {
		backendKeys = append(backendKeys, key)
	}
	sort.Strings(backendKeys)
	for _, key := range backendKeys {
		projection.Backends = append(projection.Backends, backends[key])
	}
	sort.Strings(projection.Skipped)
	sort.Strings(projection.OnNginx)
	return projection, nil
}

func backendPortText(port networkingv1.ServiceBackendPort) string {
	if port.Name != "" {
		return port.Name
	}
	return strconv.Itoa(int(port.Number))
}

// ingressBackendPort resolves an Ingress backend port to the Service port number and the pod port it
// sends to.
func ingressBackendPort(service *corev1.Service, wanted networkingv1.ServiceBackendPort) (int32, intstr.IntOrString, bool) {
	for _, port := range service.Spec.Ports {
		if (wanted.Name != "" && port.Name == wanted.Name) || (wanted.Name == "" && port.Port == wanted.Number) {
			target := port.TargetPort
			if target.Type == intstr.Int && target.IntVal == 0 {
				target = intstr.FromInt32(port.Port)
			}
			return port.Port, target, true
		}
	}
	return 0, intstr.IntOrString{}, false
}

// zettaTunnelIngressReconciler is the cluster's one ingress gateway. It exists on every cluster: the
// gateway runs while there is a host to serve, its load balancer always, so a cutover only moves DNS.
type zettaTunnelIngressReconciler struct {
	client.Client
	// APIReader reads the manager's own pod without a cluster-wide pod informer.
	APIReader         client.Reader
	Scheme            *runtime.Scheme
	Image             string
	OperatorNamespace string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingressclasses,verbs=get;list;watch;create;update;patch

var zettaTunnelIngressRequest = reconcile.Request{NamespacedName: client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressName}}

func (r *zettaTunnelIngressReconciler) image() string {
	if r.Image != "" {
		return r.Image
	}
	return DefaultZettaTunnelImage
}

func (r *zettaTunnelIngressReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("zetta-tunnel-ingress")
	namespace, err := ensureOperatorNamespace(ctx, r.APIReader, r.Client, zettaTunnelIngressNamespace, tunnelNamespaceLabels())
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileIngressClass(ctx); err != nil {
		return ctrl.Result{}, err
	}
	pullSecrets, err := copyOperatorPullSecrets(ctx, r.Client, r.APIReader, r.OperatorNamespace, zettaTunnelIngressNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	projection, err := projectIngresses(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileSandbox(ctx, namespace, projection.Backends); err != nil {
		return ctrl.Result{}, err
	}
	routesHash, err := r.reconcileRoutes(ctx, projection)
	if err != nil {
		return ctrl.Result{}, err
	}
	service, err := r.reconcileService(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDisruptionBudget(ctx); err != nil {
		return ctrl.Result{}, err
	}
	replicas := int32(0)
	if len(projection.Document.Hosts) > 0 {
		replicas = zettaTunnelIngressReplicas
	}
	deployment, err := r.reconcileDeployment(ctx, pullSecrets, replicas)
	if err != nil {
		return ctrl.Result{}, err
	}
	available := deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.AvailableReplicas >= 1
	address := service.Status.LoadBalancer.Ingress
	if err := r.reconcileIngressStatus(ctx, projection, address, available); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileReport(ctx, projection, routesHash); err != nil {
		return ctrl.Result{}, err
	}
	if len(address) == 0 || (replicas > 0 && !available) {
		log.V(1).Info("waiting for the ingress gateway's address or pods")
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, nil
	}
	// Certificates renew in place; look again so a renewal reaches the projection.
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *zettaTunnelIngressReconciler) reconcileIngressClass(ctx context.Context) error {
	class := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelIngressClass}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, class, func() error {
		if class.ResourceVersion != "" && class.Spec.Controller != zettaTunnelIngressController {
			return fmt.Errorf("IngressClass %s exists with controller %q", zettaTunnelIngressClass, class.Spec.Controller)
		}
		class.Labels = withLabels(class.Labels, zettaTunnelIngressObjectLabels())
		class.Spec.Controller = zettaTunnelIngressController
		return nil
	})
	return err
}

func (r *zettaTunnelIngressReconciler) reconcileSandbox(ctx context.Context, namespace *corev1.Namespace, backends []zettaIngressBackend) error {
	gateway := metav1.LabelSelector{MatchLabels: zettaTunnelIngressLabels()}
	public := []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}, {IPBlock: &networkingv1.IPBlock{CIDR: "::/0"}}}
	egress := []networkingv1.NetworkPolicyEgressRule{workspaceDNSPolicy(zettaTunnelIngressNamespace, nil).Spec.Egress[0]}
	for _, backend := range backends {
		port := backend.Port
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: namespaceNameSelector(backend.Namespace),
				PodSelector:       &metav1.LabelSelector{MatchLabels: copyStringMap(backend.Selector)},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: &port}},
		})
	}
	policies := []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(
			"Public HTTPS reaches the ingress gateway's TLS listener.",
			networkPolicy("gateway-public-ingress", gateway, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{From: public, Ports: tcpPorts(zettaTunnelIngressPort)}}, nil),
		),
		sandboxAllowance(
			"The ingress gateway reaches only the backend pods of the routes it serves, on their ports, and DNS.",
			networkPolicy("gateway-backend-egress", gateway, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil, egress),
		),
	}
	_, err := reconcileOwnedNetworkSandbox(ctx, r.Client, r.Scheme, namespace, zettaTunnelIngressNamespace,
		map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy}, metav1.LabelSelector{}, policies)
	return err
}

func zettaIngressRoutesData(projection zettaIngressProjection) (map[string][]byte, error) {
	document := projection.Document
	if document.Hosts == nil {
		document.Hosts = []zettaIngressHost{}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{zettaTunnelRoutesKey: append(encoded, '\n')}
	for name, value := range projection.Files {
		data[name] = append([]byte(nil), value...)
	}
	return data, nil
}

// reconcileRoutes writes routes.json and every certificate and key into one Secret, so the
// kubelet projects a complete generation at once.
func (r *zettaTunnelIngressReconciler) reconcileRoutes(ctx context.Context, projection zettaIngressProjection) (string, error) {
	data, err := zettaIngressRoutesData(projection)
	if err != nil {
		return "", err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelIngressRoutes, Namespace: zettaTunnelIngressNamespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.ResourceVersion != "" && secret.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("Secret %s/%s is not the operator's", zettaTunnelIngressNamespace, zettaTunnelIngressRoutes)
		}
		secret.Labels = zettaTunnelIngressObjectLabels()
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = data
		return nil
	})
	return sha256Hex(data[zettaTunnelRoutesKey]), err
}

func (r *zettaTunnelIngressReconciler) reconcileService(ctx context.Context) (*corev1.Service, error) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelIngressName, Namespace: zettaTunnelIngressNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.ResourceVersion != "" && service.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("Service %s/%s is not the operator's", zettaTunnelIngressNamespace, zettaTunnelIngressName)
		}
		service.Labels = zettaTunnelIngressObjectLabels()
		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		// Local, as ingress-nginx needs on AKS: the cloud probe reaches only nodes with a ready
		// gateway, and the client's address is kept for the audit records.
		service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeLocal
		service.Spec.Selector = zettaTunnelIngressLabels()
		service.Spec.Ports = []corev1.ServicePort{{
			Name: "https", Protocol: corev1.ProtocolTCP, Port: zettaTunnelIngressPublic, TargetPort: intstr.FromInt32(zettaTunnelIngressPort),
		}}
		return nil
	})
	return service, err
}

func (r *zettaTunnelIngressReconciler) reconcileDisruptionBudget(ctx context.Context) error {
	budget := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelIngressName, Namespace: zettaTunnelIngressNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, budget, func() error {
		if budget.ResourceVersion != "" && budget.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("PodDisruptionBudget %s/%s is not the operator's", zettaTunnelIngressNamespace, zettaTunnelIngressName)
		}
		budget.Labels = zettaTunnelIngressObjectLabels()
		minAvailable := intstr.FromInt32(1)
		budget.Spec.MinAvailable = &minAvailable
		budget.Spec.Selector = &metav1.LabelSelector{MatchLabels: zettaTunnelIngressLabels()}
		return nil
	})
	return err
}

func (r *zettaTunnelIngressReconciler) reconcileDeployment(ctx context.Context, pullSecrets []string, replicas int32) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelIngressName, Namespace: zettaTunnelIngressNamespace}}
	var references []corev1.LocalObjectReference
	for _, name := range pullSecrets {
		references = append(references, corev1.LocalObjectReference{Name: name})
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if deployment.ResourceVersion != "" && deployment.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("Deployment %s/%s is not the operator's", zettaTunnelIngressNamespace, zettaTunnelIngressName)
		}
		deployment.Labels = zettaTunnelIngressObjectLabels()
		deployment.Spec.Replicas = int32Ptr(replicas)
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: zettaTunnelIngressLabels()}
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: intstrPtr(intstr.FromInt32(0)), MaxSurge: intstrPtr(intstr.FromInt32(1)),
			},
		}
		deployment.Spec.Template.Labels = zettaTunnelIngressObjectLabels()
		deployment.Spec.Template.Spec = corev1.PodSpec{
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			ImagePullSecrets:              references,
			SecurityContext:               tunnelPodSecurityContext(),
			TerminationGracePeriodSeconds: int64Ptr(30),
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector: &metav1.LabelSelector{MatchLabels: zettaTunnelIngressLabels()},
			}},
			Containers: []corev1.Container{{
				Name:            "gateway",
				Image:           r.image(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args: []string{
					"ingress",
					"--configuration", "/etc/zetta-ingress/" + zettaTunnelRoutesKey,
					"--listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelIngressPort),
					"--health-listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelProbePort),
					"--max-sessions", "4096",
					"--max-requests", "4096",
					"--audit", "-",
				},
				Ports: []corev1.ContainerPort{
					{Name: "https", ContainerPort: zettaTunnelIngressPort, Protocol: corev1.ProtocolTCP},
					{Name: "probe", ContainerPort: zettaTunnelProbePort, Protocol: corev1.ProtocolTCP},
				},
				ReadinessProbe: zettaTunnelProbe("/readyz", 5),
				LivenessProbe:  zettaTunnelProbe("/healthz", 10),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
				SecurityContext: tunnelContainerSecurityContext(),
				VolumeMounts:    []corev1.VolumeMount{{Name: "routes", MountPath: "/etc/zetta-ingress", ReadOnly: true}},
			}},
			// Mounted without subPath, so a new generation of routes and certificates reaches the
			// gateway atomically through the kubelet's ..data swap; it re-reads every two seconds.
			Volumes: []corev1.Volume{{Name: "routes", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: zettaTunnelIngressRoutes, DefaultMode: int32Ptr(0o440),
			}}}},
		}
		return nil
	})
	return deployment, err
}

// reconcileIngressStatus gives an Ingress of class zetta-tunnel the gateway's address only when the
// gateway serves all of it and is available; otherwise its address is cleared, so DNS never points
// at a gateway that would refuse the host.
func (r *zettaTunnelIngressReconciler) reconcileIngressStatus(
	ctx context.Context,
	projection zettaIngressProjection,
	address []corev1.LoadBalancerIngress,
	available bool,
) error {
	var ingresses networkingv1.IngressList
	if err := r.List(ctx, &ingresses); err != nil {
		return err
	}
	for i := range ingresses.Items {
		ingress := &ingresses.Items[i]
		if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != zettaTunnelIngressClass {
			continue
		}
		var desired []networkingv1.IngressLoadBalancerIngress
		if available && projection.Served[ingress.Namespace+"/"+ingress.Name] {
			for _, entry := range address {
				desired = append(desired, networkingv1.IngressLoadBalancerIngress{IP: entry.IP, Hostname: entry.Hostname})
			}
		}
		if apiequality.Semantic.DeepEqual(ingress.Status.LoadBalancer.Ingress, desired) {
			continue
		}
		ingress.Status.LoadBalancer.Ingress = desired
		if err := r.Status().Update(ctx, ingress); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileReport records what the gateway serves, what it refuses and why, and which Ingresses are
// still on ingress-nginx: a cluster may drop ingress-nginx only when the last two are empty.
func (r *zettaTunnelIngressReconciler) reconcileReport(ctx context.Context, projection zettaIngressProjection, routesHash string) error {
	var hosts []string
	for _, host := range projection.Document.Hosts {
		hosts = append(hosts, host.Name)
	}
	report := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelIngressReport, Namespace: zettaTunnelIngressNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, report, func() error {
		if report.ResourceVersion != "" && report.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("ConfigMap %s/%s is not the operator's", zettaTunnelIngressNamespace, zettaTunnelIngressReport)
		}
		report.Labels = zettaTunnelIngressObjectLabels()
		report.Data = map[string]string{
			"served":   strings.Join(hosts, "\n"),
			"skipped":  strings.Join(projection.Skipped, "\n"),
			"on-nginx": strings.Join(projection.OnNginx, "\n"),
			"routes":   routesHash,
		}
		return nil
	})
	return err
}

// copyOperatorPullSecrets copies the manager pod's own image pull secrets into namespace and
// returns their names. A manager not running in a pod (a local run) copies none.
func copyOperatorPullSecrets(ctx context.Context, c client.Client, reader client.Reader, operatorNamespace, namespace string) ([]string, error) {
	podName := os.Getenv("HOSTNAME")
	if reader == nil || operatorNamespace == "" || podName == "" {
		return nil, nil
	}
	var pod corev1.Pod
	if err := reader.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: podName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, reference := range pod.Spec.ImagePullSecrets {
		var source corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: reference.Name}, &source); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		copy := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: reference.Name, Namespace: namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, c, copy, func() error {
			if copy.ResourceVersion != "" && copy.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
				return fmt.Errorf("Secret %s/%s is not the operator's", namespace, reference.Name)
			}
			copy.Labels = map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy}
			copy.Type = source.Type
			copy.Data = map[string][]byte{}
			for key, value := range source.Data {
				copy.Data[key] = append([]byte(nil), value...)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		names = append(names, reference.Name)
	}
	return names, nil
}

func (r *zettaTunnelIngressReconciler) request(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{zettaTunnelIngressRequest}
}

// startupRunnable reconciles the gateway once when the manager starts, so it exists on a cluster
// with no Ingress yet.
type zettaTunnelIngressStartup struct{ r *zettaTunnelIngressReconciler }

func (s zettaTunnelIngressStartup) Start(ctx context.Context) error {
	_, err := s.r.Reconcile(ctx, zettaTunnelIngressRequest)
	if err != nil {
		logf.FromContext(ctx).Error(err, "first ingress gateway reconcile failed; the watches retry it")
	}
	return nil
}

func (s zettaTunnelIngressStartup) NeedLeaderElection() bool { return true }

func (r *zettaTunnelIngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(r.request)
	own := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		if object.GetNamespace() != zettaTunnelIngressNamespace {
			return nil
		}
		return r.request(ctx, object)
	})
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("zettatunnel-ingress").
		Watches(&networkingv1.Ingress{}, all).
		Watches(&corev1.Service{}, all).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			secret, ok := object.(*corev1.Secret)
			if ok && (secret.Type == corev1.SecretTypeTLS || secret.Namespace == zettaTunnelIngressNamespace) {
				return r.request(ctx, object)
			}
			return nil
		})).
		Watches(&appsv1.Deployment{}, own).
		Watches(&corev1.ConfigMap{}, own).
		Watches(&oramav1alpha1.NetworkSandbox{}, own).
		Complete(r); err != nil {
		return err
	}
	return mgr.Add(zettaTunnelIngressStartup{r: r})
}
