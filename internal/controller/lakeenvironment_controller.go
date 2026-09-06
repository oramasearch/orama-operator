/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const (
	lakeEnvironmentFinalizer           = "orama.cloud/lake-environment-cleanup"
	deploymentIDAnnotation             = "orama.cloud/deployment-id"
	environmentOwnerAnnotation         = "orama.cloud/lake-environment"
	environmentIdentityLabel           = "orama.cloud/lake-environment-id"
	environmentManagedByLabel          = "app.kubernetes.io/managed-by"
	environmentManagedByValue          = "orama-operator"
	environmentProfileName             = "tenant-defaults"
	environmentDefaultDenyName         = "default-deny"
	environmentNetworkAccessName       = "environment-access"
	environmentServicePort       int32 = 80
	environmentStatusRequeue           = 15 * time.Second
	environmentDriftRequeue            = 5 * time.Minute
)

// LakeEnvironmentReconciler materializes a LakeEnvironment into an isolated k0s tenant profile.
// The namespaced CR cannot owner-reference its cluster-scoped Namespace or cross-namespace workload,
// so the controller labels every object and uses a finalizer for verified, explicit teardown.
type LakeEnvironmentReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakeenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakeenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakeenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *LakeEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var environment oramav1alpha1.LakeEnvironment
	if err := r.Get(ctx, req.NamespacedName, &environment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !environment.DeletionTimestamp.IsZero() {
		return r.finalizeLakeEnvironment(ctx, &environment)
	}
	if !controllerutil.ContainsFinalizer(&environment, lakeEnvironmentFinalizer) {
		controllerutil.AddFinalizer(&environment, lakeEnvironmentFinalizer)
		if err := r.Update(ctx, &environment); err != nil {
			return ctrl.Result{}, err
		}
	}

	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: environment.Spec.LakeRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finishEnvironmentStatus(ctx, &environment, nil, "", oramav1alpha1.LakeEnvironmentPhaseDegraded, "LakeNotFound", fmt.Sprintf("Lake %q does not exist", environment.Spec.LakeRef))
		}
		return ctrl.Result{}, err
	}
	lakeNamespace := lake.Spec.Namespace
	if lakeNamespace == "" {
		lakeNamespace = "lake-dev"
	}
	if environment.Namespace != lakeNamespace {
		return r.finishEnvironmentStatus(ctx, &environment, nil, "", oramav1alpha1.LakeEnvironmentPhaseDegraded, "WrongNamespace", fmt.Sprintf("LakeEnvironment must be in Lake namespace %q", lakeNamespace))
	}

	workloadNamespace := lakeEnvironmentNamespace(&environment)
	if err := r.reconcileEnvironmentNamespace(ctx, &environment, workloadNamespace); err != nil {
		return r.finishEnvironmentStatus(ctx, &environment, nil, "", oramav1alpha1.LakeEnvironmentPhaseDegraded, "NamespaceConflict", err.Error())
	}
	if err := r.reconcileEnvironmentProfile(ctx, &environment, workloadNamespace); err != nil {
		return ctrl.Result{}, err
	}

	host := lakeEnvironmentHost(&lake, &environment)
	if len(environment.Spec.SecretRefs) > 0 {
		if err := r.teardownEnvironmentWorkload(ctx, &environment, workloadNamespace); err != nil {
			return ctrl.Result{}, err
		}
		return r.finishEnvironmentStatus(ctx, &environment, nil, host, oramav1alpha1.LakeEnvironmentPhaseDegraded, "SecretDeliveryUnavailable", "secretRefs require LakeSealedSecret support from operator #129")
	}
	if err := r.reconcileEnvironmentWorkload(ctx, &environment, workloadNamespace); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileEnvironmentIngress(ctx, &lake, &environment, workloadNamespace, host); err != nil {
		return ctrl.Result{}, err
	}

	var deployment appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: workloadNamespace, Name: environmentWorkloadName(&environment)}, &deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.finishEnvironmentStatus(ctx, &environment, nil, host, oramav1alpha1.LakeEnvironmentPhaseProgressing, "DeploymentPending", "waiting for the environment Deployment")
	}
	phase, reason, message := environmentDeploymentState(&environment, &deployment)
	return r.finishEnvironmentStatus(ctx, &environment, &deployment, host, phase, reason, message)
}

