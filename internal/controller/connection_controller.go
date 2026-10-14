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
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	connectionFinalizer          = "orama.cloud/connection-policy"
	connectionAgentIDLabel       = "orama.cloud/agent-id"
	connectionTunnelIDLabel      = "orama.cloud/tunnel-grant-id"
	connectionPendingRequeue     = 2 * time.Second
	connectionReadyConditionType = "Ready"
)

type connectionPendingError struct {
	message string
}

func (e connectionPendingError) Error() string {
	return e.message
}

// ConnectionReconciler writes both halves of a declared path into the workspace's
// NetworkSandbox. The sandbox remains the owner and renderer of every NetworkPolicy.
type ConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the Lake's admin Secret uncached for the workspace secret writer.
	APIReader client.Reader
	// Roles manages per-connection datastore roles (operator#469); nil uses pgx.
	Roles PostgresRoleManager
	// Secrets writes issued credentials into the workspace's secret store; nil calls Lake.
	Secrets WorkspaceSecretWriter
}

// +kubebuilder:rbac:groups=orama.cloud,resources=connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=connections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=connections/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=networksandboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakeapps;lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *ConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var connection oramav1alpha1.Connection
	if err := r.Get(ctx, req.NamespacedName, &connection); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !connection.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&connection, connectionFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.removeAllowances(ctx, &connection); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.revokeCredential(ctx, &connection); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&connection, connectionFinalizer)
		return ctrl.Result{}, r.Update(ctx, &connection)
	}
	if !controllerutil.ContainsFinalizer(&connection, connectionFinalizer) {
		controllerutil.AddFinalizer(&connection, connectionFinalizer)
		if err := r.Update(ctx, &connection); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch connection.Spec.Desired {
	case oramav1alpha1.ConnectionDesiredAbsent:
		// The path closes first, then the role is dropped and its sessions ended, both in this
		// reconcile, before Deleted is reported.
		if err := r.removeAllowances(ctx, &connection); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.revokeCredential(ctx, &connection); err != nil {
			return ctrl.Result{}, err
		}
		message := "connection policies deleted"
		if connection.Spec.CredentialScope != nil || connection.Status.CredentialRole != "" {
			message = "connection policies and credential role deleted"
		}
		return ctrl.Result{}, r.updateStatusWith(
			ctx,
			&connection,
			oramav1alpha1.ConnectionPhaseDeleted,
			message,
			metav1.ConditionFalse,
			"Deleted",
			connectionCredentialState{},
		)
	case oramav1alpha1.ConnectionDesiredPresent:
		return r.reconcilePresent(ctx, &connection)
	default:
		message := "desired must be present or absent"
		return ctrl.Result{}, r.updateStatus(
			ctx,
			&connection,
			oramav1alpha1.ConnectionPhaseFailed,
			message,
			metav1.ConditionFalse,
			"Invalid",
		)
	}
}

func (r *ConnectionReconciler) reconcilePresent(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
) (ctrl.Result, error) {
	sandbox, err := r.workspaceSandbox(ctx, connection)
	if err != nil {
		var pending connectionPendingError
		if errors.As(err, &pending) {
			statusErr := r.updateStatus(
				ctx,
				connection,
				oramav1alpha1.ConnectionPhasePending,
				pending.Error(),
				metav1.ConditionFalse,
				"WaitingForSandbox",
			)
			return ctrl.Result{RequeueAfter: connectionPendingRequeue}, statusErr
		}
		return ctrl.Result{}, r.fail(ctx, connection, err)
	}
	source, err := r.workloadSelector(ctx, connection, connection.Spec.SourceKind, connection.Spec.SourceID)
	if err != nil {
		return r.selectorResult(ctx, connection, "SourceNotReady", err)
	}
	target, err := r.workloadSelector(ctx, connection, connection.Spec.TargetKind, connection.Spec.TargetID)
	if err != nil {
		return r.selectorResult(ctx, connection, "TargetNotReady", err)
	}
	ports, err := r.connectionPorts(ctx, connection, target)
	if err != nil {
		return r.selectorResult(ctx, connection, "TargetPortsNotReady", err)
	}
	allowances := connectionAllowances(connection, source, target, ports)
	sandbox, err = r.replaceAllowances(ctx, sandbox, connection, allowances)
	if err != nil {
		return ctrl.Result{}, err
	}
	if _, _, err := reconcileNetworkSandboxPolicies(ctx, r.Client, r.Scheme, sandbox); err != nil {
		return ctrl.Result{}, err
	}
	credential, err := r.reconcileCredential(ctx, connection)
	if err != nil {
		var pending connectionPendingError
		if errors.As(err, &pending) {
			statusErr := r.updateStatusWith(
				ctx,
				connection,
				oramav1alpha1.ConnectionPhasePending,
				pending.Error(),
				metav1.ConditionFalse,
				"CredentialNotReady",
				credential,
			)
			return ctrl.Result{RequeueAfter: connectionCredentialRequeue}, statusErr
		}
		return ctrl.Result{}, r.updateStatusWith(
			ctx,
			connection,
			oramav1alpha1.ConnectionPhaseFailed,
			err.Error(),
			metav1.ConditionFalse,
			"Invalid",
			credential,
		)
	}
	message := "connection policies ready"
	if credential.Secret != "" {
		message = "connection policies and scoped credential ready"
	}
	return ctrl.Result{}, r.updateStatusWith(
		ctx,
		connection,
		oramav1alpha1.ConnectionPhaseReady,
		message,
		metav1.ConditionTrue,
		"PoliciesReady",
		credential,
	)
}

