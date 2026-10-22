/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The shim runs beside the cache: its token Secret is generated once and kept, its Deployment
// connects to the cache with the generated password and requires the token, its Service listens on
// 80, and the dashboard's Upstash client is pointed at it with the same token (#562).
func TestRedisHTTPShimServesTheDashboardsUpstashClient(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()

	tok := h.secret("v1-redis-http")
	token := string(tok.Data[redisHTTPTokenKey])
	if !hex64.MatchString(token) {
		t.Fatalf("v1-redis-http token is not 64 hex characters (32 random bytes)")
	}
	if len(tok.OwnerReferences) != 1 || tok.OwnerReferences[0].Name != "v1" || tok.Labels[componentLabel] != credentialsComponent {
		t.Errorf("v1-redis-http owner %+v, component %q", tok.OwnerReferences, tok.Labels[componentLabel])
	}
	cache := h.secret("v1-cache-auth")
	secrets := map[string]*corev1.Secret{tok.Name: tok, cache.Name: cache, testMasterSecret: h.secret(testMasterSecret)}

	dep := &appsv1.Deployment{}
	if !h.get("v1-redis-http", dep) {
		t.Fatal("Deployment v1-redis-http not found")
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *dep.Spec.Replicas)
	}
	c := containerIn(t, dep.Spec.Template.Spec, redisHTTPComponent)
	if c.Image != redisHTTPImage {
		t.Errorf("image = %q, want %q", c.Image, redisHTTPImage)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 80 {
		t.Errorf("ports = %+v, want 80", c.Ports)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntVal != 80 {
		t.Errorf("readiness = %+v, want TCP on 80", c.ReadinessProbe)
	}
	if c.SecurityContext == nil || !slices.Contains(c.SecurityContext.Capabilities.Add, "NET_BIND_SERVICE") {
		t.Errorf("a port-80 container needs NET_BIND_SERVICE: %+v", c.SecurityContext)
	}
	if got := c.Resources.Requests.Memory().String() + "/" + c.Resources.Requests.Cpu().String() + "/" + c.Resources.Limits.Memory().String(); got != "64Mi/50m/256Mi" {
		t.Errorf("resources = %s, want 64Mi/50m/256Mi", got)
	}
	names := make([]string, 0, len(c.Env))
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	if !reflect.DeepEqual(names, []string{"REDIS_PASSWORD", "SRH_MODE", "SRH_TOKEN", "SRH_CONNECTION_STRING"}) {
		t.Errorf("env order = %v", names)
	}
	env := expandEnv(t, c.Env, secrets)
	if env["SRH_MODE"] != "env" || env["SRH_TOKEN"] != token {
		t.Errorf("SRH_MODE %q, SRH_TOKEN is the token: %v", env["SRH_MODE"], env["SRH_TOKEN"] == token)
	}
	if env["SRH_CONNECTION_STRING"] != "redis://:"+string(cache.Data["password"])+"@v1-cache:6379" {
		t.Error("SRH_CONNECTION_STRING does not expand to the cache password")
	}
	for _, e := range c.Env {
		if e.Value != "" && (e.Value == token || e.Value == string(cache.Data["password"])) {
			t.Errorf("%s holds a credential literally", e.Name)
		}
	}
	if dep.Spec.Template.Annotations[credentialsHashKey] == "" {
		t.Error("the shim's template carries no credentials hash, so a rotation would not roll it")
	}

	svc := h.service("v1-redis-http")
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntVal != 80 {
		t.Errorf("Service ports = %+v, want 80", svc.Spec.Ports)
	}

	dash := containerIn(t, h.dashboard().Spec.Template.Spec, "dashboard")
	var url, ref *corev1.EnvVar
	for i := range dash.Env {
		switch dash.Env[i].Name {
		case "REDIS_CACHE_URL":
			url = &dash.Env[i]
		case "REDIS_CACHE_TOKEN":
			ref = &dash.Env[i]
		}
	}
	if url == nil || url.Value != "http://v1-redis-http" {
		t.Errorf("dashboard REDIS_CACHE_URL = %+v, want http://v1-redis-http", url)
	}
	if ref == nil || ref.ValueFrom == nil || ref.ValueFrom.SecretKeyRef == nil ||
		ref.ValueFrom.SecretKeyRef.Name != "v1-redis-http" || ref.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("dashboard REDIS_CACHE_TOKEN = %+v, want a secretKeyRef to v1-redis-http/token", ref)
	}

	// The token is never rotated by the reconcile.
	h.mustRun()
	if got := string(h.secret("v1-redis-http").Data[redisHTTPTokenKey]); got != token {
		t.Error("a second reconcile changed the token")
	}

	// An override replaces the image, as for every other component.
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides = map[string]string{redisHTTPComponent: "mirror.example/srh@sha256:" + token}
	})
	h.mustRun()
	h.get("v1-redis-http", dep)
	if got := containerIn(t, dep.Spec.Template.Spec, redisHTTPComponent).Image; got != "mirror.example/srh@sha256:"+token {
		t.Errorf("override image = %q", got)
	}
}

