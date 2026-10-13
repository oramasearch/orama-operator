/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testSubscription = "11111111-2222-3333-4444-555555555555"
	testKubeconfig   = "apiVersion: v1\nkind: Config\nclusters: []\n"
)

// fakeAzure is the Azure Resource Manager and its token endpoint, as far as the AksCluster
// controller uses them. It records every request so a test can assert what was and was not sent.
type fakeAzure struct {
	t      *testing.T
	server *httptest.Server

	mu             sync.Mutex
	requests       []string // "METHOD path"
	tokenForms     []map[string]string
	groups         map[string]bool
	clusters       map[string]*azureManagedCluster
	rejectToken    bool
	deleteFinishes bool // a DELETE removes the cluster at once instead of marking it Deleting
}

func newFakeAzure(t *testing.T) *fakeAzure {
	t.Helper()
	f := &fakeAzure{t: t, groups: map[string]bool{}, clusters: map[string]*azureManagedCluster{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAzure) client() *azureClient {
	return &azureClient{ARMBase: f.server.URL, LoginBase: f.server.URL, HTTP: f.server.Client()}
}

func (f *fakeAzure) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, request := range f.requests {
		if strings.HasPrefix(request, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeAzure) setState(name, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cluster := f.clusters[name]
	cluster.Properties.ProvisioningState = state
	for i := range cluster.Properties.AgentPoolProfiles {
		cluster.Properties.AgentPoolProfiles[i].ProvisioningState = state
	}
}

func (f *fakeAzure) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	respond := func(status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if value != nil {
			_ = json.NewEncoder(w).Encode(value)
		}
	}
	notFound := func() {
		respond(http.StatusNotFound, map[string]any{"error": map[string]string{"code": "ResourceNotFound", "message": "not found"}})
	}

	if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
		_ = r.ParseForm()
		form := map[string]string{}
		for _, pair := range strings.Split(string(body), "&") {
			if key, value, ok := strings.Cut(pair, "="); ok {
				form[key] = value
			}
		}
		f.tokenForms = append(f.tokenForms, form)
		if f.rejectToken {
			respond(http.StatusUnauthorized, map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: abc"})
			return
		}
		respond(http.StatusOK, map[string]any{"access_token": "test-token", "expires_in": 3600})
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		respond(http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "AuthenticationFailed", "message": "no token"}})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/subscriptions/"+testSubscription)
	switch {
	case path == "":
		respond(http.StatusOK, map[string]string{"subscriptionId": testSubscription})
	case strings.Contains(path, "/managedClusters/") && strings.HasSuffix(path, "/listClusterAdminCredential"):
		respond(http.StatusOK, map[string]any{"kubeconfigs": []map[string]string{{
			"name": "clusterAdmin", "value": base64.StdEncoding.EncodeToString([]byte(testKubeconfig)),
		}}})
	case strings.Contains(path, "/managedClusters/"):
		name := path[strings.LastIndex(path, "/")+1:]
		switch r.Method {
		case http.MethodGet:
			cluster, ok := f.clusters[name]
			if !ok {
				notFound()
				return
			}
			respond(http.StatusOK, cluster)
		case http.MethodPut:
			var cluster azureManagedCluster
			if err := json.Unmarshal(body, &cluster); err != nil {
				respond(http.StatusBadRequest, nil)
				return
			}
			cluster.ID = r.URL.Path
			cluster.Properties.ProvisioningState = "Creating"
			cluster.Properties.FQDN = name + "-dns-abc123.hcp.westus2.azmk8s.io"
			cluster.Properties.CurrentKubernetesVersion = cluster.Properties.KubernetesVersion + ".8"
			cluster.Properties.NodeResourceGroup = "MC_" + name
			cluster.Properties.PowerState = &azurePowerState{Code: "Running"}
			if cluster.Properties.OIDCIssuerProfile != nil {
				cluster.Properties.OIDCIssuerProfile.IssuerURL = "https://westus2.oic.prod-aks.azure.com/tenant/id/"
			}
			f.clusters[name] = &cluster
			respond(http.StatusCreated, &cluster)
		case http.MethodDelete:
			if _, ok := f.clusters[name]; !ok {
				notFound()
				return
			}
			if f.deleteFinishes {
				delete(f.clusters, name)
			} else {
				f.clusters[name].Properties.ProvisioningState = "Deleting"
			}
			respond(http.StatusAccepted, nil)
		}
	case strings.HasPrefix(path, "/resourcegroups/"):
		group := strings.TrimPrefix(path, "/resourcegroups/")
		switch r.Method {
		case http.MethodGet:
			if !f.groups[group] {
				notFound()
				return
			}
			respond(http.StatusOK, map[string]string{"name": group})
		case http.MethodPut:
			f.groups[group] = true
			respond(http.StatusCreated, map[string]string{"name": group})
		case http.MethodDelete:
			delete(f.groups, group)
			respond(http.StatusAccepted, nil)
		}
	default:
		notFound()
	}
}

func clusterTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, oramav1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func clusterTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.AksCluster{}, &oramav1alpha1.KubernetesCluster{}, &oramav1alpha1.KubernetesClusterProvider{}).
		Build()
}

