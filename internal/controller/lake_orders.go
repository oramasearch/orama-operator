/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Provisioning paid lake.live orders (operator#634, lake#831).
//
// lake records each order in its control-plane Postgres (`cloud_orders`). A LakeOrderSource on
// the management cluster names the LakeSet whose Lakes take orders. On every pass the operator
// reads each member's `cloud_orders` through the member's cluster (lake_orders_pgx.go), claims
// every `paid` order atomically, and provisions it by creating resources on the cluster it runs
// on, the same kinds a person writes in config/clusters/:
//
//   - private_warehouse: a LakeSet with one member, a new Lake on the set's existing cluster in
//     the ordered region;
//   - dedicated_cluster: a KubernetesCluster in the ordered region made like the template
//     cluster, a copy of each of that cluster's ClusterHelmReleases and of its OramaOperator,
//     and the LakeSet with one member on the new cluster.
//
// The existing controllers do the rest. When the order's LakeSet is Ready the order moves to
// `ready` with its host; an order that cannot be provisioned as written moves to `failed` with
// the reason. Every created resource carries the order id as a label and the source as an
// annotation, and is described entirely by the order's row and the source, so a recreated
// management cluster creates the same resources again from the same rows. Nothing is ever
// deleted here.

const (
	cloudOrderPaid         = "paid"
	cloudOrderProvisioning = "provisioning"
	cloudOrderReady        = "ready"
	cloudOrderFailed       = "failed"

	offerPrivateWarehouse = "private_warehouse"
	offerDedicatedCluster = "dedicated_cluster"

	// orderIDLabel carries the order's id on every resource made for it; orderSourceLabel the
	// LakeOrderSource that made it. orderOwnerAnnotation says, for a person reading the
	// resource, where it came from and that it is not in config/clusters/.
	orderIDLabel         = "orama.cloud/order-id"
	orderSourceLabel     = "orama.cloud/order-source"
	orderOwnerAnnotation = "orama.cloud/order-owner"
	// orderClusterTag is the order id among a dedicated cluster's cloud tags.
	orderClusterTag = "order-id"

	// cloudOrderClaimantLimit is lake's CHECK on cloud_orders.claimed_by.
	cloudOrderClaimantLimit = 255

	defaultOrderPollInterval = 30 * time.Second
	orderPassTimeout         = 2 * time.Minute
)

// LakeOrderSourceReconciler provisions the paid orders of a LakeSet's Lakes.
type LakeOrderSourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Orders opens a member's control Postgres. Nil reaches it through the member's cluster
	// (openCloudOrderStoreThroughCluster); tests substitute a fake store.
	Orders CloudOrderStoreOpener
}

// orderClaimant is what the operator writes in claimed_by. It names the source, not the cluster
// or the pod, so the same source on a recreated management cluster resumes the orders it held.
func orderClaimant(source *oramav1alpha1.LakeOrderSource) string {
	return "orama-operator/LakeOrderSource/" + source.Name
}

// orderResourceName is the name of every resource made for an order: `order-` and the first
// twelve hex digits of its id. It is a valid name for a LakeSet member, a namespace suffix, a
// KubernetesCluster and an AKS cluster.
func orderResourceName(id string) string {
	hex := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "order-" + hex
}

// orderOutcome is what one pass found for one order.
type orderOutcome struct {
	host    string // set when the order is ready
	failure string // set when the order cannot be provisioned as written
	waiting string // what the order is waiting for
	lakeSet string
}

// errOrderNameTaken is returned when a resource with an order's name exists for something else.
var errOrderNameTaken = errors.New("exists and was not made for this order")

