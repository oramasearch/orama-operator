/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The Azure Resource Manager calls the AksCluster controller makes (operator#354). Like the
// Cloudflare and Clerk controllers, this talks to the provider's REST API directly; there is no
// Azure SDK dependency. The AKS controller and azure_*.go helpers own the provider API.

const (
	azureDefaultARMBase   = "https://management.azure.com"
	azureDefaultLoginBase = "https://login.microsoftonline.com"
	azureARMScope         = "https://management.azure.com/.default"

	azureAPIResourceGroups  = "2021-04-01"
	azureAPIManagedClusters = "2025-05-01"
	// The latest-operation resource. From this version its status carries Azure's progress text
	// ("CreatingAgentPools: 0/1 nodes completed"); 2024-09-01 answers only "InProgress".
	azureAPIOperations    = "2025-05-01"
	azureAPISubscriptions = "2022-12-01"

	// azureFederatedTokenFileEnv is set by the AKS workload identity webhook to the path of the
	// projected service-account token.
	azureFederatedTokenFileEnv = "AZURE_FEDERATED_TOKEN_FILE"

	azureAuthWorkloadIdentity = "workload-identity"
	azureAuthServicePrincipal = "service-principal"
)

// azureCredentials is what the credentials Secret holds.
type azureCredentials struct {
	TenantID     string
	ClientID     string
	ClientSecret string // empty selects workload identity
}

func (c azureCredentials) authMethod() string {
	if c.ClientSecret != "" {
		return azureAuthServicePrincipal
	}
	return azureAuthWorkloadIdentity
}

// azureCredentialsFromSecret reads tenantID, clientID and the optional clientSecret.
func azureCredentialsFromSecret(sec *corev1.Secret) (azureCredentials, error) {
	creds := azureCredentials{
		TenantID:     strings.TrimSpace(string(sec.Data["tenantID"])),
		ClientID:     strings.TrimSpace(string(sec.Data["clientID"])),
		ClientSecret: strings.TrimSpace(string(sec.Data["clientSecret"])),
	}
	if creds.TenantID == "" || creds.ClientID == "" {
		return creds, fmt.Errorf("credentials Secret %s/%s needs the keys tenantID and clientID", sec.Namespace, sec.Name)
	}
	return creds, nil
}

// azureError is a refused Azure call. Code and Message come from Azure's error envelope.
type azureError struct {
	Status  int
	Code    string
	Message string
}

func (e *azureError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("azure: HTTP %d", e.Status)
	}
	return fmt.Sprintf("azure: %s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

func azureNotFound(err error) bool {
	var azErr *azureError
	return errors.As(err, &azErr) && azErr.Status == http.StatusNotFound
}

// azureClient calls Azure Resource Manager with a bearer token it obtains and caches. The base
// URLs are fields so a test points them at a fake.
type azureClient struct {
	ARMBase   string
	LoginBase string
	HTTP      *http.Client
	// TokenFile overrides AZURE_FEDERATED_TOKEN_FILE for workload identity.
	TokenFile string

	mu     sync.Mutex
	tokens map[string]azureToken
}

type azureToken struct {
	value   string
	expires time.Time
}

func newAzureClient() *azureClient {
	return &azureClient{
		ARMBase:   azureDefaultARMBase,
		LoginBase: azureDefaultLoginBase,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

// token returns a management-plane token for the credentials, reusing one that has more than two
// minutes left.
func (c *azureClient) token(ctx context.Context, creds azureCredentials) (string, error) {
	key := creds.TenantID + "/" + creds.ClientID + "/" + creds.authMethod()
	c.mu.Lock()
	if cached, ok := c.tokens[key]; ok && time.Until(cached.expires) > 2*time.Minute {
		c.mu.Unlock()
		return cached.value, nil
	}
	c.mu.Unlock()

	form := url.Values{
		"grant_type": {"client_credentials"},
		"client_id":  {creds.ClientID},
		"scope":      {azureARMScope},
	}
	if creds.ClientSecret != "" {
		form.Set("client_secret", creds.ClientSecret)
	} else {
		path := c.TokenFile
		if path == "" {
			path = os.Getenv(azureFederatedTokenFileEnv)
		}
		if path == "" {
			return "", fmt.Errorf("azure: the credentials Secret has no clientSecret and %s is not set, so workload identity is not available to this pod", azureFederatedTokenFileEnv)
		}
		assertion, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("azure: read the federated token: %w", err)
		}
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", strings.TrimSpace(string(assertion)))
	}

	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", strings.TrimRight(c.LoginBase, "/"), url.PathEscape(creds.TenantID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("azure: token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &failure)
		// The description names the tenant and client, never a secret.
		return "", &azureError{Status: resp.StatusCode, Code: failure.Error, Message: azureFirstLine(failure.ErrorDescription)}
	}
	var granted struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &granted); err != nil || granted.AccessToken == "" {
		return "", fmt.Errorf("azure: token response had no access_token")
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[string]azureToken{}
	}
	c.tokens[key] = azureToken{value: granted.AccessToken, expires: time.Now().Add(time.Duration(granted.ExpiresIn) * time.Second)}
	c.mu.Unlock()
	return granted.AccessToken, nil
}

func azureFirstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// do sends one ARM request. A 2xx with a body is decoded into out when out is not nil.
func (c *azureClient) do(ctx context.Context, creds azureCredentials, method, path, apiVersion string, body, out any) (int, error) {
	token, err := c.token(ctx, creds)
	if err != nil {
		return 0, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	target := strings.TrimRight(c.ARMBase, "/") + path + "?api-version=" + url.QueryEscape(apiVersion)
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("azure: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		return resp.StatusCode, &azureError{Status: resp.StatusCode, Code: failure.Error.Code, Message: azureFirstLine(failure.Error.Message)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("azure: decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// ── paths ─────────────────────────────────────────────────────────────────────

func azureSubscriptionPath(subscription string) string {
	return "/subscriptions/" + url.PathEscape(subscription)
}

func azureResourceGroupPath(subscription, group string) string {
	return azureSubscriptionPath(subscription) + "/resourcegroups/" + url.PathEscape(group)
}

func azureManagedClusterPath(subscription, group, name string) string {
	return azureResourceGroupPath(subscription, group) +
		"/providers/Microsoft.ContainerService/managedClusters/" + url.PathEscape(name)
}

// ── wire types ────────────────────────────────────────────────────────────────

type azureResourceGroup struct {
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags,omitempty"`
}

type azureAgentPool struct {
	Name              string              `json:"name"`
	Mode              string              `json:"mode,omitempty"`
	VMSize            string              `json:"vmSize,omitempty"`
	Count             int32               `json:"count"`
	EnableAutoScaling bool                `json:"enableAutoScaling,omitempty"`
	MinCount          *int32              `json:"minCount,omitempty"`
	MaxCount          *int32              `json:"maxCount,omitempty"`
	OSType            string              `json:"osType,omitempty"`
	OSSKU             string              `json:"osSKU,omitempty"`
	OSDiskSizeGB      int32               `json:"osDiskSizeGB,omitempty"`
	Type              string              `json:"type,omitempty"`
	AvailabilityZones []string            `json:"availabilityZones,omitempty"`
	NodeLabels        map[string]string   `json:"nodeLabels,omitempty"`
	NodeTaints        []string            `json:"nodeTaints,omitempty"`
	LinuxOSConfig     *azureLinuxOSConfig `json:"linuxOSConfig,omitempty"`
	ProvisioningState string              `json:"provisioningState,omitempty"`
	// Read only: Azure's account of what is wrong with the pool.
	Status *azureResourceStatus `json:"status,omitempty"`
}

// azureErrorDetail is the error object Azure nests in an operation and in a resource's status.
type azureErrorDetail struct {
	Code    string             `json:"code,omitempty"`
	Message string             `json:"message,omitempty"`
	Target  string             `json:"target,omitempty"`
	Details []azureErrorDetail `json:"details,omitempty"`
}

// text renders the error and what Azure nests under it as "code: message; code: message". The
// nested entries usually hold the cause (the VM extension's exit code, the quota that ran out).
func (e *azureErrorDetail) text() string {
	if e == nil {
		return ""
	}
	var parts []string
	var walk func(d azureErrorDetail, depth int)
	walk = func(d azureErrorDetail, depth int) {
		line := strings.TrimSpace(strings.Join(strings.Fields(d.Message), " "))
		switch {
		case d.Code != "" && line != "":
			line = d.Code + ": " + line
		case d.Code != "":
			line = d.Code
		}
		if line != "" {
			parts = append(parts, line)
		}
		if depth < azureErrorDetailDepth {
			for _, nested := range d.Details {
				walk(nested, depth+1)
			}
		}
	}
	walk(*e, 0)
	text := strings.Join(parts, "; ")
	if len(text) > azureErrorTextLimit {
		text = text[:azureErrorTextLimit] + "... (truncated; the operation ID finds the rest in Azure)"
	}
	return text
}

const (
	azureErrorDetailDepth = 4
	// A status field, so bounded: Azure's extension errors can run to many kilobytes.
	azureErrorTextLimit = 2000
)

type azureResourceStatus struct {
	ProvisioningError *azureErrorDetail `json:"provisioningError,omitempty"`
}

// azureOperation is a managed cluster's long-running operation.
type azureOperation struct {
	ID        string            `json:"id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Status    string            `json:"status,omitempty"`
	StartTime string            `json:"startTime,omitempty"`
	EndTime   string            `json:"endTime,omitempty"`
	Error     *azureErrorDetail `json:"error,omitempty"`
}

type azureManagedCluster struct {
	ID       string            `json:"id,omitempty"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags,omitempty"`
	SKU      *struct {
		Name string `json:"name"`
		Tier string `json:"tier"`
	} `json:"sku,omitempty"`
	Identity *struct {
		Type string `json:"type"`
	} `json:"identity,omitempty"`
	Properties azureManagedClusterProperties `json:"properties"`
}

type azureManagedClusterProperties struct {
	ProvisioningState        string            `json:"provisioningState,omitempty"`
	PowerState               *azurePowerState  `json:"powerState,omitempty"`
	KubernetesVersion        string            `json:"kubernetesVersion,omitempty"`
	CurrentKubernetesVersion string            `json:"currentKubernetesVersion,omitempty"`
	DNSPrefix                string            `json:"dnsPrefix,omitempty"`
	FQDN                     string            `json:"fqdn,omitempty"`
	EnableRBAC               *bool             `json:"enableRBAC,omitempty"`
	NodeResourceGroup        string            `json:"nodeResourceGroup,omitempty"`
	AgentPoolProfiles        []azureAgentPool  `json:"agentPoolProfiles,omitempty"`
	NetworkProfile           *azureNetwork     `json:"networkProfile,omitempty"`
	OIDCIssuerProfile        *azureOIDCIssuer  `json:"oidcIssuerProfile,omitempty"`
	SecurityProfile          *azureSecurity    `json:"securityProfile,omitempty"`
	AutoUpgradeProfile       *azureAutoUpgrade `json:"autoUpgradeProfile,omitempty"`
	// Read only: Azure's account of what is wrong with the cluster.
	Status *azureResourceStatus `json:"status,omitempty"`
}

type azurePowerState struct {
	Code string `json:"code,omitempty"`
}

type azureNetwork struct {
	NetworkPlugin     string `json:"networkPlugin,omitempty"`
	NetworkPluginMode string `json:"networkPluginMode,omitempty"`
	NetworkDataplane  string `json:"networkDataplane,omitempty"`
	NetworkPolicy     string `json:"networkPolicy,omitempty"`
	PodCIDR           string `json:"podCidr,omitempty"`
	ServiceCIDR       string `json:"serviceCidr,omitempty"`
	DNSServiceIP      string `json:"dnsServiceIP,omitempty"`
	LoadBalancerSKU   string `json:"loadBalancerSku,omitempty"`
}

type azureOIDCIssuer struct {
	Enabled   bool   `json:"enabled"`
	IssuerURL string `json:"issuerURL,omitempty"`
}

type azureSecurity struct {
	WorkloadIdentity *struct {
		Enabled bool `json:"enabled"`
	} `json:"workloadIdentity,omitempty"`
}

type azureAutoUpgrade struct {
	UpgradeChannel       string `json:"upgradeChannel,omitempty"`
	NodeOSUpgradeChannel string `json:"nodeOSUpgradeChannel,omitempty"`
}

// azureManagedClusterBody is the create request for an AksCluster. It is a pure function of the
// spec, so the same resource always asks Azure for the same cluster.
func azureManagedClusterBody(cluster *oramav1alpha1.AksCluster) (azureManagedCluster, error) {
	spec := cluster.Spec
	name := aksClusterAzureName(cluster)
	rbac := true
	body := azureManagedCluster{
		Location: spec.Location,
		Tags:     spec.Tags,
		Properties: azureManagedClusterProperties{
			KubernetesVersion: spec.KubernetesVersion,
			DNSPrefix:         name + "-dns",
			EnableRBAC:        &rbac,
			NetworkProfile: &azureNetwork{
				NetworkPlugin:     spec.Network.Plugin,
				NetworkPluginMode: spec.Network.PluginMode,
				NetworkDataplane:  spec.Network.Dataplane,
				NetworkPolicy:     spec.Network.Policy,
				PodCIDR:           spec.Network.PodCIDR,
				ServiceCIDR:       spec.Network.ServiceCIDR,
				DNSServiceIP:      spec.Network.DNSServiceIP,
				LoadBalancerSKU:   "standard",
			},
		},
	}
	tier := spec.SKUTier
	if tier == "" {
		tier = "Standard"
	}
	body.SKU = &struct {
		Name string `json:"name"`
		Tier string `json:"tier"`
	}{Name: "Base", Tier: tier}
	body.Identity = &struct {
		Type string `json:"type"`
	}{Type: "SystemAssigned"}
	if spec.WorkloadIdentity {
		body.Properties.OIDCIssuerProfile = &azureOIDCIssuer{Enabled: true}
		body.Properties.SecurityProfile = &azureSecurity{WorkloadIdentity: &struct {
			Enabled bool `json:"enabled"`
		}{Enabled: true}}
	}
	if spec.UpgradeChannel != "" || spec.NodeOSUpgradeChannel != "" {
		body.Properties.AutoUpgradeProfile = &azureAutoUpgrade{
			UpgradeChannel:       spec.UpgradeChannel,
			NodeOSUpgradeChannel: spec.NodeOSUpgradeChannel,
		}
	}
	for _, pool := range spec.NodePools {
		linuxOS, err := aksLinuxOSConfig(pool.Sysctls)
		if err != nil {
			return azureManagedCluster{}, fmt.Errorf("node pool %s: %w", pool.Name, err)
		}
		profile := azureAgentPool{
			LinuxOSConfig:     linuxOS,
			Name:              pool.Name,
			Mode:              pool.Mode,
			VMSize:            pool.VMSize,
			Count:             pool.Count,
			OSType:            "Linux",
			OSSKU:             "Ubuntu",
			OSDiskSizeGB:      pool.OSDiskSizeGB,
			Type:              "VirtualMachineScaleSets",
			AvailabilityZones: pool.AvailabilityZones,
			NodeLabels:        pool.NodeLabels,
		}
		if pool.EnableAutoScaling {
			profile.EnableAutoScaling = true
			profile.MinCount = pool.MinCount
			profile.MaxCount = pool.MaxCount
		}
		for _, taint := range pool.NodeTaints {
			profile.NodeTaints = append(profile.NodeTaints, fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect))
		}
		body.Properties.AgentPoolProfiles = append(body.Properties.AgentPoolProfiles, profile)
	}
	return body, nil
}

// aksClusterAzureName is the managed cluster's name in Azure.
func aksClusterAzureName(cluster *oramav1alpha1.AksCluster) string {
	if cluster.Spec.ClusterName != "" {
		return cluster.Spec.ClusterName
	}
	return cluster.Name
}

// azureTagProblem says why Azure would refuse a set of tags, or "" when it would not. Azure
// rejects the whole create call for one bad tag name (InvalidTagNameCharacters), so this is checked
// before anything is sent. A Kubernetes-style name such as orama.cloud/role is not a valid Azure
// tag name: the slash is reserved.
func azureTagProblem(tags map[string]string) string {
	if len(tags) > 50 {
		return fmt.Sprintf("Azure allows 50 tags on a resource, and %d were given", len(tags))
	}
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch {
		case name == "":
			return "a tag has an empty name"
		case len(name) > 512:
			return fmt.Sprintf("tag name %q is longer than the 512 characters Azure allows", name[:40]+"...")
		case strings.ContainsAny(name, `<>%&\?/`):
			return fmt.Sprintf("tag name %q contains a character Azure reserves (one of < > %% & \\ ? /)", name)
		case len(tags[name]) > 256:
			return fmt.Sprintf("the value of tag %q is longer than the 256 characters Azure allows", name)
		}
	}
	return ""
}

// ── operations ────────────────────────────────────────────────────────────────

// subscriptionReachable is the one cheap call a provider makes to prove its credentials work.
func (c *azureClient) subscriptionReachable(ctx context.Context, creds azureCredentials, subscription string) error {
	_, err := c.do(ctx, creds, http.MethodGet, azureSubscriptionPath(subscription), azureAPISubscriptions, nil, nil)
	return err
}

// ensureResourceGroup creates the group when it is absent and says whether it did.
func (c *azureClient) ensureResourceGroup(ctx context.Context, creds azureCredentials, subscription, group, location string, tags map[string]string) (bool, error) {
	path := azureResourceGroupPath(subscription, group)
	_, err := c.do(ctx, creds, http.MethodGet, path, azureAPIResourceGroups, nil, nil)
	if err == nil {
		return false, nil
	}
	if !azureNotFound(err) {
		return false, err
	}
	_, err = c.do(ctx, creds, http.MethodPut, path, azureAPIResourceGroups, azureResourceGroup{Location: location, Tags: tags}, nil)
	return err == nil, err
}

// resourceGroupExists reports whether the group is still there.
func (c *azureClient) resourceGroupExists(ctx context.Context, creds azureCredentials, subscription, group string) (bool, error) {
	_, err := c.do(ctx, creds, http.MethodGet, azureResourceGroupPath(subscription, group), azureAPIResourceGroups, nil, nil)
	if azureNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (c *azureClient) deleteResourceGroup(ctx context.Context, creds azureCredentials, subscription, group string) error {
	_, err := c.do(ctx, creds, http.MethodDelete, azureResourceGroupPath(subscription, group), azureAPIResourceGroups, nil, nil)
	if azureNotFound(err) {
		return nil
	}
	return err
}

// getManagedCluster returns nil, nil when the cluster does not exist.
func (c *azureClient) getManagedCluster(ctx context.Context, creds azureCredentials, subscription, group, name string) (*azureManagedCluster, error) {
	var found azureManagedCluster
	_, err := c.do(ctx, creds, http.MethodGet, azureManagedClusterPath(subscription, group, name), azureAPIManagedClusters, nil, &found)
	if azureNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &found, nil
}

// latestOperation returns the most recent operation Azure ran on the cluster, or nil, nil when
// Azure keeps none for it.
func (c *azureClient) latestOperation(ctx context.Context, creds azureCredentials, subscription, group, name string) (*azureOperation, error) {
	var op azureOperation
	_, err := c.do(ctx, creds, http.MethodGet, azureManagedClusterPath(subscription, group, name)+"/operations/latest", azureAPIOperations, nil, &op)
	if azureNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &op, nil
}

func (c *azureClient) createManagedCluster(ctx context.Context, creds azureCredentials, subscription, group, name string, body azureManagedCluster) error {
	_, err := c.do(ctx, creds, http.MethodPut, azureManagedClusterPath(subscription, group, name), azureAPIManagedClusters, body, nil)
	return err
}

func (c *azureClient) deleteManagedCluster(ctx context.Context, creds azureCredentials, subscription, group, name string) error {
	_, err := c.do(ctx, creds, http.MethodDelete, azureManagedClusterPath(subscription, group, name), azureAPIManagedClusters, nil, nil)
	if azureNotFound(err) {
		return nil
	}
	return err
}

// adminKubeconfig fetches the cluster's admin credential.
func (c *azureClient) adminKubeconfig(ctx context.Context, creds azureCredentials, subscription, group, name string) ([]byte, error) {
	var listed struct {
		Kubeconfigs []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"kubeconfigs"`
	}
	path := azureManagedClusterPath(subscription, group, name) + "/listClusterAdminCredential"
	if _, err := c.do(ctx, creds, http.MethodPost, path, azureAPIManagedClusters, nil, &listed); err != nil {
		return nil, err
	}
	if len(listed.Kubeconfigs) == 0 || listed.Kubeconfigs[0].Value == "" {
		return nil, fmt.Errorf("azure: the cluster returned no admin kubeconfig")
	}
	decoded, err := base64.StdEncoding.DecodeString(listed.Kubeconfigs[0].Value)
	if err != nil {
		return nil, fmt.Errorf("azure: the admin kubeconfig is not base64: %w", err)
	}
	return decoded, nil
}