func azureCredentialsSecret(withSecret bool) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: "orama-system"},
		Data:       map[string][]byte{"tenantID": []byte("tenant-1"), "clientID": []byte("client-1")},
	}
	if withSecret {
		sec.Data["clientSecret"] = []byte("s3cret-value")
	}
	return sec
}

func testAksCluster(policy oramav1alpha1.ClusterDeletionPolicy) *oramav1alpha1.AksCluster {
	one, five := int32(1), int32(5)
	return &oramav1alpha1.AksCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orama-nonprod-cluster-01"},
		Spec: oramav1alpha1.AksClusterSpec{
			SubscriptionID:       testSubscription,
			ResourceGroup:        "orama-nonprod-cluster-01_group",
			Location:             "westus2",
			CredentialsSecretRef: oramav1alpha1.ClusterSecretReference{Name: "azure", Namespace: "orama-system"},
			KubernetesVersion:    "1.35",
			SKUTier:              "Standard",
			Network:              oramav1alpha1.AksNetworkSettings{Plugin: "azure", PluginMode: "overlay", Dataplane: "cilium", Policy: "cilium"},
			WorkloadIdentity:     true,
			NodePools: []oramav1alpha1.AksNodePool{{
				Name: "agentpool", Mode: "System", VMSize: "Standard_D8as_v7", Count: 2,
				EnableAutoScaling: true, MinCount: &one, MaxCount: &five, OSDiskSizeGB: 256,
			}},
			DeletionPolicy:      policy,
			KubeconfigSecretRef: oramav1alpha1.ClusterSecretReference{Name: "orama-nonprod-cluster-01-kubeconfig", Namespace: "orama-system"},
		},
	}
}

func reconcileAks(t *testing.T, r *AksClusterReconciler, name string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return result
}

func getAks(t *testing.T, c client.Client, name string) *oramav1alpha1.AksCluster {
	t.Helper()
	var cluster oramav1alpha1.AksCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &cluster); err != nil {
		t.Fatalf("get AksCluster: %v", err)
	}
	return &cluster
}

