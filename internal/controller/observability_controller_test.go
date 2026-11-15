/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func newObservabilityCluster(t *testing.T, scheme *runtime.Scheme, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.PixieCloud{}, &oramav1alpha1.PixieVizier{}, &oramav1alpha1.PeerConnection{}).
		WithInterceptorFuncs(applyAsUpsert).Build()
}

func purposeCluster(name, purpose string) *oramav1alpha1.KubernetesCluster {
	return &oramav1alpha1.KubernetesCluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: oramav1alpha1.KubernetesClusterSpec{Tags: map[string]string{clusterPurposeTag: purpose}}}
}

func pullSecret(name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orama-system", Labels: labels},
		Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
}

// observabilityFixture is a managing cluster that runs production's hub, a production cluster and a
// nonprod cluster.
type observabilityFixture struct {
	scheme     *runtime.Scheme
	management client.Client
	clusters   *fakePeerClusters
	hubs       *ObservabilityHubReconciler
	agents     *ObservabilityAgentReconciler
}

func newObservabilityFixture(t *testing.T, extra ...client.Object) *observabilityFixture {
	t.Helper()
	scheme := newLakeScheme(t)
	release := testPixieRelease()
	objects := []client.Object{
		purposeCluster("prod2", productionPurpose),
		purposeCluster("np", nonprodPurpose),
		&oramav1alpha1.ObservabilityProvider{ObjectMeta: metav1.ObjectMeta{Name: "pixie"},
			Spec: oramav1alpha1.ObservabilityProviderSpec{Type: oramav1alpha1.ObservabilityProviderPixie, Pixie: &release}},
		&oramav1alpha1.ObservabilityHub{ObjectMeta: metav1.ObjectMeta{Name: "production"}, Spec: oramav1alpha1.ObservabilityHubSpec{
			ProviderRef: "pixie", Domain: "pixie.ops.example", Organization: "orama", AdminEmail: "ops@ops.example",
			ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "hub-pull", Namespace: "orama-system"},
			Alerting: &oramav1alpha1.ObservabilityAlerting{Receivers: []oramav1alpha1.AlertReceiver{{Name: "oncall",
				Slack: &oramav1alpha1.SlackAlertReceiver{URLSecretRef: oramav1alpha1.ObservabilitySecretKeyRef{Name: "prod-slack", Namespace: "orama-system", Key: "url"}}}}},
		}},
		pullSecret("hub-pull", nil),
		pullSecret("prod2-pull", nil),
		pullSecret("np-pull", nil),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "np-slack", Namespace: "orama-system", Labels: map[string]string{secretPurposeLabel: nonprodPurpose}},
			Data: map[string][]byte{"url": []byte("https://hooks.slack.example/T000/B000/np")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-slack", Namespace: "orama-system", Labels: map[string]string{secretPurposeLabel: productionPurpose}},
			Data: map[string][]byte{"url": []byte("https://hooks.slack.example/T000/B000/xyz")}},
	}
	objects = append(objects, extra...)
	management := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.ObservabilityHub{}, &oramav1alpha1.ObservabilityAgent{}, &oramav1alpha1.ObservabilityProvider{}, &oramav1alpha1.PeerLink{},
			&oramav1alpha1.PixieCloud{}, &oramav1alpha1.PeerConnection{}).
		WithInterceptorFuncs(applyAsUpsert).Build()
	clusters := &fakePeerClusters{clusters: map[string]client.Client{
		"":      management, // the hub's cluster: the managing cluster itself
		"prod2": newObservabilityCluster(t, scheme),
		"np":    newObservabilityCluster(t, scheme),
	}}
	return &observabilityFixture{
		scheme:     scheme,
		management: management,
		clusters:   clusters,
		hubs:       &ObservabilityHubReconciler{Client: management, Scheme: scheme, Clusters: clusters},
		agents:     &ObservabilityAgentReconciler{Client: management, Scheme: scheme, Clusters: clusters},
	}
}

