/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
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

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testPeerID = "a0000000-0000-4000-8000-00000000000a"

// peerCluster is one fake cluster with a namespace, its owned sandbox and a PeerConnection.
type peerCluster struct {
	kube client.WithWatch
	r    *PeerConnectionReconciler
	ns   string
	name string
}

func newPeerCluster(t *testing.T, scheme *runtime.Scheme, namespace string, peer *oramav1alpha1.PeerConnection, extra ...client.Object) *peerCluster {
	t.Helper()
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "stack", Namespace: namespace, UID: types.UID(namespace + "-owner")}}
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: namespace, UID: types.UID(namespace + "-sandbox"),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name, UID: owner.UID, Controller: boolPtr(true)}}},
		Spec: oramav1alpha1.NetworkSandboxSpec{PodSelector: metav1.LabelSelector{}},
	}
	objects := append([]client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, owner, sandbox, peer,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}, Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.1"}},
		&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}},
	}, extra...)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.PeerConnection{}, &oramav1alpha1.NetworkSandbox{}, &appsv1.Deployment{}, &corev1.Service{}).
		WithObjects(objects...).Build()
	return &peerCluster{kube: kube, r: &PeerConnectionReconciler{Client: kube, Scheme: scheme}, ns: namespace, name: peer.Name}
}

func (c *peerCluster) reconcile(t *testing.T) oramav1alpha1.PeerConnection {
	t.Helper()
	for i := 0; i < 3; i++ {
		if _, err := c.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: c.ns, Name: c.name}}); err != nil {
			t.Fatal(err)
		}
	}
	var peer oramav1alpha1.PeerConnection
	if err := c.kube.Get(context.Background(), client.ObjectKey{Namespace: c.ns, Name: c.name}, &peer); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return peer
}

func (c *peerCluster) publish(t *testing.T, ip string) {
	t.Helper()
	ctx := context.Background()
	var service corev1.Service
	if err := c.kube.Get(ctx, client.ObjectKey{Namespace: c.ns, Name: c.name + "-gateway"}, &service); err != nil {
		t.Fatal(err)
	}
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
	if err := c.kube.Status().Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
}

