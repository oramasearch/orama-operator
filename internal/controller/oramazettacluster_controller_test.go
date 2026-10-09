/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestZettaPodLabelsExposeManagedServiceWithoutChangingSelector(t *testing.T) {
	z := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "prod-svc-123456789abc",
		Labels: map[string]string{
			serviceLakeLabel:     "prod",
			serviceIDLabel:       "8831658d-5691-4d40-b2cd-d52c651a8087",
			serviceKindLabel:     serviceKindZetta,
			serviceCustomerLabel: "sha256-customer",
		},
	}}
	selector := zettaLabels(z)
	podLabels := zettaPodLabels(z)
	for _, key := range []string{serviceLakeLabel, serviceIDLabel, serviceKindLabel, serviceCustomerLabel} {
		if selector[key] != "" {
			t.Errorf("immutable selector contains %s", key)
		}
		if podLabels[key] != z.Labels[key] {
			t.Errorf("pod label %s = %q, want %q", key, podLabels[key], z.Labels[key])
		}
	}
}

func zettaTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := oramav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add orama scheme: %v", err)
	}
	return s
}

func newZettaTestCluster() *oramav1alpha1.OramaZettaCluster {
	return &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "z0", Namespace: "orama-system", UID: types.UID("uid-z0")},
		Spec:       oramav1alpha1.OramaZettaClusterSpec{Replicas: 3},
	}
}

// findEnv returns the EnvVar with the given name, or nil.
func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

// TestZettaStatefulSetInjectsSharedClusterToken asserts every node pod receives the shared cluster join
// token from the `<name>-zetta-cluster` Secret (via secretKeyRef) — the fix for operator#92 — and that the
// removed plaintext escape hatch leaves no trace (no ZETTA_CLUSTER_PLAINTEXT env, no --cluster-plaintext).
func TestZettaStatefulSetInjectsSharedClusterToken(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}

	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}

	c := sts.Spec.Template.Spec.Containers[0]
	tok := findEnv(c.Env, "ZETTA_CLUSTER_TOKEN")
	if tok == nil {
		t.Fatal("ZETTA_CLUSTER_TOKEN env not injected — nodes would each form a cluster-of-one (operator#92)")
	}
	if tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("ZETTA_CLUSTER_TOKEN must come from a Secret, got %+v", tok)
	}
	if got := tok.ValueFrom.SecretKeyRef.Name; got != "z0-zetta-cluster" {
		t.Errorf("cluster token Secret name = %q, want %q", got, "z0-zetta-cluster")
	}
	if got := tok.ValueFrom.SecretKeyRef.Key; got != "token" {
		t.Errorf("cluster token Secret key = %q, want %q", got, "token")
	}

	if findEnv(c.Env, "ZETTA_CLUSTER_PLAINTEXT") != nil {
		t.Error("ZETTA_CLUSTER_PLAINTEXT env still present — the plaintext mode was removed as a security hole")
	}
	entry := c.Command[len(c.Command)-1]
	if strings.Contains(entry, "--cluster-plaintext") || strings.Contains(entry, "PLAINTEXT") {
		t.Error("entrypoint still references the removed plaintext mode")
	}
}

func TestZettaStatefulSetSkipsRecursiveFSGroupChanges(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}

	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	securityContext := sts.Spec.Template.Spec.SecurityContext
	if securityContext == nil || securityContext.FSGroup == nil || *securityContext.FSGroup != 10001 ||
		securityContext.FSGroupChangePolicy == nil ||
		*securityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch ||
		securityContext.RunAsNonRoot == nil || !*securityContext.RunAsNonRoot ||
		securityContext.SeccompProfile == nil ||
		securityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context = %+v", securityContext)
	}
	pod := sts.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("Zetta pod mounts a Kubernetes API token")
	}
	containerSecurity := pod.Containers[0].SecurityContext
	if containerSecurity == nil || containerSecurity.AllowPrivilegeEscalation == nil ||
		*containerSecurity.AllowPrivilegeEscalation || containerSecurity.Capabilities == nil ||
		len(containerSecurity.Capabilities.Drop) != 1 || containerSecurity.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container security context = %+v", containerSecurity)
	}
}