func (r *LakeEnvironmentReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func lakeEnvironmentNamespace(environment *oramav1alpha1.LakeEnvironment) string {
	id := environment.Annotations[deploymentIDAnnotation]
	if id == "" {
		id = environment.Namespace + "/" + environment.Name
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:10]
	lakeName := dnsLabel(environment.Spec.LakeRef)
	maxLakeLength := 63 - len("lake--env-") - len(hash)
	if len(lakeName) > maxLakeLength {
		lakeName = strings.Trim(lakeName[:maxLakeLength], "-")
	}
	return fmt.Sprintf("lake-%s-env-%s", lakeName, hash)
}

func dnsLabel(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
			out.WriteRune(char)
		} else {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func environmentOwner(environment *oramav1alpha1.LakeEnvironment) string {
	return environment.Namespace + "/" + environment.Name
}

func environmentLabels(environment *oramav1alpha1.LakeEnvironment) map[string]string {
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(environmentOwner(environment))))[:16]
	return map[string]string{
		"app.kubernetes.io/name":     "lake-environment",
		"app.kubernetes.io/instance": identity,
		environmentIdentityLabel:     identity,
		environmentManagedByLabel:    environmentManagedByValue,
	}
}

func environmentObjectMeta(environment *oramav1alpha1.LakeEnvironment, name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Labels:    environmentLabels(environment),
		Annotations: map[string]string{
			environmentOwnerAnnotation: environmentOwner(environment),
		},
	}
}

func environmentNamespaceLabels(environment *oramav1alpha1.LakeEnvironment) map[string]string {
	labels := environmentLabels(environment)
	labels["pod-security.kubernetes.io/audit"] = "restricted"
	labels["pod-security.kubernetes.io/audit-version"] = "latest"
	labels["pod-security.kubernetes.io/enforce"] = "restricted"
	labels["pod-security.kubernetes.io/enforce-version"] = "latest"
	labels["pod-security.kubernetes.io/warn"] = "restricted"
	labels["pod-security.kubernetes.io/warn-version"] = "latest"
	return labels
}

func (r *LakeEnvironmentReconciler) reconcileEnvironmentNamespace(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, name string) error {
	desired := &corev1.Namespace{ObjectMeta: environmentObjectMeta(environment, name, "")}
	desired.Labels = environmentNamespaceLabels(environment)
	if err := r.Create(ctx, desired); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create environment namespace %q: %w", name, err)
	}

	var existing corev1.Namespace
	if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
		return fmt.Errorf("read existing environment namespace %q: %w", name, err)
	}
	if existing.Annotations[environmentOwnerAnnotation] != environmentOwner(environment) {
		return fmt.Errorf("namespace %q is not owned by LakeEnvironment %s", name, environmentOwner(environment))
	}
	changed := false
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for key, value := range environmentNamespaceLabels(environment) {
		if existing.Labels[key] != value {
			existing.Labels[key] = value
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Update(ctx, &existing)
}

func (r *LakeEnvironmentReconciler) reconcileEnvironmentProfile(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, namespace string) error {
	quota := &corev1.ResourceQuota{ObjectMeta: environmentObjectMeta(environment, environmentProfileName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Labels = environmentLabels(environment)
		quota.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		quota.Spec.Hard = corev1.ResourceList{
			corev1.ResourceLimitsCPU:              resource.MustParse("8"),
			corev1.ResourceLimitsMemory:           resource.MustParse("16Gi"),
			corev1.ResourcePersistentVolumeClaims: resource.MustParse("20"),
			corev1.ResourcePods:                   resource.MustParse("50"),
			corev1.ResourceRequestsCPU:            resource.MustParse("4"),
			corev1.ResourceRequestsMemory:         resource.MustParse("8Gi"),
			corev1.ResourceServices:               resource.MustParse("20"),
		}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile ResourceQuota: %w", err)
	}

	limitRange := &corev1.LimitRange{ObjectMeta: environmentObjectMeta(environment, environmentProfileName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, limitRange, func() error {
		limitRange.Labels = environmentLabels(environment)
		limitRange.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		limitRange.Spec.Limits = []corev1.LimitRangeItem{{
			Type: corev1.LimitTypeContainer,
			Default: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			DefaultRequest: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile LimitRange: %w", err)
	}

	policy := &networkingv1.NetworkPolicy{ObjectMeta: environmentObjectMeta(environment, environmentDefaultDenyName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = environmentLabels(environment)
		policy.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		policy.Spec.PodSelector = metav1.LabelSelector{}
		policy.Spec.PolicyTypes = []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		}
		policy.Spec.Ingress = nil
		policy.Spec.Egress = nil
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile NetworkPolicy: %w", err)
	}
	access := &networkingv1.NetworkPolicy{ObjectMeta: environmentObjectMeta(environment, environmentNetworkAccessName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, access, func() error {
		access.Labels = environmentLabels(environment)
		access.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		access.Spec.PodSelector = metav1.LabelSelector{MatchLabels: environmentLabels(environment)}
		access.Spec.PolicyTypes = []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		}
		access.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": "ingress-nginx",
				}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{
				Protocol: protocolPtr(corev1.ProtocolTCP),
				Port:     intstrPtr(intstr.FromInt32(environment.Spec.Port)),
			}},
		}}
		access.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": "kube-system",
				}},
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"k8s-app": "kube-dns",
				}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: protocolPtr(corev1.ProtocolUDP), Port: intstrPtr(intstr.FromInt(53))},
				{Protocol: protocolPtr(corev1.ProtocolTCP), Port: intstrPtr(intstr.FromInt(53))},
			},
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile environment access NetworkPolicy: %w", err)
	}
	return nil
}

