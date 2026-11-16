// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// envNamed returns the env entry called name, or nil.
func envNamed(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

// HPES runs in the stack whenever ingestion runs: a CPU Deployment and a Service on 8787 with the
// overridden image, requiring the generated bearer that index-builder sends to it at
// http://<cr>-hpes:8787/v1/embeddings_b64 (#574).
func TestHPESServesIndexBuilderInTheStack(t *testing.T) {
	const pinned = "oramasearch/embeddings-server-py@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	h := newEngineHarness(t, stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides = map[string]string{hpesComponent: pinned}
	}), engineSecret())
	h.mustRun()

	tok := h.secret("v1-hpes")
	token := string(tok.Data[hpesTokenKey])
	if !hex64.MatchString(token) {
		t.Fatal("v1-hpes token is not 64 hex characters (32 random bytes)")
	}
	if len(tok.OwnerReferences) != 1 || tok.OwnerReferences[0].Name != "v1" || tok.Labels[componentLabel] != credentialsComponent {
		t.Errorf("v1-hpes owner %+v, component %q", tok.OwnerReferences, tok.Labels[componentLabel])
	}

	dep := &appsv1.Deployment{}
	if !h.get("v1-hpes", dep) {
		t.Fatal("Deployment v1-hpes not found")
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *dep.Spec.Replicas)
	}
	pod := dep.Spec.Template.Spec
	c := containerIn(t, pod, hpesComponent)
	if c.Image != pinned {
		t.Errorf("image = %q, want the override %q", c.Image, pinned)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8787 {
		t.Errorf("ports = %+v, want 8787 only", c.Ports)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntVal != 8787 {
		t.Errorf("readiness = %+v, want TCP on 8787 (/health is behind the bearer)", c.ReadinessProbe)
	}
	// CPU only: no GPU request or limit, no GPU node selector or toleration.
	for name := range c.Resources.Requests {
		if name != corev1.ResourceMemory && name != corev1.ResourceCPU && name != corev1.ResourceEphemeralStorage {
			t.Errorf("HPES requests unexpected resource %s", name)
		}
	}
	for name := range c.Resources.Limits {
		if name != corev1.ResourceMemory && name != corev1.ResourceEphemeralStorage {
			t.Errorf("HPES limits unexpected resource %s", name)
		}
	}
	if len(pod.NodeSelector) != 0 || len(pod.Tolerations) != 0 {
		t.Errorf("HPES has node selector %v / tolerations %v; it runs on the stack's ordinary nodes", pod.NodeSelector, pod.Tolerations)
	}
	storageRequest, storageLimit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]
	if storageRequest.Cmp(resource.MustParse("100Mi")) != 0 || storageLimit.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("HPES storage request=%s limit=%s", storageRequest.String(), storageLimit.String())
	}
	if got := c.Resources.Requests.Memory().String() + "/" + c.Resources.Requests.Cpu().String() + "/" + c.Resources.Limits.Memory().String(); got != "7Gi/2/10Gi" {
		t.Errorf("resources = %s, want 7Gi/2/10Gi", got)
	}
	env := expandEnv(t, c.Env, map[string]*corev1.Secret{tok.Name: tok})
	want := map[string]string{
		"API_KEY":             token,
		"EXECUTION_PROVIDERS": "CPUExecutionProvider",
		"MODELS_GROUP":        "all",
		"HOST":                "0.0.0.0",
		"HTTP_PORT":           "8787",
		"TOTAL_THREADS":       "4",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("HPES %s = %q, want %q", k, env[k], v)
		}
	}
	if api := envNamed(c.Env, "API_KEY"); api == nil || api.Value != "" || api.ValueFrom == nil || api.ValueFrom.SecretKeyRef == nil ||
		api.ValueFrom.SecretKeyRef.Name != "v1-hpes" || api.ValueFrom.SecretKeyRef.Key != hpesTokenKey {
		t.Errorf("HPES API_KEY = %+v, want a secretKeyRef to v1-hpes/token", api)
	}
	if dep.Spec.Template.Annotations[credentialsHashKey] == "" {
		t.Error("HPES's template carries no credentials hash, so a new bearer would not roll it")
	}

	svc := h.service("v1-hpes")
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8787 || svc.Spec.Ports[0].TargetPort.IntVal != 8787 {
		t.Errorf("Service ports = %+v, want 8787", svc.Spec.Ports)
	}
	if svc.Spec.Type != "" && svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %s, want ClusterIP: HPES has no public host", svc.Spec.Type)
	}
	if h.get("v1-hpes", &networkingv1.Ingress{}) {
		t.Error("HPES has an Ingress; it has no public host")
	}

	// index-builder calls it with the same bearer.
	ib := &appsv1.Deployment{}
	if !h.get("v1-index-builder", ib) {
		t.Fatal("Deployment v1-index-builder not found")
	}
	ibc := containerIn(t, ib.Spec.Template.Spec, "index-builder")
	if u := envNamed(ibc.Env, "ORAMA_EMBEDDINGS_URL"); u == nil || u.Value != "http://v1-hpes:8787/v1/embeddings_b64" {
		t.Errorf("index-builder ORAMA_EMBEDDINGS_URL = %+v, want http://v1-hpes:8787/v1/embeddings_b64", u)
	}
	if b := envNamed(ibc.Env, "ORAMA_EMBEDDINGS_BEARER"); b == nil || b.Value != "" || b.ValueFrom == nil || b.ValueFrom.SecretKeyRef == nil ||
		b.ValueFrom.SecretKeyRef.Name != "v1-hpes" || b.ValueFrom.SecretKeyRef.Key != hpesTokenKey {
		t.Errorf("index-builder ORAMA_EMBEDDINGS_BEARER = %+v, want a secretKeyRef to v1-hpes/token", b)
	}
	// No dev-* placeholder is seeded for the bearer any more.
	if _, ok := h.secret("v1-index-builder-secret").Data["ORAMA_EMBEDDINGS_BEARER"]; ok {
		t.Error("v1-index-builder-secret still seeds ORAMA_EMBEDDINGS_BEARER")
	}

	// The bearer is never rotated by the reconcile.
	h.mustRun()
	if got := string(h.secret("v1-hpes").Data[hpesTokenKey]); got != token {
		t.Error("a second reconcile changed the bearer")
	}
}

