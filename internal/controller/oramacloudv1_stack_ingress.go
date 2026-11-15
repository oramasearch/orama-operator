/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Each OramaCloudV1 stack runs its own ingress-nginx, with its own IngressClass and LoadBalancer
// (operator#573, LSR-210 operator#805; bridge decision 5b of 2026-10-16). Cloud v1 never shares the
// platform ingress-nginx, or zetta-tunnel's gateway, with Lake: a flood on app.orama.ing no longer
// lands on the replicas that serve lake.live, and a misrouted gateway host cannot reach a v1 pod.
//
// Decided under Steve's 2026-11-14 delegation, the proposals on #573:
//   - the class is derived from the CR, never a spec field: stackIngressClassName, the same rule on
//     every cluster; spec.components.ingressClassName is ignored (kept so stored objects stay valid);
//   - the operator renders the controller's objects itself, so there is no per-stack chart version to
//     keep in step with helmfile.yaml;
//   - one LoadBalancer, so one public address, per stack;
//   - the image is upstream's controller by digest, the version the platform's ingress-nginx runs
//     (chart 4.15.1), overridable through spec.images.overrides["ingress-nginx"] (a private mirror).
//
// The controller is upstream's namespace-scoped mode (the chart's controller.scope.enabled and
// rbac.scope): it watches only the stack's namespace, serves only its own class
// (--ingress-class-by-name, so it needs no cluster-wide read of IngressClasses), and its account is
// bound in that namespace alone to the chart's ClusterRole orama-v1-ingress-nginx. It has no
// admission webhook and refuses snippet annotations, like the platform's. It listens on 8080 and 8443
// so it needs no capability; its Service maps 80 and 443 to them.
//
// The IngressClass is cluster-scoped, so it cannot be owned by the namespaced CR. It carries the
// stack's namespace and name as labels, is deleted with the stack's other ingress objects when the
// stack stops serving any host, when the CR is gone (Reconcile's not-found path), and by the sweep in
// every reconcile for a stack deleted while the operator was down.

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingressclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=orama-v1-ingress-nginx

const (
	// stackIngressComponent labels every object of a stack's controller.
	stackIngressComponent = "ingress-nginx"
	// stackIngressImageKey is the spec.images.overrides key of the controller image.
	stackIngressImageKey = "ingress-nginx"
	// stackIngressDefaultImage is upstream's controller at the version and digest the platform's
	// ingress-nginx runs (chart 4.15.1, controller v1.15.1).
	stackIngressDefaultImage = "registry.k8s.io/ingress-nginx/controller:v1.15.1@sha256:594ceea76b01c592858f803f9ff4d2cb40542cae2060410b2c95f75907d659e1"
	// stackIngressClusterRole is the chart's ClusterRole (chart/templates/rbac.yaml) each stack's
	// controller account is bound to in its own namespace: upstream's scoped Role.
	stackIngressClusterRole = "orama-v1-ingress-nginx"
	// stackIngressHTTPPort, stackIngressHTTPSPort and stackIngressHealthPort are the controller's
	// container ports: unprivileged, so the container drops every capability.
	stackIngressHTTPPort   = 8080
	stackIngressHTTPSPort  = 8443
	stackIngressHealthPort = 10254
	// stackIngressStackNamespaceLabel and stackIngressStackNameLabel name the stack an IngressClass
	// serves, for its deletion.
	stackIngressStackNamespaceLabel = "orama.cloud/stack-namespace"
	stackIngressStackNameLabel      = "orama.cloud/stack-name"
	// stackIngressManagedBy marks the IngressClasses this file writes.
	stackIngressManagedBy = "orama-cloud-v1-operator"
)

// cloudflareProxyCIDRs are Cloudflare's published ranges (https://www.cloudflare.com/ips/, read
// 2026-10-21), the same list as the platform ingress-nginx's proxy-real-ip-cidr in helmfile.yaml
// (TestStackIngressTrustsTheSameProxiesAsThePlatform keeps them equal).
const cloudflareProxyCIDRs = "173.245.48.0/20,103.21.244.0/22,103.22.200.0/22,103.31.4.0/22,141.101.64.0/18,108.162.192.0/18,190.93.240.0/20,188.114.96.0/20,197.234.240.0/22,198.41.128.0/17,162.158.0.0/15,104.16.0.0/13,104.24.0.0/14,172.64.0.0/13,131.0.72.0/22,2400:cb00::/32,2606:4700::/32,2803:f800::/32,2405:b500::/32,2405:8100::/32,2a06:98c0::/29,2c0f:f248::/32"

// stackIngressClassFor is the IngressClass of the stack namespace/name: orama-v1-<namespace>-<name>,
// cut to 63 characters with a hash of the whole when longer, so it is a valid label value too.
func stackIngressClassFor(namespace, name string) string {
	class := "orama-v1-" + namespace + "-" + name
	if len(class) <= 63 {
		return class
	}
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return class[:54] + "-" + hex.EncodeToString(sum[:])[:8]
}