func (f *observabilityFixture) reconcileHub(t *testing.T) *oramav1alpha1.ObservabilityHub {
	t.Helper()
	for i := 0; i < 2; i++ { // the first pass adds the finalizer
		if _, err := f.hubs.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "production"}}); err != nil {
			t.Fatalf("reconcile hub: %v", err)
		}
	}
	var hub oramav1alpha1.ObservabilityHub
	if err := f.management.Get(context.Background(), types.NamespacedName{Name: "production"}, &hub); err != nil {
		t.Fatal(err)
	}
	return &hub
}

func (f *observabilityFixture) reconcileAgent(t *testing.T, name string) *oramav1alpha1.ObservabilityAgent {
	t.Helper()
	for i := 0; i < 2; i++ {
		if _, err := f.agents.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
			t.Fatalf("reconcile agent %s: %v", name, err)
		}
	}
	var agent oramav1alpha1.ObservabilityAgent
	if err := f.management.Get(context.Background(), types.NamespacedName{Name: name}, &agent); err != nil {
		t.Fatal(err)
	}
	return &agent
}

func (f *observabilityFixture) cluster(name string) client.Client { return f.clusters.clusters[name] }

func agentNamed(name, cluster, pull string) *oramav1alpha1.ObservabilityAgent {
	return &oramav1alpha1.ObservabilityAgent{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("0b3f2f6e-1c7a-4c55-9d1e-" + strings.Repeat("a", 12))},
		Spec: oramav1alpha1.ObservabilityAgentSpec{ClusterRef: cluster, HubRef: "production",
			ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: pull, Namespace: "orama-system"}}}
}

func TestObservabilityPurposeRules(t *testing.T) {
	hubs := []struct {
		cluster, purpose string
		refused          bool
	}{
		{"", productionPurpose, false}, // the managing cluster
		{"np", nonprodPurpose, false},
		{"prod2", productionPurpose, true},
		{"c", "", true},
		{"c", "staging", true},
		{"c", "prod-support", true},
	}
	for _, h := range hubs {
		if got := hubPurposeRefusal(h.cluster, h.purpose) != ""; got != h.refused {
			t.Errorf("hub on %q (%q): refused=%v, want %v", h.cluster, h.purpose, got, h.refused)
		}
	}
	cases := []struct {
		agent, hub string
		allowed    bool
	}{
		{productionPurpose, productionPurpose, true},
		{nonprodPurpose, nonprodPurpose, true},
		{productionPurpose, nonprodPurpose, false},
		{nonprodPurpose, productionPurpose, false}, // a production hub on a cluster: there is none, but never
		{"prod-support", productionPurpose, false},
		{"", nonprodPurpose, false},
	}
	for _, c := range cases {
		if got := agentPurposeRefusal("a", c.agent, "h", c.hub) == ""; got != c.allowed {
			t.Errorf("agent %q to hub %q: allowed=%v, want %v", c.agent, c.hub, got, c.allowed)
		}
	}
	// The managing cluster's hub (no clusterRef) takes nonprod and production agents.
	for _, purpose := range []string{productionPurpose, nonprodPurpose} {
		if refusal := agentPurposeRefusal("a", purpose, "", productionPurpose); refusal != "" {
			t.Errorf("a %s agent to the managing cluster's hub: %s", purpose, refusal)
		}
	}
	// The managing cluster is production: its hub may hold production credentials. Nonprod never may.
	ctx := context.Background()
	reader := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(purposeCluster("np", nonprodPurpose)).Build()
	production := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{secretPurposeLabel: productionPurpose}}}
	ref := oramav1alpha1.SetSecret{Namespace: "orama-system", Name: "s"}
	for cluster, refused := range map[string]bool{"": false, "np": true} {
		scope, err := operatorSecretScope(ctx, reader, cluster)
		if err != nil {
			t.Fatal(err)
		}
		if got := scope.refuse(ref, "s", production) != nil; got != refused {
			t.Errorf("a production Secret copied to %q: refused=%v, want %v", firstNonEmpty(cluster, managingClusterName), got, refused)
		}
	}
}