// A new bearer rolls both sides: HPES and index-builder carry its hash on their templates.
func TestHPESBearerChangeRollsHPESAndIndexBuilder(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	before := h.templates()

	sec := h.secret("v1-hpes")
	sec.Data[hpesTokenKey] = []byte("0000000000000000000000000000000000000000000000000000000000000000")
	if err := h.cl.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	h.mustRun()
	after := h.templates()
	for _, comp := range []string{hpesComponent, "index-builder"} {
		if before[comp].Annotations[credentialsHashKey] == after[comp].Annotations[credentialsHashKey] {
			t.Errorf("%s did not roll when the HPES bearer changed", comp)
		}
	}
	if before["dashboard"].Annotations[credentialsHashKey] != after["dashboard"].Annotations[credentialsHashKey] {
		t.Error("the dashboard rolled on the HPES bearer, which it does not read")
	}
	if !reflect.DeepEqual(secretsReadBy(stackOC(nil))[:5], []string{"v1-broker-admin", "v1-broker-users", "v1-cache-auth", "v1-redis-http", "v1-hpes"}) {
		t.Errorf("secretsReadBy does not map v1-hpes back to the CR: %v", secretsReadBy(stackOC(nil)))
	}
}

// A value set before the first reconcile is kept, and hpesResources replaces the default resources.
func TestHPESKeepsASuppliedBearerAndTakesResources(t *testing.T) {
	supplied := secretOf("v1-hpes", map[string]string{hpesTokenKey: "supplied-bearer"})
	h := newEngineHarness(t, stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Ingestion.HPESResources = resourceReqs("3Gi", "1", "4Gi")
	}), engineSecret(), supplied)
	h.mustRun()
	if got := string(h.secret("v1-hpes").Data[hpesTokenKey]); got != "supplied-bearer" {
		t.Errorf("bearer = %q, want the supplied value kept", got)
	}
	dep := &appsv1.Deployment{}
	h.get("v1-hpes", dep)
	c := containerIn(t, dep.Spec.Template.Spec, hpesComponent)
	if got := c.Resources.Requests.Memory().String() + "/" + c.Resources.Requests.Cpu().String() + "/" + c.Resources.Limits.Memory().String(); got != "3Gi/1/4Gi" {
		t.Errorf("resources = %s, want the override 3Gi/1/4Gi", got)
	}
	if c.Image != "oramasearch/embeddings-server-py:latest" {
		t.Errorf("image without an override = %q, want componentImage's oramasearch/embeddings-server-py:latest", c.Image)
	}
}