//+kubebuilder:rbac:groups=orama.cloud,resources=lakeordersources,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=lakeordersources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=lakesets;kubernetesclusters;clusterhelmreleases;oramaoperators,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusterproviders,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *LakeOrderSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var source oramav1alpha1.LakeOrderSource
	if err := r.Get(ctx, req.NamespacedName, &source); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	observed := source.Status.DeepCopy()
	status := &source.Status
	status.ObservedGeneration = source.Generation
	interval := defaultOrderPollInterval
	if source.Spec.PollInterval != nil && source.Spec.PollInterval.Duration > 0 {
		interval = source.Spec.PollInterval.Duration
	}
	setPhase := func(phase, reason, message string) {
		setClusterStatus(&status.Phase, &status.Message, &status.Conditions, source.Generation, phase, reason, message)
	}

	member, err := orderMemberSpec(&source)
	if err == nil && len(orderClaimant(&source)) > cloudOrderClaimantLimit {
		// lake's CHECK on claimed_by: a longer claimant could claim nothing, on every pass.
		err = fmt.Errorf("the name is too long: claimed_by %q is over lake's %d characters; use a name of at most %d",
			orderClaimant(&source), cloudOrderClaimantLimit, cloudOrderClaimantLimit-len(orderClaimant(&oramav1alpha1.LakeOrderSource{})))
	}
	if err != nil {
		// Only an edit fixes it. No order is read, so none is claimed.
		status.Members, status.Orders = nil, nil
		setPhase(clusterPhaseDegraded, "InvalidSpec", err.Error())
		return settle(ctrl.Result{}, r.writeStatus(ctx, &source, observed))
	}
	var set oramav1alpha1.LakeSet
	if err := r.Get(ctx, types.NamespacedName{Name: source.Spec.LakeSetRef}, &set); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		status.Members, status.Orders = nil, nil
		setPhase(clusterPhasePending, "LakeSetMissing", "LakeSet "+source.Spec.LakeSetRef+" does not exist")
		return settle(ctrl.Result{RequeueAfter: interval}, r.writeStatus(ctx, &source, observed))
	}

	open := r.Orders
	if open == nil {
		open = openCloudOrderStoreThroughCluster
	}
	passCtx, cancel := context.WithTimeout(ctx, orderPassTimeout)
	defer cancel()
	claimant := orderClaimant(&source)
	pass := orderPass{source: &source, set: &set, member: member}
	var members []oramav1alpha1.LakeOrderSourceMemberStatus
	var orders []oramav1alpha1.LakeOrderStatus
	var provisioning, ready, failed int32
	var unreachable, tableMissing []string
	for _, m := range set.Spec.Clusters {
		target := orderSourceMember{ClusterRef: m.ClusterRef, Name: m.Name, Namespace: firstNonEmpty(m.Namespace, lakeSetKind.namespacePrefix+m.Name)}
		entry := oramav1alpha1.LakeOrderSourceMemberStatus{Cluster: m.ClusterRef, Name: m.Name}
		rows, store, err := r.readOrders(passCtx, open, target, claimant)
		if err != nil {
			entry.Message = err.Error()
			if errors.Is(err, errCloudOrdersTableMissing) {
				tableMissing = append(tableMissing, m.Name)
			} else {
				unreachable = append(unreachable, m.Name+" on "+m.ClusterRef+" ("+err.Error()+")")
			}
			members = append(members, entry)
			continue
		}
		entry.Reachable = true
		members = append(members, entry)
		for _, order := range rows {
			state, outcome := r.advance(passCtx, store, pass, claimant, order)
			switch state {
			case cloudOrderProvisioning:
				provisioning++
				orders = append(orders, oramav1alpha1.LakeOrderStatus{ID: order.ID, Member: m.Name, Offer: order.Offer,
					Region: order.Region, LakeSet: outcome.lakeSet, Message: outcome.waiting})
			case cloudOrderReady:
				ready++
			case cloudOrderFailed:
				failed++
			}
		}
		closeCloudOrderStore(store)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].ID < orders[j].ID })
	status.Members, status.Orders = members, orders
	status.Provisioning, status.ReadyOrders, status.Failed = provisioning, ready, failed

	switch {
	case len(unreachable) > 0:
		setPhase(clusterPhaseDegraded, "MemberUnreachable", "orders cannot be read from "+strings.Join(unreachable, "; "))
	case len(tableMissing) > 0:
		setPhase(clusterPhasePending, "TableMissing", "waiting for lake's cloud_orders table (lake#831) on "+strings.Join(tableMissing, ", "))
	case provisioning > 0:
		setPhase(setPhaseProgressing, "Provisioning", fmt.Sprintf("%d order(s) being provisioned", provisioning))
	default:
		setPhase(clusterPhaseReady, "AllOrdersSettled", "")
	}
	return settle(ctrl.Result{RequeueAfter: interval}, r.writeStatus(ctx, &source, observed))
}

