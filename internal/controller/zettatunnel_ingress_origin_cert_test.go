/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// testEdgeClientCASecret is a Secret holding an origin client CA in ca.crt, as reconcileEdgeClientCA
// writes it from the zone's origin-pull CA (orama-edge-origin-pull-<zone ID>).
func testEdgeClientCASecret(t *testing.T, namespace, name, subject string) (*corev1.Secret, []byte) {
	t.Helper()
	ca, err := newTunnelCA(subject, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"ca.crt": ca.CertPEM}}, ca.CertPEM
}

// TestIngressGatewayProjectsTheOriginClientCAOfAuthTLSHosts proves operator#1152 (following LSR-136,
// operator#798). A Lake host whose zone has authenticated origin pulls carries nginx's auth-tls
// annotations (applyLakeEdgeAnnotations): ingress-nginx refuses a caller without the edge's client
// certificate. zetta-tunnel's ingress gateway verifies a per-host client_ca (zetta-tunnel#34), so
// the host is projected with the same CA nginx verifies, mounted beside routes.json, instead of being
// refused. The requirement holds on every Ingress that names the host, whatever its class. What the
// gateway cannot check the way nginx does (optional verification, a missing CA, two CAs for one host)
// is still refused, and a host without the requirement gets no client_ca.
func TestIngressGatewayProjectsTheOriginClientCAOfAuthTLSHosts(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	edge := lakeEdge{originEncrypted: true, clientCA: "the zone's origin-pull CA"}

	lakeCA, lakeCAPEM := testEdgeClientCASecret(t, "lake-system", "lake-prod-edge-client-ca", "edge-a")
	bCA, bCAPEM := testEdgeClientCASecret(t, "lake-b", "lake-b-edge-client-ca", "edge-b")
	otherCA, _ := testEdgeClientCASecret(t, "lake-e", "other-ca", "edge-other")
	eCA, _ := testEdgeClientCASecret(t, "lake-e", "lake-e-edge-client-ca", "edge-e")

	// The Lake Ingress as the Lake controller writes it once the zone's origin pulls are on.
	lake := testOperatorIngress("lake-system", "lake-prod", "Lake", "nginx", "lake.example.com", "lake-tls", "lake-prod", 80, nil)
	applyLakeEdgeAnnotations(lake, edge, "lake-system/lake-prod-edge-client-ca")
	if lake.Annotations[annotationNginxAuthTLSVerifyClient] != "on" {
		t.Fatalf("the Lake Ingress does not require the edge's certificate: %v", lake.Annotations)
	}
	// Cut over to the gateway, with the same requirement; a CA Secret named without a namespace is
	// in the Ingress's own, as nginx reads it.
	cutOver := testOperatorIngress("lake-b", "lake-b", "Lake", "zetta-tunnel", "b.example.com", "b-tls", "lake-b", 80, nil)
	applyLakeEdgeAnnotations(cutOver, edge, "lake-b-edge-client-ca")
	// Optional verification lets a caller without a certificate through nginx; the gateway either
	// requires one or asks for none, so it refuses the host rather than serve it differently.
	optional := testOperatorIngress("lake-c", "lake-c", "Lake", "nginx", "c.example.com", "c-tls", "lake-c", 80, map[string]string{
		annotationNginxAuthTLSSecret: "lake-c/ca", annotationNginxAuthTLSVerifyClient: "optional",
	})
	// A CA Secret that does not exist yet: refused until it does.
	missing := testOperatorIngress("lake-d", "lake-d", "Lake", "nginx", "d.example.com", "d-tls", "lake-d", 80, nil)
	applyLakeEdgeAnnotations(missing, edge, "lake-d/lake-d-edge-client-ca")
	// Two Ingresses for one host naming different CAs: refused, never one of them or their union.
	conflictA := testOperatorIngress("lake-e", "lake-e", "Lake", "nginx", "e.example.com", "e-tls", "lake-e", 80, nil)
	applyLakeEdgeAnnotations(conflictA, edge, "lake-e/lake-e-edge-client-ca")
	conflictB := testOperatorIngress("lake-e", "lake-e-extra", "Lake", "nginx", "e.example.com", "e-tls", "lake-e", 80, nil)
	conflictB.Spec.Rules[0].HTTP.Paths[0].Path = "/extra"
	applyLakeEdgeAnnotations(conflictB, edge, "lake-e/other-ca")
	// A second operator Ingress for the protected host, without the annotations: ingress-nginx
	// applies auth-tls to the whole host, so its path is served under the host's client_ca too.
	companion := testOperatorIngress("lake-system", "lake-prod-extra", "Lake", "zetta-tunnel", "lake.example.com", "lake-tls", "lake-prod", 80, nil)
	companion.Spec.Rules[0].HTTP.Paths[0].Path = "/extra"
	// A host without the requirement is served without a client CA.
	open := testOperatorIngress("v1", "v1-dashboard", "OramaCloudV1", "zetta-tunnel", "app.example.com", "app-tls", "v1-dashboard", 3000, nil)

	objects := []client.Object{
		lake, cutOver, optional, missing, conflictA, conflictB, companion, open,
		lakeCA, bCA, otherCA, eCA,
		testTLSSecret(t, "lake-system", "lake-tls", "lake.example.com"),
		testTLSSecret(t, "lake-b", "b-tls", "b.example.com"),
		testTLSSecret(t, "lake-c", "c-tls", "c.example.com"),
		testTLSSecret(t, "lake-d", "d-tls", "d.example.com"),
		testTLSSecret(t, "lake-e", "e-tls", "e.example.com"),
		testTLSSecret(t, "v1", "app-tls", "app.example.com"),
		testBackend("lake-system", "lake-prod", 80, 8080),
		testBackend("lake-b", "lake-b", 80, 8080),
		testBackend("lake-c", "lake-c", 80, 8080),
		testBackend("lake-d", "lake-d", 80, 8080),
		testBackend("lake-e", "lake-e", 80, 8080),
		testBackend("v1", "v1-dashboard", 3000, 3000),
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&appsv1.Deployment{}, &oramav1alpha1.NetworkSandbox{}).Build()
	r := &zettaTunnelIngressReconciler{Client: kube, Scheme: scheme}
	if _, err := r.Reconcile(ctx, zettaTunnelIngressRequest); err != nil {
		t.Fatal(err)
	}

	// routes.json, decoded as zetta-tunnel reads it (its docs/ingress.md): client_ca per host.
	var routes corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressRoutes}, &routes); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Hosts []struct {
			Name     string            `json:"name"`
			ClientCA *string           `json:"client_ca"`
			Routes   []json.RawMessage `json:"routes"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(routes.Data[zettaTunnelRoutesKey], &document); err != nil {
		t.Fatal(err)
	}
	clientCA := map[string]*string{}
	routeCount := map[string]int{}
	var hosts []string
	for _, host := range document.Hosts {
		hosts = append(hosts, host.Name)
		clientCA[host.Name] = host.ClientCA
		routeCount[host.Name] = len(host.Routes)
	}
	if strings.Join(hosts, ",") != "app.example.com,b.example.com,lake.example.com" {
		t.Fatalf("served hosts = %v", hosts)
	}
	for host, want := range map[string][]byte{"lake.example.com": lakeCAPEM, "b.example.com": bCAPEM} {
		name := clientCA[host]
		if name == nil || *name != host+".client-ca.pem" {
			t.Fatalf("%s is projected without its client_ca: %v", host, name)
		}
		// The CA file is in the routes Secret, the volume the gateway mounts beside routes.json.
		if string(routes.Data[*name]) != string(want) {
			t.Fatalf("%s's client CA file is not the CA nginx verifies:\n%s", host, routes.Data[*name])
		}
	}
	if clientCA["app.example.com"] != nil {
		t.Fatalf("a host without the requirement got client_ca %q", *clientCA["app.example.com"])
	}
	if routeCount["lake.example.com"] != 2 {
		t.Fatalf("lake.example.com has %d routes; the companion's /extra must be served under the same client_ca", routeCount["lake.example.com"])
	}
	for name := range routes.Data {
		for _, refused := range []string{"c.example.com", "d.example.com", "e.example.com"} {
			if strings.HasPrefix(name, refused+".") {
				t.Fatalf("the gateway got a file of a refused host: %s", name)
			}
		}
	}

	// The gateway mounts the routes Secret, client CAs included, read-only and without subPath,
	// so client_ca resolves beside routes.json and a renewed CA arrives with its generation.
	var deployment appsv1.Deployment
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelIngressNamespace, Name: zettaTunnelIngressName}, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	if len(pod.Volumes) != 1 || pod.Volumes[0].Secret == nil || pod.Volumes[0].Secret.SecretName != zettaTunnelIngressRoutes ||
		len(pod.Containers[0].VolumeMounts) != 1 || pod.Containers[0].VolumeMounts[0].MountPath != "/etc/zetta-ingress" ||
		pod.Containers[0].VolumeMounts[0].SubPath != "" || !pod.Containers[0].VolumeMounts[0].ReadOnly {
		t.Fatalf("the client CA files are not mounted beside routes.json: %+v / %+v", pod.Volumes, pod.Containers[0].VolumeMounts)
	}

	projection, err := projectIngresses(ctx, kube)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lake-system/lake-prod", "lake-b/lake-b", "lake-system/lake-prod-extra", "v1/v1-dashboard"} {
		if !projection.Served[key] {
			t.Fatalf("%s is not served: %v", key, projection.Skipped)
		}
	}
	for _, key := range []string{"lake-c/lake-c", "lake-d/lake-d", "lake-e/lake-e", "lake-e/lake-e-extra"} {
		if projection.Served[key] {
			t.Fatalf("%s is reported served; it would get the gateway's address", key)
		}
	}
	skipped := strings.Join(projection.Skipped, "\n")
	for _, want := range []string{
		`lake-c/lake-c c.example.com: it verifies origin client certificates as "optional"`,
		"lake-d/lake-d d.example.com: its origin client CA Secret lake-d/lake-d-edge-client-ca does not exist yet",
		"lake-e/lake-e e.example.com: Ingresses for this host require client certificates from different CAs",
		"lake-e/lake-e-extra e.example.com: Ingresses for this host require client certificates from different CAs",
	} {
		if !strings.Contains(skipped, want) {
			t.Fatalf("skipped = %q, lacks %q", skipped, want)
		}
	}
	for _, backend := range projection.Backends {
		if backend.Namespace == "lake-c" || backend.Namespace == "lake-d" || backend.Namespace == "lake-e" {
			t.Fatalf("the gateway may reach a refused host's backend: %+v", backend)
		}
	}

	// Turning the requirement off (the zone's origin pulls switched off) serves the host without a
	// client CA.
	applyLakeEdgeAnnotations(lake, lakeEdge{originEncrypted: true}, "")
	if err := kube.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	projection, err = projectIngresses(ctx, kube)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range projection.Document.Hosts {
		if host.Name == "lake.example.com" && host.ClientCA != "" {
			t.Fatalf("lake.example.com keeps client_ca %q after the requirement is off", host.ClientCA)
		}
	}
	if _, ok := projection.Files["lake.example.com.client-ca.pem"]; ok {
		t.Fatal("the client CA file stays after the requirement is off")
	}
}