// A production agent reports to the hub on the managing cluster through its own link and key. A
// nonprod agent may report there too (Steve, 2026-10-31), but production's receivers never reach
// its cluster: it pages only through receivers of its own.
func TestObservabilityAgentsReportToTheManagingHub(t *testing.T) {
	ctx := context.Background()
	f := newObservabilityFixture(t, agentNamed("prod2", "prod2", "prod2-pull"), agentNamed("np", "np", "np-pull"))
	hub := f.reconcileHub(t)
	if hub.Status.Purpose != productionPurpose || hub.Status.ConcreteRef == "" {
		t.Fatalf("hub status %+v", hub.Status)
	}
	managing := f.cluster("")
	var cloud oramav1alpha1.PixieCloud
	if err := managing.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieConcreteName}, &cloud); err != nil {
		t.Fatal(err)
	}
	if len(cloud.Spec.Agents) != 2 || cloud.Spec.Agents[0].Name != "np" || cloud.Spec.Agents[1].Name != "prod2" {
		t.Fatalf("the hub issues keys for %+v; want the nonprod and the production agent", cloud.Spec.Agents)
	}
	if cloud.Spec.Release.Commit != pixieManifestCommit || cloud.Spec.Purpose != productionPurpose {
		t.Fatalf("PixieCloud spec %+v", cloud.Spec)
	}

	// Before the hub issues the key, the agent waits.
	agent := f.reconcileAgent(t, "prod2")
	if agent.Status.Phase != oramav1alpha1.ObservabilityPhasePending || !strings.Contains(agent.Status.Message, "deploy key") {
		t.Fatalf("agent before its key: %+v", agent.Status)
	}
	// The cluster's alert routing is already there: paging does not wait for Pixie.
	var early oramav1alpha1.PixieVizier
	if err := f.cluster("prod2").Get(ctx, client.ObjectKey{Namespace: "pl", Name: pixieConcreteName}, &early); err != nil {
		t.Fatalf("no PixieVizier before the key: %v", err)
	}
	if len(early.Spec.AlertRoutes) != 1 || early.Spec.AlertRoutes[0].SecretName != "alert-oncall" {
		t.Fatalf("alert routes before the key: %+v", early.Spec.AlertRoutes)
	}
	var receiver corev1.Secret
	if err := f.cluster("prod2").Get(ctx, client.ObjectKey{Namespace: "pl", Name: "alert-oncall"}, &receiver); err != nil || len(receiver.Data[alertRouteSecretKey]) == 0 {
		t.Fatalf("the production receiver did not reach the production cluster: %v", err)
	}
	// The hub's operator issues the key and publishes its CA.
	key := "px-dep-11111111-2222-3333-4444-555555555555"
	if err := managing.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pixieDeployKeyPrefix + "prod2", Namespace: "plc"},
		Data: map[string][]byte{"deploy-key": []byte(key)}}); err != nil {
		t.Fatal(err)
	}
	cloud.Status.CACertificate = "-----BEGIN CERTIFICATE-----\nhub\n-----END CERTIFICATE-----\n"
	cloud.Status.Agents = []oramav1alpha1.PixieCloudAgentStatus{{Name: "prod2", Registered: true, Connected: true}}
	if err := managing.Status().Update(ctx, &cloud); err != nil {
		t.Fatal(err)
	}
	agent = f.reconcileAgent(t, "prod2")
	if agent.Status.Link != "PeerLink pixie-prod2" || agent.Status.Purpose != productionPurpose || agent.Status.HubPurpose != productionPurpose {
		t.Fatalf("agent status %+v", agent.Status)
	}
	prod2 := f.cluster("prod2")
	var vizier oramav1alpha1.PixieVizier
	if err := prod2.Get(ctx, client.ObjectKey{Namespace: "pl", Name: pixieConcreteName}, &vizier); err != nil {
		t.Fatal(err)
	}
	if vizier.Spec.CloudAddress != "peer-pixie-cloud.pl.svc:51600" || vizier.Spec.ClusterName != "prod2" || len(vizier.Spec.AlertRoutes) != 1 {
		t.Fatalf("PixieVizier spec %+v", vizier.Spec)
	}
	var delivered corev1.Secret
	if err := prod2.Get(ctx, client.ObjectKey{Namespace: "pl", Name: vizierDeploySecret}, &delivered); err != nil || string(delivered.Data["deploy-key"]) != key {
		t.Fatalf("the cluster's own key was not delivered: %v", err)
	}
	// The link: the hub's end exports only vzconn, the agent's end imports only it, and the
	// managing cluster's PeerLink pairs exactly those two.
	var hubEnd, agentEnd oramav1alpha1.PeerConnection
	if err := managing.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieAgentPeer("prod2")}, &hubEnd); err != nil {
		t.Fatal(err)
	}
	if err := prod2.Get(ctx, client.ObjectKey{Namespace: "pl", Name: pixieHubPeer}, &agentEnd); err != nil {
		t.Fatal(err)
	}
	// One link, two directions, each exactly one service: the hub exports only vzconn and imports
	// only the agent's metrics; the agent exports only its read-only federation proxy.
	if len(hubEnd.Spec.Exports) != 1 || hubEnd.Spec.Exports[0].ServiceName != pixieVzconnService ||
		len(hubEnd.Spec.Imports) != 1 || hubEnd.Spec.Imports[0].Name != agentMetricsImport("prod2") ||
		len(agentEnd.Spec.Imports) != 1 || agentEnd.Spec.Imports[0].Name != pixieAgentImport ||
		len(agentEnd.Spec.Exports) != 1 || agentEnd.Spec.Exports[0].ServiceName != metricsFederationName ||
		agentEnd.Spec.Exports[0].Name != hubEnd.Spec.Imports[0].Name || hubEnd.Spec.PeerID != agentEnd.Spec.PeerID {
		t.Fatalf("link ends: hub %+v agent %+v", hubEnd.Spec, agentEnd.Spec)
	}
	// The hub Prometheus scrapes exactly the Service the hub end's import creates.
	if err := managing.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieConcreteName}, &cloud); err != nil {
		t.Fatal(err)
	}
	if cloud.Spec.Cluster != "" {
		t.Fatalf("PixieCloud cluster %q, want none: the hub runs on the managing cluster", cloud.Spec.Cluster)
	}
	wantTarget := peerImportServiceName(hubEnd.Spec.Imports[0].Name) + ".plc.svc.cluster.local:9090"
	found := false
	for _, federated := range hubFederatedClusters(&cloud) {
		found = found || (federated.Name == "prod2" && federated.Target == wantTarget)
	}
	if !found {
		t.Fatalf("the hub does not federate prod2 at %s: %+v", wantTarget, hubFederatedClusters(&cloud))
	}
	federatesNonprod := false
	for _, federated := range hubFederatedClusters(&cloud) {
		federatesNonprod = federatesNonprod || federated.Name == "np"
	}
	if !federatesNonprod {
		t.Fatal("the managing cluster's hub does not federate the nonprod cluster's metrics")
	}
	var link oramav1alpha1.PeerLink
	if err := f.management.Get(ctx, types.NamespacedName{Name: "pixie-prod2"}, &link); err != nil {
		t.Fatal(err)
	}
	if link.Spec.Ends[0].ClusterRef != "" || link.Spec.Ends[1].ClusterRef != "prod2" {
		t.Fatalf("PeerLink ends %+v", link.Spec.Ends)
	}

	// A steady agent writes nothing.
	before := agent.ResourceVersion
	agent = f.reconcileAgent(t, "prod2")
	if agent.ResourceVersion != before {
		t.Errorf("a steady agent rewrote its status")
	}

	// The nonprod agent is accepted, and production's receiver never reaches its cluster.
	nonprod := f.reconcileAgent(t, "np")
	if nonprod.Status.Phase == oramav1alpha1.ObservabilityPhaseRefused || nonprod.Status.Purpose != nonprodPurpose || nonprod.Status.HubPurpose != productionPurpose {
		t.Fatalf("nonprod agent status %+v", nonprod.Status)
	}
	np := f.cluster("np")
	var npVizier oramav1alpha1.PixieVizier
	if err := np.Get(ctx, client.ObjectKey{Namespace: "pl", Name: pixieConcreteName}, &npVizier); err != nil {
		t.Fatalf("no PixieVizier on the nonprod cluster: %v", err)
	}
	if len(npVizier.Spec.AlertRoutes) != 0 {
		t.Fatalf("a nonprod agent naming no receivers got routes %+v", npVizier.Spec.AlertRoutes)
	}
	if err := np.Get(ctx, client.ObjectKey{Namespace: "pl", Name: "alert-oncall"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Error("production's receiver reached the nonprod cluster")
	}
	// Naming its own receiver, it pages there.
	nonprod.Spec.Alerting = &oramav1alpha1.ObservabilityAlerting{Receivers: []oramav1alpha1.AlertReceiver{{Name: "staging",
		Slack: &oramav1alpha1.SlackAlertReceiver{URLSecretRef: oramav1alpha1.ObservabilitySecretKeyRef{Name: "np-slack", Namespace: "orama-system", Key: "url"}}}}}
	if err := f.management.Update(ctx, nonprod); err != nil {
		t.Fatal(err)
	}
	f.reconcileAgent(t, "np")
	if err := np.Get(ctx, client.ObjectKey{Namespace: "pl", Name: pixieConcreteName}, &npVizier); err != nil {
		t.Fatal(err)
	}
	if len(npVizier.Spec.AlertRoutes) != 1 || npVizier.Spec.AlertRoutes[0].SecretName != "alert-staging" {
		t.Fatalf("the nonprod agent's own receiver: routes %+v", npVizier.Spec.AlertRoutes)
	}

	// Deleting the production agent takes everything back, and the hub stops issuing its key.
	if err := f.management.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.agents.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod2"}}); err != nil {
		t.Fatal(err)
	}
	if err := prod2.Get(ctx, client.ObjectKeyFromObject(&vizier), &vizier); !apierrors.IsNotFound(err) {
		t.Error("the PixieVizier outlived its agent")
	}
	if err := managing.Get(ctx, client.ObjectKeyFromObject(&hubEnd), &hubEnd); !apierrors.IsNotFound(err) {
		t.Error("the hub's end of the link outlived the agent")
	}
	if err := f.management.Get(ctx, types.NamespacedName{Name: "pixie-prod2"}, &link); !apierrors.IsNotFound(err) {
		t.Error("the PeerLink outlived the agent")
	}
	var leftover corev1.SecretList
	if err := prod2.List(ctx, &leftover, client.InNamespace("pl"), client.MatchingLabels{observabilityAgentLbl: "prod2"}); err != nil || len(leftover.Items) != 0 {
		t.Errorf("credentials copied for the agent remain on its cluster: %d", len(leftover.Items))
	}
	f.reconcileHub(t)
	if err := managing.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieConcreteName}, &cloud); err != nil {
		t.Fatal(err)
	}
	if len(cloud.Spec.Agents) != 1 || cloud.Spec.Agents[0].Name != "np" {
		t.Errorf("the hub lists %+v; want only the nonprod agent", cloud.Spec.Agents)
	}
}

