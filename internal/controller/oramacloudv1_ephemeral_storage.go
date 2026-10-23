/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	v1EphemeralRequest = "100Mi"
	v1EphemeralLimit   = "1Gi"
)

// boundV1PodStorage covers regular and init containers after resource overrides are applied.
// Kubernetes accounts writable image layers and logs against these limits, and disk emptyDirs
// against the pod's aggregate limit. PVC data is accounted separately.
func boundV1PodStorage(pod *corev1.PodSpec) {
	for _, containers := range [][]corev1.Container{pod.Containers, pod.InitContainers} {
		for i := range containers {
			c := &containers[i]
			c.Resources = v1StorageResources(c.Resources, c.Name)
		}
	}
	for i := range pod.Volumes {
		v := &pod.Volumes[i]
		if v.EmptyDir != nil && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Sign() <= 0) {
			size := "256Mi"
			if v.Name == "rendered-config" {
				size = "64Mi"
			}
			v.EmptyDir.SizeLimit = ptrTo(resource.MustParse(size))
		}
	}
	for i := range pod.Containers {
		for j := range pod.Containers[i].VolumeMounts {
			mount := &pod.Containers[i].VolumeMounts[j]
			if mount.Name == "rendered-config" {
				mount.ReadOnly = true
			}
		}
	}
	for i := range pod.InitContainers {
		c := &pod.InitContainers[i]
		// These commands write only to their rendered-config volume (or only poll the broker).
		if c.Name == "render-config" || c.Name == "wait-for-broker" {
			if c.SecurityContext == nil {
				c.SecurityContext = hardenedSecurityContext()
			}
			c.SecurityContext.ReadOnlyRootFilesystem = boolPtr(true)
		}
	}
}

// v1StorageResources copies the input so filling storage defaults never mutates a CR's maps.
// Missing or nonpositive bounds cannot opt out; positive explicit storage sizing is retained.
func v1StorageResources(in corev1.ResourceRequirements, container string) corev1.ResourceRequirements {
	out := *in.DeepCopy()
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	if out.Limits == nil {
		out.Limits = corev1.ResourceList{}
	}
	request := out.Requests[corev1.ResourceEphemeralStorage]
	limit := out.Limits[corev1.ResourceEphemeralStorage]
	if limit.Sign() <= 0 {
		limit = resource.MustParse(v1EphemeralLimit)
		// Leave headroom for embedding model caches.
		if container == "embeddings" || container == hpesComponent {
			limit = resource.MustParse("16Gi")
		}
		if request.Cmp(limit) > 0 {
			limit = request.DeepCopy()
		}
	}
	if request.Sign() <= 0 {
		request = resource.MustParse(v1EphemeralRequest)
		if request.Cmp(limit) > 0 {
			request = limit.DeepCopy()
		}
	}
	out.Requests[corev1.ResourceEphemeralStorage] = request
	out.Limits[corev1.ResourceEphemeralStorage] = limit
	return out
}

// reconcileV1StorageDefaults also bounds future containers and pods created by child operators.
// LimitRanger admission rejects explicit zero storage instead of allowing a default bypass.
func (r *OramaCloudV1Reconciler) reconcileV1StorageDefaults(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	limits := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-ephemeral-storage", Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, limits, func() error {
		if err := refuseForeign(oc, "LimitRange", limits); err != nil {
			return err
		}
		limits.Labels = labelsFor(oc, "storage-defaults")
		limits.Spec.Limits = []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			Min:            corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1")},
			DefaultRequest: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse(v1EphemeralRequest)},
			Default:        corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse(v1EphemeralLimit)},
		}}
		return controllerutil.SetControllerReference(oc, limits, r.Scheme)
	})
	return err
}