func TestAksClusterIsCreatedObservedAndItsKubeconfigDelivered(t *testing.T) {
	azure := newFakeAzure(t)
	c := clusterTestClient(t, testAksCluster(oramav1alpha1.ClusterDeletionDelete), azureCredentialsSecret(true))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	name := "orama-nonprod-cluster-01"

	// First pass: the finalizer, the resource group and the create request.
	if result := reconcileAks(t, r, name); result.RequeueAfter != aksPollInterval {
		t.Fatalf("a cluster being created is polled every %s, got %s", aksPollInterval, result.RequeueAfter)
	}
	cluster := getAks(t, c, name)
	if len(cluster.Finalizers) != 1 || cluster.Finalizers[0] != aksClusterFinalizer {
		t.Fatalf("finalizer not registered: %v", cluster.Finalizers)
	}
	if cluster.Status.Phase != clusterPhaseProvisioning || !cluster.Status.CreatedResourceGroup {
		t.Fatalf("after create: phase %q, createdResourceGroup %t", cluster.Status.Phase, cluster.Status.CreatedResourceGroup)
	}
	sent := azure.clusters[name]
	if sent == nil {
		t.Fatal("no managed cluster was created in Azure")
	}
	if sent.Location != "westus2" || sent.SKU.Tier != "Standard" || sent.Identity.Type != "SystemAssigned" {
		t.Fatalf("create request: location %q, tier %q, identity %q", sent.Location, sent.SKU.Tier, sent.Identity.Type)
	}
	if net := sent.Properties.NetworkProfile; net.NetworkDataplane != "cilium" || net.NetworkPluginMode != "overlay" {
		t.Fatalf("create request network: %+v", net)
	}
	if !sent.Properties.OIDCIssuerProfile.Enabled || !sent.Properties.SecurityProfile.WorkloadIdentity.Enabled {
		t.Fatal("workload identity was asked for and not sent")
	}
	pool := sent.Properties.AgentPoolProfiles[0]
	if pool.Mode != "System" || pool.VMSize != "Standard_D8as_v7" || !pool.EnableAutoScaling || *pool.MinCount != 1 || *pool.MaxCount != 5 || pool.OSDiskSizeGB != 256 {
		t.Fatalf("create request pool: %+v", pool)
	}

	// Still creating: nothing is created twice, and no credential is fetched yet.
	reconcileAks(t, r, name)
	if got := azure.count("PUT /subscriptions/" + testSubscription + "/resourcegroups/orama-nonprod-cluster-01_group/providers"); got != 1 {
		t.Fatalf("the cluster was created %d times", got)
	}
	if azure.count("POST ") != 1 { // the token request only
		t.Fatalf("a credential was fetched before the cluster was provisioned")
	}

	// Azure finishes: Ready, with the kubeconfig in its Secret.
	azure.setState(name, "Succeeded")
	if result := reconcileAks(t, r, name); result.RequeueAfter != aksSteadyRecheck {
		t.Fatalf("a ready cluster is rechecked every %s, got %s", aksSteadyRecheck, result.RequeueAfter)
	}
	cluster = getAks(t, c, name)
	if cluster.Status.Phase != clusterPhaseReady || cluster.Status.ProvisioningState != "Succeeded" {
		t.Fatalf("phase %q, provisioningState %q", cluster.Status.Phase, cluster.Status.ProvisioningState)
	}
	if !apimeta.IsStatusConditionTrue(cluster.Status.Conditions, clusterConditionReady) ||
		!apimeta.IsStatusConditionTrue(cluster.Status.Conditions, clusterConditionInSync) {
		t.Fatalf("conditions: %+v", cluster.Status.Conditions)
	}
	if cluster.Status.KubernetesVersion != "1.35.8" || cluster.Status.FQDN == "" || cluster.Status.OIDCIssuerURL == "" ||
		cluster.Status.NodeResourceGroup != "MC_"+name || cluster.Status.PowerState != "Running" {
		t.Fatalf("status not observed: %+v", cluster.Status)
	}
	if len(cluster.Status.NodePools) != 1 || cluster.Status.NodePools[0].Count != 2 {
		t.Fatalf("node pools: %+v", cluster.Status.NodePools)
	}
	var kubeconfig corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "orama-system", Name: name + "-kubeconfig"}, &kubeconfig); err != nil {
		t.Fatalf("kubeconfig Secret: %v", err)
	}
	if string(kubeconfig.Data[kubeconfigSecretKey]) != testKubeconfig {
		t.Fatalf("kubeconfig Secret holds %q", kubeconfig.Data[kubeconfigSecretKey])
	}
	if !metav1.IsControlledBy(&kubeconfig, cluster) {
		t.Fatal("the kubeconfig Secret is not owned by the AksCluster")
	}

	// Steady: no status write, no second credential fetch, no create.
	before := cluster.ResourceVersion
	credentialFetches := azure.count("POST /subscriptions/")
	reconcileAks(t, r, name)
	reconcileAks(t, r, name)
	if after := getAks(t, c, name).ResourceVersion; after != before {
		t.Fatalf("a steady cluster wrote its status: resourceVersion %s -> %s", before, after)
	}
	if got := azure.count("POST /subscriptions/"); got != credentialFetches {
		t.Fatalf("the admin credential was fetched again: %d -> %d", credentialFetches, got)
	}
	if got := azure.count("POST "); got != credentialFetches+1 {
		t.Fatalf("expected one token request across every reconcile, saw %d", got-credentialFetches)
	}
}

func TestAksClusterDeleteDestroysTheClusterAndTheGroupItCreated(t *testing.T) {
	azure := newFakeAzure(t)
	c := clusterTestClient(t, testAksCluster(oramav1alpha1.ClusterDeletionDelete), azureCredentialsSecret(true))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	name := "orama-nonprod-cluster-01"
	reconcileAks(t, r, name)
	azure.setState(name, "Succeeded")
	reconcileAks(t, r, name)

	if err := c.Delete(context.Background(), getAks(t, c, name)); err != nil {
		t.Fatal(err)
	}
	// Azure accepts the delete and takes time over it: the resource is held.
	reconcileAks(t, r, name)
	cluster := getAks(t, c, name)
	if cluster.Status.Phase != clusterPhaseDeleting || len(cluster.Finalizers) != 1 {
		t.Fatalf("while Azure deletes: phase %q, finalizers %v", cluster.Status.Phase, cluster.Finalizers)
	}
	reconcileAks(t, r, name)
	if got := azure.count("DELETE /subscriptions/" + testSubscription + "/resourcegroups/orama-nonprod-cluster-01_group/providers"); got != 1 {
		t.Fatalf("the delete was sent %d times while Azure was already deleting", got)
	}

	// The cluster is gone: the group the operator created goes next.
	azure.mu.Lock()
	delete(azure.clusters, name)
	azure.mu.Unlock()
	reconcileAks(t, r, name)
	if azure.groups["orama-nonprod-cluster-01_group"] {
		t.Fatal("the resource group the operator created was not deleted")
	}
	if len(getAks(t, c, name).Finalizers) != 1 {
		t.Fatal("the resource was released before the group was confirmed gone")
	}
	reconcileAks(t, r, name)
	var gone oramav1alpha1.AksCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the resource should be released once Azure has nothing left: %v", err)
	}
}