// A hub is never placed on a production cluster.
// A production agent writes its alert routing before Pixie's release exists. The hub has made
// nothing on the managing cluster yet, and the API server refuses an object in a missing namespace.
func TestObservabilityAgentPagesBeforeTheHubRuns(t *testing.T) {
	f := newObservabilityFixture(t, agentNamed("prod2", "prod2", "prod2-pull"))
	ctx := context.Background()
	if err := f.management.Delete(ctx, &oramav1alpha1.ObservabilityProvider{ObjectMeta: metav1.ObjectMeta{Name: "pixie"}}); err != nil {
		t.Fatal(err)
	}
	refuseMissingNamespace := func(ctx context.Context, c client.WithWatch, obj client.Object) error {
		if ns := obj.GetNamespace(); ns != "" {
			if err := c.Get(ctx, client.ObjectKey{Name: ns}, &corev1.Namespace{}); err != nil {
				return err
			}
		}
		return nil
	}
	f.clusters.clusters[""] = interceptor.NewClient(f.management.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := refuseMissingNamespace(ctx, c, obj); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := refuseMissingNamespace(ctx, c, obj); err != nil {
				return err
			}
			return applyAsUpsert.Patch(ctx, c, obj, patch, opts...)
		},
	})
	agent := f.reconcileAgent(t, "prod2")
	var vizier oramav1alpha1.PixieVizier
	if err := f.cluster("prod2").Get(ctx, client.ObjectKey{Namespace: oramav1alpha1.PixieVizierNamespace, Name: pixieConcreteName}, &vizier); err != nil {
		t.Fatalf("no PixieVizier before the release (%s): %v", agent.Status.Message, err)
	}
	if len(vizier.Spec.AlertRoutes) != 1 {
		t.Fatalf("alert routes before the release: %+v", vizier.Spec.AlertRoutes)
	}
}

