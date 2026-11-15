/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testTunnelGrantID = "70000000-0000-4000-8000-000000000007"
	testTunnelUserID  = "80000000-0000-4000-8000-000000000008"
	testTunnelOther   = "90000000-0000-4000-8000-000000000009"
	testTunnelNS      = "workspace"
)

// fakeTunnelStore is the Lake tunnel contract in memory. Like the pgx query, it leaves out grants
// whose terminal status is recorded.
type fakeTunnelStore struct {
	*fakeServiceStore
	mu      sync.Mutex
	grants  []LakeTunnelGrant
	upserts []LakeTunnelGrantStatus
	err     error
}

func (f *fakeTunnelStore) ListTunnelGrants(context.Context) ([]LakeTunnelGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []LakeTunnelGrant
	for _, grant := range f.grants {
		if grant.Status != nil && grant.Status.ObservedGeneration == grant.Generation &&
			(grant.Status.Phase == oramav1alpha1.TunnelPhaseExpired || grant.Status.Phase == oramav1alpha1.TunnelPhaseRevoked) {
			continue
		}
		copied := grant
		if grant.Status != nil {
			status := *grant.Status
			copied.Status = &status
		}
		out = append(out, copied)
	}
	return out, nil
}

func (f *fakeTunnelStore) UpsertTunnelGrantStatus(_ context.Context, status LakeTunnelGrantStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, status)
	for i := range f.grants {
		if f.grants[i].ID == status.GrantID {
			copied := status
			f.grants[i].Status = &copied
		}
	}
	return nil
}

func (f *fakeTunnelStore) last(t *testing.T) LakeTunnelGrantStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.upserts) == 0 {
		t.Fatal("no tunnel grant status was written")
	}
	return f.upserts[len(f.upserts)-1]
}

type tunnelFixture struct {
	t        *testing.T
	ctx      context.Context
	scheme   *runtime.Scheme
	kube     client.Client
	lake     *oramav1alpha1.Lake
	now      time.Time
	clientCA tunnelCA
	store    *fakeTunnelStore
	lakeR    *LakeReconciler
	tunnelR  *TunnelReconciler
	zettaR   *ZettaTunnelReconciler
	relayR   *zettaTunnelRelayReconciler
	connR    *ConnectionReconciler
}

func newTunnelFixture(t *testing.T, extra ...client.Object) *tunnelFixture {
	t.Helper()
	ctx := context.Background()
	scheme := newLakeScheme(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake-system",
			PullSecretName: "registry",
			Workspaces:     oramav1alpha1.LakeWorkspacesSpec{},
		},
	}
	workspace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testTunnelNS, Labels: map[string]string{
		workspaceLabel: testWorkspaceID, workspaceLakeLabel: lake.Name, workspaceManagedByLabel: "orama-operator",
	}}}
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: testTunnelNS, UID: types.UID("sandbox-uid")},
		Spec:       oramav1alpha1.NetworkSandboxSpec{PodSelector: metav1.LabelSelector{}},
	}
	if err := controllerutil.SetControllerReference(lake, sandbox, scheme); err != nil {
		t.Fatal(err)
	}
	pull := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "lake-system"},
		Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	targetService := func(name string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testTunnelNS, Labels: map[string]string{serviceIDLabel: testConnectionTargetID}},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
				Name: "postgres", Port: 5432, TargetPort: intstr.FromInt32(5432), Protocol: corev1.ProtocolTCP,
			}}},
		}
	}
	kubernetesAPI := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.1"}}
	kubernetesEndpoints := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "20.0.0.10"}}}}}
	objects := append([]client.Object{lake, workspace, sandbox, pull, targetService("db-ro"), targetService("db-rw"), kubernetesAPI, kubernetesEndpoints}, extra...)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&oramav1alpha1.Connection{}, &oramav1alpha1.Tunnel{}, &oramav1alpha1.ZettaTunnel{},
			&oramav1alpha1.CloudflareTunnel{}, &oramav1alpha1.TunnelProvider{}, &oramav1alpha1.NetworkSandbox{},
			&oramav1alpha1.PeerConnection{},
			&appsv1.Deployment{}, &corev1.Service{},
		).
		WithObjects(objects...).
		Build()
	clientCA, err := newTunnelCA("customer CA", now)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return now }
	fixture := &tunnelFixture{
		t: t, ctx: ctx, scheme: scheme, kube: kube, lake: lake, now: now, clientCA: clientCA,
		store:   &fakeTunnelStore{fakeServiceStore: &fakeServiceStore{}},
		lakeR:   &LakeReconciler{Client: kube, Scheme: scheme},
		tunnelR: &TunnelReconciler{Client: kube, Scheme: scheme, Now: clock},
		zettaR:  &ZettaTunnelReconciler{Client: kube, Scheme: scheme, Now: clock},
		relayR:  &zettaTunnelRelayReconciler{Client: kube, Scheme: scheme, Now: clock},
		connR:   &ConnectionReconciler{Client: kube, Scheme: scheme},
	}
	previous := lakeTunnelClock
	lakeTunnelClock = clock
	t.Cleanup(func() { lakeTunnelClock = previous })
	return fixture
}