func TestZettaStatefulSetAppendsCustomEnvAndProtectsManagedNames(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	z.Spec.Env = []corev1.EnvVar{
		{Name: "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES", Value: "134217728"},
		{Name: "ZETTA_ADMIN_TOKEN", Value: "not-the-managed-token"},
		{Name: "MALLOC_ARENA_MAX", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "allocator-settings"},
			Key:                  "arenas",
		}}},
		{Name: "ZETTA_ADMIN_TOKEN", Value: "still-not-the-managed-token"},
		{Name: "POD_NAME", Value: "not-the-pod-name"},
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}

	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}

	env := sts.Spec.Template.Spec.Containers[0].Env
	trim := findEnv(env, "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES")
	if trim == nil || trim.Value != "134217728" {
		t.Fatalf("ZETTA_MALLOC_TRIM_THRESHOLD_BYTES = %+v", trim)
	}
	arenas := findEnv(env, "MALLOC_ARENA_MAX")
	if arenas == nil || arenas.ValueFrom == nil || arenas.ValueFrom.SecretKeyRef == nil ||
		arenas.ValueFrom.SecretKeyRef.Name != "allocator-settings" || arenas.ValueFrom.SecretKeyRef.Key != "arenas" {
		t.Fatalf("MALLOC_ARENA_MAX = %+v", arenas)
	}
	admin := findEnv(env, "ZETTA_ADMIN_TOKEN")
	if admin == nil || admin.Value != "" || admin.ValueFrom == nil || admin.ValueFrom.SecretKeyRef == nil ||
		admin.ValueFrom.SecretKeyRef.Name != "z0-zetta-admin" {
		t.Fatalf("managed ZETTA_ADMIN_TOKEN was overridden: %+v", admin)
	}
	if env[len(env)-2].Name != "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES" || env[len(env)-1].Name != "MALLOC_ARENA_MAX" {
		t.Fatalf("custom env was not appended in authored order: %+v", env)
	}
	_, ignored := zettaCustomEnv(z.Spec.Env)
	if !slices.Equal(ignored, []string{"POD_NAME", "ZETTA_ADMIN_TOKEN"}) {
		t.Fatalf("ignored env = %v", ignored)
	}
}

// TestZettaStatefulSetPerNodeAdvertise asserts a wan-accessible slice injects the positional advertise
// map so each node gossips ITS OWN routable address (field ORD+1), spreading cross-cluster sync across all
// their uplinks instead of dialing unreachable pod IPs (operator#104, oramasearch/zetta#739); and that the
// sandbox always re-pulls its mutable-tag image (operator#103).
func TestZettaStatefulSetPerNodeAdvertise(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}

	adv := "1.1.1.1:7777,2.2.2.2:7777,3.3.3.3:7777"
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), adv, nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	if env := findEnv(c.Env, "ZETTA_ADVERTISE_MAP"); env == nil || env.Value != adv {
		t.Fatalf("ZETTA_ADVERTISE_MAP = %+v, want value %q", env, adv)
	}
	entry := c.Command[len(c.Command)-1]
	if !strings.Contains(entry, "--advertise") {
		t.Error("entrypoint never renders --advertise for a wan slice")
	}
	if !strings.Contains(entry, "cut -d, -f$((ORD+1))") {
		t.Error("entrypoint should select THIS node's OWN advertise address by ordinal, not a shared one")
	}
	if string(c.ImagePullPolicy) != "Always" {
		t.Errorf("imagePullPolicy = %q, want Always (operator#103 — mutable-tag caching runs stale images)", c.ImagePullPolicy)
	}
	// A dial-out-only slice (no advertise) must NOT inject the map.
	sts2 := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSet(z, sts2, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet (dial-out): %v", err)
	}
	if findEnv(sts2.Spec.Template.Spec.Containers[0].Env, "ZETTA_ADVERTISE_MAP") != nil {
		t.Error("dial-out-only slice must not inject ZETTA_ADVERTISE_MAP")
	}
}

