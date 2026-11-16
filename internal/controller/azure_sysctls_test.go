// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestAKSSysctlWireMapping(t *testing.T) {
	// Linux names and wire names are checked independently against SysctlConfig in
	// the 2025-05-01 API, including its tcpkeepaliveIntvl capitalization.
	cases := []struct {
		kernel, wire string
		value        int32
	}{
		{"net.core.somaxconn", "netCoreSomaxconn", 4096},
		{"net.core.netdev_max_backlog", "netCoreNetdevMaxBacklog", 1000},
		{"net.core.rmem_default", "netCoreRmemDefault", 212992},
		{"net.core.rmem_max", "netCoreRmemMax", 134217728},
		{"net.core.wmem_default", "netCoreWmemDefault", 212992},
		{"net.core.wmem_max", "netCoreWmemMax", 134217728},
		{"net.core.optmem_max", "netCoreOptmemMax", 20480},
		{"net.ipv4.tcp_max_syn_backlog", "netIpv4TcpMaxSynBacklog", 128},
		{"net.ipv4.tcp_max_tw_buckets", "netIpv4TcpMaxTwBuckets", 8000},
		{"net.ipv4.tcp_fin_timeout", "netIpv4TcpFinTimeout", 5},
		{"net.ipv4.tcp_keepalive_time", "netIpv4TcpKeepaliveTime", 30},
		{"net.ipv4.tcp_keepalive_probes", "netIpv4TcpKeepaliveProbes", 1},
		{"net.ipv4.tcp_keepalive_intvl", "netIpv4TcpkeepaliveIntvl", 10},
		{"net.ipv4.neigh.default.gc_thresh1", "netIpv4NeighDefaultGcThresh1", 128},
		{"net.ipv4.neigh.default.gc_thresh2", "netIpv4NeighDefaultGcThresh2", 512},
		{"net.ipv4.neigh.default.gc_thresh3", "netIpv4NeighDefaultGcThresh3", 1024},
		{"net.netfilter.nf_conntrack_max", "netNetfilterNfConntrackMax", 131072},
		{"net.netfilter.nf_conntrack_buckets", "netNetfilterNfConntrackBuckets", 65536},
		{"fs.inotify.max_user_watches", "fsInotifyMaxUserWatches", 781250},
		{"fs.file-max", "fsFileMax", 8192},
		{"fs.aio-max-nr", "fsAioMaxNr", 65536},
		{"fs.nr_open", "fsNrOpen", 8192},
		{"kernel.threads-max", "kernelThreadsMax", 99999},
		{"vm.max_map_count", "vmMaxMapCount", 4194304},
		{"vm.swappiness", "vmSwappiness", 0},
		{"vm.vfs_cache_pressure", "vmVfsCachePressure", 100},
	}
	desired := oramav1alpha1.NodeSysctls{}
	expected := map[string]json.RawMessage{}
	for _, tc := range cases {
		desired[tc.kernel] = intstr.FromInt32(tc.value)
		encoded, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		expected[tc.wire] = encoded
	}
	desired["net.ipv4.tcp_tw_reuse"] = intstr.FromInt32(0)
	desired["net.ipv4.ip_local_port_range"] = intstr.FromString("20000\t60000")
	expected["netIpv4TcpTwReuse"] = json.RawMessage(`false`)
	expected["netIpv4IpLocalPortRange"] = json.RawMessage(`"20000 60000"`)
	config, err := aksLinuxOSConfig(desired)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config.Sysctls)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("wire config = %s, want %v", encoded, expected)
	}
	desired["net.ipv4.ip_local_port_range"] = intstr.FromString("20000 60000")
	if decoded := aksReportedSysctls(config); !reflect.DeepEqual(decoded, desired) {
		t.Fatalf("reported settings = %v, want %v", decoded, desired)
	}
	// An omitted map sends no linuxOSConfig, preserving existing resource behavior.
	if config, err := aksLinuxOSConfig(nil); config != nil || err != nil {
		t.Fatalf("empty config: %v, %v", config, err)
	}
}

