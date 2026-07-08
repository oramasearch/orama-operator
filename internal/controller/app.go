/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// tcpReadiness is a universal readiness probe — the port accepting connections. Used
// for components whose HTTP health path varies (data stores, ported apps); the engine
// uses richer HTTP /health probes.
func tcpReadiness(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		TimeoutSeconds:      3,
		FailureThreshold:    6,
	}
}

// appDef describes a stateless app run as a Deployment (+ Service, + optional Ingress).
type appDef struct {
	component   string
	image       string
	port        int32
	replicas    int32
	env         []corev1.EnvVar
	args        []string
	ingressHost string
}

// reconcileApp provisions a Deployment + Service (+ Ingress if ingressHost set).
func (r *OramaCloudV1Reconciler) reconcileApp(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, def appDef) error {
	if _, err := r.ensureComponentSecret(ctx, oc, def.component); err != nil {
		return err
	}
	name := oc.Name + "-" + def.component
	lbls := labelsFor(oc, def.component)
	replicas := def.replicas
	if replicas == 0 {
		replicas = 1
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(replicas)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				ImagePullSecrets: imagePullSecrets(oc),
				Containers: []corev1.Container{{
					Name:           def.component,
					Image:          def.image,
					Args:           def.args,
					Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: def.port}},
					Env:            def.env,
					EnvFrom:        envFromFor(oc, def.component),
					ReadinessProbe: tcpReadiness(def.port),
				}},
			},
		}
		return controllerutil.SetControllerReference(oc, dep, r.Scheme)
	}); err != nil {
		return err
	}
	if err := r.upsertService(ctx, oc, name, def.component,
		[]corev1.ServicePort{{Name: "http", Port: def.port, TargetPort: intstr.FromInt32(def.port)}}); err != nil {
		return err
	}
	if def.ingressHost != "" {
		return r.upsertIngress(ctx, oc, name, def.component, def.ingressHost, def.port)
	}
	return nil
}

func (r *OramaCloudV1Reconciler) upsertIngress(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component, host string, port int32) error {
	lbls := labelsFor(oc, component)
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = lbls
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: name,
								Port: networkingv1.ServiceBackendPort{Number: port},
							},
						},
					}},
				},
			},
		}}
		return controllerutil.SetControllerReference(oc, ing, r.Scheme)
	})
	return err
}

// ---- workers + dashboard reconcilers ----

func (r *OramaCloudV1Reconciler) reconcileWorkers(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	w := oc.Spec.Components.Workers
	if !w.Enabled {
		return nil
	}
	img := w.Image
	if img == "" {
		img = componentImage(oc, "workers", "orama-workers") // one bundled image for all CF workers
	}
	return r.reconcileApp(ctx, oc, appDef{component: "workers", image: img, port: 8080, replicas: w.Replicas})
}

func (r *OramaCloudV1Reconciler) reconcileDashboard(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Dashboard
	if !d.Enabled {
		return nil
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:   "dashboard",
		image:       componentImage(oc, "dashboard", "dashboard"),
		port:        3000,
		ingressHost: d.Host,
	})
}
