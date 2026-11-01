/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The managing cluster's half of observability (operator#15, #456). ObservabilityHub and
// ObservabilityAgent are cluster-scoped resources here; their controllers check the clusters'
// purposes, write the provider's concrete kinds onto the hub's and agents' clusters through each
// KubernetesCluster's kubeconfig, copy each cluster its own credentials, and link every agent on
// another cluster to its hub with a PeerConnection pair and a PeerLink (#437, decision D6). The
// clusters' own operators do the rest.

const (
	observabilityFinalizer = "orama.cloud/observability"
	observabilityHubLabel  = "orama.cloud/observability-hub"
	observabilityAgentLbl  = "orama.cloud/observability-agent"
	pixiePullSecret        = "pixie-pull"
	pixieCloudCACopy       = "pixie-cloud-ca"
	pixieConcreteName      = "pixie"
	// pixieHubPeer is the agent's end of its link, in namespace pl of the agent's cluster.
	pixieHubPeer = "pixie-hub"
)

// pixieAgentPeer is the hub's end of one agent's link, in namespace plc of the hub's cluster.
func pixieAgentPeer(agent string) string { return "pixie-agent-" + agent }

func alertReceiverSecrets(hub *oramav1alpha1.ObservabilityHub) []oramav1alpha1.ObservabilitySecretKeyRef {
	if hub.Spec.Alerting == nil {
		return nil
	}
	var refs []oramav1alpha1.ObservabilitySecretKeyRef
	for _, receiver := range hub.Spec.Alerting.Receivers {
		switch {
		case receiver.Slack != nil:
			refs = append(refs, receiver.Slack.URLSecretRef)
		case receiver.Webhook != nil:
			refs = append(refs, receiver.Webhook.URLSecretRef)
		}
	}
	return refs
}

// observabilityClusters gives the managing cluster's view of the clusters it reaches.
type observabilityClusters struct {
	client.Client
	Scheme   *runtime.Scheme
	Clusters PeerClusterClients
}

func (o observabilityClusters) clusters() PeerClusterClients {
	if o.Clusters != nil {
		return o.Clusters
	}
	return managedPeerClusterClients{local: o.Client}
}

// purposeOf is a cluster's purpose, and whether the cluster exists (observability_purpose.go). An
// empty name is the managing cluster.
func (o observabilityClusters) purposeOf(ctx context.Context, name string) (string, bool, error) {
	return clusterPurpose(ctx, o.Client, name)
}

// ensurePixieNamespace creates the namespace with its Pod Security level.
func ensurePixieNamespace(ctx context.Context, c client.Client, name, level string) error {
	ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": level,
			"pod-security.kubernetes.io/warn":    "restricted",
			pixieManagedLabel:                    "namespace",
		},
	}}
	if err := c.Patch(ctx, ns, client.Apply, client.FieldOwner(pixieFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("the cluster refused namespace %s: %w", name, err)
	}
	return nil
}