func protocolPtr(value corev1.Protocol) *corev1.Protocol {
	return &value
}

func intstrPtr(value intstr.IntOrString) *intstr.IntOrString {
	return &value
}

func environmentWorkloadName(environment *oramav1alpha1.LakeEnvironment) string {
	return "environment"
}

func environmentContainerResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

func environmentContainerSecurityContext() *corev1.SecurityContext {
	allowPrivilegeEscalation := false
	runAsNonRoot := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		RunAsNonRoot:             &runAsNonRoot,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func (r *LakeEnvironmentReconciler) reconcileEnvironmentWorkload(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, namespace string) error {
	labels := environmentLabels(environment)
	name := environmentWorkloadName(environment)
	replicas := environment.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	revisionHistoryLimit := int32(10)
	env := make([]corev1.EnvVar, 0, len(environment.Spec.Env))
	for _, variable := range environment.Spec.Env {
		env = append(env, corev1.EnvVar{Name: variable.Name, Value: variable.Value})
	}

	deployment := &appsv1.Deployment{ObjectMeta: environmentObjectMeta(environment, name, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = labels
		deployment.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		deployment.Spec.Replicas = &replicas
		deployment.Spec.RevisionHistoryLimit = &revisionHistoryLimit
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deployment.Spec.Template.ObjectMeta.Labels = labels
		deployment.Spec.Template.ObjectMeta.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		deployment.Spec.Template.Spec.RuntimeClassName = strPtr("untrusted")
		deployment.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:            "application",
			Image:           environment.Spec.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Env:             env,
			Ports: []corev1.ContainerPort{{
				Name:          "http",
				ContainerPort: environment.Spec.Port,
			}},
			Resources:       environmentContainerResources(),
			SecurityContext: environmentContainerSecurityContext(),
			StartupProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(environment.Spec.Port)}},
				PeriodSeconds:    5,
				FailureThreshold: 60,
				TimeoutSeconds:   2,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:   corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(environment.Spec.Port)}},
				PeriodSeconds:  10,
				TimeoutSeconds: 2,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(environment.Spec.Port)}},
				PeriodSeconds:    20,
				FailureThreshold: 3,
				TimeoutSeconds:   2,
			},
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile environment Deployment: %w", err)
	}

	service := &corev1.Service{ObjectMeta: environmentObjectMeta(environment, name, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		clusterIP := service.Spec.ClusterIP
		clusterIPs := service.Spec.ClusterIPs
		service.Labels = labels
		service.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		service.Spec.Selector = labels
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       environmentServicePort,
			TargetPort: intstr.FromInt32(environment.Spec.Port),
		}}
		service.Spec.ClusterIP = clusterIP
		service.Spec.ClusterIPs = clusterIPs
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile environment Service: %w", err)
	}
	return nil
}

func lakeEnvironmentHost(lake *oramav1alpha1.Lake, environment *oramav1alpha1.LakeEnvironment) string {
	if !lake.Spec.IngressEnabled {
		return ""
	}
	if environment.Spec.Kind == oramav1alpha1.LakeEnvironmentKindPreview {
		if lake.Spec.PreviewWildcard == "" {
			return ""
		}
		change := dnsLabel(environment.Spec.Change)
		if len(change) > 12 {
			change = strings.Trim(change[:12], "-")
		}
		if change == "" {
			return ""
		}
		return change + "." + strings.TrimPrefix(lake.Spec.PreviewWildcard, "*.")
	}
	return environment.Spec.Host
}

