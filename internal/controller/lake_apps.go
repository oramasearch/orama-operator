/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeAppsPoll      = 60 * time.Second
	lakeAppRowIDLabel = "orama.cloud/app-id"
)

type LakeAppsReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	APIReader client.Reader
	AppStores LakeAppsStoreOpener
	Events    <-chan event.GenericEvent
}

func lakeAppsEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Apps.Enabled && lakePostgresEnabled(lake) && lake.DeletionTimestamp.IsZero()
}

func (r *LakeAppsReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, request.NamespacedName, &lake); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !lake.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if err := r.reconcileLakeAppsDNS(ctx, &lake); err != nil {
		return ctrl.Result{}, err
	}
	if !lakeAppsEnabled(&lake) {
		return ctrl.Result{}, nil
	}
	parent := &LakeReconciler{Client: r.Client, Scheme: r.Scheme}
	_, _, schema := parent.lakeSchemaStatus(ctx, &lake, lakeNamespace(&lake))
	if schema.phase != "Ready" {
		return ctrl.Result{RequeueAfter: lakeServicesChangingPoll}, nil
	}
	changing := r.pollLakeApps(ctx, &lake)
	if changing {
		return ctrl.Result{RequeueAfter: lakeServicesChangingPoll}, nil
	}
	return ctrl.Result{RequeueAfter: lakeAppsPoll}, nil
}

func (r *LakeAppsReconciler) pollLakeApps(ctx context.Context, lake *oramav1alpha1.Lake) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeControlDB(lake.Name) + "-app"}
	if err := r.Get(ctx, key, &secret); err != nil || len(secret.Data["uri"]) == 0 {
		log.V(1).Info("apps deferred: control DB credential unavailable")
		return false
	}
	passCtx, cancel := context.WithTimeout(ctx, servicePassTimeout)
	defer cancel()
	open := r.AppStores
	if open == nil {
		open = openPgxAppsStore
	}
	store, err := open(passCtx, string(secret.Data["uri"]))
	if err != nil {
		log.Info("apps deferred: cannot connect to control DB")
		return false
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = store.Close(closeCtx)
	}()
	rows, err := store.ListApps(passCtx)
	if err != nil {
		log.Info("apps deferred: complete apps list unavailable")
		return false
	}
	services, err := store.ListServices(passCtx)
	if err != nil {
		log.Info("apps deferred: service identities unavailable")
		return false
	}
	// The stores apps mount (lake#666). Without the store_mounts table (a schema before 0080) no
	// app mounts any; once it exists, a failed read defers the apps, so a transient read never
	// takes a mount out of an app's pods.
	var storeRows lakeAppStoreRows
	mounts, err := store.ListStoreMounts(passCtx)
	switch {
	case errors.Is(err, errStoreMountsTableMissing):
	case err != nil:
		log.Info("apps deferred: store mounts unavailable")
		return false
	default:
		stores, err := store.ListStores(passCtx)
		if err != nil {
			log.Info("apps deferred: stores unavailable")
			return false
		}
		storeRows = lakeAppStoreRows{stores: stores, mounts: mounts}
	}
	return r.syncLakeApps(passCtx, lake, store, rows, services, storeRows)
}

// lakeAppStoreRows are the stores and store_mounts rows of one apps pass.
type lakeAppStoreRows struct {
	stores []LakeStore
	mounts []LakeStoreMount
}

func (r *LakeAppsReconciler) syncLakeApps(ctx context.Context, lake *oramav1alpha1.Lake, store LakeAppsStore, rows []LakeAppRow, services []LakeService, storeRows lakeAppStoreRows) bool {
	keep := make(map[string]bool, len(rows))
	changing := false
	for i := range rows {
		row := &rows[i]
		keep[row.ID] = true
		status, err := r.syncLakeAppRow(ctx, lake, store, row, services, storeRows)
		if err != nil {
			logf.FromContext(ctx).Info("app reconciliation deferred", "lake", lake.Name, "app", row.ID)
			changing = true
			continue
		}
		if status.Phase == "Pending" || status.Phase == "Deploying" {
			changing = true
		}
		if status != row.Status {
			updated, err := store.UpdateAppStatus(ctx, row, status)
			if err != nil || !updated {
				changing = true
			}
		}
	}
	var apps oramav1alpha1.LakeAppList
	if err := r.List(ctx, &apps, client.InNamespace(lakeNamespace(lake)), client.MatchingLabels{serviceLakeLabel: lake.Name}); err != nil {
		return true
	}
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.Sync == nil || keep[app.Spec.Sync.ID] || !metav1.IsControlledBy(app, lake) {
			continue
		}
		changing = true
		if err := r.deleteSyncedLakeApp(ctx, app); err != nil {
			logf.FromContext(ctx).Info("orphan app deletion deferred", "lake", lake.Name, "app", app.Spec.Sync.ID)
		}
	}
	return changing
}

