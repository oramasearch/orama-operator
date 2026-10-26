/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeAppFinalizer                      = "orama.cloud/lake-app-cleanup"
	lakeAppOwnerAnnotation                = "orama.cloud/lake-app"
	lakeAppIdentityLabel                  = "orama.cloud/lake-app-id"
	lakeAppCredentialHashAnnotation       = "orama.cloud/lake-app-credential-hash"
	lakeAppProfileName                    = "tenant-defaults"
	lakeAppDefaultDenyName                = "default-deny"
	lakeAppNetworkAccessName              = "app-access"
	lakeAppWorkloadName                   = "app"
	lakeAppBindingsSecretName             = "app-bindings"
	lakeAppEventsSecretName               = "app-events"
	lakeAppSourceSecretName               = "app-source"
	lakeAppPullSecretName                 = "app-pull"
	lakeAppBindingsVolume                 = "bindings"
	lakeAppTempVolume                     = "tmp"
	lakeAppBindingsPath                   = "/var/run/orama/bindings"
	lakeAppRunAsUser                      = int64(10001)
	lakeAppServicePort              int32 = 80
	lakeAppStatusRequeue                  = 15 * time.Second
	lakeAppDriftRequeue                   = 5 * time.Minute
	lakeAppEventTimeout                   = 2 * time.Second
)

type lakeAppEvent struct {
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Revision string `json:"revision"`
	Message  string `json:"message"`
	URL      string `json:"url,omitempty"`
}

// LakeAppEventPublisher publishes one transition to an app's scoped topic.
type LakeAppEventPublisher func(ctx context.Context, baseURL, token, topic string, event lakeAppEvent) error

// LakeAppReconciler materializes one LakeApp into a restricted, dedicated namespace. Cross-
// namespace children cannot use owner references, so every object carries a stable ownership
// annotation and the finalizer verifies that annotation before deleting a standalone namespace.
type LakeAppReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	APIReader      client.Reader
	EventPublisher LakeAppEventPublisher
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakeapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakeapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakeapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;secrets;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *LakeAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var app oramav1alpha1.LakeApp
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !app.DeletionTimestamp.IsZero() {
		return r.finalizeLakeApp(ctx, &app)
	}
	if !controllerutil.ContainsFinalizer(&app, lakeAppFinalizer) {
		controllerutil.AddFinalizer(&app, lakeAppFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: app.Spec.LakeRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finishLakeAppStatus(ctx, &app, nil, "", oramav1alpha1.LakeAppPhaseDegraded, false, "LakeNotFound", fmt.Sprintf("Lake %q does not exist", app.Spec.LakeRef))
		}
		return ctrl.Result{}, err
	}
	lakeNamespace := lake.Spec.Namespace
	if lakeNamespace == "" {
		lakeNamespace = "lake-dev"
	}
	if app.Namespace != lakeNamespace {
		message := fmt.Sprintf("LakeApp must be in Lake namespace %q", lakeNamespace)
		return r.finishLakeAppStatus(ctx, &app, nil, "", oramav1alpha1.LakeAppPhaseDegraded, false, "WrongNamespace", message)
	}
	if reason, message := validateLakeApp(&lake, &app); reason != "" {
		return r.finishLakeAppStatus(ctx, &app, nil, lakeAppHost(&lake, &app), oramav1alpha1.LakeAppPhaseDegraded, false, reason, message)
	}

	workloadNamespace := lakeAppNamespace(&app)
	if err := r.reconcileLakeAppNamespace(ctx, &app, workloadNamespace); err != nil {
		return r.finishLakeAppStatus(ctx, &app, nil, "", oramav1alpha1.LakeAppPhaseDegraded, false, "NamespaceConflict", err.Error())
	}
	if err := r.reconcileLakeAppProfile(ctx, &lake, &app, workloadNamespace); err != nil {
		return ctrl.Result{}, err
	}

	secrets, reason, message, err := r.reconcileLakeAppSecrets(ctx, &lake, &app, workloadNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	host := lakeAppHost(&lake, &app)
	if reason != "" {
		if err := r.teardownLakeAppWorkload(ctx, &app, workloadNamespace); err != nil {
			return ctrl.Result{}, err
		}
		phase := oramav1alpha1.LakeAppPhaseDegraded
		if reason == "TokenRegistrationPending" || reason == "BindingPending" || reason == "SecretDeliveryPending" {
			phase = oramav1alpha1.LakeAppPhaseProgressing
		}
		return r.finishLakeAppStatus(ctx, &app, nil, host, phase, false, reason, message)
	}
	if err := r.reconcileLakeAppWorkload(ctx, &lake, &app, workloadNamespace, secrets); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileLakeAppIngress(ctx, &lake, &app, workloadNamespace, host); err != nil {
		return ctrl.Result{}, err
	}

	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: workloadNamespace, Name: lakeAppWorkloadObjectName(&app)}
	// The cache can still hold the previous ready Deployment immediately after an update.
	// Read through the API reader so that old readiness cannot acknowledge this app generation.
	if err := r.reader().Get(ctx, key, &deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.finishLakeAppStatus(ctx, &app, nil, host, oramav1alpha1.LakeAppPhaseProgressing, false, "DeploymentPending", "waiting for the app Deployment")
	}
	phase, ready, reason, message, err := r.lakeAppDeploymentState(ctx, &app, &deployment, workloadNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.finishLakeAppStatus(ctx, &app, &deployment, host, phase, ready, reason, message)
}

func (r *LakeAppReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func validateLakeApp(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) (string, string) {
	if app.Spec.Sync != nil && !metav1.IsControlledBy(app, lake) {
		return "SyncOwnerInvalid", "a synchronized app must be controlled by its Lake"
	}
	if len(app.Spec.Stores) > 0 && app.Spec.Sync == nil {
		return "StoresRequireSync", "only an app synchronized from lake may mount stores"
	}
	mode := app.Spec.Mode
	if mode == "" {
		mode = oramav1alpha1.LakeAppModeRuntime
	}
	switch mode {
	case oramav1alpha1.LakeAppModeRuntime:
		if app.Spec.Source == nil {
			return "SourceRequired", "runtime mode requires spec.source"
		}
		if app.Spec.Sync == nil && (app.Spec.Source.TokenSecretRef.Name == "" || app.Spec.Source.TokenSecretRef.Key == "") {
			return "SourceCredentialRequired", "runtime mode requires spec.source.tokenSecretRef name and key"
		}
	case oramav1alpha1.LakeAppModeImage:
		if app.Spec.Image == "" {
			return "ImageRequired", "image mode requires spec.image"
		}
	default:
		return "ModeInvalid", "spec.mode must be runtime or image"
	}
	if app.Spec.Bindings != nil {
		for _, name := range app.Spec.Bindings.ServiceNames {
			if !validAppServiceName(name) {
				return "ServiceNameInvalid", "service binding filename is invalid"
			}
		}
		hasBindings := app.Spec.Bindings.Zetta != nil || len(app.Spec.Bindings.Services) > 0
		if app.Spec.Sync == nil && hasBindings && (app.Spec.Bindings.SecretRef == nil || app.Spec.Bindings.SecretRef.Name == "") {
			return "BindingSecretRequired", "bindings.secretRef is required when data bindings are requested"
		}
	}
	return "", ""
}

func lakeAppNamespace(app *oramav1alpha1.LakeApp) string {
	if app.Spec.Sync != nil {
		return app.Spec.Sync.WorkspaceNamespace
	}
	id := app.Spec.LakeRef + "\n" + app.Namespace + "\n" + app.Name
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:10]
	lakeName := dnsLabel(app.Spec.LakeRef)
	maxLakeLength := 63 - len("lake--app-") - len(hash)
	if len(lakeName) > maxLakeLength {
		lakeName = strings.Trim(lakeName[:maxLakeLength], "-")
	}
	return fmt.Sprintf("lake-%s-app-%s", lakeName, hash)
}

func lakeAppManagedName(app *oramav1alpha1.LakeApp, legacyName, suffix string) string {
	if app.Spec.Sync == nil {
		return legacyName
	}
	hex := strings.ReplaceAll(app.Spec.Sync.ID, "-", "")
	if len(hex) < 12 {
		hex = fmt.Sprintf("%x", sha256.Sum256([]byte(app.Spec.Sync.ID)))
	}
	name := "app-" + hex[:12]
	if suffix != "" {
		name += "-" + suffix
	}
	return name
}

// lakeAppWorkloadObjectName also identifies tenant app locations to ingress-nginx's credential
// filter in helmfile.yaml and every platform.yaml. Keep its app/app-<12 hex> names in sync there.
func lakeAppWorkloadObjectName(app *oramav1alpha1.LakeApp) string {
	return lakeAppManagedName(app, lakeAppWorkloadName, "")
}

func lakeAppTokensName(app *oramav1alpha1.LakeApp) string {
	return lakeAppManagedName(app, lakeAppTokensSecretName, "tokens")
}

func lakeAppBindingsName(app *oramav1alpha1.LakeApp) string {
	return lakeAppManagedName(app, lakeAppBindingsSecretName, "bindings")
}

func lakeAppEventsName(app *oramav1alpha1.LakeApp) string {
	return lakeAppManagedName(app, lakeAppEventsSecretName, "events")
}

func lakeAppSourceName(app *oramav1alpha1.LakeApp) string {
	return lakeAppManagedName(app, lakeAppSourceSecretName, "source")
}

func lakeAppPullName(app *oramav1alpha1.LakeApp) string {
	return lakeAppManagedName(app, lakeAppPullSecretName, "pull")
}

