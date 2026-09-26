/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The service catalog (liquid-bridge decision 47; the contract is lake's
// docs/control-plane/services.md, "The catalog"). Kafka, ClickHouse, TiDB, Cassandra and Valkey
// are each run by their own upstream operator. The Orama operator writes that operator's resources
// as unstructured objects — it never imports the upstream Go modules — named <lake>-<slug> (plus,
// for some kinds, a second resource), controlled by the Lake and labelled like every service, then
// reads readiness from the upstream status and the connection from a Secret. The per-kind builders
// live in lake_service_<kind>.go; this file holds what every kind shares.
//
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkas;kafkanodepools;kafkausers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhouseinstallations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clickhouse-keeper.altinity.com,resources=clickhousekeeperinstallations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pingcap.com,resources=tidbclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8ssandra.io,resources=k8ssandraclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters,verbs=get;list;watch;create;update;patch;delete

const (
	serviceKindKafka      = "kafka"
	serviceKindClickHouse = "clickhouse"
	serviceKindTiDB       = "tidb"
	serviceKindCassandra  = "cassandra"
	serviceKindValkey     = "valkey"

	// servicePasswordBytes is the entropy of a password the Orama operator generates: 32 hex
	// characters, safe unescaped in a URI, an ACL file and a ClickHouse users.xml.
	servicePasswordBytes = 16
	// serviceComponentLabel marks the password Secret the Orama operator generates for a service.
	serviceComponentLabel  = "app.kubernetes.io/component"
	serviceCredentialsRole = "credentials"
)

// serviceKindNames is every kind the operator runs, in the order the contract lists them. The
// unknown-kind message names them all.
var serviceKindNames = []string{serviceKindZetta, serviceKindPostgres, serviceKindKafka, serviceKindClickHouse, serviceKindTiDB, serviceKindCassandra, serviceKindValkey}

// catalogKinds maps a catalog kind to its builder.
var catalogKinds = map[string]catalogKind{
	serviceKindKafka:      kafkaServiceKind{},
	serviceKindClickHouse: clickHouseServiceKind{},
	serviceKindTiDB:       tidbServiceKind{},
	serviceKindCassandra:  cassandraServiceKind{},
	serviceKindValkey:     valkeyServiceKind{},
}

// catalogService is what a builder sees of one present or absent service.
type catalogService struct {
	lake      *oramav1alpha1.Lake
	svc       *LakeService
	spec      lakeServiceSpec
	namespace string
	// name is <lake>-<slug>, the primary resource's name.
	name string
}

// catalogObject names one upstream object a kind may write.
type catalogObject struct {
	gvk  schema.GroupVersionKind
	name string
}

// serviceField is one spec field the Orama operator owns on an upstream object. Only these fields
// are written, so the upstream API server's defaults on the rest of the object survive and a
// steady service is never rewritten. A createOnly field is written when the object is created and
// left alone afterwards: storage (volume claim templates are immutable) and arrays whose items the
// upstream CRD defaults.
type serviceField struct {
	path       []string
	value      interface{}
	createOnly bool
}

// catalogResource is one desired upstream object.
type catalogResource struct {
	catalogObject
	// labels are added to the service labels, for example the Strimzi cluster label.
	labels map[string]string
	fields []serviceField
}

// catalogKind is one upstream-operator kind of the catalog.
type catalogKind interface {
	// title names the kind in status messages: "Kafka", "ClickHouse", ...
	title() string
	// objects lists every object the kind can write for the service, primary first. Deletion and
	// ownership checks walk this list, so it does not depend on the spec.
	objects(in catalogService) []catalogObject
	// build returns the desired objects for a present service, primary first. password is the
	// Orama-generated password, empty for kinds whose upstream operator makes the credential.
	build(in catalogService, password string) []catalogResource
	// passwordSecret names the Secret the Orama operator generates and the key holding the
	// password, or "" when the upstream operator makes the credential.
	passwordSecret(name string) (secretName, key string)
	// endpoint is the in-cluster address written to service_status.
	endpoint(in catalogService) string
	// ready reads the upstream status of the objects found (keyed by kind) and reports whether
	// the service is ready and, when it is not, what it is waiting for.
	ready(in catalogService, found map[string]*unstructured.Unstructured) (bool, string)
	// credential returns the <slug>.json body, or nil while the upstream Secret it reads is not
	// there yet. An error is a failed read; a credential that cannot be trusted is reported by
	// the returned message, which fails the service.
	credential(ctx context.Context, c client.Reader, in catalogService, found map[string]*unstructured.Unstructured, password string) (credential []byte, refusal string, err error)
	// claimSelectors select the PersistentVolumeClaims the upstream operator creates for the
	// service. Deletion removes them once the upstream objects are gone.
	claimSelectors(in catalogService) []client.MatchingLabels
	// nameOverhead is how many characters the upstream operator adds to <lake>-<slug> in the
	// longest name it derives (counting a StatefulSet's 11-character controller-revision-hash
	// suffix, which must fit a 63-character label value).
	nameOverhead() int
}

// unknownServiceKindMessage fails a row whose kind this operator does not run.
func unknownServiceKindMessage(kind string) string {
	return fmt.Sprintf("unknown service kind %q: this operator runs %s", kind, strings.Join(serviceKindNames, ", "))
}

// serviceNotInstalledMessage fails a service whose upstream operator's CRD is not on the cluster.
func serviceNotInstalledMessage(kind string) string {
	return fmt.Sprintf("the %s operator is not installed on this cluster", kind)
}

// isKindNotInstalled reports whether err says the API server does not serve the kind: its CRD is
// not installed.
func isKindNotInstalled(err error) bool {
	return err != nil && meta.IsNoMatchError(err)
}

// serviceNameOverhead is the longest suffix a kind's derived names add to <lake>-<slug>.
func serviceNameOverhead(kind string) int {
	overhead := len(serviceLongestNameSuffix)
	if catalog, found := catalogKinds[kind]; found && catalog.nameOverhead() > overhead {
		overhead = catalog.nameOverhead()
	}
	return overhead
}

func newServiceObject(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func newCatalogService(lake *oramav1alpha1.Lake, namespace string, svc *LakeService, spec lakeServiceSpec) catalogService {
	return catalogService{lake: lake, svc: svc, spec: spec, namespace: namespace, name: serviceResourceName(lake.Name, svc.Slug)}
}

// setUpstreamServiceMetadata writes the service labels and annotation on an object an upstream
// operator reconciles. app.kubernetes.io/instance is the object's own name rather than the Lake's:
// upstream operators read it as the instance of their cluster (TiDB Operator derives every
// selector from it), so a Lake-wide value would make two services of one kind select each
// other's pods.
func setUpstreamServiceMetadata(obj metav1.Object, lake *oramav1alpha1.Lake, svc *LakeService, extra map[string]string) {
	setServiceMetadata(obj, lake, svc)
	labels := obj.GetLabels()
	labels["app.kubernetes.io/instance"] = obj.GetName()
	for key, value := range extra {
		labels[key] = value
	}
	obj.SetLabels(labels)
}

// applyServiceFields writes the fields a builder owns onto obj.
func applyServiceFields(obj *unstructured.Unstructured, fields []serviceField, creating bool) error {
	for _, field := range fields {
		if field.createOnly && !creating {
			continue
		}
		if err := unstructured.SetNestedField(obj.Object, runtime.DeepCopyJSONValue(field.value), field.path...); err != nil {
			return fmt.Errorf("set %s: %w", strings.Join(field.path, "."), err)
		}
	}
	return nil
}

// reconcileCatalogService creates or updates the upstream objects of one present catalog service
// and reports what it observed. It follows reconcilePostgresService: a foreign object of the same
// name fails the service, a new service is refused over the quota, and only owned fields are
// written.
func (r *LakeReconciler) reconcileCatalogService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService, spec lakeServiceSpec, live *int, kind catalogKind) (serviceResult, error) {
	in := newCatalogService(lake, namespace, svc, spec)
	found, refusal, err := r.findCatalogObjects(ctx, in, kind, svc)
	if err != nil || refusal != nil {
		return derefResult(refusal), err
	}
	primary := kind.objects(in)[0]
	creating := found[primary.gvk.Kind] == nil
	if creating {
		// A new cluster must not bind an old data volume of the same name.
		if result, blocked, err := r.catalogVolumesBlockCreation(ctx, in, kind); err != nil || blocked {
			return result, err
		}
		if refusal, refused := quotaRefusal(lake, *live); refused {
			return refusal, nil
		}
	}

	password := ""
	if secretName, key := kind.passwordSecret(in.name); secretName != "" {
		var failure string
		password, failure, err = r.ensureServicePassword(ctx, in, secretName, key)
		if err != nil {
			return serviceResult{}, err
		}
		if failure != "" {
			return serviceResult{phase: ServicePhaseFailed, message: failure}, nil
		}
	}

	for _, resource := range kind.build(in, password) {
		obj, err := r.applyCatalogResource(ctx, in, resource, found[resource.gvk.Kind] == nil)
		if isKindNotInstalled(err) {
			return serviceResult{phase: ServicePhaseFailed, message: serviceNotInstalledMessage(svc.Kind)}, nil
		}
		if err != nil {
			return serviceResult{}, err
		}
		found[resource.gvk.Kind] = obj
	}
	if creating {
		*live++
	}

	endpoint := kind.endpoint(in)
	ready, waiting := kind.ready(in, found)
	if !ready {
		return serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: waiting}, nil
	}
	result := serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: waiting, resourceReady: true}
	credential, distrust, err := kind.credential(ctx, r.Client, in, found, password)
	if err != nil {
		return serviceResult{}, err
	}
	if distrust != "" {
		return serviceResult{phase: ServicePhaseFailed, endpoint: endpoint, message: distrust}, nil
	}
	result.credential = credential
	return result, nil
}