// copySecret copies a Secret from the managing cluster to a cluster under the production-credential
// rules (managedset_secret_scope.go). keys maps source keys to target keys; nil copies every key
// and the type.
func (o observabilityClusters) copySecret(
	ctx context.Context,
	target client.Client,
	clusterRef string,
	source oramav1alpha1.ClusterSecretReference,
	namespace, name string,
	labels map[string]string,
	keys map[string]string,
) error {
	var secret corev1.Secret
	if err := o.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return &setError{"SecretMissing", fmt.Sprintf("Secret %s/%s does not exist on the managing cluster", source.Namespace, source.Name)}
		}
		return err
	}
	scope, err := operatorSecretScope(ctx, o.Client, clusterRef)
	if err != nil {
		return err
	}
	if err := scope.refuse(oramav1alpha1.SetSecret{Namespace: source.Namespace, Name: source.Name}, name, &secret); err != nil {
		return err
	}
	copied := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Type:       secret.Type,
		Data:       secret.Data,
	}
	if keys != nil {
		copied.Type = corev1.SecretTypeOpaque
		copied.Data = map[string][]byte{}
		for from, to := range keys {
			value, ok := secret.Data[from]
			if !ok || len(value) == 0 {
				return &setError{"SecretMissing", fmt.Sprintf("Secret %s/%s has no key %s", source.Namespace, source.Name, from)}
			}
			copied.Data[to] = value
		}
	}
	if err := target.Patch(ctx, copied, client.Apply, client.FieldOwner(pixieFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("the cluster refused Secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// alertRoutesOn copies the receivers to one cluster's namespace and returns the routes that name
// them.
func (o observabilityClusters) alertRoutesOn(ctx context.Context, target client.Client, alerting *oramav1alpha1.ObservabilityAlerting, clusterRef, namespace string, labels map[string]string) ([]oramav1alpha1.PixieAlertRoute, error) {
	if alerting == nil {
		return nil, nil
	}
	routes := make([]oramav1alpha1.PixieAlertRoute, 0, len(alerting.Receivers))
	for _, receiver := range alerting.Receivers {
		route := oramav1alpha1.PixieAlertRoute{Name: receiver.Name, Severities: receiver.Severities, SecretName: "alert-" + receiver.Name}
		var ref oramav1alpha1.ObservabilitySecretKeyRef
		switch {
		case receiver.Slack != nil:
			route.Kind, route.Channel, ref = "slack", receiver.Slack.Channel, receiver.Slack.URLSecretRef
		case receiver.Webhook != nil:
			route.Kind, ref = "webhook", receiver.Webhook.URLSecretRef
		default:
			continue
		}
		if len(route.Severities) == 0 {
			route.Severities = []string{"critical", "warning"}
		}
		if err := o.copySecret(ctx, target, clusterRef, oramav1alpha1.ClusterSecretReference{Namespace: ref.Namespace, Name: ref.Name},
			namespace, route.SecretName, labels, map[string]string{ref.Key: alertRouteSecretKey}); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, nil
}

// applyConcrete server-side applies a concrete resource onto a cluster, refusing one the managing
// cluster does not own.
func applyConcrete(ctx context.Context, target client.Client, object client.Object, ownerLabel, owner string) error {
	existing := object.DeepCopyObject().(client.Object)
	switch err := target.Get(ctx, client.ObjectKeyFromObject(object), existing); {
	case err == nil:
		if existing.GetLabels()[ownerLabel] != owner {
			return &setError{"NotOwned", fmt.Sprintf("%s/%s exists on the cluster and is not %s's", object.GetNamespace(), object.GetName(), owner)}
		}
	case apierrors.IsNotFound(err):
	case apimeta.IsNoMatchError(err):
		return &setError{"MissingCRD", "the cluster's operator does not have the observability CRDs yet: upgrade it first"}
	default:
		return err
	}
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ownerLabel] = owner
	object.SetLabels(labels)
	if err := target.Patch(ctx, object, client.Apply, client.FieldOwner(pixieFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("the cluster refused %s/%s: %w", object.GetNamespace(), object.GetName(), err)
	}
	return nil
}

func deleteRemote(ctx context.Context, target client.Client, object client.Object, ownerLabel, owner string) error {
	if err := target.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	if object.GetLabels()[ownerLabel] != owner {
		return nil
	}
	return client.IgnoreNotFound(target.Delete(ctx, object))
}

// deleteLabelledSecrets deletes the Secrets the managing cluster copied into a namespace for one
// hub or agent, found by its label.
func deleteLabelledSecrets(ctx context.Context, target client.Client, namespace, label, owner string) error {
	var secrets corev1.SecretList
	if err := target.List(ctx, &secrets, client.InNamespace(namespace), client.MatchingLabels{label: owner}); err != nil {
		return err
	}
	var errs []error
	for i := range secrets.Items {
		errs = append(errs, client.IgnoreNotFound(target.Delete(ctx, &secrets.Items[i])))
	}
	return errors.Join(errs...)
}

// observabilityStatusConditions sets the Ready condition from a phase.
func observabilityConditions(existing []metav1.Condition, phase, message string, generation int64) []metav1.Condition {
	conditions := append([]metav1.Condition(nil), existing...)
	apimeta.SetStatusCondition(&conditions, pixieCondition(phase, message, generation))
	return conditions
}

// refusalPhase maps a refusal to Refused (only an edit changes it) and anything else to Pending.
func refusalPhase(err error) (string, string, bool) {
	var refusal *setError
	if errors.As(err, &refusal) {
		switch refusal.reason {
		case "ProductionCredential", "NotOwned", "Purpose":
			return oramav1alpha1.ObservabilityPhaseRefused, refusal.message, true
		}
		return oramav1alpha1.ObservabilityPhasePending, refusal.message, true
	}
	return "", "", false
}

// ---- ObservabilityProvider ----

// ObservabilityProviderReconciler reports whether a provider's release can be installed.
type ObservabilityProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityproviders/status,verbs=get;update;patch

func (r *ObservabilityProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var provider oramav1alpha1.ObservabilityProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := oramav1alpha1.ObservabilityProviderStatus{ObservedGeneration: provider.Generation, Ready: true}
	if problems := pixieReleaseProblems(provider.Spec.Pixie); len(problems) > 0 {
		status.Ready, status.Message = false, strings.Join(problems, "; ")
	}
	var hubs oramav1alpha1.ObservabilityHubList
	if err := r.List(ctx, &hubs); err != nil {
		return ctrl.Result{}, err
	}
	var agents oramav1alpha1.ObservabilityAgentList
	if err := r.List(ctx, &agents); err != nil {
		return ctrl.Result{}, err
	}
	hubNames := map[string]bool{}
	for _, hub := range hubs.Items {
		if hub.Spec.ProviderRef == provider.Name {
			status.Hubs++
			hubNames[hub.Name] = true
		}
	}
	for _, agent := range agents.Items {
		if hubNames[agent.Spec.HubRef] {
			status.Agents++
		}
	}
	if apiequality.Semantic.DeepEqual(provider.Status, status) {
		return ctrl.Result{}, nil
	}
	provider.Status = status
	return ctrl.Result{}, r.Status().Update(ctx, &provider)
}

func (r *ObservabilityProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var providers oramav1alpha1.ObservabilityProviderList
		if err := r.List(ctx, &providers); err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(providers.Items))
		for i := range providers.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&providers.Items[i])})
		}
		return requests
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.ObservabilityProvider{}).
		Watches(&oramav1alpha1.ObservabilityHub{}, all).
		Watches(&oramav1alpha1.ObservabilityAgent{}, all).
		Complete(r)
}