// The default image is a private oramasearch repository pinned by digest, never a tag.
func TestRedisHTTPImageIsPrivateAndPinned(t *testing.T) {
	if !regexp.MustCompile(`^oramasearch/serverless-redis-http@sha256:[0-9a-f]{64}$`).MatchString(redisHTTPImage) {
		t.Errorf("redisHTTPImage %q is not oramasearch/serverless-redis-http pinned by digest", redisHTTPImage)
	}
}

// Without an in-cluster cache there is no shim, no token and no Upstash variables on the dashboard;
// turning the cache off removes the shim the cache had and keeps the token for its return.
func TestRedisHTTPShimFollowsTheCache(t *testing.T) {
	noCache := func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{} }
	h := newEngineHarness(t, stackOC(noCache), engineSecret())
	h.mustRun()
	if h.get("v1-redis-http", &appsv1.Deployment{}) || h.get("v1-redis-http", &corev1.Secret{}) {
		t.Fatal("the shim or its token exists without a cache")
	}
	for _, e := range containerIn(t, h.dashboard().Spec.Template.Spec, "dashboard").Env {
		if e.Name == "REDIS_CACHE_URL" || e.Name == "REDIS_CACHE_TOKEN" {
			t.Errorf("dashboard has %s without a cache", e.Name)
		}
	}

	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
	})
	h.mustRun()
	if !h.get("v1-redis-http", &appsv1.Deployment{}) {
		t.Fatal("enabling the cache did not start the shim")
	}
	token := string(h.secret("v1-redis-http").Data[redisHTTPTokenKey])

	h.updateCR(noCache)
	h.mustRun()
	if h.get("v1-redis-http", &appsv1.Deployment{}) || h.get("v1-redis-http", &corev1.Service{}) {
		t.Error("disabling the cache left the shim running")
	}
	if got := string(h.secret("v1-redis-http").Data[redisHTTPTokenKey]); got != token {
		t.Error("disabling the cache changed or removed the token")
	}
}

// Only the dashboard reaches the shim, on 80; the shim reaches the cache on 6379 and the cache admits
// it. No other component, workers included, gets a path to the shim (#562).
func TestRedisHTTPNetworkPaths(t *testing.T) {
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
		"disco", "analytics-processor", "pgadmin", "keycloak", "index-shared-worker", "cache", redisHTTPComponent}
	// egress and ingress are the ports any allowance opens from one component to another.
	egress := func(from, to string) []int32 {
		var ports []int32
		for _, a := range sandbox.Spec.Policies {
			if !selects(t, a.Spec.PodSelector, labelsFor(stack, from)) {
				continue
			}
			for _, rule := range a.Spec.Egress {
				for _, peer := range rule.To {
					if peer.PodSelector != nil && selects(t, *peer.PodSelector, labelsFor(stack, to)) {
						ports = append(ports, policyPorts(t, rule.Ports)...)
					}
				}
			}
		}
		return ports
	}
	ingress := func(to, from string) []int32 {
		var ports []int32
		for _, a := range sandbox.Spec.Policies {
			if !selects(t, a.Spec.PodSelector, labelsFor(stack, to)) {
				continue
			}
			for _, rule := range a.Spec.Ingress {
				for _, peer := range rule.From {
					if peer.PodSelector != nil && selects(t, *peer.PodSelector, labelsFor(stack, from)) {
						ports = append(ports, policyPorts(t, rule.Ports)...)
					}
				}
			}
		}
		return ports
	}
	for _, from := range components {
		want := []int32(nil)
		if from == "dashboard" {
			want = []int32{80}
		}
		if got := ingress(redisHTTPComponent, from); !reflect.DeepEqual(got, want) {
			t.Errorf("the shim admits %s on %v, want %v", from, got, want)
		}
		if from == redisHTTPComponent {
			continue
		}
		if got := egress(from, redisHTTPComponent); !reflect.DeepEqual(got, want) {
			t.Errorf("%s may send to the shim on %v, want %v", from, got, want)
		}
	}
	if got := egress(redisHTTPComponent, "cache"); !reflect.DeepEqual(got, []int32{6379}) {
		t.Errorf("the shim may send to the cache on %v, want [6379]", got)
	}
	if got := ingress("cache", redisHTTPComponent); !reflect.DeepEqual(got, []int32{6379}) {
		t.Errorf("the cache admits the shim on %v, want [6379]", got)
	}
	// The shim reaches nothing else in the stack.
	for _, to := range components {
		if to == "cache" || to == redisHTTPComponent {
			continue
		}
		if got := egress(redisHTTPComponent, to); len(got) > 0 {
			t.Errorf("the shim may send to %s on %v", to, got)
		}
	}
	allowanceByName(t, &sandbox, "allow-redis-http-ingress")
	allowanceByName(t, &sandbox, "allow-redis-http-egress")
}