func lakeAppAccessPolicyName(app *oramav1alpha1.LakeApp) string {
	return lakeAppManagedName(app, lakeAppNetworkAccessName, "access")
}

func lakeAppEnvSecretName(app *oramav1alpha1.LakeApp, sourceName string) string {
	if app.Spec.Sync == nil {
		return sourceName
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(sourceName)))[:8]
	return lakeAppManagedName(app, sourceName, "env-"+hash)
}

func lakeAppOwner(app *oramav1alpha1.LakeApp) string {
	return app.Namespace + "/" + app.Name
}

func lakeAppLabels(app *oramav1alpha1.LakeApp) map[string]string {
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(lakeAppOwner(app))))[:16]
	return map[string]string{
		"app.kubernetes.io/name":       "lake-app",
		"app.kubernetes.io/instance":   identity,
		"app.kubernetes.io/managed-by": "orama-operator",
		lakeAppIdentityLabel:           identity,
	}
}

func lakeAppObjectMeta(app *oramav1alpha1.LakeApp, name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      lakeAppLabels(app),
		Annotations: map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)},
	}
}

func lakeAppNamespaceLabels(app *oramav1alpha1.LakeApp) map[string]string {
	labels := lakeAppLabels(app)
	if app.Spec.Sync != nil {
		labels[workspaceLabel] = app.Spec.Sync.WorkspaceID
	}
	labels["pod-security.kubernetes.io/audit"] = "restricted"
	labels["pod-security.kubernetes.io/audit-version"] = "latest"
	labels["pod-security.kubernetes.io/enforce"] = "restricted"
	labels["pod-security.kubernetes.io/enforce-version"] = "latest"
	labels["pod-security.kubernetes.io/warn"] = "restricted"
	labels["pod-security.kubernetes.io/warn-version"] = "latest"
	return labels
}

func (r *LakeAppReconciler) reconcileLakeAppNamespace(ctx context.Context, app *oramav1alpha1.LakeApp, name string) error {
	if app.Spec.Sync != nil {
		var existing corev1.Namespace
		if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
			return fmt.Errorf("read workspace namespace %q: %w", name, err)
		}
		// The namespace must be the workspace's own, labelled by the workspaces reconciler with the
		// workspace and its customer, and never the Lake namespace: the operator decides this from
		// the cluster, not from the row's workspace_namespace (#713).
		if name == app.Namespace || existing.Labels[workspaceLabel] != app.Spec.Sync.WorkspaceID ||
			existing.Labels[workspaceCustomerLabel] != app.Spec.Sync.CustomerID {
			return fmt.Errorf("namespace %q is not owned by workspace %s of customer %s", name, app.Spec.Sync.WorkspaceID, app.Spec.Sync.CustomerID)
		}
		return nil
	}
	desired := &corev1.Namespace{ObjectMeta: lakeAppObjectMeta(app, name, "")}
	desired.Labels = lakeAppNamespaceLabels(app)
	if err := r.Create(ctx, desired); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create app namespace %q: %w", name, err)
	}

	var existing corev1.Namespace
	if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
		return fmt.Errorf("read existing app namespace %q: %w", name, err)
	}
	if existing.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
		return fmt.Errorf("namespace %q is not owned by LakeApp %s", name, lakeAppOwner(app))
	}
	changed := false
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for key, value := range lakeAppNamespaceLabels(app) {
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

func (r *LakeAppReconciler) reconcileLakeAppProfile(ctx context.Context, lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, namespace string) error {
	if app.Spec.Sync != nil {
		return r.reconcileLakeAppAccessPolicy(ctx, lake, app, namespace)
	}
	quota := &corev1.ResourceQuota{ObjectMeta: lakeAppObjectMeta(app, lakeAppProfileName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Labels = lakeAppLabels(app)
		quota.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
		quota.Spec.Hard = corev1.ResourceList{
			corev1.ResourceLimitsCPU:      resource.MustParse("8"),
			corev1.ResourceLimitsMemory:   resource.MustParse("16Gi"),
			corev1.ResourcePods:           resource.MustParse("50"),
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("8Gi"),
			corev1.ResourceServices:       resource.MustParse("20"),
		}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile app ResourceQuota: %w", err)
	}

	limitRange := &corev1.LimitRange{ObjectMeta: lakeAppObjectMeta(app, lakeAppProfileName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, limitRange, func() error {
		limitRange.Labels = lakeAppLabels(app)
		limitRange.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
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
		return fmt.Errorf("reconcile app LimitRange: %w", err)
	}

	deny := &networkingv1.NetworkPolicy{ObjectMeta: lakeAppObjectMeta(app, lakeAppDefaultDenyName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deny, func() error {
		deny.Labels = lakeAppLabels(app)
		deny.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
		deny.Spec.PodSelector = metav1.LabelSelector{}
		deny.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
		deny.Spec.Ingress = nil
		deny.Spec.Egress = nil
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile app default-deny NetworkPolicy: %w", err)
	}

	return r.reconcileLakeAppAccessPolicy(ctx, lake, app, namespace)
}

func (r *LakeAppReconciler) reconcileLakeAppAccessPolicy(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	app *oramav1alpha1.LakeApp,
	namespace string,
) error {
	connectionsActive := false
	if app.Spec.Sync != nil {
		var workspace corev1.Namespace
		if err := r.reader().Get(ctx, client.ObjectKey{Name: namespace}, &workspace); err != nil {
			return fmt.Errorf("read workspace connection rollout state: %w", err)
		}
		connectionsActive = workspace.Annotations[workspaceConnectionsActiveAnnotation] == "true"
	}
	access := &networkingv1.NetworkPolicy{
		ObjectMeta: lakeAppObjectMeta(app, lakeAppAccessPolicyName(app), namespace),
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, access, func() error {
		comment := "ingress-nginx serves this app and its owning Lake proxies its backend on the declared app port; app egress retains DNS, Lake and explicit bindings."
		if connectionsActive {
			comment = "ingress-nginx and the owning Lake reach this app; app egress retains DNS and Lake while declared workload paths come from Connection resources."
		}
		access.Labels = lakeAppLabels(app)
		access.Annotations = map[string]string{
			lakeAppOwnerAnnotation:          lakeAppOwner(app),
			networkSandboxCommentAnnotation: comment,
		}
		access.Spec.PodSelector = metav1.LabelSelector{MatchLabels: lakeAppLabels(app)}
		access.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
		access.Spec.Ingress = lakeAppIngressRules(lake, app)
		access.Spec.Egress = lakeAppEgressRules(lake, app, connectionsActive)
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile app access NetworkPolicy: %w", err)
	}
	return nil
}

func lakeAppIngressRules(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) []networkingv1.NetworkPolicyIngressRule {
	rules := []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": "ingress-nginx",
			}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{
			Protocol: protocolPtr(corev1.ProtocolTCP),
			Port:     intstrPtr(intstr.FromInt32(app.Spec.Port)),
		}},
	}}
	// Lake proxies this app's backend; only the owning Lake pod in its exact namespace may dial it.
	lakePods := lakePodSelector(lake)
	rules = append(rules, networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: namespaceNameSelector(lakeNamespace(lake)),
			PodSelector:       &lakePods,
		}},
		Ports: tcpPorts(app.Spec.Port),
	})
	return rules
}

func lakeAppEgressRules(
	lake *oramav1alpha1.Lake,
	app *oramav1alpha1.LakeApp,
	connectionsActive bool,
) []networkingv1.NetworkPolicyEgressRule {
	rules := []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": "kube-system",
			}},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: protocolPtr(corev1.ProtocolUDP), Port: intstrPtr(intstr.FromInt(53))},
			{Protocol: protocolPtr(corev1.ProtocolTCP), Port: intstrPtr(intstr.FromInt(53))},
		},
	}}

	mode := app.Spec.Mode
	if mode == "" {
		mode = oramav1alpha1.LakeAppModeRuntime
	}
	if mode == oramav1alpha1.LakeAppModeRuntime || app.Spec.Sync != nil {
		rules = append(rules, lakeAppLakeEgressRule(lake, app.Namespace))
	}
	if connectionsActive && app.Spec.Sync != nil {
		return rules
	}
	if app.Spec.Sync == nil && app.Spec.Bindings != nil && app.Spec.Bindings.Zetta != nil {
		customer := serviceCustomerLabelValue(app.Spec.Bindings.Zetta.CustomerID)
		rules = append(rules, lakeAppTargetEgressRule(app.Namespace, map[string]string{
			serviceCustomerLabel: customer,
			serviceKindLabel:     serviceKindZetta,
		}))
	}
	dataNamespace := app.Namespace
	if app.Spec.Sync != nil {
		dataNamespace = app.Spec.Sync.WorkspaceNamespace
	}
	if app.Spec.Bindings != nil {
		serviceIDs := append([]string(nil), app.Spec.Bindings.Services...)
		sort.Strings(serviceIDs)
		for _, serviceID := range serviceIDs {
			rules = append(rules, lakeAppTargetEgressRule(dataNamespace, map[string]string{serviceIDLabel: serviceID}))
		}
	}
	if app.Spec.Sync == nil && app.Spec.EventsSecretRef != nil {
		rules = append(rules, lakeAppTargetEgressRule(app.Namespace, map[string]string{
			"app.kubernetes.io/name":     "orama-zetta",
			"app.kubernetes.io/instance": "lake-" + lake.Name + "-zetta",
		}))
	}
	return rules
}

