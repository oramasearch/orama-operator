/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
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

// observabilityFixture is a managing cluster with a prod-support hub cluster, a production cluster
// and a nonprod cluster.
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
		purposeCluster("support", prodSupportPurpose),
		purposeCluster("prod2", productionPurpose),
		purposeCluster("np", nonprodPurpose),
		&oramav1alpha1.ObservabilityProvider{ObjectMeta: metav1.ObjectMeta{Name: "pixie"},
			Spec: oramav1alpha1.ObservabilityProviderSpec{Type: oramav1alpha1.ObservabilityProviderPixie, Pixie: &release}},
		&oramav1alpha1.ObservabilityHub{ObjectMeta: metav1.ObjectMeta{Name: "prod-support"}, Spec: oramav1alpha1.ObservabilityHubSpec{
			ProviderRef: "pixie", ClusterRef: "support", Domain: "pixie.support.example", Organization: "orama", AdminEmail: "ops@support.example",
			ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "support-pull", Namespace: "orama-system"},
			Alerting: &oramav1alpha1.ObservabilityAlerting{Receivers: []oramav1alpha1.AlertReceiver{{Name: "oncall",
				Slack: &oramav1alpha1.SlackAlertReceiver{URLSecretRef: oramav1alpha1.ObservabilitySecretKeyRef{Name: "prod-slack", Namespace: "orama-system", Key: "url"}}}}},
		}},
		pullSecret("support-pull", nil),
		pullSecret("prod2-pull", nil),
		pullSecret("np-pull", nil),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-slack", Namespace: "orama-system", Labels: map[string]string{secretPurposeLabel: productionPurpose}},
			Data: map[string][]byte{"url": []byte("https://hooks.slack.example/T000/B000/xyz")}},
	}
	objects = append(objects, extra...)
	management := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.ObservabilityHub{}, &oramav1alpha1.ObservabilityAgent{}, &oramav1alpha1.ObservabilityProvider{}, &oramav1alpha1.PeerLink{}).
		WithInterceptorFuncs(applyAsUpsert).Build()
	clusters := &fakePeerClusters{clusters: map[string]client.Client{
		"support": newObservabilityCluster(t, scheme),
		"prod2":   newObservabilityCluster(t, scheme),
		"np":      newObservabilityCluster(t, scheme),
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
		if _, err := f.hubs.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod-support"}}); err != nil {
			t.Fatalf("reconcile hub: %v", err)
		}
	}
	var hub oramav1alpha1.ObservabilityHub
	if err := f.management.Get(context.Background(), types.NamespacedName{Name: "prod-support"}, &hub); err != nil {
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
		Spec: oramav1alpha1.ObservabilityAgentSpec{ClusterRef: cluster, HubRef: "prod-support",
			ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: pull, Namespace: "orama-system"}}}
}

func TestObservabilityPurposeRules(t *testing.T) {
	for purpose, refused := range map[string]bool{prodSupportPurpose: false, nonprodPurpose: false, productionPurpose: true, "": true, "staging": true} {
		if got := hubPurposeRefusal("c", purpose) != ""; got != refused {
			t.Errorf("hub on %q: refused=%v, want %v", purpose, got, refused)
		}
	}
	cases := []struct {
		agent, hub string
		allowed    bool
	}{
		{productionPurpose, prodSupportPurpose, true},
		{prodSupportPurpose, prodSupportPurpose, true},
		{nonprodPurpose, nonprodPurpose, true},
		{productionPurpose, nonprodPurpose, false},
		{nonprodPurpose, prodSupportPurpose, false},
		{"", nonprodPurpose, false},
	}
	for _, c := range cases {
		if got := agentPurposeRefusal("a", c.agent, "h", c.hub) == ""; got != c.allowed {
			t.Errorf("agent %q to hub %q: allowed=%v, want %v", c.agent, c.hub, got, c.allowed)
		}
	}
	// Production credentials reach prod-support, never nonprod.
	production := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{secretPurposeLabel: productionPurpose}}}
	ref := oramav1alpha1.SetSecret{Namespace: "orama-system", Name: "s"}
	if err := (secretScope{purpose: prodSupportPurpose, operator: true}).refuse(ref, "s", production); err != nil {
		t.Errorf("a production Secret is refused for prod-support: %v", err)
	}
	if err := (secretScope{purpose: nonprodPurpose, operator: true}).refuse(ref, "s", production); err == nil {
		t.Error("a production Secret may be copied to nonprod")
	}
}