func (r *LakeEnvironmentReconciler) reconcileEnvironmentIngress(ctx context.Context, lake *oramav1alpha1.Lake, environment *oramav1alpha1.LakeEnvironment, namespace, host string) error {
	name := environmentWorkloadName(environment)
	dnsName := ""
	if environment.Spec.Kind == oramav1alpha1.LakeEnvironmentKindNamed && host != "" {
		dnsName = environmentDNSRecordName(environment, host)
	}
	if err := r.deleteEnvironmentDNSRecords(ctx, environment, dnsName); err != nil {
		return err
	}
	if host == "" {
		if err := deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}); err != nil {
			return err
		}
		return nil
	}

	prefix := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{ObjectMeta: environmentObjectMeta(environment, name, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		ingress.Labels = environmentLabels(environment)
		ingress.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		ingress.Spec.IngressClassName = strPtr("nginx")
		ingress.Spec.Rules = []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     "/",
					PathType: &prefix,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: name,
						Port: networkingv1.ServiceBackendPort{Number: environmentServicePort},
					}},
				}},
			}},
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile environment Ingress: %w", err)
	}

	if environment.Spec.Kind != oramav1alpha1.LakeEnvironmentKindNamed || lake.Spec.DNSSecretRef == nil || lake.Spec.DNSSecretRef.Name == "" {
		return r.deleteEnvironmentDNSRecords(ctx, environment, "")
	}
	target := ingressLoadBalancerTarget(ingress)
	if target == "" {
		return nil
	}
	dns := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: dnsName, Namespace: environment.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dns, func() error {
		dns.Labels = environmentLabels(environment)
		dns.Spec.SecretRef = lake.Spec.DNSSecretRef.Name
		dns.Spec.Name = host
		dns.Spec.Content = target
		dns.Spec.Proxied = true
		return controllerutil.SetControllerReference(environment, dns, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconcile named environment DNS: %w", err)
	}
	return nil
}

func environmentDNSRecordName(environment *oramav1alpha1.LakeEnvironment, host string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(host)))[:8]
	prefix := environment.Name
	maxPrefixLength := 253 - len("-dns-") - len(hash)
	if len(prefix) > maxPrefixLength {
		prefix = strings.Trim(prefix[:maxPrefixLength], "-")
	}
	return prefix + "-dns-" + hash
}

