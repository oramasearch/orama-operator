// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Used space of a volume store (lake#666). The operator reads the kubelet's stats summary of the
// node that runs the pod mounting the store's claim (nodes/proxy, /stats/summary) and writes
// store_status.used_bytes and used_bytes_at. A store not mounted or not measured keeps what it had
// (null until first measured). It measures at most once per storeUsageInterval per Lake, and writes a
// store's row only when the value moved by more than storeUsageMinChange or storeUsageMaxAge has
// passed since the last write, so a steady volume makes almost no database writes. The manager's
// nodes/proxy get is marked in lake_controller.go and granted in the chart ClusterRole.
const (
	storeUsageInterval  = time.Minute
	storeUsageMinChange = int64(1 << 20)
	storeUsageMaxAge    = 15 * time.Minute
)

// LakeStoreUsage is a store's used space as last written.
type LakeStoreUsage struct {
	UsedBytes int64
	At        time.Time
}

// VolumeStatsReader reads one node's volume stats: the bytes in use per PersistentVolumeClaim of
// the pods on it.
type VolumeStatsReader interface {
	PVCUsedBytes(ctx context.Context, node string) (map[types.NamespacedName]int64, error)
}

// kubeletVolumeStats reads the kubelet's stats summary through the API server's node proxy.
type kubeletVolumeStats struct{ clientset kubernetes.Interface }

// kubeletStatsSummary is the part of the kubelet's /stats/summary the operator reads.
type kubeletStatsSummary struct {
	Pods []struct {
		Volumes []struct {
			UsedBytes *uint64 `json:"usedBytes"`
			PVCRef    *struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
		} `json:"volume"`
	} `json:"pods"`
}

func (k kubeletVolumeStats) PVCUsedBytes(ctx context.Context, node string) (map[types.NamespacedName]int64, error) {
	raw, err := k.clientset.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/nodes", node, "proxy", "stats", "summary").DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	return parseKubeletVolumeStats(raw)
}

func parseKubeletVolumeStats(raw []byte) (map[types.NamespacedName]int64, error) {
	var summary kubeletStatsSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("parse the kubelet stats summary: %w", err)
	}
	used := map[types.NamespacedName]int64{}
	for _, pod := range summary.Pods {
		for _, volume := range pod.Volumes {
			if volume.PVCRef == nil || volume.UsedBytes == nil {
				continue
			}
			used[types.NamespacedName{Namespace: volume.PVCRef.Namespace, Name: volume.PVCRef.Name}] = int64(*volume.UsedBytes)
		}
	}
	return used, nil
}

// storeUsageClock throttles the measurement per Lake.
type storeUsageClock struct {
	mu   sync.Mutex
	last map[types.UID]time.Time
}

func (c *storeUsageClock) due(uid types.UID, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[types.UID]time.Time{}
	}
	if last, found := c.last[uid]; found && now.Sub(last) < storeUsageInterval {
		return false
	}
	c.last[uid] = now
	return true
}

func (r *LakeReconciler) volumeStats() VolumeStatsReader {
	if r.VolumeStats != nil {
		return r.VolumeStats
	}
	if r.Clientset != nil {
		return kubeletVolumeStats{clientset: r.Clientset}
	}
	return nil
}

// storeUsageNeedsWrite: the first measurement, a move of more than 1 MiB, or a stale row.
func storeUsageNeedsWrite(previous LakeStoreUsage, found bool, used int64, now time.Time) bool {
	if !found {
		return true
	}
	delta := used - previous.UsedBytes
	if delta < 0 {
		delta = -delta
	}
	return delta > storeUsageMinChange || now.Sub(previous.At) >= storeUsageMaxAge
}

// reconcileStoreUsage measures each Ready volume store that a running pod mounts and writes its
// used space when it should.
func (r *LakeReconciler) reconcileStoreUsage(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore, stores []*LakeStore, now time.Time) error {
	stats := r.volumeStats()
	if stats == nil {
		return nil
	}
	var volumes []*LakeStore
	for _, st := range stores {
		if storeIsVolume(st) && st.Status != nil && st.Status.Phase == ServicePhaseReady {
			volumes = append(volumes, st)
		}
	}
	if len(volumes) == 0 {
		return nil
	}
	usage, err := store.ListStoreUsage(ctx)
	if err != nil {
		return err
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	// The node running a pod that mounts each claim, from one pod list per namespace.
	nodes := map[types.NamespacedName]string{}
	listed := map[string]bool{}
	for _, st := range volumes {
		if listed[st.WorkspaceNamespace] {
			continue
		}
		listed[st.WorkspaceNamespace] = true
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(st.WorkspaceNamespace)); err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
				continue
			}
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil {
					nodes[types.NamespacedName{Namespace: pod.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}] = pod.Spec.NodeName
				}
			}
		}
	}
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	summaries := map[string]map[types.NamespacedName]int64{}
	var errs []error
	for _, st := range volumes {
		claim := types.NamespacedName{Namespace: st.WorkspaceNamespace, Name: storeResourceName(lake.Name, st.Slug)}
		node, mounted := nodes[claim]
		if !mounted {
			continue
		}
		summary, read := summaries[node]
		if !read {
			summary, err = stats.PVCUsedBytes(ctx, node)
			if err != nil {
				log.Info("store used space deferred: cannot read the node's volume stats", "node", node, "err", err.Error())
				summary = nil
			}
			summaries[node] = summary
		}
		used, measured := summary[claim]
		if !measured {
			continue
		}
		previous, found := usage[st.ID]
		if !storeUsageNeedsWrite(previous, found, used, now) {
			continue
		}
		if err := store.UpdateStoreUsedBytes(ctx, st.ID, used); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