func lakeAppRowName(lake string, row *LakeAppRow) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(lake)))[:8]
	return "app-" + hash + "-" + row.ID
}

func (r *LakeAppsReconciler) syncLakeAppRow(ctx context.Context, lake *oramav1alpha1.Lake, store LakeAppsStore, row *LakeAppRow, services []LakeService, storeRows lakeAppStoreRows) (LakeAppRowStatus, error) {
	status := LakeAppRowStatus{Phase: "Pending", Message: "waiting for the LakeApp controller", ObservedGeneration: row.Generation}
	if _, err := uuid.Parse(row.ID); err != nil || row.ID != strings.ToLower(row.ID) || len(row.ID) != 36 {
		status.Phase, status.Message = "Failed", "app id must be a canonical UUID"
		return status, nil
	}
	app := &oramav1alpha1.LakeApp{}
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeAppRowName(lake.Name, row)}
	err := r.Get(ctx, key, app)
	if err != nil && !apierrors.IsNotFound(err) {
		return status, err
	}
	exists := err == nil
	if exists && (!metav1.IsControlledBy(app, lake) || app.Spec.Sync == nil || app.Spec.Sync.ID != row.ID ||
		app.Spec.Sync.CustomerID != row.CustomerID || app.Spec.Sync.WorkspaceID != row.WorkspaceID) {
		status.Phase, status.Message = "Failed", "LakeApp name is occupied by another owner or app"
		return status, nil
	}
	if row.Desired == "deleted" {
		return r.syncDeletedLakeApp(ctx, store, row, app, exists)
	}
	spec, err := lakeAppRowSpec(lake, row, services)
	if err != nil {
		status.Phase, status.Message = "Failed", err.Error()
		return status, nil
	}
	spec.Stores = lakeAppRowStores(lake, row, storeRows)
	if exists && !app.DeletionTimestamp.IsZero() {
		status.Message = "waiting for previous app teardown"
		return status, nil
	}
	if exists {
		spec.Sync.TokenHashes, err = r.registerLakeAppTokens(ctx, store, row, app)
		if err != nil {
			return status, err
		}
	}
	app.Name, app.Namespace = key.Name, key.Namespace
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, app, func() error {
		if app.ResourceVersion != "" && (!metav1.IsControlledBy(app, lake) || app.Spec.Sync == nil || app.Spec.Sync.ID != row.ID) {
			return errors.New("LakeApp ownership changed")
		}
		if app.Labels == nil {
			app.Labels = map[string]string{}
		}
		app.Labels[serviceLakeLabel] = lake.Name
		app.Labels[lakeAppRowIDLabel] = row.ID
		app.Labels[workspaceLabel] = row.WorkspaceID
		app.Spec = spec
		controllerutil.AddFinalizer(app, lakeAppFinalizer)
		return controllerutil.SetControllerReference(lake, app, r.Scheme)
	})
	if err != nil {
		return status, errors.New("cannot apply LakeApp")
	}
	return lakeAppRowObservedStatus(row, app), nil
}

func (r *LakeAppsReconciler) registerLakeAppTokens(ctx context.Context, store LakeAppsStore, row *LakeAppRow, app *oramav1alpha1.LakeApp) (*oramav1alpha1.LakeAppTokenHashes, error) {
	var tokens corev1.Secret
	key := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppTokensName(app)}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.Get(ctx, key, &tokens); apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, errors.New("cannot read app tokens")
	}
	if !lakeAppOwnsTokens(app, &tokens) {
		return nil, errors.New("app tokens have another owner")
	}
	hashes := lakeAppTokenHashes(&tokens)
	if !apiequality.Semantic.DeepEqual(hashes, row.TokenHashes) {
		updated, err := store.UpdateAppTokenHashes(ctx, row, hashes)
		if err != nil || !updated {
			return nil, errors.New("app token registration deferred")
		}
	}
	return hashes, nil
}

