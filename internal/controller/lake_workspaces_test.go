/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testWorkspaceID       = "123e4567-e89b-42d3-a456-426614174000"
	testWorkspaceCustomer = "223e4567-e89b-42d3-a456-426614174000"
	testWorkspaceName     = "prod-ws-123e4567e89b"
)

type fakeWorkspacesStore struct {
	rows    []LakeWorkspaceRow
	writes  int
	updated []LakeWorkspaceStatus
}

func (s *fakeWorkspacesStore) ListWorkspaces(context.Context) ([]LakeWorkspaceRow, error) {
	return append([]LakeWorkspaceRow(nil), s.rows...), nil
}

func (s *fakeWorkspacesStore) UpdateWorkspaceStatus(
	_ context.Context,
	row *LakeWorkspaceRow,
	status LakeWorkspaceStatus,
) (bool, error) {
	for i := range s.rows {
		candidate := &s.rows[i]
		if candidate.ID != row.ID || candidate.CustomerID != row.CustomerID ||
			candidate.Generation != row.Generation || candidate.Desired != row.Desired {
			continue
		}
		candidate.Phase = status.Phase
		candidate.ObservedGeneration = status.ObservedGeneration
		candidate.Message = status.Message
		candidate.Namespace = status.Namespace
		s.writes++
		s.updated = append(s.updated, status)
		return true, nil
	}
	return false, nil
}

func (s *fakeWorkspacesStore) Close(context.Context) error {
	return nil
}

func testWorkspaceLake() *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-prod-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake",
			Image:          "oramasearch/lake:test",
			PullSecretName: "private-pull",
			Workspaces: oramav1alpha1.LakeWorkspacesSpec{
				Enabled: true,
				Image:   "oramasearch/zetta:workspace",
				Storage: oramav1alpha1.OramaZettaStorage{Size: "20Gi", StorageClassName: "managed-csi"},
			},
		},
	}
}

func testWorkspaceRow(desired string) LakeWorkspaceRow {
	return LakeWorkspaceRow{
		ID:          testWorkspaceID,
		CustomerID:  testWorkspaceCustomer,
		Name:        "Production",
		Environment: "prod",
		Desired:     desired,
		Generation:  7,
	}
}