func (r *ConnectionReconciler) selectorResult(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
	reason string,
	err error,
) (ctrl.Result, error) {
	var pending connectionPendingError
	if errors.As(err, &pending) {
		statusErr := r.updateStatus(
			ctx,
			connection,
			oramav1alpha1.ConnectionPhasePending,
			pending.Error(),
			metav1.ConditionFalse,
			reason,
		)
		return ctrl.Result{RequeueAfter: connectionPendingRequeue}, statusErr
	}
	return ctrl.Result{}, r.fail(ctx, connection, err)
}

func (r *ConnectionReconciler) fail(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
	err error,
) error {
	return r.updateStatus(
		ctx,
		connection,
		oramav1alpha1.ConnectionPhaseFailed,
		err.Error(),
		metav1.ConditionFalse,
		"Invalid",
	)
}

func (r *ConnectionReconciler) workspaceSandbox(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
) (*oramav1alpha1.NetworkSandbox, error) {
	return lakeWorkspaceSandbox(ctx, r.Client, connection.Spec.LakeRef, connection, "Connection")
}

// lakeWorkspaceSandbox returns the workspace NetworkSandbox that a Lake-controlled resource adds
// its allowances to, after checking that the Lake controls both.
func lakeWorkspaceSandbox(
	ctx context.Context,
	reader client.Reader,
	lakeRef string,
	owned client.Object,
	kind string,
) (*oramav1alpha1.NetworkSandbox, error) {
	var lake oramav1alpha1.Lake
	if err := reader.Get(ctx, client.ObjectKey{Name: lakeRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("Lake %q does not exist", lakeRef)
		}
		return nil, err
	}
	controlled := metav1.IsControlledBy(owned, &lake)
	if connection, ok := owned.(*oramav1alpha1.Connection); ok && !controlled {
		// A Connection generated for a tunnel is controlled by its ZettaTunnel or CloudflareTunnel.
		controlled = connectionControlledByLakeTunnel(ctx, reader, connection)
	}
	if !controlled {
		return nil, fmt.Errorf("%s is not controlled by Lake %q", kind, lake.Name)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	key := client.ObjectKey{Namespace: owned.GetNamespace(), Name: networkSandboxName}
	if err := reader.Get(ctx, key, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, connectionPendingError{message: "waiting for the workspace NetworkSandbox"}
		}
		return nil, err
	}
	if !metav1.IsControlledBy(&sandbox, &lake) {
		return nil, fmt.Errorf("NetworkSandbox %s/%s is not controlled by Lake %q", sandbox.Namespace, sandbox.Name, lake.Name)
	}
	return &sandbox, nil
}

func (r *ConnectionReconciler) workloadSelector(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
	kind string,
	id string,
) (metav1.LabelSelector, error) {
	return lakeWorkloadSelector(ctx, r.Client, connection.Spec.LakeRef, connection.Namespace, kind, id)
}