func (r *LakeAppsReconciler) syncDeletedLakeApp(ctx context.Context, store LakeAppsStore, row *LakeAppRow, app *oramav1alpha1.LakeApp, exists bool) (LakeAppRowStatus, error) {
	status := LakeAppRowStatus{Phase: "Deploying", Message: "waiting for app teardown", ObservedGeneration: row.Generation}
	if exists {
		return status, r.deleteSyncedLakeApp(ctx, app)
	}
	if row.TokenHashes != nil {
		updated, err := store.UpdateAppTokenHashes(ctx, row, nil)
		if err != nil || !updated {
			return status, errors.New("app token revocation deferred")
		}
	}
	status.Phase, status.Message = "Deleted", "app deleted"
	return status, nil
}

func (r *LakeAppsReconciler) deleteSyncedLakeApp(ctx context.Context, app *oramav1alpha1.LakeApp) error {
	if !app.DeletionTimestamp.IsZero() {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, app, client.Preconditions{UID: &app.UID, ResourceVersion: &app.ResourceVersion}))
}

func lakeAppRowObservedStatus(row *LakeAppRow, app *oramav1alpha1.LakeApp) LakeAppRowStatus {
	status := LakeAppRowStatus{Phase: "Pending", Message: "waiting for the LakeApp controller", ObservedGeneration: row.Generation}
	condition := apimeta.FindStatusCondition(app.Status.Conditions, "Ready")
	if condition == nil || condition.ObservedGeneration != app.Generation {
		return status
	}
	status.URL, status.Message = app.Status.URL, app.Status.Message
	switch app.Status.Phase {
	case oramav1alpha1.LakeAppPhaseReady:
		status.Phase, status.Ready = "Running", app.Status.Ready
	case oramav1alpha1.LakeAppPhaseDegraded:
		status.Phase = "Degraded"
	default:
		status.Phase = "Deploying"
	}
	if condition.Reason == "DeploymentStopped" {
		status.Phase, status.Ready = "Stopped", false
	}
	if condition.Reason == "TokenRegistrationPending" {
		status.Phase = "Pending"
	}
	return status
}

func lakeAppRowSpec(lake *oramav1alpha1.Lake, row *LakeAppRow, services []LakeService) (oramav1alpha1.LakeAppSpec, error) {
	spec := oramav1alpha1.LakeAppSpec{
		LakeRef: lake.Name, Mode: row.Mode, Image: row.Image, Port: row.Port,
		Replicas: &row.Replicas, Auth: row.Auth, Host: row.Host,
		Sync: &oramav1alpha1.LakeAppSync{
			ID: row.ID, CustomerID: row.CustomerID, WorkspaceID: row.WorkspaceID,
			WorkspaceNamespace: row.WorkspaceNamespace, Generation: row.Generation,
		},
	}
	if row.CustomerID == "" || row.Generation < 1 || len(validation.IsDNS1123Label(row.Name)) != 0 {
		return spec, errors.New("app customer, generation or name is invalid")
	}
	if _, err := uuid.Parse(row.CustomerID); err != nil {
		return spec, errors.New("app customer must be a UUID")
	}
	if workspaceID, err := uuid.Parse(row.WorkspaceID); err != nil || workspaceID.String() != row.WorkspaceID ||
		len(validation.IsDNS1123Label(row.WorkspaceNamespace)) != 0 {
		return spec, errors.New("app workspace and its reconciled namespace are required")
	}
	if row.Desired != "running" && row.Desired != "stopped" {
		return spec, errors.New("desired must be running, stopped or deleted")
	}
	if row.Port < 1 || row.Port > 65535 || row.Replicas < 0 {
		return spec, errors.New("app port or replicas is invalid")
	}
	if row.Desired == "stopped" {
		stopped := int32(0)
		spec.Replicas = &stopped
	}
	if spec.Auth == "" {
		spec.Auth = oramav1alpha1.LakeAppAuthLake
	}
	// Lake's apps table stores public access as 'none' (its app_auth_check allows lake|none).
	if spec.Auth == "none" {
		spec.Auth = oramav1alpha1.LakeAppAuthPublic
	}
	if spec.Auth != "lake" && spec.Auth != "public" {
		return spec, errors.New("auth must be lake or public")
	}
	if err := lakeAppRowSource(row, &spec); err != nil {
		return spec, err
	}
	wildcard := strings.TrimPrefix(lake.Spec.Apps.Wildcard, "*.")
	if spec.Host == "" && wildcard != "" {
		spec.Host = row.Name + "." + wildcard
	}
	if wildcard == "" || len(validation.IsDNS1123Subdomain(spec.Host)) != 0 || !strings.HasSuffix(spec.Host, "."+wildcard) || strings.Contains(strings.TrimSuffix(spec.Host, "."+wildcard), ".") {
		return spec, errors.New("app host must be one label beneath the Lake apps wildcard")
	}
	bindings, err := lakeAppRowBindings(row, services)
	if err != nil {
		return spec, err
	}
	spec.Bindings = bindings
	if err := lakeAppRowEnvironment(row, &spec); err != nil {
		return spec, err
	}
	resources := lakeAppResources(&oramav1alpha1.LakeApp{})
	spec.Resources = &resources
	return spec, nil
}

