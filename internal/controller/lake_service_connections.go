/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// Service connections on demand (operator#773, docs/lake/services.md#connections-on-demand).
//
// Every managed service's and store's connection used to live in one Secret,
// lake-<lake>-services, mounted into every Lake pod: one file read in the Lake process gave every
// tenant's database passwords. Now:
//
//   - each connection is its own Secret, lake-<lake>-<slug>-connection, in the service's own
//     workspace namespace. Only the operator reads it; an app of that workspace is given a copy
//     of the connections it binds, and never one from another workspace's namespace;
//   - a lake image whose manifest lists LAKE_SERVICE_CONNECTION_URL asks the operator's control
//     plane for one connection when it needs it (POST /v1/service_connection), with a credential
//     of its own, lake-<lake>-connections. For that image the operator mounts no service
//     credential into the Lake pod, and removes the aggregate Secret once no Lake pod mounts it;
//   - an older image keeps the aggregate mount exactly as before, so the operator can roll first.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// The variables an on-demand lake image reads (lake's manifest environment lists the first).
	lakeServiceConnectionURLEnv       = "LAKE_SERVICE_CONNECTION_URL"
	lakeServiceConnectionTokenFileEnv = "LAKE_SERVICE_CONNECTION_TOKEN_FILE"
	lakeServiceConnectionCAFileEnv    = "LAKE_SERVICE_CONNECTION_CA_FILE"

	// The Lake pod's projected volume: its connection credential and the control plane's CA.
	lakeServiceConnectionVolume    = "service-connection"
	lakeServiceConnectionMountPath = "/var/run/lake/service-connection"
	lakeServiceConnectionTokenKey  = "token"
	lakeServiceConnectionCAKey     = "ca.crt"

	// lakeServiceConnectionTokenPrefix tells this credential apart from the admin and deploy ones.
	lakeServiceConnectionTokenPrefix = "lake_conn_"

	// lakeServiceConnectionPath is the control-plane operation lake calls.
	lakeServiceConnectionPath = "/v1/service_connection"

	// The per-service connection Secret's key and labels. The labels are their own, never
	// serviceIDLabel, so the Secret is not counted as a live service resource.
	serviceConnectionKey        = "connection.json"
	serviceConnectionLakeLabel  = "orama.cloud/connection-lake"
	serviceConnectionSlugLabel  = "orama.cloud/connection-slug"
	serviceConnectionIDLabel    = "orama.cloud/connection-service-id"
	serviceConnectionKindLabel  = "orama.cloud/connection"
	serviceConnectionKindValue  = "service"
	serviceConnectionRequestMax = 4 << 10

	// The control plane's NetworkPolicy admits these to its port (chart
	// templates/control-plane-network-policy.yaml): the Lake namespace, and in it the Lake pods of
	// an on-demand image only.
	serviceConnectionClientsNamespaceLabel = "orama.cloud/service-connection-clients"
	serviceConnectionClientPodLabel        = "orama.cloud/service-connection-client"
)

// lakeConnectionsSecret is the Lake's credential for POST /v1/service_connection. It grants only
// that operation, only for this Lake's own services; it is neither the admin nor the deploy token.
func lakeConnectionsSecret(name string) string { return "lake-" + name + "-connections" }

// lakeControlPlaneCAConfigMap holds the control plane's public CA certificate in the Lake namespace.
func lakeControlPlaneCAConfigMap(name string) string { return "lake-" + name + "-control-plane-ca" }

// serviceConnectionSecret names a service's or store's connection Secret in its workspace namespace.
func serviceConnectionSecret(lake, slug string) string {
	return "lake-" + lake + "-" + slug + "-connection"
}

// serviceConnectionLabels are the labels the control plane and the app bindings find a connection by.
func serviceConnectionLabels(lake *oramav1alpha1.Lake, slug, id string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "orama-operator",
		serviceConnectionKindLabel:     serviceConnectionKindValue,
		serviceConnectionLakeLabel:     lake.Name,
		serviceConnectionSlugLabel:     slug,
	}
	// A service or store id is a UUID; one that is not a valid label value is left off, so the
	// connection is still delivered to lake but no app can bind it by that id.
	if id != "" && len(validation.IsValidLabelValue(id)) == 0 {
		labels[serviceConnectionIDLabel] = id
	}
	return labels
}