// stackIngressClassName is the stack's IngressClass, which every Ingress it writes names.
func stackIngressClassName(oc *oramav1alpha1.OramaCloudV1) string {
	return stackIngressClassFor(oc.Namespace, oc.Name)
}

// stackIngressControllerValue is the IngressClass's spec.controller and the controller's
// --controller-class: orama.cloud/<class>.
func stackIngressControllerValue(oc *oramav1alpha1.OramaCloudV1) string {
	return "orama.cloud/" + stackIngressClassName(oc)
}

// stackIngressName is the name of the controller's namespaced objects: Deployment, Service,
// ConfigMap, ServiceAccount and RoleBinding.
func stackIngressName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-ingress-nginx"
}

// stackIngressImage is the controller image: the override, else upstream's by digest.
func stackIngressImage(oc *oramav1alpha1.OramaCloudV1) string {
	if o := oc.Spec.Images.Overrides[stackIngressImageKey]; o != "" {
		return o
	}
	return stackIngressDefaultImage
}

// stackServesIngress reports whether the stack writes any Ingress: a public host on an enabled
// dashboard or docs, or a published writer or reader. (The oramasearch app's reconciler is not
// wired, so it writes none.)
func stackServesIngress(oc *oramav1alpha1.OramaCloudV1) bool {
	c := oc.Spec.Components
	return (c.Dashboard.Enabled && c.Dashboard.Host != "") ||
		(c.Docs.Enabled && c.Docs.Host != "") ||
		writerPublicHost(oc) != "" || readerPublicHost(oc) != ""
}

// stackIngressConfig is the controller's ConfigMap: the client address from Cloudflare's
// CF-Connecting-IP only when the connection comes from Cloudflare (as the platform's, operator#719),
// and no snippet annotations.
func stackIngressConfig() map[string]string {
	return map[string]string{
		"allow-snippet-annotations": "false",
		"enable-real-ip":            "true",
		"forwarded-for-header":      "CF-Connecting-IP",
		"proxy-real-ip-cidr":        cloudflareProxyCIDRs,
	}
}

// stackIngressArgs are the controller's flags: its own namespace, class and controller value, the
// Service it publishes as the Ingresses' address, and unprivileged ports.
func stackIngressArgs(oc *oramav1alpha1.OramaCloudV1) []string {
	name := stackIngressName(oc)
	return []string{
		"/nginx-ingress-controller",
		"--publish-service=$(POD_NAMESPACE)/" + name,
		"--election-id=" + name + "-leader",
		"--controller-class=" + stackIngressControllerValue(oc),
		"--ingress-class=" + stackIngressClassName(oc),
		"--ingress-class-by-name=true",
		"--watch-ingress-without-class=false",
		"--watch-namespace=$(POD_NAMESPACE)",
		"--configmap=$(POD_NAMESPACE)/" + name,
		fmt.Sprintf("--http-port=%d", stackIngressHTTPPort),
		fmt.Sprintf("--https-port=%d", stackIngressHTTPSPort),
	}
}