// lakeAppRowStores is the stores the app mounts, from store_mounts: a volume of the app's
// workspace as its claim, a bucket as its connection and the Lake-namespace Secret its credential
// was delivered to.
func lakeAppRowStores(lake *oramav1alpha1.Lake, row *LakeAppRow, storeRows lakeAppStoreRows) []oramav1alpha1.LakeAppStoreMount {
	resolved := withoutReservedPaths(
		resolveStoreMounts(lake, storeRows.stores, storeRows.mounts, StoreMountKindApp, row.ID, row.CustomerID, row.WorkspaceID),
		"/tmp", lakeAppBindingsPath)
	var out []oramav1alpha1.LakeAppStoreMount
	for _, mount := range resolved {
		out = append(out, oramav1alpha1.LakeAppStoreMount{
			ID: mount.StoreID, Name: mount.Name, Slug: mount.Slug, Type: mount.Type, Provider: mount.Provider,
			ClaimName: mount.Claim, MountPath: mount.MountPath, ReadOnly: mount.ReadOnly,
			Bucket: mount.Bucket, Endpoint: mount.Endpoint, Region: mount.Region, Prefix: mount.Prefix,
			SecretName: mount.Secret,
		})
	}
	return out
}

func lakeAppRowSource(row *LakeAppRow, spec *oramav1alpha1.LakeAppSpec) error {
	switch row.Mode {
	case oramav1alpha1.LakeAppModeRuntime:
		if row.SourceRepository == "" || row.SourceRevision == "" {
			return errors.New("runtime source repository and revision are required")
		}
		if row.Framework != "fastapi" && row.Framework != "streamlit" && row.Framework != "static" {
			return errors.New("framework must be fastapi, streamlit or static")
		}
		spec.Image = ""
		spec.Source = &oramav1alpha1.LakeAppSource{
			Repository: row.SourceRepository, Revision: row.SourceRevision, Path: row.SourcePath,
			Entrypoint: row.SourceEntrypoint, Framework: row.Framework,
		}
	case oramav1alpha1.LakeAppModeImage:
		parts := strings.Split(row.Image, "@sha256:")
		if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 || strings.Trim(parts[1], "0123456789abcdefABCDEF") != "" {
			return errors.New("image must be pinned by SHA-256 digest")
		}
	default:
		return errors.New("mode must be runtime or image")
	}
	return nil
}

