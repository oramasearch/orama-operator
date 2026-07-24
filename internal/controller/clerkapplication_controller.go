/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const clerkPlatformBase = "https://api.clerk.com/v1/platform"

// clerkFinalizer releases the CR only after the deletionPolicy has been honored (Retain: orphan
// the Clerk app; Delete: remove it via the Platform API).
const clerkFinalizer = "orama.cloud/clerk-application-cleanup"

// ClerkApplicationReconciler provisions a Clerk application per environment via the Clerk
// Platform API and reflects the instance keys + issuer into an owned Secret (orama-bridge#116).
// It follows the CloudflareDNSRecord "external resource as an owned CRD" pattern: adopt-or-create
// on reconcile, delete-from-Clerk only when deletionPolicy=Delete on finalize.
type ClerkApplicationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=clerkapplications,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=clerkapplications/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=clerkapplications/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *ClerkApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var app oramav1alpha1.ClerkApplication
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	token, err := r.clerkToken(ctx, app.Namespace, app.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Deletion: honor deletionPolicy, then release. Retain (default) orphans the Clerk app —
	// a fat-fingered `kubectl delete` NEVER destroys a user pool; the owned keys Secret is GC'd
	// by owner-ref regardless.
	if !app.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&app, clerkFinalizer) {
			if app.Spec.DeletionPolicy == "Delete" && token != "" && app.Status.ApplicationID != "" {
				if err := clerkDeleteApplication(ctx, token, app.Status.ApplicationID); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&app, clerkFinalizer)
			if err := r.Update(ctx, &app); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Register the finalizer before creating any external state.
	if !controllerutil.ContainsFinalizer(&app, clerkFinalizer) {
		controllerutil.AddFinalizer(&app, clerkFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Safe no-op until the Platform API token Secret is present.
	if token == "" {
		return ctrl.Result{}, nil
	}

	envType := app.Spec.InstanceType
	if envType == "" {
		envType = "development"
		if app.Spec.Environment == "prod" {
			envType = "production"
		}
	}

	// Adopt-or-create (create is NOT idempotent, so match by name first) and pull the instance.
	inst, appID, err := clerkEnsureApplication(ctx, token, app.Spec.ApplicationName, envType, app.Spec.Template)
	if err != nil {
		r.markFailed(ctx, &app, err.Error())
		return ctrl.Result{}, err
	}

	// Write the keys + issuer into an owned Secret. The secret key lives ONLY here (never in
	// status, never in a log). Issuer is derived from the publishable key (the Frontend API host,
	// base64-encoded in the pk after the pk_test_/pk_live_ prefix).
	issuer := issuerFromPublishableKey(inst.PublishableKey)
	keysSecretName := app.Spec.KeysSecretName
	if keysSecretName == "" {
		keysSecretName = app.Name + "-clerk-keys"
	}
	if err := r.writeKeysSecret(ctx, &app, keysSecretName, inst, issuer); err != nil {
		r.markFailed(ctx, &app, err.Error())
		return ctrl.Result{}, err
	}

	// Anti-flap: only write status when something the observer cares about actually changed.
	if app.Status.ApplicationID != appID || app.Status.InstanceID != inst.InstanceID ||
		app.Status.Issuer != issuer || app.Status.PublishableKey != inst.PublishableKey ||
		app.Status.KeysSecretRef != keysSecretName || !app.Status.Ready ||
		app.Status.ObservedGeneration != app.Generation {
		app.Status.ApplicationID = appID
		app.Status.InstanceID = inst.InstanceID
		app.Status.EnvironmentType = inst.EnvironmentType
		app.Status.Issuer = issuer
		app.Status.PublishableKey = inst.PublishableKey
		app.Status.KeysSecretRef = keysSecretName
		app.Status.Ready = true
		app.Status.Message = "reconciled"
		app.Status.ObservedGeneration = app.Generation
		if err := r.Status().Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *ClerkApplicationReconciler) markFailed(ctx context.Context, app *oramav1alpha1.ClerkApplication, msg string) {
	app.Status.Ready, app.Status.Message = false, msg
	_ = r.Status().Update(ctx, app)
}

// clerkToken reads the Platform API token from the referenced Secret (nil-safe: a missing secret
// is a no-op, not an error, so the app waits until it's configured).
func (r *ClerkApplicationReconciler) clerkToken(ctx context.Context, ns, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return string(sec.Data["platformToken"]), nil
}

// writeKeysSecret creates/updates the owned Secret holding the instance's keys + issuer. Owner-ref'd
// so it is garbage-collected when the CR is deleted.
func (r *ClerkApplicationReconciler) writeKeysSecret(ctx context.Context, app *oramav1alpha1.ClerkApplication, name string, inst clerkInstance, issuer string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data["publishableKey"] = []byte(inst.PublishableKey)
		sec.Data["secretKey"] = []byte(inst.SecretKey)
		sec.Data["issuer"] = []byte(issuer)
		return controllerutil.SetControllerReference(app, sec, r.Scheme)
	})
	return err
}

func (r *ClerkApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.ClerkApplication{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// ── Clerk Platform API ────────────────────────────────────────────────────────

type clerkInstance struct {
	InstanceID      string `json:"instance_id"`
	EnvironmentType string `json:"environment_type"`
	SecretKey       string `json:"secret_key"`
	PublishableKey  string `json:"publishable_key"`
}

type clerkApplication struct {
	ApplicationID string          `json:"application_id"`
	Name          string          `json:"name"`
	Instances     []clerkInstance `json:"instances"`
}

// clerkEnsureApplication adopts the app named `name` if it already exists (create is not
// idempotent — re-reconcile must not spawn duplicates), else creates it with the environment type,
// and returns the instance for that environment type. Reuses cfDo (the shared bearer HTTP helper).
func clerkEnsureApplication(ctx context.Context, token, name, envType, template string) (clerkInstance, string, error) {
	var existing []clerkApplication
	if err := cfDo(ctx, token, http.MethodGet, clerkPlatformBase+"/applications?include_secret_keys=true", nil, &existing); err != nil {
		return clerkInstance{}, "", err
	}
	var appObj *clerkApplication
	for i := range existing {
		if existing[i].Name == name {
			appObj = &existing[i]
			break
		}
	}
	if appObj == nil {
		body := map[string]interface{}{"name": name, "environment_types": []string{envType}}
		if template != "" {
			body["template"] = template
		}
		var created clerkApplication
		if err := cfDo(ctx, token, http.MethodPost, clerkPlatformBase+"/applications", body, &created); err != nil {
			return clerkInstance{}, "", err
		}
		appObj = &created
	}
	for _, inst := range appObj.Instances {
		if inst.EnvironmentType == envType {
			return inst, appObj.ApplicationID, nil
		}
	}
	// The app exists but not with the requested environment type (e.g. adopted a dev-only app for a
	// prod CR). Surface it rather than silently returning the wrong instance.
	return clerkInstance{}, appObj.ApplicationID, fmt.Errorf("clerk app %q has no %s instance", name, envType)
}

func clerkDeleteApplication(ctx context.Context, token, appID string) error {
	return cfDo(ctx, token, http.MethodDelete, clerkPlatformBase+"/applications/"+appID, nil, nil)
}

// issuerFromPublishableKey derives the Frontend API URL (-> CLERK_ISSUER) from a publishable key.
// A pk is `pk_(test|live)_<base64(frontend-api-host + "$")>`; SDKs decode the host and prefix https.
func issuerFromPublishableKey(pk string) string {
	body := pk
	for _, prefix := range []string{"pk_test_", "pk_live_"} {
		if strings.HasPrefix(pk, prefix) {
			body = pk[len(prefix):]
			break
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		// Some keys use raw-std without padding; try with padding added.
		if p := body + strings.Repeat("=", (4-len(body)%4)%4); p != body {
			decoded, err = base64.StdEncoding.DecodeString(p)
		}
	}
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.TrimSpace(string(decoded)), "$")
	if host == "" {
		return ""
	}
	return "https://" + host
}
