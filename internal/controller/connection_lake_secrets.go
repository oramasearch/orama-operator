/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A connection's credential is a user-level secret (the two levels of secrets): it lives in Lake's
// secret store, sealed in the workspace Zetta, and reaches a Kubernetes Secret only as the
// projection of an app that mounts it (lake#625: an app's secret environment names
// lake-secret://<mount>/<name>#<field>, and a LakeSealedSecret delivers each new version). The
// operator writes it through Lake's own capabilities with its operator credential, the Lake's admin
// bearer, exactly as it asks Lake for sealed deliveries; it never talks to a workspace Zetta.

const (
	// connectionCredentialMount is the secret mount, one per workspace, that holds the credentials
	// the operator issues for that workspace's connections.
	connectionCredentialMount = "network-credentials"
	lakeAPITimeout            = 15 * time.Second
)

// WorkspaceSecretWriter is the injected dependency through which the Connection controller writes a
// credential into the workspace's secret store. The HTTP implementation calls Lake; tests
// substitute a fake.
type WorkspaceSecretWriter interface {
	// PutWorkspaceSecret writes a new version of one secret in the workspace's credential mount and
	// returns its reference, lake-secret://<mount id>/<name>.
	PutWorkspaceSecret(
		ctx context.Context,
		lake *oramav1alpha1.Lake,
		customerID, workspaceID, name string,
		value map[string]any,
		metadata map[string]string,
	) (string, error)
}

func connectionCredentialSecretName(connectionID string) string {
	return "connection-" + connectionID
}

func lakeSecretReference(mountID, name string) string {
	return lakeSecretScheme + mountID + "/" + name
}

// lakeHTTPSecretWriter writes workspace secrets through Lake's REST capabilities.
type lakeHTTPSecretWriter struct {
	Reader     client.Reader
	HTTPClient *http.Client
	// BaseURL overrides the Lake Service address (tests).
	BaseURL string
}

type lakeCapabilityResponse struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error *lakeAPIError   `json:"error,omitempty"`
}

func (w *lakeHTTPSecretWriter) PutWorkspaceSecret(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	customerID, workspaceID, name string,
	value map[string]any,
	metadata map[string]string,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lakeAPITimeout)
	defer cancel()
	token, err := w.adminToken(ctx, lake)
	if err != nil {
		return "", err
	}
	mountID, err := w.credentialMount(ctx, lake, token, customerID, workspaceID)
	if err != nil {
		return "", err
	}
	var written struct {
		MountID string `json:"mount_id"`
		Name    string `json:"name"`
		Version int64  `json:"version"`
	}
	if err := w.call(ctx, lake, token, "put_secret", map[string]any{
		"mount_id":        mountID,
		"name":            name,
		"value":           value,
		"custom_metadata": metadata,
	}, &written); err != nil {
		return "", err
	}
	if written.Version < 1 {
		return "", errors.New("lake put_secret returned no version")
	}
	return lakeSecretReference(mountID, name), nil
}

// credentialMount finds the workspace's credential mount, creating it on first use.
func (w *lakeHTTPSecretWriter) credentialMount(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	token, customerID, workspaceID string,
) (string, error) {
	var listed struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	query := url.Values{"customer_id": {customerID}, "workspace_id": {workspaceID}, "limit": {"500"}}
	if err := w.read(ctx, lake, token, "list_secret_mounts", query, &listed); err != nil {
		return "", err
	}
	for _, mount := range listed.Items {
		if mount.Name == connectionCredentialMount && mount.ID != "" {
			return mount.ID, nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := w.call(ctx, lake, token, "create_secret_mount", map[string]any{
		"customer_id":  customerID,
		"workspace_id": workspaceID,
		"name":         connectionCredentialMount,
		"description":  "Credentials the Orama operator issues for this workspace's connections.",
	}, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("lake create_secret_mount returned no mount id")
	}
	return created.ID, nil
}

// read calls a Lake read capability. Lake answers a read only to GET with a query string; a POST
// gets method_not_allowed.
func (w *lakeHTTPSecretWriter) read(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	token, capability string,
	query url.Values,
	output any,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL(lake)+"/v1/"+capability+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	return w.do(request, token, capability, output)
}

// call calls a Lake action capability: POST with a JSON body.
func (w *lakeHTTPSecretWriter) call(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	token, capability string,
	input any,
	output any,
) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL(lake)+"/v1/"+capability, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return w.do(request, token, capability, output)
}

func (w *lakeHTTPSecretWriter) do(request *http.Request, token, capability string, output any) error {
	request.Header.Set("Authorization", "Bearer "+token)
	httpClient := w.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: lakeAPITimeout}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call lake %s: %w", capability, err)
	}
	defer response.Body.Close()
	var decoded lakeCapabilityResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&decoded); err != nil {
		return fmt.Errorf("lake %s returned %s", capability, response.Status)
	}
	if !decoded.OK || response.StatusCode < 200 || response.StatusCode >= 300 {
		if decoded.Error != nil {
			return fmt.Errorf("lake %s: %s: %s", capability, decoded.Error.Code, decoded.Error.Message)
		}
		return fmt.Errorf("lake %s returned %s", capability, response.Status)
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(decoded.Value, output); err != nil {
		return fmt.Errorf("decode lake %s: %w", capability, err)
	}
	return nil
}

func (w *lakeHTTPSecretWriter) adminToken(ctx context.Context, lake *oramav1alpha1.Lake) (string, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: lakeWorkloadNamespace(lake), Name: lakeAdminSecret(lake.Name)}
	if err := w.Reader.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("read the Lake's admin Secret %s: %w", key, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", fmt.Errorf("the Lake's admin Secret %s has no token", key)
	}
	return token, nil
}

func (w *lakeHTTPSecretWriter) baseURL(lake *oramav1alpha1.Lake) string {
	if w.BaseURL != "" {
		return strings.TrimRight(w.BaseURL, "/")
	}
	return fmt.Sprintf("http://lake-%s.%s.svc.cluster.local", lake.Name, lakeWorkloadNamespace(lake))
}