// ---- ObservabilityHub ----

// ObservabilityHubReconciler writes a hub's concrete kind onto its cluster and reports it.
type ObservabilityHubReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clusters is injected for tests; nil reaches managed clusters by their kubeconfig.
	Clusters PeerClusterClients
}

// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityhubs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityhubs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityhubs/finalizers,verbs=update

func (r *ObservabilityHubReconciler) view() observabilityClusters {
	return observabilityClusters{Client: r.Client, Scheme: r.Scheme, Clusters: r.Clusters}
}

func (r *ObservabilityHubReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var hub oramav1alpha1.ObservabilityHub
	if err := r.Get(ctx, req.NamespacedName, &hub); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	view := r.view()
	if !hub.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&hub, observabilityFinalizer) {
			return ctrl.Result{}, nil
		}
		target, waiting, err := view.clusters().ClientFor(ctx, hub.Spec.ClusterRef, oramav1alpha1.PixieCloudNamespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		if waiting == "" {
			cloud := &oramav1alpha1.PixieCloud{ObjectMeta: metav1.ObjectMeta{Name: pixieConcreteName, Namespace: oramav1alpha1.PixieCloudNamespace}}
			if err := deleteRemote(ctx, target, cloud, observabilityHubLabel, hub.Name); err != nil {
				return ctrl.Result{}, err
			}
			if err := deleteLabelledSecrets(ctx, target, oramav1alpha1.PixieCloudNamespace, observabilityHubLabel, hub.Name); err != nil {
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(&hub, observabilityFinalizer)
		return ctrl.Result{}, r.Update(ctx, &hub)
	}
	if !controllerutil.ContainsFinalizer(&hub, observabilityFinalizer) {
		controllerutil.AddFinalizer(&hub, observabilityFinalizer)
		if err := r.Update(ctx, &hub); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	status, err := r.reconcile(ctx, &hub, view)
	if phase, message, ok := refusalPhase(err); ok {
		status.Phase, status.Message, err = phase, message, nil
	}
	if statusErr := r.setStatus(ctx, &hub, status); statusErr != nil && err == nil {
		err = statusErr
	}
	return ctrl.Result{RequeueAfter: peerLinkPoll}, err
}

func (r *ObservabilityHubReconciler) reconcile(ctx context.Context, hub *oramav1alpha1.ObservabilityHub, view observabilityClusters) (oramav1alpha1.ObservabilityHubStatus, error) {
	status := oramav1alpha1.ObservabilityHubStatus{}
	purpose, found, err := view.purposeOf(ctx, hub.Spec.ClusterRef)
	if err != nil {
		return status, err
	}
	status.Purpose = purpose
	if !found {
		return status, &setError{"Pending", "KubernetesCluster " + hub.Spec.ClusterRef + " does not exist"}
	}
	if refusal := hubPurposeRefusal(hub.Spec.ClusterRef, purpose); refusal != "" {
		return status, &setError{"Purpose", refusal}
	}
	release, err := providerRelease(ctx, r.Client, hub.Spec.ProviderRef)
	if err != nil {
		return status, err
	}
	target, waiting, err := view.clusters().ClientFor(ctx, hub.Spec.ClusterRef, oramav1alpha1.PixieCloudNamespace)
	if err != nil {
		return status, err
	}
	if waiting != "" {
		return status, &setError{"Pending", waiting}
	}
	agents, hubClusterHasAgent, err := r.acceptedAgents(ctx, hub, purpose)
	if err != nil {
		return status, err
	}
	labels := map[string]string{observabilityHubLabel: hub.Name}
	if err := ensurePixieNamespace(ctx, target, oramav1alpha1.PixieCloudNamespace, "baseline"); err != nil {
		return status, err
	}
	if err := view.copySecret(ctx, target, hub.Spec.ClusterRef, hub.Spec.ImagePullSecretRef, oramav1alpha1.PixieCloudNamespace, pixiePullSecret, labels, nil); err != nil {
		return status, err
	}
	// The hub's own cluster routes its alerts through its agent when it has one, so each alert is
	// delivered once.
	var routes []oramav1alpha1.PixieAlertRoute
	if !hubClusterHasAgent {
		if routes, err = view.alertRoutesOn(ctx, target, hub.Spec.Alerting, hub.Spec.ClusterRef, oramav1alpha1.PixieCloudNamespace, labels); err != nil {
			return status, err
		}
	}
	cloud := &oramav1alpha1.PixieCloud{
		TypeMeta:   metav1.TypeMeta{APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "PixieCloud"},
		ObjectMeta: metav1.ObjectMeta{Name: pixieConcreteName, Namespace: oramav1alpha1.PixieCloudNamespace},
		Spec: oramav1alpha1.PixieCloudSpec{
			Release:          *release,
			Hub:              hub.Name,
			Purpose:          purpose,
			Cluster:          hub.Spec.ClusterRef,
			MetricsRetention: hub.Spec.MetricsRetention,
			Domain:           hub.Spec.Domain,
			Organization:     hub.Spec.Organization,
			AdminEmail:       hub.Spec.AdminEmail,
			Users:            hub.Spec.Users,
			PullSecretName:   pixiePullSecret,
			Storage:          hub.Spec.Storage,
			Agents:           agents,
			AlertRoutes:      routes,
		},
	}
	if err := applyConcrete(ctx, target, cloud, observabilityHubLabel, hub.Name); err != nil {
		return status, err
	}
	status.ConcreteRef = "PixieCloud " + oramav1alpha1.PixieCloudNamespace + "/" + pixieConcreteName
	var live oramav1alpha1.PixieCloud
	if err := target.Get(ctx, client.ObjectKeyFromObject(cloud), &live); err != nil {
		return status, err
	}
	status.Phase = firstNonEmpty(live.Status.Phase, oramav1alpha1.ObservabilityPhasePending)
	status.Ready, status.Components, status.Message = live.Status.Ready, live.Status.Components, live.Status.Message
	if live.Status.ObservedGeneration < live.Generation && status.Phase == oramav1alpha1.ObservabilityPhaseReady {
		status.Phase, status.Message = oramav1alpha1.ObservabilityPhaseProgressing, "the hub's cluster has not reconciled the latest spec yet"
	}
	reported := map[string]oramav1alpha1.PixieCloudAgentStatus{}
	for _, agent := range live.Status.Agents {
		reported[agent.Name] = agent
	}
	for _, agent := range agents {
		status.Agents = append(status.Agents, oramav1alpha1.ObservabilityHubAgentStatus{
			Name: agent.Name, Cluster: agent.Cluster, Connected: reported[agent.Name].Connected, MetricsConnected: reported[agent.Name].MetricsConnected,
		})
	}
	return status, nil
}

// acceptedAgents is every agent that names this hub and whose cluster's purpose may report here,
// sorted, and whether one of them runs on the hub's own cluster.
func (r *ObservabilityHubReconciler) acceptedAgents(ctx context.Context, hub *oramav1alpha1.ObservabilityHub, hubPurpose string) ([]oramav1alpha1.PixieCloudAgent, bool, error) {
	var agents oramav1alpha1.ObservabilityAgentList
	if err := r.List(ctx, &agents); err != nil {
		return nil, false, err
	}
	view := r.view()
	var accepted []oramav1alpha1.PixieCloudAgent
	local := false
	for _, agent := range agents.Items {
		if agent.Spec.HubRef != hub.Name || !agent.DeletionTimestamp.IsZero() {
			continue
		}
		purpose, _, err := view.purposeOf(ctx, agent.Spec.ClusterRef)
		if err != nil {
			return nil, false, err
		}
		if agentPurposeRefusal(agent.Spec.ClusterRef, purpose, hub.Spec.ClusterRef, hubPurpose) != "" {
			continue
		}
		accepted = append(accepted, oramav1alpha1.PixieCloudAgent{Name: agent.Name, Cluster: agentClusterName(&agent, hub)})
		if agent.Spec.ClusterRef == hub.Spec.ClusterRef {
			local = true
		}
	}
	sort.Slice(accepted, func(i, j int) bool { return accepted[i].Name < accepted[j].Name })
	return accepted, local, nil
}

func providerRelease(ctx context.Context, c client.Client, name string) (*oramav1alpha1.PixieRelease, error) {
	var provider oramav1alpha1.ObservabilityProvider
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &setError{"Pending", "ObservabilityProvider " + name + " does not exist"}
		}
		return nil, err
	}
	if provider.Spec.Type != oramav1alpha1.ObservabilityProviderPixie {
		return nil, &setError{"Pending", fmt.Sprintf("ObservabilityProvider %s has type %s, which this operator does not provision", name, provider.Spec.Type)}
	}
	if problems := pixieReleaseProblems(provider.Spec.Pixie); len(problems) > 0 {
		return nil, &setError{"Pending", "ObservabilityProvider " + name + " is not installable: " + problems[0]}
	}
	return provider.Spec.Pixie.DeepCopy(), nil
}