// lakeServiceConnectionsOnDemand reports whether this Lake's lake asks for connections on demand:
// the operator serves the operation (url is set) and the manifest of spec.image says the image
// reads LAKE_SERVICE_CONNECTION_URL. Anything else, including a manifest of another image, keeps
// the aggregate mount.
func lakeServiceConnectionsOnDemand(lake *oramav1alpha1.Lake, manifest *oramav1alpha1.LakeImageManifest, url string) bool {
	return url != "" && manifest != nil && manifest.Supported && manifest.ServiceConnectionsOnDemand &&
		manifest.Image != "" && manifest.Image == lake.Spec.Image
}

// serviceConnectionPort is the control plane's port, from its URL (443 when the URL names none).
func serviceConnectionPort(raw string) (int32, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return 0, fmt.Errorf("service connection URL %q must be an https URL", raw)
	}
	if parsed.Port() == "" {
		return 443, nil
	}
	port, err := strconv.ParseInt(parsed.Port(), 10, 32)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("service connection URL %q has an invalid port", raw)
	}
	return int32(port), nil
}

// reconcileLakeServiceConnectionAccess gives the Lake what an on-demand image needs to call the
// operator: its credential (minted once), the control plane's CA certificate and the namespace
// label the control plane's NetworkPolicy admits. It is the same for every image, so an image
// switching to on-demand finds them already there.
func (r *LakeReconciler) reconcileLakeServiceConnectionAccess(ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string) error {
	if r.ServiceConnectionURL == "" {
		return nil
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeConnectionsSecret(lake.Name), Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labels
		secret.Type = corev1.SecretTypeOpaque
		if len(secret.Data[lakeServiceConnectionTokenKey]) == 0 {
			token, err := randHex(32)
			if err != nil {
				return err
			}
			secret.Data = map[string][]byte{lakeServiceConnectionTokenKey: []byte(lakeServiceConnectionTokenPrefix + token)}
		}
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconcile service connection credential: %w", err)
	}
	if r.ServiceConnectionCAFile != "" {
		ca, err := os.ReadFile(r.ServiceConnectionCAFile)
		if err != nil {
			return fmt.Errorf("read the control plane CA certificate: %w", err)
		}
		configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: lakeControlPlaneCAConfigMap(lake.Name), Namespace: ns}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
			configMap.Labels = labels
			configMap.Data = map[string]string{lakeServiceConnectionCAKey: string(ca)}
			return controllerutil.SetControllerReference(lake, configMap, r.Scheme)
		}); err != nil {
			return fmt.Errorf("reconcile control plane CA ConfigMap: %w", err)
		}
	}
	// The namespace label, read uncached: the manager holds no Namespace informer (operator#106).
	reader := lakeAPIReader(r)
	var namespace corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		return fmt.Errorf("read namespace %s: %w", ns, err)
	}
	if namespace.Labels[serviceConnectionClientsNamespaceLabel] == "true" {
		return nil
	}
	patch := client.MergeFrom(namespace.DeepCopy())
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	namespace.Labels[serviceConnectionClientsNamespaceLabel] = "true"
	if err := r.Patch(ctx, &namespace, patch); err != nil {
		return fmt.Errorf("label namespace %s for the service connection operation: %w", ns, err)
	}
	return nil
}

// lakeServiceConnectionEgress lets the Lake pods reach the control plane's port in the operator
// namespace. Nil when the operation is not served or the operator namespace is unknown.
func (r *LakeReconciler) lakeServiceConnectionEgress(lakeSelector metav1.LabelSelector) *oramav1alpha1.NetworkSandboxPolicy {
	if r.ServiceConnectionURL == "" || r.OperatorNamespace == "" {
		return nil
	}
	port, err := serviceConnectionPort(r.ServiceConnectionURL)
	if err != nil {
		return nil
	}
	operatorPeer := namespacePodPeer(r.OperatorNamespace, map[string]string{"app.kubernetes.io/name": "orama-operator"})
	allowance := sandboxAllowance(
		"Lake asks the Orama operator's control plane for one service connection at a time (operator#773).",
		networkPolicy(
			"allow-lake-to-operator-connections",
			lakeSelector,
			[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			nil,
			[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{operatorPeer}, Ports: tcpPorts(port)}},
		),
	)
	return &allowance
}