func derefResult(result *serviceResult) serviceResult {
	if result == nil {
		return serviceResult{}
	}
	return *result
}

// findCatalogObjects reads every object the kind may have written. It returns a refusal when the
// kind is not installed, an object is foreign, or an object is still being deleted.
func (r *LakeReconciler) findCatalogObjects(ctx context.Context, in catalogService, kind catalogKind, svc *LakeService) (map[string]*unstructured.Unstructured, *serviceResult, error) {
	found := map[string]*unstructured.Unstructured{}
	for _, ref := range kind.objects(in) {
		obj := newServiceObject(ref.gvk, in.namespace, ref.name)
		err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		switch {
		case isKindNotInstalled(err):
			return nil, &serviceResult{phase: ServicePhaseFailed, message: serviceNotInstalledMessage(svc.Kind)}, nil
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, nil, err
		}
		if serviceOwnershipOf(in.lake, obj, svc) != serviceOwned {
			return nil, &serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage(ref.gvk.Kind, in.namespace, ref.name)}, nil
		}
		if obj.GetDeletionTimestamp() != nil {
			return nil, &serviceResult{phase: ServicePhaseProvisioning, endpoint: kind.endpoint(in),
				message: fmt.Sprintf("waiting for the previous %s %s to be deleted", ref.gvk.Kind, ref.name)}, nil
		}
		found[ref.gvk.Kind] = obj
	}
	return found, nil, nil
}

