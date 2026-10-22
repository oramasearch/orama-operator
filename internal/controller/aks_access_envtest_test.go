/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestAksAccessAdmissionKeepsThePrincipalBinding(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for AKS access admission envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{
		"../../config/crd/bases/orama.cloud_aksclusters.yaml", "../../config/crd/bases/orama.cloud_kubernetesclusterproviders.yaml",
	}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	kube, err := client.New(config, client.Options{Scheme: clusterTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	aks, provider := testAksCluster(oramav1alpha1.ClusterDeletionRetain), testProvider()
	aks.Spec.Access = oramav1alpha1.AksClusterAccess{}
	provider.Spec.AKS.Access = oramav1alpha1.AksProviderAccess{}
	for _, object := range []client.Object{aks, provider} {
		if err := kube.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	// A legacy resource may add its initial principal. Once bound, removing the whole access
	// object must not bypass field-level immutability and strand a prior Azure role assignment.
	aks.Spec.Access = testAksAccess()
	provider.Spec.AKS.Access = testProvider().Spec.AKS.Access
	for _, object := range []client.Object{aks, provider} {
		if err := kube.Update(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		object client.Object
		patch  string
	}{
		{aks, `{"spec":{"access":null}}`},
		{aks, `{"spec":{"access":{"principalID":null}}}`},
		{aks, `{"spec":{"access":{"principalID":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}}`},
		{provider, `{"spec":{"aks":{"access":null}}}`},
		{provider, `{"spec":{"aks":{"access":{"principalID":null}}}}`},
		{provider, `{"spec":{"aks":{"access":{"principalID":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}}}`},
	} {
		if err := kube.Patch(ctx, entry.object, client.RawPatch(types.MergePatchType, []byte(entry.patch))); !apierrors.IsInvalid(err) {
			t.Errorf("principal binding mutation %s: %v", entry.patch, err)
		}
	}
}