// readOrders opens a member's control plane and lists its orders. The caller closes the store.
func (r *LakeOrderSourceReconciler) readOrders(ctx context.Context, open CloudOrderStoreOpener, member orderSourceMember, claimant string) ([]CloudOrder, CloudOrderStore, error) {
	store, err := open(ctx, r.Client, member)
	if err != nil {
		return nil, nil, err
	}
	rows, err := store.ListOrders(ctx, claimant)
	if err != nil {
		closeCloudOrderStore(store)
		return nil, nil, err
	}
	return rows, store, nil
}

// closeCloudOrderStore ends a member's connection and its port-forward. It is bounded: the pass's
// context may be spent, and a leaf that stopped answering must not hold the reconciler.
func closeCloudOrderStore(store CloudOrderStore) {
	ctx, cancel := context.WithTimeout(context.Background(), serviceStoreConnectTimeout)
	defer cancel()
	_ = store.Close(ctx)
}

// orderPass is what every order of one pass shares.
type orderPass struct {
	source *oramav1alpha1.LakeOrderSource
	set    *oramav1alpha1.LakeSet
	member map[string]any
}

// advance takes one order as far as it can go on this pass and returns the state it is left in.
// A paid order is claimed first; an order another claimant holds is never touched.
func (r *LakeOrderSourceReconciler) advance(ctx context.Context, store CloudOrderStore, pass orderPass, claimant string, order CloudOrder) (string, orderOutcome) {
	log := logf.FromContext(ctx).WithValues("order", order.ID, "offer", order.Offer)
	switch order.State {
	case cloudOrderPaid:
		claimed, err := store.Claim(ctx, order.ID, claimant)
		if err != nil {
			log.Error(err, "the paid order cannot be claimed; it is tried again on the next pass")
			return cloudOrderPaid, orderOutcome{}
		}
		if !claimed {
			// Another poller has it, or had it first. Its claim stands.
			return "", orderOutcome{}
		}
		log.Info("claimed a paid order")
	case cloudOrderProvisioning:
	case cloudOrderReady:
		// Its resources are made again if missing, so a recreated management cluster has them. Its
		// LakeSet keeps its cluster, and a dedicated cluster is not copied from the template again.
		if outcome := r.provision(ctx, pass, order); outcome.failure != "" {
			log.Info("a ready order's resources cannot be kept as described", "reason", outcome.failure)
		}
		return cloudOrderReady, orderOutcome{}
	case cloudOrderFailed:
		return cloudOrderFailed, orderOutcome{}
	default:
		return "", orderOutcome{}
	}
	if order.ClaimedBy != "" && order.ClaimedBy != claimant && order.State != cloudOrderPaid {
		return "", orderOutcome{}
	}
	outcome := r.provision(ctx, pass, order)
	switch {
	case outcome.failure != "":
		moved, err := store.MarkFailed(ctx, order.ID, claimant, outcome.failure)
		if err != nil {
			log.Error(err, "the order cannot be marked failed; it is tried again on the next pass", "reason", outcome.failure)
			return cloudOrderProvisioning, orderOutcome{waiting: outcome.failure}
		}
		if moved {
			log.Info("the order failed", "reason", outcome.failure)
		}
		return cloudOrderFailed, outcome
	case outcome.host != "":
		moved, err := store.MarkReady(ctx, order.ID, claimant, outcome.host)
		if err != nil {
			log.Error(err, "the order cannot be marked ready; it is tried again on the next pass")
			return cloudOrderProvisioning, orderOutcome{waiting: "marking the order ready", lakeSet: outcome.lakeSet}
		}
		if moved {
			log.Info("the order is ready", "host", outcome.host)
		}
		return cloudOrderReady, outcome
	}
	return cloudOrderProvisioning, outcome
}

