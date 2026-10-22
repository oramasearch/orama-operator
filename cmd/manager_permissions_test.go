/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"os/exec"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestManagerReadOnlyRootHasBoundedWritableTemporaryStorage(t *testing.T) {
	raw, err := exec.Command("helm", "template", "orama-operator", "../chart", "--namespace", "orama-system", "--kube-version", "1.35.0", "--set", "operator.image.tag=test@sha256:"+strings.Repeat("0", 64), "--show-only", "templates/operator-deployment.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render manager: %v: %s", err, raw)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("containers: %d", len(pod.Containers))
	}
	manager := pod.Containers[0]
	sc := manager.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("manager process isolation: %+v", sc)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 {
		t.Fatal("temporary storage must be writable by the nonroot manager")
	}
	for _, mount := range manager.VolumeMounts {
		if mount.MountPath != "/tmp" || mount.ReadOnly {
			continue
		}
		for _, volume := range pod.Volumes {
			if volume.Name == mount.Name && volume.EmptyDir != nil && volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.Value() == 256<<20 {
				return
			}
		}
	}
	t.Fatal("read-only manager requires its bounded writable /tmp for Helm")
}