// TestZettaStatefulSetRendersReachabilityAndNodeType asserts the operator drives the engine's node:<id>
// CRDT seed (zetta#803/#810): --dial-out-only iff the slice's effective reachability is dial-out-only, and
// --node-type ephemeral|permanent from spec.ephemeral. Without these the engine defaults every node to
// permanent/wan-accessible and system.nodes mislabels dial-out-only slices (the reachability-aware dialer
// stays dormant). reachability + node_type are INDEPENDENT axes; ephemeral additionally forces dial-out-only.
func TestZettaStatefulSetRendersReachabilityAndNodeType(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	entryFor := func(mutate func(*oramav1alpha1.OramaZettaCluster)) string {
		z := newZettaTestCluster()
		mutate(z)
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
		if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
			t.Fatalf("mutateZettaStatefulSet: %v", err)
		}
		// The node flags are container args since operator#279 (the script itself is constant).
		return strings.Join(sts.Spec.Template.Spec.Containers[0].Args, " ")
	}

	// wan-accessible, not ephemeral → --node-type permanent, NO --dial-out-only.
	wan := entryFor(func(z *oramav1alpha1.OramaZettaCluster) {
		z.Spec.Reachability = oramav1alpha1.ReachabilityWANAccessible
	})
	if !strings.Contains(wan, "--node-type permanent") {
		t.Error("wan slice: entrypoint should render --node-type permanent")
	}
	if strings.Contains(wan, "--dial-out-only") {
		t.Error("wan slice: entrypoint must NOT render --dial-out-only")
	}

	// dial-out-only (explicit), not ephemeral → --node-type permanent + --dial-out-only.
	dial := entryFor(func(z *oramav1alpha1.OramaZettaCluster) { z.Spec.Reachability = oramav1alpha1.ReachabilityDialOutOnly })
	if !strings.Contains(dial, "--node-type permanent") || !strings.Contains(dial, "--dial-out-only") {
		t.Errorf("dial-out slice: want --node-type permanent AND --dial-out-only, got %q", dial)
	}

	// unset reachability defaults to dial-out-only (a private node is the initiator).
	def := entryFor(func(z *oramav1alpha1.OramaZettaCluster) {})
	if !strings.Contains(def, "--dial-out-only") {
		t.Error("unset reachability should default to --dial-out-only")
	}

	// ephemeral → --node-type ephemeral, and ephemeral forces dial-out-only regardless of reachability.
	eph := entryFor(func(z *oramav1alpha1.OramaZettaCluster) {
		z.Spec.Ephemeral = true
		z.Spec.Reachability = oramav1alpha1.ReachabilityWANAccessible // overridden by ephemeral
	})
	if !strings.Contains(eph, "--node-type ephemeral") {
		t.Error("ephemeral slice: entrypoint should render --node-type ephemeral")
	}
	if !strings.Contains(eph, "--dial-out-only") {
		t.Error("ephemeral slice is always dial-out-only (must render --dial-out-only)")
	}
	if strings.Contains(eph, "--node-type permanent") {
		t.Error("ephemeral slice must not also render --node-type permanent")
	}
}