// provision creates or keeps an order's resources and reports how far they are.
func (r *LakeOrderSourceReconciler) provision(ctx context.Context, pass orderPass, order CloudOrder) orderOutcome {
	name := orderResourceName(order.ID)
	host := name + "." + pass.source.Spec.HostDomain
	if order.Region == "" {
		return orderOutcome{failure: "the order names no region"}
	}
	var cluster string
	var outcome orderOutcome
	switch order.Offer {
	case offerPrivateWarehouse:
		cluster, outcome = r.placedWarehouse(ctx, name, order)
		if cluster == "" && outcome.waiting == "" {
			cluster, outcome = r.warehouseCluster(ctx, pass.set, order.Region)
		}
	case offerDedicatedCluster:
		cluster, outcome = r.dedicatedCluster(ctx, pass.source, order, name)
	default:
		return orderOutcome{failure: fmt.Sprintf("the operator provisions private_warehouse and dedicated_cluster orders only; a %q order goes to support through the order form", order.Offer)}
	}
	if outcome.failure != "" || outcome.waiting != "" {
		return outcome
	}
	lakeSet, err := orderLakeSet(pass, order, name, cluster, host)
	if err != nil {
		return orderOutcome{waiting: err.Error()}
	}
	if err := r.ensure(ctx, pass.source, order, lakeSet, func(existing client.Object) {
		existing.(*oramav1alpha1.LakeSet).Spec = lakeSet.Spec
	}); err != nil {
		return orderError(err, "LakeSet "+name)
	}
	outcome.lakeSet = name
	var current oramav1alpha1.LakeSet
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &current); err != nil {
		outcome.waiting = "LakeSet " + name + " cannot be read: " + err.Error()
		return outcome
	}
	if current.Status.Phase == clusterPhaseReady && current.Status.ObservedGeneration == current.Generation {
		outcome.host = host
		return outcome
	}
	outcome.waiting = "LakeSet " + name + " is " + firstNonEmpty(current.Status.Phase, clusterPhasePending)
	if current.Status.Message != "" {
		outcome.waiting += ": " + current.Status.Message
	}
	return outcome
}

// orderError turns a failed create or update into an outcome: a name taken by something else
// fails the order, anything else is retried.
func orderError(err error, what string) orderOutcome {
	if errors.Is(err, errOrderNameTaken) {
		return orderOutcome{failure: what + " " + err.Error()}
	}
	return orderOutcome{waiting: what + " cannot be written: " + err.Error()}
}

// placedWarehouse is the cluster an order's private warehouse already runs on: its LakeSet's member's.
// A warehouse is placed once, when its LakeSet is first made. Choosing again would move a ready
// customer to a new, empty Lake on another cluster whenever the source set's member list changes,
// and leave the old Lake, with the customer's data, running unmanaged under Retain. "" when the
// order has no LakeSet yet, or the name is taken by something else (ensure then fails the order).
func (r *LakeOrderSourceReconciler) placedWarehouse(ctx context.Context, name string, order CloudOrder) (string, orderOutcome) {
	var set oramav1alpha1.LakeSet
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &set); err != nil {
		if apierrors.IsNotFound(err) {
			return "", orderOutcome{}
		}
		return "", orderOutcome{waiting: "LakeSet " + name + " cannot be read: " + err.Error()}
	}
	if set.Labels[orderIDLabel] != order.ID || len(set.Spec.Clusters) == 0 {
		return "", orderOutcome{}
	}
	return set.Spec.Clusters[0].ClusterRef, orderOutcome{}
}

// warehouseCluster is the set's cluster in the ordered region, for an order placed for the first
// time. A private warehouse runs on the hardware lake.live already runs on there.
func (r *LakeOrderSourceReconciler) warehouseCluster(ctx context.Context, set *oramav1alpha1.LakeSet, region string) (string, orderOutcome) {
	var offered []string
	for _, member := range set.Spec.Clusters {
		var cluster oramav1alpha1.KubernetesCluster
		if err := r.Get(ctx, types.NamespacedName{Name: member.ClusterRef}, &cluster); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", orderOutcome{waiting: "KubernetesCluster " + member.ClusterRef + " cannot be read: " + err.Error()}
		}
		if cluster.Spec.Region == region {
			return cluster.Name, orderOutcome{}
		}
		offered = append(offered, cluster.Spec.Region)
	}
	sort.Strings(offered)
	return "", orderOutcome{failure: fmt.Sprintf("no cluster of LakeSet %s runs in region %q, so a private warehouse cannot be placed there (regions offered: %s)",
		set.Name, region, strings.Join(dedupe(offered), ", "))}
}

