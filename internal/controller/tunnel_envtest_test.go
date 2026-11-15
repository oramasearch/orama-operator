// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// startTunnelEnvtest runs the Tunnel, ZettaTunnel (and relay), Connection and NetworkSandbox
// controllers against a real API server. It returns a direct client.
func startTunnelEnvtest(t *testing.T) client.Client {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the Tunnel envtest")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		CRDs:                  networkSandboxCiliumTestCRDs(),
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := newLakeScheme(t)
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, setup := range []interface{ SetupWithManager(ctrl.Manager) error }{
		&NetworkSandboxReconciler{Client: manager.GetClient(), Scheme: scheme},
		&ConnectionReconciler{Client: manager.GetClient(), Scheme: scheme},
		&TunnelReconciler{Client: manager.GetClient(), Scheme: scheme},
		&TunnelProviderReconciler{Client: manager.GetClient(), Scheme: scheme},
		&ZettaTunnelReconciler{Client: manager.GetClient(), Scheme: scheme},
	} {
		if err := setup.SetupWithManager(manager); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop manager: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return kube
}

func TestTunnelEnvtestServesTheGrantAndRemovesEverythingWhenItEnds(t *testing.T) {
	kube := startTunnelEnvtest(t)
	ctx := context.Background()
	await := func(what string, check func() (bool, error)) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 20*time.Second, true, func(context.Context) (bool, error) {
			return check()
		}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}, Spec: oramav1alpha1.LakeSpec{Namespace: "lake-system", Image: "oramasearch/lake:test"}}
	if err := kube.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{testTunnelNS, "default"} {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{workspaceLabel: testWorkspaceID}}}
		if err := kube.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	scheme := newLakeScheme(t)
	sandbox := &oramav1alpha1.NetworkSandbox{ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: testTunnelNS}}
	tunnel := &oramav1alpha1.Tunnel{
		ObjectMeta: metav1.ObjectMeta{Name: lakeTunnelName(testTunnelGrantID), Namespace: testTunnelNS},
		Spec: oramav1alpha1.TunnelSpec{
			LakeRef: lake.Name, GrantID: testTunnelGrantID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
			UserID: testTunnelUserID, TargetKind: oramav1alpha1.TunnelTargetService, TargetID: testConnectionTargetID,
			Port: 5432, ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second)), Generation: 1,
		},
	}
	ca, err := newTunnelCA("customer", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tunnel.Spec.ClientCAPEM = string(ca.CertPEM)
	for _, object := range []client.Object{sandbox, tunnel} {
		if err := controllerutil.SetControllerReference(lake, object, scheme); err != nil {
			t.Fatal(err)
		}
	}
	target := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db-rw", Namespace: testTunnelNS, Labels: map[string]string{serviceIDLabel: testConnectionTargetID}},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 5432, TargetPort: intstr.FromInt32(5432)}}},
	}
	for _, object := range []client.Object{sandbox, target, tunnel} {
		if err := kube.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}

	await("the endpoint, its Connection and the relay grant", func() (bool, error) {
		var endpoint appsv1.Deployment
		if err := kube.Get(ctx, client.ObjectKey{Namespace: testTunnelNS, Name: zettaTunnelEndpointName(testTunnelGrantID)}, &endpoint); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		var connection oramav1alpha1.Connection
		if err := kube.Get(ctx, client.ObjectKey{Namespace: testTunnelNS, Name: tunnelConnectionName(testTunnelGrantID)}, &connection); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		var grants corev1.ConfigMap
		if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayGrants}, &grants); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return connection.Status.Phase == oramav1alpha1.ConnectionPhaseReady &&
			strings.Contains(grants.Data[zettaTunnelGrantsKey], testTunnelGrantID), nil
	})
	var relayNamespace corev1.Namespace
	if err := kube.Get(ctx, client.ObjectKey{Name: zettaTunnelNamespace}, &relayNamespace); err != nil {
		t.Fatal(err)
	}
	if relayNamespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("relay namespace labels = %v", relayNamespace.Labels)
	}
	await("the relay namespace's default deny", func() (bool, error) {
		var policies networkingv1.NetworkPolicyList
		if err := kube.List(ctx, &policies, client.InNamespace(zettaTunnelNamespace)); err != nil {
			return false, err
		}
		return len(policies.Items) >= 4, nil
	})

	// Revocation ends it: the provider, its policies, its Connection and the relay grant go.
	var current oramav1alpha1.Tunnel
	if err := kube.Get(ctx, client.ObjectKeyFromObject(tunnel), &current); err != nil {
		t.Fatal(err)
	}
	revoked := metav1.NewTime(time.Now().Truncate(time.Second))
	current.Spec.RevokedAt = &revoked
	current.Spec.Generation = 2
	if err := kube.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	await("Revoked after the provider is gone", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(tunnel), &current); err != nil {
			return false, err
		}
		return current.Status.Phase == oramav1alpha1.TunnelPhaseRevoked && current.Status.ObservedGeneration == 2, nil
	})
	for _, object := range []client.Object{
		&oramav1alpha1.ZettaTunnel{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointName(testTunnelGrantID)}},
		&oramav1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: tunnelConnectionName(testTunnelGrantID)}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayAllowance(testTunnelGrantID)}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: tunnelConnectionName(testTunnelGrantID) + "-egress"}},
	} {
		object.SetNamespace(testTunnelNS)
		if err := kube.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("%T %s remains after revocation (%v)", object, object.GetName(), err)
		}
	}
	await("the relay without grants", func() (bool, error) {
		var grants corev1.ConfigMap
		err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayGrants}, &grants)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		var document zettaTunnelGrantsDocument
		return json.Unmarshal([]byte(grants.Data[zettaTunnelGrantsKey]), &document) == nil && len(document.Grants) == 0, nil
	})
}