// lakeAppLakeEgressRule lets the app call its Lake's API, on the Lake container port only: a tenant's
// app has no business with any other port of the Lake pod (operator#711).
func lakeAppLakeEgressRule(lake *oramav1alpha1.Lake, namespace string) networkingv1.NetworkPolicyEgressRule {
	rule := lakeAppTargetEgressRule(namespace, map[string]string{
		"app.kubernetes.io/name":     "lake",
		"app.kubernetes.io/instance": lake.Name,
	})
	rule.Ports = tcpPorts(int32(lakeContainerPort(lake)))
	return rule
}

func lakeAppTargetEgressRule(namespace string, podLabels map[string]string) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"kubernetes.io/metadata.name": namespace,
		}},
		PodSelector: &metav1.LabelSelector{MatchLabels: podLabels},
	}}}
}

type lakeAppSecrets struct {
	EnvFromNames []string
	SecretEnv    []corev1.EnvVar
	BindingsName string
	EventsName   string
	SourceName   string
	PullName     string
	Digest       string
	// StoreSecrets maps a mounted bucket's store id to its credential copy in the app namespace
	// (lake#666).
	StoreSecrets map[string]string
}

func (r *LakeAppReconciler) reconcileLakeAppSecrets(ctx context.Context, lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, namespace string) (lakeAppSecrets, string, string, error) {
	wanted := map[string]bool{}
	digests := map[string][]byte{}
	result := lakeAppSecrets{}
	if app.Spec.Sync != nil {
		var reason, message string
		var err error
		result, reason, message, err = r.reconcileSyncedLakeAppSecrets(ctx, lake, app, namespace, wanted, digests)
		if err != nil || reason != "" {
			return result, reason, message, err
		}
	}

	reason, message, err := r.reconcileLakeAppLakeSecrets(ctx, lake, app)
	if err != nil || reason != "" {
		return result, reason, message, err
	}
	reason, message, err = r.copyLakeAppStoreCredentials(ctx, lake, app, namespace, wanted, digests, &result)
	if err != nil || reason != "" {
		return result, reason, message, err
	}
	reason, message, err = r.reconcileLakeAppEnvironmentSecrets(ctx, app, namespace, wanted, digests, &result)
	if err != nil || reason != "" {
		return result, reason, message, err
	}
	mode := app.Spec.Mode
	if mode == "" {
		mode = oramav1alpha1.LakeAppModeRuntime
	}
	if app.Spec.Sync == nil && mode == oramav1alpha1.LakeAppModeRuntime && app.Spec.Source != nil {
		selector := app.Spec.Source.TokenSecretRef
		source, reason, message, err := r.lakeAppSourceSecret(ctx, app, selector.Name)
		if err != nil || reason != "" {
			return result, reason, message, err
		}
		value, found := source.Data[selector.Key]
		if !found || len(value) == 0 {
			message := fmt.Sprintf("source token Secret %q has no %s key", selector.Name, selector.Key)
			return result, "SourceCredentialPending", message, nil
		}
		data := map[string][]byte{"token": value}
		targetName := lakeAppSourceName(app)
		if err := r.copyLakeAppSecret(ctx, app, namespace, targetName, corev1.SecretTypeOpaque, data); err != nil {
			return result, "", "", err
		}
		wanted[targetName] = true
		result.SourceName = targetName
		addLakeAppSecretDigest(digests, targetName, data)
	}

	if app.Spec.Sync == nil && app.Spec.Bindings != nil && (app.Spec.Bindings.Zetta != nil || len(app.Spec.Bindings.Services) > 0) {
		sourceName := app.Spec.Bindings.SecretRef.Name
		source, reason, message, err := r.lakeAppSourceSecret(ctx, app, sourceName)
		if err != nil || reason != "" {
			return result, reason, message, err
		}
		data := map[string][]byte{}
		if app.Spec.Bindings.Zetta != nil {
			value, found := source.Data["zetta"]
			if !found || len(value) == 0 {
				return result, "BindingPending", fmt.Sprintf("binding Secret %q has no zetta key", sourceName), nil
			}
			binding, err := lakeAppZettaBinding(lake, app, value)
			if err != nil {
				return result, "", "", err
			}
			data["zetta"] = binding
		}
		for _, serviceID := range app.Spec.Bindings.Services {
			value, found := source.Data[serviceID]
			if !found || len(value) == 0 {
				message := fmt.Sprintf("binding Secret %q has no key for service %q", sourceName, serviceID)
				return result, "BindingPending", message, nil
			}
			data[serviceID] = value
		}
		targetName := lakeAppBindingsName(app)
		if err := r.copyLakeAppSecret(ctx, app, namespace, targetName, corev1.SecretTypeOpaque, data); err != nil {
			return result, "", "", err
		}
		wanted[targetName] = true
		result.BindingsName = targetName
		addLakeAppSecretDigest(digests, targetName, data)
	}

	if app.Spec.Sync == nil && app.Spec.EventsSecretRef != nil && app.Spec.EventsSecretRef.Name != "" {
		sourceName := app.Spec.EventsSecretRef.Name
		source, reason, message, err := r.lakeAppSourceSecret(ctx, app, sourceName)
		if err != nil || reason != "" {
			return result, reason, message, err
		}
		data := map[string][]byte{}
		for _, key := range []string{"url", "token"} {
			value, found := source.Data[key]
			if !found || len(value) == 0 {
				message := fmt.Sprintf("events Secret %q has no %s key", sourceName, key)
				return result, "EventsCredentialPending", message, nil
			}
			data[key] = value
		}
		targetName := lakeAppEventsName(app)
		if err := r.copyLakeAppSecret(ctx, app, namespace, targetName, corev1.SecretTypeOpaque, data); err != nil {
			return result, "", "", err
		}
		wanted[targetName] = true
		result.EventsName = targetName
		addLakeAppSecretDigest(digests, targetName, data)
	}

	pullName := lake.Spec.PullSecretName
	if mode == oramav1alpha1.LakeAppModeImage && app.Spec.PullSecretName != "" {
		pullName = app.Spec.PullSecretName
	}
	if app.Spec.Sync != nil && (pullName != "" || lake.Spec.Host != "") {
		data, reason, message, err := r.mergedLakeAppPullSecret(ctx, lake, app, namespace, pullName)
		if err != nil || reason != "" {
			return result, reason, message, err
		}
		targetName := lakeAppPullName(app)
		if err := r.copyLakeAppSecret(ctx, app, namespace, targetName, corev1.SecretTypeDockerConfigJson, data); err != nil {
			return result, "", "", err
		}
		wanted[targetName] = true
		result.PullName = targetName
		addLakeAppSecretDigest(digests, targetName, data)
	} else if pullName != "" {
		source, reason, message, err := r.lakeAppSourceSecret(ctx, app, pullName)
		if err != nil || reason != "" {
			return result, reason, message, err
		}
		targetName := lakeAppPullName(app)
		if err := r.copyLakeAppSecret(ctx, app, namespace, targetName, source.Type, source.Data); err != nil {
			return result, "", "", err
		}
		wanted[targetName] = true
		result.PullName = targetName
		addLakeAppSecretDigest(digests, targetName, source.Data)
	}
	if err := r.deleteStaleLakeAppSecrets(ctx, app, namespace, wanted); err != nil {
		return result, "", "", err
	}
	result.Digest = lakeAppSecretDigest(digests)
	return result, "", "", nil
}

func (r *LakeAppReconciler) mergedLakeAppPullSecret(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	app *oramav1alpha1.LakeApp,
	namespace string,
	sourceName string,
) (map[string][]byte, string, string, error) {
	var source *corev1.Secret
	if sourceName != "" {
		found, reason, message, err := r.lakeAppSourceSecret(ctx, app, sourceName)
		if err != nil || reason != "" {
			return nil, reason, message, err
		}
		source = found
	}
	var tokens corev1.Secret
	tokensKey := client.ObjectKey{Namespace: namespace, Name: lakeAppTokensName(app)}
	if err := r.Get(ctx, tokensKey, &tokens); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", "", err
		}
		return nil, "TokenRegistrationPending", "waiting for app token hashes to be registered in Lake", nil
	}
	if !lakeAppOwnsTokens(app, &tokens) || len(tokens.Data["pull"]) == 0 {
		return nil, "TokenRegistrationPending", "waiting for app token hashes to be registered in Lake", nil
	}
	config, ok := mergeLakeAppDockerConfig(source, lake.Spec.Host, tokens.Data["pull"])
	if !ok {
		return nil, "PullSecretInvalid", fmt.Sprintf("pull Secret %q has invalid dockerconfigjson data", sourceName), nil
	}
	return map[string][]byte{corev1.DockerConfigJsonKey: config}, "", "", nil
}

func mergeLakeAppDockerConfig(source *corev1.Secret, host string, token []byte) ([]byte, bool) {
	return mergeRegistryDockerConfig(source, host, "app", token)
}