func TestAksClusterRetainLeavesTheClusterRunning(t *testing.T) {
	azure := newFakeAzure(t)
	c := clusterTestClient(t, testAksCluster(oramav1alpha1.ClusterDeletionRetain), azureCredentialsSecret(true))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	name := "orama-nonprod-cluster-01"
	reconcileAks(t, r, name)
	azure.setState(name, "Succeeded")
	reconcileAks(t, r, name)

	if err := c.Delete(context.Background(), getAks(t, c, name)); err != nil {
		t.Fatal(err)
	}
	reconcileAks(t, r, name)
	var gone oramav1alpha1.AksCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("Retain releases the resource at once: %v", err)
	}
	if azure.count("DELETE ") != 0 {
		t.Fatal("Retain sent a delete to Azure")
	}
	if azure.clusters[name] == nil || !azure.groups["orama-nonprod-cluster-01_group"] {
		t.Fatal("Retain must leave the cluster and its group in Azure")
	}
}

func TestAksClusterDoesNotDeleteAGroupItDidNotCreate(t *testing.T) {
	azure := newFakeAzure(t)
	azure.groups["orama-nonprod-cluster-01_group"] = true // made by a person, before the operator
	azure.deleteFinishes = true
	c := clusterTestClient(t, testAksCluster(oramav1alpha1.ClusterDeletionDelete), azureCredentialsSecret(true))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	name := "orama-nonprod-cluster-01"
	reconcileAks(t, r, name)
	if getAks(t, c, name).Status.CreatedResourceGroup {
		t.Fatal("an existing resource group was recorded as created by the operator")
	}
	azure.setState(name, "Succeeded")
	reconcileAks(t, r, name)
	if err := c.Delete(context.Background(), getAks(t, c, name)); err != nil {
		t.Fatal(err)
	}
	reconcileAks(t, r, name)
	reconcileAks(t, r, name)
	if !azure.groups["orama-nonprod-cluster-01_group"] {
		t.Fatal("a resource group the operator did not create was deleted")
	}
	if azure.clusters[name] != nil {
		t.Fatal("the cluster should be deleted")
	}
}