// catalogClaims lists the PVCs the upstream operator created for the service, sorted by name.
func (r *LakeReconciler) catalogClaims(ctx context.Context, in catalogService, kind catalogKind) ([]corev1.PersistentVolumeClaim, error) {
	seen := map[string]bool{}
	var claims []corev1.PersistentVolumeClaim
	for _, selector := range kind.claimSelectors(in) {
		var list corev1.PersistentVolumeClaimList
		if err := r.List(ctx, &list, client.InNamespace(in.namespace), selector); err != nil {
			return nil, err
		}
		for _, claim := range list.Items {
			if !seen[claim.Name] {
				seen[claim.Name] = true
				claims = append(claims, claim)
			}
		}
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })
	return claims, nil
}

// catalogVolumesBlockCreation waits for a terminating claim of the service's selector and refuses
// a live one nothing proves is this service's.
func (r *LakeReconciler) catalogVolumesBlockCreation(ctx context.Context, in catalogService, kind catalogKind) (serviceResult, bool, error) {
	claims, err := r.catalogClaims(ctx, in, kind)
	if err != nil {
		return serviceResult{}, false, err
	}
	for _, claim := range claims {
		if claim.DeletionTimestamp != nil {
			return serviceResult{phase: ServicePhaseProvisioning, message: fmt.Sprintf("waiting for the previous %s data volume to be deleted", kind.title())}, true, nil
		}
		return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("PersistentVolumeClaim", in.namespace, claim.Name)}, true, nil
	}
	return serviceResult{}, false, nil
}