// applyLakeServiceConnectionsOnDemand turns a Lake pod template built for the aggregate mount
// into the on-demand form: no services volume and no LAKE_SERVICE_DIR; instead the operation's
// URL, its credential and CA as files, and the pod label the control plane admits.
func applyLakeServiceConnectionsOnDemand(lake *oramav1alpha1.Lake, template *corev1.PodTemplateSpec, serviceURL string) {
	spec := &template.Spec
	volumes := spec.Volumes[:0]
	for _, volume := range spec.Volumes {
		if volume.Name != lakeServicesVolume {
			volumes = append(volumes, volume)
		}
	}
	spec.Volumes = volumes
	container := &spec.Containers[0]
	mounts := container.VolumeMounts[:0]
	for _, mount := range container.VolumeMounts {
		if mount.Name != lakeServicesVolume {
			mounts = append(mounts, mount)
		}
	}
	container.VolumeMounts = mounts
	env := container.Env[:0]
	for _, variable := range container.Env {
		switch variable.Name {
		case lakeServiceDirEnv, lakeServiceConnectionURLEnv, lakeServiceConnectionTokenFileEnv, lakeServiceConnectionCAFileEnv:
			continue
		}
		env = append(env, variable)
	}
	container.Env = append(env,
		corev1.EnvVar{Name: lakeServiceConnectionURLEnv, Value: strings.TrimRight(serviceURL, "/") + lakeServiceConnectionPath},
		corev1.EnvVar{Name: lakeServiceConnectionTokenFileEnv, Value: lakeServiceConnectionMountPath + "/" + lakeServiceConnectionTokenKey},
		corev1.EnvVar{Name: lakeServiceConnectionCAFileEnv, Value: lakeServiceConnectionMountPath + "/" + lakeServiceConnectionCAKey},
	)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      lakeServiceConnectionVolume,
		MountPath: lakeServiceConnectionMountPath,
		ReadOnly:  true,
	})
	mode := int32(0o440) // readable by lake through its fsGroup
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: lakeServiceConnectionVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: &mode,
			Sources: []corev1.VolumeProjection{
				{Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: lakeConnectionsSecret(lake.Name)},
					Items:                []corev1.KeyToPath{{Key: lakeServiceConnectionTokenKey, Path: lakeServiceConnectionTokenKey}},
				}},
				{ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: lakeControlPlaneCAConfigMap(lake.Name)},
					Items:                []corev1.KeyToPath{{Key: lakeServiceConnectionCAKey, Path: lakeServiceConnectionCAKey}},
				}},
			},
		}},
	})
	template.Labels = withLabels(template.Labels, map[string]string{serviceConnectionClientPodLabel: "true"})
}

// lakeDeploymentMountsServices reports whether any Lake pod may still read the aggregate Secret:
// the Deployment's template mounts it, or a rollout away from that template is not complete.
func lakeDeploymentMountsServices(deployment *appsv1.Deployment) bool {
	if deployment == nil {
		return true
	}
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == lakeServicesVolume {
			return true
		}
	}
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	status := deployment.Status
	return status.ObservedGeneration < deployment.Generation ||
		status.UpdatedReplicas != replicas || status.Replicas != replicas
}

// lakeAggregateServicesNeeded reports whether lake-<lake>-services must still be written: always
// for an image that reads the mount, and for an on-demand image until no Lake pod mounts it.
func (r *LakeReconciler) lakeAggregateServicesNeeded(ctx context.Context, lake *oramav1alpha1.Lake, namespace string) bool {
	if !lakeServiceConnectionsOnDemand(lake, lake.Status.ImageManifest, r.ServiceConnectionURL) {
		return true
	}
	var deployment appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "lake-" + lake.Name}, &deployment); err != nil {
		return true
	}
	return lakeDeploymentMountsServices(&deployment)
}