func TestObservabilityHubRefusedOnProduction(t *testing.T) {
	f := newObservabilityFixture(t)
	var hub oramav1alpha1.ObservabilityHub
	if err := f.management.Get(context.Background(), types.NamespacedName{Name: "production"}, &hub); err != nil {
		t.Fatal(err)
	}
	hub.Spec.ClusterRef = "prod2"
	if err := f.management.Update(context.Background(), &hub); err != nil {
		t.Fatal(err)
	}
	got := f.reconcileHub(t)
	if got.Status.Phase != oramav1alpha1.ObservabilityPhaseRefused || !strings.Contains(got.Status.Message, "managing cluster") {
		t.Fatalf("hub on production: %+v", got.Status)
	}
	var cloud oramav1alpha1.PixieCloud
	if err := f.cluster("prod2").Get(context.Background(), client.ObjectKey{Namespace: "plc", Name: pixieConcreteName}, &cloud); !apierrors.IsNotFound(err) {
		t.Error("a hub was written onto a production cluster")
	}
}

// A nonprod agent cannot borrow a production agent's pull Secret: each cluster has its own.
func TestObservabilityProductionCredentialsNeverReachNonprod(t *testing.T) {
	f := newObservabilityFixture(t,
		agentNamed("prod2", "prod2", "prod2-pull"),
		agentNamed("np", "np", "prod2-pull"),
		&oramav1alpha1.ObservabilityHub{ObjectMeta: metav1.ObjectMeta{Name: "nonprod"}, Spec: oramav1alpha1.ObservabilityHubSpec{
			ProviderRef: "pixie", ClusterRef: "np", Domain: "pixie.np.example", Organization: "orama", AdminEmail: "ops@np.example",
			ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "np-pull", Namespace: "orama-system"}}},
	)
	var np oramav1alpha1.ObservabilityAgent
	if err := f.management.Get(context.Background(), types.NamespacedName{Name: "np"}, &np); err != nil {
		t.Fatal(err)
	}
	np.Spec.HubRef = "nonprod"
	if err := f.management.Update(context.Background(), &np); err != nil {
		t.Fatal(err)
	}
	if err := f.cluster("np").Create(context.Background(), &oramav1alpha1.PixieCloud{ObjectMeta: metav1.ObjectMeta{Name: pixieConcreteName, Namespace: "plc"}}); err != nil {
		t.Fatal(err)
	}
	got := f.reconcileAgent(t, "np")
	if got.Status.Phase != oramav1alpha1.ObservabilityPhaseRefused || !strings.Contains(got.Status.Message, "production credential") {
		t.Fatalf("nonprod agent with a production pull Secret: %+v", got.Status)
	}
	var copied corev1.Secret
	if err := f.cluster("np").Get(context.Background(), client.ObjectKey{Namespace: "pl", Name: pixiePullSecret}, &copied); !apierrors.IsNotFound(err) {
		t.Error("the production pull Secret reached the nonprod cluster")
	}
}