// ensureServicePassword returns the password in the Secret the Orama operator generates for the
// service, creating the Secret with a random password the first time. The Secret is controlled by
// the Lake and labelled with the service; the password is never rotated. A Secret of that name the
// Lake does not own fails the service.
func (r *LakeReconciler) ensureServicePassword(ctx context.Context, in catalogService, secretName, key string) (string, string, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: secretName}, secret)
	if err == nil {
		if serviceOwnershipOf(in.lake, secret, in.svc) != serviceOwned {
			return "", foreignServiceMessage("Secret", in.namespace, secretName), nil
		}
		if password := string(secret.Data[key]); password != "" {
			return password, "", nil
		}
		return "", fmt.Sprintf("Secret %s/%s has no %s", in.namespace, secretName, key), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", "", err
	}
	password, err := randHex(servicePasswordBytes)
	if err != nil {
		return "", "", err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: in.namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{key: []byte(password)},
	}
	setServiceMetadata(secret, in.lake, in.svc)
	secret.Labels[serviceComponentLabel] = serviceCredentialsRole
	if err := controllerutil.SetControllerReference(in.lake, secret, r.Scheme); err != nil {
		return "", "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", "", err
	}
	return password, "", nil
}

// applyCatalogResource creates or updates one upstream object, writing only the fields the builder
// owns.
func (r *LakeReconciler) applyCatalogResource(ctx context.Context, in catalogService, resource catalogResource, creating bool) (*unstructured.Unstructured, error) {
	obj := newServiceObject(resource.gvk, in.namespace, resource.name)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		setUpstreamServiceMetadata(obj, in.lake, in.svc, resource.labels)
		if err := applyServiceFields(obj, resource.fields, creating); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(in.lake, obj, r.Scheme)
	}); err != nil {
		return nil, err
	}
	return obj, nil
}

