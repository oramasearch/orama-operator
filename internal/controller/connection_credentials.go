/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Credentials on connections (operator#469, design decision D4, Postgres first). A Connection with
// a credential scope gets a role of its own in the target service's datastore, with exactly the
// grants of the scope, and its credential is written as a workspace secret. The service's owner
// credential is never handed to the app. A new Lake generation rotates the credential; setting the
// row absent, deleting the Connection or removing the scope drops the role and ends its sessions in
// the same reconcile.

const (
	connectionCredentialRequeue = 10 * time.Second

	pendingPostgresCluster  = "waiting for the target's Postgres cluster"
	pendingPostgresDeleting = "the target's Postgres cluster is being deleted"
)

var (
	// connectionSchemaPattern is a plain, unquoted Postgres identifier.
	connectionSchemaPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)
	// errConnectionCredentialUnsupported: the target's kind cannot carry a credential yet.
	errConnectionCredentialUnsupported = errors.New("credential scopes are implemented for Postgres services")
)

// connectionCredentialState is the credential half of ConnectionStatus.
type connectionCredentialState struct {
	Secret     string
	Generation int64
	Role       string
}

func credentialStateOf(connection *oramav1alpha1.Connection) connectionCredentialState {
	return connectionCredentialState{
		Secret:     connection.Status.CredentialSecret,
		Generation: connection.Status.CredentialGeneration,
		Role:       connection.Status.CredentialRole,
	}
}

// connectionPostgresService is the resolved target of a credential: the service's CloudNativePG
// cluster and the operator's superuser sign-in to it.
type connectionPostgresService struct {
	Cluster string
	Host    string
	Admin   ConnectionPostgresTarget
}

func (r *ConnectionReconciler) roleManager() PostgresRoleManager {
	if r.Roles != nil {
		return r.Roles
	}
	return pgxPostgresRoleManager{}
}

func (r *ConnectionReconciler) secretWriter() WorkspaceSecretWriter {
	if r.Secrets != nil {
		return r.Secrets
	}
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	return &lakeHTTPSecretWriter{Reader: reader}
}

func validConnectionScope(scope *oramav1alpha1.ConnectionCredentialScope) bool {
	if scope == nil {
		return true
	}
	if scope.Access != oramav1alpha1.ConnectionAccessRead && scope.Access != oramav1alpha1.ConnectionAccessWrite {
		return false
	}
	return scope.Schema == "" || connectionSchemaPattern.MatchString(scope.Schema)
}

// postgresServiceFor resolves the target service's CloudNativePG cluster. It answers a
// connectionPendingError while the cluster or its superuser Secret is not there yet, and a plain
// error when the target cannot carry a credential.
func (r *ConnectionReconciler) postgresServiceFor(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
	lake *oramav1alpha1.Lake,
) (connectionPostgresService, error) {
	clusters := &unstructured.UnstructuredList{}
	clusters.SetGroupVersionKind(cnpgClusterListGVK)
	if err := r.List(ctx, clusters, client.InNamespace(connection.Namespace), client.MatchingLabels{
		serviceIDLabel: connection.Spec.TargetID,
	}); err != nil {
		return connectionPostgresService{}, err
	}
	var cluster *unstructured.Unstructured
	for i := range clusters.Items {
		if metav1.IsControlledBy(&clusters.Items[i], lake) {
			cluster = &clusters.Items[i]
			break
		}
	}
	if cluster == nil {
		kind, err := r.targetServiceKind(ctx, connection)
		if err != nil {
			return connectionPostgresService{}, err
		}
		if kind != "" && kind != serviceKindPostgres {
			return connectionPostgresService{}, fmt.Errorf(
				"%w; %s services follow in their own issue", errConnectionCredentialUnsupported, kind)
		}
		return connectionPostgresService{}, connectionPendingError{message: pendingPostgresCluster}
	}
	if cluster.GetDeletionTimestamp() != nil {
		return connectionPostgresService{}, connectionPendingError{message: pendingPostgresDeleting}
	}
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	if ready < 1 {
		return connectionPostgresService{}, connectionPendingError{message: "waiting for the target's Postgres instances"}
	}
	var superuser corev1.Secret
	key := client.ObjectKey{Namespace: connection.Namespace, Name: cluster.GetName() + "-superuser"}
	if err := r.Get(ctx, key, &superuser); err != nil {
		if apierrors.IsNotFound(err) {
			return connectionPostgresService{}, connectionPendingError{message: "waiting for the target's Postgres superuser Secret"}
		}
		return connectionPostgresService{}, err
	}
	user, password := string(superuser.Data["username"]), string(superuser.Data["password"])
	if user == "" || password == "" {
		return connectionPostgresService{}, connectionPendingError{message: "waiting for the target's Postgres superuser Secret"}
	}
	host := fmt.Sprintf("%s-rw.%s.svc.cluster.local", cluster.GetName(), connection.Namespace)
	return connectionPostgresService{
		Cluster: cluster.GetName(),
		Host:    host,
		Admin: ConnectionPostgresTarget{
			Host:     host,
			Port:     servicePostgresPort,
			Database: servicePostgresDatabase,
			User:     user,
			Password: password,
		},
	}, nil
}