// The managing cluster's hub holds production's pull Secret and alert receivers. Listing them never
// looks up a KubernetesCluster with no name, which the API server refuses, and a nonprod cluster
// never gets them.
func TestObservabilityManagingHubSecretsAreProduction(t *testing.T) {
	ctx := context.Background()
	hub := &oramav1alpha1.ObservabilityHub{ObjectMeta: metav1.ObjectMeta{Name: "production"}, Spec: oramav1alpha1.ObservabilityHubSpec{
		ProviderRef: "pixie", Domain: "pixie.ops.example", Organization: "orama", AdminEmail: "ops@ops.example",
		ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "hub-pull", Namespace: "orama-system"},
		Alerting: &oramav1alpha1.ObservabilityAlerting{Receivers: []oramav1alpha1.AlertReceiver{{Name: "oncall",
			Slack: &oramav1alpha1.SlackAlertReceiver{URLSecretRef: oramav1alpha1.ObservabilitySecretKeyRef{Name: "prod-slack", Namespace: "orama-system", Key: "url"}}}}},
	}}
	reader := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(purposeCluster("np", nonprodPurpose), hub).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "" {
				return errors.New("resource name may not be empty")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	sources, err := productionSecretSources(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"orama-system/hub-pull", "orama-system/prod-slack"} {
		if _, found := sources[key]; !found {
			t.Errorf("%s is not listed as a production credential: %v", key, sources)
		}
	}
	scope, err := operatorSecretScope(ctx, reader, "np")
	if err != nil {
		t.Fatal(err)
	}
	if scope.refuse(oramav1alpha1.SetSecret{Namespace: "orama-system", Name: "hub-pull"}, pixiePullSecret, pullSecret("hub-pull", nil)) == nil {
		t.Error("the managing cluster's hub pull Secret may be copied to a nonprod cluster")
	}
}