func (f *tunnelFixture) setNow(now time.Time) {
	f.now = now
	clock := func() time.Time { return now }
	f.tunnelR.Now, f.zettaR.Now, f.relayR.Now = clock, clock, clock
	lakeTunnelClock = clock
}

func (f *tunnelFixture) grant() LakeTunnelGrant {
	return LakeTunnelGrant{
		ID: testTunnelGrantID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testTunnelNS, UserID: testTunnelUserID, UserEmail: "dev@example.com",
		TargetKind: oramav1alpha1.TunnelTargetService, TargetID: testConnectionTargetID, Port: 5432,
		ExpiresAt: f.now.Add(2 * time.Hour), ClientCAPEM: string(f.clientCA.CertPEM), Generation: 1,
	}
}

func (f *tunnelFixture) request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testTunnelNS, Name: name}}
}

// settle runs every reconciler a few times, as the manager would after each change.
func (f *tunnelFixture) settle() {
	f.t.Helper()
	for i := 0; i < 4; i++ {
		f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, f.store)
		for _, step := range []func() error{
			func() error {
				_, err := f.tunnelR.Reconcile(f.ctx, f.request(lakeTunnelName(testTunnelGrantID)))
				return err
			},
			func() error {
				_, err := f.zettaR.Reconcile(f.ctx, f.request(zettaTunnelEndpointName(testTunnelGrantID)))
				return err
			},
			func() error {
				_, err := f.connR.Reconcile(f.ctx, f.request(tunnelConnectionName(testTunnelGrantID)))
				return err
			},
			func() error { _, err := f.relayR.Reconcile(f.ctx, zettaTunnelRelayRequest); return err },
		} {
			if err := step(); err != nil {
				f.t.Fatal(err)
			}
		}
	}
}

func (f *tunnelFixture) get(key client.ObjectKey, object client.Object) bool {
	f.t.Helper()
	err := f.kube.Get(f.ctx, key, object)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return true
}

func (f *tunnelFixture) markAvailable(namespace, name string) {
	f.t.Helper()
	var deployment appsv1.Deployment
	if !f.get(client.ObjectKey{Namespace: namespace, Name: name}, &deployment) {
		f.t.Fatalf("Deployment %s/%s does not exist", namespace, name)
	}
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas = *deployment.Spec.Replicas
	deployment.Status.UpdatedReplicas = *deployment.Spec.Replicas
	deployment.Status.AvailableReplicas = *deployment.Spec.Replicas
	if err := f.kube.Status().Update(f.ctx, &deployment); err != nil {
		f.t.Fatal(err)
	}
}

func (f *tunnelFixture) policy(name string) (networkingv1.NetworkPolicy, bool) {
	var policy networkingv1.NetworkPolicy
	return policy, f.get(client.ObjectKey{Namespace: testTunnelNS, Name: name}, &policy)
}

