/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Model checkpoints on a Zetta node's data volume (spec.models, lake#246, docs/zetta-models.md).
// Each model is an OCI image holding the checkpoint under /model; an init container running that
// image copies it to /data/models/<name> before the node starts. Images reach every node through
// the kubelet and the pull secret, so a workspace namespace, whose egress is denied by default,
// needs no route to the internet or a token to get its model.
const (
	// zettaModelsDir is where every model lands on the data volume.
	zettaModelsDir = zettaDataDir + "/models"
	// zettaModelImageDir is where a model image holds the checkpoint files.
	zettaModelImageDir = "/model"
	// zettaModelMarker names the file, inside a placed model, that records the image it came from.
	zettaModelMarker = ".zetta-model-image"
	// zettaModelInitPrefix prefixes each model's init container name.
	zettaModelInitPrefix = "model-"
)

// zettaModelCopyScript places one model. It is a constant: the directory and the image arrive as
// $1 and $2, never as script text (operator#279). It copies only when the directory does not
// already hold this image's copy, copies into a staging directory and renames it into place, so a
// half-finished copy is never taken for a model. A copy that fails (a full volume, a broken image)
// is logged and the node starts without the model: a workspace's data is never held offline by
// its model, and a native agent's chat says the checkpoint is missing.
const zettaModelCopyScript = `dir="$1"
want="$2"
staging="$dir.staging"
if [ -f "$dir/` + zettaModelMarker + `" ] && [ "$(cat "$dir/` + zettaModelMarker + `")" = "$want" ]; then
  echo "zetta model: $dir already holds $want"
  exit 0
fi
rm -rf "$staging"
if mkdir -p "$staging" &&
  cp -R ` + zettaModelImageDir + `/. "$staging/" &&
  printf '%s\n' "$want" > "$staging/` + zettaModelMarker + `" &&
  rm -rf "$dir" &&
  mv "$staging" "$dir"; then
  echo "zetta model: $dir now holds $want"
else
  rm -rf "$staging"
  echo "zetta model: could not place $want at $dir; the node starts without it" >&2
fi
exit 0
`

// zettaModelPath is where a model named name lands on a node.
func zettaModelPath(name string) string {
	return zettaModelsDir + "/" + name
}

// zettaModelInitContainers is one init container per spec.models entry, in spec order. Each runs
// the model image as the node's own user, with the node's data volume mounted, and small fixed
// resources so a workspace's quota and LimitRange admit it.
func zettaModelInitContainers(z *oramav1alpha1.OramaZettaCluster) []corev1.Container {
	if len(z.Spec.Models) == 0 {
		return nil
	}
	noPrivilegeEscalation := false
	containers := make([]corev1.Container, 0, len(z.Spec.Models))
	for _, model := range z.Spec.Models {
		containers = append(containers, corev1.Container{
			Name:            zettaModelInitPrefix + model.Name,
			Image:           model.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/bin/sh", "-c", zettaModelCopyScript, "zetta-model"},
			Args:            []string{zettaModelPath(model.Name), model.Image},
			VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: zettaDataDir}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &noPrivilegeEscalation,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		})
	}
	return containers
}