func TestAksClusterWaitsForCredentialsAndHoldsADeleteWithoutThem(t *testing.T) {
	azure := newFakeAzure(t)
	c := clusterTestClient(t, testAksCluster(oramav1alpha1.ClusterDeletionDelete))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	name := "orama-nonprod-cluster-01"

	reconcileAks(t, r, name)
	cluster := getAks(t, c, name)
	if cluster.Status.Phase != clusterPhasePending || !strings.Contains(cluster.Status.Message, "does not exist") {
		t.Fatalf("without credentials: phase %q, message %q", cluster.Status.Phase, cluster.Status.Message)
	}
	if len(azure.requests) != 0 {
		t.Fatalf("Azure was called without credentials: %v", azure.requests)
	}

	// Deleting a Delete-policy cluster without credentials must not orphan it silently.
	if err := c.Delete(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	reconcileAks(t, r, name)
	held := getAks(t, c, name)
	if len(held.Finalizers) != 1 || held.Status.Phase != clusterPhaseDeleting ||
		!strings.Contains(held.Status.Message, "cannot delete the cluster in Azure") {
		t.Fatalf("a delete without credentials must hold: finalizers %v, phase %q, message %q",
			held.Finalizers, held.Status.Phase, held.Status.Message)
	}
}

func TestAksClusterReportsWhatTheSpecAsksForThatTheClusterLacks(t *testing.T) {
	azure := newFakeAzure(t)
	c := clusterTestClient(t, testAksCluster(oramav1alpha1.ClusterDeletionRetain), azureCredentialsSecret(true))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	name := "orama-nonprod-cluster-01"
	reconcileAks(t, r, name)
	azure.setState(name, "Succeeded")
	reconcileAks(t, r, name)

	cluster := getAks(t, c, name)
	ten := int32(10)
	cluster.Spec.KubernetesVersion = "1.36"
	cluster.Spec.NodePools[0].MaxCount = &ten
	cluster.Spec.NodePools = append(cluster.Spec.NodePools, oramav1alpha1.AksNodePool{Name: "userpool", Mode: "User", VMSize: "Standard_D8as_v7"})
	if err := c.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	puts := azure.count("PUT ")
	reconcileAks(t, r, name)
	if azure.count("PUT ") != puts {
		t.Fatal("this version must not change an existing cluster")
	}
	inSync := apimeta.FindStatusCondition(getAks(t, c, name).Status.Conditions, clusterConditionInSync)
	if inSync == nil || inSync.Status != metav1.ConditionFalse {
		t.Fatalf("InSync should be false: %+v", inSync)
	}
	for _, want := range []string{"kubernetesVersion: want 1.36", "want 1 to 10 nodes, cluster allows 1 to 5", "node pool userpool: not in the cluster"} {
		if !strings.Contains(inSync.Message, want) {
			t.Errorf("InSync message lacks %q: %s", want, inSync.Message)
		}
	}
	if getAks(t, c, name).Status.Phase != clusterPhaseReady {
		t.Fatal("a difference in spec does not make a running cluster not Ready")
	}
}

// Azure refuses a create call outright for one tag name with a slash in it. A Kubernetes-style
// name is exactly that, so the refusal has to come from the operator, before Azure is called.
func TestATagNameAzureWouldRefuseIsRefusedBeforeAzureIsCalled(t *testing.T) {
	for name, want := range map[string]string{
		"orama.cloud/role": "reserves",
		"cost&centre":      "reserves",
		"":                 "empty name",
	} {
		if got := azureTagProblem(map[string]string{name: "x"}); !strings.Contains(got, want) {
			t.Errorf("tag %q: want a problem containing %q, got %q", name, want, got)
		}
	}
	if got := azureTagProblem(map[string]string{"role": "leaf", "purpose": "nonprod", "orama.cloud-role": "leaf"}); got != "" {
		t.Errorf("valid tags were refused: %s", got)
	}
	if got := azureTagProblem(map[string]string{"role": strings.Repeat("x", 257)}); !strings.Contains(got, "256") {
		t.Errorf("an over-long value was not refused: %q", got)
	}

	// The concrete kind refuses and sends nothing.
	azure := newFakeAzure(t)
	concrete := testAksCluster(oramav1alpha1.ClusterDeletionDelete)
	concrete.Spec.Tags = map[string]string{"orama.cloud/role": "leaf"}
	c := clusterTestClient(t, concrete, azureCredentialsSecret(true))
	r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	reconcileAks(t, r, concrete.Name)
	got := getAks(t, c, concrete.Name)
	if got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, `"orama.cloud/role"`) {
		t.Fatalf("phase %q, message %q", got.Status.Phase, got.Status.Message)
	}
	if len(azure.requests) != 0 {
		t.Fatalf("Azure was called for a cluster it would refuse: %v", azure.requests)
	}

	// The abstract kind says so too, and provisions nothing.
	abstract := testKubernetesCluster()
	abstract.Spec.Tags = map[string]string{"orama.cloud/role": "leaf"}
	c = clusterTestClient(t, abstract, testProvider())
	kr := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
	if _, err := kr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: abstract.Name}}); err == nil {
		t.Fatal("expected the reconcile to report the tag")
	}
	summary := getKC(t, c, abstract.Name)
	if summary.Status.Phase != clusterPhaseDegraded || !strings.Contains(summary.Status.Message, "spec.tags") {
		t.Fatalf("phase %q, message %q", summary.Status.Phase, summary.Status.Message)
	}
	var none oramav1alpha1.AksCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: abstract.Name}, &none); !apierrors.IsNotFound(err) {
		t.Fatal("a cluster with a refused tag must not provision anything")
	}
}

func TestAzureTokenUsesTheServicePrincipalSecretOrTheFederatedToken(t *testing.T) {
	azure := newFakeAzure(t)
	ctx := context.Background()

	client := azure.client()
	if _, err := client.token(ctx, azureCredentials{TenantID: "tenant-1", ClientID: "client-1", ClientSecret: "s3cret-value"}); err != nil {
		t.Fatal(err)
	}
	form := azure.tokenForms[0]
	if form["client_secret"] != "s3cret-value" || form["client_assertion"] != "" || form["grant_type"] != "client_credentials" {
		t.Fatalf("service principal form: %v", form)
	}

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected.jwt.value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client = azure.client()
	client.TokenFile = tokenFile
	creds := azureCredentials{TenantID: "tenant-1", ClientID: "client-1"}
	if creds.authMethod() != azureAuthWorkloadIdentity {
		t.Fatalf("no clientSecret selects workload identity, got %s", creds.authMethod())
	}
	if _, err := client.token(ctx, creds); err != nil {
		t.Fatal(err)
	}
	form = azure.tokenForms[1]
	if form["client_assertion"] != "projected.jwt.value" || form["client_secret"] != "" ||
		!strings.Contains(form["client_assertion_type"], "jwt-bearer") {
		t.Fatalf("workload identity form: %v", form)
	}

	// Workload identity with no projected token is an error that names the cause.
	client = azure.client()
	t.Setenv(azureFederatedTokenFileEnv, "")
	if _, err := client.token(ctx, creds); err == nil || !strings.Contains(err.Error(), azureFederatedTokenFileEnv) {
		t.Fatalf("expected an error naming %s, got %v", azureFederatedTokenFileEnv, err)
	}

	// A refused credential reports Azure's code and first line, never the secret.
	azure.rejectToken = true
	client = azure.client()
	_, err := client.token(ctx, azureCredentials{TenantID: "tenant-1", ClientID: "client-1", ClientSecret: "s3cret-value"})
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "s3cret-value") || strings.Contains(err.Error(), "Trace ID") {
		t.Fatalf("refused token error: %v", err)
	}
}