func (c *peerCluster) available(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var deployment appsv1.Deployment
	if err := c.kube.Get(ctx, client.ObjectKey{Namespace: c.ns, Name: c.name + "-gateway"}, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = 2
	if err := c.kube.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
}

func (c *peerCluster) document(t *testing.T) zettaTunnelPeerDocument {
	t.Helper()
	var configMap corev1.ConfigMap
	if err := c.kube.Get(context.Background(), client.ObjectKey{Namespace: c.ns, Name: c.name + "-gateway"}, &configMap); err != nil {
		t.Fatal(err)
	}
	var document zettaTunnelPeerDocument
	if err := decodeStrict([]byte(configMap.Data["peer.json"]), &document); err != nil {
		t.Fatalf("peer.json is not zetta-tunnel's exact document: %v", err)
	}
	return document
}

func (c *peerCluster) policy(t *testing.T, name string) (networkingv1.NetworkPolicy, bool) {
	t.Helper()
	var policy networkingv1.NetworkPolicy
	err := c.kube.Get(context.Background(), client.ObjectKey{Namespace: c.ns, Name: name}, &policy)
	if apierrors.IsNotFound(err) {
		return policy, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return policy, true
}

// fakePeerClusters maps clusterRef to a fake cluster's client.
type fakePeerClusters struct {
	mu       sync.Mutex
	clusters map[string]client.Client
}

func (f *fakePeerClusters) ClientFor(_ context.Context, clusterRef, _ string) (client.Client, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clusters[clusterRef]
	if !ok {
		return nil, "KubernetesCluster " + clusterRef + " is Pending, not Ready", nil
	}
	return c, "", nil
}

// TestPeerConnectionRehearsalBetweenTwoClusters is the nonprod rehearsal in miniature: the EU end
// exports its writer, the US end imports it, the credentials come only from the managing cluster's
// PeerLink, and revoking the peer removes everything at that end.
func TestPeerConnectionRehearsalBetweenTwoClusters(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	writer := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: "v1-eu"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "writer"},
			Ports: []corev1.ServicePort{{Port: 5432, TargetPort: intstr.FromInt32(5433)}}},
	}
	eu := newPeerCluster(t, scheme, "v1-eu", &oramav1alpha1.PeerConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "eu-us", Namespace: "v1-eu"},
		Spec: oramav1alpha1.PeerConnectionSpec{PeerID: testPeerID, Desired: "present", Generation: 1,
			Exports: []oramav1alpha1.PeerExport{{Name: "writer.eu", TargetKind: oramav1alpha1.PeerTargetKubernetesService, ServiceName: "writer", Port: 5432}}},
	}, writer)
	us := newPeerCluster(t, scheme, "v1-us", &oramav1alpha1.PeerConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "eu-us", Namespace: "v1-us"},
		Spec: oramav1alpha1.PeerConnectionSpec{PeerID: testPeerID, Desired: "present", Generation: 1,
			Imports: []oramav1alpha1.PeerImport{{Name: "writer.eu", Port: 5432}}},
	})
	link := &oramav1alpha1.PeerLink{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-eu-us"},
		Spec: oramav1alpha1.PeerLinkSpec{PeerID: testPeerID, Ends: []oramav1alpha1.PeerLinkEnd{
			{Name: "eu", ClusterRef: "orama-production-cluster-02", Namespace: "v1-eu", PeerConnection: "eu-us"},
			{Name: "us", ClusterRef: "orama-production-cluster-03", Namespace: "v1-us", PeerConnection: "eu-us"},
		}},
	}
	management := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.PeerLink{}).
		WithObjects(link, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}}).Build()
	clusters := &fakePeerClusters{clusters: map[string]client.Client{}}
	links := &PeerLinkReconciler{Client: management, Scheme: scheme, OperatorNamespace: "orama-system", Clusters: clusters,
		Now: func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }}
	reconcileLink := func() oramav1alpha1.PeerLink {
		t.Helper()
		for i := 0; i < 2; i++ {
			if _, err := links.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: link.Name}}); err != nil {
				t.Fatal(err)
			}
		}
		var current oramav1alpha1.PeerLink
		if err := management.Get(ctx, client.ObjectKeyFromObject(link), &current); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return current
	}

	// Without credentials from the managing cluster, nothing runs.
	if peer := us.reconcile(t); peer.Status.Phase != oramav1alpha1.PeerPhasePending || !strings.Contains(peer.Status.Message, "PeerLink") {
		t.Fatalf("US end without credentials = %+v", peer.Status)
	}
	var deployment appsv1.Deployment
	if err := us.kube.Get(ctx, client.ObjectKey{Namespace: "v1-us", Name: "eu-us-gateway"}, &deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("a gateway ran without credentials (%v)", err)
	}
	if current := reconcileLink(); current.Status.Phase != oramav1alpha1.PeerPhasePending {
		t.Fatalf("PeerLink with unreachable clusters = %+v", current.Status)
	}
	clusters.clusters["orama-production-cluster-02"] = eu.kube
	clusters.clusters["orama-production-cluster-03"] = us.kube
	reconcileLink()
	eu.reconcile(t)
	us.reconcile(t)
	eu.publish(t, "20.0.0.2")
	us.publish(t, "40.0.0.3")
	eu.reconcile(t)
	us.reconcile(t)
	if err := us.kube.Get(ctx, client.ObjectKey{Namespace: "v1-us", Name: "eu-us-gateway"}, &deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("a gateway ran before it knew the other end (%v)", err)
	}
	reconcileLink()
	eu.reconcile(t)
	us.reconcile(t)
	eu.available(t)
	us.available(t)
	eu.reconcile(t)
	us.reconcile(t)
	if current := reconcileLink(); current.Status.Phase != oramav1alpha1.PeerPhaseReady ||
		current.Status.Ends[0].Address != "20.0.0.2:7446" || current.Status.Ends[1].Address != "40.0.0.3:7446" {
		t.Fatalf("PeerLink = %+v", current.Status)
	}
	euPeer, usPeer := eu.reconcile(t), us.reconcile(t)
	if euPeer.Status.Phase != oramav1alpha1.PeerPhaseReady || usPeer.Status.Phase != oramav1alpha1.PeerPhaseReady {
		t.Fatalf("ends = %+v / %+v", euPeer.Status, usPeer.Status)
	}

	// Each end trusts exactly the other: same root, the other's exact identity.
	var euSecret, usSecret corev1.Secret
	if err := eu.kube.Get(ctx, client.ObjectKey{Namespace: "v1-eu", Name: "eu-us-credentials"}, &euSecret); err != nil {
		t.Fatal(err)
	}
	if err := us.kube.Get(ctx, client.ObjectKey{Namespace: "v1-us", Name: "eu-us-credentials"}, &usSecret); err != nil {
		t.Fatal(err)
	}
	euCert := parseTestCertificate(t, euSecret.Data[corev1.TLSCertKey])
	usCert := parseTestCertificate(t, usSecret.Data[corev1.TLSCertKey])
	if string(euSecret.Data["peer-name"]) != usCert.DNSNames[0] || string(usSecret.Data["peer-name"]) != euCert.DNSNames[0] ||
		usCert.DNSNames[0] != "us."+testPeerID+".peer.orama.internal" {
		t.Fatalf("identities: eu expects %s, us is %v; us expects %s, eu is %v",
			euSecret.Data["peer-name"], usCert.DNSNames, usSecret.Data["peer-name"], euCert.DNSNames)
	}
	if err := usCert.CheckSignatureFrom(parseTestCertificate(t, euSecret.Data["ca.crt"])); err != nil {
		t.Fatalf("the EU end cannot verify the US end: %v", err)
	}
	if string(usSecret.Data["remote"]) != "20.0.0.2:7446" || string(euSecret.Data["remote"]) != "40.0.0.3:7446" {
		t.Fatal("the managing cluster did not pass each end the other's address")
	}

	// The documents are zetta-tunnel's, complementary, and carry no routed address.
	euDoc, usDoc := eu.document(t), us.document(t)
	if euDoc.Peer.ID != testPeerID || usDoc.Peer.ID != testPeerID || euDoc.Peer.Remote != "40.0.0.3:7446" || usDoc.Peer.Remote != "20.0.0.2:7446" {
		t.Fatalf("documents: %+v / %+v", euDoc, usDoc)
	}
	if len(euDoc.Peer.Exports) != 1 || euDoc.Peer.Exports[0] != (zettaTunnelPeerExport{Name: "writer.eu", Port: 5432, Target: "writer.v1-eu.svc.cluster.local:5432"}) ||
		len(euDoc.Peer.Imports) != 0 {
		t.Fatalf("EU document = %+v", euDoc)
	}
	if len(usDoc.Peer.Imports) != 1 || usDoc.Peer.Imports[0] != (zettaTunnelPeerImport{Name: "writer.eu", Port: 5432, Listen: "0.0.0.0:20000"}) ||
		len(usDoc.Peer.Exports) != 0 {
		t.Fatalf("US document = %+v", usDoc)
	}
	if err := us.kube.Get(ctx, client.ObjectKey{Namespace: "v1-us", Name: "eu-us-gateway"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(deployment.Spec.Template.Spec.Containers[0].Args, " "); !strings.Contains(args, "peer --configuration /etc/zetta-tunnel/peer/peer.json") ||
		!strings.Contains(args, "--peer-name eu."+testPeerID+".peer.orama.internal") {
		t.Fatalf("US gateway args = %s", args)
	}

	// Only the named service is reachable: at the EU end only the gateway reaches only the writer's
	// pod port; at the US end the import is one Service and one listener.
	exportPolicy, ok := eu.policy(t, "peer-eu-us-export-0")
	if !ok || exportPolicy.Spec.PodSelector.MatchLabels["app"] != "writer" ||
		exportPolicy.Spec.Ingress[0].From[0].PodSelector.MatchLabels[peerIDLabel] != testPeerID ||
		len(exportPolicy.Spec.Ingress[0].Ports) != 1 || exportPolicy.Spec.Ingress[0].Ports[0].Port.IntVal != 5433 {
		t.Fatalf("EU export policy = %+v", exportPolicy.Spec)
	}
	euEgress, _ := eu.policy(t, "peer-eu-us-gateway-egress")
	if euEgress.Spec.Egress[0].To[0].IPBlock.CIDR != "40.0.0.3/32" || euEgress.Spec.Egress[0].Ports[0].Port.IntVal != 7446 {
		t.Fatalf("EU gateway reaches more than the US gateway: %+v", euEgress.Spec.Egress[0])
	}
	if len(euEgress.Spec.Egress) != 3 || euEgress.Spec.Egress[2].Ports[0].Port.IntVal != 5433 {
		t.Fatalf("EU gateway egress = %+v", euEgress.Spec.Egress)
	}
	var importService corev1.Service
	if err := us.kube.Get(ctx, client.ObjectKey{Namespace: "v1-us", Name: "peer-writer-eu"}, &importService); err != nil {
		t.Fatal(err)
	}
	if len(importService.Spec.Ports) != 1 || importService.Spec.Ports[0].Port != 5432 || importService.Spec.Ports[0].TargetPort.IntVal != 20000 {
		t.Fatalf("import Service = %+v", importService.Spec)
	}
	importsIn, ok := us.policy(t, "peer-eu-us-imports-ingress")
	if !ok || len(importsIn.Spec.Ingress[0].Ports) != 1 || importsIn.Spec.Ingress[0].Ports[0].Port.IntVal != 20000 {
		t.Fatalf("US import ingress = %+v", importsIn.Spec)
	}
	if _, ok := us.policy(t, "peer-eu-us-export-0"); ok {
		t.Fatal("the US end exports something it never declared")
	}
	if !strings.Contains(usPeer.Status.Message, "writer.eu:5432 at peer-writer-eu.v1-us.svc:5432") {
		t.Fatalf("US status message = %q", usPeer.Status.Message)
	}

	// Revoking the peer at the US end: import Service and policies first, then the gateway.
	var current oramav1alpha1.PeerConnection
	if err := us.kube.Get(ctx, client.ObjectKey{Namespace: "v1-us", Name: "eu-us"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Desired = oramav1alpha1.ConnectionDesiredAbsent
	current.Spec.Generation = 2
	if err := us.kube.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if revoked := us.reconcile(t); revoked.Status.Phase != oramav1alpha1.PeerPhaseDeleted || revoked.Status.ObservedGeneration != 2 {
		t.Fatalf("revoked = %+v", revoked.Status)
	}
	for _, object := range []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "peer-writer-eu"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "eu-us-gateway"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "eu-us-gateway"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "eu-us-gateway"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "peer-eu-us-imports-ingress"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "peer-eu-us-imports-egress"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "peer-eu-us-gateway-egress"}},
	} {
		object.SetNamespace("v1-us")
		if err := us.kube.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("%T %s survived revocation (%v)", object, object.GetName(), err)
		}
	}

	// Deleting the PeerLink takes the credentials back from both clusters.
	if err := management.Delete(ctx, link); err != nil {
		t.Fatal(err)
	}
	reconcileLink()
	for _, end := range []*peerCluster{eu, us} {
		var secret corev1.Secret
		if err := end.kube.Get(ctx, client.ObjectKey{Namespace: end.ns, Name: "eu-us-credentials"}, &secret); !apierrors.IsNotFound(err) {
			t.Fatalf("credentials remain in %s (%v)", end.ns, err)
		}
		end.reconcile(t)
		if err := end.kube.Get(ctx, client.ObjectKey{Namespace: end.ns, Name: "eu-us-gateway"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
			t.Fatalf("PeerLink deletion left the gateway authorized in %s (%v)", end.ns, err)
		}
	}
}

