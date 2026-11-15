// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"os"
	"reflect"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Exercise native admission: integer/string union values and unknown kernel names
// must survive storage so the provider can reject unsupported settings with a message.
func TestNodeSysctlsAdmissionRoundTrip(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for node sysctl admission envtest")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{"../../config/crd/bases/orama.cloud_kubernetesclusters.yaml", "../../config/crd/bases/orama.cloud_aksclusters.yaml"},
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
	kube, err := client.New(config, client.Options{Scheme: clusterTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	settings := oramav1alpha1.NodeSysctls{
		"vm.max_map_count":             intstr.FromInt32(4194304),
		"vm.swappiness":                intstr.FromInt32(0),
		"net.ipv4.ip_local_port_range": intstr.FromString("20000 60000"),
		"kernel.unlisted":              intstr.FromInt32(1),
	}
	abstract := testKubernetesCluster()
	abstract.Spec.NodePools[0].Sysctls = settings
	concrete := testAksCluster(oramav1alpha1.ClusterDeletionRetain)
	concrete.Spec.NodePools[0].Sysctls = settings
	for _, object := range []client.Object{abstract, concrete} {
		if err := kube.Create(context.Background(), object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(abstract.Spec.NodePools[0].Sysctls, settings) || !reflect.DeepEqual(concrete.Spec.NodePools[0].Sysctls, settings) {
		t.Fatalf("admission lost sysctls: abstract %v, concrete %v", abstract.Spec.NodePools[0].Sysctls, concrete.Spec.NodePools[0].Sysctls)
	}
}
