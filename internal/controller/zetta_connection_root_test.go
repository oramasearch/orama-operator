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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// operator#1166, zetta#1314: the connector-credential root secret is minted once, 64 hex characters,
// owned by the cluster, and a later reconcile never replaces it (or the rotation lines a person added).
func TestReconcileZettaConnectionRootMintsOnce(t *testing.T) {
	scheme := zettaTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}
	z := newZettaTestCluster()
	ctx := context.Background()

	if err := r.reconcileZettaConnectionRoot(ctx, z); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var sec corev1.Secret
	key := types.NamespacedName{Name: "z0-zetta-connection-root", Namespace: "orama-system"}
	if err := cl.Get(ctx, key, &sec); err != nil {
		t.Fatalf("get connection-root Secret: %v", err)
	}
	first := string(sec.Data["root"])
	if len(first) != 64 {
		t.Fatalf("root secret length = %d, want 64 hex characters (the engine refuses fewer than 32)", len(first))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("root secret is not hex: %v", err)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != "z0" {
		t.Errorf("connection-root Secret must be owned by the OramaZettaCluster, got %+v", sec.OwnerReferences)
	}

	if err := r.reconcileZettaConnectionRoot(ctx, z); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := cl.Get(ctx, key, &sec); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if got := string(sec.Data["root"]); got != first {
		t.Fatalf("root secret replaced across reconciles: it would orphan every sealed credential")
	}

	// A rotation by hand: the new secret first, the old one kept below it. The operator keeps it as is.
	rotated := strings.Repeat("b", 64) + "\n" + first + "\n"
	sec.Data["root"] = []byte(rotated)
	if err := cl.Update(ctx, &sec); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := r.reconcileZettaConnectionRoot(ctx, z); err != nil {
		t.Fatalf("reconcile after rotation: %v", err)
	}
	if err := cl.Get(ctx, key, &sec); err != nil {
		t.Fatalf("re-get after rotation: %v", err)
	}
	if got := string(sec.Data["root"]); got != rotated {
		t.Fatalf("the operator rewrote a rotated root secret")
	}
}

// Every node mounts the root secret read-only outside /data, mode 0440, and the engine is pointed at
// it with ZETTA_CONNECTION_ROOT_SECRET_FILE, which spec.env cannot replace.
func TestZettaStatefulSetMountsConnectionRootOutsideTheDataVolume(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	z.Spec.Env = []corev1.EnvVar{{Name: "ZETTA_CONNECTION_ROOT_SECRET_FILE", Value: "/data/root"}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	var values []string
	for _, variable := range c.Env {
		if variable.Name == "ZETTA_CONNECTION_ROOT_SECRET_FILE" {
			values = append(values, variable.Value)
		}
	}
	if len(values) != 1 || values[0] != "/run/secrets/zetta-connection-root/root" {
		t.Fatalf("ZETTA_CONNECTION_ROOT_SECRET_FILE = %v, want exactly the operator's mounted file", values)
	}
	if _, ignored := zettaCustomEnv(z.Spec.Env); len(ignored) != 1 || ignored[0] != "ZETTA_CONNECTION_ROOT_SECRET_FILE" {
		t.Fatalf("a spec.env ZETTA_CONNECTION_ROOT_SECRET_FILE must be ignored and reported, got %v", ignored)
	}
	var mount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "connection-root" {
			mount = &c.VolumeMounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly || mount.MountPath != "/run/secrets/zetta-connection-root" {
		t.Fatalf("connection-root mount = %+v, want read-only at /run/secrets/zetta-connection-root", mount)
	}
	if strings.HasPrefix(mount.MountPath+"/", zettaDataDir+"/") {
		t.Fatalf("the root secret must be outside the data volume %s", zettaDataDir)
	}
	var volume *corev1.Volume
	for i := range sts.Spec.Template.Spec.Volumes {
		if sts.Spec.Template.Spec.Volumes[i].Name == "connection-root" {
			volume = &sts.Spec.Template.Spec.Volumes[i]
		}
	}
	if volume == nil || volume.Secret == nil || volume.Secret.SecretName != "z0-zetta-connection-root" {
		t.Fatalf("connection-root volume = %+v, want Secret z0-zetta-connection-root", volume)
	}
	if len(volume.Secret.Items) != 1 || volume.Secret.Items[0].Key != "root" || volume.Secret.Items[0].Path != "root" {
		t.Errorf("connection-root items = %+v, want root -> root", volume.Secret.Items)
	}
	if mode := volume.Secret.DefaultMode; mode == nil || *mode != 0o440 || *mode&0o022 != 0 {
		t.Errorf("connection-root file mode = %v, want 0440 (the engine refuses a group- or other-writable file)", mode)
	}
}