// fakePeerStore is Lake's network_peers contract in memory.
type fakePeerStore struct {
	*fakeServiceStore
	mu      sync.Mutex
	rows    []LakePeer
	upserts []LakePeerStatus
	err     error
}

func (f *fakePeerStore) ListPeers(context.Context) ([]LakePeer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []LakePeer
	for _, row := range f.rows {
		if row.Desired == "absent" && row.Status != nil && row.Status.Phase == "Deleted" && row.Status.ObservedGeneration == row.Generation {
			continue
		}
		copied := row
		if row.Status != nil {
			status := *row.Status
			copied.Status = &status
		}
		out = append(out, copied)
	}
	return out, nil
}

func (f *fakePeerStore) UpsertPeerStatus(_ context.Context, status LakePeerStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, status)
	for i := range f.rows {
		if f.rows[i].ID == status.PeerID {
			copied := status
			f.rows[i].Status = &copied
		}
	}
	return nil
}

func TestLakePeerRowBecomesAPeerConnectionAndAbsentEndsIt(t *testing.T) {
	f := newTunnelFixture(t)
	store := &fakePeerStore{fakeServiceStore: &fakeServiceStore{}, rows: []LakePeer{{
		ID: testPeerID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID, WorkspaceNamespace: testTunnelNS,
		Name: "to-us", Remote: "peer-us.example.com:7446",
		Exports: []byte(fmt.Sprintf(`[{"target_kind":"service","target_id":%q,"port":5432}]`, testConnectionTargetID)),
		Imports: []byte(`[{"name":"cache.us","port":6379}]`), Desired: "present", Generation: 1,
	}}}
	peerR := &PeerConnectionReconciler{Client: f.kube, Scheme: f.scheme}
	name := lakePeerName(testPeerID)
	f.lakeR.reconcileLakePeers(f.ctx, f.lake, store)
	var peer oramav1alpha1.PeerConnection
	if !f.get(client.ObjectKey{Namespace: testTunnelNS, Name: name}, &peer) {
		t.Fatalf("the row did not become a PeerConnection: %+v", store.upserts)
	}
	if !metav1.IsControlledBy(&peer, f.lake) || peer.Spec.Remote != "peer-us.example.com:7446" || peer.Spec.PullSecretName != "registry" ||
		len(peer.Spec.Exports) != 1 || peer.Spec.Exports[0].TargetID != testConnectionTargetID || peer.Spec.Imports[0].Name != "cache.us" {
		t.Fatalf("PeerConnection = %+v", peer.Spec)
	}
	run := func() {
		for i := 0; i < 3; i++ {
			if _, err := peerR.Reconcile(f.ctx, f.request(name)); err != nil {
				t.Fatal(err)
			}
			f.lakeR.reconcileLakePeers(f.ctx, f.lake, store)
		}
	}
	run()
	if last := store.upserts[len(store.upserts)-1]; last.Phase != oramav1alpha1.PeerPhasePending || !strings.Contains(last.Message, "PeerLink") {
		t.Fatalf("status without credentials = %+v", last)
	}
	// The credentials a PeerLink would write.
	now := time.Now()
	ca, _ := newTunnelCA("pair", now)
	cert, key, _ := issueTunnelLeaf(ca, tunnelLeafRequest{DNSNames: []string{"eu." + testPeerID + ".peer.orama.internal"}, Server: true, Client: true}, now)
	if err := f.kube.Create(f.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials", Namespace: testTunnelNS},
		Data: map[string][]byte{"tls.crt": cert, "tls.key": key, "ca.crt": ca.CertPEM, "peer-name": []byte("us." + testPeerID + ".peer.orama.internal")}}); err != nil {
		t.Fatal(err)
	}
	run()
	var configMap corev1.ConfigMap
	f.get(client.ObjectKey{Namespace: testTunnelNS, Name: name + "-gateway"}, &configMap)
	var document zettaTunnelPeerDocument
	if err := json.Unmarshal([]byte(configMap.Data["peer.json"]), &document); err != nil {
		t.Fatal(err)
	}
	if document.Peer.Exports[0] != (zettaTunnelPeerExport{Name: "db-rw", Port: 5432, Target: "db-rw.workspace.svc.cluster.local:5432"}) ||
		document.Peer.Workspace != testTunnelNS {
		t.Fatalf("a Lake export is named for its Service: %+v", document)
	}
	// The gateway reaches a DNS-named remote only on public addresses.
	egress, ok := f.policy("peer-" + name + "-gateway-egress")
	if !ok || egress.Spec.Egress[0].To[0].IPBlock.CIDR != "0.0.0.0/0" || !slices.Contains(egress.Spec.Egress[0].To[0].IPBlock.Except, "10.0.0.0/8") {
		t.Fatalf("gateway egress = %+v", egress.Spec)
	}
	names, err := workspaceTunnelAllowanceNames(f.ctx, f.kube, testTunnelNS)
	if err != nil || !slices.Contains(names, "peer-"+name+"-gateway-egress") || !slices.Contains(names, "peer-"+name+"-export-0") {
		t.Fatalf("the workspace renderer would drop peer allowances: %v (%v)", names, err)
	}

	store.mu.Lock()
	store.rows[0].Desired = "absent"
	store.rows[0].Generation = 2
	store.mu.Unlock()
	run()
	f.lakeR.reconcileLakePeers(f.ctx, f.lake, store)
	if last := store.upserts[len(store.upserts)-1]; last.Phase != oramav1alpha1.PeerPhaseDeleted || last.ObservedGeneration != 2 {
		t.Fatalf("absent status = %+v", last)
	}
	if _, ok := f.policy("peer-" + name + "-gateway-egress"); ok {
		t.Fatal("an absent peer's policy remains")
	}
	run()
	if f.get(client.ObjectKey{Namespace: testTunnelNS, Name: name}, &peer) && peer.DeletionTimestamp.IsZero() {
		t.Fatal("the PeerConnection of a recorded absent row was not deleted")
	}
}