func TestZettaTunnelGrantReachesOnlyItsTargetAndRevocationEndsIt(t *testing.T) {
	f := newTunnelFixture(t)
	f.store.grants = []LakeTunnelGrant{f.grant()}
	f.settle()

	var tunnel oramav1alpha1.Tunnel
	if !f.get(client.ObjectKey{Namespace: testTunnelNS, Name: lakeTunnelName(testTunnelGrantID)}, &tunnel) {
		t.Fatal("the grant did not become a Tunnel")
	}
	if !metav1.IsControlledBy(&tunnel, f.lake) || tunnel.Spec.ProviderRef != "" || tunnel.Spec.Subject != "" {
		t.Fatalf("Tunnel = %+v; a ZettaTunnel grant must carry no e-mail address", tunnel.Spec)
	}
	var endpoint appsv1.Deployment
	if !f.get(client.ObjectKey{Namespace: testTunnelNS, Name: zettaTunnelEndpointName(testTunnelGrantID)}, &endpoint) {
		t.Fatal("no endpoint Deployment")
	}
	args := strings.Join(endpoint.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{
		"endpoint ",
		"--target db-rw.workspace.svc.cluster.local:5432",
		"--peer-name " + zettaTunnelRelayIdentity,
		"--health-listen 0.0.0.0:8080",
		"--audit -",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("endpoint args %q lack %q", args, want)
		}
	}
	podSpec := endpoint.Spec.Template.Spec
	if podSpec.ImagePullSecrets[0].Name != "registry" || *podSpec.SecurityContext.RunAsNonRoot != true ||
		*podSpec.Containers[0].SecurityContext.AllowPrivilegeEscalation || podSpec.Containers[0].Image != DefaultZettaTunnelImage {
		t.Fatalf("endpoint pod is not restricted or not pulling the private image: %+v", podSpec)
	}

	var endpointTLS corev1.Secret
	if !f.get(client.ObjectKey{Namespace: testTunnelNS, Name: zettaTunnelEndpointSecret(testTunnelGrantID)}, &endpointTLS) {
		t.Fatal("no endpoint certificate")
	}
	cert := parseTestCertificate(t, endpointTLS.Data[corev1.TLSCertKey])
	if !slices.Equal(cert.DNSNames, []string{"endpoint." + testTunnelGrantID}) {
		t.Fatalf("endpoint certificate names %v", cert.DNSNames)
	}

	// The relay is the only peer admitted to the endpoint, on its tunnel port only.
	relayIngress, ok := f.policy(zettaTunnelRelayAllowance(testTunnelGrantID))
	if !ok {
		t.Fatal("no relay-to-endpoint allowance was rendered")
	}
	from := relayIngress.Spec.Ingress[0].From[0]
	if relayIngress.Spec.PodSelector.MatchLabels[connectionTunnelIDLabel] != testTunnelGrantID ||
		from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != zettaTunnelNamespace ||
		from.PodSelector.MatchLabels[zettaTunnelComponentKey] != "relay" ||
		len(relayIngress.Spec.Ingress[0].Ports) != 1 || relayIngress.Spec.Ingress[0].Ports[0].Port.IntVal != zettaTunnelEndpointPort {
		t.Fatalf("relay allowance = %+v", relayIngress.Spec)
	}
	// The endpoint reaches the target's pods on the target port only.
	egress, ok := f.policy(tunnelConnectionName(testTunnelGrantID) + "-egress")
	if !ok {
		t.Fatal("no endpoint-to-target Connection policy")
	}
	if egress.Spec.PodSelector.MatchLabels[connectionTunnelIDLabel] != testTunnelGrantID ||
		egress.Spec.Egress[0].To[0].PodSelector.MatchLabels[serviceIDLabel] != testConnectionTargetID ||
		len(egress.Spec.Egress[0].Ports) != 1 || egress.Spec.Egress[0].Ports[0].Port.IntVal != 5432 {
		t.Fatalf("endpoint egress = %+v", egress.Spec)
	}

	// The relay serves the grant, and trusts the customer's CA for it.
	var grants corev1.ConfigMap
	if !f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayGrants}, &grants) {
		t.Fatal("no relay grants file")
	}
	var document zettaTunnelGrantsDocument
	if err := json.Unmarshal([]byte(grants.Data[zettaTunnelGrantsKey]), &document); err != nil {
		t.Fatal(err)
	}
	want := zettaTunnelGrant{
		ID: testTunnelGrantID, Workspace: testTunnelNS,
		Endpoint:  "tunnel-" + testTunnelGrantID + ".workspace.svc.cluster.local:7444",
		ExpiresAt: f.now.Add(2 * time.Hour).Format(time.RFC3339),
	}
	if fingerprints, err := zettaTunnelClientCAFingerprints(string(f.clientCA.CertPEM)); err != nil {
		t.Fatal(err)
	} else {
		want.ClientCASHA256 = fingerprints
	}
	if len(document.Grants) != 1 || !reflect.DeepEqual(document.Grants[0], want) {
		t.Fatalf("grants file = %+v", document)
	}
	if !strings.Contains(grants.Data[zettaTunnelClientCAKey], strings.TrimSpace(string(f.clientCA.CertPEM))) {
		t.Fatal("the relay does not trust the grant's client CA")
	}
	var relay appsv1.Deployment
	if f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayName}, &relay) {
		t.Fatal("the relay started before its public address was known")
	}
	if f.store.last(t).Phase != oramav1alpha1.TunnelPhasePending || f.store.last(t).Endpoint != "" {
		t.Fatalf("status before the relay serves = %+v", f.store.last(t))
	}

	var service corev1.Service
	f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayName}, &service)
	if service.Spec.Type != corev1.ServiceTypeLoadBalancer || service.Spec.Ports[0].Port != 443 ||
		service.Spec.Ports[0].TargetPort.IntVal != zettaTunnelRelayPort {
		t.Fatalf("relay Service = %+v", service.Spec)
	}
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "20.1.2.3"}}
	if err := f.kube.Status().Update(f.ctx, &service); err != nil {
		t.Fatal(err)
	}
	f.settle()
	if !f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayName}, &relay) {
		t.Fatal("the relay was not started")
	}
	relayArgs := strings.Join(relay.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{"relay ", "--grants /etc/zetta-tunnel/grants/grants.json", "--ca /etc/zetta-tunnel/grants/client-ca.pem", "--endpoint-ca /etc/zetta-tunnel/workload/ca.crt"} {
		if !strings.Contains(relayArgs, want) {
			t.Fatalf("relay args %q lack %q", relayArgs, want)
		}
	}
	var serverTLS corev1.Secret
	f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayServerTLS}, &serverTLS)
	serverCert := parseTestCertificate(t, serverTLS.Data[corev1.TLSCertKey])
	if len(serverCert.IPAddresses) != 1 || serverCert.IPAddresses[0].String() != "20.1.2.3" {
		t.Fatalf("relay server certificate addresses %v", serverCert.IPAddresses)
	}

	f.markAvailable(testTunnelNS, zettaTunnelEndpointName(testTunnelGrantID))
	f.markAvailable(zettaTunnelNamespace, zettaTunnelRelayName)
	f.settle()
	ready := f.store.last(t)
	var pki corev1.Secret
	f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelPKISecret}, &pki)
	if ready.Phase != oramav1alpha1.TunnelPhaseReady || ready.Endpoint != "20.1.2.3:443" ||
		ready.RelayCAPEM != string(pki.Data[zettaTunnelPKIRelayCA]) || ready.ObservedGeneration != 1 {
		t.Fatalf("ready status = %+v", ready)
	}
	if err := serverCert.CheckSignatureFrom(parseTestCertificate(t, []byte(ready.RelayCAPEM))); err != nil {
		t.Fatalf("relay_ca_pem does not verify the relay: %v", err)
	}
	writes := len(f.store.upserts)
	f.settle()
	if len(f.store.upserts) != writes {
		t.Fatal("a steady tunnel rewrote its Postgres status")
	}

	// Revocation: the endpoint, its policies and the relay entry all go before Revoked is reported.
	revoked := f.now.Add(time.Minute)
	f.store.mu.Lock()
	f.store.grants[0].RevokedAt = &revoked
	f.store.grants[0].Generation = 2
	f.store.mu.Unlock()
	f.settle()
	for _, name := range []string{zettaTunnelRelayAllowance(testTunnelGrantID), tunnelConnectionName(testTunnelGrantID) + "-egress", tunnelConnectionName(testTunnelGrantID) + "-ingress"} {
		if _, ok := f.policy(name); ok {
			t.Fatalf("policy %s survived revocation", name)
		}
	}
	if f.get(client.ObjectKey{Namespace: testTunnelNS, Name: zettaTunnelEndpointName(testTunnelGrantID)}, &endpoint) {
		t.Fatal("the endpoint survived revocation")
	}
	if f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayName}, &relay) {
		t.Fatal("the relay kept running with no tunnel")
	}
	if !f.get(client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelPKISecret}, &pki) {
		t.Fatal("the CAs handed to developers were deleted with the relay")
	}
	final := f.store.last(t)
	if final.Phase != oramav1alpha1.TunnelPhaseRevoked || final.ObservedGeneration != 2 || final.Endpoint != "" {
		t.Fatalf("revoked status = %+v", final)
	}
	f.settle()
	if f.get(client.ObjectKey{Namespace: testTunnelNS, Name: lakeTunnelName(testTunnelGrantID)}, &tunnel) {
		t.Fatal("the Tunnel of a recorded revocation was not deleted")
	}
}