// mergeRegistryDockerConfig adds lake's registry host, with a Basic username and token, to the
// auths of a dockerconfigjson source Secret (which may be nil).
func mergeRegistryDockerConfig(source *corev1.Secret, host, username string, token []byte) ([]byte, bool) {
	config := map[string]json.RawMessage{}
	if source != nil {
		raw, found := source.Data[corev1.DockerConfigJsonKey]
		if !found || json.Unmarshal(raw, &config) != nil || config == nil {
			return nil, false
		}
	}
	auths := map[string]json.RawMessage{}
	if raw, found := config["auths"]; found {
		if json.Unmarshal(raw, &auths) != nil || auths == nil {
			return nil, false
		}
	}
	if host != "" {
		credentials, err := json.Marshal(map[string]string{
			"username": username,
			"password": string(token),
		})
		if err != nil {
			return nil, false
		}
		auths[host] = credentials
	}
	encodedAuths, err := json.Marshal(auths)
	if err != nil {
		return nil, false
	}
	config["auths"] = encodedAuths
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

type lakeAppEnvironmentSecretSource struct {
	full bool
	keys map[string]bool
}

func (r *LakeAppReconciler) reconcileLakeAppEnvironmentSecrets(
	ctx context.Context,
	app *oramav1alpha1.LakeApp,
	namespace string,
	wanted map[string]bool,
	digests map[string][]byte,
	result *lakeAppSecrets,
) (string, string, error) {
	sources, reason, message := lakeAppEnvironmentSecretSources(app)
	if reason != "" {
		return reason, message, nil
	}
	targetNames, reason, message, err := r.copyLakeAppEnvironmentSecrets(ctx, app, namespace, sources, wanted, digests, result)
	if err != nil || reason != "" {
		return reason, message, err
	}
	sort.Strings(result.EnvFromNames)
	secretEnvironment := append([]oramav1alpha1.LakeAppSecretEnvironmentVariable(nil), app.Spec.SecretEnv...)
	sort.Slice(secretEnvironment, func(i, j int) bool {
		return secretEnvironment[i].Name < secretEnvironment[j].Name
	})
	for _, variable := range secretEnvironment {
		selector := variable.SecretKeyRef
		result.SecretEnv = append(result.SecretEnv, secretEnv(variable.Name, targetNames[selector.Name], selector.Key))
	}
	return "", "", nil
}

func lakeAppEnvironmentSecretSources(app *oramav1alpha1.LakeApp) (map[string]*lakeAppEnvironmentSecretSource, string, string) {
	sources := map[string]*lakeAppEnvironmentSecretSource{}
	for _, sourceName := range app.Spec.SecretRefs {
		if len(validation.IsDNS1123Subdomain(sourceName)) != 0 {
			message := fmt.Sprintf("secret reference %q must be a valid Secret name", sourceName)
			return nil, "SecretReferenceInvalid", message
		}
		request := lakeAppEnvironmentSecretSourceFor(sources, sourceName)
		request.full = true
	}
	environmentNames := map[string]bool{}
	for _, variable := range app.Spec.SecretEnv {
		if len(validation.IsEnvVarName(variable.Name)) != 0 || environmentNames[variable.Name] {
			message := fmt.Sprintf("secret environment name %q is invalid or duplicated", variable.Name)
			return nil, "SecretReferenceInvalid", message
		}
		environmentNames[variable.Name] = true
		selector := variable.SecretKeyRef
		if len(validation.IsDNS1123Subdomain(selector.Name)) != 0 {
			message := fmt.Sprintf("secret environment %q has an invalid Secret name", variable.Name)
			return nil, "SecretReferenceInvalid", message
		}
		if len(validation.IsConfigMapKey(selector.Key)) != 0 {
			message := fmt.Sprintf("secret environment %q has an invalid Secret key", variable.Name)
			return nil, "SecretReferenceInvalid", message
		}
		request := lakeAppEnvironmentSecretSourceFor(sources, selector.Name)
		request.keys[selector.Key] = true
	}
	return sources, "", ""
}

func lakeAppEnvironmentSecretSourceFor(sources map[string]*lakeAppEnvironmentSecretSource, name string) *lakeAppEnvironmentSecretSource {
	request := sources[name]
	if request == nil {
		request = &lakeAppEnvironmentSecretSource{keys: map[string]bool{}}
		sources[name] = request
	}
	return request
}

func (r *LakeAppReconciler) copyLakeAppEnvironmentSecrets(
	ctx context.Context,
	app *oramav1alpha1.LakeApp,
	namespace string,
	sources map[string]*lakeAppEnvironmentSecretSource,
	wanted map[string]bool,
	digests map[string][]byte,
	result *lakeAppSecrets,
) (map[string]string, string, string, error) {
	sourceNames := make([]string, 0, len(sources))
	for sourceName := range sources {
		sourceNames = append(sourceNames, sourceName)
	}
	sort.Strings(sourceNames)
	targetNames := make(map[string]string, len(sourceNames))
	for _, sourceName := range sourceNames {
		targetName := lakeAppEnvSecretName(app, sourceName)
		if lakeAppSecretNameIsManaged(app, targetName) {
			message := fmt.Sprintf("secret reference %q conflicts with an operator-managed Secret", sourceName)
			return nil, "SecretReferenceConflict", message, nil
		}
		source, reason, message, err := r.lakeAppSourceSecret(ctx, app, sourceName)
		if err != nil || reason != "" {
			return nil, reason, message, err
		}
		request := sources[sourceName]
		data := source.Data
		secretType := source.Type
		if !request.full {
			data = map[string][]byte{}
			secretType = corev1.SecretTypeOpaque
		}
		for key := range request.keys {
			value, found := source.Data[key]
			if !found {
				message := fmt.Sprintf("Secret %q has no %q key", sourceName, key)
				return nil, "SecretReferenceKeyNotFound", message, nil
			}
			if !request.full {
				data[key] = value
			}
		}
		if err := r.copyLakeAppSecret(ctx, app, namespace, targetName, secretType, data); err != nil {
			return nil, "", "", err
		}
		wanted[targetName] = true
		targetNames[sourceName] = targetName
		addLakeAppSecretDigest(digests, targetName, data)
		if request.full {
			result.EnvFromNames = append(result.EnvFromNames, targetName)
		}
	}
	return targetNames, "", "", nil
}

func lakeAppDeliversSecret(app *oramav1alpha1.LakeApp, name string) bool {
	for _, variable := range app.Spec.SecretEnv {
		if variable.LakeSecret != nil && variable.SecretKeyRef.Name == name {
			return true
		}
	}
	return false
}

// lakeAppLakeSecretLabel marks the LakeSealedSecrets a LakeApp owns for its lake secrets.
const lakeAppLakeSecretLabel = "lake.orama.cloud/app-lake-secret"

// reconcileLakeAppLakeSecrets owns one LakeSealedSecret per lake secret the app reads (lake#625).
// Each has a lake source and produces, in the Lake namespace, the Secret the app's SecretKeyRef
// names; the environment copy below then carries the field into the app namespace, and its digest
// rolls the pods when a new secret version is delivered. The user's secret stays in lake's store,
// sealed in its workspace Zetta: the Secret is only this projection, rewritten on rotation.
func (r *LakeAppReconciler) reconcileLakeAppLakeSecrets(ctx context.Context, lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) (string, string, error) {
	wanted := map[string]oramav1alpha1.LakeAppLakeSecretReference{}
	for _, variable := range app.Spec.SecretEnv {
		if variable.LakeSecret != nil {
			wanted[variable.SecretKeyRef.Name] = *variable.LakeSecret
		}
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)
	labels := lakeAppLabels(app)
	labels[lakeAppLakeSecretLabel] = "true"
	pending := ""
	for _, name := range names {
		reference := wanted[name]
		sealed := &oramav1alpha1.LakeSealedSecret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sealed, func() error {
			if sealed.ResourceVersion != "" && sealed.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
				return fmt.Errorf("LakeSealedSecret %q is not owned by this LakeApp", name)
			}
			sealed.Labels = labels
			if sealed.Annotations == nil {
				sealed.Annotations = map[string]string{}
			}
			sealed.Annotations[lakeAppOwnerAnnotation] = lakeAppOwner(app)
			sealed.Spec.LakeRef = corev1.LocalObjectReference{Name: lake.Name}
			sealed.Spec.Template = oramav1alpha1.LakeSealedSecretTemplate{Name: name, Type: corev1.SecretTypeOpaque}
			sealed.Spec.Data = nil
			sealed.Spec.Source = oramav1alpha1.LakeSealedSecretSource{
				Type: "lake",
				Lake: &oramav1alpha1.LakeStaticSecretSource{Mount: reference.Mount, Name: reference.Name},
			}
			// The Lake stays its controller (the LakeSealedSecret reconciler sets it); the app is a
			// plain owner, so deleting the app deletes the delivery and its Secret.
			return controllerutil.SetOwnerReference(app, sealed, r.Scheme)
		}); err != nil {
			return "", "", fmt.Errorf("reconcile LakeSealedSecret %q: %w", name, err)
		}
		ready := apimeta.FindStatusCondition(sealed.Status.Conditions, "Ready")
		if sealed.Status.Phase == "Error" && ready != nil && pending == "" {
			pending = fmt.Sprintf("lake secret %q in mount %s: %s", reference.Name, reference.Mount, ready.Message)
		}
	}
	var existing oramav1alpha1.LakeSealedSecretList
	if err := r.List(ctx, &existing, client.InNamespace(app.Namespace), client.MatchingLabels(labels)); err != nil {
		return "", "", err
	}
	for index := range existing.Items {
		sealed := &existing.Items[index]
		if sealed.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
			continue
		}
		if _, keep := wanted[sealed.Name]; keep {
			continue
		}
		if err := r.Delete(ctx, sealed); err != nil && !apierrors.IsNotFound(err) {
			return "", "", err
		}
	}
	if pending != "" {
		return "SecretDeliveryPending", pending, nil
	}
	return "", "", nil
}

// lakeAppStoreSecretName is the copy, in the app namespace, of a mounted bucket's credential.
func lakeAppStoreSecretName(app *oramav1alpha1.LakeApp, slug string) string {
	return lakeAppManagedName(app, "store-"+slug, "store-"+slug)
}