// removeLakeAggregateServicesSecret deletes lake-<lake>-services once nothing reads it.
func (r *LakeReconciler) removeLakeAggregateServicesSecret(ctx context.Context, lake *oramav1alpha1.Lake, namespace string) error {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeServicesSecret(lake.Name)}, &secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&secret, lake) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &secret))
}

// serviceConnectionTarget is where one slug's connection Secret belongs.
type serviceConnectionTarget struct {
	namespace string
	id        string
}

// reconcileServiceConnectionSecrets writes each fresh connection into its own Secret in its
// workspace namespace and removes the connection Secrets of slugs keep refuses. A slug that is
// kept but has no fresh credential this pass keeps its Secret. It returns the slugs written.
func (r *LakeReconciler) reconcileServiceConnectionSecrets(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	keep func(slug string) bool,
	credentials map[string][]byte,
	targets map[string]serviceConnectionTarget,
) (map[string]bool, error) {
	written := map[string]bool{}
	var errs []error
	for slug, credential := range credentials {
		target, found := targets[slug]
		if !found || target.namespace == "" {
			continue
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: serviceConnectionSecret(lake.Name, slug), Namespace: target.namespace}}
		if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); err == nil && secret.Labels[serviceConnectionLakeLabel] != lake.Name {
			errs = append(errs, fmt.Errorf("secret %s/%s exists and is not Lake %s's connection", secret.Namespace, secret.Name, lake.Name))
			continue
		} else if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			secret.Labels = serviceConnectionLabels(lake, slug, target.id)
			secret.Type = corev1.SecretTypeOpaque
			secret.Data = map[string][]byte{serviceConnectionKey: credential}
			return nil
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		written[slug] = true
	}
	var existing corev1.SecretList
	if err := r.List(ctx, &existing, client.MatchingLabels{
		serviceConnectionKindLabel: serviceConnectionKindValue,
		serviceConnectionLakeLabel: lake.Name,
	}); err != nil {
		errs = append(errs, err)
		return written, errors.Join(errs...)
	}
	for i := range existing.Items {
		secret := &existing.Items[i]
		slug := secret.Labels[serviceConnectionSlugLabel]
		target, found := targets[slug]
		stale := !keep(slug) || (found && target.namespace != "" && target.namespace != secret.Namespace)
		if !stale {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return written, errors.Join(errs...)
}

// removeServiceConnectionSecrets drops every connection Secret of one slug of this Lake.
func removeServiceConnectionSecrets(ctx context.Context, c client.Client, lake *oramav1alpha1.Lake, slug string) error {
	var existing corev1.SecretList
	if err := c.List(ctx, &existing, client.MatchingLabels{
		serviceConnectionKindLabel: serviceConnectionKindValue,
		serviceConnectionLakeLabel: lake.Name,
		serviceConnectionSlugLabel: slug,
	}); err != nil {
		return err
	}
	for i := range existing.Items {
		if err := c.Delete(ctx, &existing.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// serviceConnectionByID reads the connection of the service with this id from one namespace only.
// An app binding and the workspace readiness check use it, so a connection is found only in its
// own workspace's namespace.
func serviceConnectionByID(ctx context.Context, reader client.Reader, lake *oramav1alpha1.Lake, namespace, id string) ([]byte, error) {
	var existing corev1.SecretList
	if err := reader.List(ctx, &existing, client.InNamespace(namespace), client.MatchingLabels{
		serviceConnectionKindLabel: serviceConnectionKindValue,
		serviceConnectionLakeLabel: lake.Name,
		serviceConnectionIDLabel:   id,
	}); err != nil {
		return nil, err
	}
	if len(existing.Items) != 1 {
		return nil, nil
	}
	return existing.Items[0].Data[serviceConnectionKey], nil
}

// serviceConnectionRequest is POST /v1/service_connection's body.
type serviceConnectionRequest struct {
	Service string `json:"service"`
}

// serveServiceConnection answers one connection of the calling Lake's own services. The caller is
// the Lake whose lake-<lake>-connections token it presents; the connection is found only among
// that Lake's connection Secrets, only in a namespace that is one of its workspaces. Every answer
// is logged without its value.
func (s *DeploymentControlPlane) serveServiceConnection(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		writeCapabilityError(writer, &capabilityError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "use POST"})
		return
	}
	ctx := request.Context()
	lake, authError := s.authenticateServiceConnection(ctx, request.Header.Get("Authorization"))
	if authError != nil {
		s.logger().Info("service connection refused", "outcome", authError.Code)
		writeCapabilityError(writer, authError)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, serviceConnectionRequestMax+1))
	if err != nil || len(body) > serviceConnectionRequestMax {
		writeCapabilityError(writer, invalidInput("the request body is unreadable or too large", "/"))
		return
	}
	var input serviceConnectionRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeCapabilityError(writer, invalidInput("the request body must be {\"service\": \"<slug>\"}", "/"))
		return
	}
	if !serviceSlugPattern.MatchString(input.Service) && !storeSlugPattern.MatchString(input.Service) {
		writeCapabilityError(writer, invalidInput("service must be a service or store slug", "/service"))
		return
	}
	log := s.logger().WithValues("lake", lake.Name, "service", input.Service)
	connection, lookupError := s.lookupServiceConnection(ctx, lake, input.Service)
	if lookupError != nil {
		log.Info("service connection refused", "outcome", lookupError.Code)
		writeCapabilityError(writer, lookupError)
		return
	}
	log.Info("service connection delivered")
	writeJSON(writer, http.StatusOK, map[string]any{"service": input.Service, "connection": json.RawMessage(connection)})
}