func TestZettaTunnelExpiryRemovesTheGrantFromTheRelay(t *testing.T) {
	f := newTunnelFixture(t)
	other := f.grant()
	other.ID = testTunnelOther
	other.ExpiresAt = f.now.Add(10 * time.Hour)
	short := f.grant()
	short.ExpiresAt = f.now.Add(time.Hour)
	f.store.grants = []LakeTunnelGrant{short, other}
	for i := 0; i < 3; i++ {
		f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, f.store)
		for _, id := range []string{testTunnelGrantID, testTunnelOther} {
			if _, err := f.tunnelR.Reconcile(f.ctx, f.request(lakeTunnelName(id))); err != nil {
				t.Fatal(err)
			}
		}
	}
	var zetta oramav1alpha1.ZettaTunnelList
	if err := f.kube.List(f.ctx, &zetta); err != nil {
		t.Fatal(err)
	}
	grants, _, next := liveZettaTunnelGrants(zetta.Items, f.now)
	if len(grants) != 2 || grants[0].ID != testTunnelGrantID || grants[1].ID != testTunnelOther || !next.Equal(short.ExpiresAt) {
		t.Fatalf("live grants = %+v, next expiry %v", grants, next)
	}
	// Even before any controller runs, the relay's own view drops an expired grant.
	grants, _, _ = liveZettaTunnelGrants(zetta.Items, short.ExpiresAt)
	if len(grants) != 1 || grants[0].ID != testTunnelOther {
		t.Fatalf("grants at the first expiry = %+v", grants)
	}

	f.setNow(short.ExpiresAt.Add(time.Second))
	for i := 0; i < 4; i++ {
		f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, f.store)
		if _, err := f.tunnelR.Reconcile(f.ctx, f.request(lakeTunnelName(testTunnelGrantID))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.zettaR.Reconcile(f.ctx, f.request(zettaTunnelEndpointName(testTunnelGrantID))); err != nil {
			t.Fatal(err)
		}
	}
	var tunnel oramav1alpha1.Tunnel
	if f.get(client.ObjectKey{Namespace: testTunnelNS, Name: lakeTunnelName(testTunnelGrantID)}, &tunnel) &&
		tunnel.Status.Phase != oramav1alpha1.TunnelPhaseExpired {
		t.Fatalf("expired Tunnel phase = %q", tunnel.Status.Phase)
	}
	var child oramav1alpha1.ZettaTunnel
	if f.get(client.ObjectKey{Namespace: testTunnelNS, Name: zettaTunnelEndpointName(testTunnelGrantID)}, &child) {
		t.Fatal("the expired grant's ZettaTunnel remains")
	}
	expired := false
	for _, status := range f.store.upserts {
		if status.GrantID == testTunnelGrantID && status.Phase == oramav1alpha1.TunnelPhaseExpired {
			expired = true
		}
	}
	if !expired {
		t.Fatalf("no Expired status was written: %+v", f.store.upserts)
	}
}

