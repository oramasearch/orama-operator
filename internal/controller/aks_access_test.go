/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestAksAccessCreationSecurity(t *testing.T) {
	cluster := testAksCluster(oramav1alpha1.ClusterDeletionRetain)
	body, err := azureManagedClusterBody(cluster)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Properties struct {
			Disabled bool `json:"disableLocalAccounts"`
			AAD      struct {
				Managed bool `json:"managed"`
				RBAC    bool `json:"enableAzureRBAC"`
			} `json:"aadProfile"`
			API struct {
				Ranges []string `json:"authorizedIPRanges"`
			} `json:"apiServerAccessProfile"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.Properties.Disabled || !wire.Properties.AAD.Managed || !wire.Properties.AAD.RBAC || !reflect.DeepEqual(wire.Properties.API.Ranges, cluster.Spec.Access.AuthorizedIPRanges) {
		t.Fatal("AKS create request permits local accounts or unrestricted API access")
	}
	for _, ranges := range [][]string{nil, {}, {"0.0.0.0/0"}, {"0.0.0.0/1", "128.0.0.0/1"}, {"::/0"}, {"::ffff:203.0.113.1/128"}, {"203.0.113.1/24"}, {"203.0.113.1"}, {" 203.0.113.1/32"}, {"203.0.113.1/32", "203.0.113.1/32"}} {
		cluster.Spec.Access.AuthorizedIPRanges = ranges
		if _, err := azureManagedClusterBody(cluster); err == nil {
			t.Errorf("accepted unsafe API ranges %q", ranges)
		}
	}
	cluster.Spec.Access = testAksAccess()
	cluster.Spec.Access.PrincipalID = ""
	if _, err := azureManagedClusterBody(cluster); err == nil {
		t.Fatal("accepted missing managing principal")
	}
	abstract, provider := testKubernetesCluster(), testProvider()
	abstract.Spec.Access.Enforce = true
	mapped := aksClusterSpecFor(abstract, provider)
	if !mapped.Access.Enforce || mapped.Access.PrincipalID != provider.Spec.AKS.Access.PrincipalID || !reflect.DeepEqual(mapped.Access.AuthorizedIPRanges, provider.Spec.AKS.Access.AuthorizedIPRanges) {
		t.Fatal("neutral provider path lost the secure access policy")
	}
}

func TestAksAccessMigrationRequiresConsentAndIdentityProof(t *testing.T) {
	azure := newFakeAzure(t)
	aks := testAksCluster(oramav1alpha1.ClusterDeletionRetain)
	aks.UID = types.UID("aks-owner")
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aks.Spec.KubeconfigSecretRef.Name, Namespace: aks.Spec.KubeconfigSecretRef.Namespace}, Data: map[string][]byte{kubeconfigSecretKey: []byte("legacy-admin-certificate")}}
	if err := controllerutil.SetControllerReference(aks, old, clusterTestScheme(t)); err != nil {
		t.Fatal(err)
	}
	body, err := azureManagedClusterBody(aks)
	if err != nil {
		t.Fatal(err)
	}
	body.Properties.DisableLocalAccounts = nil
	body.Properties.AADProfile = nil
	body.Properties.APIServerAccessProfile = nil
	body.Properties.ProvisioningState = "Succeeded"
	body.Properties.FQDN = aks.Name + "-dns-abc123.hcp.westus2.azmk8s.io"
	body.Properties.KubernetesVersion = "1.34"
	body.Properties.CurrentKubernetesVersion = "1.34.8"
	azure.clusters[aks.Name] = &body
	superseded := old.DeepCopy()
	superseded.Name = "superseded-kubeconfig"
	superseded.Labels = map[string]string{"orama.cloud/kubernetes-cluster": aks.Name}
	foreign := superseded.DeepCopy()
	foreign.Name = "foreign-kubeconfig"
	foreign.OwnerReferences = nil
	c := clusterTestClient(t, aks, old, superseded, foreign, azureCredentialsSecret(false))
	events := record.NewFakeRecorder(5)
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client(), Recorder: events, AccessCheck: allowFakeAksAccess}
	reconcileAks(t, r, aks.Name)
	first := getAks(t, c, aks.Name)
	if first.Status.Phase != clusterPhaseDegraded || apimeta.FindStatusCondition(first.Status.Conditions, clusterConditionReady).Reason != "InsecureAccess" || !first.Status.KubeconfigWritten {
		t.Fatalf("existing insecure cluster: %+v", first.Status)
	}
	if azure.count("PUT ") != 0 || azure.count("POST /subscriptions/") != 0 {
		t.Fatal("existing cluster was changed without consent")
	}
	var stored corev1.Secret
	key := client.ObjectKeyFromObject(old)
	if err := c.Get(context.Background(), key, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Data[kubeconfigSecretKey]) != "legacy-admin-certificate" {
		t.Fatal("pre-migration management credential was changed")
	}
	reconcileAks(t, r, aks.Name)
	if getAks(t, c, aks.Name).ResourceVersion != first.ResourceVersion || len(events.Events) != 1 {
		t.Fatal("insecure observation flapped status or repeated its event")
	}
	abstract := testKubernetesCluster()
	abstract.Status.ProviderType = oramav1alpha1.KubernetesClusterProviderAKS
	abstract.Status.Phase = clusterPhaseDegraded
	abstract.Status.KubeconfigSecretRef = &aks.Spec.KubeconfigSecretRef
	if err := c.Create(context.Background(), abstract); err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Update(context.Background(), abstract); err != nil {
		t.Fatal(err)
	}
	if data, waiting, err := managedClusterKubeconfig(context.Background(), c, aks.Name); err != nil || waiting != "" || string(data) != "legacy-admin-certificate" {
		t.Fatalf("pre-migration management must continue: %q %v", waiting, err)
	}
	first.Spec.Access.Enforce = true
	if err := c.Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, _, err := managedClusterKubeconfig(context.Background(), c, aks.Name); err == nil {
		// Degraded status blocks before the Secret can be used; a waiting result is also safe.
		data, _, _ := managedClusterKubeconfig(context.Background(), c, aks.Name)
		if len(data) != 0 {
			t.Fatal("enforced cluster used its legacy certificate")
		}
	}
	reconcileAks(t, r, aks.Name)
	migrated := azure.clusters[aks.Name]
	if migrated.Properties.KubernetesVersion != "1.34" || migrated.Properties.AgentPoolProfiles[0].VMSize != body.Properties.AgentPoolProfiles[0].VMSize {
		t.Fatal("security migration changed desired pool/version settings")
	}
	if migrated.Properties.DisableLocalAccounts == nil || !*migrated.Properties.DisableLocalAccounts || !migrated.Properties.AADProfile.Managed || len(azure.roles) != 1 {
		t.Fatal("security migration or role assignment missing")
	}
	azure.setState(aks.Name, "Succeeded")
	r.AccessCheck = func(context.Context, []byte) error { return fmt.Errorf("identity is not authorized yet") }
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: aks.Name}})
	if err == nil {
		t.Fatal("accepted unproven identity access")
	}
	if err := c.Get(context.Background(), key, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Data[kubeconfigSecretKey]) != "legacy-admin-certificate" {
		t.Fatal("erased recovery credential before identity worked")
	}
	r.AccessCheck = allowFakeAksAccess
	reconcileAks(t, r, aks.Name)
	var retired, untouched corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(superseded), &retired); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &untouched); err != nil {
		t.Fatal(err)
	}
	if len(retired.Data[kubeconfigSecretKey]) != 0 || len(untouched.Data[kubeconfigSecretKey]) == 0 {
		t.Fatal("credential retirement missed an old destination or changed a foreign Secret")
	}
	if getAks(t, c, aks.Name).Status.Phase != clusterPhaseReady {
		t.Fatal("secured cluster is not Ready")
	}
	if err := c.Get(context.Background(), key, &stored); err != nil {
		t.Fatal(err)
	}
	creds := azureCredentials{TenantID: "tenant-1", ClientID: "client-1"}
	if err := validateAksIdentityKubeconfig(stored.Data[kubeconfigSecretKey], creds, "https://"+body.Properties.FQDN+":443"); err != nil {
		t.Fatal(err)
	}
	if azure.count("POST /subscriptions/"+testSubscription+"/resourceGroups/"+aks.Spec.ResourceGroup+"/providers/Microsoft.ContainerService/managedClusters/"+aks.Name+"/listClusterAdminCredential") != 0 {
		t.Fatal("admin credential API was called")
	}
}

func TestAksAccessSanitizesAllAlternateCredentials(t *testing.T) {
	endpoint := "https://example-dns-abc123.hcp.westus2.azmk8s.io:443"
	creds := azureCredentials{TenantID: "tenant", ClientID: "client"}
	clean, err := aksIdentityKubeconfig([]byte(fakeAksUserKubeconfig("example")), creds, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*clientcmdapi.Config){
		func(c *clientcmdapi.Config) { c.AuthInfos["identity"].ClientCertificateData = []byte("certificate") },
		func(c *clientcmdapi.Config) { c.AuthInfos["identity"].ClientKeyData = []byte("key") },
		func(c *clientcmdapi.Config) { c.AuthInfos["identity"].Token = "bearer" },
		func(c *clientcmdapi.Config) { c.AuthInfos["identity"].TokenFile = "/tmp/token" },
		func(c *clientcmdapi.Config) {
			c.AuthInfos["identity"].Exec = &clientcmdapi.ExecConfig{Command: "unexpected"}
		},
		func(c *clientcmdapi.Config) {
			c.AuthInfos["unused"] = &clientcmdapi.AuthInfo{ClientKeyData: []byte("key")}
		},
		func(c *clientcmdapi.Config) { c.Clusters["cluster"].ProxyURL = "https://other.example" },
		func(c *clientcmdapi.Config) { c.Clusters["cluster"].Server = "https://other.example" },
		func(c *clientcmdapi.Config) { c.Clusters["cluster"].InsecureSkipTLSVerify = true },
	} {
		c, err := clientcmd.Load(clean)
		if err != nil {
			t.Fatal(err)
		}
		mutate(c)
		data, err := clientcmd.Write(*c)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateAksIdentityKubeconfig(data, creds, endpoint); err == nil {
			t.Fatal("accepted alternate credentials or endpoint in stored kubeconfig")
		}
	}
}

type aksRoundTripFunc func(*http.Request) (*http.Response, error)

func (f aksRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAksAccessTokensRefreshInMemoryAndNeverFailOpen(t *testing.T) {
	azure := newFakeAzure(t)
	c := azure.client()
	creds := azureCredentials{TenantID: "tenant-1", ClientID: "client-1"}
	if _, err := c.token(context.Background(), creds); err != nil {
		t.Fatal(err)
	}
	p := &aksWorkloadIdentityProvider{azure: c, creds: creds, host: "api.example"}
	calls := 0
	wrapped := p.WrapTransport(aksRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("request lacked identity token")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://api.example/api/v1/namespaces", nil)
	for i := 0; i < 2; i++ {
		if _, err := wrapped.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
	}
	if len(azure.tokenForms) != 2 {
		t.Fatal("AKS and ARM scopes must be isolated and cached")
	}
	form := azure.tokenForms[1]
	scope, _ := url.QueryUnescape(form["scope"])
	if scope != aksTokenScope || form["client_assertion"] != "fake-projected-jwt" || form["client_secret"] != "" {
		t.Fatal("leaf did not use workload identity with the AKS audience")
	}
	c.mu.Lock()
	for key, value := range c.tokens {
		value.expires = time.Now().Add(time.Second)
		c.tokens[key] = value
	}
	c.mu.Unlock()
	if _, err := wrapped.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if len(azure.tokenForms) != 3 {
		t.Fatal("near-expiry AKS token was not refreshed")
	}
	wrong, _ := http.NewRequest(http.MethodGet, "https://other.example/api", nil)
	if _, err := wrapped.RoundTrip(wrong); err == nil || calls != 3 {
		t.Fatal("identity token escaped the selected server")
	}
	c.mu.Lock()
	c.tokens = nil
	c.mu.Unlock()
	azure.rejectToken = true
	if _, err := wrapped.RoundTrip(req); err == nil || calls != 3 {
		t.Fatal("token failure sent an unauthenticated request")
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("transport mutated caller's request")
	}
}

func TestAksAccessProbeConfirmsAuthenticatedAccess(t *testing.T) {
	azure := newFakeAzure(t)
	status := http.StatusForbidden
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/namespaces" || req.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("identity probe did not authenticate the namespace read")
		}
		w.WriteHeader(status)
	}))
	defer api.Close()
	raw, err := clientcmd.Load([]byte(fakeAksUserKubeconfig("example")))
	if err != nil {
		t.Fatal(err)
	}
	raw.Clusters["aks"].Server = api.URL
	raw.Clusters["aks"].CertificateAuthorityData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	data, err := clientcmd.Write(*raw)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := aksIdentityKubeconfig(data, azureCredentials{TenantID: "tenant-1", ClientID: "client-1"}, api.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := &AksClusterReconciler{Azure: azure.client()}
	if err := r.verifyIdentityAccess(context.Background(), clean); err == nil {
		t.Fatal("403 was treated as proven identity access")
	}
	status = http.StatusOK
	if err := r.verifyIdentityAccess(context.Background(), clean); err != nil {
		t.Fatal(err)
	}
	// Exercise the registered provider through the same client-go configuration used by Helm.
	// Preload only a dummy token, so this test never calls an external identity endpoint.
	cacheKey := "tenant-1/client-1/" + azureAuthWorkloadIdentity + "/" + aksTokenScope
	aksIdentityAzure.mu.Lock()
	if aksIdentityAzure.tokens == nil {
		aksIdentityAzure.tokens = map[string]azureToken{}
	}
	aksIdentityAzure.tokens[cacheKey] = azureToken{value: "test-token", expires: time.Now().Add(time.Hour)}
	aksIdentityAzure.mu.Unlock()
	t.Cleanup(func() {
		aksIdentityAzure.mu.Lock()
		delete(aksIdentityAzure.tokens, cacheKey)
		aksIdentityAzure.mu.Unlock()
	})
	getter, _, err := leafClients(clean, "orama-system")
	if err != nil {
		t.Fatal(err)
	}
	config, err := getter.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	leafHTTP, err := rest.HTTPClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	response, err := leafHTTP.Get(api.URL + "/api/v1/namespaces")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("shared leaf client failed to authenticate with the registered provider")
	}

}

func TestAksAccessNewClusterNeverDowngradesToLegacy(t *testing.T) {
	azure := newFakeAzure(t)
	aks := testAksCluster(oramav1alpha1.ClusterDeletionRetain)
	c := clusterTestClient(t, aks, azureCredentialsSecret(false))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client(), AccessCheck: allowFakeAksAccess}
	reconcileAks(t, r, aks.Name)
	azure.setState(aks.Name, "Succeeded")
	reconcileAks(t, r, aks.Name)
	if !getAks(t, c, aks.Name).Status.AccessSecured {
		t.Fatal("secure creation did not prohibit legacy authentication")
	}
	azure.clusters[aks.Name].Properties.DisableLocalAccounts = nil
	before := azure.count("PUT ")
	reconcileAks(t, r, aks.Name)
	if azure.count("PUT ") != before+1 || getAks(t, c, aks.Name).Status.Phase != clusterPhaseProvisioning {
		t.Fatal("new cluster opted into the pre-migration legacy path after access drift")
	}
}

func TestAksAccessExistingSecureAzureStillRequiresExplicitCutover(t *testing.T) {
	azure := newFakeAzure(t)
	aks := testAksCluster(oramav1alpha1.ClusterDeletionRetain)
	body, err := azureManagedClusterBody(aks)
	if err != nil {
		t.Fatal(err)
	}
	body.Properties.AADProfile.TenantID = "tenant-1"
	body.Properties.ProvisioningState = "Succeeded"
	azure.clusters[aks.Name] = &body
	c := clusterTestClient(t, aks, azureCredentialsSecret(false))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client(), AccessCheck: allowFakeAksAccess}
	reconcileAks(t, r, aks.Name)
	state := getAks(t, c, aks.Name)
	if state.Status.Phase != clusterPhaseDegraded || state.Status.AccessSecured || azure.count("PUT ") != 0 || azure.count("POST /subscriptions/") != 0 {
		t.Fatal("existing cluster cut over to workload identity without explicit consent")
	}
}

func TestAksAccessWaitsForProviderBeforeReadingCredentials(t *testing.T) {
	cluster := testKubernetesCluster()
	cluster.Status.ProviderType = oramav1alpha1.KubernetesClusterProviderAKS
	cluster.Status.Phase = clusterPhaseReady
	cluster.Status.KubeconfigSecretRef = &oramav1alpha1.ClusterSecretReference{Name: "unread", Namespace: "orama-system"}
	c := clusterTestClient(t, cluster)
	data, waiting, err := managedClusterKubeconfig(context.Background(), c, cluster.Name)
	if err != nil || waiting == "" || len(data) != 0 {
		t.Fatalf("missing concrete access must wait without reading credentials: %q %v", waiting, err)
	}
}