// copyLakeAppStoreCredentials copies each mounted bucket's delivered credential (Secret
// <lake>-<slug> in the Lake namespace, produced by the store's LakeSealedSecret) into the app
// namespace, so the app's STORE_<NAME>_* variables can read it there; its digest rolls the pods on a
// new credential version (lake#666). The app waits until every mounted bucket's credential is there.
func (r *LakeAppReconciler) copyLakeAppStoreCredentials(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	app *oramav1alpha1.LakeApp,
	namespace string,
	wanted map[string]bool,
	digests map[string][]byte,
	result *lakeAppSecrets,
) (string, string, error) {
	for _, store := range app.Spec.Stores {
		if store.Type == storeTypeVolume {
			continue
		}
		data, found, err := readStoreCredential(ctx, r.Client, lake, app.Namespace, workloadStoreMount{StoreID: store.ID, Secret: store.SecretName})
		if err != nil {
			return "StoreCredentialInvalid", fmt.Sprintf("store %q: %v", store.Name, err), nil
		}
		if !found {
			return "StoreCredentialPending", fmt.Sprintf("waiting for the credentials of store %q from the workspace's secret store", store.Name), nil
		}
		targetName := lakeAppStoreSecretName(app, store.Slug)
		if err := r.copyLakeAppSecret(ctx, app, namespace, targetName, corev1.SecretTypeOpaque, data); err != nil {
			return "", "", err
		}
		wanted[targetName] = true
		addLakeAppSecretDigest(digests, targetName, data)
		if result.StoreSecrets == nil {
			result.StoreSecrets = map[string]string{}
		}
		result.StoreSecrets[store.ID] = targetName
	}
	return "", "", nil
}

// lakeAppStoreMounts is the app's stores as the pod-building helpers take them.
func lakeAppStoreMounts(app *oramav1alpha1.LakeApp) []workloadStoreMount {
	out := make([]workloadStoreMount, 0, len(app.Spec.Stores))
	for _, store := range app.Spec.Stores {
		out = append(out, workloadStoreMount{
			StoreID: store.ID, Name: store.Name, Slug: store.Slug, Type: store.Type, Provider: store.Provider,
			Claim: store.ClaimName, MountPath: store.MountPath, ReadOnly: store.ReadOnly,
			Bucket: store.Bucket, Endpoint: store.Endpoint, Region: store.Region, Prefix: store.Prefix, Secret: store.SecretName,
		})
	}
	return out
}

func lakeAppSecretNameIsManaged(app *oramav1alpha1.LakeApp, name string) bool {
	return name == lakeAppTokensName(app) ||
		name == lakeAppBindingsName(app) ||
		name == lakeAppEventsName(app) ||
		name == lakeAppSourceName(app) ||
		name == lakeAppPullName(app)
}

func (r *LakeAppReconciler) lakeAppSourceSecret(ctx context.Context, app *oramav1alpha1.LakeApp, name string) (*corev1.Secret, string, string, error) {
	if name == "" || strings.Contains(name, "/") {
		return nil, "SecretReferenceInvalid", fmt.Sprintf("secret reference %q must be a resource name", name), nil
	}
	var source corev1.Secret
	key := client.ObjectKey{Namespace: app.Namespace, Name: name}
	if err := r.Get(ctx, key, &source); err != nil {
		if apierrors.IsNotFound(err) && lakeAppDeliversSecret(app, name) {
			return nil, "SecretDeliveryPending", fmt.Sprintf("lake has not delivered Secret %q yet", name), nil
		}
		if apierrors.IsNotFound(err) {
			return nil, "SecretReferenceNotFound", fmt.Sprintf("Secret %q was not found", name), nil
		}
		return nil, "", "", err
	}
	return &source, "", "", nil
}

func (r *LakeAppReconciler) copyLakeAppSecret(ctx context.Context, app *oramav1alpha1.LakeApp, namespace, name string, secretType corev1.SecretType, data map[string][]byte) error {
	destination := &corev1.Secret{ObjectMeta: lakeAppObjectMeta(app, name, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, destination, func() error {
		if destination.ResourceVersion != "" && destination.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
			return fmt.Errorf("Secret %q is not owned by this LakeApp", name)
		}
		destination.Labels = lakeAppLabels(app)
		destination.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
		destination.Type = secretType
		destination.Immutable = nil
		destination.Data = make(map[string][]byte, len(data))
		for key, value := range data {
			destination.Data[key] = append([]byte(nil), value...)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("copy app Secret %q: %w", name, err)
	}
	return nil
}

func (r *LakeAppReconciler) deleteStaleLakeAppSecrets(ctx context.Context, app *oramav1alpha1.LakeApp, namespace string, wanted map[string]bool) error {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(namespace), client.MatchingLabels(lakeAppLabels(app))); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) || wanted[secret.Name] {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func addLakeAppSecretDigest(destination map[string][]byte, name string, data map[string][]byte) {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var encoded bytes.Buffer
	encoded.WriteString(name)
	encoded.WriteByte(0)
	for _, key := range keys {
		encoded.WriteString(key)
		encoded.WriteByte(0)
		encoded.Write(data[key])
		encoded.WriteByte(0)
	}
	destination[name] = encoded.Bytes()
}

func lakeAppSecretDigest(values map[string][]byte) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		hash.Write(values[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func lakeAppHost(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) string {
	if app.Spec.Host != "" {
		return app.Spec.Host
	}
	wildcard := lakeAppsWildcard(lake)
	if wildcard == "" {
		return ""
	}
	name := dnsLabel(app.Name)
	if name == "" {
		return ""
	}
	return name + "." + wildcard
}

func lakeAppRevision(app *oramav1alpha1.LakeApp) string {
	mode := app.Spec.Mode
	if mode == "" {
		mode = oramav1alpha1.LakeAppModeRuntime
	}
	if mode == oramav1alpha1.LakeAppModeRuntime && app.Spec.Source != nil {
		return app.Spec.Source.Revision
	}
	return app.Spec.Image
}

func lakeAppImage(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) string {
	mode := app.Spec.Mode
	if mode == "" || mode == oramav1alpha1.LakeAppModeRuntime {
		return lakeAppsRuntimeImage(lake)
	}
	return app.Spec.Image
}

func lakeAppResources(app *oramav1alpha1.LakeApp) corev1.ResourceRequirements {
	if app.Spec.Resources != nil {
		return *app.Spec.Resources.DeepCopy()
	}
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

// lakeAppAuth is the app's auth mode with the CRD default applied. It is lake only when the Ingress
// puts Lake's forward auth in front of the app, and public for anything else, so the pod is never
// told it is protected when it is not.
func lakeAppAuth(app *oramav1alpha1.LakeApp) string {
	if app.Spec.Auth == "" || app.Spec.Auth == oramav1alpha1.LakeAppAuthLake {
		return oramav1alpha1.LakeAppAuthLake
	}
	return oramav1alpha1.LakeAppAuthPublic
}

// lakeAppLakeURL is the Lake base URL an app calls: the workspace's /w/<workspace id> route for a
// synchronized app, Lake's in-cluster Service for a standalone one. LAKE_URL and the Zetta binding
// both use it.
func lakeAppLakeURL(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) string {
	if app.Spec.Sync != nil {
		return workspaceLakeInternalURL(lake, app.Spec.Sync.WorkspaceID)
	}
	return lakeInternalURL(lake, app.Namespace)
}

// lakeAppZettaBindingDocument is the file app-runtime reads at /var/run/orama/bindings/zetta
// (app-runtime docs/data-apps/bindings.md): one JSON object naming Lake, the workspace owner and the
// scoped bearer. app-runtime calls <lake_url>/workspaces/<customer_id>/zetta/v1/<capability> with it.
// A bare token in that file is not JSON and stops the app at startup (lake#1205).
type lakeAppZettaBindingDocument struct {
	LakeURL    string `json:"lake_url"`
	CustomerID string `json:"customer_id"`
	Token      string `json:"token"`
}

// lakeAppZettaBinding renders the Zetta binding file for an app whose spec.bindings.zetta is set.
func lakeAppZettaBinding(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, token []byte) ([]byte, error) {
	encoded, err := json.Marshal(lakeAppZettaBindingDocument{
		LakeURL:    lakeAppLakeURL(lake, app),
		CustomerID: app.Spec.Bindings.Zetta.CustomerID,
		Token:      string(token),
	})
	if err != nil {
		return nil, fmt.Errorf("cannot encode the zetta binding: %w", err)
	}
	return encoded, nil
}

func lakeAppManagedEnv(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, secrets lakeAppSecrets) []corev1.EnvVar {
	lakeURL := lakeAppLakeURL(lake, app)
	env := []corev1.EnvVar{
		{Name: "LAKE_URL", Value: lakeURL},
		{Name: "APP_PORT", Value: fmt.Sprint(app.Spec.Port)},
		// app-runtime trusts X-Orama-User only when this says lake: only then does the Ingress
		// replace the client's copy with Lake's forward-auth answer (app-runtime#11).
		{Name: "APP_AUTH", Value: lakeAppAuth(app)},
	}
	mode := app.Spec.Mode
	if mode == "" {
		mode = oramav1alpha1.LakeAppModeRuntime
	}
	if mode == oramav1alpha1.LakeAppModeRuntime && app.Spec.Source != nil {
		source := app.Spec.Source
		env = append(env,
			corev1.EnvVar{Name: "APP_SOURCE", Value: lakeURL + "|" + source.Repository + "|" + source.Revision + "|" + source.Path},
			corev1.EnvVar{Name: "APP_REPOSITORY", Value: source.Repository},
			corev1.EnvVar{Name: "APP_REVISION", Value: source.Revision},
			corev1.EnvVar{Name: "APP_PATH", Value: source.Path},
			corev1.EnvVar{Name: "APP_FRAMEWORK", Value: source.Framework},
		)
		if source.Entrypoint != "" {
			env = append(env, corev1.EnvVar{Name: "APP_ENTRYPOINT", Value: source.Entrypoint})
		}
	}
	if app.Spec.Sync != nil {
		tokensName := lakeAppTokensName(app)
		env = append(env,
			corev1.EnvVar{Name: "APP_ID", Value: app.Spec.Sync.ID},
			corev1.EnvVar{Name: "WORKSPACE_ID", Value: app.Spec.Sync.WorkspaceID},
			corev1.EnvVar{Name: "EVENTS_URL", Value: lakeURL + "/v1/publish_app_event"},
			secretEnv("EVENTS_TOKEN", tokensName, "events"),
		)
		if mode == oramav1alpha1.LakeAppModeRuntime {
			env = append(env, secretEnv("APP_TOKEN", tokensName, "source"))
		}
	} else if secrets.SourceName != "" {
		env = append(env, secretEnv("APP_TOKEN", secrets.SourceName, "token"))
	}
	if secrets.EventsName != "" {
		env = append(env,
			secretEnv("EVENTS_URL", secrets.EventsName, "url"),
			secretEnv("EVENTS_TOKEN", secrets.EventsName, "token"),
		)
	}
	used := make(map[string]bool, len(env)+len(secrets.SecretEnv))
	used["APP_ENTRYPOINT"] = true
	for _, variable := range env {
		used[variable.Name] = true
	}
	for _, variable := range secrets.SecretEnv {
		if used[variable.Name] {
			continue
		}
		env = append(env, variable)
		used[variable.Name] = true
	}
	for _, variable := range app.Spec.Env {
		if used[variable.Name] {
			continue
		}
		env = append(env, corev1.EnvVar{Name: variable.Name, Value: variable.Value})
	}
	return env
}

func lakeAppPodSpec(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, secrets lakeAppSecrets) corev1.PodSpec {
	noToken := false
	noLinks := false
	nonRoot := true
	noEscalation := false
	readOnlyRoot := true
	user := lakeAppRunAsUser
	tempSize := resource.MustParse("1Gi")
	container := corev1.Container{
		Name:            "app",
		Image:           lakeAppImage(lake, app),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             lakeAppManagedEnv(lake, app, secrets),
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: app.Spec.Port,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources: lakeAppResources(app),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &noEscalation,
			ReadOnlyRootFilesystem:   &readOnlyRoot,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: lakeAppTempVolume, MountPath: "/tmp"}},
	}
	probe := corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http")}}
	container.StartupProbe = &corev1.Probe{ProbeHandler: probe, PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 60}
	container.ReadinessProbe = &corev1.Probe{ProbeHandler: probe, PeriodSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3}
	container.LivenessProbe = &corev1.Probe{ProbeHandler: probe, PeriodSeconds: 20, TimeoutSeconds: 2, FailureThreshold: 3}
	for _, name := range secrets.EnvFromNames {
		container.EnvFrom = append(container.EnvFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
		}})
	}
	volumes := []corev1.Volume{{
		Name: lakeAppTempVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: &tempSize,
		}},
	}}
	if secrets.BindingsName != "" && app.Spec.Bindings != nil {
		items := []corev1.KeyToPath{}
		if app.Spec.Bindings.Zetta != nil {
			items = append(items, corev1.KeyToPath{Key: "zetta", Path: "zetta"})
		}
		serviceIDs := append([]string(nil), app.Spec.Bindings.Services...)
		sort.Strings(serviceIDs)
		for _, serviceID := range serviceIDs {
			name := serviceID
			if app.Spec.Bindings.ServiceNames[serviceID] != "" {
				name = app.Spec.Bindings.ServiceNames[serviceID]
			}
			items = append(items, corev1.KeyToPath{Key: serviceID, Path: "services/" + name})
		}
		volumes = append(volumes, corev1.Volume{
			Name: lakeAppBindingsVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secrets.BindingsName,
				Items:      items,
			}},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      lakeAppBindingsVolume,
			MountPath: lakeAppBindingsPath,
			ReadOnly:  true,
		})
	}
	// The stores the app mounts (lake#666): a volume's claim at its path, a bucket's connection as
	// STORE_<NAME>_* variables reading its credential copy.
	if len(app.Spec.Stores) > 0 {
		mounts := lakeAppStoreMounts(app)
		storeVolumes, storeVolumeMounts, storeEnv := storeMountsPodParts(mounts, func(mount workloadStoreMount) string {
			if name := secrets.StoreSecrets[mount.StoreID]; name != "" {
				return name
			}
			return lakeAppStoreSecretName(app, mount.Slug)
		})
		volumes = append(volumes, storeVolumes...)
		container.VolumeMounts = append(container.VolumeMounts, storeVolumeMounts...)
		container.Env = append(container.Env, storeEnv...)
	}
	pod := corev1.PodSpec{
		AutomountServiceAccountToken: &noToken,
		EnableServiceLinks:           &noLinks,
		DNSConfig:                    lakePodDNSConfig(),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &nonRoot,
			RunAsUser:      &user,
			RunAsGroup:     &user,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{container},
		Volumes:    volumes,
	}
	// A mounted volume is written by the app's user: the kubelet gives it the app's group.
	if storeMountsHaveVolume(lakeAppStoreMounts(app)) {
		group := user
		onRootMismatch := corev1.FSGroupChangeOnRootMismatch
		pod.SecurityContext.FSGroup = &group
		pod.SecurityContext.FSGroupChangePolicy = &onRootMismatch
	}
	if secrets.PullName != "" {
		pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: secrets.PullName}}
	}
	return pod
}