func TestLakeTunnelIntakeEndsWithoutBuildingAnythingForAnAlreadyEndedGrant(t *testing.T) {
	f := newTunnelFixture(t)
	grant := f.grant()
	grant.ExpiresAt = f.now.Add(-time.Minute)
	f.store.grants = []LakeTunnelGrant{grant}
	f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, f.store)
	var tunnels oramav1alpha1.TunnelList
	if err := f.kube.List(f.ctx, &tunnels); err != nil {
		t.Fatal(err)
	}
	if len(tunnels.Items) != 0 || f.store.last(t).Phase != oramav1alpha1.TunnelPhaseExpired {
		t.Fatalf("ended grant made %d Tunnels; status %+v", len(tunnels.Items), f.store.last(t))
	}
}

func TestLakeTunnelIntakeReadsNothingBeforeTheContract(t *testing.T) {
	for _, missing := range []error{errNetworkGuardMissing, errTunnelCAMissing} {
		f := newTunnelFixture(t)
		f.store.err = missing
		if f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, f.store) {
			t.Fatalf("%v reported changing work", missing)
		}
		var tunnels oramav1alpha1.TunnelList
		if err := f.kube.List(f.ctx, &tunnels); err != nil {
			t.Fatal(err)
		}
		if len(tunnels.Items) != 0 || len(f.store.upserts) != 0 {
			t.Fatalf("%v: %d Tunnels, %d status writes", missing, len(tunnels.Items), len(f.store.upserts))
		}
	}
	// A store without the tunnel half (an older operator test double) is skipped too.
	f := newTunnelFixture(t)
	if f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, &fakeServiceStore{}) {
		t.Fatal("a store without tunnels reported changing work")
	}
}