// TestZettaStatefulSetAlwaysSeedsAdminTokenNoRemovedFlags asserts the auth-companion behavior to zetta#799:
// auth is MANDATORY and the flags --require-auth/--mcp-token/--cluster-plaintext were REMOVED (passing any
// crashes the node). So the entrypoint must render --admin-token but NONE of the removed flags, and
// ZETTA_ADMIN_TOKEN must be injected on every node UNCONDITIONALLY (even with spec.requireAuth unset), while
// ZETTA_MCP_TOKEN is never injected.
func TestZettaStatefulSetAlwaysSeedsAdminTokenNoRemovedFlags(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster() // RequireAuth / MCPTokenAuth both zero-valued (unset)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	c := sts.Spec.Template.Spec.Containers[0]

	// Admin token injected unconditionally from the <name>-zetta-admin Secret (auth is mandatory).
	adm := findEnv(c.Env, "ZETTA_ADMIN_TOKEN")
	if adm == nil {
		t.Fatal("ZETTA_ADMIN_TOKEN env not injected — auth is mandatory (zetta#799); the admin token must always be seeded")
	}
	if adm.ValueFrom == nil || adm.ValueFrom.SecretKeyRef == nil ||
		adm.ValueFrom.SecretKeyRef.Name != "z0-zetta-admin" || adm.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("ZETTA_ADMIN_TOKEN must come from Secret z0-zetta-admin key token, got %+v", adm)
	}
	// The removed --mcp-token env must never be injected.
	if findEnv(c.Env, "ZETTA_MCP_TOKEN") != nil {
		t.Error("ZETTA_MCP_TOKEN env still injected — --mcp-token was removed in zetta#799")
	}

	// The entrypoint renders --admin-token but NONE of the removed flags.
	entry := c.Command[len(c.Command)-1]
	if !strings.Contains(entry, "--admin-token") {
		t.Error("entrypoint should render --admin-token (seeds the owner)")
	}
	for _, removed := range []string{"--require-auth", "--mcp-token", "--cluster-plaintext"} {
		if strings.Contains(entry, removed) {
			t.Errorf("entrypoint still renders removed flag %q — passing it crashes the node (zetta#799)", removed)
		}
	}
}

// TestZettaSQLSendsBearer asserts the operator's Zetta SQL client authenticates with the admin bearer — the
// engine's auth is mandatory (zetta#799), so an unauthenticated call would 401.
func TestZettaSQLSendsBearer(t *testing.T) {
	var gotAuth, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, gotURL = req.Header.Get("Authorization"), req.URL.String()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if err := zettaSQL(context.Background(), srv.URL, "sekret-token", "orama_control", "SELECT 1"); err != nil {
		t.Fatalf("zettaSQL: %v", err)
	}
	if gotAuth != "Bearer sekret-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sekret-token")
	}
	if !strings.Contains(gotURL, "database=orama_control") {
		t.Errorf("request URL missing database param: %q", gotURL)
	}
}

// TestReconcileZettaClusterTokenMintsOnce asserts the shared join token is minted as a 64-hex value into
// `<name>-zetta-cluster` and is PRESERVED across reconciles (mint-once, never rotate — rotating would
// re-derive the cluster CA and orphan every existing node cert).
func TestReconcileZettaClusterTokenMintsOnce(t *testing.T) {
	scheme := zettaTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}
	z := newZettaTestCluster()
	ctx := context.Background()

	if err := r.reconcileZettaClusterToken(ctx, z); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var sec corev1.Secret
	key := types.NamespacedName{Name: "z0-zetta-cluster", Namespace: "orama-system"}
	if err := cl.Get(ctx, key, &sec); err != nil {
		t.Fatalf("get cluster-token Secret: %v", err)
	}
	first := string(sec.Data["token"])
	if len(first) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(first))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("token is not valid hex: %v", err)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != "z0" {
		t.Errorf("cluster-token Secret must be owned by the OramaZettaCluster, got %+v", sec.OwnerReferences)
	}

	// A second reconcile must NOT rotate the token.
	if err := r.reconcileZettaClusterToken(ctx, z); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := cl.Get(ctx, key, &sec); err != nil {
		t.Fatalf("re-get cluster-token Secret: %v", err)
	}
	if got := string(sec.Data["token"]); got != first {
		t.Errorf("token rotated across reconciles: %q -> %q (must be mint-once)", first, got)
	}
}