func TestAKSSysctlsRefuseUnsupportedNamesTypesAndValues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   intstr.IntOrString
		problem string
	}{
		{"kernel.unlisted", intstr.FromInt32(1), "not supported"},
		{"vmMaxMapCount", intstr.FromInt32(4194304), "not supported"},
		{"vm.max_map_count", intstr.FromInt32(0), "integer in"},
		{"vm.max_map_count", intstr.FromString("4194304"), "integer in"},
		{"vm.swappiness", intstr.FromInt32(101), "0..100"},
		{"net.ipv4.tcp_keepalive_intvl", intstr.FromInt32(9), "10..90"},
		{"net.netfilter.nf_conntrack_max", intstr.FromInt32(2097153), "131072..2097152"},
		{"net.ipv4.tcp_tw_reuse", intstr.FromInt32(2), "0 or 1"},
		{"net.ipv4.tcp_tw_reuse", intstr.FromString("true"), "0 or 1"},
		{"net.ipv4.ip_local_port_range", intstr.FromInt32(20000), "string"},
		{"net.ipv4.ip_local_port_range", intstr.FromString("20000"), "two space-separated"},
		{"net.ipv4.ip_local_port_range", intstr.FromString("60000 40000"), "first < last"},
		{"net.ipv4.ip_local_port_range", intstr.FromString("1000 60000"), "1024..60999"},
		{"net.ipv4.ip_local_port_range", intstr.FromString("20000 65536"), "32768..65535"},
		{"net.ipv4.ip_local_port_range", intstr.FromString("no ports"), "first port"},
	} {
		t.Run(tc.name+"/"+tc.value.String(), func(t *testing.T) {
			_, err := aksLinuxOSConfig(oramav1alpha1.NodeSysctls{tc.name: tc.value})
			if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("want named problem %q, got %v", tc.problem, err)
			}
		})
	}
}

func TestClusterSysctlsAreCreatedAndDriftIsObservedWithoutWrites(t *testing.T) {
	azure := newFakeAzure(t)
	abstract := testKubernetesCluster()
	abstract.Spec.NodePools[1].Sysctls = oramav1alpha1.NodeSysctls{
		"vm.max_map_count":      intstr.FromInt32(4194304),
		"vm.swappiness":         intstr.FromInt32(0),
		"net.ipv4.tcp_tw_reuse": intstr.FromInt32(1),
	}
	c := clusterTestClient(t, abstract, testProvider(), azureCredentialsSecret(true))
	kr := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
	ar := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	reconcileKC(t, kr, abstract.Name)
	reconcileAks(t, ar, abstract.Name)
	sent := azure.clusters[abstract.Name]
	if sent.Properties.AgentPoolProfiles[0].LinuxOSConfig != nil {
		t.Fatal("sysctls leaked into the system pool")
	}
	reader := sent.Properties.AgentPoolProfiles[1].LinuxOSConfig.Sysctls
	if reader.VmMaxMapCount == nil || *reader.VmMaxMapCount != 4194304 || reader.VmSwappiness == nil || *reader.VmSwappiness != 0 || reader.NetIpv4TcpTwReuse == nil || !*reader.NetIpv4TcpTwReuse {
		t.Fatalf("reader settings did not reach Azure: %+v", reader)
	}
	azure.setState(abstract.Name, "Succeeded")
	reconcileAks(t, ar, abstract.Name)
	if !apimeta.IsStatusConditionTrue(getAks(t, c, abstract.Name).Status.Conditions, clusterConditionInSync) {
		t.Fatal("matching settings are not InSync")
	}
	// Azure reports a changed value and omits two configured fields. Both are drift;
	// omitted fields must never be assumed to match an OS default.
	changed := int32(1048576)
	reader.VmMaxMapCount = &changed
	reader.VmSwappiness = nil
	reader.NetIpv4TcpTwReuse = nil
	puts := azure.count("PUT ")
	reconcileAks(t, ar, abstract.Name)
	reconcileKC(t, kr, abstract.Name)
	got := getKC(t, c, abstract.Name)
	condition := apimeta.FindStatusCondition(got.Status.Conditions, clusterConditionInSync)
	if got.Status.Phase != clusterPhaseReady || condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("drift status: %+v", got.Status)
	}
	for _, message := range []string{
		"node pool userpool: sysctl vm.max_map_count: want 4194304, cluster has 1048576",
		"sysctl vm.swappiness: want 0, Azure does not report a value",
		"sysctl net.ipv4.tcp_tw_reuse: want 1, Azure does not report a value",
	} {
		if !strings.Contains(condition.Message, message) {
			t.Errorf("drift lacks %q: %s", message, condition.Message)
		}
	}
	beforeAks, beforeKC := getAks(t, c, abstract.Name).ResourceVersion, got.ResourceVersion
	reconcileAks(t, ar, abstract.Name)
	reconcileKC(t, kr, abstract.Name)
	if azure.count("PUT ") != puts || getAks(t, c, abstract.Name).ResourceVersion != beforeAks || getKC(t, c, abstract.Name).ResourceVersion != beforeKC {
		t.Fatal("steady drift caused an Azure mutation or status flap")
	}
	// Editing the resource is also observed only; the operator cannot retrofit a pool.
	got.Spec.NodePools[1].Sysctls["vm.max_map_count"] = intstr.FromInt32(8388608)
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileKC(t, kr, abstract.Name)
	reconcileAks(t, ar, abstract.Name)
	reconcileKC(t, kr, abstract.Name)
	condition = apimeta.FindStatusCondition(getKC(t, c, abstract.Name).Status.Conditions, clusterConditionInSync)
	if !strings.Contains(condition.Message, "want 8388608, cluster has 1048576") || azure.count("PUT ") != puts {
		t.Fatalf("spec edit was not observed safely: %+v", condition)
	}
}