func TestTunnelRefusesAProviderOfAnotherCustomerAndAConnectionItDoesNotOwn(t *testing.T) {
	provider := &oramav1alpha1.TunnelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: defaultTunnelProviderName, Namespace: testTunnelNS},
		Spec:       oramav1alpha1.TunnelProviderSpec{Type: oramav1alpha1.TunnelProviderZetta, CustomerID: testTunnelOther},
	}
	f := newTunnelFixture(t, provider)
	f.store.grants = []LakeTunnelGrant{f.grant()}
	f.settle()
	last := f.store.last(t)
	if last.Phase != oramav1alpha1.TunnelPhaseFailed || !strings.Contains(last.Message, "another customer") {
		t.Fatalf("status = %+v", last)
	}
	var children oramav1alpha1.ZettaTunnelList
	if err := f.kube.List(f.ctx, &children); err != nil || len(children.Items) != 0 {
		t.Fatalf("a refused provider still produced %d ZettaTunnels (%v)", len(children.Items), err)
	}

	// A tunnel-sourced Connection controlled by nothing the Lake made is refused.
	forged := &oramav1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: tunnelConnectionName(testTunnelGrantID), Namespace: testTunnelNS, Finalizers: []string{connectionFinalizer}},
		Spec: oramav1alpha1.ConnectionSpec{
			LakeRef: "prod", ConnectionID: testTunnelGrantID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
			SourceKind: oramav1alpha1.ConnectionSourceTunnel, SourceID: testTunnelGrantID,
			TargetKind: oramav1alpha1.ConnectionTargetService, TargetID: testConnectionTargetID,
			Ports: []int32{5432}, Desired: oramav1alpha1.ConnectionDesiredPresent, Generation: 1,
		},
	}
	if err := f.kube.Create(f.ctx, forged); err != nil {
		t.Fatal(err)
	}
	if _, err := f.connR.Reconcile(f.ctx, f.request(forged.Name)); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(forged), forged); err != nil {
		t.Fatal(err)
	}
	if forged.Status.Phase != oramav1alpha1.ConnectionPhaseFailed {
		t.Fatalf("unowned tunnel Connection phase = %q", forged.Status.Phase)
	}
	if _, ok := f.policy(forged.Name + "-egress"); ok {
		t.Fatal("an unowned tunnel Connection opened a path")
	}
}

