/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// The service catalog (the contract is lake's
// docs/control-plane/services.md, "The catalog"). Kafka, ClickHouse, TiDB, Cassandra and Valkey
// are each run by their own upstream operator. The Orama operator writes that operator's resources
// as unstructured objects — it never imports the upstream Go modules — named <lake>-<slug> (plus,
// for some kinds, a second resource), controlled by the Lake and labelled like every service, then
// reads readiness from the upstream status and the connection from a Secret. SQLite, DuckDB and
// Polars have no upstream operator: the Orama operator writes the StatefulSet and Service of
// lake-sqlite, lake-duckdb and lake-polars itself through the same machinery
// (lake_service_wrapper.go; lake#195). The per-kind builders live in
// lake_service_<kind>.go; this file holds what every kind shares.
//
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkas;kafkanodepools;kafkausers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.strimzi.io,resources=strimzipodsets,verbs=get;list
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;create;patch
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
	serviceKindSQLite     = "sqlite"
	serviceKindDuckDB     = "duckdb"
	serviceKindPolars     = "polars"

	// servicePasswordBytes is the entropy of a password the Orama operator generates: 32 hex
	// characters, safe unescaped in a URI, an ACL file and a ClickHouse users.xml.
	servicePasswordBytes = 16
	// serviceComponentLabel marks the password Secret the Orama operator generates for a service.
	serviceComponentLabel  = "app.kubernetes.io/component"
	serviceCredentialsRole = "credentials"
	// serviceAppliedHashAnnotation records the hash of the onChange fields last written to an
	// object, so they are rewritten only when the desired value changes.
	serviceAppliedHashAnnotation = "orama.cloud/applied-spec-hash"
)

// serviceKindNames is every kind the operator runs, in the order the contract lists them. The
// unknown-kind message names them all.
var serviceKindNames = []string{serviceKindZetta, serviceKindPostgres, serviceKindKafka, serviceKindClickHouse, serviceKindTiDB, serviceKindCassandra, serviceKindValkey, serviceKindSQLite, serviceKindDuckDB, serviceKindPolars}

// catalogKinds maps a catalog kind to its builder.
var catalogKinds = map[string]catalogKind{
	serviceKindKafka:      kafkaServiceKind{},
	serviceKindClickHouse: clickHouseServiceKind{},
	serviceKindTiDB:       tidbServiceKind{},
	serviceKindCassandra:  cassandraServiceKind{},
	serviceKindValkey:     valkeyServiceKind{},
	serviceKindSQLite:     sqliteServiceKind,
	serviceKindDuckDB:     duckdbServiceKind,
	serviceKindPolars:     polarsServiceKind,
}

