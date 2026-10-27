/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Alerting (operator#15). Every cluster runs Prometheus and Alertmanager from the platform layer
// (kube-prometheus-stack, the same release and values on every cluster). What differs between
// clusters is only what resources ask for:
//
//   - PrometheusRules, written next to what they watch: each OramaCloudV1 stack's, each Vizier's,
//     each hub's. Most rules read the operator's own status through kube-state-metrics'
//     custom-resource metrics (orama_<kind>_phase, orama_<kind>_component_ready,
//     orama_pixiecloud_agent_connected), so an alert says what the resource's status says.
//   - One AlertmanagerConfig per cluster, written by the managing cluster through the cluster's
//     PixieVizier (or the hub's PixieCloud when the hub's cluster has no agent): every alert of
//     the cluster goes to its hub's receivers. The platform Alertmanager selects it by the
//     orama.cloud/alerting label and does not restrict it to its namespace.
//
// A cluster without the monitoring CRDs yet (the platform layer not installed) is not an error:
// the objects are written once the CRDs exist, and the condition says they are not.

var (
	prometheusRuleGVK     = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"}
	podMonitorGVK         = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PodMonitor"}
	alertmanagerConfigGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1alpha1", Kind: "AlertmanagerConfig"}
)

const (
	alertingLabel        = "orama.cloud/alerting"
	observabilityRules   = "orama-observability"
	observabilityRouting = "orama-alert-routing"
	// alertRouteSecretKey is the key of the URL in each copied receiver Secret.
	alertRouteSecretKey = "url"
)

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

// alertmanagerConfigObject routes every alert of the cluster whose severity a receiver takes to
// that receiver, titled with the cluster's name (Prometheus' labels are the same on every cluster).
func alertmanagerConfigObject(namespace, cluster string, routes []oramav1alpha1.PixieAlertRoute) *unstructured.Unstructured {
	sorted := append([]oramav1alpha1.PixieAlertRoute(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	receivers := []any{map[string]any{"name": "none"}}
	children := make([]any, 0, len(sorted))
	title := fmt.Sprintf(`[%s] {{ .Status | toUpper }} {{ .CommonLabels.alertname }}`, cluster)
	for _, route := range sorted {
		receiver := map[string]any{"name": route.Name}
		url := map[string]any{"name": route.SecretName, "key": alertRouteSecretKey}
		switch route.Kind {
		case "slack":
			slack := map[string]any{"apiURL": url, "sendResolved": true, "title": title,
				"text": `{{ range .Alerts }}{{ .Annotations.summary }} ({{ .Labels.namespace }}){{ "\n" }}{{ end }}`}
			if route.Channel != "" {
				slack["channel"] = route.Channel
			}
			receiver["slackConfigs"] = []any{slack}
		case "webhook":
			receiver["webhookConfigs"] = []any{map[string]any{"urlSecret": url, "sendResolved": true}}
		}
		receivers = append(receivers, receiver)
		severities := route.Severities
		if len(severities) == 0 {
			severities = []string{"critical", "warning"}
		}
		children = append(children, map[string]any{
			"receiver": route.Name,
			"matchers": []any{map[string]any{"name": "severity", "matchType": "=~", "value": strings.Join(severities, "|")}},
			"continue": true,
		})
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(alertmanagerConfigGVK)
	object.SetNamespace(namespace)
	object.SetName(observabilityRouting)
	object.SetLabels(map[string]string{alertingLabel: "true"})
	object.Object["spec"] = map[string]any{
		"route": map[string]any{
			"receiver":       "none",
			"groupBy":        []any{"alertname", "namespace"},
			"groupWait":      "30s",
			"groupInterval":  "5m",
			"repeatInterval": "4h",
			"routes":         children,
		},
		"receivers": receivers,
	}
	return object
}

// reconcileObservabilityAlerting writes owner's rules and, when routes are given, the cluster's
// alert routing; it removes the routing when routes are empty. Missing monitoring CRDs return
// errMonitoringCRDsMissing after writing nothing.
func reconcileObservabilityAlerting(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	cluster string,
	routes []oramav1alpha1.PixieAlertRoute,
	rules []alertRule,
) error {
	objects := []*unstructured.Unstructured{
		prometheusRuleObject(owner.GetNamespace(), observabilityRules, map[string]string{alertingLabel: "true"}, "orama.observability", rules),
	}
	if len(routes) > 0 {
		objects = append(objects, alertmanagerConfigObject(owner.GetNamespace(), cluster, routes))
	} else if err := deleteMonitoringObject(ctx, c, alertmanagerConfigGVK, owner.GetNamespace(), observabilityRouting); err != nil {
		return err
	}
	return applyMonitoringObjects(ctx, c, scheme, owner, objects)
}

// applyMonitoringObjects creates or updates monitoring objects controlled by owner, writing only
// when an object differs from what is wanted.
func applyMonitoringObjects(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, objects []*unstructured.Unstructured) error {
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

func deleteMonitoringObject(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string) error {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	object.SetNamespace(namespace)
	object.SetName(name)
	if err := c.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		return err
	}
	return nil
}

// vizierAlertRules page when this cluster's agents, or its link to the hub, stay down. They are
// evaluated on the agent's own cluster, so they fire even when the hub cannot be reached.
func vizierAlertRules(vz *oramav1alpha1.PixieVizier) []alertRule {
	rules := []alertRule{{
		Alert:    "ObservabilityAgentStatusMissing",
		Expr:     fmt.Sprintf(`absent(orama_pixievizier_phase{namespace=%q})`, vz.Namespace),
		For:      "15m",
		Severity: "warning",
		Summary:  "kube-state-metrics reports no status for this cluster's observability agents: their alerts cannot fire.",
	}, {
		Alert:    "ObservabilityAgentNotReady",
		Expr:     fmt.Sprintf(`orama_pixievizier_phase{namespace=%q,phase="Ready"} == 0`, vz.Namespace),
		For:      "15m",
		Severity: "warning",
		Summary:  "Pixie's agents on this cluster have not been Ready for 15 minutes (kubectl get pxvizier -n pl).",
	}}
	if isPeerImportAddress(vz.Spec.CloudAddress) {
		rules = append(rules, alertRule{
			Alert:    "ObservabilityHubUnreachable",
			Expr:     fmt.Sprintf(`orama_peerconnection_phase{namespace=%q,phase="Ready"} == 0`, vz.Namespace),
			For:      "15m",
			Severity: "critical",
			Summary:  "The link from this cluster to its observability hub has been down for 15 minutes (kubectl get peer -n pl).",
		})
	}
	return rules
}

// pixieCloudAlertRules page when the hub is not Ready or an agent cluster stops reporting. A whole
// agent cluster lost cannot page from itself, so this is the rule that notices.
func pixieCloudAlertRules(cloud *oramav1alpha1.PixieCloud) []alertRule {
	rules := []alertRule{
		{
			Alert:    "ObservabilityHubNotReady",
			Expr:     fmt.Sprintf(`orama_pixiecloud_phase{namespace=%q,phase="Ready"} == 0`, cloud.Namespace),
			For:      "15m",
			Severity: "warning",
			Summary:  "The observability hub has not been Ready for 15 minutes (kubectl get pxcloud -n plc).",
		},
		{
			Alert:    "ObservabilityAgentDisconnected",
			Expr:     fmt.Sprintf(`orama_pixiecloud_agent_connected{namespace=%q} == 0`, cloud.Namespace),
			For:      "10m",
			Severity: "critical",
			Summary:  "An agent cluster ({{ $labels.agent }}) has not reported to the observability hub for 10 minutes: the cluster or its link may be down.",
		},
	}
	if len(cloud.Spec.Agents) > 0 {
		// With agents listed, no metric at all means the rule above cannot fire.
		rules = append(rules, alertRule{
			Alert:    "ObservabilityAgentStatusMissing",
			Expr:     fmt.Sprintf(`absent(orama_pixiecloud_agent_connected{namespace=%q})`, cloud.Namespace),
			For:      "15m",
			Severity: "warning",
			Summary:  "kube-state-metrics reports no agent status for the observability hub: lost clusters cannot page.",
		})
	}
	return rules
}