func (r *ObservabilityHubReconciler) setStatus(ctx context.Context, hub *oramav1alpha1.ObservabilityHub, observed oramav1alpha1.ObservabilityHubStatus) error {
	desired := observed
	if desired.Phase == "" {
		desired.Phase = oramav1alpha1.ObservabilityPhasePending
	}
	desired.ObservedGeneration = hub.Generation
	desired.Conditions = observabilityConditions(hub.Status.Conditions, desired.Phase, desired.Message, hub.Generation)
	if apiequality.Semantic.DeepEqual(hub.Status, desired) {
		return nil
	}
	hub.Status = desired
	return r.Status().Update(ctx, hub)
}

func (r *ObservabilityHubReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.ObservabilityHub{}).
		Watches(&oramav1alpha1.ObservabilityAgent{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
			agent, ok := object.(*oramav1alpha1.ObservabilityAgent)
			if !ok {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: agent.Spec.HubRef}}}
		})).
		Watches(&oramav1alpha1.ObservabilityProvider{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			var hubs oramav1alpha1.ObservabilityHubList
			if err := r.List(ctx, &hubs); err != nil {
				return nil
			}
			var requests []reconcile.Request
			for i := range hubs.Items {
				if hubs.Items[i].Spec.ProviderRef == object.GetName() {
					requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&hubs.Items[i])})
				}
			}
			return requests
		})).
		Complete(r)
}

