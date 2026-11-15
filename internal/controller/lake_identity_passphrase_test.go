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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// operator#1168: an image reads the passphrase file when its manifest environment lists
// LIQUID_IDENTITY_PASSPHRASE_FILE; any other image does not.
func TestLakeManifestReportsTheIdentityPassphraseFile(t *testing.T) {
	document := func(environment string) lakeManifestDocument {
		var parsed lakeManifestDocument
		raw := `{"manifest_version":1,"name":"lake","version":"v1","revision":"abc",
			"schema":{"version":"sha256:000000000000","latest_migration":"0001_a","guard_table":"a_guard","guard_tables":["a_guard"],"migrations":["0001_a"]},
			"migrate":{"args":["migrate"],"wait_flag":"--wait-seconds","database_env":"LAKE_CONTROL_DB_URI"},
			"stripe":{"webhook_events":["invoice.paid"],"products":[{"id":"lake_seat","name":"Seat"}]},
			"environment":` + environment + `}`
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	reads, err := lakeManifestFromDocument(document(`[{"name":"LIQUID_HOME"},{"name":"LIQUID_IDENTITY_PASSPHRASE_FILE"}]`))
	if err != nil || !reads.IdentityPassphraseFile || reads.ServiceConnectionsOnDemand {
		t.Fatalf("an image listing the variable: %+v %v", reads, err)
	}
	older, err := lakeManifestFromDocument(document(`[{"name":"LIQUID_HOME"}]`))
	if err != nil || older.IdentityPassphraseFile {
		t.Fatalf("an image that does not list it: %+v %v", older, err)
	}
}

func TestTheIdentityPassphraseFileOnlyForTheImageThatSaidSo(t *testing.T) {
	lake := &oramav1alpha1.Lake{Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:new"}}
	yes := &oramav1alpha1.LakeImageManifest{Image: "oramasearch/lake:new", Supported: true, IdentityPassphraseFile: true}
	if !lakeReadsIdentityPassphraseFile(lake, yes) {
		t.Fatal("the image said it reads the file")
	}
	for name, manifest := range map[string]*oramav1alpha1.LakeImageManifest{
		"no manifest":   nil,
		"another image": {Image: "oramasearch/lake:old", Supported: true, IdentityPassphraseFile: true},
		"it does not":   {Image: "oramasearch/lake:new", Supported: true},
		"unsupported":   {Image: "oramasearch/lake:new", IdentityPassphraseFile: true},
	} {
		if lakeReadsIdentityPassphraseFile(lake, manifest) {
			t.Errorf("%s: the pod would get a file its image does not read", name)
		}
	}
}

// The lake container mounts only the owner-only copy; the Secret reaches the init container alone,
// which copies it as lake's uid with mode 0400 into memory.
func TestTheLakeContainerGetsOnlyTheOwnerOnlyCopy(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptrInt64(lakeRunAsUser), FSGroup: ptrInt64(lakeRunAsGroup)},
		Containers: []corev1.Container{{Name: "lake", Image: "oramasearch/lake:new",
			VolumeMounts: []corev1.VolumeMount{{Name: lakeStoreVolume, MountPath: lakeStorePath}}}},
		Volumes: []corev1.Volume{{Name: lakeStoreVolume}},
	}}
	applyLakeIdentityPassphrase(lake, &template, "oramasearch/lake:new")

	volumes := map[string]corev1.Volume{}
	for _, volume := range template.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	source := volumes[lakeIdentityPassphraseSourceVolume]
	if source.Secret == nil || source.Secret.SecretName != "lake-prod-identity-passphrase" ||
		len(source.Secret.Items) != 1 || source.Secret.Items[0].Key != "passphrase" {
		t.Fatalf("source volume = %+v", source)
	}
	copied := volumes[lakeIdentityPassphraseVolume]
	if copied.EmptyDir == nil || copied.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("the copy must be in memory: %+v", copied)
	}

	lakeContainer := template.Spec.Containers[0]
	for _, mount := range lakeContainer.VolumeMounts {
		if mount.Name == lakeIdentityPassphraseSourceVolume {
			t.Fatal("the lake container mounts the group-readable Secret")
		}
		if mount.Name == lakeIdentityPassphraseVolume && !mount.ReadOnly {
			t.Fatal("lake mounts the copy read-only")
		}
	}
	if got := findEnv(lakeContainer.Env, lakeIdentityPassphraseFileEnv); got == nil ||
		got.Value != "/var/run/lake/identity-passphrase/passphrase" || got.ValueFrom != nil {
		t.Fatalf("%s = %+v", lakeIdentityPassphraseFileEnv, got)
	}

	if len(template.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %+v", template.Spec.InitContainers)
	}
	init := template.Spec.InitContainers[0]
	if init.Image != "oramasearch/lake:new" || init.SecurityContext == nil || init.SecurityContext.RunAsNonRoot == nil ||
		!*init.SecurityContext.RunAsNonRoot || init.SecurityContext.RunAsUser != nil {
		t.Fatalf("the copy runs from the lake image as the pod's uid: %+v", init)
	}
	script := strings.Join(init.Command, " ")
	if !strings.Contains(script, "install -m 0400") ||
		!strings.Contains(script, lakeIdentityPassphraseSourceMountPath+"/passphrase") ||
		!strings.Contains(script, lakeIdentityPassphraseFilePath) {
		t.Fatalf("command = %q", init.Command)
	}
	if len(init.Env) != 0 {
		t.Fatal("the copy needs no environment")
	}
}

// Every Lake gets its passphrase once; a value changed by hand is kept.
func TestTheIdentityPassphraseIsMintedOnce(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := serviceTestLake()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	r := &LakeReconciler{Client: cl, Scheme: scheme}
	labels := map[string]string{"app.kubernetes.io/name": "lake"}
	if err := r.reconcileLakeIdentityPassphrase(ctx, lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: testServiceNamespace, Name: "lake-prod-identity-passphrase"}
	if err := cl.Get(ctx, key, &secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data["passphrase"]) != 64 || !metav1.IsControlledBy(&secret, lake) {
		t.Fatalf("passphrase Secret = %+v", secret.ObjectMeta)
	}
	secret.Data["passphrase"] = []byte("rotated-by-hand")
	if err := cl.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileLakeIdentityPassphrase(ctx, lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, &secret); err != nil || string(secret.Data["passphrase"]) != "rotated-by-hand" {
		t.Fatalf("a value already there was replaced: %v", err)
	}
}

// The variable is the operator's: spec.env cannot point lake at another file.
func TestSpecEnvCannotNameTheIdentityPassphraseFile(t *testing.T) {
	lake := serviceTestLake()
	lake.Spec.Env = []corev1.EnvVar{{Name: lakeIdentityPassphraseFileEnv, Value: "/var/lib/lake/store/passphrase"}}
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		if variable.Name == lakeIdentityPassphraseFileEnv {
			t.Fatalf("spec.env set %s = %q", variable.Name, variable.Value)
		}
	}
}
