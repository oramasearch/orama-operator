/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"
	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	aksIdentityProvider = "orama-aks-workload-identity"
	aksTokenScope       = "6dae42f8-4368-4678-94ff-3960e28e3630/.default"
	aksClusterAdminRole = "b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b"
)

var aksIdentityAzure = newAzureClient()

func init() {
	if err := rest.RegisterAuthProviderPlugin(aksIdentityProvider, func(address string, config map[string]string, _ rest.AuthProviderConfigPersister) (rest.AuthProvider, error) {
		target, err := url.Parse(address)
		if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
			return nil, fmt.Errorf("AKS workload identity requires an HTTPS API server")
		}
		creds := azureCredentials{TenantID: config["tenant-id"], ClientID: config["client-id"]}
		if creds.TenantID == "" || creds.ClientID == "" || len(config) != 2 {
			return nil, fmt.Errorf("AKS workload identity requires only tenant-id and client-id")
		}
		return &aksWorkloadIdentityProvider{azure: aksIdentityAzure, creds: creds, host: target.Host}, nil
	}); err != nil {
		panic(err)
	}
}

// The transport obtains short-lived AKS tokens in memory using the pod's projected identity.
// No command from a kubeconfig is executed and no token is persisted to Kubernetes or disk.
type aksWorkloadIdentityProvider struct {
	azure *azureClient
	creds azureCredentials
	host  string
}

func (p *aksWorkloadIdentityProvider) Login() error { return nil }
func (p *aksWorkloadIdentityProvider) WrapTransport(next http.RoundTripper) http.RoundTripper {
	return &aksIdentityTransport{provider: p, next: next}
}

type aksIdentityTransport struct {
	provider *aksWorkloadIdentityProvider
	next     http.RoundTripper
}

func (t *aksIdentityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != t.provider.host {
		return nil, fmt.Errorf("AKS workload identity refuses a different API server")
	}
	token, err := t.provider.azure.tokenForScope(req.Context(), t.provider.creds, aksTokenScope)
	if err != nil {
		return nil, fmt.Errorf("AKS workload identity token unavailable: %w", err)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	return t.next.RoundTrip(clone)
}

// aksIdentityKubeconfig retains only the selected server and CA from Azure's user response.
// Even an unexpected certificate, token, exec command or file reference is never delivered.
func aksIdentityKubeconfig(data []byte, creds azureCredentials, endpoint string) ([]byte, error) {
	raw, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("Azure returned an invalid user kubeconfig")
	}
	ctx := raw.Contexts[raw.CurrentContext]
	if ctx == nil || raw.Clusters[ctx.Cluster] == nil {
		return nil, fmt.Errorf("Azure user kubeconfig has no selected cluster")
	}
	cluster := raw.Clusters[ctx.Cluster]
	if cluster.Server != endpoint || !strings.HasPrefix(endpoint, "https://") || len(cluster.CertificateAuthorityData) == 0 || cluster.InsecureSkipTLSVerify || cluster.ProxyURL != "" {
		return nil, fmt.Errorf("Azure user kubeconfig must match the observed HTTPS endpoint with an embedded CA")
	}
	clean := clientcmdapi.NewConfig()
	clean.CurrentContext = "operator"
	clean.Clusters["cluster"] = &clientcmdapi.Cluster{Server: endpoint, CertificateAuthorityData: cluster.CertificateAuthorityData}
	clean.Contexts["operator"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "identity"}
	clean.AuthInfos["identity"] = &clientcmdapi.AuthInfo{AuthProvider: &clientcmdapi.AuthProviderConfig{
		Name: aksIdentityProvider, Config: map[string]string{"tenant-id": creds.TenantID, "client-id": creds.ClientID},
	}}
	return clientcmd.Write(*clean)
}

