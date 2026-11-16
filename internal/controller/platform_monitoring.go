/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The platform layer's monitoring (operator#15). Every cluster runs Prometheus, Alertmanager and
// Perses from kube-prometheus-stack, the same release and values on every cluster (helmfile.yaml
// and each cluster's platform.yaml). Resources that want monitoring write their own PodMonitors,
// PrometheusRules and dashboard ConfigMaps next to themselves (oramacloudv1_monitoring.go).
//
// A cluster without the monitoring CRDs yet (the platform layer not installed) is not an error:
// the objects are written once the CRDs exist, and the resource's status says they are not.

var (
	prometheusRuleGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"}
	podMonitorGVK     = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PodMonitor"}
)

// platformMonitoringNamespace is where the platform layer runs kube-prometheus-stack.
const platformMonitoringNamespace = "monitoring"

// errMonitoringCRDsMissing says the platform layer's monitoring CRDs are not on the cluster.
var errMonitoringCRDsMissing = fmt.Errorf("the monitoring CRDs (kube-prometheus-stack) are not installed on this cluster")

// alertRule is one Prometheus alerting rule.
type alertRule struct {
	Alert, Expr, For, Severity, Summary string
}

func (rule alertRule) object() map[string]any {
	return map[string]any{
		"alert":       rule.Alert,
		"expr":        rule.Expr,
		"for":         rule.For,
		"labels":      map[string]any{"severity": rule.Severity},
		"annotations": map[string]any{"summary": rule.Summary},
	}
}

func prometheusRuleObject(namespace, name string, labels map[string]string, group string, rules []alertRule) *unstructured.Unstructured {
	items := make([]any, 0, len(rules))
	for _, rule := range rules {
		items = append(items, rule.object())
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(prometheusRuleGVK)
	object.SetNamespace(namespace)
	object.SetName(name)
	object.SetLabels(labels)
	object.Object["spec"] = map[string]any{"groups": []any{map[string]any{"name": group, "rules": items}}}
	return object
}

// applyMonitoringObjects creates or updates monitoring objects controlled by owner, writing only
// when an object differs from what is wanted. Every kind is looked up first: if one is missing (the
// platform layer not installed, or installed in part), nothing is written.
func applyMonitoringObjects(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, objects []*unstructured.Unstructured) error {
	kinds := make([]schema.GroupVersionKind, 0, len(objects))
	for _, object := range objects {
		kinds = append(kinds, object.GroupVersionKind())
	}
	if !kindsInstalled(c, kinds...) {
		return errMonitoringCRDsMissing
	}
	for _, desired := range objects {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(desired.GroupVersionKind())
		live.SetNamespace(desired.GetNamespace())
		live.SetName(desired.GetName())
		_, err := controllerutil.CreateOrUpdate(ctx, c, live, func() error {
			if live.GetResourceVersion() != "" && !metav1.IsControlledBy(live, owner) {
				return fmt.Errorf("%s %s/%s exists and is not controlled by %s", desired.GetKind(), desired.GetNamespace(), desired.GetName(), owner.GetName())
			}
			labels := live.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			for key, value := range desired.GetLabels() {
				labels[key] = value
			}
			live.SetLabels(labels)
			for key, value := range desired.Object {
				if key != "apiVersion" && key != "kind" && key != "metadata" && key != "status" {
					live.Object[key] = runtime.DeepCopyJSONValue(value)
				}
			}
			return controllerutil.SetControllerReference(owner, live, scheme)
		})
		if err != nil {
			if apimeta.IsNoMatchError(err) {
				return errMonitoringCRDsMissing
			}
			return fmt.Errorf("write %s %s: %w", desired.GetKind(), desired.GetName(), err)
		}
	}
	return nil
}

// kindsInstalled reports whether the cluster serves every kind.
func kindsInstalled(c client.Client, kinds ...schema.GroupVersionKind) bool {
	mapper := c.RESTMapper()
	if mapper == nil {
		return false
	}
	for _, kind := range kinds {
		if _, err := mapper.RESTMapping(kind.GroupKind(), kind.Version); err != nil {
			return false
		}
	}
	return true
}

// configMapToUnstructured is a ConfigMap as an apply body.
func configMapToUnstructured(configMap *corev1.ConfigMap) (*unstructured.Unstructured, error) {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(configMap)
	if err != nil {
		return nil, err
	}
	object := &unstructured.Unstructured{Object: data}
	object.SetAPIVersion("v1")
	object.SetKind("ConfigMap")
	unstructured.RemoveNestedField(object.Object, "metadata", "creationTimestamp")
	return object, nil
}