func (r *LakeAppReconciler) reconcileLakeAppWorkload(ctx context.Context, lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, namespace string, secrets lakeAppSecrets) error {
	labels := lakeAppLabels(app)
	replicas := int32(1)
	if app.Spec.Replicas != nil {
		replicas = *app.Spec.Replicas
	}
	revisionHistoryLimit := int32(10)
	workloadName := lakeAppWorkloadObjectName(app)
	deployment := &appsv1.Deployment{ObjectMeta: lakeAppObjectMeta(app, workloadName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = labels
		deployment.Annotations = withOwnedAnnotation(deployment.Annotations, lakeAppOwnerAnnotation, lakeAppOwner(app))
		deployment.Spec.Replicas = &replicas
		deployment.Spec.RevisionHistoryLimit = &revisionHistoryLimit
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deployment.Spec.Template.Labels = labels
		deployment.Spec.Template.Annotations = map[string]string{
			lakeAppOwnerAnnotation:          lakeAppOwner(app),
			lakeAppCredentialHashAnnotation: secrets.Digest,
		}
		deployment.Spec.Template.Spec = lakeAppPodSpec(lake, app, secrets)
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile app Deployment: %w", err)
	}

	service := &corev1.Service{ObjectMeta: lakeAppObjectMeta(app, workloadName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		clusterIP := service.Spec.ClusterIP
		clusterIPs := service.Spec.ClusterIPs
		service.Labels = labels
		service.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
		service.Spec.Selector = labels
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       lakeAppServicePort,
			TargetPort: intstr.FromInt32(app.Spec.Port),
		}}
		service.Spec.ClusterIP = clusterIP
		service.Spec.ClusterIPs = clusterIPs
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile app Service: %w", err)
	}
	return nil
}

func (r *LakeAppReconciler) reconcileLakeAppIngress(ctx context.Context, lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, namespace, host string) error {
	dnsName := ""
	if host != "" && !lakeAppUsesWildcard(lake, host) {
		dnsName = lakeAppDNSRecordName(app, host)
	}
	if err := r.deleteLakeAppDNSRecords(ctx, app, dnsName); err != nil {
		return err
	}
	workloadName := lakeAppWorkloadObjectName(app)
	tlsOwner := lakeTLSOwnerValue("LakeApp", app.Namespace+"/"+app.Name)
	if host == "" {
		if _, err := reconcileLakeHostCertificates(ctx, r.Client, r.Scheme, lake, app, tlsOwner, nil); err != nil {
			return err
		}
		if err := r.deleteLakeAppTLS(ctx, namespace, workloadName); err != nil {
			return err
		}
		return deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
			Name: workloadName, Namespace: namespace,
		}})
	}

	// Origin TLS (operator#578): the host's certificate, issued in the Lake namespace and copied
	// here, and the zone's redirect and client-certificate settings.
	// The Lake's Issuer solves DNS-01 in its credential's zone and HTTP-01 for any other host
	// (operator#632, docs/lake/origin-tls.md).
	tlsHosts := lakeIssuableHosts(lake, []string{host}, "")
	sources, err := reconcileLakeHostCertificates(ctx, r.Client, r.Scheme, lake, app, tlsOwner, tlsHosts)
	if err != nil {
		return fmt.Errorf("app certificate: %w", err)
	}
	tls, err := copyLakeTLSSecrets(ctx, r.Client, lake, namespace, workloadName, tlsHosts, sources, func(secret *corev1.Secret) {
		secret.Labels = lakeAppLabels(app)
		secret.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
	})
	if err != nil {
		return err
	}
	edge, err := readLakeEdge(ctx, r.Client, lake)
	if err != nil {
		return err
	}
	caRef, err := reconcileEdgeClientCA(ctx, r.Client, namespace, workloadName+"-edge-client-ca", edge, func(secret *corev1.Secret) error {
		secret.Labels = lakeAppLabels(app)
		secret.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
		return nil
	})
	if err != nil {
		return err
	}

	prefix := networkingv1.PathTypePrefix
	ingressClass := lake.Spec.Apps.IngressClass
	if ingressClass == "" {
		ingressClass = "nginx"
	}
	ingress := &networkingv1.Ingress{ObjectMeta: lakeAppObjectMeta(app, workloadName, namespace)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		ingress.Labels = lakeAppLabels(app)
		ingress.Annotations = lakeAppIngressAnnotations(lake, app)
		ingress.Spec.IngressClassName = &ingressClass
		ingress.Spec.Rules = []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     "/",
					PathType: &prefix,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: workloadName,
						Port: networkingv1.ServiceBackendPort{Number: lakeAppServicePort},
					}},
				}},
			}},
		}}
		ingress.Spec.TLS = tls
		applyLakeEdgeAnnotations(ingress, edge, caRef)
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile app Ingress: %w", err)
	}
	if dnsName == "" || lake.Spec.DNSSecretRef == nil || lake.Spec.DNSSecretRef.Name == "" {
		return r.deleteLakeAppDNSRecords(ctx, app, "")
	}
	target := ingressLoadBalancerTarget(ingress)
	if target == "" {
		return nil
	}
	dns := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: dnsName, Namespace: app.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dns, func() error {
		dns.Labels = lakeAppLabels(app)
		dns.Annotations = map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
		dns.Spec.SecretRef = lake.Spec.DNSSecretRef.Name
		dns.Spec.Name = host
		dns.Spec.Content = target
		dns.Spec.Proxied = true
		return controllerutil.SetControllerReference(app, dns, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconcile app DNS: %w", err)
	}
	return nil
}

