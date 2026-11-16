// SPDX-License-Identifier: MIT

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeAppTokensSecretName = "app-tokens"
	lakeAppUIDAnnotation    = "orama.cloud/lake-app-uid"
)

// Tokens are persisted before their hashes are acknowledged by the apps sync. Retrying a
// database write never mints another token, and a missing Secret recovers with new credentials.
func (r *LakeAppReconciler) reconcileLakeAppTokens(ctx context.Context, app *oramav1alpha1.LakeApp, namespace string) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: lakeAppObjectMeta(app, lakeAppTokensName(app), namespace)}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.ResourceVersion != "" && !lakeAppOwnsTokens(app, secret) {
			return errors.New("app token Secret is not owned by this LakeApp")
		}
		secret.Annotations[lakeAppUIDAnnotation] = string(app.UID)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		keys := []string{"source", "events", "pull"}
		if app.Spec.Bindings != nil && app.Spec.Bindings.Zetta != nil {
			keys = append(keys, "zetta")
		} else {
			delete(secret.Data, "zetta")
		}
		for _, key := range keys {
			if len(secret.Data[key]) != 0 {
				continue
			}
			token, err := randomHex(32)
			if err != nil {
				return errors.New("cannot generate app token")
			}
			secret.Data[key] = []byte(token)
		}
		return nil
	})
	return secret, err
}

func lakeAppOwnsTokens(app *oramav1alpha1.LakeApp, secret *corev1.Secret) bool {
	return secret.Annotations[lakeAppOwnerAnnotation] == lakeAppOwner(app) &&
		secret.Annotations[lakeAppUIDAnnotation] == string(app.UID)
}

func lakeAppTokenHashes(secret *corev1.Secret) *oramav1alpha1.LakeAppTokenHashes {
	if len(secret.Data["source"]) == 0 || len(secret.Data["events"]) == 0 || len(secret.Data["pull"]) == 0 {
		return nil
	}
	hashes := &oramav1alpha1.LakeAppTokenHashes{
		Source: credentialDigest(string(secret.Data["source"])),
		Events: credentialDigest(string(secret.Data["events"])),
		Pull:   credentialDigest(string(secret.Data["pull"])),
	}
	if token := secret.Data["zetta"]; len(token) != 0 {
		hash := credentialDigest(string(token))
		hashes.Zetta = &hash
	}
	return hashes
}

func (r *LakeAppReconciler) reconcileSyncedLakeAppSecrets(ctx context.Context, lake *oramav1alpha1.Lake, app *oramav1alpha1.LakeApp, namespace string, wanted map[string]bool, digests map[string][]byte) (lakeAppSecrets, string, string, error) {
	result := lakeAppSecrets{}
	tokens, err := r.reconcileLakeAppTokens(ctx, app, namespace)
	if err != nil {
		return result, "TokenSecretConflict", "cannot reconcile the owned app token Secret", nil
	}
	tokensName := lakeAppTokensName(app)
	wanted[tokensName] = true
	addLakeAppSecretDigest(digests, tokensName, tokens.Data)
	if !apiequality.Semantic.DeepEqual(app.Spec.Sync.TokenHashes, lakeAppTokenHashes(tokens)) {
		return result, "TokenRegistrationPending", "waiting for app token hashes to be registered in Lake", nil
	}
	if app.Spec.Bindings == nil {
		return result, "", "", nil
	}
	data := map[string][]byte{}
	if app.Spec.Bindings.Zetta != nil {
		binding, err := lakeAppZettaBinding(lake, app, tokens.Data["zetta"])
		if err != nil {
			return result, "", "", err
		}
		data["zetta"] = binding
	}
	if len(app.Spec.Bindings.Services) > 0 {
		connections := &corev1.Secret{}
		key := client.ObjectKey{Namespace: app.Namespace, Name: lakeServicesSecret(lake.Name)}
		if err := r.Get(ctx, key, connections); err != nil || !metav1.IsControlledBy(connections, lake) {
			return result, "BindingPending", "waiting for the Lake-owned services Secret", nil
		}
		for _, id := range app.Spec.Bindings.Services {
			if len(connections.Data[id]) == 0 || !validAppServiceName(app.Spec.Bindings.ServiceNames[id]) {
				return result, "BindingPending", "waiting for a requested service connection", nil
			}
			// Only a service of the app's own workspace is handed to it, whatever its row says
			// (lake#945).
			if app.Spec.Sync == nil || app.Spec.Sync.WorkspaceID == "" ||
				connections.Annotations[serviceWorkspaceAnnotation(id)] != app.Spec.Sync.WorkspaceID {
				return result, "ServiceInAnotherWorkspace", "service " + id + " is not in this app's workspace", nil
			}
			data[id] = connections.Data[id]
		}
	}
	if len(data) > 0 {
		bindingsName := lakeAppBindingsName(app)
		if err := r.copyLakeAppSecret(ctx, app, namespace, bindingsName, corev1.SecretTypeOpaque, data); err != nil {
			return result, "", "", err
		}
		wanted[bindingsName] = true
		result.BindingsName = bindingsName
		addLakeAppSecretDigest(digests, bindingsName, data)
	}
	return result, "", "", nil
}

func validAppServiceName(name string) bool {
	return name != "" && len(name) <= 255 && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func (r *LakeAppReconciler) publishSyncedLakeAppEvent(ctx context.Context, app *oramav1alpha1.LakeApp) {
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: app.Spec.LakeRef}, &lake); err != nil || !metav1.IsControlledBy(app, &lake) {
		return
	}
	var tokens corev1.Secret
	key := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppTokensName(app)}
	if err := r.Get(ctx, key, &tokens); err != nil || !lakeAppOwnsTokens(app, &tokens) {
		return
	}
	if app.Spec.Sync.TokenHashes == nil || credentialDigest(string(tokens.Data["events"])) != app.Spec.Sync.TokenHashes.Events {
		return
	}
	baseURL := workspaceLakeInternalURL(&lake, app.Spec.Sync.WorkspaceID)
	token := string(tokens.Data["events"])
	id := app.Spec.Sync.ID
	event := lakeAppEvent{
		Phase: app.Status.Phase, Ready: app.Status.Ready, Revision: app.Status.ObservedRevision,
		Message: app.Status.Message, URL: app.Status.URL,
	}
	if r.EventPublisher != nil {
		_ = r.EventPublisher(ctx, baseURL, token, "app-events."+id, event)
		return
	}
	go func() {
		publishCtx, cancel := context.WithTimeout(context.Background(), lakeAppEventTimeout)
		defer cancel()
		if err := publishSyncedLakeAppEvent(publishCtx, baseURL, token, id, event); err != nil {
			logf.Log.Info("app event publication deferred", "app", id)
		}
	}()
}

func publishSyncedLakeAppEvent(ctx context.Context, baseURL, token, id string, event lakeAppEvent) error {
	body, err := json.Marshal(map[string]any{"app_id": id, "event": event})
	if err != nil {
		return errors.New("cannot encode app event")
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/publish_app_event"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot construct app event request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	// Credentials must never follow a redirect to another endpoint.
	httpClient := &http.Client{Timeout: lakeAppEventTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := httpClient.Do(request)
	if err != nil {
		return errors.New("app event request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("publish_app_event returned HTTP %d", response.StatusCode)
	}
	return nil
}
