// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tenant isolation of the Lake namespace (orama-operator#778).
//
// A tenant's managed service runs only in its workspace namespace, never in the Lake's own
// namespace, which holds the Lake's control database, admin token, instance key and every
// projection. Three rules keep it so:
//
//   - validateService refuses a service whose workspace namespace is the Lake namespace, so the
//     operator never creates (or deletes) a tenant service there;
//   - every object of a service carries the Lake's controller reference and the service labels
//     (setServiceMetadata), and the objects an upstream operator makes from it are owned by that
//     object, so deleting the Lake or the service removes them;
//   - this check lists what is left of a tenant service in the Lake namespace and reports it, as
//     the Lake's TenantServicesIsolated condition and a Warning event. It never deletes anything:
//     a leftover may hold a tenant's data, and removing it is a person's decision.
//
// The leftover that started this was a CloudNativePG Cluster lake/prod-svc-5d97f8ed8de9, made
// before services moved to workspace namespaces and left there with its Services and Secrets.

const (
	// lakeTenantIsolationConditionType is True when no tenant service object is in the Lake
	// namespace, False when one is, and Unknown when the check could not list them.
	lakeTenantIsolationConditionType = "TenantServicesIsolated"

	lakeTenantIsolatedReason       = "NoTenantObjects"
	lakeTenantObjectsFoundReason   = "TenantObjectsInLakeNamespace"
	lakeTenantIsolationCheckFailed = "CheckFailed"

	// lakeTenantObjectEventReason is the Warning event emitted when the leftovers change.
	lakeTenantObjectEventReason = "TenantObjectInLakeNamespace"

	// lakeTenantObjectsListed caps how many objects the condition message names.
	lakeTenantObjectsListed = 10

	// serviceAppNameValue is the app.kubernetes.io/name of every managed service resource.
	serviceAppNameValue = "lake-service"
)

// lakeNamespaceServiceMessage is the status a service gets when lake names the Lake namespace as
// its workspace namespace.
func lakeNamespaceServiceMessage(lake *oramav1alpha1.Lake) string {
	return fmt.Sprintf("workspace namespace %s is the Lake namespace; a tenant service runs only in its workspace namespace", lakeNamespace(lake))
}

// tenantObjectNamePattern matches the names a service's objects take in its namespace: the service
// resource <lake>-svc-<12 hex> or <lake>-ws-<12 hex>, what CloudNativePG, Strimzi and the
// StatefulSet controller derive from it (<name>-rw, <name>-app, <name>-1), and the Zetta data
// claims data-<name>-zetta-<n>. A copy left without its labels is still found by its name.
func tenantObjectNamePattern(lake *oramav1alpha1.Lake) *regexp.Regexp {
	return regexp.MustCompile(`^(data-)?` + regexp.QuoteMeta(lake.Name) + `-(svc|ws)-[0-9a-f]{12}($|-)`)
}

// isTenantServiceObject reports whether an object in the Lake namespace belongs to a tenant
// service: it carries a service id or the lake-service name label, or its name is a service's.
// The workspace label alone does not count: a bucket store's delivered credential lives in the
// Lake namespace by design (lake_store_mounts.go) and carries it.
func isTenantServiceObject(obj metav1.Object, names *regexp.Regexp) bool {
	labels := obj.GetLabels()
	if labels[serviceIDLabel] != "" || labels["app.kubernetes.io/name"] == serviceAppNameValue {
		return true
	}
	return names.MatchString(obj.GetName())
}

