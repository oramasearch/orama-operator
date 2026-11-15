/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

func renderDeploymentChart(t *testing.T, port int) (map[string]string, error) {
	t.Helper()
	chart, err := loader.Load(filepath.Join("..", "chart"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValues(chart, map[string]interface{}{
		"controlPlane": map[string]interface{}{"service": map[string]interface{}{"port": port}},
	}, chartutil.ReleaseOptions{Name: "orama-operator", Namespace: "operator-system"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	return engine.Render(chart, values)
}

func TestDeploymentChartRestrictsCallersAndProvisionsTLS(t *testing.T) {
	for _, port := range []int{8090, 9443} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			rendered, err := renderDeploymentChart(t, port)
			if err != nil {
				t.Fatal(err)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal([]byte(rendered["orama-operator/templates/operator-deployment.yaml"]), &deployment); err != nil {
				t.Fatal(err)
			}
			var policy networkingv1.NetworkPolicy
			if err := yaml.Unmarshal([]byte(rendered["orama-operator/templates/control-plane-network-policy.yaml"]), &policy); err != nil {
				t.Fatal(err)
			}
			selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
			if err != nil || selector.Empty() || !selector.Matches(labels.Set(deployment.Spec.Template.Labels)) {
				t.Fatal("NetworkPolicy does not select the manager")
			}
			if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || len(policy.Spec.Ingress) != 3 {
				t.Fatal("manager ingress is not isolated")
			}
			deployRule := policy.Spec.Ingress[0]
			if len(deployRule.From) != 1 || len(deployRule.Ports) != 1 || deployRule.Ports[0].Port.IntValue() != port {
				t.Fatal("deployment port is not restricted to one caller group")
			}
			peer := deployRule.From[0]
			if peer.NamespaceSelector == nil || peer.PodSelector == nil || peer.IPBlock != nil {
				t.Fatal("caller must match both namespace and pod selectors")
			}
			namespaces, err := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)
			if err != nil {
				t.Fatal(err)
			}
			pods, err := metav1.LabelSelectorAsSelector(peer.PodSelector)
			if err != nil {
				t.Fatal(err)
			}
			allowedNamespaces := labels.Set{"orama.cloud/deployment-clients": "true"}
			allowedPods := labels.Set{"orama.cloud/deployment-client": "true"}
			if namespaces.Matches(labels.Set{}) || pods.Matches(labels.Set{}) ||
				!namespaces.Matches(allowedNamespaces) || !pods.Matches(allowedPods) {
				t.Fatal("unlabelled callers are admitted or authorized callers are blocked")
			}
			// The Lake pods that ask for one service connection (operator#773): their own labelled
			// namespace and pods, on the control-plane port only.
			connectionRule := policy.Spec.Ingress[1]
			if len(connectionRule.From) != 1 || len(connectionRule.Ports) != 1 || connectionRule.Ports[0].Port.IntValue() != port ||
				connectionRule.From[0].NamespaceSelector == nil || connectionRule.From[0].PodSelector == nil || connectionRule.From[0].IPBlock != nil {
				t.Fatal("the service connection callers are not restricted to labelled namespaces and pods")
			}
			connectionNamespaces, _ := metav1.LabelSelectorAsSelector(connectionRule.From[0].NamespaceSelector)
			connectionPods, _ := metav1.LabelSelectorAsSelector(connectionRule.From[0].PodSelector)
			if connectionNamespaces.Matches(labels.Set{}) || connectionPods.Matches(labels.Set{}) ||
				!connectionNamespaces.Matches(labels.Set{"orama.cloud/service-connection-clients": "true"}) ||
				!connectionPods.Matches(labels.Set{"orama.cloud/service-connection-client": "true"}) {
				t.Fatal("unlabelled callers reach the service connection operation")
			}
			if !strings.Contains(strings.Join(deployment.Spec.Template.Spec.Containers[0].Args, " "),
				fmt.Sprintf("--control-plane-service-url=https://orama-operator-control-plane.operator-system.svc:%d", port)) {
				t.Fatal("the manager is not told where lake asks for service connections")
			}
			for _, p := range policy.Spec.Ingress[2].Ports {
				if p.Port == nil || (p.Port.IntValue() != 8080 && p.Port.IntValue() != 8081) || p.EndPort != nil {
					t.Fatal("unrestricted ingress reaches a deployment port")
				}
			}
			container := deployment.Spec.Template.Spec.Containers[0]
			if !strings.Contains(strings.Join(container.Args, " "), "--control-plane-cert-file=/var/run/orama/control-plane/tls.crt") ||
				!strings.Contains(strings.Join(container.Args, " "), "--control-plane-key-file=/var/run/orama/control-plane/tls.key") {
				t.Fatal("manager TLS flags are absent")
			}
			if len(container.VolumeMounts) != 1 || !container.VolumeMounts[0].ReadOnly || container.VolumeMounts[0].SubPath != "" ||
				container.VolumeMounts[0].MountPath != "/var/run/orama/control-plane" || len(deployment.Spec.Template.Spec.Volumes) != 1 ||
				deployment.Spec.Template.Spec.Volumes[0].Secret == nil || deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName != "orama-operator-control-plane-tls" {
				t.Fatal("serving certificate is not projected for live renewal")
			}
			objects := map[string]unstructured.Unstructured{}
			for _, document := range strings.Split(rendered["orama-operator/templates/control-plane-tls.yaml"], "---") {
				var object unstructured.Unstructured
				if err := yaml.Unmarshal([]byte(document), &object.Object); err != nil {
					t.Fatal(err)
				}
				if object.GetNamespace() != "operator-system" {
					t.Fatal("TLS resources are outside the manager namespace")
				}
				objects[object.GetKind()+"/"+object.GetName()] = object
			}
			leaf := objects["Certificate/orama-operator-control-plane"]
			secret, _, _ := unstructured.NestedString(leaf.Object, "spec", "secretName")
			dnsNames, _, _ := unstructured.NestedStringSlice(leaf.Object, "spec", "dnsNames")
			issuer, _, _ := unstructured.NestedString(leaf.Object, "spec", "issuerRef", "name")
			if secret != "orama-operator-control-plane-tls" || issuer != "orama-operator-control-plane" ||
				!strings.Contains(strings.Join(dnsNames, " "), "orama-operator-control-plane.operator-system.svc.cluster.local") {
				t.Fatal("serving certificate does not match the Service and projected Secret")
			}
			caIssuer := objects["Issuer/"+issuer]
			caSecret, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "ca", "secretName")
			ca := objects["Certificate/orama-operator-control-plane-ca"]
			isCA, _, _ := unstructured.NestedBool(ca.Object, "spec", "isCA")
			rootSecret, _, _ := unstructured.NestedString(ca.Object, "spec", "secretName")
			if !isCA || caSecret != rootSecret || rootSecret != "orama-operator-control-plane-ca" {
				t.Fatal("serving certificate lacks a separate private CA")
			}
		})
	}
	for _, port := range []int{8080, 8081} {
		if _, err := renderDeploymentChart(t, port); err == nil {
			t.Fatal("chart allowed a deployment port reachable through unrestricted ingress")
		}
	}
}
