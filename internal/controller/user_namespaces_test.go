package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func userNamespaceTestNode(name, kubelet, kernel, runtime string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			OperatingSystem:         "linux",
			KubeletVersion:          kubelet,
			KernelVersion:           kernel,
			ContainerRuntimeVersion: runtime,
		}},
	}
}

// The managing cluster as read on 2026-11-14: AKS 1.35.9, Ubuntu 24.04, kernel 6.8, containerd 2.3.
func TestTheManagingClusterNodesSupportUserNamespaces(t *testing.T) {
	nodes := []corev1.Node{
		userNamespaceTestNode("aks-agentpool-33345110-vmss000000", "v1.35.9", "6.8.0-1073-azure", "containerd://2.3.7-4"),
		userNamespaceTestNode("aks-agentpool-33345110-vmss000009", "v1.35.9", "6.8.0-1073-azure", "containerd://2.3.7-4"),
	}
	if supported, reason := nodesSupportUserNamespaces(nodes); !supported {
		t.Fatalf("expected support, got %q", reason)
	}
}

func TestOneUnqualifiedNodeTurnsUserNamespacesOff(t *testing.T) {
	good := userNamespaceTestNode("good", "v1.35.9", "6.8.0-1073-azure", "containerd://2.3.7-4")
	windows := good
	windows.Name = "win"
	windows.Status.NodeInfo.OperatingSystem = "windows"
	cases := map[string]corev1.Node{
		"kubelet":    userNamespaceTestNode("old-kubelet", "v1.32.4", "6.8.0", "containerd://2.0.1"),
		"kernel":     userNamespaceTestNode("old-kernel", "v1.35.9", "5.15.0-1079-azure", "containerd://2.0.1"),
		"containerd": userNamespaceTestNode("old-containerd", "v1.35.9", "6.8.0", "containerd://1.7.27"),
		"cri-o":      userNamespaceTestNode("old-crio", "v1.35.9", "6.8.0", "cri-o://1.24.6"),
		"runtime":    userNamespaceTestNode("docker", "v1.35.9", "6.8.0", "docker://27.0.0"),
		"windows":    windows,
	}
	for name, bad := range cases {
		supported, reason := nodesSupportUserNamespaces([]corev1.Node{good, bad})
		if supported || !strings.Contains(reason, bad.Name) {
			t.Errorf("%s: expected refusal naming %s, got %v %q", name, bad.Name, supported, reason)
		}
	}
	if supported, _ := nodesSupportUserNamespaces(nil); supported {
		t.Error("a cluster with no nodes must not be taken to support user namespaces")
	}
	if supported, _ := nodesSupportUserNamespaces([]corev1.Node{userNamespaceTestNode("crio", "v1.33.0", "6.3.0", "cri-o://1.25.0")}); !supported {
		t.Error("the minimum versions must qualify")
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		raw     string
		minimum []int
		want    bool
	}{
		{"v1.35.9", userNamespacesMinKubelet, true},
		{"v1.33.0", userNamespacesMinKubelet, true},
		{"1.33", userNamespacesMinKubelet, true},
		{"v1.32.9", userNamespacesMinKubelet, false},
		{"v2.0.0", userNamespacesMinKubelet, true},
		{"6.8.0-1073-azure", userNamespacesMinKernel, true},
		{"6.3", userNamespacesMinKernel, true},
		{"6.2.16", userNamespacesMinKernel, false},
		{"", userNamespacesMinKernel, false},
		{"unknown", userNamespacesMinKernel, false},
	} {
		if got := versionAtLeast(c.raw, c.minimum); got != c.want {
			t.Errorf("versionAtLeast(%q, %v) = %v, want %v", c.raw, c.minimum, got, c.want)
		}
	}
}

func TestAnAppPodGetsItsOwnUserNamespaceOnlyWhenSupported(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}, Spec: oramav1alpha1.LakeSpec{Host: "lake.example.test"}}
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "lake"},
		Spec:       oramav1alpha1.LakeAppSpec{LakeRef: "prod", Image: "example.test/app@sha256:" + strings.Repeat("a", 64), Port: 8080},
	}
	pod := withLakeAppUserNamespace(lakeAppPodSpec(lake, app, lakeAppSecrets{}), app, true)
	if pod.HostUsers == nil || *pod.HostUsers {
		t.Fatalf("supported cluster: expected hostUsers false, got %v", pod.HostUsers)
	}
	pod = withLakeAppUserNamespace(lakeAppPodSpec(lake, app, lakeAppSecrets{}), app, false)
	if pod.HostUsers != nil {
		t.Fatalf("unsupported cluster: expected hostUsers unset, got %v", *pod.HostUsers)
	}
	// A store volume keeps the host's user namespace: its filesystem may not support idmapped mounts.
	app.Spec.Stores = []oramav1alpha1.LakeAppStoreMount{{
		ID: "store", Name: "data", Slug: "st-0123456789ab", Type: "volume", ClaimName: "data", MountPath: "/data",
	}}
	pod = withLakeAppUserNamespace(lakeAppPodSpec(lake, app, lakeAppSecrets{}), app, true)
	if pod.HostUsers != nil {
		t.Fatalf("app with a store volume: expected hostUsers unset, got %v", *pod.HostUsers)
	}
}