func testProvider() *oramav1alpha1.KubernetesClusterProvider {
	return &oramav1alpha1.KubernetesClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-orama"},
		Spec: oramav1alpha1.KubernetesClusterProviderSpec{
			Type:                 oramav1alpha1.KubernetesClusterProviderAKS,
			CredentialsSecretRef: oramav1alpha1.ClusterSecretReference{Name: "azure", Namespace: "orama-system"},
			AKS: &oramav1alpha1.AksProviderSettings{
				SubscriptionID: testSubscription, ResourceGroupSuffix: "_group", SKUTier: "Standard",
				Network:        oramav1alpha1.AksNetworkSettings{Plugin: "azure", PluginMode: "overlay", Dataplane: "cilium", Policy: "cilium"},
				UpgradeChannel: "patch", NodeOSUpgradeChannel: "NodeImage", OSDiskSizeGB: 256,
			},
		},
	}
}

func testKubernetesCluster() *oramav1alpha1.KubernetesCluster {
	one, five, zero, hundred := int32(1), int32(5), int32(0), int32(100)
	two := int32(2)
	return &oramav1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orama-nonprod-cluster-01"},
		Spec: oramav1alpha1.KubernetesClusterSpec{
			ProviderRef: "azure-orama", Region: "westus2", KubernetesVersion: "1.35",
			DeletionPolicy: oramav1alpha1.ClusterDeletionDelete,
			NodePools: []oramav1alpha1.ClusterNodePool{
				{Name: "agentpool", Role: oramav1alpha1.ClusterNodePoolSystem, MachineSize: "Standard_D8as_v7", Count: &two, MinCount: &one, MaxCount: &five, Zones: []string{"1", "2", "3"}},
				{Name: "userpool", Role: oramav1alpha1.ClusterNodePoolUser, MachineSize: "Standard_D8as_v7", MinCount: &zero, MaxCount: &hundred},
			},
		},
	}
}

func reconcileKC(t *testing.T, r *KubernetesClusterReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getKC(t *testing.T, c client.Client, name string) *oramav1alpha1.KubernetesCluster {
	t.Helper()
	var cluster oramav1alpha1.KubernetesCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &cluster); err != nil {
		t.Fatalf("get KubernetesCluster: %v", err)
	}
	return &cluster
}