// catalogService is what a builder sees of one present or absent service.
type catalogService struct {
	lake      *oramav1alpha1.Lake
	svc       *LakeService
	spec      lakeServiceSpec
	namespace string
	// name is <lake>-<slug>, the primary resource's name.
	name string
	// image is the image of a kind the Orama operator runs itself (sqlite, duckdb, polars); empty
	// otherwise.
	image string
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
// upstream CRD defaults. An onChange field is written when the object is created and again only
// when its desired value changes, tracked by a hash in serviceAppliedHashAnnotation: a pod
// template, whose many API-server defaults would otherwise make every pass an update.
type serviceField struct {
	path       []string
	value      interface{}
	createOnly bool
	onChange   bool
}

// catalogPasswordSizer is implemented by a kind whose generated secret is not
// servicePasswordBytes long.
type catalogPasswordSizer interface {
	passwordBytes() int
}

func servicePasswordLength(kind catalogKind) int {
	if sized, ok := kind.(catalogPasswordSizer); ok {
		return sized.passwordBytes()
	}
	return servicePasswordBytes
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
	// podSelector selects the pods a client of the service reaches, by the labels the upstream
	// operator puts on them. Connections and tunnels select their target with it, and take its
	// ports from the Services whose selector holds all of it (service_targets.go).
	podSelector(in catalogService) map[string]string
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
	appliedHash, err := onChangeFieldsHash(fields)
	if err != nil {
		return err
	}
	writeOnChange := appliedHash != "" && (creating || obj.GetAnnotations()[serviceAppliedHashAnnotation] != appliedHash)
	for _, field := range fields {
		if field.createOnly && !creating {
			continue
		}
		if field.onChange && !writeOnChange {
			continue
		}
		if err := unstructured.SetNestedField(obj.Object, runtime.DeepCopyJSONValue(field.value), field.path...); err != nil {
			return fmt.Errorf("set %s: %w", strings.Join(field.path, "."), err)
		}
	}
	if writeOnChange {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[serviceAppliedHashAnnotation] = appliedHash
		obj.SetAnnotations(annotations)
	}
	return nil
}

// onChangeFieldsHash hashes the paths and values of the onChange fields, "" when there are none.
func onChangeFieldsHash(fields []serviceField) (string, error) {
	var owned []interface{}
	for _, field := range fields {
		if field.onChange {
			owned = append(owned, []interface{}{field.path, field.value})
		}
	}
	if len(owned) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(owned)
	if err != nil {
		return "", fmt.Errorf("hash the owned fields: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:16]), nil
}

// reconcileCatalogService creates or updates the upstream objects of one present catalog service
// and reports what it observed. It follows reconcilePostgresService: a foreign object of the same
// name fails the service, a new service is refused over the quota, and only owned fields are
// written.
func (r *LakeReconciler) reconcileCatalogService(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, svc *LakeService, spec lakeServiceSpec, live *int, kind catalogKind) (serviceResult, error) {
	in := newCatalogService(lake, namespace, svc, spec)
	in.image = r.serviceImage(lake, svc.Kind)
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
		password, failure, err = r.ensureServicePassword(ctx, in, secretName, key, servicePasswordLength(kind))
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
		// "Waiting" alone hides a service that will never start. A kind that can say what the
		// upstream operator is stuck on (a pod the API server refuses, for one) says it here, so
		// the reason reaches lake and the customer instead of staying in another operator's log.
		why := ""
		if explainer, ok := kind.(catalogWaitExplainer); ok {
			if why, err = explainer.waitingReason(ctx, r.Client, in, found); err != nil {
				return serviceResult{}, err
			}
		}
		if why == "" {
			// Every kind: what Kubernetes itself last refused or failed for this service's
			// objects (a pod the API server would not admit, a quota, an image that cannot be
			// pulled, a container that keeps exiting).
			if why, err = serviceWarningReason(ctx, r.Client, in); err != nil {
				return serviceResult{}, err
			}
		}
		if why != "" {
			waiting += ": " + why
		}
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
// service, creating the Secret with a random password of size bytes (hex-encoded) the first time.
// The Secret is controlled by the Lake and labelled with the service; the password is never
// rotated. A Secret of that name the Lake does not own fails the service.
func (r *LakeReconciler) ensureServicePassword(ctx context.Context, in catalogService, secretName, key string, size int) (string, string, error) {
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
	password, err := randHex(size)
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
// first, in one pass), then — once they are gone, so nothing can recreate them — the PVCs left
// behind, and once those are gone too, the generated password Secret: the credential is the last
// thing to go, never before the data it opens. It reports what remains; only objects
// passing the ownership rule are deleted, and a PVC only when the service provably ran here (an
// owned object was seen, or service_status carries its endpoint).
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

	if secretName, _ := kind.passwordSecret(in.name); secretName != "" {
		if err := r.deleteServicePassword(ctx, in, secretName); err != nil {
			return serviceResult{}, false, err
		}
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
// catalogWaitExplainer is implemented by a kind that can say why its upstream resources are not
// ready, in the upstream operator's own words.
type catalogWaitExplainer interface {
	waitingReason(ctx context.Context, c client.Reader, in catalogService, found map[string]*unstructured.Unstructured) (string, error)
}

// serviceWarningReason returns the most recent Warning event about one of a service's objects, in
// Kubernetes' own words. It is how a kind whose upstream operator records nothing useful still
// says why it is stuck: a pod refused by Pod Security or by a quota appears only as a FailedCreate
// event on the StatefulSet or ReplicaSet that tried to create it. Read uncached, and only for a
// service that is not ready.
func serviceWarningReason(ctx context.Context, c client.Reader, in catalogService) (string, error) {
	events := &unstructured.UnstructuredList{}
	events.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "EventList"})
	if err := c.List(ctx, events, client.InNamespace(in.namespace)); err != nil {
		return "", fmt.Errorf("list the namespace's events: %w", err)
	}
	slug := ""
	if in.svc != nil {
		slug = in.svc.Slug
	}
	var newest, reason string
	for _, event := range events.Items {
		if kind, _, _ := unstructured.NestedString(event.Object, "type"); kind != corev1.EventTypeWarning {
			continue
		}
		object, _, _ := unstructured.NestedString(event.Object, "involvedObject", "name")
		if !strings.Contains(object, in.name) && (slug == "" || !strings.Contains(object, slug)) {
			continue
		}
		message, _, _ := unstructured.NestedString(event.Object, "message")
		// A controller that has merely not caught up is not a reason.
		if message == "" || strings.Contains(message, "has not caught up yet") || strings.Contains(message, "no pods were ready to be started") {
			continue
		}
		when, _, _ := unstructured.NestedString(event.Object, "lastTimestamp")
		if when == "" {
			when, _, _ = unstructured.NestedString(event.Object, "eventTime")
		}
		if when >= newest {
			newest, reason = when, message
		}
	}
	return boundedReason(reason), nil
}

// serviceResources is a container's requests and limits as an upstream resource takes them.
// Every container of every kind states them: a container that states nothing takes the workspace
// namespace's LimitRange default, which is how a three-container Kafka came to cost three CPUs.
func serviceResources(requestCPU, requestMemory, limitCPU, limitMemory string) map[string]interface{} {
	return map[string]interface{}{
		"requests": map[string]interface{}{"cpu": requestCPU, "memory": requestMemory},
		"limits":   map[string]interface{}{"cpu": limitCPU, "memory": limitMemory},
	}
}

// restrictedContainer is a named container override that carries the `restricted` security
// context, and resources when given.
func restrictedContainer(name string, resources map[string]interface{}) map[string]interface{} {
	container := map[string]interface{}{"name": name, "securityContext": restrictedContainerSecurityContext()}
	if resources != nil {
		container["resources"] = resources
	}
	return container
}

// conditionMessage returns the message of a condition that is True, or "".
func conditionMessage(obj map[string]interface{}, conditionType string) string {
	conditions, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if ok && condition["type"] == conditionType && condition["status"] == string(metav1.ConditionTrue) {
			message, _ := condition["message"].(string)
			return message
		}
	}
	return ""
}

// serviceWaitReasonLimit bounds an upstream message stored in service_status.
const serviceWaitReasonLimit = 700

func boundedReason(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > serviceWaitReasonLimit {
		text = text[:serviceWaitReasonLimit] + "..."
	}
	return text
}

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