// cloudProviderTypes maps an order's cloud_provider to the provider type that creates its
// clusters. Only Azure is supported today (lake#831: any other goes through the order form).
var cloudProviderTypes = map[string]oramav1alpha1.KubernetesClusterProviderType{
	"azure": oramav1alpha1.KubernetesClusterProviderAKS,
}

// dedicatedCluster creates the cluster of a dedicated_cluster order: the KubernetesCluster, its
// platform and its operator, each a copy of the template cluster's.
//
// The template is copied once, when the order's resources are first made, and never again: an
// ordered cluster does not follow the template, so a change to the template cluster (an operator
// version, a Kubernetes version, a node pool) never reaches customers' clusters at once and
// ungated. The KubernetesCluster is made last, so once it exists every copy was made, and the order
// is not compared with the template again.
func (r *LakeOrderSourceReconciler) dedicatedCluster(ctx context.Context, source *oramav1alpha1.LakeOrderSource, order CloudOrder, name string) (string, orderOutcome) {
	spec := source.Spec.DedicatedCluster
	if spec == nil {
		return "", orderOutcome{failure: "this cluster cannot create clusters: LakeOrderSource " + source.Name + " names no dedicatedCluster provider"}
	}
	var made oramav1alpha1.KubernetesCluster
	switch err := r.Get(ctx, types.NamespacedName{Name: name}, &made); {
	case err == nil:
		if id := made.Labels[orderIDLabel]; id != order.ID {
			return "", orderError(fmt.Errorf("%w (its %s label is %q)", errOrderNameTaken, orderIDLabel, id), "KubernetesCluster "+name)
		}
		return name, orderOutcome{}
	case !apierrors.IsNotFound(err):
		return "", orderOutcome{waiting: "KubernetesCluster " + name + " cannot be read: " + err.Error()}
	}
	providerType, supported := cloudProviderTypes[strings.ToLower(order.CloudProvider)]
	if !supported {
		return "", orderOutcome{failure: fmt.Sprintf("dedicated clusters are created on Azure only today; a %q cluster goes to support through the order form", order.CloudProvider)}
	}
	var provider oramav1alpha1.KubernetesClusterProvider
	if err := r.Get(ctx, types.NamespacedName{Name: spec.ProviderRef}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return "", orderOutcome{failure: "this cluster has no cloud identity to create clusters: KubernetesClusterProvider " + spec.ProviderRef + " does not exist on it"}
		}
		return "", orderOutcome{waiting: "KubernetesClusterProvider " + spec.ProviderRef + " cannot be read: " + err.Error()}
	}
	if provider.Spec.Type != providerType {
		return "", orderOutcome{failure: fmt.Sprintf("this cluster has no cloud identity for %s: KubernetesClusterProvider %s is of type %s", order.CloudProvider, provider.Name, provider.Spec.Type)}
	}
	if !provider.Status.Ready {
		checked := provider.Status.ObservedGeneration == provider.Generation && provider.Status.Message != ""
		if checked && provider.Status.AuthMethod == "" {
			// Its credentials could not be read at all: the cluster has no identity to use.
			return "", orderOutcome{failure: "this cluster has no cloud identity to create clusters: KubernetesClusterProvider " + provider.Name + " has no usable credentials: " + provider.Status.Message}
		}
		return "", orderOutcome{waiting: "KubernetesClusterProvider " + provider.Name + " is not Ready: " + firstNonEmpty(provider.Status.Message, "its credentials are not checked yet")}
	}
	if _, known := cloudRegionLocations[order.Region]; !known {
		return "", orderOutcome{failure: fmt.Sprintf("region %q is not one the operator creates clusters in; another region goes to support through the order form", order.Region)}
	}

	var template oramav1alpha1.KubernetesCluster
	if err := r.Get(ctx, types.NamespacedName{Name: spec.TemplateClusterRef}, &template); err != nil {
		return "", orderOutcome{waiting: "the template cluster " + spec.TemplateClusterRef + " cannot be read: " + err.Error()}
	}
	var releases oramav1alpha1.ClusterHelmReleaseList
	if err := r.List(ctx, &releases); err != nil {
		return "", orderOutcome{waiting: "ClusterHelmReleases cannot be listed: " + err.Error()}
	}
	var operators oramav1alpha1.OramaOperatorList
	if err := r.List(ctx, &operators); err != nil {
		return "", orderOutcome{waiting: "OramaOperators cannot be listed: " + err.Error()}
	}
	objects, err := dedicatedClusterObjects(&template, releases.Items, operators.Items, spec.ProviderRef, order, name)
	if err != nil {
		return "", orderOutcome{waiting: err.Error()}
	}
	// Made only: a copy already made on an earlier pass (one that stopped before the cluster) keeps
	// its spec. The KubernetesCluster comes last in objects.
	for _, desired := range objects {
		if err := r.ensure(ctx, source, order, desired, func(client.Object) {}); err != nil {
			return "", orderError(err, fmt.Sprintf("%T %s", desired, desired.GetName()))
		}
	}
	// The Lake's LakeSet is made at once: it waits for the cluster and its operator itself, and
	// says so in its status.
	return name, orderOutcome{}
}