// A production agent reports to the prod-support hub through its own link and key; a nonprod agent
// naming the same hub is refused and gets nothing.
func TestObservabilityAgentsReportOnlyToTheirPurposesHub(t *testing.T) {
	ctx := context.Background()
	f := newObservabilityFixture(t, agentNamed("prod2", "prod2", "prod2-pull"), agentNamed("np", "np", "np-pull"))
	hub := f.reconcileHub(t)
	if hub.Status.Purpose != prodSupportPurpose || hub.Status.ConcreteRef == "" {
		t.Fatalf("hub status %+v", hub.Status)
	}
	support := f.cluster("support")
	var cloud oramav1alpha1.PixieCloud
	if err := support.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieConcreteName}, &cloud); err != nil {
		t.Fatal(err)
	}
	if len(cloud.Spec.Agents) != 1 || cloud.Spec.Agents[0].Name != "prod2" {
		t.Fatalf("the hub issues keys for %+v; only the production agent may report to a prod-support hub", cloud.Spec.Agents)
	}
	if cloud.Spec.Release.Commit != pixieManifestCommit || cloud.Spec.Purpose != prodSupportPurpose {
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
	if err := support.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pixieDeployKeyPrefix + "prod2", Namespace: "plc"},
		Data: map[string][]byte{"deploy-key": []byte(key)}}); err != nil {
		t.Fatal(err)
	}
	cloud.Status.CACertificate = "-----BEGIN CERTIFICATE-----\nhub\n-----END CERTIFICATE-----\n"
	cloud.Status.Agents = []oramav1alpha1.PixieCloudAgentStatus{{Name: "prod2", Registered: true, Connected: true}}
	if err := support.Status().Update(ctx, &cloud); err != nil {
		t.Fatal(err)
	}
	agent = f.reconcileAgent(t, "prod2")
	if agent.Status.Link != "PeerLink pixie-prod2" || agent.Status.Purpose != productionPurpose || agent.Status.HubPurpose != prodSupportPurpose {
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
	if err := support.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieAgentPeer("prod2")}, &hubEnd); err != nil {
		t.Fatal(err)
	}
	if err := prod2.Get(ctx, client.ObjectKey{Namespace: "pl", Name: pixieHubPeer}, &agentEnd); err != nil {
		t.Fatal(err)
	}
	if len(hubEnd.Spec.Exports) != 1 || hubEnd.Spec.Exports[0].ServiceName != pixieVzconnService || len(hubEnd.Spec.Imports) != 0 ||
		len(agentEnd.Spec.Imports) != 1 || len(agentEnd.Spec.Exports) != 0 || hubEnd.Spec.PeerID != agentEnd.Spec.PeerID {
		t.Fatalf("link ends: hub %+v agent %+v", hubEnd.Spec, agentEnd.Spec)
	}
	var link oramav1alpha1.PeerLink
	if err := f.management.Get(ctx, types.NamespacedName{Name: "pixie-prod2"}, &link); err != nil {
		t.Fatal(err)
	}
	if link.Spec.Ends[0].ClusterRef != "support" || link.Spec.Ends[1].ClusterRef != "prod2" {
		t.Fatalf("PeerLink ends %+v", link.Spec.Ends)
	}

	// A steady agent writes nothing.
	before := agent.ResourceVersion
	agent = f.reconcileAgent(t, "prod2")
	if agent.ResourceVersion != before {
		t.Errorf("a steady agent rewrote its status")
	}

	// The nonprod agent is refused and nothing reaches its cluster.
	refused := f.reconcileAgent(t, "np")
	if refused.Status.Phase != oramav1alpha1.ObservabilityPhaseRefused || !strings.Contains(refused.Status.Message, "nonprod hub") {
		t.Fatalf("nonprod agent status %+v", refused.Status)
	}
	np := f.cluster("np")
	for _, object := range []client.Object{
		&oramav1alpha1.PixieVizier{ObjectMeta: metav1.ObjectMeta{Name: pixieConcreteName, Namespace: "pl"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: vizierDeploySecret, Namespace: "pl"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alert-oncall", Namespace: "pl"}},
	} {
		if err := np.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s reached the nonprod cluster", object, object.GetName())
		}
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
	if err := support.Get(ctx, client.ObjectKeyFromObject(&hubEnd), &hubEnd); !apierrors.IsNotFound(err) {
		t.Error("the hub's end of the link outlived the agent")
	}
	if err := f.management.Get(ctx, types.NamespacedName{Name: "pixie-prod2"}, &link); !apierrors.IsNotFound(err) {
		t.Error("the PeerLink outlived the agent")
	}
	f.reconcileHub(t)
	if err := support.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieConcreteName}, &cloud); err != nil {
		t.Fatal(err)
	}
	if len(cloud.Spec.Agents) != 0 {
		t.Errorf("the hub still lists %+v", cloud.Spec.Agents)
	}
}

// A hub is never placed on a production cluster.
func TestObservabilityHubRefusedOnProduction(t *testing.T) {
	f := newObservabilityFixture(t)
	var hub oramav1alpha1.ObservabilityHub
	if err := f.management.Get(context.Background(), types.NamespacedName{Name: "prod-support"}, &hub); err != nil {
		t.Fatal(err)
	}
	hub.Spec.ClusterRef = "prod2"
	if err := f.management.Update(context.Background(), &hub); err != nil {
		t.Fatal(err)
	}
	got := f.reconcileHub(t)
	if got.Status.Phase != oramav1alpha1.ObservabilityPhaseRefused || !strings.Contains(got.Status.Message, "prod-support") {
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