func lakeAppRowBindings(row *LakeAppRow, services []LakeService) (*oramav1alpha1.LakeAppBindings, error) {
	var desired struct {
		Zetta *struct {
			Scopes []string `json:"scopes"`
		} `json:"zetta"`
		Services []string `json:"services"`
	}
	if row.BindingsJSON != "" && json.Unmarshal([]byte(row.BindingsJSON), &desired) != nil {
		return nil, errors.New("bindings_json must contain zetta scopes and service IDs")
	}
	if desired.Zetta == nil && len(desired.Services) == 0 {
		return nil, nil
	}
	bindings := &oramav1alpha1.LakeAppBindings{ServiceNames: map[string]string{}}
	if desired.Zetta != nil {
		if len(desired.Zetta.Scopes) == 0 {
			return nil, errors.New("zetta binding requires read scopes")
		}
		bindings.Zetta = &oramav1alpha1.LakeAppZettaBinding{CustomerID: row.CustomerID, Scopes: sortedUnique(desired.Zetta.Scopes)}
	}
	names := map[string]bool{}
	for _, id := range sortedUnique(desired.Services) {
		var matched *LakeService
		for i := range services {
			if services[i].ID == id && services[i].CustomerID == row.CustomerID &&
				services[i].WorkspaceID == row.WorkspaceID && services[i].Desired == serviceDesiredPresent {
				matched = &services[i]
				break
			}
		}
		if matched == nil || !validAppServiceName(matched.Name) || names[matched.Name] {
			return nil, errors.New("service binding must name an active service in the same workspace with a unique filename")
		}
		names[matched.Name] = true
		bindings.Services = append(bindings.Services, id)
		bindings.ServiceNames[id] = matched.Name
	}
	return bindings, nil
}

func lakeAppRowEnvironment(row *LakeAppRow, spec *oramav1alpha1.LakeAppSpec) error {
	var env map[string]string
	if row.EnvJSON != "" && json.Unmarshal([]byte(row.EnvJSON), &env) != nil {
		return errors.New("env_json must be a string-to-string object")
	}
	names := make([]string, 0, len(env))
	for name := range env {
		if len(validation.IsEnvVarName(name)) != 0 {
			return errors.New("env_json contains an invalid environment name")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec.Env = append(spec.Env, oramav1alpha1.LakeEnvironmentVariable{Name: name, Value: env[name]})
	}
	return lakeAppRowSecretEnvironment(row.SecretEnvRefs, spec)
}

func lakeAppRowSecretEnvironment(raw string, spec *oramav1alpha1.LakeAppSpec) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		return lakeAppRowLegacySecretEnvironment(raw, spec)
	}
	if !strings.HasPrefix(raw, "{") {
		return errors.New("secret_env_refs must be an object of environment names to Secret references or a legacy array of Secret names")
	}
	secretEnvironment := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &secretEnvironment); err != nil {
		return errors.New("secret_env_refs must be an object of environment names to Secret references or a legacy array of Secret names")
	}
	names := make([]string, 0, len(secretEnvironment))
	for name := range secretEnvironment {
		if len(validation.IsEnvVarName(name)) != 0 {
			return fmt.Errorf("secret_env_refs environment name %q is invalid", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if reference, ok, err := parseLakeSecretReference(secretEnvironment[name]); ok || err != nil {
			if err != nil {
				return fmt.Errorf("secret_env_refs reference for %q: %w", name, err)
			}
			// The lake secret is the source of truth (lake#625): the LakeApp controller owns a
			// LakeSealedSecret that produces this Secret in the Lake namespace, then copies the field.
			spec.SecretEnv = append(spec.SecretEnv, oramav1alpha1.LakeAppSecretEnvironmentVariable{
				Name: name,
				SecretKeyRef: oramav1alpha1.LakeAppSecretKeyReference{
					Name: lakeAppLakeSecretName(spec.Sync, reference),
					Key:  reference.Field,
				},
				LakeSecret: &reference,
			})
			continue
		}
		secretName, key, err := lakeAppSecretReference(name, secretEnvironment[name])
		if err != nil {
			return err
		}
		spec.SecretEnv = append(spec.SecretEnv, oramav1alpha1.LakeAppSecretEnvironmentVariable{
			Name: name,
			SecretKeyRef: oramav1alpha1.LakeAppSecretKeyReference{
				Name: secretName,
				Key:  key,
			},
		})
	}
	return nil
}

// lakeSecretScheme prefixes lake's reference to one field of one secret in a lake secret mount:
// lake-secret://<mount id>/<secret name>#<field> (lake docs/api/web-apps.md). The name may contain
// "/" and "#"; the field follows the last "#".
const lakeSecretScheme = "lake-secret://"

func parseLakeSecretReference(value string) (oramav1alpha1.LakeAppLakeSecretReference, bool, error) {
	reference := oramav1alpha1.LakeAppLakeSecretReference{}
	if !strings.HasPrefix(value, lakeSecretScheme) {
		return reference, false, nil
	}
	rest := strings.TrimPrefix(value, lakeSecretScheme)
	slash := strings.Index(rest, "/")
	hash := strings.LastIndex(rest, "#")
	if slash <= 0 || hash <= slash+1 || hash == len(rest)-1 {
		return reference, true, errors.New("must be lake-secret://<mount id>/<name>#<field>")
	}
	reference.Mount = rest[:slash]
	reference.Name = rest[slash+1 : hash]
	reference.Field = rest[hash+1:]
	if len(validation.IsConfigMapKey(reference.Field)) != 0 {
		return reference, true, fmt.Errorf("field %q is not a Secret key", reference.Field)
	}
	return reference, true, nil
}

// lakeAppLakeSecretName is the Secret, in the Lake namespace, that the app's LakeSealedSecret for
// one lake secret produces: one per app and secret, whichever fields the app reads from it.
func lakeAppLakeSecretName(sync *oramav1alpha1.LakeAppSync, reference oramav1alpha1.LakeAppLakeSecretReference) string {
	id := ""
	if sync != nil {
		id = sync.ID
	}
	hexID := strings.ReplaceAll(id, "-", "")
	if len(hexID) < 12 {
		hexID = fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
	}
	secret := fmt.Sprintf("%x", sha256.Sum256([]byte(reference.Mount+"\x00"+reference.Name)))[:12]
	return "app-" + hexID[:12] + "-lake-" + secret
}

func lakeAppRowLegacySecretEnvironment(raw string, spec *oramav1alpha1.LakeAppSpec) error {
	if err := json.Unmarshal([]byte(raw), &spec.SecretRefs); err != nil {
		return errors.New("secret_env_refs must be an object of environment names to Secret references or a legacy array of Secret names")
	}
	spec.SecretRefs = sortedUnique(spec.SecretRefs)
	for _, name := range spec.SecretRefs {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("secret_env_refs legacy Secret name %q is invalid", name)
		}
	}
	return nil
}