// dedicatedClusterObjects is what a dedicated cluster is made of: the template cluster's
// ClusterHelmReleases, OramaOperator and KubernetesCluster, each copied with the order's name and
// region, the KubernetesCluster last. A release's dependencies are renamed with it.
func dedicatedClusterObjects(template *oramav1alpha1.KubernetesCluster, releases []oramav1alpha1.ClusterHelmRelease, operators []oramav1alpha1.OramaOperator, providerRef string, order CloudOrder, name string) ([]client.Object, error) {
	cluster := &oramav1alpha1.KubernetesCluster{ObjectMeta: metav1.ObjectMeta{Name: name}}
	template.Spec.DeepCopyInto(&cluster.Spec)
	cluster.Spec.ProviderRef = providerRef
	cluster.Spec.Region = order.Region
	cluster.Spec.KubeconfigSecretRef = nil
	// Deleting the resource never destroys a customer's cluster.
	cluster.Spec.DeletionPolicy = oramav1alpha1.ClusterDeletionRetain
	tags := map[string]string{}
	for k, v := range template.Spec.Tags {
		tags[k] = v
	}
	tags[orderClusterTag] = order.ID
	cluster.Spec.Tags = tags
	var objects []client.Object

	renamed := map[string]string{}
	var platform []oramav1alpha1.ClusterHelmRelease
	for _, release := range releases {
		if release.Spec.ClusterRef == template.Name {
			renamed[release.Name] = name + "-" + release.Spec.ReleaseName
			platform = append(platform, release)
		}
	}
	if len(platform) == 0 {
		return nil, fmt.Errorf("the template cluster %s has no ClusterHelmRelease, so a dedicated cluster would have no platform", template.Name)
	}
	sort.Slice(platform, func(i, j int) bool { return platform[i].Name < platform[j].Name })
	for _, release := range platform {
		copied := &oramav1alpha1.ClusterHelmRelease{ObjectMeta: metav1.ObjectMeta{Name: renamed[release.Name]}}
		release.Spec.DeepCopyInto(&copied.Spec)
		copied.Spec.ClusterRef = name
		for i, dependency := range copied.Spec.DependsOn {
			if to, ok := renamed[dependency]; ok {
				copied.Spec.DependsOn[i] = to
			}
		}
		objects = append(objects, copied)
	}

	var operator *oramav1alpha1.OramaOperator
	for i := range operators {
		if operators[i].Spec.ClusterRef == template.Name {
			operator = &operators[i]
			break
		}
	}
	if operator == nil {
		return nil, fmt.Errorf("the template cluster %s has no OramaOperator, so a dedicated cluster would have no operator", template.Name)
	}
	copiedOperator := &oramav1alpha1.OramaOperator{ObjectMeta: metav1.ObjectMeta{Name: name}}
	operator.Spec.DeepCopyInto(&copiedOperator.Spec)
	copiedOperator.Spec.ClusterRef = name
	return append(objects, copiedOperator, cluster), nil
}