func TestWorkspaceRendererKeepsTunnelAllowances(t *testing.T) {
	f := newTunnelFixture(t)
	f.store.grants = []LakeTunnelGrant{f.grant()}
	f.settle()
	names, err := workspaceTunnelAllowanceNames(f.ctx, f.kube, testTunnelNS)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, zettaTunnelRelayAllowance(testTunnelGrantID)) {
		t.Fatalf("names = %v", names)
	}
	workspaces := &LakeWorkspacesReconciler{Client: f.kube, Scheme: f.scheme}
	kept, err := workspaces.existingWorkspaceConnectionAllowances(f.ctx, testTunnelNS)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, policy := range kept {
		found = found || policy.Name == zettaTunnelRelayAllowance(testTunnelGrantID)
	}
	if !found {
		t.Fatal("the Lake workspace renderer would drop the relay allowance")
	}
}

func parseTestCertificate(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// Every projected grant carries the fingerprints of its own customer's CA and of no other (#844,
// LSR-074): the relay checks the issuer of a developer certificate against them.
func TestEveryRelayGrantCarriesItsOwnCustomerCAFingerprint(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	caPEM := func(name string) []byte {
		ca, err := newTunnelCA(name, now)
		if err != nil {
			t.Fatal(err)
		}
		return ca.CertPEM
	}
	first := caPEM("customer-one")
	second := caPEM("customer-two")
	tunnelFor := func(grantID, workspace string, caPEM []byte) oramav1alpha1.ZettaTunnel {
		return oramav1alpha1.ZettaTunnel{
			ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointName(grantID), Namespace: workspace},
			Spec: oramav1alpha1.ZettaTunnelSpec{
				TunnelGrantSpec: oramav1alpha1.TunnelGrantSpec{
					GrantID:     grantID,
					CustomerID:  "11111111-1111-4111-8111-111111111111",
					WorkspaceID: "22222222-2222-4222-8222-222222222222",
					TargetID:    "33333333-3333-4333-8333-333333333333",
					TargetKind:  oramav1alpha1.TunnelTargetService,
					Port:        5432,
					ExpiresAt:   metav1.NewTime(now.Add(time.Hour)),
				},
				ClientCAPEM: string(caPEM),
			},
		}
	}
	tunnels := []oramav1alpha1.ZettaTunnel{
		tunnelFor("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "ws-one", first),
		// A repeated certificate is projected once.
		tunnelFor("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "ws-two", append(append([]byte{}, second...), second...)),
	}
	grants, _, _ := liveZettaTunnelGrants(tunnels, now)
	if len(grants) != 2 {
		t.Fatalf("grants = %+v", grants)
	}
	fingerprint := func(caPEM []byte) string {
		certs, err := pemCertificates(caPEM)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(certs[0].Raw)
		return hex.EncodeToString(sum[:])
	}
	for i, want := range []string{fingerprint(first), fingerprint(second)} {
		got := grants[i].ClientCASHA256
		if len(got) != 1 || got[0] != want {
			t.Fatalf("grant %s client_ca_sha256 = %v, want only [%s]", grants[i].ID, got, want)
		}
	}
	document, err := zettaTunnelGrantsData(grants, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document[zettaTunnelGrantsKey], `"client_ca_sha256":["`+fingerprint(first)+`"]`) {
		t.Fatalf("grants.json does not carry client_ca_sha256: %s", document[zettaTunnelGrantsKey])
	}

	// More CAs than the relay accepts make the ZettaTunnel invalid, so it is never projected.
	var many []byte
	for i := 0; i <= zettaTunnelMaxClientCAs; i++ {
		many = append(many, caPEM(fmt.Sprintf("ca-%d", i))...)
	}
	tooMany := tunnelFor("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "ws-three", many)
	if validateZettaTunnel(&tooMany) == "" {
		t.Fatal("a ZettaTunnel with 17 client CAs is valid")
	}
	if grants, _, _ := liveZettaTunnelGrants([]oramav1alpha1.ZettaTunnel{tooMany}, now); len(grants) != 0 {
		t.Fatalf("a grant with 17 client CAs was projected: %+v", grants)
	}
}