// stackIngressPodSpec is the controller pod: upstream's controller container, non-root, every
// capability dropped, its account's token mounted (it watches the API).
func stackIngressPodSpec(oc *oramav1alpha1.OramaCloudV1) corev1.PodSpec {
	health := func(failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(stackIngressHealthPort), Scheme: corev1.URISchemeHTTP}},
			InitialDelaySeconds: 10, PeriodSeconds: 10, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: failures,
		}
	}
	sc := hardenedSecurityContext()
	sc.RunAsNonRoot = boolPtr(true)
	sc.RunAsUser = int64Ptr(101)
	sc.RunAsGroup = int64Ptr(82)
	return corev1.PodSpec{
		ServiceAccountName:            stackIngressName(oc),
		AutomountServiceAccountToken:  boolPtr(true),
		EnableServiceLinks:            boolPtr(false),
		TerminationGracePeriodSeconds: int64Ptr(300),
		NodeSelector:                  map[string]string{"kubernetes.io/os": "linux"},
		Containers: []corev1.Container{{
			Name:  "controller",
			Image: stackIngressImage(oc),
			Args:  stackIngressArgs(oc),
			Env: []corev1.EnvVar{
				{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
				{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
				{Name: "LD_PRELOAD", Value: "/usr/local/lib/libmimalloc.so"},
			},
			Ports: []corev1.ContainerPort{
				{Name: "http", ContainerPort: stackIngressHTTPPort, Protocol: corev1.ProtocolTCP},
				{Name: "https", ContainerPort: stackIngressHTTPSPort, Protocol: corev1.ProtocolTCP},
			},
			Lifecycle:       &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/wait-shutdown"}}}},
			LivenessProbe:   health(5),
			ReadinessProbe:  health(3),
			Resources:       *resourceReqs("90Mi", "100m", "512Mi"),
			SecurityContext: sc,
		}},
	}
}

// reconcileStackIngress writes the stack's controller before any component writes an Ingress that
// names its class, or removes it all once the stack serves no host.
func (r *OramaCloudV1Reconciler) reconcileStackIngress(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if err := r.sweepStackIngressClasses(ctx); err != nil {
		return err
	}
	if !stackServesIngress(oc) {
		return r.teardownStackIngress(ctx, oc)
	}
	name := stackIngressName(oc)
	lbls := labelsFor(oc, stackIngressComponent)

	class := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: stackIngressClassName(oc)}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, class, func() error {
		if class.ResourceVersion != "" && (class.Labels[stackIngressStackNamespaceLabel] != oc.Namespace ||
			class.Labels[stackIngressStackNameLabel] != oc.Name) {
			return fmt.Errorf("IngressClass %s exists and is not this stack's", class.Name)
		}
		class.Labels = map[string]string{
			"app.kubernetes.io/managed-by":  stackIngressManagedBy,
			"app.kubernetes.io/component":   stackIngressComponent,
			stackIngressStackNamespaceLabel: oc.Namespace,
			stackIngressStackNameLabel:      oc.Name,
		}
		// spec.controller is immutable: a class with another controller fails here, and is reported.
		class.Spec.Controller = stackIngressControllerValue(oc)
		return nil
	}); err != nil {
		return fmt.Errorf("IngressClass %s: %w", class.Name, err)
	}

	// The controller's own account, which mounts its token: it watches its namespace through the
	// API. A foreign account of the same name is a conflict, so it inherits no other permission.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		if sa.ResourceVersion != "" && !metav1.IsControlledBy(sa, oc) {
			return fmt.Errorf("ServiceAccount %s/%s is not controlled by %s", sa.Namespace, sa.Name, oc.Name)
		}
		sa.Labels = lbls
		sa.AutomountServiceAccountToken = boolPtr(true)
		return controllerutil.SetControllerReference(oc, sa, r.Scheme)
	}); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = lbls
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: stackIngressClusterRole}
		binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: oc.Namespace}}
		return controllerutil.SetControllerReference(oc, binding, r.Scheme)
	}); err != nil {
		return err
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = lbls
		cm.Data = stackIngressConfig()
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}

	selector := map[string]string{
		"app.kubernetes.io/name":      appName,
		"app.kubernetes.io/instance":  oc.Name,
		"app.kubernetes.io/component": stackIngressComponent,
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = lbls
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		// Local: the cloud's probe passes only where a ready controller pod runs, and the client
		// address reaches nginx (the platform's reason, operator#38).
		svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		svc.Spec.Selector = selector
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("http")},
			{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("https")},
		}
		return controllerutil.SetControllerReference(oc, svc, r.Scheme)
	}); err != nil {
		return err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(int32(profileInstances(oc)))
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template.Labels = lbls
		dep.Spec.Template.Spec = stackIngressPodSpec(oc)
		boundV1PodStorage(&dep.Spec.Template.Spec)
		return controllerutil.SetControllerReference(oc, dep, r.Scheme)
	}); err != nil {
		return err
	}
	return nil
}

// teardownStackIngress removes the stack's controller: its namespaced objects and its IngressClass.
func (r *OramaCloudV1Reconciler) teardownStackIngress(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if err := r.teardownByComponent(ctx, oc, stackIngressComponent); err != nil {
		return err
	}
	name := stackIngressName(oc)
	for _, obj := range []client.Object{
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}},
	} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, oc) {
			continue
		}
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return r.deleteStackIngressClass(ctx, oc.Namespace, oc.Name)
}

// deleteStackIngressClass deletes the IngressClass of the stack namespace/name, if it is that
// stack's.
func (r *OramaCloudV1Reconciler) deleteStackIngressClass(ctx context.Context, namespace, name string) error {
	class := &networkingv1.IngressClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: stackIngressClassFor(namespace, name)}, class); err != nil {
		return client.IgnoreNotFound(err)
	}
	if class.Labels[stackIngressStackNamespaceLabel] != namespace || class.Labels[stackIngressStackNameLabel] != name {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, class))
}

// sweepStackIngressClasses deletes every stack IngressClass whose OramaCloudV1 no longer exists, so a
// stack deleted while the operator was not running leaves no class behind.
func (r *OramaCloudV1Reconciler) sweepStackIngressClasses(ctx context.Context) error {
	var classes networkingv1.IngressClassList
	if err := r.List(ctx, &classes, client.MatchingLabels{
		"app.kubernetes.io/managed-by": stackIngressManagedBy,
		"app.kubernetes.io/component":  stackIngressComponent,
	}); err != nil {
		return err
	}
	for i := range classes.Items {
		class := &classes.Items[i]
		key := client.ObjectKey{Namespace: class.Labels[stackIngressStackNamespaceLabel], Name: class.Labels[stackIngressStackNameLabel]}
		if key.Namespace == "" || key.Name == "" {
			continue
		}
		err := r.Get(ctx, key, &oramav1alpha1.OramaCloudV1{})
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Delete(ctx, class); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