// Without ingestion there is no HPES and no bearer; turning ingestion off removes the HPES it had
// and keeps the bearer for its return.
func TestHPESFollowsIngestion(t *testing.T) {
	noIngestion := func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Ingestion = oramav1alpha1.IngestionSpec{} }
	h := newEngineHarness(t, stackOC(noIngestion), engineSecret())
	h.mustRun()
	if h.get("v1-hpes", &appsv1.Deployment{}) || h.get("v1-hpes", &corev1.Secret{}) {
		t.Fatal("HPES or its bearer exists without ingestion")
	}

	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Ingestion = oramav1alpha1.IngestionSpec{Enabled: true}
	})
	h.mustRun()
	if !h.get("v1-hpes", &appsv1.Deployment{}) {
		t.Fatal("enabling ingestion did not start HPES")
	}
	token := string(h.secret("v1-hpes").Data[hpesTokenKey])

	h.updateCR(noIngestion)
	h.mustRun()
	if h.get("v1-hpes", &appsv1.Deployment{}) || h.get("v1-hpes", &corev1.Service{}) {
		t.Error("disabling ingestion left HPES running")
	}
	if got := string(h.secret("v1-hpes").Data[hpesTokenKey]); got != token {
		t.Error("disabling ingestion changed or removed the bearer")
	}
}

// Only index-builder reaches HPES, on 8787; HPES reaches nothing in the stack (#574).
func TestHPESNetworkPaths(t *testing.T) {
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	stack := newOCV1(nil)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{stack}, sandboxDiscoveryObjects()...)...).Build()
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
	if err := r.reconcileNetworkSandbox(ctx, stack); err != nil {
		t.Fatalf("reconcile Cloud v1 NetworkSandbox: %v", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, &sandbox)

	components := []string{"dashboard", "workers", "index-builder", "harvest-master", "writer", "reader", "docs",
		"disco", "analytics-processor", "pgadmin", "keycloak", "index-shared-worker", "cache", redisHTTPComponent,
		"search", "controldb", "openai-decrypter", hpesComponent}
	peerPorts := func(self, other string, egress bool) []int32 {
		var ports []int32
		for _, a := range sandbox.Spec.Policies {
			if !selects(t, a.Spec.PodSelector, labelsFor(stack, self)) {
				continue
			}
			if egress {
				for _, rule := range a.Spec.Egress {
					for _, peer := range rule.To {
						if peer.PodSelector != nil && selects(t, *peer.PodSelector, labelsFor(stack, other)) {
							ports = append(ports, policyPorts(t, rule.Ports)...)
						}
					}
				}
				continue
			}
			for _, rule := range a.Spec.Ingress {
				for _, peer := range rule.From {
					if peer.PodSelector != nil && selects(t, *peer.PodSelector, labelsFor(stack, other)) {
						ports = append(ports, policyPorts(t, rule.Ports)...)
					}
				}
			}
		}
		return ports
	}
	for _, other := range components {
		want := []int32(nil)
		if other == "index-builder" {
			want = []int32{8787}
		}
		if got := peerPorts(hpesComponent, other, false); !reflect.DeepEqual(got, want) {
			t.Errorf("HPES admits %s on %v, want %v", other, got, want)
		}
		if other == hpesComponent {
			continue
		}
		if got := peerPorts(other, hpesComponent, true); !reflect.DeepEqual(got, want) {
			t.Errorf("%s may send to HPES on %v, want %v", other, got, want)
		}
		if got := peerPorts(hpesComponent, other, true); len(got) > 0 {
			t.Errorf("HPES may send to %s on %v", other, got)
		}
	}
	// HPES has no egress allowance of its own (DNS is the stack-wide allow-dns-egress), and no
	// public-internet allowance: its models are in the image.
	for _, a := range sandbox.Spec.Policies {
		if len(a.Spec.Egress) == 0 || a.Name == "allow-dns-egress" {
			continue
		}
		if selects(t, a.Spec.PodSelector, labelsFor(stack, hpesComponent)) {
			t.Errorf("egress allowance %s selects HPES", a.Name)
		}
	}
	allowanceByName(t, &sandbox, "allow-hpes-ingress")
}