func newWorkspaceHarness(t *testing.T, lake *oramav1alpha1.Lake, objects ...client.Object) (*LakeWorkspacesReconciler, client.Client) {
	t.Helper()
	scheme := newLakeScheme(t)
	all := []client.Object{lake, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lake.Spec.PullSecretName, Namespace: lakeNamespace(lake)},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"registry.example.test":{}}}`)},
	}}
	all = append(all, objects...)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(all...).
		WithStatusSubresource(&oramav1alpha1.OramaZettaCluster{}).
		Build()
	return &LakeWorkspacesReconciler{
		Client:            c,
		Scheme:            scheme,
		APIReader:         c,
		OperatorNamespace: "orama-system",
	}, c
}

func TestLakeWorkspaceReconcilesIsolatedNamespaceAndStatus(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	reconciler, kube := newWorkspaceHarness(t, lake)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	if changing := reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows); !changing {
		t.Fatal("a new workspace did not report changing")
	}
	if store.writes != 1 || store.rows[0].Phase != WorkspacePhasePending || store.rows[0].Namespace != testWorkspaceName {
		t.Fatalf("workspace status = %+v, writes %d", store.rows[0], store.writes)
	}

	var namespace corev1.Namespace
	if err := kube.Get(ctx, client.ObjectKey{Name: testWorkspaceName}, &namespace); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		workspaceLabel:                               testWorkspaceID,
		workspaceCustomerLabel:                       testWorkspaceCustomer,
		workspaceEnvironmentLabel:                    "prod",
		workspaceLakeLabel:                           lake.Name,
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
	} {
		if namespace.Labels[key] != want {
			t.Errorf("namespace label %s = %q, want %q", key, namespace.Labels[key], want)
		}
	}

	var account corev1.ServiceAccount
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: "default"}, &account); err != nil {
		t.Fatal(err)
	}
	if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
		t.Fatal("workspace default ServiceAccount mounts API credentials")
	}
	var quota corev1.ResourceQuota
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceProfileName}, &quota); err != nil {
		t.Fatal(err)
	}
	requestCPU := quota.Spec.Hard[corev1.ResourceRequestsCPU]
	requestMemory := quota.Spec.Hard[corev1.ResourceRequestsMemory]
	if requestCPU.String() != "4" || requestMemory.String() != "8Gi" {
		t.Fatalf("workspace quota = %#v", quota.Spec.Hard)
	}
	var limits corev1.LimitRange
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceProfileName}, &limits); err != nil {
		t.Fatal(err)
	}
	if len(limits.Spec.Limits) != 1 || limits.Spec.Limits[0].DefaultRequest.Cpu().String() != "100m" {
		t.Fatalf("workspace limits = %#v", limits.Spec.Limits)
	}

	var policies networkingv1.NetworkPolicyList
	if err := kube.List(ctx, &policies, client.InNamespace(testWorkspaceName)); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 5 {
		t.Fatalf("network policies = %d, want 5", len(policies.Items))
	}
	for _, policy := range policies.Items {
		if policy.Name == workspaceInternetName {
			t.Fatal("internet egress is allowed without an explicit workspace override")
		}
		if policy.Name == workspaceLakeIngressName {
			operatorNamespace := policy.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
			if operatorNamespace != "orama-system" {
				t.Fatalf("operator ingress namespace = %q", operatorNamespace)
			}
		}
	}
	var cluster oramav1alpha1.OramaZettaCluster
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: testWorkspaceName}, &cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Labels[workspaceLabel] != testWorkspaceID || cluster.Spec.HostNetwork ||
		cluster.Spec.IngressEnabled || cluster.Spec.PullSecretName != lake.Spec.PullSecretName ||
		cluster.Spec.Storage.Size != "20Gi" {
		t.Fatalf("workspace Zetta = %+v", cluster)
	}
	var pull corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: lake.Spec.PullSecretName}, &pull); err != nil {
		t.Fatal(err)
	}
	if pull.Labels[workspaceLabel] != testWorkspaceID || len(pull.Data[corev1.DockerConfigJsonKey]) == 0 {
		t.Fatal("workspace pull credential was not localized")
	}

	cluster.Status.Phase = "Ready"
	cluster.Status.ReadyNodes = 1
	if err := kube.Status().Update(ctx, &cluster); err != nil {
		t.Fatal(err)
	}
	if changing := reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows); changing {
		t.Fatal("ready workspace remained changing")
	}
	if store.rows[0].Phase != WorkspacePhaseReady || store.writes != 2 {
		t.Fatalf("ready status = %+v, writes %d", store.rows[0], store.writes)
	}
	if changing := reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows); changing || store.writes != 2 {
		t.Fatalf("steady workspace changed or rewrote status: changing %v writes %d", changing, store.writes)
	}
}

func TestLakeWorkspaceInternetEgressExcludesClusterAndMetadata(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	lake.Spec.Workspaces.Overrides = map[string]oramav1alpha1.LakeWorkspaceOverrideSpec{
		testWorkspaceID: {InternetEgress: true},
	}
	kubernetes := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.1", ClusterIPs: []string{"10.0.0.1"}},
	}
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "203.0.114.10"}},
		}},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{PodCIDR: "10.244.0.0/24", PodCIDRs: []string{"10.244.0.0/24"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.1.0.4"},
			{Type: corev1.NodeExternalIP, Address: "203.0.114.9"},
		}},
	}
	reconciler, kube := newWorkspaceHarness(t, lake, kubernetes, endpoints, node)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	var policy networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceInternetName}, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].Ports) != 1 ||
		policy.Spec.Egress[0].Ports[0].Port.IntVal != 443 {
		t.Fatalf("internet rule = %#v", policy.Spec.Egress)
	}
	excluded := map[string]bool{}
	for _, peer := range policy.Spec.Egress[0].To {
		for _, cidr := range peer.IPBlock.Except {
			excluded[cidr] = true
		}
	}
	for _, cidr := range []string{"10.0.0.0/8", "169.254.0.0/16", "203.0.114.9/32", "203.0.114.10/32"} {
		if !excluded[cidr] {
			t.Errorf("internet policy does not exclude %s", cidr)
		}
	}
}

func TestLakeWorkspaceInternetFailureStillInstallsDefaultDeny(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	lake.Spec.Workspaces.Overrides = map[string]oramav1alpha1.LakeWorkspaceOverrideSpec{
		testWorkspaceID: {InternetEgress: true},
	}
	staleInternet := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceInternetName, Namespace: testWorkspaceName},
	}
	reconciler, kube := newWorkspaceHarness(t, lake, staleInternet)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	if changing := reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows); !changing {
		t.Fatal("a workspace with an unavailable internet exclusion source did not retry")
	}
	var policy networkingv1.NetworkPolicy
	key := client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceDefaultDenyName}
	if err := kube.Get(ctx, key, &policy); err != nil {
		t.Fatalf("default-deny policy was not installed before internet policy calculation: %v", err)
	}
	if len(policy.Spec.PolicyTypes) != 2 || len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 0 {
		t.Fatalf("default-deny policy = %#v", policy.Spec)
	}
	key = client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceInternetName}
	if err := kube.Get(ctx, key, &networkingv1.NetworkPolicy{}); client.IgnoreNotFound(err) != nil {
		t.Fatal("stale internet policy remained after exclusion discovery failed")
	}
	var cluster oramav1alpha1.OramaZettaCluster
	key = client.ObjectKey{Namespace: testWorkspaceName, Name: testWorkspaceName}
	if err := kube.Get(ctx, key, &cluster); client.IgnoreNotFound(err) != nil {
		t.Fatal("workspace workload was created before its network policy was complete")
	}
}

func TestLakeWorkspaceStoppedAndDeletedLifecycle(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	reconciler, kube := newWorkspaceHarness(t, lake)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-" + testWorkspaceName + "-zetta-0", Namespace: testWorkspaceName}}
	if err := kube.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	store.rows[0].Desired = "stopped"
	store.rows[0].Generation++
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	if store.rows[0].Phase != WorkspacePhaseStopped {
		t.Fatalf("stopped status = %+v", store.rows[0])
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(claim), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatal("stopping removed the workspace data volume")
	}
	store.rows[0].Desired = "deleted"
	store.rows[0].Generation++
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	if store.rows[0].Phase != WorkspacePhaseDeleted {
		t.Fatalf("deleted status = %+v", store.rows[0])
	}
	if err := kube.Get(ctx, client.ObjectKey{Name: testWorkspaceName}, &corev1.Namespace{}); client.IgnoreNotFound(err) != nil {
		t.Fatal("deleted workspace namespace remains")
	}
}

func TestLakeWorkspaceDeletesOnlyExactLegacyResources(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	scheme := newLakeScheme(t)
	legacy := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{
		Name:      testWorkspaceName,
		Namespace: lakeNamespace(lake),
		Labels:    map[string]string{serviceLakeLabel: lake.Name},
	}}
	if err := controllerutil.SetControllerReference(lake, legacy, scheme); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "data-" + testWorkspaceName + "-zetta-0",
		Namespace: lakeNamespace(lake),
	}}
	admin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      testWorkspaceName + "-zetta-admin",
		Namespace: lakeNamespace(lake),
		Labels:    map[string]string{"app.kubernetes.io/instance": testWorkspaceName},
	}}
	clusterToken := admin.DeepCopy()
	clusterToken.Name = testWorkspaceName + "-zetta-cluster"
	kek := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-instance-kek", Namespace: lakeNamespace(lake)}}
	storeVolume := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-store", Namespace: lakeNamespace(lake)}}
	reconciler, kube := newWorkspaceHarness(t, lake, legacy, claim, admin, clusterToken, kek, storeVolume)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	for range 4 {
		reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	}
	for _, object := range []client.Object{legacy, claim, admin, clusterToken} {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(object), object); client.IgnoreNotFound(err) != nil {
			t.Fatalf("legacy object remains: %T %s", object, client.ObjectKeyFromObject(object))
		}
	}
	for _, object := range []client.Object{kek, storeVolume} {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatalf("Lake control-plane object was touched: %T %s: %v", object, client.ObjectKeyFromObject(object), err)
		}
	}
	var cluster oramav1alpha1.OramaZettaCluster
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: testWorkspaceName}, &cluster); err != nil {
		t.Fatal("isolated replacement was not created")
	}
}

func TestLakeWorkspaceRejectsInvalidIdentityAndPlan(t *testing.T) {
	lake := testWorkspaceLake()
	reconciler, _ := newWorkspaceHarness(t, lake)
	row := testWorkspaceRow("running")
	row.ID = "not-a-uuid"
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{row}}
	reconciler.syncLakeWorkspaces(context.Background(), lake, store, store.rows)
	if store.rows[0].Phase != WorkspacePhaseFailed || store.rows[0].Message != "workspace id must be a lowercase canonical UUID" {
		t.Fatalf("invalid identity status = %+v", store.rows[0])
	}

	lake.Spec.Workspaces.DefaultPlan = "missing"
	reconciler, kube := newWorkspaceHarness(t, lake)
	store = &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(context.Background(), lake, store, store.rows)
	if store.rows[0].Phase != WorkspacePhaseFailed || store.rows[0].Message != `workspace plan "missing" does not exist` {
		t.Fatalf("missing plan status = %+v", store.rows[0])
	}
	var namespace corev1.Namespace
	if err := kube.Get(context.Background(), client.ObjectKey{Name: testWorkspaceName}, &namespace); client.IgnoreNotFound(err) != nil {
		t.Fatal("invalid plan created a workspace namespace")
	}
}

func TestLakeWorkspaceInvalidPlanPreservesLegacyCluster(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	lake.Spec.Workspaces.DefaultPlan = "missing"
	legacy := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{
		Name:      testWorkspaceName,
		Namespace: lakeNamespace(lake),
		Labels:    map[string]string{serviceLakeLabel: lake.Name},
	}}
	reconciler, kube := newWorkspaceHarness(t, lake, legacy)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(legacy), &oramav1alpha1.OramaZettaCluster{}); err != nil {
		t.Fatalf("invalid workspace plan deleted the legacy cluster: %v", err)
	}
}

func TestLakeWorkspacesListenerUsesDedicatedChannel(t *testing.T) {
	lake := testWorkspaceLake()
	reconciler, kube := newWorkspaceHarness(t, lake)
	_ = reconciler
	listener := NewLakeWorkspacesListener(kube)
	if listener.channel != "lake_workspaces" || listener.enabled == nil || !listener.enabled(lake) {
		t.Fatal("workspace listener is not scoped to enabled workspace Lakes")
	}
	lake.Spec.Workspaces.Enabled = false
	if listener.enabled(lake) {
		t.Fatal("workspace listener stays enabled after workspace synchronization is disabled")
	}
}

func TestWorkspaceIsolationVerifierCoversBoundary(t *testing.T) {
	script, err := os.ReadFile("../../hack/verify-workspace-isolation.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"automountServiceAccountToken: false",
		"allowPrivilegeEscalation: false",
		"seccompProfile:",
		"Kubernetes API",
		"/api/v1/pods",
		"/api/v1/secrets",
		"another workspace Zetta",
		"Lake Postgres",
		"169.254.169.254",
		"kube-system pod",
		"PASS: DNS and Lake Service are reachable",
		"kubectl get rolebindings",
		"kubectl get clusterrolebindings",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("workspace verifier does not cover %q", required)
		}
	}
}
