// SPDX-License-Identifier: MIT

package controller

// withOwnedAnnotation sets the operator's one annotation and keeps every other key.
// A Deployment's metadata annotations are shared with the Deployment controller, which
// writes deployment.kubernetes.io/revision; replacing the map removed it, the controller
// wrote it back, and each write bumped metadata.generation, so the two looped forever and
// the rollout never reported observed.
func withOwnedAnnotation(annotations map[string]string, key, value string) map[string]string {
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	return annotations
}
