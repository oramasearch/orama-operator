// SPDX-License-Identifier: MIT

package controller

// Regression test for the methodology isolation rule found by a security review of workspace
// services.

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Rule: a pod that holds a run's or deployment's credential never starts in, and never imports
// from, the work volume the sandbox writes. The runtime runs `python3 -m`, which would otherwise
// import a module the sandbox planted in its working directory, with the credential and the
// transfer network.
func TestMethodologyCredentialPodsNeverStartInTheSandboxVolume(t *testing.T) {
	lake := &oramav1alpha1.Lake{}
	lake.Name = "prod"
	lake.Spec.Namespace = "lake"
	run := &LakeMethodologyRun{ID: "run-id", Slug: "prod-mr-abc", RuntimeImage: "oramasearch/methodology-runtime@sha256:abc"}
	deployment := &LakeMethodologyDeployment{ID: "dep-id", Slug: "prod-md-abc", RuntimeImage: run.RuntimeImage}
	exit := int32(0)
	for name, pod := range map[string]corev1.PodSpec{
		"stage":       methodologyStagePod(lake, run, nil),
		"collect":     methodologyCollectPod(lake, run, &exit),
		"model stage": methodologyDeploymentStagePod(lake, deployment, nil),
	} {
		t.Run(name, func(t *testing.T) {
			container := pod.Containers[0]
			if container.WorkingDir != methodologySafeWorkingDir {
				t.Fatalf("working directory %q, want %q", container.WorkingDir, methodologySafeWorkingDir)
			}
			if strings.HasPrefix(container.WorkingDir, methodologyWorkPath) {
				t.Fatalf("working directory %q is on the sandbox's volume", container.WorkingDir)
			}
			safe := false
			for _, variable := range container.Env {
				if variable.Name == "PYTHONSAFEPATH" && variable.Value == "1" {
					safe = true
				}
			}
			if !safe {
				t.Fatalf("PYTHONSAFEPATH=1 is not set: %v", container.Env)
			}
			tmpIsPodLocal := false
			for _, volume := range pod.Volumes {
				for _, mount := range container.VolumeMounts {
					if mount.Name == volume.Name && mount.MountPath == methodologySafeWorkingDir && volume.EmptyDir != nil {
						tmpIsPodLocal = true
					}
				}
			}
			if !tmpIsPodLocal {
				t.Fatal("the working directory is not this pod's own emptyDir")
			}
		})
	}
	// The sandbox itself is unchanged: it holds no credential.
	sandbox := methodologySandboxPod(run, "image@sha256:abc")
	for _, volume := range sandbox.Volumes {
		if volume.Secret != nil {
			t.Fatalf("the sandbox mounts Secret %q", volume.Secret.SecretName)
		}
	}
}
