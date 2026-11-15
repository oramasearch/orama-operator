/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// The identity passphrase file of a Lake pod (operator#1168, liquid#518, liquid#522, lake#1303).
//
// liquid protects a new identity's key with the macOS Keychain or a passphrase. A Linux pod has no
// terminal, so the only way is a passphrase file, and liquid refuses that file unless it is a
// regular file owned by the user lake runs as (or root), with mode 0600 or 0400, at most 64 KiB.
// Without the file, lake cannot create an identity on a pod.
//
// The operator keeps one Secret per Lake, lake-<lake>-identity-passphrase (key passphrase, 32
// random bytes as 64 hex), minted once and never written again while it holds a value, so a
// rotation done by hand is kept.
//
// The Secret cannot be handed to lake as a Secret volume. The Lake pod needs fsGroup (its store
// volume is written by uid 10001), and kubelet gives every file of a Secret volume the pod's
// fsGroup and adds group read to its mode, so a defaultMode of 0400 arrives as root:10001 0440:
// group-readable, which liquid refuses. Without fsGroup the file is root:root 0400, which lake's
// uid cannot read. So the Secret is mounted, group-readable, only into an init container, which
// copies it into a memory-backed emptyDir as a file owned by lake's uid with mode 0400, and only
// that copy is mounted into the lake container. LIQUID_IDENTITY_PASSPHRASE_FILE names the copy.
//
// Whether the image reads the file is what the image says, never a flag: an image whose manifest
// environment lists LIQUID_IDENTITY_PASSPHRASE_FILE (status.imageManifest.identityPassphraseFile)
// gets it. Any other image is rendered exactly as before, so the operator rolls first and the lake
// image with the new liquid pin follows.

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// lakeIdentityPassphraseFileEnv is liquid's variable, and what lake's manifest lists when the
	// image reads it.
	lakeIdentityPassphraseFileEnv = "LIQUID_IDENTITY_PASSPHRASE_FILE"
	// lakeIdentityPassphraseKey is the Secret's key, and the file name in both volumes.
	lakeIdentityPassphraseKey = "passphrase"

	// The Secret, mounted only into the init container, group-readable through the pod's fsGroup.
	lakeIdentityPassphraseSourceVolume    = "identity-passphrase-source"
	lakeIdentityPassphraseSourceMountPath = "/var/run/lake/identity-passphrase-source"
	// The owner-only copy lake reads, in memory.
	lakeIdentityPassphraseVolume    = "identity-passphrase"
	lakeIdentityPassphraseMountPath = "/var/run/lake/identity-passphrase"
	lakeIdentityPassphraseFilePath  = lakeIdentityPassphraseMountPath + "/" + lakeIdentityPassphraseKey

	// lakeIdentityPassphraseInit is the init container that makes the copy.
	lakeIdentityPassphraseInit = "identity-passphrase"
)

// lakeIdentityPassphraseCopyScript copies the passphrase into the emptyDir as lake's uid, mode
// 0400. The paths arrive as positional parameters; the file is removed first, because a pod whose
// sandbox restarts keeps its emptyDir and reruns its init containers, and install cannot replace a
// 0400 file it may not write. The value is never printed.
const lakeIdentityPassphraseCopyScript = `rm -f "$2"; install -m 0400 "$1" "$2"`

// lakeIdentityPassphraseSecret names a Lake's identity passphrase Secret.
func lakeIdentityPassphraseSecret(name string) string { return "lake-" + name + "-identity-passphrase" }

// lakeReadsIdentityPassphraseFile reports whether spec.image's manifest says it reads
// LIQUID_IDENTITY_PASSPHRASE_FILE. A manifest of another image, or none, is false.
func lakeReadsIdentityPassphraseFile(lake *oramav1alpha1.Lake, manifest *oramav1alpha1.LakeImageManifest) bool {
	return manifest != nil && manifest.Supported && manifest.IdentityPassphraseFile &&
		manifest.Image != "" && manifest.Image == lake.Spec.Image
}

// reconcileLakeIdentityPassphrase mints the Lake's passphrase once. Every Lake gets one, before its
// image reads it, so the Secret is there when the new image rolls. A value already present is never
// changed: liquid wraps the identity key under it.
func (r *LakeReconciler) reconcileLakeIdentityPassphrase(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string) error {
	name := lakeIdentityPassphraseSecret(lake.Name)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labels
		if len(secret.Data[lakeIdentityPassphraseKey]) == 0 {
			passphrase, err := randHex(32)
			if err != nil {
				return err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data[lakeIdentityPassphraseKey] = []byte(passphrase)
		}
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconcile identity passphrase: %w", err)
	}
	if op == controllerutil.OperationResultCreated {
		logf.FromContext(ctx).Info("provisioned the identity passphrase liquid wraps lake's identity key under",
			"secret", name)
	}
	return nil
}

// applyLakeIdentityPassphrase adds the passphrase file to a Lake pod template built for an image
// that reads it: the Secret mounted into the init container only, the owner-only copy mounted
// read-only into lake, and LIQUID_IDENTITY_PASSPHRASE_FILE naming the copy.
func applyLakeIdentityPassphrase(lake *oramav1alpha1.Lake, template *corev1.PodTemplateSpec, image string) {
	spec := &template.Spec
	sourceMode := int32(0o440) // readable by the init container through the pod's fsGroup
	copySize := resource.MustParse("1Mi")
	spec.Volumes = append(spec.Volumes,
		corev1.Volume{Name: lakeIdentityPassphraseSourceVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  lakeIdentityPassphraseSecret(lake.Name),
				DefaultMode: &sourceMode,
				Items:       []corev1.KeyToPath{{Key: lakeIdentityPassphraseKey, Path: lakeIdentityPassphraseKey}},
			},
		}},
		corev1.Volume{Name: lakeIdentityPassphraseVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &copySize},
		}},
	)
	securityContext := hardenedSecurityContext()
	securityContext.RunAsNonRoot = boolPtr(true)
	securityContext.ReadOnlyRootFilesystem = boolPtr(true)
	var pullPolicy corev1.PullPolicy
	if len(spec.Containers) > 0 {
		pullPolicy = spec.Containers[0].ImagePullPolicy
	}
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:            lakeIdentityPassphraseInit,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		// The lake image is Ubuntu: /bin/sh, rm and install are there.
		Command: []string{"/bin/sh", "-ec", lakeIdentityPassphraseCopyScript, lakeIdentityPassphraseInit,
			lakeIdentityPassphraseSourceMountPath + "/" + lakeIdentityPassphraseKey, lakeIdentityPassphraseFilePath},
		Resources:       *resourceReqs("16Mi", "10m", "64Mi"),
		SecurityContext: securityContext,
		VolumeMounts: []corev1.VolumeMount{
			{Name: lakeIdentityPassphraseSourceVolume, MountPath: lakeIdentityPassphraseSourceMountPath, ReadOnly: true},
			{Name: lakeIdentityPassphraseVolume, MountPath: lakeIdentityPassphraseMountPath},
		},
	})
	if len(spec.Containers) == 0 {
		return
	}
	container := &spec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: lakeIdentityPassphraseVolume, MountPath: lakeIdentityPassphraseMountPath, ReadOnly: true,
	})
	container.Env = append(container.Env, corev1.EnvVar{Name: lakeIdentityPassphraseFileEnv, Value: lakeIdentityPassphraseFilePath})
}