func TestClusterSysctlsRefusedBeforeProvisioning(t *testing.T) {
	bad := oramav1alpha1.NodeSysctls{"kernel.unlisted": intstr.FromInt32(1)}
	abstract := testKubernetesCluster()
	abstract.Spec.NodePools[1].Sysctls = bad
	abstract.Status.Conditions = []metav1.Condition{{Type: clusterConditionInSync, Status: metav1.ConditionTrue, Reason: "InSync", LastTransitionTime: metav1.Now()}}
	c := clusterTestClient(t, abstract, testProvider())
	kr := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
	if _, err := kr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: abstract.Name}}); err == nil {
		t.Fatal("expected unsupported sysctl error")
	}
	got := getKC(t, c, abstract.Name)
	if got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, "node pool userpool: sysctl kernel.unlisted") || !apimeta.IsStatusConditionFalse(got.Status.Conditions, clusterConditionInSync) {
		t.Fatalf("abstract refusal: %+v", got.Status)
	}
	var concrete oramav1alpha1.AksCluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: abstract.Name}, &concrete); !apierrors.IsNotFound(err) {
		t.Fatal("invalid settings created a concrete cluster")
	}
	azure := newFakeAzure(t)
	aks := testAksCluster(oramav1alpha1.ClusterDeletionRetain)
	aks.Spec.NodePools[0].Sysctls = bad
	aks.Status.Conditions = []metav1.Condition{{Type: clusterConditionInSync, Status: metav1.ConditionTrue, Reason: "InSync", LastTransitionTime: metav1.Now()}}
	c = clusterTestClient(t, aks, azureCredentialsSecret(true))
	ar := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t), Azure: azure.client()}
	reconcileAks(t, ar, aks.Name)
	gotAks := getAks(t, c, aks.Name)
	if gotAks.Status.Phase != clusterPhaseDegraded || !strings.Contains(gotAks.Status.Message, "not supported by AKS") || !apimeta.IsStatusConditionFalse(gotAks.Status.Conditions, clusterConditionInSync) || len(azure.requests) != 0 {
		t.Fatalf("concrete refusal: %+v; requests %v", gotAks.Status, azure.requests)
	}
}

func TestAKSSysctlDriftIgnoresUnmanagedDefaultsAndNormalizesRanges(t *testing.T) {
	have := int32(1048576)
	portRange := "20000\t60000"
	live := &azureLinuxOSConfig{Sysctls: &azureSysctlConfig{VmMaxMapCount: &have, NetIpv4IpLocalPortRange: &portRange}}
	if drift := aksSysctlDrift(oramav1alpha1.AksNodePool{Name: "readers"}, live); len(drift) != 0 {
		t.Fatalf("unmanaged defaults are drift: %v", drift)
	}
	pool := oramav1alpha1.AksNodePool{Name: "readers", Sysctls: oramav1alpha1.NodeSysctls{"net.ipv4.ip_local_port_range": intstr.FromString("20000 60000")}}
	if drift := aksSysctlDrift(pool, live); len(drift) != 0 {
		t.Fatalf("equivalent range is drift: %v", drift)
	}
	for _, missing := range []*azureLinuxOSConfig{nil, {}} {
		if drift := aksSysctlDrift(pool, missing); len(drift) != 1 || !strings.Contains(drift[0], "does not report") {
			t.Fatalf("missing config drift: %v", drift)
		}
	}
}