// An agent with no clusterRef runs on the managing cluster, beside production's hub: it is
// production, reaches the hub without a link, and is registered and federated under the hub's own
// name, so the hub does not read its own cluster twice.
func TestObservabilityAgentOnTheManagingCluster(t *testing.T) {
	ctx := context.Background()
	f := newObservabilityFixture(t, agentNamed("managing", "", "hub-pull"))
	f.reconcileHub(t)
	managing := f.cluster("")
	var cloud oramav1alpha1.PixieCloud
	if err := managing.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieConcreteName}, &cloud); err != nil {
		t.Fatal(err)
	}
	if len(cloud.Spec.Agents) != 1 || cloud.Spec.Agents[0] != (oramav1alpha1.PixieCloudAgent{Name: "managing", Cluster: "production"}) {
		t.Fatalf("the hub lists %+v; want the managing cluster's agent under the hub's name", cloud.Spec.Agents)
	}
	if federated := hubFederatedClusters(&cloud); len(federated) != 1 || federated[0].Name != "production" || federated[0].Target != platformPrometheusTarget {
		t.Fatalf("the hub federates %+v; want only its own cluster, read directly", federated)
	}
	if err := managing.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pixieDeployKeyPrefix + "managing", Namespace: "plc"},
		Data: map[string][]byte{"deploy-key": []byte("px-dep-11111111-2222-3333-4444-555555555555")}}); err != nil {
		t.Fatal(err)
	}
	cloud.Status.CACertificate = "-----BEGIN CERTIFICATE-----\nhub\n-----END CERTIFICATE-----\n"
	cloud.Status.Agents = []oramav1alpha1.PixieCloudAgentStatus{{Name: "managing", Registered: true, Connected: true}}
	if err := managing.Status().Update(ctx, &cloud); err != nil {
		t.Fatal(err)
	}
	agent := f.reconcileAgent(t, "managing")
	if agent.Status.Link != "local" || agent.Status.Purpose != productionPurpose || agent.Status.HubPurpose != productionPurpose {
		t.Fatalf("agent status %+v", agent.Status)
	}
	var vizier oramav1alpha1.PixieVizier
	if err := managing.Get(ctx, client.ObjectKey{Namespace: "pl", Name: pixieConcreteName}, &vizier); err != nil {
		t.Fatal(err)
	}
	if vizier.Spec.ClusterName != "production" || vizier.Spec.CloudAddress != fmt.Sprintf("%s.plc.svc:%d", pixieVzconnService, pixieVzconnPort) {
		t.Fatalf("PixieVizier spec %+v", vizier.Spec)
	}
	var links oramav1alpha1.PeerLinkList
	if err := f.management.List(ctx, &links); err != nil || len(links.Items) != 0 {
		t.Fatalf("an agent beside its hub got a PeerLink: %d (%v)", len(links.Items), err)
	}
}