// lakeAppIngressAnnotations is the app Ingress's own annotations, before the edge's. A Lake-protected
// app names X-Orama-User and X-Orama-Space as auth response headers: ingress-nginx then sets both
// from Lake's answer and replaces whatever the client sent under those names, so a browser cannot
// forge the signed-in member (operator#711).
func lakeAppIngressAnnotations(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) map[string]string {
	annotations := map[string]string{lakeAppOwnerAnnotation: lakeAppOwner(app)}
	auth := app.Spec.Auth
	if auth == "" {
		auth = oramav1alpha1.LakeAppAuthLake
	}
	if auth != oramav1alpha1.LakeAppAuthLake {
		return annotations
	}
	annotations["nginx.ingress.kubernetes.io/auth-url"] = lakeAppAuthURL(lake, app)
	annotations["nginx.ingress.kubernetes.io/auth-response-headers"] = "X-Orama-User,X-Orama-Space"
	// Lake answers the auth call with the app-scoped session cookie, and a browser without one is
	// sent to Lake's sign-in handoff (lake#407).
	annotations["nginx.ingress.kubernetes.io/auth-always-set-cookie"] = "true"
	if signin := lakeAppAuthSignin(lake, app); signin != "" {
		annotations["nginx.ingress.kubernetes.io/auth-signin"] = signin
	}
	return annotations
}

func lakeAppAuthURL(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) string {
	baseURL := fmt.Sprintf(
		"http://lake-%s.%s.svc.cluster.local:%d",
		lake.Name,
		lakeNamespace(lake),
		lakeServicePort,
	)
	if app.Spec.Sync != nil {
		baseURL += "/w/" + app.Spec.Sync.WorkspaceID
	}
	return baseURL + "/v1/apps/auth"
}

// lakeAppAuthSignin is Lake's public /apps/authorize handoff. The return URL is always https:
// TLS ends at the edge proxy, so nginx's own $scheme would say http.
func lakeAppAuthSignin(lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp) string {
	if !lake.Spec.IngressEnabled || lake.Spec.Host == "" {
		return ""
	}
	query := "rd=https://$host$escaped_request_uri"
	if app.Spec.Sync != nil && app.Spec.Sync.ID != "" {
		query = "app=" + app.Spec.Sync.ID + "&" + query
	}
	return "https://" + lake.Spec.Host + "/apps/authorize?" + query
}

func lakeAppUsesWildcard(lake *oramav1alpha1.Lake, host string) bool {
	wildcard := lakeAppsWildcard(lake)
	return wildcard != "" && strings.HasSuffix(host, "."+wildcard)
}

func lakeAppDNSRecordName(app *oramav1alpha1.LakeApp, host string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(host)))[:8]
	prefix := app.Name
	maxPrefixLength := 253 - len("-dns-") - len(hash)
	if len(prefix) > maxPrefixLength {
		prefix = strings.Trim(prefix[:maxPrefixLength], "-")
	}
	return prefix + "-dns-" + hash
}

func (r *LakeAppReconciler) deleteLakeAppDNSRecords(ctx context.Context, app *oramav1alpha1.LakeApp, keepName string) error {
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := r.List(ctx, &records, client.InNamespace(app.Namespace), client.MatchingLabels{
		lakeAppIdentityLabel: lakeAppLabels(app)[lakeAppIdentityLabel],
	}); err != nil {
		return err
	}
	for i := range records.Items {
		record := &records.Items[i]
		if record.Name == keepName || record.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
			continue
		}
		if err := r.Delete(ctx, record); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *LakeAppReconciler) lakeAppDeploymentState(ctx context.Context, app *oramav1alpha1.LakeApp, deployment *appsv1.Deployment, namespace string) (string, bool, string, string, error) {
	if app.Spec.Replicas != nil && *app.Spec.Replicas == 0 {
		if deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.Replicas == 0 {
			return oramav1alpha1.LakeAppPhaseProgressing, false, "DeploymentStopped", "app stopped", nil
		}
		return oramav1alpha1.LakeAppPhaseProgressing, false, "DeploymentStopping", "waiting for app replicas to stop", nil
	}
	if phase, reason, message, found, err := r.lakeAppPodFailure(ctx, app, namespace); err != nil {
		return "", false, "", "", err
	} else if found {
		return phase, false, reason, message, nil
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			return oramav1alpha1.LakeAppPhaseDegraded, false, "ReplicaFailure", condition.Message, nil
		}
	}
	desired := int32(1)
	if app.Spec.Replicas != nil {
		desired = *app.Spec.Replicas
	}
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return oramav1alpha1.LakeAppPhaseProgressing, false, "DeploymentProgressing", "waiting for the Deployment controller to observe the latest change", nil
	}
	if deployment.Status.ReadyReplicas >= desired && deployment.Status.UpdatedReplicas >= desired && deployment.Status.AvailableReplicas >= desired {
		return oramav1alpha1.LakeAppPhaseReady, true, "DeploymentReady", fmt.Sprintf("all %d replicas ready", desired), nil
	}
	message := fmt.Sprintf("%d/%d replicas ready", deployment.Status.ReadyReplicas, desired)
	return oramav1alpha1.LakeAppPhaseProgressing, false, "DeploymentProgressing", message, nil
}

func (r *LakeAppReconciler) lakeAppPodFailure(ctx context.Context, app *oramav1alpha1.LakeApp, namespace string) (string, string, string, bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(lakeAppLabels(app))); err != nil {
		return "", "", "", false, err
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for _, pod := range pods.Items {
		statuses := append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...)
		statuses = append(statuses, pod.Status.ContainerStatuses...)
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
		for _, status := range statuses {
			if status.State.Waiting != nil {
				switch status.State.Waiting.Reason {
				case "ImagePullBackOff", "ErrImagePull":
					message := fmt.Sprintf("pod %s container %s: %s", pod.Name, status.Name, status.State.Waiting.Reason)
					return oramav1alpha1.LakeAppPhaseDegraded, "ImagePullError", message, true, nil
				case "CrashLoopBackOff":
					message := fmt.Sprintf("pod %s container %s is crash-looping", pod.Name, status.Name)
					return oramav1alpha1.LakeAppPhaseDegraded, "CrashLooping", message, true, nil
				}
			}
			terminated := status.State.Terminated
			if terminated == nil {
				terminated = status.LastTerminationState.Terminated
			}
			if terminated != nil && terminated.Reason == "OOMKilled" {
				message := fmt.Sprintf("pod %s container %s was OOMKilled", pod.Name, status.Name)
				return oramav1alpha1.LakeAppPhaseDegraded, "OOMKilled", message, true, nil
			}
		}
	}
	return "", "", "", false, nil
}

func (r *LakeAppReconciler) finishLakeAppStatus(ctx context.Context, app *oramav1alpha1.LakeApp, deployment *appsv1.Deployment, host, phase string, ready bool, reason, message string) (ctrl.Result, error) {
	desired := app.Status.DeepCopy()
	desired.Phase = phase
	desired.URL = ""
	if host != "" {
		desired.URL = "https://" + host
	}
	desired.Ready = ready
	desired.ObservedRevision = lakeAppRevision(app)
	desired.Message = message
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus,
		ObservedGeneration: app.Generation,
		Reason:             reason,
		Message:            message,
	})
	changed := !apiequality.Semantic.DeepEqual(app.Status, *desired)
	if changed {
		app.Status = *desired
		if err := r.Status().Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		r.publishLakeAppEvent(ctx, app)
	}
	if ready {
		return ctrl.Result{RequeueAfter: lakeAppDriftRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: lakeAppStatusRequeue}, nil
}