// lakeTenantObjects lists the tenant service objects in the Lake namespace, as sorted
// "<Kind> <name>" entries. Kinds whose CRD is not installed have none. It only reads.
func (r *LakeReconciler) lakeTenantObjects(ctx context.Context, lake *oramav1alpha1.Lake, namespace string) ([]string, error) {
	names := tenantObjectNamePattern(lake)
	in := client.InNamespace(namespace)
	var found []string
	add := func(kind string, obj metav1.Object) {
		if isTenantServiceObject(obj, names) {
			found = append(found, kind+" "+obj.GetName())
		}
	}

	var zettas oramav1alpha1.OramaZettaClusterList
	if err := r.List(ctx, &zettas, in); err != nil && !isKindNotInstalled(err) {
		return nil, fmt.Errorf("list OramaZettaClusters: %w", err)
	}
	for i := range zettas.Items {
		cluster := &zettas.Items[i]
		// The Lake's own data plane (spec.zetta) lives here; only a service or workspace Zetta is a
		// tenant's.
		if isLakeServiceCluster(cluster) || names.MatchString(cluster.Name) {
			found = append(found, "OramaZettaCluster "+cluster.Name)
		}
	}

	// The upstream kinds: CloudNativePG's Cluster, then each catalog kind's primary object.
	upstream := []schema.GroupVersionKind{cnpgClusterGVK}
	for _, name := range serviceKindNames {
		if kind, ok := catalogKinds[name]; ok {
			upstream = append(upstream, kind.objects(catalogService{})[0].gvk)
		}
	}
	for _, gvk := range upstream {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := r.List(ctx, list, in); err != nil {
			if isKindNotInstalled(err) {
				continue
			}
			return nil, fmt.Errorf("list %s: %w", gvk.Kind, err)
		}
		for i := range list.Items {
			add(gvk.Kind, &list.Items[i])
		}
	}

	var statefulSets appsv1.StatefulSetList
	if err := r.List(ctx, &statefulSets, in); err != nil {
		return nil, fmt.Errorf("list StatefulSets: %w", err)
	}
	for i := range statefulSets.Items {
		add("StatefulSet", &statefulSets.Items[i])
	}
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, in); err != nil {
		return nil, fmt.Errorf("list Deployments: %w", err)
	}
	for i := range deployments.Items {
		add("Deployment", &deployments.Items[i])
	}
	var services corev1.ServiceList
	if err := r.List(ctx, &services, in); err != nil {
		return nil, fmt.Errorf("list Services: %w", err)
	}
	for i := range services.Items {
		add("Service", &services.Items[i])
	}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, in); err != nil {
		return nil, fmt.Errorf("list Secrets: %w", err)
	}
	for i := range secrets.Items {
		add("Secret", &secrets.Items[i])
	}
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, in); err != nil {
		return nil, fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	for i := range claims.Items {
		add("PersistentVolumeClaim", &claims.Items[i])
	}

	sort.Strings(found)
	return found, nil
}

// lakeTenantIsolationCondition is the TenantServicesIsolated condition for what the check found.
func lakeTenantIsolationCondition(lake *oramav1alpha1.Lake, namespace string, found []string, err error) metav1.Condition {
	condition := metav1.Condition{Type: lakeTenantIsolationConditionType, ObservedGeneration: lake.Generation}
	switch {
	case err != nil:
		condition.Status, condition.Reason = metav1.ConditionUnknown, lakeTenantIsolationCheckFailed
		condition.Message = "cannot list the Lake namespace for tenant service objects: " + err.Error()
	case len(found) == 0:
		condition.Status, condition.Reason = metav1.ConditionTrue, lakeTenantIsolatedReason
		condition.Message = fmt.Sprintf("no tenant service object is in the Lake namespace %s", namespace)
	default:
		listed := found
		more := ""
		if len(listed) > lakeTenantObjectsListed {
			more = fmt.Sprintf(" and %d more", len(listed)-lakeTenantObjectsListed)
			listed = listed[:lakeTenantObjectsListed]
		}
		condition.Status, condition.Reason = metav1.ConditionFalse, lakeTenantObjectsFoundReason
		condition.Message = fmt.Sprintf(
			"%d tenant service object(s) are in the Lake namespace %s, outside any workspace namespace: %s%s. "+
				"The operator does not delete them; check whether they hold data and remove them by hand",
			len(found), namespace, strings.Join(listed, ", "), more)
	}
	return condition
}

// reconcileLakeTenantIsolation runs the check and returns the condition. When the leftovers are
// new or have changed it records a Warning event on the Lake. It never fails the Lake and never
// deletes anything.
func (r *LakeReconciler) reconcileLakeTenantIsolation(ctx context.Context, lake *oramav1alpha1.Lake, namespace string) metav1.Condition {
	found, err := r.lakeTenantObjects(ctx, lake, namespace)
	condition := lakeTenantIsolationCondition(lake, namespace, found, err)
	previous := apimeta.FindStatusCondition(lake.Status.Conditions, lakeTenantIsolationConditionType)
	changed := previous == nil || previous.Status != condition.Status || previous.Message != condition.Message
	if condition.Status == metav1.ConditionFalse && changed && r.Recorder != nil {
		r.Recorder.Event(lake, corev1.EventTypeWarning, lakeTenantObjectEventReason, condition.Message)
	}
	return condition
}
