// SPDX-License-Identifier: MIT

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

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeEnvironmentFinalizer               = "orama.cloud/lake-environment-cleanup"
	deploymentIDAnnotation                 = "orama.cloud/deployment-id"
	environmentOwnerAnnotation             = "orama.cloud/lake-environment"
	environmentAdmissionLabel              = "liquid.oramasearch.com/environment"
	environmentSealedNamesAnnotation       = "liquid.oramasearch.com/sealed-secret-names"
	environmentIdentityLabel               = "orama.cloud/lake-environment-id"
	environmentManagedByLabel              = "app.kubernetes.io/managed-by"
	environmentManagedByValue              = "orama-operator"
	environmentProfileName                 = "tenant-defaults"
	environmentDefaultDenyName             = "default-deny"
	environmentNetworkAccessName           = "environment-access"
	environmentServicePort           int32 = 80
	environmentStatusRequeue               = 15 * time.Second
	environmentDriftRequeue                = 5 * time.Minute
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
// +kubebuilder:rbac:groups=orama.cloud,resources=lakesealedsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;secrets;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
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
			message := fmt.Sprintf("Lake %q does not exist", environment.Spec.LakeRef)
			return r.finishEnvironmentStatus(ctx, &environment, nil, "", pendingDomainStatuses(&environment, message), oramav1alpha1.LakeEnvironmentPhaseDegraded, "LakeNotFound", message)
		}
		return ctrl.Result{}, err
	}
	lakeNamespace := lake.Spec.Namespace
	if lakeNamespace == "" {
		lakeNamespace = "lake-dev"
	}
	if environment.Namespace != lakeNamespace {
		message := fmt.Sprintf("LakeEnvironment must be in Lake namespace %q", lakeNamespace)
		return r.finishEnvironmentStatus(ctx, &environment, nil, "", pendingDomainStatuses(&environment, message), oramav1alpha1.LakeEnvironmentPhaseDegraded, "WrongNamespace", message)
	}

	workloadNamespace := lakeEnvironmentNamespace(&environment)
	if err := r.reconcileEnvironmentNamespace(ctx, &environment, workloadNamespace); err != nil {
		return r.finishEnvironmentStatus(ctx, &environment, nil, "", pendingDomainStatuses(&environment, err.Error()), oramav1alpha1.LakeEnvironmentPhaseDegraded, "NamespaceConflict", err.Error())
	}
	if err := r.reconcileEnvironmentProfile(ctx, &environment, workloadNamespace); err != nil {
		return ctrl.Result{}, err
	}

	host := lakeEnvironmentHost(&lake, &environment)
	secretNames, reason, message, err := r.reconcileEnvironmentSecrets(ctx, &environment, workloadNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		if err := r.deleteStaleEnvironmentSecrets(ctx, &environment, workloadNamespace, nil); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.teardownEnvironmentWorkload(ctx, &environment, workloadNamespace); err != nil {
			return ctrl.Result{}, err
		}
		return r.finishEnvironmentStatus(ctx, &environment, nil, host, pendingDomainStatuses(&environment, message), oramav1alpha1.LakeEnvironmentPhaseDegraded, reason, message)
	}
	if err := r.reconcileEnvironmentWorkload(ctx, &environment, workloadNamespace, secretNames); err != nil {
		return ctrl.Result{}, err
	}
	domains, err := r.reconcileEnvironmentIngress(ctx, &lake, &environment, workloadNamespace, host)
	if err != nil {
		return ctrl.Result{}, err
	}

	var deployment appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: workloadNamespace, Name: environmentWorkloadName(&environment)}, &deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.finishEnvironmentStatus(ctx, &environment, nil, host, domains, oramav1alpha1.LakeEnvironmentPhaseProgressing, "DeploymentPending", "waiting for the environment Deployment")
	}
	phase, reason, message := environmentDeploymentState(&environment, &deployment)
	return r.finishEnvironmentStatus(ctx, &environment, &deployment, host, domains, phase, reason, message)
}