func lakeAppSecretReference(environmentName, reference string) (string, string, error) {
	parts := strings.Split(reference, "/")
	if len(parts) > 2 || parts[0] == "" {
		return "", "", fmt.Errorf("secret_env_refs reference for %q must be <secret-name> or <secret-name>/<key>", environmentName)
	}
	secretName := parts[0]
	key := environmentName
	if len(parts) == 2 {
		key = parts[1]
	}
	if len(validation.IsDNS1123Subdomain(secretName)) != 0 {
		return "", "", fmt.Errorf("secret_env_refs Secret name for %q is invalid", environmentName)
	}
	if len(validation.IsConfigMapKey(key)) != 0 {
		return "", "", fmt.Errorf("secret_env_refs Secret key for %q is invalid", environmentName)
	}
	return secretName, key, nil
}

func sortedUnique(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	var result []string
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (r *LakeAppsReconciler) requestsForAppDependency(ctx context.Context, object client.Object) []reconcile.Request {
	if owner := object.GetAnnotations()[lakeAppOwnerAnnotation]; owner != "" {
		parts := strings.SplitN(owner, "/", 2)
		if len(parts) == 2 {
			var app oramav1alpha1.LakeApp
			if r.Get(ctx, client.ObjectKey{Namespace: parts[0], Name: parts[1]}, &app) == nil && app.Spec.Sync != nil {
				return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: app.Spec.LakeRef}}}
			}
		}
	}
	var lakes oramav1alpha1.LakeList
	if r.List(ctx, &lakes) != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range lakes.Items {
		lake := &lakes.Items[i]
		if lakeNamespace(lake) == object.GetNamespace() && lakeAppsEnabled(lake) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: lake.Name}})
		}
	}
	return requests
}

func (r *LakeAppsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapDependency := handler.EnqueueRequestsFromMapFunc(r.requestsForAppDependency)
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Lake{}).
		Owns(&oramav1alpha1.LakeApp{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Owns(&oramav1alpha1.PostgresSchema{}).
		Watches(&corev1.Secret{}, mapDependency).
		Watches(&networkingv1.Ingress{}, mapDependency)
	if r.Events != nil {
		builder = builder.WatchesRawSource(source.Channel(r.Events, &handler.EnqueueRequestForObject{}))
	}
	return builder.Named("lake-apps-sync").Complete(r)
}