func TestLakePeerRowsAreValidatedAndReadOnlyAfterTheGuard(t *testing.T) {
	f := newTunnelFixture(t)
	missing := &fakePeerStore{fakeServiceStore: &fakeServiceStore{}, err: errNetworkGuardMissing}
	if f.lakeR.reconcileLakePeers(f.ctx, f.lake, missing) || len(missing.upserts) != 0 {
		t.Fatal("work before network_guard")
	}
	for _, c := range []struct{ exports, imports, want string }{
		{`[{"target_kind":"service","target_id":"x","port":1}]`, `[]`, "target_id"},
		{`[{"target_kind":"tunnel","target_id":"` + testConnectionTargetID + `","port":1}]`, `[]`, "target_kind"},
		{`[]`, `[{"name":"10.0.0.1","port":1}]`, "address"},
		{`[]`, `[{"name":"a","port":1,"extra":true}]`, "imports must be"},
	} {
		store := &fakePeerStore{fakeServiceStore: &fakeServiceStore{}, rows: []LakePeer{{
			ID: testPeerID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID, WorkspaceNamespace: testTunnelNS,
			Remote: "1.2.3.4:7446", Exports: []byte(c.exports), Imports: []byte(c.imports), Desired: "present", Generation: 1,
		}}}
		f.lakeR.reconcileLakePeers(f.ctx, f.lake, store)
		if len(store.upserts) != 1 || store.upserts[0].Phase != oramav1alpha1.PeerPhaseFailed || !strings.Contains(store.upserts[0].Message, c.want) {
			t.Fatalf("%s %s: %+v", c.exports, c.imports, store.upserts)
		}
	}
}