// lakeWorkloadSelector maps a Lake workload id to the operator-owned labels of its pods in one
// workspace namespace. Connections and EgressRules share it.
func lakeWorkloadSelector(
	ctx context.Context,
	reader client.Reader,
	lakeRef string,
	namespace string,
	kind string,
	id string,
) (metav1.LabelSelector, error) {
	switch kind {
	case oramav1alpha1.ConnectionSourceService:
		target, err := resolveServiceTarget(ctx, reader, lakeRef, namespace, id)
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return metav1.LabelSelector{MatchLabels: target.Pods}, nil
	case oramav1alpha1.ConnectionSourceAgent:
		return metav1.LabelSelector{MatchLabels: map[string]string{connectionAgentIDLabel: id}}, nil
	case oramav1alpha1.ConnectionSourceTunnel:
		return metav1.LabelSelector{MatchLabels: map[string]string{connectionTunnelIDLabel: id}}, nil
	case oramav1alpha1.ConnectionSourceApp:
		var lake oramav1alpha1.Lake
		if err := reader.Get(ctx, client.ObjectKey{Name: lakeRef}, &lake); err != nil {
			return metav1.LabelSelector{}, err
		}
		var apps oramav1alpha1.LakeAppList
		if err := reader.List(
			ctx,
			&apps,
			client.InNamespace(lakeNamespace(&lake)),
			client.MatchingLabels{lakeAppRowIDLabel: id},
		); err != nil {
			return metav1.LabelSelector{}, err
		}
		for i := range apps.Items {
			app := &apps.Items[i]
			if app.Spec.Sync == nil || app.Spec.Sync.ID != id || app.Spec.Sync.WorkspaceNamespace != namespace {
				continue
			}
			return metav1.LabelSelector{MatchLabels: map[string]string{
				lakeAppIdentityLabel: lakeAppLabels(app)[lakeAppIdentityLabel],
			}}, nil
		}
		return metav1.LabelSelector{}, connectionPendingError{message: fmt.Sprintf("waiting for app %s in workspace %s", id, namespace)}
	default:
		return metav1.LabelSelector{}, fmt.Errorf("unsupported workload kind %q", kind)
	}
}

func (r *ConnectionReconciler) connectionPorts(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
	target metav1.LabelSelector,
) ([]networkingv1.NetworkPolicyPort, error) {
	if len(connection.Spec.Ports) > 0 {
		values := append([]int32(nil), connection.Spec.Ports...)
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		values = sortedUniqueInt32(values)
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(values))
		for _, value := range values {
			ports = append(ports, networkingv1.NetworkPolicyPort{
				Protocol: protocolPtr(corev1.ProtocolTCP),
				Port:     intstrPtr(intstr.FromInt32(value)),
			})
		}
		return ports, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&target)
	if err != nil {
		return nil, err
	}
	services, err := targetServices(ctx, r.Client, connection.Namespace, target, selector)
	if err != nil {
		return nil, err
	}
	type keyedPort struct {
		key  string
		port networkingv1.NetworkPolicyPort
	}
	byKey := map[string]networkingv1.NetworkPolicyPort{}
	for i := range services {
		for _, servicePort := range services[i].Spec.Ports {
			protocol := servicePort.Protocol
			if protocol == "" {
				protocol = corev1.ProtocolTCP
			}
			targetPort := servicePort.TargetPort
			if targetPort.Type == intstr.Int && targetPort.IntVal == 0 {
				targetPort = intstr.FromInt32(servicePort.Port)
			}
			key := string(protocol) + "/" + targetPort.String()
			byKey[key] = networkingv1.NetworkPolicyPort{Protocol: protocolPtr(protocol), Port: intstrPtr(targetPort)}
		}
	}
	if len(byKey) == 0 {
		return nil, connectionPendingError{message: "waiting for the target's Kubernetes Service ports"}
	}
	keyed := make([]keyedPort, 0, len(byKey))
	for key, port := range byKey {
		keyed = append(keyed, keyedPort{key: key, port: port})
	}
	sort.Slice(keyed, func(i, j int) bool { return keyed[i].key < keyed[j].key })
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(keyed))
	for _, item := range keyed {
		ports = append(ports, item.port)
	}
	return ports, nil
}

