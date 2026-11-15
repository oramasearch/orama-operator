/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func testOperatorIngress(namespace, name, kind, class, host, secret, service string, port int32, annotations map[string]string) *networkingv1.Ingress {
	prefix := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			Rules: []networkingv1.IngressRule{{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &prefix, Backend: networkingv1.IngressBackend{
					Service: &networkingv1.IngressServiceBackend{Name: service, Port: networkingv1.ServiceBackendPort{Number: port}},
				}}},
			}}}},
		},
	}
	if kind != "" {
		ingress.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: oramav1alpha1.GroupVersion.String(), Kind: kind, Name: name, UID: types.UID(name), Controller: boolPtr(true),
		}}
	}
	if secret != "" {
		ingress.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: secret}}
	}
	return ingress
}

func testTLSSecret(t *testing.T, namespace, name, host string) *corev1.Secret {
	t.Helper()
	now := time.Now()
	ca, err := newTunnelCA("test", now)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := issueTunnelLeaf(ca, tunnelLeafRequest{DNSNames: []string{host}, Server: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: key}}
}

func testBackend(namespace, name string, port, target int32) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": name}, Ports: []corev1.ServicePort{{Name: "http", Port: port, TargetPort: intstr.FromInt32(target)}}}}
}

func TestIngressGatewayServesEveryOperatorHostItCanAndRefusesTheRest(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	objects := []client.Object{
		// Lake, cut over, with its certificate: served, with nginx's 64m body limit.
		testOperatorIngress("lake-system", "lake-prod", "Lake", "zetta-tunnel", "lake.example.com", "lake-tls", "lake-prod", 80,
			map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": "64m"}),
		testTLSSecret(t, "lake-system", "lake-tls", "lake.example.com"),
		testBackend("lake-system", "lake-prod", 80, 8080),
		// A Zetta host still on nginx, with a certificate: served too, so a cutover only moves DNS.
		testOperatorIngress("v1", "v1-dashboard", "OramaZettaCluster", "nginx", "app.example.com", "app-tls", "v1-dashboard", 3000, nil),
		testTLSSecret(t, "v1", "app-tls", "app.example.com"),
		testBackend("v1", "v1-dashboard", 3000, 3000),
		// Zetta without a certificate yet (#457): refused, never served over plain HTTP.
		testOperatorIngress("zetta", "zetta", "OramaZettaCluster", "zetta-tunnel", "zetta.example.com", "", "zetta", 4800, nil),
		testBackend("zetta", "zetta", 4800, 4800),
		// A Lake app: stays on nginx (auth subrequest, metering).
		testOperatorIngress("apps", "app", "LakeApp", "nginx", "app1.apps.example.com", "app1-tls", "app", 8080,
			map[string]string{"nginx.ingress.kubernetes.io/auth-url": "http://lake/auth"}),
		testTLSSecret(t, "apps", "app1-tls", "app1.apps.example.com"),
		// Not the operator's: ignored altogether.
		testOperatorIngress("other", "foreign", "", "nginx", "foreign.example.com", "foreign-tls", "foreign", 80, nil),
		testTLSSecret(t, "other", "foreign-tls", "foreign.example.com"),
		// The same host claimed by a second Ingress with another certificate: refused.
		testOperatorIngress("v2", "v2-dashboard", "OramaZettaCluster", "zetta-tunnel", "app.example.com", "other-tls", "v2-dashboard", 3000, nil),
		testTLSSecret(t, "v2", "other-tls", "app.example.com"),
		testBackend("v2", "v2-dashboard", 3000, 3000),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orama-operator-abc", Namespace: "orama-system"},
			Spec: corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "oramasearch-dockerhub"}}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oramasearch-dockerhub", Namespace: "orama-system"}, Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&networkingv1.Ingress{}, &corev1.Service{}, &appsv1.Deployment{}, &oramav1alpha1.NetworkSandbox{}).
		WithObjects(objects...).Build()
	t.Setenv("HOSTNAME", "orama-operator-abc")
	r := &zettaTunnelIngressReconciler{Client: namespaceCacheGuard{kube}, APIReader: kube, Scheme: scheme, OperatorNamespace: "orama-system"}
	reconcile := func() {
		t.Helper()
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(ctx, zettaTunnelIngressRequest); err != nil {
				t.Fatal(err)
			}
		}
	}
	reconcile()

	var routes corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressRoutes}, &routes); err != nil {
		t.Fatal(err)
	}
	var document zettaIngressDocument
	if err := decodeStrict(routes.Data[zettaTunnelRoutesKey], &document); err != nil {
		t.Fatalf("routes.json is not zetta-tunnel's exact projection: %v", err)
	}
	var hosts []string
	for _, host := range document.Hosts {
		hosts = append(hosts, host.Name)
	}
	if !slices.Equal(hosts, []string{"app.example.com", "lake.example.com"}) {
		t.Fatalf("served hosts = %v", hosts)
	}
	lake := document.Hosts[1]
	if lake.Certificate != "lake.example.com.crt" || lake.Key != "lake.example.com.key" || len(lake.Routes) != 1 ||
		lake.Routes[0].Target != "http://lake-prod.lake-system.svc.cluster.local:80" || lake.Routes[0].MaxBodyBytes != 64<<20 ||
		lake.Routes[0].Path != "/" || lake.Routes[0].PathType != "Prefix" || !strings.HasPrefix(lake.Routes[0].ID, "r-") {
		t.Fatalf("Lake host = %+v", lake)
	}
	if document.Hosts[0].Routes[0].MaxBodyBytes != nginxDefaultBodyBytes {
		t.Fatal("a route without a body-size annotation lost nginx's default limit")
	}
	if len(routes.Data["lake.example.com.crt"]) == 0 || len(routes.Data["lake.example.com.key"]) == 0 || len(routes.Data["foreign.example.com.crt"]) != 0 {
		t.Fatal("the certificate files beside routes.json are wrong")
	}

	var report corev1.ConfigMap
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressReport}, &report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zetta/zetta zetta.example.com: no TLS certificate", "apps/app *: Lake apps stay on ingress-nginx", "v2/v2-dashboard app.example.com: another Ingress serves this host"} {
		if !strings.Contains(report.Data["skipped"], want) {
			t.Fatalf("report skipped = %q, lacks %q", report.Data["skipped"], want)
		}
	}
	if report.Data["on-nginx"] != "apps/app\nv1/v1-dashboard" {
		t.Fatalf("report on-nginx = %q", report.Data["on-nginx"])
	}

	var deployment appsv1.Deployment
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressName}, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	args := strings.Join(pod.Containers[0].Args, " ")
	if *deployment.Spec.Replicas != 2 || !strings.HasPrefix(args, "ingress --configuration /etc/zetta-ingress/routes.json --listen 0.0.0.0:8443") ||
		pod.ImagePullSecrets[0].Name != "oramasearch-dockerhub" || *pod.Containers[0].SecurityContext.AllowPrivilegeEscalation ||
		pod.Volumes[0].Secret.SecretName != zettaTunnelIngressRoutes || pod.Containers[0].VolumeMounts[0].SubPath != "" {
		t.Fatalf("gateway Deployment = %+v / %s", pod, args)
	}
	var copied corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: "oramasearch-dockerhub"}, &copied); err != nil {
		t.Fatalf("the operator's pull secret was not copied: %v", err)
	}
	var class networkingv1.IngressClass
	if err := kube.Get(ctx, client.ObjectKey{Name: zettaTunnelIngressClass}, &class); err != nil || class.Spec.Controller != zettaTunnelIngressController {
		t.Fatalf("IngressClass = %+v (%v)", class.Spec, err)
	}

	// The gateway reaches only the served backends' pods and ports; the public reaches only 8443.
	var egress networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: "gateway-backend-egress"}, &egress); err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, rule := range egress.Spec.Egress[1:] {
		targets = append(targets, rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]+"/"+rule.To[0].PodSelector.MatchLabels["app"]+":"+rule.Ports[0].Port.String())
	}
	if !slices.Equal(targets, []string{"lake-system/lake-prod:8080", "v1/v1-dashboard:3000"}) {
		t.Fatalf("gateway egress targets = %v", targets)
	}

	// No address until the load balancer has one and the gateway is available; then only served
	// Ingresses of class zetta-tunnel get it, so their DNS follows. nginx's Ingresses are not touched.
	address := func(namespace, name string) []networkingv1.IngressLoadBalancerIngress {
		t.Helper()
		var ingress networkingv1.Ingress
		if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &ingress); err != nil {
			t.Fatal(err)
		}
		return ingress.Status.LoadBalancer.Ingress
	}
	if len(address("lake-system", "lake-prod")) != 0 {
		t.Fatal("an address was given before the gateway could serve")
	}
	var service corev1.Service
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressName}, &service); err != nil {
		t.Fatal(err)
	}
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "20.9.9.9"}}
	if err := kube.Status().Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ObservedGeneration, deployment.Status.AvailableReplicas = deployment.Generation, 2
	if err := kube.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if got := address("lake-system", "lake-prod"); len(got) != 1 || got[0].IP != "20.9.9.9" {
		t.Fatalf("cut-over Lake address = %+v", got)
	}
	for _, key := range [][2]string{{"zetta", "zetta"}, {"v2", "v2-dashboard"}, {"v1", "v1-dashboard"}} {
		if got := address(key[0], key[1]); len(got) != 0 {
			t.Fatalf("%s/%s got an address it must not have: %+v", key[0], key[1], got)
		}
	}
	// A certificate lost later takes the address away again: fail closed.
	if err := kube.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "lake-tls", Namespace: "lake-system"}}); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if got := address("lake-system", "lake-prod"); len(got) != 0 {
		t.Fatalf("a host without its certificate kept the gateway address: %+v", got)
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressRoutes}, &routes); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(routes.Data[zettaTunnelRoutesKey], &document); err != nil || len(document.Hosts) != 1 {
		t.Fatalf("routes after the certificate went = %s", routes.Data[zettaTunnelRoutesKey])
	}
}