func (r *LakeAppReconciler) publishLakeAppEvent(ctx context.Context, app *oramav1alpha1.LakeApp) {
	if app.Spec.Sync != nil {
		r.publishSyncedLakeAppEvent(ctx, app)
		return
	}
	if app.Spec.EventsSecretRef == nil || app.Spec.EventsSecretRef.Name == "" {
		return
	}
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: app.Namespace, Name: app.Spec.EventsSecretRef.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		logf.FromContext(ctx).Info("app event publication skipped", "app", lakeAppOwner(app), "reason", err.Error())
		return
	}
	baseURL := string(secret.Data["url"])
	token := string(secret.Data["token"])
	if baseURL == "" || token == "" {
		return
	}
	event := lakeAppEvent{
		Phase:    app.Status.Phase,
		Ready:    app.Status.Ready,
		Revision: app.Status.ObservedRevision,
		Message:  app.Status.Message,
		URL:      app.Status.URL,
	}
	topic := "app-events." + app.Name
	if r.EventPublisher != nil {
		if err := r.EventPublisher(ctx, baseURL, token, topic, event); err != nil {
			logf.FromContext(ctx).Info("app event publication deferred", "app", lakeAppOwner(app), "reason", err.Error())
		}
		return
	}
	go func() {
		publishCtx, cancel := context.WithTimeout(context.Background(), lakeAppEventTimeout)
		defer cancel()
		if err := publishLakeAppEvent(publishCtx, baseURL, token, topic, event); err != nil {
			logf.Log.Info("app event publication deferred", "app", lakeAppOwner(app), "reason", err.Error())
		}
	}()
}

func publishLakeAppEvent(ctx context.Context, baseURL, token, topic string, event lakeAppEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"topic":     topic,
		"payload":   string(payload),
		"encoding":  "json",
		"mime_type": "application/json",
	})
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(endpoint, "/v1/publish_event") {
		endpoint += "/v1/publish_event"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("publish_event returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (r *LakeAppReconciler) teardownLakeAppWorkload(ctx context.Context, app *oramav1alpha1.LakeApp, namespace string) error {
	workloadName := lakeAppWorkloadObjectName(app)
	objects := []client.Object{
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: workloadName, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: workloadName, Namespace: namespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: workloadName, Namespace: namespace}},
	}
	for _, object := range objects {
		if err := deleteIfExists(ctx, r.Client, object); err != nil {
			return err
		}
	}
	if err := r.deleteLakeAppTLS(ctx, namespace, workloadName); err != nil {
		return err
	}
	return r.deleteLakeAppDNSRecords(ctx, app, "")
}

// deleteLakeAppTLS removes the certificate copies and the edge CA Secret next to an app's Ingress.
// The app's Certificates in the Lake namespace are owned by the LakeApp and go with it.
func (r *LakeAppReconciler) deleteLakeAppTLS(ctx context.Context, namespace, workloadName string) error {
	if err := deleteLakeTLSCopies(ctx, r.Client, namespace, workloadName, nil); err != nil {
		return err
	}
	return deleteIfExists(ctx, r.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: workloadName + "-edge-client-ca", Namespace: namespace}})
}

// requestsForLakeAppEdge re-reconciles the apps in a Lake namespace when one of its certificates is
// issued or renewed (so the copy follows) or its zone policy changes (the redirect and
// client-certificate settings follow).
func (r *LakeAppReconciler) requestsForLakeAppEdge(ctx context.Context, object client.Object) []reconcile.Request {
	if _, ok := object.(*corev1.Secret); ok && !isLakeCertificateSecret(object) {
		return nil
	}
	var apps oramav1alpha1.LakeAppList
	if err := r.List(ctx, &apps, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(apps.Items))
	for i := range apps.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&apps.Items[i])})
	}
	return requests
}

func (r *LakeAppReconciler) deleteLakeAppSharedResources(
	ctx context.Context,
	app *oramav1alpha1.LakeApp,
	namespace string,
) error {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: lakeAppAccessPolicyName(app), Namespace: namespace},
	}
	if err := deleteIfExists(ctx, r.Client, policy); err != nil {
		return err
	}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(namespace), client.MatchingLabels(lakeAppLabels(app))); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *LakeAppReconciler) finalizeLakeApp(ctx context.Context, app *oramav1alpha1.LakeApp) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(app, lakeAppFinalizer) {
		return ctrl.Result{}, nil
	}
	if app.Status.Phase != oramav1alpha1.LakeAppPhaseTornDown {
		desired := app.Status.DeepCopy()
		desired.Phase = oramav1alpha1.LakeAppPhaseTornDown
		desired.Ready = false
		desired.Message = "app resources removed"
		apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: app.Generation,
			Reason:             "TornDown",
			Message:            desired.Message,
		})
		app.Status = *desired
		if err := r.Status().Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		r.publishLakeAppEvent(ctx, app)
	}
	if err := r.teardownLakeAppWorkload(ctx, app, lakeAppNamespace(app)); err != nil {
		return ctrl.Result{}, err
	}

	name := lakeAppNamespace(app)
	if app.Spec.Sync != nil {
		if err := r.deleteLakeAppSharedResources(ctx, app, name); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		var namespace corev1.Namespace
		if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &namespace); err == nil {
			if namespace.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) {
				return ctrl.Result{}, fmt.Errorf("refusing to delete namespace %q without ownership annotation %q", name, lakeAppOwner(app))
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
	}

	var records oramav1alpha1.CloudflareDNSRecordList
	if err := r.List(ctx, &records, client.InNamespace(app.Namespace), client.MatchingLabels{
		lakeAppIdentityLabel: lakeAppLabels(app)[lakeAppIdentityLabel],
	}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range records.Items {
		if records.Items[i].Annotations[lakeAppOwnerAnnotation] == lakeAppOwner(app) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	controllerutil.RemoveFinalizer(app, lakeAppFinalizer)
	if err := r.Update(ctx, app); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LakeAppReconciler) requestsForLakeAppObject(_ context.Context, object client.Object) []reconcile.Request {
	owner := object.GetAnnotations()[lakeAppOwnerAnnotation]
	parts := strings.SplitN(owner, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: parts[0], Name: parts[1]}}}
}

func (r *LakeAppReconciler) requestsForLakeAppLake(ctx context.Context, object client.Object) []reconcile.Request {
	lake, ok := object.(*oramav1alpha1.Lake)
	if !ok {
		return nil
	}
	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}
	var apps oramav1alpha1.LakeAppList
	if err := r.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		return nil
	}
	requests := []reconcile.Request{}
	for i := range apps.Items {
		if apps.Items[i].Spec.LakeRef == lake.Name {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&apps.Items[i])})
		}
	}
	return requests
}

func (r *LakeAppReconciler) requestsForLakeAppSourceSecret(ctx context.Context, object client.Object) []reconcile.Request {
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return nil
	}
	if requests := r.requestsForLakeAppObject(ctx, object); len(requests) > 0 {
		return requests
	}
	var apps oramav1alpha1.LakeAppList
	if err := r.List(ctx, &apps, client.InNamespace(secret.Namespace)); err != nil {
		return nil
	}
	requests := []reconcile.Request{}
	for i := range apps.Items {
		app := &apps.Items[i]
		if r.lakeAppReferencesSecret(ctx, app, secret.Name) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
		}
	}
	return requests
}

// requestsForLakeAppLakeSecret wakes the app that owns a delivery when its status changes.
func (r *LakeAppReconciler) requestsForLakeAppLakeSecret(_ context.Context, object client.Object) []reconcile.Request {
	for _, owner := range object.GetOwnerReferences() {
		if owner.Kind == "LakeApp" {
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: object.GetNamespace(), Name: owner.Name}}}
		}
	}
	return nil
}

func (r *LakeAppReconciler) lakeAppReferencesSecret(ctx context.Context, app *oramav1alpha1.LakeApp, name string) bool {
	if app.Spec.Sync != nil && name == lakeServicesSecret(app.Spec.LakeRef) {
		return true
	}
	if app.Spec.PullSecretName == name || app.Spec.EventsSecretRef != nil && app.Spec.EventsSecretRef.Name == name {
		return true
	}
	if app.Spec.Bindings != nil && app.Spec.Bindings.SecretRef != nil && app.Spec.Bindings.SecretRef.Name == name {
		return true
	}
	if app.Spec.Source != nil && app.Spec.Source.TokenSecretRef.Name == name {
		return true
	}
	for _, reference := range app.Spec.SecretRefs {
		if reference == name {
			return true
		}
	}
	for _, variable := range app.Spec.SecretEnv {
		if variable.SecretKeyRef.Name == name {
			return true
		}
	}
	for _, store := range app.Spec.Stores {
		if store.SecretName != "" && store.SecretName == name {
			return true
		}
	}
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: app.Spec.LakeRef}, &lake); err != nil {
		return false
	}
	return lake.Spec.PullSecretName == name
}

// SetupWithManager sets up watches for the CR, its Lake, source Secrets, and cross-namespace
// children whose status or drift can change the app's observed state.
func (r *LakeAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapObject := handler.EnqueueRequestsFromMapFunc(r.requestsForLakeAppObject)
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.LakeApp{}).
		Watches(&oramav1alpha1.Lake{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLakeAppLake)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLakeAppSourceSecret)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLakeAppEdge)).
		Watches(&oramav1alpha1.EdgeZonePolicy{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLakeAppEdge)).
		Watches(&oramav1alpha1.LakeSealedSecret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLakeAppLakeSecret)).
		Watches(&appsv1.Deployment{}, mapObject).
		Watches(&corev1.Pod{}, mapObject).
		Watches(&corev1.Service{}, mapObject).
		Watches(&networkingv1.Ingress{}, mapObject).
		Watches(&networkingv1.NetworkPolicy{}, mapObject).
		Watches(&corev1.ResourceQuota{}, mapObject).
		Watches(&corev1.LimitRange{}, mapObject).
		Named("lake-app").
		Complete(r)
}