// orderLakeSet is the LakeSet of an order: the source set's template and secrets, and one member
// on the chosen cluster with the source's member settings, its host and its node name.
func orderLakeSet(pass orderPass, order CloudOrder, name, cluster, host string) (*oramav1alpha1.LakeSet, error) {
	member := mergeSpec(pass.member, map[string]any{
		"host": host,
		"args": []any{"--listen", "0.0.0.0:4900", "--node", name},
	})
	raw, err := json.Marshal(member)
	if err != nil {
		return nil, err
	}
	set := &oramav1alpha1.LakeSet{ObjectMeta: metav1.ObjectMeta{Name: name}}
	set.Spec.Template = *pass.set.Spec.Template.DeepCopy()
	set.Spec.Secrets = append([]oramav1alpha1.SetSecret(nil), pass.set.Spec.Secrets...)
	set.Spec.Clusters = []oramav1alpha1.SetMember{{
		ClusterRef: cluster, Name: name,
		Spec:    &apiextensionsv1.JSON{Raw: raw},
		Secrets: append([]oramav1alpha1.SetSecret(nil), pass.source.Spec.MemberSecrets...),
	}}
	// Removing the member or the set leaves the customer's Lake running.
	set.Spec.DeletionPolicy = oramav1alpha1.SetDeletionRetain
	return set, nil
}

// orderMemberSpec reads the source's member settings and refuses the fields the operator sets
// per order.
func orderMemberSpec(source *oramav1alpha1.LakeOrderSource) (map[string]any, error) {
	member := map[string]any{}
	if source.Spec.Member != nil && len(source.Spec.Member.Raw) > 0 {
		var err error
		if member, err = jsonObject(source.Spec.Member.Raw, "spec.member"); err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"host", "args", "apexHost", "region", "regions", "namespace"} {
		if _, set := member[field]; set {
			return nil, fmt.Errorf("spec.member sets %s, which the operator sets for each order: remove it", field)
		}
	}
	if env, ok := member["env"].([]any); ok {
		for _, entry := range env {
			if variable, ok := entry.(map[string]any); ok && variable["name"] == "LAKE_PUBLIC_HOST" {
				return nil, errors.New("spec.member sets LAKE_PUBLIC_HOST in env, which the operator sets from each order's host: remove it")
			}
		}
	}
	return member, nil
}

// ensure creates an order's resource, or brings an existing one made for the same order to the
// desired spec. One of the same name made for anything else is never touched.
func (r *LakeOrderSourceReconciler) ensure(ctx context.Context, source *oramav1alpha1.LakeOrderSource, order CloudOrder, desired client.Object, setSpec func(existing client.Object)) error {
	existing := desired.DeepCopyObject().(client.Object)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if existing.GetResourceVersion() != "" {
			if id := existing.GetLabels()[orderIDLabel]; id != order.ID {
				return fmt.Errorf("%w (its %s label is %q)", errOrderNameTaken, orderIDLabel, id)
			}
		}
		labels := existing.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[orderIDLabel] = order.ID
		labels[orderSourceLabel] = source.Name
		existing.SetLabels(labels)
		annotations := existing.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[orderOwnerAnnotation] = "LakeOrderSource/" + source.Name + ": made from cloud_orders row " + order.ID + "; not in config/clusters/"
		existing.SetAnnotations(annotations)
		setSpec(existing)
		return nil
	})
	return err
}

func (r *LakeOrderSourceReconciler) writeStatus(ctx context.Context, source *oramav1alpha1.LakeOrderSource, observed *oramav1alpha1.LakeOrderSourceStatus) error {
	if reflect.DeepEqual(&source.Status, observed) {
		return nil
	}
	base := source.DeepCopy()
	base.Status = *observed.DeepCopy()
	return r.Status().Patch(ctx, source, client.MergeFrom(base))
}

func (r *LakeOrderSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A LakeSet made for an order wakes its source, so a Ready set is reported at once.
	bySourceLabel := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		if name := obj.GetLabels()[orderSourceLabel]; name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
		}
		return nil
	})
	// A pass opens a port-forward and a connection to every member, so the source's own status
	// writes must not start another one: a message that differs each pass (an error naming the
	// forward's local port) would otherwise reconcile it without pause. Its spec, the order
	// LakeSets and clusters below, and the poll interval are what start a pass.
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.LakeOrderSource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&oramav1alpha1.LakeSet{}, bySourceLabel).
		Watches(&oramav1alpha1.KubernetesCluster{}, bySourceLabel).
		Complete(r)
}