// Equality to the rebuilt allowlist rejects all alternate authentication and local-file fields,
// including inactive contexts, exec hooks, bearer tokens and client certificates.
func validateAksIdentityKubeconfig(data []byte, creds azureCredentials, endpoint string) error {
	clean, err := aksIdentityKubeconfig(data, creds, endpoint)
	if err != nil {
		return err
	}
	raw, err := clientcmd.Load(data)
	if err != nil {
		return err
	}
	expected, err := clientcmd.Load(clean)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(raw, expected) {
		return fmt.Errorf("AKS kubeconfig must contain only the operator workload identity and API CA")
	}
	return nil
}

func aksAccessDrift(cluster *oramav1alpha1.AksCluster, found *azureManagedCluster, tenant string) []string {
	var drift []string
	p := found.Properties
	if p.EnableRBAC == nil || !*p.EnableRBAC {
		drift = append(drift, "Kubernetes RBAC is disabled")
	}
	if p.DisableLocalAccounts == nil || !*p.DisableLocalAccounts {
		drift = append(drift, "local accounts are enabled")
	}
	if p.AADProfile == nil || !p.AADProfile.Managed || !p.AADProfile.EnableAzureRBAC || !strings.EqualFold(p.AADProfile.TenantID, tenant) {
		drift = append(drift, "managed Entra authentication and Azure RBAC for the provider tenant are required")
	}
	if err := validateAksAccess(cluster.Spec.Access); err != nil {
		drift = append(drift, err.Error())
	}
	want := append([]string(nil), cluster.Spec.Access.AuthorizedIPRanges...)
	var actual []string
	if p.APIServerAccessProfile != nil {
		actual = append(actual, p.APIServerAccessProfile.AuthorizedIPRanges...)
	}
	sort.Strings(want)
	sort.Strings(actual)
	if len(actual) == 0 || !reflect.DeepEqual(want, actual) {
		drift = append(drift, "API authorized ranges differ from the managing egress ranges")
	}
	return drift
}

// A deterministic assignment binds this identity to this cluster only. Subscription-wide role
// assignment permission is an explicit account prerequisite; no Kubernetes admin credential is used.
func (c *azureClient) ensureAksRole(ctx context.Context, creds azureCredentials, scope, principal string) error {
	role := "/subscriptions/" + strings.Split(strings.TrimPrefix(scope, "/subscriptions/"), "/")[0] + "/providers/Microsoft.Authorization/roleDefinitions/" + aksClusterAdminRole
	name := uuid.NewSHA1(uuid.NameSpaceURL, []byte(strings.ToLower(scope+"/"+principal+"/"+aksClusterAdminRole))).String()
	path := scope + "/providers/Microsoft.Authorization/roleAssignments/" + name
	type properties struct {
		PrincipalID      string `json:"principalId"`
		RoleDefinitionID string `json:"roleDefinitionId"`
		PrincipalType    string `json:"principalType"`
	}
	var existing struct {
		Properties properties `json:"properties"`
	}
	_, err := c.do(ctx, creds, http.MethodGet, path, "2022-04-01", nil, &existing)
	if err == nil {
		if !strings.EqualFold(existing.Properties.PrincipalID, principal) || !strings.EqualFold(existing.Properties.RoleDefinitionID, role) {
			return fmt.Errorf("AKS role assignment has a different principal or role")
		}
		return nil
	}
	if !azureNotFound(err) {
		return err
	}
	body := struct {
		Properties properties `json:"properties"`
	}{Properties: properties{PrincipalID: principal, RoleDefinitionID: role, PrincipalType: "ServicePrincipal"}}
	_, err = c.do(ctx, creds, http.MethodPut, path, "2022-04-01", body, nil)
	return err
}