// TestReconcileZettaIngressGate asserts spec.ingressEnabled is the explicit exposure gate: no Ingress
// is created unless ingressEnabled is true AND host is set, and flipping the gate off tears the Ingress
// back down even while host stays set (the field is the on/off switch, host merely names the endpoint).
func TestReconcileZettaIngressGate(t *testing.T) {
	scheme := zettaTestScheme(t)
	ctx := context.Background()
	ingKey := types.NamespacedName{Name: "z0-zetta", Namespace: "orama-system"}

	reconcile := func(t *testing.T, z *oramav1alpha1.OramaZettaCluster, cl client.Client) {
		t.Helper()
		r := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}
		if err := r.reconcileZettaIngress(ctx, z, "z0-zetta", zettaLabels(z)); err != nil {
			t.Fatalf("reconcileZettaIngress: %v", err)
		}
	}
	hasIngress := func(t *testing.T, cl client.Client) bool {
		t.Helper()
		var ing networkingv1.Ingress
		err := cl.Get(ctx, ingKey, &ing)
		if err == nil {
			return true
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get ingress: %v", err)
		}
		return false
	}

	t.Run("host set but gate off → no ingress", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		z := newZettaTestCluster()
		z.Spec.Host, z.Spec.IngressEnabled = "zetta.example.com", false
		reconcile(t, z, cl)
		if hasIngress(t, cl) {
			t.Fatal("Ingress created while ingressEnabled=false")
		}
	})

	t.Run("gate on but no host → no ingress", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		z := newZettaTestCluster()
		z.Spec.IngressEnabled = true // host left empty
		reconcile(t, z, cl)
		if hasIngress(t, cl) {
			t.Fatal("Ingress created with empty host")
		}
	})

	t.Run("gate on + host → ingress created, then gate off → torn down", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		z := newZettaTestCluster()
		z.Spec.Host, z.Spec.IngressEnabled = "zetta.example.com", true
		reconcile(t, z, cl)

		var ing networkingv1.Ingress
		if err := cl.Get(ctx, ingKey, &ing); err != nil {
			t.Fatalf("expected Ingress created: %v", err)
		}
		if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
			t.Errorf("ingressClassName = %v, want nginx", ing.Spec.IngressClassName)
		}
		if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "zetta.example.com" {
			t.Errorf("ingress host rule = %+v, want host zetta.example.com", ing.Spec.Rules)
		}

		// Flip the gate off while host stays set → the Ingress must be torn back down.
		z.Spec.IngressEnabled = false
		reconcile(t, z, cl)
		if hasIngress(t, cl) {
			t.Fatal("Ingress not torn down after ingressEnabled flipped to false")
		}
	})
}

// TestZettaStatefulSetKeepsItsClaimTemplate is operator#259: a StatefulSet's claim templates are
// immutable, so a larger spec.storage.size (a workspace grown by lake's storage_gi) sets the
// template only on create and leaves an existing StatefulSet's template as it was; the claims
// themselves are grown in place.
func TestZettaStatefulSetKeepsItsClaimTemplate(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	z.Spec.Storage.Size = "5Gi"
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	size := func() string {
		quantity := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
		return quantity.String()
	}
	if got := size(); got != "5Gi" {
		t.Fatalf("new StatefulSet template = %s, want 5Gi", got)
	}
	sts.ResourceVersion = "1"
	z.Spec.Storage.Size = "20Gi"
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	if got := size(); got != "5Gi" {
		t.Fatalf("existing StatefulSet template changed to %s; the API server refuses that update", got)
	}
}

// Review of #267: a cluster whose spec.storage no longer matches its StatefulSet's immutable claim
// template says so in a condition instead of ignoring it silently; a matching one has none.
func TestZettaStorageTemplateConditionReportsAnUnchangedTemplate(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	z.Spec.Storage.Size = "5Gi"
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatal(err)
	}
	if condition := zettaStorageTemplateCondition(z, sts); condition != nil {
		t.Fatalf("matching template reported %+v", condition)
	}
	z.Spec.Storage.Size = "20Gi"
	condition := zettaStorageTemplateCondition(z, sts)
	if condition == nil || condition.Type != storageTemplateConditionType || condition.Status != metav1.ConditionFalse ||
		condition.Reason != "ClaimTemplateUnchanged" || !strings.Contains(condition.Message, "grown by hand") {
		t.Fatalf("changed size reported %+v", condition)
	}
	z.Labels = map[string]string{workspaceLabel: testWorkspaceID}
	if condition := zettaStorageTemplateCondition(z, sts); condition == nil || !strings.Contains(condition.Message, "workspace reconciler") {
		t.Fatalf("workspace cluster reported %+v", condition)
	}
}
