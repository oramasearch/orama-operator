/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const (
	discoPort           = 8080
	discoEmbeddingsPort = 50051
)

// reconcileDisco provisions the DISCO semantic cache as a Deployment: the Go DISCO
// service + a co-located gRPC embeddings sidecar (matches prod). In-memory (no PVC —
// a restart cold-starts the cache), single-node, exposed on discoPort.
func (r *OramaCloudV1Reconciler) reconcileDisco(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.Disco.Enabled {
		return nil
	}
	if _, err := r.ensureComponentSecret(ctx, oc, "disco"); err != nil {
		return err
	}
	name := oc.Name + "-disco"
	lbls := labelsFor(oc, "disco")

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(1)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:    "disco",
						Image:   componentImage(oc, "disco", "disco"),
						Ports:   []corev1.ContainerPort{{Name: "http", ContainerPort: discoPort}},
						EnvFrom: envFromFor(oc, "disco"),
						Env: []corev1.EnvVar{
							{Name: "DISCO_PORT", Value: "8080"},
							{Name: "EMBEDDINGS_ENDPOINT", Value: "127.0.0.1:50051"},
						},
					},
					{
						Name:  "embeddings",
						Image: componentImage(oc, "embeddings", "embeddings-server-py"),
						Ports: []corev1.ContainerPort{{Name: "grpc", ContainerPort: discoEmbeddingsPort}},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(oc, dep, r.Scheme)
	}); err != nil {
		return err
	}
	return r.upsertService(ctx, oc, name, "disco",
		[]corev1.ServicePort{{Name: "http", Port: discoPort, TargetPort: intstr.FromInt32(discoPort)}})
}
