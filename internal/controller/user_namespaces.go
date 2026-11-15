package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Kubernetes user namespaces (`hostUsers: false`, operator#763) map a pod's UIDs to a host range of
// its own, so UID 10001 in one tenant's app is not UID 10001 in another's, nor on the node. The
// operator asks for one only when every node of the cluster reports, through the Kubernetes API, the
// versions that support it. There is no flag or chart value: the same operator decides the same way
// on every cluster, from what that cluster's nodes say.
//
//   - kubelet 1.33 or later: UserNamespacesSupport is on by default from 1.33 (GA in 1.36). A
//     kubelet is never newer than its API server (version skew policy), so the API server is 1.33
//     or later too.
//   - Linux kernel 6.3 or later: idmapped tmpfs mounts, which Secret, ConfigMap, projected and
//     memory emptyDir volumes need.
//   - containerd 2.0 or later, or CRI-O 1.25 or later.
//
// One node that does not qualify (an old pool, a Windows node, an unknown runtime) turns it off for
// every pod, because the scheduler may place the pod on any node.
var (
	userNamespacesMinKubelet    = []int{1, 33}
	userNamespacesMinKernel     = []int{6, 3}
	userNamespacesMinContainerd = []int{2, 0}
	userNamespacesMinCRIO       = []int{1, 25}
)

// nodesSupportUserNamespaces says whether every node supports `hostUsers: false`, and when not,
// the first node that does not and why.
func nodesSupportUserNamespaces(nodes []corev1.Node) (bool, string) {
	if len(nodes) == 0 {
		return false, "the cluster reports no nodes"
	}
	for _, node := range nodes {
		info := node.Status.NodeInfo
		if !strings.EqualFold(info.OperatingSystem, "linux") {
			return false, fmt.Sprintf("node %s runs %q, not linux", node.Name, info.OperatingSystem)
		}
		if !versionAtLeast(info.KubeletVersion, userNamespacesMinKubelet) {
			return false, fmt.Sprintf("node %s runs kubelet %q, older than 1.33", node.Name, info.KubeletVersion)
		}
		if !versionAtLeast(info.KernelVersion, userNamespacesMinKernel) {
			return false, fmt.Sprintf("node %s runs kernel %q, older than 6.3", node.Name, info.KernelVersion)
		}
		runtime, runtimeVersion, _ := strings.Cut(info.ContainerRuntimeVersion, "://")
		switch runtime {
		case "containerd":
			if !versionAtLeast(runtimeVersion, userNamespacesMinContainerd) {
				return false, fmt.Sprintf("node %s runs containerd %q, older than 2.0", node.Name, runtimeVersion)
			}
		case "cri-o":
			if !versionAtLeast(runtimeVersion, userNamespacesMinCRIO) {
				return false, fmt.Sprintf("node %s runs CRI-O %q, older than 1.25", node.Name, runtimeVersion)
			}
		default:
			return false, fmt.Sprintf("node %s runs container runtime %q, not containerd or CRI-O", node.Name, info.ContainerRuntimeVersion)
		}
	}
	return true, ""
}

// clusterSupportsUserNamespaces lists the cluster's nodes and applies nodesSupportUserNamespaces.
func clusterSupportsUserNamespaces(ctx context.Context, reader client.Reader) (bool, string, error) {
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return false, "", fmt.Errorf("list nodes for user-namespace support: %w", err)
	}
	supported, reason := nodesSupportUserNamespaces(nodes.Items)
	return supported, reason, nil
}

// versionAtLeast compares the leading dotted numbers of a version string ("v1.35.9",
// "6.8.0-1073-azure", "2.3.7-4") with a minimum. A string with no leading number is not at least
// anything.
func versionAtLeast(raw string, minimum []int) bool {
	parts := leadingVersionNumbers(raw)
	if len(parts) == 0 {
		return false
	}
	for i, want := range minimum {
		got := 0
		if i < len(parts) {
			got = parts[i]
		}
		if got != want {
			return got > want
		}
	}
	return true
}

func leadingVersionNumbers(raw string) []int {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	var parts []int
	for _, field := range strings.Split(raw, ".") {
		end := 0
		for end < len(field) && field[end] >= '0' && field[end] <= '9' {
			end++
		}
		if end == 0 {
			break
		}
		number, err := strconv.Atoi(field[:end])
		if err != nil {
			break
		}
		parts = append(parts, number)
		if end < len(field) {
			break
		}
	}
	return parts
}

// withLakeAppUserNamespace gives an app pod its own user namespace when the cluster supports it.
// An app that mounts a store volume (a PersistentVolumeClaim) keeps the host's: its filesystem
// must support idmapped mounts, which the operator cannot read from the API for every
// StorageClass, and a volume refused at mount time would leave the app down.
func withLakeAppUserNamespace(pod corev1.PodSpec, app *oramav1alpha1.LakeApp, supported bool) corev1.PodSpec {
	if !supported || storeMountsHaveVolume(lakeAppStoreMounts(app)) {
		pod.HostUsers = nil
		return pod
	}
	hostUsers := false
	pod.HostUsers = &hostUsers
	return pod
}
