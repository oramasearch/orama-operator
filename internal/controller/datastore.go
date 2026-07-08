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

// dataStoreDef describes a single-node stateful dependency run as a StatefulSet.
// For light/medium profiles this is a fine in-cluster default; heavy/production
// should swap in the dedicated operator (Altinity ClickHouse, RabbitMQ cluster
// operator) — tracked in docs/components.md.
type dataStoreDef struct {
	component string
	image     string
	ports     []corev1.ContainerPort
	dataPath  string
	env       []corev1.EnvVar
	args      []string
}

// reconcileDataStore provisions (or skips) one data store per its DataStoreSpec.
func (r *OramaCloudV1Reconciler) reconcileDataStore(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, spec oramav1alpha1.DataStoreSpec, def dataStoreDef) error {
	if !spec.Enabled || spec.Mode == "external" {
		return nil // disabled, or the app points at an external endpoint
	}
	// "central" placement: only the primary runs it; read tiers reference the endpoint.
	if spec.Placement == "central" && oc.Spec.Region.Role == "read" {
		return nil
	}
	return r.provisionStatefulSet(ctx, oc, def)
}

// provisionStatefulSet unconditionally provisions a single-node StatefulSet + PVC +
// Service for a def (used by data stores and by ingestion's Mongo).
func (r *OramaCloudV1Reconciler) provisionStatefulSet(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, def dataStoreDef) error {
	name := oc.Name + "-" + def.component
	lbls := labelsFor(oc, def.component)

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = lbls
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.ServiceName = name
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:         def.component,
					Image:        storeImage(oc, def.component, def.image),
					Ports:        def.ports,
					Env:          def.env,
					Args:         def.args,
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: def.dataPath}},
				}},
			},
		}
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{dataPVC()}
		return controllerutil.SetControllerReference(oc, sts, r.Scheme)
	}); err != nil {
		return err
	}

	svcPorts := make([]corev1.ServicePort, 0, len(def.ports))
	for _, p := range def.ports {
		svcPorts = append(svcPorts, corev1.ServicePort{Name: p.Name, Port: p.ContainerPort, TargetPort: intstr.FromInt32(p.ContainerPort)})
	}
	return r.upsertService(ctx, oc, name, def.component, svcPorts)
}

// storeImage: an override wins, else the default upstream image.
func storeImage(oc *oramav1alpha1.OramaCloudV1, component, def string) string {
	if o := oc.Spec.Images.Overrides[component]; o != "" {
		return o
	}
	return def
}

// ---- per-store reconcilers (wired into the reconcile step-list) ----

func (r *OramaCloudV1Reconciler) reconcileCache(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Cache, dataStoreDef{
		component: "cache",
		image:     "redis:7-alpine",
		ports:     []corev1.ContainerPort{{Name: "redis", ContainerPort: 6379}},
		dataPath:  "/data",
		args:      []string{"redis-server", "--appendonly", "yes"},
	})
}

func (r *OramaCloudV1Reconciler) reconcileBroker(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Broker, dataStoreDef{
		component: "broker",
		image:     "rabbitmq:3.13-management",
		ports: []corev1.ContainerPort{
			{Name: "amqp", ContainerPort: 5672},
			{Name: "management", ContainerPort: 15672},
		},
		dataPath: "/var/lib/rabbitmq",
	})
}

func (r *OramaCloudV1Reconciler) reconcileAnalytics(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Analytics, dataStoreDef{
		component: "analytics",
		image:     "clickhouse/clickhouse-server:24.8",
		ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: 8123},
			{Name: "native", ContainerPort: 9000},
		},
		dataPath: "/var/lib/clickhouse",
	})
}

func (r *OramaCloudV1Reconciler) reconcileSearch(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Search, dataStoreDef{
		component: "search",
		image:     "typesense/typesense:0.25.2",
		ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: 8108}},
		dataPath:  "/data",
		env: []corev1.EnvVar{
			{Name: "TYPESENSE_DATA_DIR", Value: "/data"},
			{Name: "TYPESENSE_ENABLE_CORS", Value: "true"},
			// Dev default; production must override via ExternalSecrets. Not a real key.
			{Name: "TYPESENSE_API_KEY", Value: "changeme-dev"},
		},
	})
}
