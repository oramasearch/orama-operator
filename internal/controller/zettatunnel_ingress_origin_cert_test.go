// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestIngressGatewayNeverServesAHostThatRequiresOriginClientCertificates proves the rule
// (operator#798). A Lake host whose zone has authenticated origin pulls carries
// nginx's auth-tls annotations (applyLakeEdgeAnnotations): ingress-nginx refuses a caller without
// the edge's client certificate. The zetta-tunnel gateway verifies no client certificate, so
// projecting the host there would let anyone who finds the gateway's address skip the edge. The
// host is refused on every Ingress that names it, whatever the Ingress's class and whichever
// Ingress carries the annotations, and a cut-over protected Ingress never gets the gateway address.
func TestIngressGatewayNeverServesAHostThatRequiresOriginClientCertificates(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	edge := lakeEdge{originEncrypted: true, clientCA: "-----BEGIN CERTIFICATE-----\nedge\n-----END CERTIFICATE-----\n"}

	// The Lake Ingress as the Lake controller writes it once the zone's origin pulls are on.
	lake := testOperatorIngress("lake-system", "lake-prod", "Lake", "nginx", "lake.example.com", "lake-tls", "lake-prod", 80, nil)
	applyLakeEdgeAnnotations(lake, edge, "lake-system/lake-prod-edge-ca")
	if lake.Annotations[annotationNginxAuthTLSVerifyClient] != "on" {
		t.Fatalf("the Lake Ingress does not require the edge's certificate: %v", lake.Annotations)
	}
	// Cut over to the gateway, with the same requirement.
	cutOver := testOperatorIngress("lake-b", "lake-b", "Lake", "zetta-tunnel", "b.example.com", "b-tls", "lake-b", 80, nil)
	applyLakeEdgeAnnotations(cutOver, edge, "lake-b/lake-b-edge-ca")
	// Optional verification is still verification the gateway does not do.
	optional := testOperatorIngress("lake-c", "lake-c", "Lake", "nginx", "c.example.com", "c-tls", "lake-c", 80, map[string]string{
		annotationNginxAuthTLSSecret: "lake-c/ca", annotationNginxAuthTLSVerifyClient: "optional",
	})
	// A second operator Ingress for the protected host, without the annotations: ingress-nginx
	// applies auth-tls to the whole host, so the gateway must not serve its path either.
	companion := testOperatorIngress("lake-system", "lake-prod-extra", "Lake", "zetta-tunnel", "lake.example.com", "lake-tls", "lake-prod", 80, nil)
	companion.Spec.Rules[0].HTTP.Paths[0].Path = "/extra"
	// A host without the requirement is still served.
	open := testOperatorIngress("v1", "v1-dashboard", "OramaCloudV1", "zetta-tunnel", "app.example.com", "app-tls", "v1-dashboard", 3000, nil)

	objects := []client.Object{
		lake, cutOver, optional, companion, open,
		testTLSSecret(t, "lake-system", "lake-tls", "lake.example.com"),
		testTLSSecret(t, "lake-b", "b-tls", "b.example.com"),
		testTLSSecret(t, "lake-c", "c-tls", "c.example.com"),
		testTLSSecret(t, "v1", "app-tls", "app.example.com"),
		testBackend("lake-system", "lake-prod", 80, 8080),
		testBackend("lake-b", "lake-b", 80, 8080),
		testBackend("lake-c", "lake-c", 80, 8080),
		testBackend("v1", "v1-dashboard", 3000, 3000),
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	projection, err := projectIngresses(ctx, kube)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, host := range projection.Document.Hosts {
		hosts = append(hosts, host.Name)
	}
	if strings.Join(hosts, ",") != "app.example.com" {
		t.Fatalf("served hosts = %v; a host that requires origin client certificates reached the gateway", hosts)
	}
	for name := range projection.Files {
		if !strings.HasPrefix(name, "app.example.com.") {
			t.Fatalf("the gateway got the key of a protected host: %s", name)
		}
	}
	for _, key := range []string{"lake-system/lake-prod", "lake-b/lake-b", "lake-c/lake-c", "lake-system/lake-prod-extra"} {
		if projection.Served[key] {
			t.Fatalf("%s is reported served; it would get the gateway's address", key)
		}
	}
	if !projection.Served["v1/v1-dashboard"] {
		t.Fatal("an unprotected host is no longer served")
	}
	skipped := strings.Join(projection.Skipped, "\n")
	for _, want := range []string{
		"lake-system/lake-prod lake.example.com: it requires origin client certificates",
		"lake-b/lake-b b.example.com: it requires origin client certificates",
		"lake-c/lake-c c.example.com: it requires origin client certificates",
		"lake-system/lake-prod-extra lake.example.com: it requires origin client certificates",
	} {
		if !strings.Contains(skipped, want) {
			t.Fatalf("skipped = %q, lacks %q", skipped, want)
		}
	}
	for _, backend := range projection.Backends {
		if backend.Namespace != "v1" {
			t.Fatalf("the gateway may reach a protected host's backend: %+v", backend)
		}
	}

	// Turning the requirement off (the zone's origin pulls switched off) serves the host again.
	applyLakeEdgeAnnotations(lake, lakeEdge{originEncrypted: true}, "")
	if err := kube.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	projection, err = projectIngresses(ctx, kube)
	if err != nil {
		t.Fatal(err)
	}
	if !projection.Served["lake-system/lake-prod"] || !projection.Served["lake-system/lake-prod-extra"] {
		t.Fatalf("an unprotected Lake host is not served: %v / %v", projection.Served, projection.Skipped)
	}
}