func TestKubernetesClusterProvisionsTheConcreteKindItsProviderNames(t *testing.T) {
	c := clusterTestClient(t, testKubernetesCluster(), testProvider(), azureCredentialsSecret(true))
	r := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
	name := "orama-nonprod-cluster-01"
	reconcileKC(t, r, name)

	concrete := getAks(t, c, name)
	abstract := getKC(t, c, name)
	if !metav1.IsControlledBy(concrete, abstract) {
		t.Fatal("the AksCluster is not owned by its KubernetesCluster")
	}
	spec := concrete.Spec
	if spec.SubscriptionID != testSubscription || spec.ResourceGroup != "orama-nonprod-cluster-01_group" ||
		spec.Location != "westus2" || spec.ClusterName != name || spec.KubernetesVersion != "1.35" {
		t.Fatalf("mapped identity fields: %+v", spec)
	}
	if spec.SKUTier != "Standard" || spec.Network.Dataplane != "cilium" || spec.UpgradeChannel != "patch" || !spec.WorkloadIdentity {
		t.Fatalf("the provider's settings did not reach the concrete resource: %+v", spec)
	}
	if spec.DeletionPolicy != oramav1alpha1.ClusterDeletionDelete {
		t.Fatalf("deletionPolicy %q", spec.DeletionPolicy)
	}
	if spec.CredentialsSecretRef != (oramav1alpha1.ClusterSecretReference{Name: "azure", Namespace: "orama-system"}) ||
		spec.KubeconfigSecretRef != (oramav1alpha1.ClusterSecretReference{Name: name + "-kubeconfig", Namespace: "orama-system"}) {
		t.Fatalf("secret references: credentials %+v, kubeconfig %+v", spec.CredentialsSecretRef, spec.KubeconfigSecretRef)
	}
	system, user := spec.NodePools[0], spec.NodePools[1]
	if system.Mode != "System" || system.Count != 2 || !system.EnableAutoScaling || *system.MinCount != 1 || *system.MaxCount != 5 ||
		system.OSDiskSizeGB != 256 || len(system.AvailabilityZones) != 3 {
		t.Fatalf("system pool: %+v", system)
	}
	if user.Mode != "User" || user.Count != 0 || *user.MinCount != 0 || *user.MaxCount != 100 {
		t.Fatalf("user pool: %+v", user)
	}
	if abstract.Status.ConcreteRef != "AksCluster/"+name || abstract.Status.ProviderType != oramav1alpha1.KubernetesClusterProviderAKS ||
		abstract.Status.Phase != clusterPhasePending {
		t.Fatalf("abstract status before the concrete kind reports: %+v", abstract.Status)
	}

	// The concrete resource reports; the abstract kind summarizes it.
	concrete.Status = oramav1alpha1.AksClusterStatus{
		Phase: clusterPhaseReady, KubernetesVersion: "1.35.8", FQDN: "nonprod.hcp.westus2.azmk8s.io", KubeconfigWritten: true,
		NodePools: []oramav1alpha1.AksNodePoolStatus{{Name: "agentpool", ProvisioningState: "Succeeded", Count: 2}},
		Conditions: []metav1.Condition{
			{Type: clusterConditionReady, Status: metav1.ConditionTrue, Reason: "Provisioned", LastTransitionTime: metav1.Now()},
			{Type: clusterConditionInSync, Status: metav1.ConditionTrue, Reason: "InSync", LastTransitionTime: metav1.Now()},
		},
	}
	if err := c.Status().Update(context.Background(), concrete); err != nil {
		t.Fatal(err)
	}
	reconcileKC(t, r, name)
	abstract = getKC(t, c, name)
	if abstract.Status.Phase != clusterPhaseReady || abstract.Status.Endpoint != "https://nonprod.hcp.westus2.azmk8s.io:443" ||
		abstract.Status.KubernetesVersion != "1.35.8" || abstract.Status.KubeconfigSecretRef == nil ||
		abstract.Status.KubeconfigSecretRef.Name != name+"-kubeconfig" || len(abstract.Status.NodePools) != 1 {
		t.Fatalf("summarized status: %+v", abstract.Status)
	}
	if !apimeta.IsStatusConditionTrue(abstract.Status.Conditions, clusterConditionReady) {
		t.Fatalf("conditions: %+v", abstract.Status.Conditions)
	}

	// Steady: no write.
	before := abstract.ResourceVersion
	concreteBefore := getAks(t, c, name).ResourceVersion
	reconcileKC(t, r, name)
	if getKC(t, c, name).ResourceVersion != before || getAks(t, c, name).ResourceVersion != concreteBefore {
		t.Fatal("a steady KubernetesCluster wrote to itself or to its AksCluster")
	}
}

func TestKubernetesClusterWaitsForItsProviderAndRefusesAnInvalidSpec(t *testing.T) {
	c := clusterTestClient(t, testKubernetesCluster())
	r := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
	name := "orama-nonprod-cluster-01"
	reconcileKC(t, r, name)
	cluster := getKC(t, c, name)
	if cluster.Status.Phase != clusterPhasePending || !strings.Contains(cluster.Status.Message, `"azure-orama" does not exist`) {
		t.Fatalf("without a provider: phase %q, message %q", cluster.Status.Phase, cluster.Status.Message)
	}
	var concrete oramav1alpha1.AksCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &concrete); !apierrors.IsNotFound(err) {
		t.Fatal("nothing concrete may be created without a provider")
	}

	for message, mutate := range map[string]func(*oramav1alpha1.KubernetesCluster){
		"at least one node pool must have role system": func(kc *oramav1alpha1.KubernetesCluster) {
			kc.Spec.NodePools = kc.Spec.NodePools[1:]
		},
		"a system pool needs minCount of at least 1": func(kc *oramav1alpha1.KubernetesCluster) {
			zero := int32(0)
			kc.Spec.NodePools[0].MinCount = &zero
		},
		"set count, or minCount and maxCount": func(kc *oramav1alpha1.KubernetesCluster) {
			kc.Spec.NodePools[1].MinCount, kc.Spec.NodePools[1].MaxCount = nil, nil
		},
		"minCount 9 is greater than maxCount 5": func(kc *oramav1alpha1.KubernetesCluster) {
			nine := int32(9)
			kc.Spec.NodePools[0].MinCount = &nine
		},
	} {
		invalid := testKubernetesCluster()
		invalid.Name = "invalid"
		mutate(invalid)
		c := clusterTestClient(t, invalid, testProvider())
		r := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
		reconcileKC(t, r, "invalid")
		got := getKC(t, c, "invalid")
		if got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, message) {
			t.Errorf("want Degraded with %q, got phase %q, message %q", message, got.Status.Phase, got.Status.Message)
		}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "invalid"}, &concrete); !apierrors.IsNotFound(err) {
			t.Errorf("%q: an invalid cluster must not provision anything", message)
		}
	}
}