func TestTunnelCRDsRefuseMalformedResources(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the Tunnel CRD envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	kube, err := client.New(config, client.Options{Scheme: newLakeScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	valid := func() *oramav1alpha1.Tunnel {
		return &oramav1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{Name: lakeTunnelName(testTunnelGrantID), Namespace: "default"},
			Spec: oramav1alpha1.TunnelSpec{
				LakeRef: "prod", GrantID: testTunnelGrantID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
				UserID: testTunnelUserID, TargetKind: "service", TargetID: testConnectionTargetID, Port: 5432,
				ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)), Generation: 1,
			},
		}
	}
	for name, mutate := range map[string]func(*oramav1alpha1.Tunnel){
		"grant id":    func(tunnel *oramav1alpha1.Tunnel) { tunnel.Spec.GrantID = "not-a-uuid" },
		"port":        func(tunnel *oramav1alpha1.Tunnel) { tunnel.Spec.Port = 70000 },
		"target kind": func(tunnel *oramav1alpha1.Tunnel) { tunnel.Spec.TargetKind = "agent" },
		"generation":  func(tunnel *oramav1alpha1.Tunnel) { tunnel.Spec.Generation = 0 },
	} {
		tunnel := valid()
		mutate(tunnel)
		if err := kube.Create(ctx, tunnel); !apierrors.IsInvalid(err) {
			t.Fatalf("%s: a malformed Tunnel was accepted (%v)", name, err)
		}
	}
	tunnel := valid()
	if err := kube.Create(ctx, tunnel); err != nil {
		t.Fatal(err)
	}
	tunnel.Spec.GrantID = testTunnelOther
	if err := kube.Update(ctx, tunnel); !apierrors.IsInvalid(err) {
		t.Fatalf("grantId changed after creation (%v)", err)
	}
	for name, spec := range map[string]oramav1alpha1.TunnelProviderSpec{
		"cloudflare without settings": {Type: "cloudflare", CustomerID: testServiceCustomer},
		"zetta with cloudflare settings": {Type: "zetta", CustomerID: testServiceCustomer, Cloudflare: &oramav1alpha1.CloudflareTunnelProviderSpec{
			AccountID: "a", ZoneID: "z", Domain: "dev.example.com", APITokenSecretRef: oramav1alpha1.TunnelProviderSecretKeyRef{Name: "s", Key: "k"},
		}},
		"unknown type": {Type: "ngrok", CustomerID: testServiceCustomer},
	} {
		provider := &oramav1alpha1.TunnelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}, Spec: spec}
		if err := kube.Create(ctx, provider); !apierrors.IsInvalid(err) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
	}
}

func TestPeerCRDsRefuseMalformedResources(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the PeerConnection CRD envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	kube, err := client.New(config, client.Options{Scheme: newLakeScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, spec := range map[string]oramav1alpha1.PeerConnectionSpec{
		"kubernetesService without serviceName": {PeerID: testPeerID, Exports: []oramav1alpha1.PeerExport{{TargetKind: "kubernetesService", Port: 1}}},
		"service without targetId":              {PeerID: testPeerID, Exports: []oramav1alpha1.PeerExport{{TargetKind: "service", Port: 1}}},
		"wildcard import":                       {PeerID: testPeerID, Imports: []oramav1alpha1.PeerImport{{Name: "*.eu", Port: 1}}},
		"peer id":                               {PeerID: "x"},
	} {
		peer := &oramav1alpha1.PeerConnection{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}, Spec: spec}
		if err := kube.Create(ctx, peer); !apierrors.IsInvalid(err) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
	}
	peer := &oramav1alpha1.PeerConnection{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}, Spec: oramav1alpha1.PeerConnectionSpec{PeerID: testPeerID}}
	if err := kube.Create(ctx, peer); err != nil {
		t.Fatal(err)
	}
	if peer.Spec.Desired != "present" || peer.Spec.Generation != 1 {
		t.Fatalf("defaults = %+v", peer.Spec)
	}
	for name, ends := range map[string][]oramav1alpha1.PeerLinkEnd{
		"one end":        {{Name: "eu", Namespace: "a", PeerConnection: "p"}},
		"same end names": {{Name: "eu", Namespace: "a", PeerConnection: "p"}, {Name: "eu", Namespace: "b", PeerConnection: "p"}},
	} {
		link := &oramav1alpha1.PeerLink{ObjectMeta: metav1.ObjectMeta{Name: "l"}, Spec: oramav1alpha1.PeerLinkSpec{PeerID: testPeerID, Ends: ends}}
		if err := kube.Create(ctx, link); !apierrors.IsInvalid(err) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
	}
}
