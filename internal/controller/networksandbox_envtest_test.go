/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// networkSandboxCiliumTestCRDs supplies the external API for persistence/watch tests, not Cilium
// schema validation or packet enforcement. Production uses the Cilium-installed CRD and dataplane.
func networkSandboxCiliumTestCRDs() []*apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return []*apiextensionsv1.CustomResourceDefinition{{
		ObjectMeta: metav1.ObjectMeta{Name: "ciliumnetworkpolicies.cilium.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "cilium.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "ciliumnetworkpolicies", Singular: "ciliumnetworkpolicy", Kind: "CiliumNetworkPolicy", ListKind: "CiliumNetworkPolicyList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v2", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", XPreserveUnknownFields: &preserve},
					},
				}},
			}},
		},
	}}
}

func startNetworkSandboxEnvtest(t *testing.T) client.Client {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for NetworkSandbox envtest")
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
	r := &NetworkSandboxReconciler{Client: manager.GetClient(), Scheme: scheme}
	if err := r.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.Start(ctx)
	}()
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

func TestNetworkSandboxEnvtestRepairsCiliumPolicyFromOwnerWatch(t *testing.T) {
	kube := startNetworkSandboxEnvtest(t)
	ctx := context.Background()
	sandbox := metadataSandboxForTest()
	sandbox.UID = ""
	sandbox.ResourceVersion = ""
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Namespace}}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	await := func(check func() (bool, error)) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
			return check()
		}); err != nil {
			t.Fatal(err)
		}
	}
	await(func() (bool, error) {
		var current oramav1alpha1.NetworkSandbox
		if err := kube.Get(ctx, client.ObjectKeyFromObject(sandbox), &current); err != nil {
			return false, err
		}
		return current.Status.CiliumNetworkPolicies == 1, nil
	})
	policy := assertMetadataDeny(t, kube, sandbox)
	want := policy.DeepCopy()
	policy.Object["spec"] = map[string]interface{}{"endpointSelector": map[string]interface{}{}}
	if err := kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	await(func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
			return false, err
		}
		return reflect.DeepEqual(policy.Object["spec"], want.Object["spec"]), nil
	})
	assertMetadataDeny(t, kube, sandbox)
	oldUID := policy.GetUID()
	if err := kube.Delete(ctx, policy); err != nil {
		t.Fatal(err)
	}
	await(func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return policy.GetUID() != oldUID, nil
	})
	assertMetadataDeny(t, kube, sandbox)
	if err := kube.Delete(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(sandbox), &oramav1alpha1.NetworkSandbox{}); !apierrors.IsNotFound(err) {
		t.Fatalf("sandbox deletion failed: %v", err)
	}
	// envtest does not run kube-controller-manager's garbage collector. The policy's exact owner
	// reference was checked above; fake-client tests also verify no recreation during deletion.
}