// Security migration retains Azure's complete current representation, including fields this
// operator does not model; it never submits desired pool/version changes as part of access repair.
func (c *azureClient) enforceAksAccess(ctx context.Context, creds azureCredentials, path string, found *azureManagedCluster, access oramav1alpha1.AksClusterAccess) error {
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(found.raw["properties"], &properties); err != nil {
		return fmt.Errorf("AKS security update has no observed properties: %w", err)
	}
	for key, value := range map[string]any{
		"disableLocalAccounts":   true,
		"enableRBAC":             true,
		"aadProfile":             &azureAADProfile{Managed: true, EnableAzureRBAC: true, TenantID: creds.TenantID},
		"apiServerAccessProfile": &azureAPIServerAccess{AuthorizedIPRanges: access.AuthorizedIPRanges},
	} {
		// Keep other API access profile settings, including private network configuration.
		if key == "apiServerAccessProfile" {
			var profile map[string]json.RawMessage
			_ = json.Unmarshal(properties[key], &profile)
			if profile == nil {
				profile = map[string]json.RawMessage{}
			}
			ranges, _ := json.Marshal(access.AuthorizedIPRanges)
			profile["authorizedIPRanges"] = ranges
			value = profile
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		properties[key] = encoded
	}
	encoded, err := json.Marshal(properties)
	if err != nil {
		return err
	}
	body := make(map[string]json.RawMessage, len(found.raw))
	for key, value := range found.raw {
		body[key] = value
	}
	body["properties"] = encoded
	_, err = c.do(ctx, creds, http.MethodPut, path, azureAPIManagedClusters, body, nil)
	return err
}

// Verify a request that anonymous access cannot satisfy before replacing any legacy credential.
func (r *AksClusterReconciler) verifyIdentityAccess(ctx context.Context, data []byte) error {
	config, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return err
	}
	if config.AuthProvider == nil || config.AuthProvider.Name != aksIdentityProvider {
		return fmt.Errorf("AKS identity probe requires workload identity")
	}
	creds := azureCredentials{TenantID: config.AuthProvider.Config["tenant-id"], ClientID: config.AuthProvider.Config["client-id"]}
	target, err := url.Parse(config.Host)
	if err != nil {
		return err
	}
	config.AuthProvider = nil
	config.Timeout = leafAPITimeout
	provider := &aksWorkloadIdentityProvider{azure: r.azure(), creds: creds, host: target.Host}
	config.Wrap(provider.WrapTransport)
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(config.Host, "/")+"/api/v1/namespaces?limit=1", nil)
	if err != nil {
		return err
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("AKS workload identity access is not confirmed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("AKS workload identity access is not confirmed (HTTP %d)", response.StatusCode)
	}
	return nil
}

// The provider owns credential policy. Shared leaf consumers use this neutral result without
// naming Azure, while the AKS helper checks both the concrete generation and migration gate.
type managedClusterAccess struct {
	legacy   bool
	waiting  string
	validate func([]byte) error
}

func managedClusterAccessFor(ctx context.Context, c client.Client, cluster *oramav1alpha1.KubernetesCluster) (managedClusterAccess, error) {
	access := managedClusterAccess{}
	if cluster.Status.ProviderType != oramav1alpha1.KubernetesClusterProviderAKS {
		return access, nil
	}
	var concrete oramav1alpha1.AksCluster
	if err := c.Get(ctx, types.NamespacedName{Name: cluster.Name}, &concrete); err != nil {
		if apierrors.IsNotFound(err) {
			access.waiting = "the cluster provider has not reported access yet"
			return access, nil
		}
		return access, err
	}
	condition := apimeta.FindStatusCondition(concrete.Status.Conditions, clusterConditionReady)
	access.legacy = !cluster.Spec.Access.Enforce && !concrete.Spec.Access.Enforce && !concrete.Status.AccessSecured && condition != nil && condition.Reason == "InsecureAccess"
	if access.legacy {
		return access, nil
	}
	if concrete.Status.Phase != clusterPhaseReady || concrete.Status.ObservedGeneration != concrete.Generation {
		access.waiting = "the cluster provider has not confirmed secure access"
		return access, nil
	}
	access.validate = func(kubeconfig []byte) error {
		creds, err := readAzureCredentials(ctx, c, concrete.Spec.CredentialsSecretRef)
		if err != nil {
			return err
		}
		if creds.ClientSecret != "" {
			return fmt.Errorf("AKS administration requires workload identity")
		}
		return validateAksIdentityKubeconfig(kubeconfig, creds, "https://"+concrete.Status.FQDN+":443")
	}
	return access, nil
}