func TestIngressGatewayIsScaledToZeroWithNothingToServe(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.Deployment{}, &oramav1alpha1.NetworkSandbox{}).Build()
	r := &zettaTunnelIngressReconciler{Client: kube, Scheme: scheme}
	if _, err := r.Reconcile(ctx, zettaTunnelIngressRequest); err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressName}, &deployment); err != nil {
		t.Fatal(err)
	}
	var service corev1.Service
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressName}, &service); err != nil {
		t.Fatal(err)
	}
	if *deployment.Spec.Replicas != 0 || service.Spec.Type != corev1.ServiceTypeLoadBalancer ||
		service.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyTypeLocal {
		t.Fatalf("an empty gateway: replicas %d, Service %+v", *deployment.Spec.Replicas, service.Spec)
	}
	var namespace corev1.Namespace
	if err := kube.Get(ctx, client.ObjectKey{Name: zettaTunnelIngressNamespace}, &namespace); err != nil ||
		namespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("gateway namespace = %+v (%v)", namespace.Labels, err)
	}
}

func TestIngressBodyLimitsFollowNginx(t *testing.T) {
	for value, want := range map[string]int64{"": nginxDefaultBodyBytes, "0": 0, "64m": 64 << 20, "512k": 512 << 10, "2g": 2 << 30, "100": 100} {
		ingress := &networkingv1.Ingress{}
		if value != "" {
			ingress.Annotations = map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": value}
		}
		got, err := ingressBodyLimit(ingress)
		if err != nil || got != want {
			t.Fatalf("%q: %d (%v), want %d", value, got, err, want)
		}
	}
	if _, err := ingressBodyLimit(&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": "lots"}}}); err == nil {
		t.Fatal("a malformed size was accepted")
	}
}

func TestBackendsAdmitTheGatewayBesideNginx(t *testing.T) {
	peer := zettaTunnelIngressPeer()
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != zettaTunnelIngressNamespace ||
		peer.PodSelector.MatchLabels[zettaTunnelComponentKey] != "ingress" {
		t.Fatalf("gateway peer = %+v", peer)
	}
	if ingressClassOrDefault("") != "nginx" || ingressClassOrDefault("zetta-tunnel") != "zetta-tunnel" {
		t.Fatal("the ingress class default changed")
	}
}