// targetServiceKind reads the kind of the target service from the label every service resource
// carries, or "" when none is found.
func (r *ConnectionReconciler) targetServiceKind(ctx context.Context, connection *oramav1alpha1.Connection) (string, error) {
	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(connection.Namespace), client.MatchingLabels{
		serviceIDLabel: connection.Spec.TargetID,
	}); err != nil {
		return "", err
	}
	for i := range services.Items {
		if kind := services.Items[i].Labels[serviceKindLabel]; kind != "" {
			return kind, nil
		}
	}
	return "", nil
}

// reconcileCredential issues, rotates or re-scopes the connection's credential and returns the
// credential state to record. It runs only after the connection's network path is rendered.
func (r *ConnectionReconciler) reconcileCredential(
	ctx context.Context,
	connection *oramav1alpha1.Connection,
) (connectionCredentialState, error) {
	state := credentialStateOf(connection)
	scope := connection.Spec.CredentialScope
	if scope == nil {
		if state.Role == "" {
			return connectionCredentialState{}, nil
		}
		// The scope was removed: the credential goes with it.
		if err := r.revokeCredential(ctx, connection); err != nil {
			return state, err
		}
		return connectionCredentialState{}, nil
	}
	if connection.Spec.SourceKind != oramav1alpha1.ConnectionSourceApp ||
		connection.Spec.TargetKind != oramav1alpha1.ConnectionTargetService {
		return state, errors.New("a credential scope is valid only from an app to a service")
	}
	if !validConnectionScope(scope) {
		return state, errors.New("credential scope access must be read or write, with an optional schema name")
	}
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: connection.Spec.LakeRef}, &lake); err != nil {
		return state, err
	}
	service, err := r.postgresServiceFor(ctx, connection, &lake)
	if err != nil {
		return state, err
	}
	role := connectionRoleName(connection.Spec.ConnectionID)
	roles := r.roleManager()
	exists, err := roles.RoleExists(ctx, service.Admin, role)
	if err != nil {
		return state, connectionPendingError{message: "cannot reach the target's Postgres: " + err.Error()}
	}
	issued := exists && state.Role == role && state.Secret != "" && state.Generation == connection.Spec.Generation
	if issued {
		if err := roles.ApplyScope(ctx, service.Admin, role, *scope); err != nil {
			return state, scopeError(err)
		}
		return state, nil
	}

	// Issue or rotate: a fresh password for this generation. The role is changed first and the
	// workspace secret second, so a failure between the two is repaired by the next reconcile
	// issuing again, never by a stored credential the role does not accept.
	password, err := newConnectionPassword()
	if err != nil {
		return state, err
	}
	verifier, err := scramSHA256Verifier(password)
	if err != nil {
		return state, err
	}
	if err := roles.SetRole(ctx, service.Admin, role, verifier, *scope); err != nil {
		return state, scopeError(err)
	}
	uri := url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(role, password),
		Host:   net.JoinHostPort(service.Host, strconv.Itoa(servicePostgresPort)),
		Path:   "/" + servicePostgresDatabase,
	}
	value := map[string]any{
		"kind":     serviceKindPostgres,
		"host":     service.Host,
		"port":     servicePostgresPort,
		"database": servicePostgresDatabase,
		"user":     role,
		"password": password,
		"uri":      uri.String(),
	}
	metadata := map[string]string{
		"managed_by":    "orama-operator",
		"connection_id": connection.Spec.ConnectionID,
		"generation":    strconv.FormatInt(connection.Spec.Generation, 10),
		"role":          role,
		"access":        scope.Access,
		"schema":        scope.Schema,
	}
	reference, err := r.secretWriter().PutWorkspaceSecret(
		ctx,
		&lake,
		connection.Spec.CustomerID,
		connection.Spec.WorkspaceID,
		connectionCredentialSecretName(connection.Spec.ConnectionID),
		value,
		metadata,
	)
	if err != nil {
		return state, connectionPendingError{message: "cannot write the workspace credential secret: " + err.Error()}
	}
	return connectionCredentialState{Secret: reference, Generation: connection.Spec.Generation, Role: role}, nil
}

func scopeError(err error) error {
	if errors.Is(err, errConnectionSchemaMissing) {
		return connectionPendingError{message: "waiting for " + err.Error()}
	}
	return connectionPendingError{message: "cannot apply the credential role: " + err.Error()}
}

// revokeCredential drops the connection's role, ending its sessions. A target whose cluster is
// gone has no role left to drop.
func (r *ConnectionReconciler) revokeCredential(ctx context.Context, connection *oramav1alpha1.Connection) error {
	if connection.Spec.CredentialScope == nil && connection.Status.CredentialRole == "" {
		return nil
	}
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: connection.Spec.LakeRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	service, err := r.postgresServiceFor(ctx, connection, &lake)
	if err != nil {
		// No cluster, a cluster being deleted, or a kind that never carried a credential: there
		// is no role left to drop. Anything else is retried, because the role may still exist.
		var pending connectionPendingError
		if errors.Is(err, errConnectionCredentialUnsupported) ||
			errors.As(err, &pending) && (pending.message == pendingPostgresCluster || pending.message == pendingPostgresDeleting) {
			return nil
		}
		return fmt.Errorf("revoke the connection credential: %w", err)
	}
	role := connectionRoleName(connection.Spec.ConnectionID)
	if err := r.roleManager().DropRole(ctx, service.Admin, role); err != nil {
		return fmt.Errorf("drop the connection role: %w", err)
	}
	return nil
}