// authenticateServiceConnection finds the Lake whose connection credential is the bearer.
func (s *DeploymentControlPlane) authenticateServiceConnection(ctx context.Context, authorization string) (*oramav1alpha1.Lake, *capabilityError) {
	token, err := bearerToken(authorization)
	if err != nil || !strings.HasPrefix(token, lakeServiceConnectionTokenPrefix) {
		return nil, &capabilityError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "a Lake service connection credential is required", Path: "/authorization"}
	}
	var lakes oramav1alpha1.LakeList
	if err := s.reader().List(ctx, &lakes); err != nil {
		return nil, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "Lake credentials could not be read"}
	}
	for i := range lakes.Items {
		lake := &lakes.Items[i]
		var secret corev1.Secret
		key := client.ObjectKey{Namespace: lakeWorkloadNamespace(lake), Name: lakeConnectionsSecret(lake.Name)}
		if err := s.reader().Get(ctx, key, &secret); err != nil || !metav1.IsControlledBy(&secret, lake) {
			continue
		}
		candidate := secret.Data[lakeServiceConnectionTokenKey]
		if len(candidate) > 0 && subtle.ConstantTimeCompare(candidate, []byte(token)) == 1 {
			return lake.DeepCopy(), nil
		}
	}
	return nil, &capabilityError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "the service connection credential was rejected", Path: "/authorization"}
}

// lookupServiceConnection reads the one connection Secret of this Lake and slug, and checks it is
// in a namespace that is this Lake's workspace.
func (s *DeploymentControlPlane) lookupServiceConnection(ctx context.Context, lake *oramav1alpha1.Lake, slug string) (json.RawMessage, *capabilityError) {
	notFound := &capabilityError{Status: http.StatusNotFound, Code: "not_found", Message: "no connection for " + slug + " on this Lake", Path: "/service"}
	var existing corev1.SecretList
	if err := s.reader().List(ctx, &existing, client.MatchingLabels{
		serviceConnectionKindLabel: serviceConnectionKindValue,
		serviceConnectionLakeLabel: lake.Name,
		serviceConnectionSlugLabel: slug,
	}); err != nil {
		return nil, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "service connections could not be read"}
	}
	if len(existing.Items) != 1 {
		return nil, notFound
	}
	secret := &existing.Items[0]
	if secret.Name != serviceConnectionSecret(lake.Name, slug) {
		return nil, notFound
	}
	var namespace corev1.Namespace
	if err := s.reader().Get(ctx, client.ObjectKey{Name: secret.Namespace}, &namespace); err != nil {
		return nil, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "the connection's namespace could not be read"}
	}
	if namespace.Labels[workspaceLakeLabel] != lake.Name {
		return nil, notFound
	}
	connection := secret.Data[serviceConnectionKey]
	if len(connection) == 0 || !json.Valid(connection) {
		return nil, notFound
	}
	return connection, nil
}