func (r *LakeEnvironmentReconciler) deleteEnvironmentDNSRecords(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, keepName string) error {
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := r.List(ctx, &records, client.InNamespace(environment.Namespace), client.MatchingLabels{
		environmentIdentityLabel: environmentLabels(environment)[environmentIdentityLabel],
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

func ingressLoadBalancerTarget(ingress *networkingv1.Ingress) string {
	for _, address := range ingress.Status.LoadBalancer.Ingress {
		if address.Hostname != "" {
			return address.Hostname
		}
		if address.IP != "" {
			return address.IP
		}
	}
	return ""
}

func environmentDeploymentState(environment *oramav1alpha1.LakeEnvironment, deployment *appsv1.Deployment) (string, string, string) {
	desired := environment.Spec.Replicas
	if desired == 0 {
		desired = 1
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			return oramav1alpha1.LakeEnvironmentPhaseDegraded, "ReplicaFailure", condition.Message
		}
	}
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return oramav1alpha1.LakeEnvironmentPhaseProgressing, "DeploymentProgressing", "waiting for the Deployment controller to observe the latest change"
	}
	if deployment.Status.ReadyReplicas >= desired && deployment.Status.UpdatedReplicas >= desired && deployment.Status.AvailableReplicas >= desired {
		return oramav1alpha1.LakeEnvironmentPhaseReady, "DeploymentReady", fmt.Sprintf("all %d replicas ready", desired)
	}
	return oramav1alpha1.LakeEnvironmentPhaseProgressing, "DeploymentProgressing", fmt.Sprintf("%d/%d replicas ready", deployment.Status.ReadyReplicas, desired)
}

func (r *LakeEnvironmentReconciler) finishEnvironmentStatus(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, deployment *appsv1.Deployment, host, phase, reason, message string) (ctrl.Result, error) {
	desired := environment.Status.DeepCopy()
	desired.Phase = phase
	desired.URL = ""
	if host != "" {
		desired.URL = "https://" + host
	}
	desired.ReadyReplicas = 0
	if deployment != nil {
		desired.ReadyReplicas = deployment.Status.ReadyReplicas
	}
	desired.DeploymentID = environment.Annotations[deploymentIDAnnotation]
	conditionStatus := metav1.ConditionFalse
	if phase == oramav1alpha1.LakeEnvironmentPhaseReady {
		conditionStatus = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus,
		ObservedGeneration: environment.Generation,
		Reason:             reason,
		Message:            message,
	})
	if !apiequality.Semantic.DeepEqual(environment.Status, *desired) {
		environment.Status = *desired
		if err := r.Status().Update(ctx, environment); err != nil {
			return ctrl.Result{}, err
		}
	}
	if phase == oramav1alpha1.LakeEnvironmentPhaseReady {
		return ctrl.Result{RequeueAfter: environmentDriftRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: environmentStatusRequeue}, nil
}

func (r *LakeEnvironmentReconciler) teardownEnvironmentWorkload(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, namespace string) error {
	name := environmentWorkloadName(environment)
	objects := []client.Object{
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
	}
	for _, object := range objects {
		if err := deleteIfExists(ctx, r.Client, object); err != nil {
			return err
		}
	}
	return r.deleteEnvironmentDNSRecords(ctx, environment, "")
}

func (r *LakeEnvironmentReconciler) finalizeLakeEnvironment(ctx context.Context, environment *oramav1alpha1.LakeEnvironment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(environment, lakeEnvironmentFinalizer) {
		return ctrl.Result{}, nil
	}
	if environment.Status.Phase != oramav1alpha1.LakeEnvironmentPhaseTornDown {
		desired := environment.Status.DeepCopy()
		desired.Phase = oramav1alpha1.LakeEnvironmentPhaseTornDown
		desired.ReadyReplicas = 0
		apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: environment.Generation,
			Reason:             "TornDown",
			Message:            "environment resources removed",
		})
		environment.Status = *desired
		if err := r.Status().Update(ctx, environment); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.teardownEnvironmentWorkload(ctx, environment, lakeEnvironmentNamespace(environment)); err != nil {
		return ctrl.Result{}, err
	}

	var namespace corev1.Namespace
	name := lakeEnvironmentNamespace(environment)
	if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &namespace); err == nil {
		if namespace.Annotations[environmentOwnerAnnotation] != environmentOwner(environment) {
			return ctrl.Result{}, fmt.Errorf("refusing to delete namespace %q without ownership annotation %q", name, environmentOwner(environment))
		}
		if namespace.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &namespace); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(environment, lakeEnvironmentFinalizer)
	if err := r.Update(ctx, environment); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LakeEnvironmentReconciler) requestsForEnvironmentObject(_ context.Context, object client.Object) []reconcile.Request {
	owner := object.GetAnnotations()[environmentOwnerAnnotation]
	parts := strings.SplitN(owner, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: parts[0], Name: parts[1]}}}
}

func (r *LakeEnvironmentReconciler) requestsForLake(ctx context.Context, object client.Object) []reconcile.Request {
	lake, ok := object.(*oramav1alpha1.Lake)
	if !ok {
		return nil
	}
	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}
	var environments oramav1alpha1.LakeEnvironmentList
	if err := r.List(ctx, &environments, client.InNamespace(namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(environments.Items))
	for i := range environments.Items {
		if environments.Items[i].Spec.LakeRef != lake.Name {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&environments.Items[i])})
	}
	return requests
}

// SetupWithManager sets up watches for the CR and cross-namespace child status changes.
func (r *LakeEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapObject := handler.EnqueueRequestsFromMapFunc(r.requestsForEnvironmentObject)
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.LakeEnvironment{}).
		Watches(&oramav1alpha1.Lake{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLake)).
		Watches(&appsv1.Deployment{}, mapObject).
		Watches(&corev1.Service{}, mapObject).
		Watches(&networkingv1.Ingress{}, mapObject).
		Watches(&networkingv1.NetworkPolicy{}, mapObject).
		Watches(&corev1.ResourceQuota{}, mapObject).
		Watches(&corev1.LimitRange{}, mapObject).
		Named("lake-environment").
		Complete(r)
}