// ---- ObservabilityAgent ----

// ObservabilityAgentReconciler writes an agent's concrete kind onto its cluster, links it to its
// hub, and gives it its own deploy key.
type ObservabilityAgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clusters is injected for tests; nil reaches managed clusters by their kubeconfig.
	Clusters PeerClusterClients
}

// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=observabilityagents/finalizers,verbs=update

func (r *ObservabilityAgentReconciler) view() observabilityClusters {
	return observabilityClusters{Client: r.Client, Scheme: r.Scheme, Clusters: r.Clusters}
}

func (r *ObservabilityAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var agent oramav1alpha1.ObservabilityAgent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	view := r.view()
	if !agent.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&agent, observabilityFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.teardown(ctx, &agent, view); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&agent, observabilityFinalizer)
		return ctrl.Result{}, r.Update(ctx, &agent)
	}
	if !controllerutil.ContainsFinalizer(&agent, observabilityFinalizer) {
		controllerutil.AddFinalizer(&agent, observabilityFinalizer)
		if err := r.Update(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	status, err := r.reconcile(ctx, &agent, view)
	if phase, message, ok := refusalPhase(err); ok {
		status.Phase, status.Message, err = phase, message, nil
	}
	if status.Phase == oramav1alpha1.ObservabilityPhaseRefused {
		// A refused agent keeps nothing on its cluster: no Vizier, no link, no key.
		if teardownErr := r.teardown(ctx, &agent, view); teardownErr != nil && err == nil {
			err = teardownErr
		}
	}
	if statusErr := r.setStatus(ctx, &agent, status); statusErr != nil && err == nil {
		err = statusErr
	}
	return ctrl.Result{RequeueAfter: peerLinkPoll}, err
}

// agentPeerID is the identity both ends of an agent's link carry: the agent's own UID.
func agentPeerID(agent *oramav1alpha1.ObservabilityAgent) string { return string(agent.UID) }

func (r *ObservabilityAgentReconciler) reconcile(ctx context.Context, agent *oramav1alpha1.ObservabilityAgent, view observabilityClusters) (oramav1alpha1.ObservabilityAgentStatus, error) {
	status := oramav1alpha1.ObservabilityAgentStatus{}
	var hub oramav1alpha1.ObservabilityHub
	if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.HubRef}, &hub); err != nil {
		if apierrors.IsNotFound(err) {
			return status, &setError{"Pending", "ObservabilityHub " + agent.Spec.HubRef + " does not exist"}
		}
		return status, err
	}
	purpose, found, err := view.purposeOf(ctx, agent.Spec.ClusterRef)
	if err != nil {
		return status, err
	}
	hubPurpose, hubFound, err := view.purposeOf(ctx, hub.Spec.ClusterRef)
	if err != nil {
		return status, err
	}
	status.Purpose, status.HubPurpose = purpose, hubPurpose
	if !found || !hubFound {
		return status, &setError{"Pending", "the agent's or the hub's KubernetesCluster does not exist"}
	}
	if refusal := hubPurposeRefusal(hub.Spec.ClusterRef, hubPurpose); refusal != "" {
		return status, &setError{"Purpose", refusal}
	}
	if refusal := agentPurposeRefusal(agent.Spec.ClusterRef, purpose, hub.Spec.ClusterRef, hubPurpose); refusal != "" {
		return status, &setError{"Purpose", refusal}
	}
	agentCluster, waiting, err := view.clusters().ClientFor(ctx, agent.Spec.ClusterRef, oramav1alpha1.PixieVizierNamespace)
	if err != nil {
		return status, err
	}
	if waiting != "" {
		return status, &setError{"Pending", waiting}
	}
	labels := map[string]string{observabilityAgentLbl: agent.Name}
	if err := ensurePixieNamespace(ctx, agentCluster, oramav1alpha1.PixieVizierNamespace, "privileged"); err != nil {
		return status, err
	}
	if err := view.copySecret(ctx, agentCluster, agent.Spec.ClusterRef, agent.Spec.ImagePullSecretRef, oramav1alpha1.PixieVizierNamespace, pixiePullSecret, labels, nil); err != nil {
		return status, err
	}
	// The cluster's alert routing goes with the PixieVizier, which is written as soon as the
	// purposes match: a cluster pages before Pixie's release, its key or its hub exist.
	routes, err := view.alertRoutesOn(ctx, agentCluster, agentAlerting(&agent.Spec, purpose, hubPurpose, hub.Spec.Alerting), agent.Spec.ClusterRef, oramav1alpha1.PixieVizierNamespace, labels)
	if err != nil {
		return status, err
	}
	// What Vizier still waits for, in order; the first is the agent's message until it runs.
	var waitingFor []string
	var release *oramav1alpha1.PixieRelease
	if release, err = providerRelease(ctx, r.Client, hub.Spec.ProviderRef); err != nil {
		var refusal *setError
		if !errors.As(err, &refusal) {
			return status, err
		}
		release, waitingFor = nil, append(waitingFor, refusal.message)
	}
	cloudAddress := fmt.Sprintf("%s.%s.svc:%d", pixieVzconnService, oramav1alpha1.PixieCloudNamespace, pixieVzconnPort)
	status.Link = "local"
	if agent.Spec.ClusterRef != hub.Spec.ClusterRef {
		cloudAddress = fmt.Sprintf("%s.%s.svc:%d", pixieAgentImportService, oramav1alpha1.PixieVizierNamespace, pixieVzconnPort)
	}
	var cloud oramav1alpha1.PixieCloud
	hubCluster, hubWaiting, err := view.clusters().ClientFor(ctx, hub.Spec.ClusterRef, oramav1alpha1.PixieCloudNamespace)
	switch {
	case err != nil:
		return status, err
	case hubWaiting != "":
		waitingFor = append(waitingFor, "the hub's cluster: "+hubWaiting)
	default:
		if agent.Spec.ClusterRef != hub.Spec.ClusterRef {
			link, err := r.reconcileLink(ctx, agent, &hub, hubCluster, agentCluster)
			if err != nil {
				return status, err
			}
			status.Link = "PeerLink " + link
		}
		// The hub issues this cluster's key once it lists the agent; nothing else holds it.
		message, err := r.deliverHubCredentials(ctx, agent, hubCluster, agentCluster, labels, &cloud)
		if err != nil {
			return status, err
		}
		if message != "" {
			waitingFor = append(waitingFor, message)
		}
	}
	vizier := &oramav1alpha1.PixieVizier{
		TypeMeta:   metav1.TypeMeta{APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "PixieVizier"},
		ObjectMeta: metav1.ObjectMeta{Name: pixieConcreteName, Namespace: oramav1alpha1.PixieVizierNamespace},
		Spec: oramav1alpha1.PixieVizierSpec{
			Release:             release,
			Agent:               agent.Name,
			Purpose:             purpose,
			ClusterName:         agentClusterName(agent, &hub),
			CloudAddress:        cloudAddress,
			CloudCASecretName:   pixieCloudCACopy,
			DeployKeySecretName: vizierDeploySecret,
			PullSecretName:      pixiePullSecret,
			NodeMemory:          quantityOr(agent.Spec.NodeMemory, "2Gi"),
			AlertRoutes:         routes,
		},
	}
	if err := applyConcrete(ctx, agentCluster, vizier, observabilityAgentLbl, agent.Name); err != nil {
		return status, err
	}
	status.ConcreteRef = "PixieVizier " + oramav1alpha1.PixieVizierNamespace + "/" + pixieConcreteName
	var live oramav1alpha1.PixieVizier
	if err := agentCluster.Get(ctx, client.ObjectKeyFromObject(vizier), &live); err != nil {
		return status, err
	}
	status.Phase = firstNonEmpty(live.Status.Phase, oramav1alpha1.ObservabilityPhasePending)
	status.Ready, status.Components, status.Message = live.Status.Ready, live.Status.Components, live.Status.Message
	// The hub reads this cluster's metrics independently of Pixie, so this holds while Vizier waits.
	for _, a := range cloud.Status.Agents {
		if a.Name == agent.Name {
			status.Connected, status.MetricsConnected = a.Connected, a.MetricsConnected
		}
	}
	if len(waitingFor) > 0 {
		status.Phase, status.Message = oramav1alpha1.ObservabilityPhasePending, "waiting for "+strings.TrimPrefix(waitingFor[0], "waiting for ")+"; alert routing and metrics federation are written"
		return status, nil
	}
	if status.Phase == oramav1alpha1.ObservabilityPhaseReady && !status.Connected {
		status.Phase, status.Message = oramav1alpha1.ObservabilityPhaseProgressing, "the agents run; the hub has not seen the cluster heartbeat yet"
	}
	return status, nil
}

