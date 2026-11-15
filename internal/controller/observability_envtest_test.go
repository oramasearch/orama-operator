/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The observability CRDs refuse what the controllers would otherwise have to refuse (operator#15).
func TestObservabilityCRDsRefuseMalformedResources(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the observability CRD envtest")
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

	release := testPixieRelease()
	tagged := testPixieRelease()
	tagged.Images["kratos"] = "oryd/kratos:v0.10.1"
	for name, spec := range map[string]oramav1alpha1.ObservabilityProviderSpec{
		"pixie without a release": {Type: oramav1alpha1.ObservabilityProviderPixie},
		"an image by tag":         {Type: oramav1alpha1.ObservabilityProviderPixie, Pixie: &tagged},
		"another vendor":          {Type: "newrelic", Pixie: &release},
	} {
		provider := &oramav1alpha1.ObservabilityProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: spec}
		if err := kube.Create(ctx, provider); !apierrors.IsInvalid(err) {
			t.Errorf("provider with %s: accepted (%v)", name, err)
		}
	}
	provider := &oramav1alpha1.ObservabilityProvider{ObjectMeta: metav1.ObjectMeta{Name: "pixie"},
		Spec: oramav1alpha1.ObservabilityProviderSpec{Type: oramav1alpha1.ObservabilityProviderPixie, Pixie: &release}}
	if err := kube.Create(ctx, provider); err != nil {
		t.Fatal(err)
	}

	hub := func(receiver oramav1alpha1.AlertReceiver) *oramav1alpha1.ObservabilityHub {
		return &oramav1alpha1.ObservabilityHub{ObjectMeta: metav1.ObjectMeta{Name: "hub"}, Spec: oramav1alpha1.ObservabilityHubSpec{
			ProviderRef: "pixie", ClusterRef: "support", Domain: "pixie.support.example", Organization: "orama", AdminEmail: "ops@support.example",
			ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "pull", Namespace: "orama-system"},
			Alerting:           &oramav1alpha1.ObservabilityAlerting{Receivers: []oramav1alpha1.AlertReceiver{receiver}},
		}}
	}
	url := oramav1alpha1.ObservabilitySecretKeyRef{Name: "s", Namespace: "orama-system", Key: "url"}
	both := oramav1alpha1.AlertReceiver{Name: "both", Slack: &oramav1alpha1.SlackAlertReceiver{URLSecretRef: url}, Webhook: &oramav1alpha1.WebhookAlertReceiver{URLSecretRef: url}}
	if err := kube.Create(ctx, hub(both)); !apierrors.IsInvalid(err) {
		t.Errorf("a receiver that is both Slack and a webhook was accepted (%v)", err)
	}
	valid := hub(oramav1alpha1.AlertReceiver{Name: "oncall", Slack: &oramav1alpha1.SlackAlertReceiver{URLSecretRef: url}})
	if err := kube.Create(ctx, valid); err != nil {
		t.Fatal(err)
	}
	if valid.Spec.Storage.Postgres == nil || valid.Spec.Storage.Postgres.String() != "20Gi" || valid.Spec.Storage.Search.String() != "20Gi" {
		t.Errorf("storage defaults %+v", valid.Spec.Storage)
	}
	valid.Spec.ClusterRef = "prod2"
	if err := kube.Update(ctx, valid); !apierrors.IsInvalid(err) {
		t.Errorf("a hub moved to another cluster (%v)", err)
	}

	agent := &oramav1alpha1.ObservabilityAgent{ObjectMeta: metav1.ObjectMeta{Name: "prod2"}, Spec: oramav1alpha1.ObservabilityAgentSpec{
		ClusterRef: "prod2", HubRef: "hub", ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "pull", Namespace: "orama-system"}}}
	if err := kube.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.NodeMemory == nil || agent.Spec.NodeMemory.String() != "2Gi" {
		t.Errorf("nodeMemory default %v, want 2Gi", agent.Spec.NodeMemory)
	}

	// The concrete kinds take only the purposes the managing cluster may write.
	cloud := testPixieCloud()
	cloud.Namespace = "default"
	cloud.Spec.Purpose = "prod-support"
	if err := kube.Create(ctx, cloud); !apierrors.IsInvalid(err) {
		t.Errorf("a PixieCloud for a prod-support cluster was accepted (%v)", err)
	}

	// Production's hub names no cluster: it runs on the managing cluster, and stays there.
	managing := hub(oramav1alpha1.AlertReceiver{Name: "oncall", Slack: &oramav1alpha1.SlackAlertReceiver{URLSecretRef: url}})
	managing.Name, managing.Spec.ClusterRef = "production", ""
	if err := kube.Create(ctx, managing); err != nil {
		t.Fatalf("a hub on the managing cluster was refused: %v", err)
	}
	managing.Spec.ClusterRef = "np"
	if err := kube.Update(ctx, managing); !apierrors.IsInvalid(err) {
		t.Errorf("a hub moved off the managing cluster (%v)", err)
	}
	valid.Spec.ClusterRef = ""
	if err := kube.Update(ctx, valid); !apierrors.IsInvalid(err) {
		t.Errorf("a hub moved onto the managing cluster (%v)", err)
	}
}