func TestKubernetesClusterDeletionWaitsForTheConcreteResource(t *testing.T) {
	c := clusterTestClient(t, testKubernetesCluster(), testProvider(), azureCredentialsSecret(true))
	r := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
	name := "orama-nonprod-cluster-01"
	reconcileKC(t, r, name)

	// The concrete resource holds its own finalizer until the vendor is done.
	concrete := getAks(t, c, name)
	concrete.Finalizers = []string{aksClusterFinalizer}
	if err := c.Update(context.Background(), concrete); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), getKC(t, c, name)); err != nil {
		t.Fatal(err)
	}
	reconcileKC(t, r, name)
	held := getKC(t, c, name)
	if held.Status.Phase != clusterPhaseDeleting || len(held.Finalizers) != 1 {
		t.Fatalf("while the concrete resource is deleting: phase %q, finalizers %v", held.Status.Phase, held.Finalizers)
	}
	if getAks(t, c, name).DeletionTimestamp.IsZero() {
		t.Fatal("the concrete resource was not asked to delete")
	}

	// The vendor finishes and the concrete resource goes; the abstract one follows.
	concrete = getAks(t, c, name)
	concrete.Finalizers = nil
	if err := c.Update(context.Background(), concrete); err != nil {
		t.Fatal(err)
	}
	reconcileKC(t, r, name)
	var gone oramav1alpha1.KubernetesCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the KubernetesCluster should be released: %v", err)
	}
}

func TestKubernetesClusterProviderChecksCredentialsAndCannotBeDeletedWhileInUse(t *testing.T) {
	azure := newFakeAzure(t)
	c := clusterTestClient(t, testProvider(), testKubernetesCluster(), azureCredentialsSecret(false))
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected.jwt.value"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := azure.client()
	client.TokenFile = tokenFile
	r := &KubernetesClusterProviderReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: client}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "azure-orama"}}); err != nil {
			t.Fatal(err)
		}
	}
	get := func() *oramav1alpha1.KubernetesClusterProvider {
		t.Helper()
		var provider oramav1alpha1.KubernetesClusterProvider
		if err := c.Get(context.Background(), types.NamespacedName{Name: "azure-orama"}, &provider); err != nil {
			t.Fatal(err)
		}
		return &provider
	}

	reconcile()
	provider := get()
	if !provider.Status.Ready || provider.Status.AuthMethod != azureAuthWorkloadIdentity || provider.Status.Clusters != 1 {
		t.Fatalf("provider status: %+v", provider.Status)
	}

	// In use: a delete is held, and says why.
	if err := c.Delete(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	reconcile()
	held := get()
	if len(held.Finalizers) != 1 || !strings.Contains(held.Status.Message, "cannot be deleted while 1 KubernetesCluster") {
		t.Fatalf("a provider in use must be held: finalizers %v, message %q", held.Finalizers, held.Status.Message)
	}

	// No longer in use: released.
	if err := c.Delete(context.Background(), testKubernetesCluster()); err != nil {
		t.Fatal(err)
	}
	reconcile()
	var gone oramav1alpha1.KubernetesClusterProvider
	if err := c.Get(context.Background(), types.NamespacedName{Name: "azure-orama"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("an unused provider should be released: %v", err)
	}
}

func TestKubernetesClusterProviderReportsRefusedCredentials(t *testing.T) {
	azure := newFakeAzure(t)
	azure.rejectToken = true
	c := clusterTestClient(t, testProvider(), azureCredentialsSecret(true))
	r := &KubernetesClusterProviderReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "azure-orama"}}); err != nil {
		t.Fatal(err)
	}
	var provider oramav1alpha1.KubernetesClusterProvider
	if err := c.Get(context.Background(), types.NamespacedName{Name: "azure-orama"}, &provider); err != nil {
		t.Fatal(err)
	}
	if provider.Status.Ready || provider.Status.AuthMethod != azureAuthServicePrincipal ||
		!strings.Contains(provider.Status.Message, "invalid_client") || strings.Contains(provider.Status.Message, "s3cret-value") {
		t.Fatalf("provider status with refused credentials: %+v", provider.Status)
	}
}