// deliverHubCredentials copies, from the hub's cluster to the agent's, the deploy key the hub
// issued for this cluster and the hub's public CA. It says what is not there yet.
func (r *ObservabilityAgentReconciler) deliverHubCredentials(
	ctx context.Context,
	agent *oramav1alpha1.ObservabilityAgent,
	hubCluster, agentCluster client.Client,
	labels map[string]string,
	cloud *oramav1alpha1.PixieCloud,
) (string, error) {
	if err := hubCluster.Get(ctx, client.ObjectKey{Namespace: oramav1alpha1.PixieCloudNamespace, Name: pixieConcreteName}, cloud); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return "the hub's PixieCloud", nil
		}
		return "", err
	}
	var deployKey corev1.Secret
	if err := hubCluster.Get(ctx, client.ObjectKey{Namespace: oramav1alpha1.PixieCloudNamespace, Name: pixieDeployKeyPrefix + agent.Name}, &deployKey); err != nil {
		if apierrors.IsNotFound(err) {
			return "the hub to issue this cluster's deploy key", nil
		}
		return "", err
	}
	if cloud.Status.CACertificate == "" {
		return "the hub's CA", nil
	}
	for _, secret := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: vizierDeploySecret}, Data: map[string][]byte{"deploy-key": deployKey.Data["deploy-key"]}},
		{ObjectMeta: metav1.ObjectMeta{Name: pixieCloudCACopy}, Data: map[string][]byte{"ca.crt": []byte(cloud.Status.CACertificate)}},
	} {
		secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
		secret.Namespace, secret.Labels, secret.Type = oramav1alpha1.PixieVizierNamespace, labels, corev1.SecretTypeOpaque
		if err := agentCluster.Patch(ctx, secret, client.Apply, client.FieldOwner(pixieFieldOwner), client.ForceOwnership); err != nil {
			return "", fmt.Errorf("the cluster refused Secret %s: %w", secret.Name, err)
		}
	}
	return "", nil
}