func (r *LakeEnvironmentReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func lakeEnvironmentNamespace(environment *oramav1alpha1.LakeEnvironment) string {
	id := strings.Join([]string{environment.Spec.LakeRef, environment.Spec.Repository, environment.Spec.Environment}, "\n")
	stableName := lakeEnvironmentResourceName(environment.Spec.LakeRef, environment.Spec.Repository, environment.Spec.Environment)
	if environment.Name != stableName && environment.Annotations[deploymentIDAnnotation] != "" {
		id = environment.Annotations[deploymentIDAnnotation]
	} else if environment.Spec.LakeRef == "" || environment.Spec.Repository == "" || environment.Spec.Environment == "" {
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
	labels[environmentAdmissionLabel] = "true"
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
			}, zettaTunnelIngressPeer()},
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

func (r *LakeEnvironmentReconciler) reconcileEnvironmentWorkload(
	ctx context.Context,
	environment *oramav1alpha1.LakeEnvironment,
	namespace string,
	secretNames []string,
) error {
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
	envFrom := make([]corev1.EnvFromSource, 0, len(secretNames))
	for _, secretName := range secretNames {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			},
		})
	}

	deployment := &appsv1.Deployment{ObjectMeta: environmentObjectMeta(environment, name, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = labels
		deployment.Annotations = withOwnedAnnotation(deployment.Annotations, environmentOwnerAnnotation, environmentOwner(environment))
		deployment.Spec.Replicas = &replicas
		deployment.Spec.RevisionHistoryLimit = &revisionHistoryLimit
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deployment.Spec.Template.ObjectMeta.Labels = labels
		deployment.Spec.Template.ObjectMeta.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		if len(secretNames) > 0 {
			deployment.Spec.Template.Annotations[environmentSealedNamesAnnotation] = strings.Join(secretNames, ",")
		}
		deployment.Spec.Template.Spec.RuntimeClassName = strPtr("untrusted")
		deployment.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:            "application",
			Image:           environment.Spec.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Env:             env,
			EnvFrom:         envFrom,
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

func sealedSecretReferenceName(reference string) (string, error) {
	name := reference
	if strings.HasPrefix(reference, "secret://") {
		name = strings.TrimPrefix(reference, "secret://")
	}
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("sealed-secret reference %q must be a resource name or secret://<name>", reference)
	}
	return name, nil
}

func (r *LakeEnvironmentReconciler) reconcileEnvironmentSecrets(
	ctx context.Context,
	environment *oramav1alpha1.LakeEnvironment,
	workloadNamespace string,
) ([]string, string, string, error) {
	keep := make(map[string]struct{}, len(environment.Spec.SecretRefs))
	secretNames := make([]string, 0, len(environment.Spec.SecretRefs))
	for _, reference := range environment.Spec.SecretRefs {
		resourceName, err := sealedSecretReferenceName(reference)
		if err != nil {
			return nil, "SecretReferenceInvalid", err.Error(), nil
		}
		var sealed oramav1alpha1.LakeSealedSecret
		key := client.ObjectKey{Namespace: environment.Namespace, Name: resourceName}
		if err := r.Get(ctx, key, &sealed); err != nil {
			if apierrors.IsNotFound(err) {
				message := fmt.Sprintf("sealed-secret reference %q was not found", reference)
				return nil, "SecretReferenceNotFound", message, nil
			}
			return nil, "", "", err
		}
		if sealed.Spec.Source.Type == "delivery" {
			return nil, "SecretReferenceConflict", "environment references must name an original sealed secret", nil
		}
		if sealed.Spec.LakeRef.Name != environment.Spec.LakeRef {
			message := fmt.Sprintf("sealed-secret reference %q belongs to Lake %q", reference, sealed.Spec.LakeRef.Name)
			return nil, "SecretReferenceWrongLake", message, nil
		}
		ready := apimeta.FindStatusCondition(sealed.Status.Conditions, "Ready")
		if sealed.Status.Phase != "Ready" || sealed.Status.ObservedGeneration != sealed.Generation || ready == nil || ready.Status != metav1.ConditionTrue {
			message := fmt.Sprintf("sealed-secret reference %q is not ready", reference)
			return nil, "SecretReferencePending", message, nil
		}
		var source corev1.Secret
		sourceKey := client.ObjectKey{Namespace: sealed.Namespace, Name: sealed.Spec.Template.Name}
		if err := r.Get(ctx, sourceKey, &source); err != nil {
			if apierrors.IsNotFound(err) {
				message := fmt.Sprintf("sealed-secret reference %q has no delivered Secret", reference)
				return nil, "SecretReferencePending", message, nil
			}
			return nil, "", "", err
		}
		if !metav1.IsControlledBy(&source, &sealed) {
			message := fmt.Sprintf("sealed-secret reference %q does not own its delivered Secret", reference)
			return nil, "SecretReferenceConflict", message, nil
		}
		if _, duplicate := keep[source.Name]; duplicate {
			message := fmt.Sprintf("sealed-secret references produce duplicate Secret %q", source.Name)
			return nil, "SecretReferenceConflict", message, nil
		}
		keep[source.Name] = struct{}{}
		secretNames = append(secretNames, source.Name)
		if err := r.copyEnvironmentSecret(ctx, environment, workloadNamespace, &sealed, &source); err != nil {
			return nil, "", "", err
		}
	}
	if err := r.deleteStaleEnvironmentSecrets(ctx, environment, workloadNamespace, keep); err != nil {
		return nil, "", "", err
	}
	return secretNames, "", "", nil
}

func (r *LakeEnvironmentReconciler) copyEnvironmentSecret(
	ctx context.Context,
	environment *oramav1alpha1.LakeEnvironment,
	workloadNamespace string,
	sealed *oramav1alpha1.LakeSealedSecret,
	source *corev1.Secret,
) error {
	owner, err := r.reconcileEnvironmentDeliveryOwner(ctx, environment, workloadNamespace, sealed)
	if err != nil {
		return err
	}
	destination := &corev1.Secret{ObjectMeta: environmentObjectMeta(environment, source.Name, workloadNamespace)}
	operation, err := controllerutil.CreateOrUpdate(ctx, r.Client, destination, func() error {
		if destination.ResourceVersion != "" && destination.Annotations[environmentOwnerAnnotation] != environmentOwner(environment) {
			return fmt.Errorf("Secret %q is not owned by this environment", destination.Name)
		}
		if err := controllerutil.SetControllerReference(owner, destination, r.Scheme); err != nil {
			return err
		}
		destination.Labels = environmentLabels(environment)
		destination.Labels[sealedSecretLabel] = "true"
		destination.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		destination.Type = source.Type
		destination.Immutable = nil
		destination.Data = make(map[string][]byte, len(source.Data))
		for key, value := range source.Data {
			destination.Data[key] = append([]byte(nil), value...)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("copy delivered Secret %q: %w", source.Name, err)
	}
	statusWriter := &LakeSealedSecretReconciler{Client: r.Client, Scheme: r.Scheme}
	return statusWriter.ready(ctx, owner, sealed.Status.KID, sealed.Status.Lease, nil, operation != controllerutil.OperationResultNone, time.Now().UTC())
}

func (r *LakeEnvironmentReconciler) deleteStaleEnvironmentSecrets(
	ctx context.Context,
	environment *oramav1alpha1.LakeEnvironment,
	workloadNamespace string,
	keep map[string]struct{},
) error {
	if err := r.deleteStaleEnvironmentDeliveryOwners(ctx, environment, workloadNamespace, keep); err != nil {
		return err
	}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(workloadNamespace), client.MatchingLabels(environmentLabels(environment))); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.Annotations[environmentOwnerAnnotation] != environmentOwner(environment) {
			continue
		}
		if _, found := keep[secret.Name]; found {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
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

// reconcileEnvironmentIngress serves the environment's host and its custom domains (spec.domains,
// lake's orama_control.domains) from one Ingress, and keeps one Cloudflare DNS record per named
// host and per domain when the Lake has a dnsSecretRef. Every record is owned by the environment
// and labelled with it, so removing a domain or the environment removes its record. It returns
// each domain's status.
func (r *LakeEnvironmentReconciler) reconcileEnvironmentIngress(ctx context.Context, lake *oramav1alpha1.Lake, environment *oramav1alpha1.LakeEnvironment, namespace, host string) ([]oramav1alpha1.LakeEnvironmentDomainStatus, error) {
	name := environmentWorkloadName(environment)
	if !lake.Spec.IngressEnabled {
		if err := r.deleteEnvironmentDNSRecords(ctx, environment, nil); err != nil {
			return nil, err
		}
		if err := deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}); err != nil {
			return nil, err
		}
		if err := r.deleteEnvironmentTLS(ctx, lake, environment, namespace); err != nil {
			return nil, err
		}
		return failedDomainStatuses(environment, fmt.Sprintf("Lake %q does not enable ingress (spec.ingressEnabled)", lake.Name)), nil
	}

	hosts := make([]string, 0, 1+len(environment.Spec.Domains))
	if host != "" {
		hosts = append(hosts, host)
	}
	for _, domain := range environment.Spec.Domains {
		if domain.Host != host {
			hosts = append(hosts, domain.Host)
		}
	}
	// The DNS records this environment should have: its named host, then each domain.
	dnsHosts := make([]string, 0, len(hosts))
	if environment.Spec.Kind == oramav1alpha1.LakeEnvironmentKindNamed && host != "" {
		dnsHosts = append(dnsHosts, host)
	}
	for _, domain := range environment.Spec.Domains {
		if domain.Host != host {
			dnsHosts = append(dnsHosts, domain.Host)
		}
	}
	manageDNS := lake.Spec.DNSSecretRef != nil && lake.Spec.DNSSecretRef.Name != ""
	keep := make(map[string]struct{}, len(dnsHosts))
	if manageDNS {
		for _, dnsHost := range dnsHosts {
			keep[environmentDNSRecordName(environment, dnsHost)] = struct{}{}
		}
	}
	if err := r.deleteEnvironmentDNSRecords(ctx, environment, keep); err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		if err := r.deleteEnvironmentTLS(ctx, lake, environment, namespace); err != nil {
			return nil, err
		}
		return nil, deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
	}

	// Origin TLS (operator#578): every host's certificate, issued in the Lake namespace and copied
	// here, and the zone's redirect and client-certificate settings.
	// The Lake's Issuer solves DNS-01 in its credential's zone and HTTP-01 for any other host
	// (operator#632, docs/lake/origin-tls.md). A preview host no wildcard covers is not issued.
	previewHost := ""
	if environment.Spec.Kind == oramav1alpha1.LakeEnvironmentKindPreview {
		previewHost = host
	}
	tlsHosts := lakeIssuableHosts(lake, hosts, previewHost)
	sources, err := reconcileLakeHostCertificates(ctx, r.Client, r.Scheme, lake, environment, environmentTLSOwner(environment), tlsHosts)
	if err != nil {
		return nil, fmt.Errorf("environment certificates: %w", err)
	}
	tls, err := copyLakeTLSSecrets(ctx, r.Client, lake, namespace, name, tlsHosts, sources, func(secret *corev1.Secret) {
		secret.Labels = environmentLabels(environment)
		secret.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
	})
	if err != nil {
		return nil, err
	}
	edge, err := readLakeEdge(ctx, r.Client, lake)
	if err != nil {
		return nil, err
	}
	caRef, err := reconcileEdgeClientCA(ctx, r.Client, namespace, name+"-edge-client-ca", edge, func(secret *corev1.Secret) error {
		secret.Labels = environmentLabels(environment)
		secret.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	prefix := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{ObjectMeta: environmentObjectMeta(environment, name, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		ingress.Labels = environmentLabels(environment)
		ingress.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		ingress.Spec.IngressClassName = strPtr(ingressClassOrDefault(lake.Spec.IngressClassName))
		rules := make([]networkingv1.IngressRule, 0, len(hosts))
		for _, ruleHost := range hosts {
			rules = append(rules, networkingv1.IngressRule{
				Host: ruleHost,
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
			})
		}
		ingress.Spec.Rules = rules
		ingress.Spec.TLS = tls
		applyLakeEdgeAnnotations(ingress, edge, caRef)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reconcile environment Ingress: %w", err)
	}

	target := ingressLoadBalancerTarget(ingress)
	records := make(map[string]*oramav1alpha1.CloudflareDNSRecord, len(dnsHosts))
	if manageDNS && target != "" {
		for _, dnsHost := range dnsHosts {
			dns := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: environmentDNSRecordName(environment, dnsHost), Namespace: environment.Namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dns, func() error {
				dns.Labels = environmentLabels(environment)
				dns.Spec.SecretRef = lake.Spec.DNSSecretRef.Name
				dns.Spec.Name = dnsHost
				dns.Spec.Content = target
				dns.Spec.Proxied = true
				return controllerutil.SetControllerReference(environment, dns, r.Scheme)
			}); err != nil {
				return nil, fmt.Errorf("reconcile environment DNS for %q: %w", dnsHost, err)
			}
			records[dnsHost] = dns
		}
	}

	statuses := make([]oramav1alpha1.LakeEnvironmentDomainStatus, 0, len(environment.Spec.Domains))
	for _, domain := range environment.Spec.Domains {
		status := oramav1alpha1.LakeEnvironmentDomainStatus{Host: domain.Host, Status: oramav1alpha1.LakeEnvironmentDomainPending}
		record := records[domain.Host]
		switch {
		case domain.Host == host:
			status.Status = oramav1alpha1.LakeEnvironmentDomainActive
			status.Message = "the environment's own host"
		case target == "":
			status.Message = "waiting for the ingress address"
		case !manageDNS:
			status.Message = fmt.Sprintf("Lake %q has no dnsSecretRef, so its DNS is not managed: point %s at %s", lake.Name, domain.Host, target)
		case record != nil && record.Status.Ready:
			status.Status = oramav1alpha1.LakeEnvironmentDomainActive
		case record != nil && record.Status.Message != "":
			status.Message = "DNS record: " + record.Status.Message
		default:
			status.Message = "waiting for the DNS record"
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// pendingDomainStatuses reports every domain as pending, for a reconcile that stopped before the
// Ingress.
func pendingDomainStatuses(environment *oramav1alpha1.LakeEnvironment, message string) []oramav1alpha1.LakeEnvironmentDomainStatus {
	return domainStatuses(environment, oramav1alpha1.LakeEnvironmentDomainPending, message)
}

func failedDomainStatuses(environment *oramav1alpha1.LakeEnvironment, message string) []oramav1alpha1.LakeEnvironmentDomainStatus {
	return domainStatuses(environment, oramav1alpha1.LakeEnvironmentDomainFailed, message)
}

func domainStatuses(environment *oramav1alpha1.LakeEnvironment, status, message string) []oramav1alpha1.LakeEnvironmentDomainStatus {
	if len(environment.Spec.Domains) == 0 {
		return nil
	}
	statuses := make([]oramav1alpha1.LakeEnvironmentDomainStatus, 0, len(environment.Spec.Domains))
	for _, domain := range environment.Spec.Domains {
		statuses = append(statuses, oramav1alpha1.LakeEnvironmentDomainStatus{Host: domain.Host, Status: status, Message: message})
	}
	return statuses
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

// deleteEnvironmentDNSRecords deletes the environment's DNS records not named in keep (nil: all).
func (r *LakeEnvironmentReconciler) deleteEnvironmentDNSRecords(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, keep map[string]struct{}) error {
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := r.List(ctx, &records, client.InNamespace(environment.Namespace), client.MatchingLabels{
		environmentIdentityLabel: environmentLabels(environment)[environmentIdentityLabel],
	}); err != nil {
		return err
	}
	for i := range records.Items {
		if _, found := keep[records.Items[i].Name]; found {
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
	if deploymentRolledOut(deployment, desired) {
		return oramav1alpha1.LakeEnvironmentPhaseReady, "DeploymentReady", fmt.Sprintf("all %d replicas ready", desired)
	}
	return oramav1alpha1.LakeEnvironmentPhaseProgressing, "DeploymentProgressing", fmt.Sprintf("%d/%d replicas ready", deployment.Status.ReadyReplicas, desired)
}

func (r *LakeEnvironmentReconciler) finishEnvironmentStatus(ctx context.Context, environment *oramav1alpha1.LakeEnvironment, deployment *appsv1.Deployment, host string, domains []oramav1alpha1.LakeEnvironmentDomainStatus, phase, reason, message string) (ctrl.Result, error) {
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
	desired.Domains = domains
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
	if phase == oramav1alpha1.LakeEnvironmentPhaseReady && domainsSettled(domains) {
		return ctrl.Result{RequeueAfter: environmentDriftRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: environmentStatusRequeue}, nil
}

// domainsSettled is whether no domain is still pending, so a Ready environment may slow its rechecks.
func domainsSettled(domains []oramav1alpha1.LakeEnvironmentDomainStatus) bool {
	for _, domain := range domains {
		if domain.Status == oramav1alpha1.LakeEnvironmentDomainPending {
			return false
		}
	}
	return true
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
	if err := deleteLakeTLSCopies(ctx, r.Client, namespace, name, nil); err != nil {
		return err
	}
	if err := deleteIfExists(ctx, r.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-edge-client-ca", Namespace: namespace}}); err != nil {
		return err
	}
	return r.deleteEnvironmentDNSRecords(ctx, environment, nil)
}

func environmentTLSOwner(environment *oramav1alpha1.LakeEnvironment) string {
	return lakeTLSOwnerValue("LakeEnvironment", environment.Namespace+"/"+environment.Name)
}

// deleteEnvironmentTLS removes the environment's Certificates, their copies and the edge CA Secret.
func (r *LakeEnvironmentReconciler) deleteEnvironmentTLS(ctx context.Context, lake *oramav1alpha1.Lake, environment *oramav1alpha1.LakeEnvironment, namespace string) error {
	name := environmentWorkloadName(environment)
	if _, err := reconcileLakeHostCertificates(ctx, r.Client, r.Scheme, lake, environment, environmentTLSOwner(environment), nil); err != nil {
		return err
	}
	if err := deleteLakeTLSCopies(ctx, r.Client, namespace, name, nil); err != nil {
		return err
	}
	return deleteIfExists(ctx, r.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-edge-client-ca", Namespace: namespace}})
}

// requestsForEnvironmentEdge re-reconciles the environments in a Lake namespace when one of its
// certificates is issued or renewed, or its zone policy changes.
func (r *LakeEnvironmentReconciler) requestsForEnvironmentEdge(ctx context.Context, object client.Object) []reconcile.Request {
	if _, ok := object.(*corev1.Secret); ok && !isLakeCertificateSecret(object) {
		return nil
	}
	var environments oramav1alpha1.LakeEnvironmentList
	if err := r.List(ctx, &environments, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(environments.Items))
	for i := range environments.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&environments.Items[i])})
	}
	return requests
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

func (r *LakeEnvironmentReconciler) requestsForSealedSecret(ctx context.Context, object client.Object) []reconcile.Request {
	sealed, ok := object.(*oramav1alpha1.LakeSealedSecret)
	if !ok {
		return nil
	}
	if sealed.Spec.Source.Type == "delivery" {
		return r.requestsForEnvironmentObject(ctx, object)
	}
	var environments oramav1alpha1.LakeEnvironmentList
	if err := r.List(ctx, &environments, client.InNamespace(sealed.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range environments.Items {
		environment := &environments.Items[i]
		for _, reference := range environment.Spec.SecretRefs {
			name, err := sealedSecretReferenceName(reference)
			if err == nil && name == sealed.Name {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(environment)})
				break
			}
		}
	}
	return requests
}

// SetupWithManager sets up watches for the CR and cross-namespace child status changes.
func (r *LakeEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapObject := handler.EnqueueRequestsFromMapFunc(r.requestsForEnvironmentObject)
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.LakeEnvironment{}).
		Watches(&oramav1alpha1.Lake{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLake)).
		Watches(&oramav1alpha1.LakeSealedSecret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSealedSecret)).
		// A domain is active once its DNS record is Ready, so a record's status change reports it.
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Watches(&appsv1.Deployment{}, mapObject).
		Watches(&corev1.Service{}, mapObject).
		Watches(&corev1.Secret{}, mapObject).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForEnvironmentEdge)).
		Watches(&oramav1alpha1.EdgeZonePolicy{}, handler.EnqueueRequestsFromMapFunc(r.requestsForEnvironmentEdge)).
		Watches(&networkingv1.Ingress{}, mapObject).
		Watches(&networkingv1.NetworkPolicy{}, mapObject).
		Watches(&corev1.ResourceQuota{}, mapObject).
		Watches(&corev1.LimitRange{}, mapObject).
		Named("lake-environment").
		Complete(r)
}