// deleteCatalogService deletes an absent catalog service: every upstream object it owns (primary
// first, in one pass), then — once they are gone, so the upstream operator can no longer recreate
// them — the generated password Secret and the PVCs the upstream operator left behind. It reports
// what remains; only objects passing the ownership rule are deleted, and a PVC only when the
// service provably ran here (an owned object was seen, or service_status carries its endpoint).
func (r *LakeReconciler) deleteCatalogService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService, previousEndpoint string, kind catalogKind) (serviceResult, bool, error) {
	in := newCatalogService(lake, namespace, svc, lakeServiceSpec{})
	remaining := false
	for _, ref := range kind.objects(in) {
		obj := newServiceObject(ref.gvk, namespace, ref.name)
		err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if isKindNotInstalled(err) || apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return serviceResult{}, false, err
		}
		if serviceOwnershipOf(lake, obj, svc) != serviceOwned {
			return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage(ref.gvk.Kind, namespace, ref.name)}, false, nil
		}
		remaining = true
		if obj.GetDeletionTimestamp() == nil {
			if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return serviceResult{}, false, fmt.Errorf("delete service %s %s/%s: %w", ref.gvk.Kind, namespace, ref.name, err)
			}
		}
	}
	if remaining {
		// The endpoint is carried into the Deleting status, which proves on the next pass that
		// the volumes left behind are this service's.
		return serviceResult{endpoint: kind.endpoint(in), message: fmt.Sprintf("deleting the %s cluster", kind.title())}, true, nil
	}

	if secretName, _ := kind.passwordSecret(in.name); secretName != "" {
		if err := r.deleteServicePassword(ctx, in, secretName); err != nil {
			return serviceResult{}, false, err
		}
	}

	claims, err := r.catalogClaims(ctx, in, kind)
	if err != nil {
		return serviceResult{}, false, err
	}
	for i := range claims {
		claim := &claims[i]
		if claim.DeletionTimestamp != nil {
			continue
		}
		if previousEndpoint == "" {
			return serviceResult{phase: ServicePhaseFailed, message: foreignServiceMessage("PersistentVolumeClaim", namespace, claim.Name)}, false, nil
		}
		if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
			return serviceResult{}, false, fmt.Errorf("delete service data volume %s/%s: %w", namespace, claim.Name, err)
		}
	}
	if len(claims) > 0 {
		return serviceResult{message: fmt.Sprintf("deleting the %s data volumes", kind.title())}, true, nil
	}
	return serviceResult{}, false, nil
}

// deleteServicePassword removes the generated password Secret when this service owns it.
func (r *LakeReconciler) deleteServicePassword(ctx context.Context, in catalogService, secretName string) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: secretName}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if serviceOwnershipOf(in.lake, secret, in.svc) != serviceOwned {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, secret))
}

// liveCatalogObjects lists the primary objects of every catalog kind in the namespace for the
// quota. A kind whose CRD is not installed has none.
func (r *LakeReconciler) liveCatalogObjects(ctx context.Context, lake *oramav1alpha1.Lake, namespace string) ([]metav1.Object, error) {
	selector := client.MatchingLabels{serviceLakeLabel: lake.Name}
	var objects []metav1.Object
	for _, name := range serviceKindNames {
		kind, found := catalogKinds[name]
		if !found {
			continue
		}
		gvk := kind.objects(catalogService{})[0].gvk
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := r.List(ctx, list, client.InNamespace(namespace), selector); err != nil {
			if isKindNotInstalled(err) {
				continue
			}
			return nil, err
		}
		for i := range list.Items {
			objects = append(objects, &list.Items[i])
		}
	}
	return objects, nil
}

// conditionTrue reports whether obj's status.conditions (or the conditions at path) holds a
// condition of the given type with status "True".
func conditionTrue(obj map[string]interface{}, conditionType string, path ...string) bool {
	if len(path) == 0 {
		path = []string{"status", "conditions"}
	}
	conditions, _, _ := unstructured.NestedSlice(obj, path...)
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if condition["type"] == conditionType {
			return condition["status"] == string(metav1.ConditionTrue)
		}
	}
	return false
}

// readServiceSecret reads a Secret an upstream operator wrote, returning nil when it is not there.
func readServiceSecret(ctx context.Context, c client.Reader, namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return secret, nil
}

// hasOwnerUID reports whether obj carries an owner reference to the object with that UID.
func hasOwnerUID(obj metav1.Object, owner metav1.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.GetUID() && owner.GetUID() != "" {
			return true
		}
	}
	return false
}

func serviceStorage(spec lakeServiceSpec) string { return fmt.Sprintf("%dGi", spec.StorageGi) }