// reconcileLink makes the agent's PeerConnection pair and the PeerLink that gives both ends their
// credentials: the hub's end exports only vzconn, the agent's end imports only it.
func (r *ObservabilityAgentReconciler) reconcileLink(ctx context.Context, agent *oramav1alpha1.ObservabilityAgent, hub *oramav1alpha1.ObservabilityHub, hubCluster, agentCluster client.Client) (string, error) {
	peerID := agentPeerID(agent)
	hubEnd := &oramav1alpha1.PeerConnection{
		TypeMeta:   metav1.TypeMeta{APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "PeerConnection"},
		ObjectMeta: metav1.ObjectMeta{Name: pixieAgentPeer(agent.Name), Namespace: oramav1alpha1.PixieCloudNamespace},
		Spec: oramav1alpha1.PeerConnectionSpec{
			PeerID:  peerID,
			Exports: []oramav1alpha1.PeerExport{{Name: pixieAgentImport, TargetKind: oramav1alpha1.PeerTargetKubernetesService, ServiceName: pixieVzconnService, Port: pixieVzconnPort}},
			// The agent cluster's metrics, which the hub Prometheus federates (observability_metrics.go).
			Imports:        []oramav1alpha1.PeerImport{{Name: agentMetricsImport(agent.Name), Port: metricsFederationPort}},
			PullSecretName: pixiePullSecret,
			Desired:        oramav1alpha1.ConnectionDesiredPresent,
			Generation:     1,
		},
	}
	agentEnd := &oramav1alpha1.PeerConnection{
		TypeMeta:   metav1.TypeMeta{APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "PeerConnection"},
		ObjectMeta: metav1.ObjectMeta{Name: pixieHubPeer, Namespace: oramav1alpha1.PixieVizierNamespace},
		Spec: oramav1alpha1.PeerConnectionSpec{
			PeerID:  peerID,
			Imports: []oramav1alpha1.PeerImport{{Name: pixieAgentImport, Port: pixieVzconnPort}},
			// Only the read-only federation proxy, never the Prometheus itself.
			Exports:        []oramav1alpha1.PeerExport{{Name: agentMetricsImport(agent.Name), TargetKind: oramav1alpha1.PeerTargetKubernetesService, ServiceName: metricsFederationName, Port: metricsFederationPort}},
			PullSecretName: pixiePullSecret,
			Desired:        oramav1alpha1.ConnectionDesiredPresent,
			Generation:     1,
		},
	}
	// The hub makes its namespace only once its release exists; the link's hub end goes there first,
	// so an agent pages before Pixie's release instead of failing on a missing namespace.
	if err := ensurePixieNamespace(ctx, hubCluster, oramav1alpha1.PixieCloudNamespace, "baseline"); err != nil {
		return "", err
	}
	if err := applyConcrete(ctx, hubCluster, hubEnd, observabilityAgentLbl, agent.Name); err != nil {
		return "", err
	}
	if err := applyConcrete(ctx, agentCluster, agentEnd, observabilityAgentLbl, agent.Name); err != nil {
		return "", err
	}
	link := &oramav1alpha1.PeerLink{ObjectMeta: metav1.ObjectMeta{Name: "pixie-" + agent.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, link, func() error {
		if link.ResourceVersion != "" && !metav1.IsControlledBy(link, agent) {
			return &setError{"NotOwned", "PeerLink " + link.Name + " exists and is not this agent's"}
		}
		link.Spec = oramav1alpha1.PeerLinkSpec{
			PeerID: peerID,
			Ends: []oramav1alpha1.PeerLinkEnd{
				{Name: "hub", ClusterRef: hub.Spec.ClusterRef, Namespace: oramav1alpha1.PixieCloudNamespace, PeerConnection: hubEnd.Name},
				{Name: "agent", ClusterRef: agent.Spec.ClusterRef, Namespace: oramav1alpha1.PixieVizierNamespace, PeerConnection: agentEnd.Name},
			},
		}
		return controllerutil.SetControllerReference(agent, link, r.Scheme)
	})
	return link.Name, err
}

// teardown removes what the agent put on its cluster and the hub's, and its link. The hub revokes
// the deploy key once the agent no longer names it.
func (r *ObservabilityAgentReconciler) teardown(ctx context.Context, agent *oramav1alpha1.ObservabilityAgent, view observabilityClusters) error {
	var errs []error
	if agentCluster, waiting, err := view.clusters().ClientFor(ctx, agent.Spec.ClusterRef, oramav1alpha1.PixieVizierNamespace); err != nil {
		errs = append(errs, err)
	} else if waiting == "" {
		for _, object := range []client.Object{
			&oramav1alpha1.PixieVizier{ObjectMeta: metav1.ObjectMeta{Name: pixieConcreteName, Namespace: oramav1alpha1.PixieVizierNamespace}},
			&oramav1alpha1.PeerConnection{ObjectMeta: metav1.ObjectMeta{Name: pixieHubPeer, Namespace: oramav1alpha1.PixieVizierNamespace}},
		} {
			errs = append(errs, deleteRemote(ctx, agentCluster, object, observabilityAgentLbl, agent.Name))
		}
		// Every credential copied there: the deploy key, the hub's CA, the pull Secret and the
		// receivers. Copies carry the agent's label, never an owner on another cluster.
		errs = append(errs, deleteLabelledSecrets(ctx, agentCluster, oramav1alpha1.PixieVizierNamespace, observabilityAgentLbl, agent.Name))
	}
	var hub oramav1alpha1.ObservabilityHub
	if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.HubRef}, &hub); err == nil {
		if hubCluster, waiting, err := view.clusters().ClientFor(ctx, hub.Spec.ClusterRef, oramav1alpha1.PixieCloudNamespace); err != nil {
			errs = append(errs, err)
		} else if waiting == "" {
			peer := &oramav1alpha1.PeerConnection{ObjectMeta: metav1.ObjectMeta{Name: pixieAgentPeer(agent.Name), Namespace: oramav1alpha1.PixieCloudNamespace}}
			errs = append(errs, deleteRemote(ctx, hubCluster, peer, observabilityAgentLbl, agent.Name))
		}
	} else if !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	link := &oramav1alpha1.PeerLink{ObjectMeta: metav1.ObjectMeta{Name: "pixie-" + agent.Name}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(link), link); err == nil && metav1.IsControlledBy(link, agent) {
		errs = append(errs, client.IgnoreNotFound(r.Delete(ctx, link)))
	} else if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *ObservabilityAgentReconciler) setStatus(ctx context.Context, agent *oramav1alpha1.ObservabilityAgent, observed oramav1alpha1.ObservabilityAgentStatus) error {
	desired := observed
	if desired.Phase == "" {
		desired.Phase = oramav1alpha1.ObservabilityPhasePending
	}
	desired.ObservedGeneration = agent.Generation
	desired.Conditions = observabilityConditions(agent.Status.Conditions, desired.Phase, desired.Message, agent.Generation)
	if apiequality.Semantic.DeepEqual(agent.Status, desired) {
		return nil
	}
	agent.Status = desired
	return r.Status().Update(ctx, agent)
}

func (r *ObservabilityAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.ObservabilityAgent{}).
		Owns(&oramav1alpha1.PeerLink{}).
		Watches(&oramav1alpha1.ObservabilityHub{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			var agents oramav1alpha1.ObservabilityAgentList
			if err := r.List(ctx, &agents); err != nil {
				return nil
			}
			var requests []reconcile.Request
			for i := range agents.Items {
				if agents.Items[i].Spec.HubRef == object.GetName() {
					requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents.Items[i])})
				}
			}
			return requests
		})).
		Complete(r)
}
