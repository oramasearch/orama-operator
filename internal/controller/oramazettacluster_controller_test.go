/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

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

	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), ""); err != nil {
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

// TestZettaStatefulSetPerNodeAdvertise asserts a wan-accessible slice injects the positional advertise
// map so each node gossips ITS OWN routable address (field ORD+1), spreading cross-cluster sync across all
// their uplinks instead of dialing unreachable pod IPs (operator#104, oramasearch/zetta#739); and that the
// sandbox always re-pulls its mutable-tag image (operator#103).
func TestZettaStatefulSetPerNodeAdvertise(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}

	adv := "1.1.1.1:7777,2.2.2.2:7777,3.3.3.3:7777"
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), adv); err != nil {
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
	if err := r.mutateZettaStatefulSet(z, sts2, "z0-zetta-headless", "z0-zetta", zettaLabels(z), ""); err != nil {
		t.Fatalf("mutateZettaStatefulSet (dial-out): %v", err)
	}
	if findEnv(sts2.Spec.Template.Spec.Containers[0].Env, "ZETTA_ADVERTISE_MAP") != nil {
		t.Error("dial-out-only slice must not inject ZETTA_ADVERTISE_MAP")
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