func sortedUniqueInt32(values []int32) []int32 {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func connectionPolicyNames(connection *oramav1alpha1.Connection) (string, string) {
	return connection.Name + "-egress", connection.Name + "-ingress"
}

func connectionAllowances(
	connection *oramav1alpha1.Connection,
	source metav1.LabelSelector,
	target metav1.LabelSelector,
	ports []networkingv1.NetworkPolicyPort,
) []oramav1alpha1.NetworkSandboxPolicy {
	egressName, ingressName := connectionPolicyNames(connection)
	comment := fmt.Sprintf(
		"Lake Connection %s permits %s %s to reach %s %s on only the declared target ports.",
		connection.Spec.ConnectionID,
		connection.Spec.SourceKind,
		connection.Spec.SourceID,
		connection.Spec.TargetKind,
		connection.Spec.TargetID,
	)
	return []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(comment, networkPolicy(
			egressName,
			source,
			[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			nil,
			[]networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: target.DeepCopy()}},
				Ports: append([]networkingv1.NetworkPolicyPort(nil), ports...),
			}},
		)),
		sandboxAllowance(comment, networkPolicy(
			ingressName,
			target,
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			[]networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{PodSelector: source.DeepCopy()}},
				Ports: append([]networkingv1.NetworkPolicyPort(nil), ports...),
			}},
			nil,
		)),
	}
}

func (r *ConnectionReconciler) replaceAllowances(
	ctx context.Context,
	sandbox *oramav1alpha1.NetworkSandbox,
	connection *oramav1alpha1.Connection,
	allowances []oramav1alpha1.NetworkSandboxPolicy,
) (*oramav1alpha1.NetworkSandbox, error) {
	egressName, ingressName := connectionPolicyNames(connection)
	kept := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(sandbox.Spec.Policies)+len(allowances))
	for _, policy := range sandbox.Spec.Policies {
		if policy.Name != egressName && policy.Name != ingressName {
			kept = append(kept, policy)
		}
	}
	kept = append(kept, allowances...)
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	if apiequality.Semantic.DeepEqual(sandbox.Spec.Policies, kept) {
		return sandbox, nil
	}
	sandbox.Spec.Policies = kept
	if err := r.Update(ctx, sandbox); err != nil {
		return nil, fmt.Errorf("update NetworkSandbox connection allowances: %w", err)
	}
	return sandbox, nil
}

func (r *ConnectionReconciler) removeAllowances(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
) error {
	var sandbox oramav1alpha1.NetworkSandbox
	key := client.ObjectKey{Namespace: connection.Namespace, Name: networkSandboxName}
	if err := r.Get(ctx, key, &sandbox); err != nil {
		return client.IgnoreNotFound(err)
	}
	updated, err := r.replaceAllowances(ctx, &sandbox, connection, nil)
	if err != nil {
		return err
	}
	_, _, err = reconcileNetworkSandboxPolicies(ctx, r.Client, r.Scheme, updated)
	return err
}

func (r *ConnectionReconciler) updateStatus(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
	phase string,
	message string,
	conditionStatus metav1.ConditionStatus,
	reason string,
) error {
	return r.updateStatusWith(ctx, connection, phase, message, conditionStatus, reason, credentialStateOf(connection))
}

// updateStatusWith writes the status with the given credential state.
func (r *ConnectionReconciler) updateStatusWith(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
	phase string,
	message string,
	conditionStatus metav1.ConditionStatus,
	reason string,
	credential connectionCredentialState,
) error {
	desired := oramav1alpha1.ConnectionStatus{
		Phase:                phase,
		Message:              message,
		ObservedGeneration:   connection.Spec.Generation,
		CredentialSecret:     credential.Secret,
		CredentialGeneration: credential.Generation,
		CredentialRole:       credential.Role,
		Conditions:           append([]metav1.Condition(nil), connection.Status.Conditions...),
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               connectionReadyConditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: connection.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if apiequality.Semantic.DeepEqual(connection.Status, desired) {
		return nil
	}
	connection.Status = desired
	return r.Status().Update(ctx, connection)
}

func (r *ConnectionReconciler) connectionsInNamespace(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	var connections oramav1alpha1.ConnectionList
	if err := r.List(ctx, &connections, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(connections.Items))
	for i := range connections.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&connections.Items[i])})
	}
	return requests
}

func (r *ConnectionReconciler) connectionsForApp(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	app, ok := object.(*oramav1alpha1.LakeApp)
	if !ok || app.Spec.Sync == nil || strings.TrimSpace(app.Spec.Sync.WorkspaceNamespace) == "" {
		return nil
	}
	app.Namespace = app.Spec.Sync.WorkspaceNamespace
	return r.connectionsInNamespace(ctx, app)
}

func (r *ConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Connection{}).
		Watches(&oramav1alpha1.NetworkSandbox{}, handler.EnqueueRequestsFromMapFunc(r.connectionsInNamespace)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.connectionsInNamespace)).
		Watches(&oramav1alpha1.LakeApp{}, handler.EnqueueRequestsFromMapFunc(r.connectionsForApp)).
		Complete(r)
}
