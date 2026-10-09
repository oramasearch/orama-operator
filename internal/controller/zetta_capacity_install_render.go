/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Rendering of the operator-owned Karpenter install (operator#75), hardened for operator#79.
//
// The two per-CR values (the EKS cluster name and the controller's IAM role ARN) used to be pasted into
// the vendored YAML as text before parsing, so a crafted value could close a string, add a `---`
// document and have the operator server-side-apply any manifest with its bind/escalate RBAC. Now:
//
//  1. both values are validated against the CRD patterns (here as well as at admission);
//  2. the template is parsed first and the values are set on the decoded objects, at one known field
//     each, which must hold its placeholder;
//  3. the rendered bundle must be exactly karpenterControllerObjects, or nothing is applied.

const (
	// Placeholders the vendored controller.yaml carries for the two per-CR values. They are never
	// replaced as text. Keep in lockstep with hack/fetch-karpenter-manifests.sh.
	installClusterNamePlaceholder = "__CLUSTER_NAME__"
	installRoleARNPlaceholder     = "__CONTROLLER_ROLE_ARN__"

	// installRoleARNAnnotation is the IRSA annotation on ServiceAccount kube-system/karpenter.
	installRoleARNAnnotation = "eks.amazonaws.com/role-arn"
	// installClusterNameEnv is the env var on the controller container of Deployment kube-system/karpenter.
	installClusterNameEnv      = "CLUSTER_NAME"
	installControllerContainer = "controller"
	karpenterControllerName    = "karpenter"

	installClusterNameMaxLength = 100
	installRoleARNMaxLength     = 550
)

// installClusterNamePattern and installRoleARNPattern are the CRD validation patterns on
// spec.capacity.karpenter.installClusterName and .controllerRoleARN (api/v1alpha1). They are checked
// again here so a CR stored before the patterns existed, or written by a client that skips validation,
// is refused. TestKarpenterInstallPatternsMatchCRD keeps the two copies identical.
var (
	installClusterNamePattern = regexp.MustCompile(`^[0-9A-Za-z][A-Za-z0-9\-_]{0,99}$`)
	installRoleARNPattern     = regexp.MustCompile(`^arn:aws(-cn|-us-gov)?:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_\-/]{1,512}$`)
)

// karpenterObjectKey names one object of a vendored Karpenter bundle.
type karpenterObjectKey struct {
	APIVersion, Kind, Namespace, Name string
}

func (k karpenterObjectKey) String() string {
	if k.Namespace == "" {
		return fmt.Sprintf("%s %s %s", k.APIVersion, k.Kind, k.Name)
	}
	return fmt.Sprintf("%s %s %s/%s", k.APIVersion, k.Kind, k.Namespace, k.Name)
}

// karpenterControllerObjects is exactly the set of objects the controller bundle may contain. The
// rendered bundle is refused unless it matches one-to-one, and the chart ClusterRole scopes the
// operator's RBAC writes, bind and escalate to these names (chart/templates/rbac.yaml). Re-vendoring a
// version that adds or renames an object fails the tests until this list and the chart are updated.
var karpenterControllerObjects = []karpenterObjectKey{
	{"policy/v1", "PodDisruptionBudget", karpenterNamespace, "karpenter"},
	{"v1", "ServiceAccount", karpenterNamespace, "karpenter"},
	{"rbac.authorization.k8s.io/v1", "ClusterRole", "", "karpenter-admin"},
	{"rbac.authorization.k8s.io/v1", "ClusterRole", "", "karpenter-core"},
	{"rbac.authorization.k8s.io/v1", "ClusterRole", "", "karpenter"},
	{"rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "karpenter-core"},
	{"rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "karpenter"},
	{"rbac.authorization.k8s.io/v1", "Role", karpenterNamespace, "karpenter"},
	{"rbac.authorization.k8s.io/v1", "Role", karpenterNamespace, "karpenter-dns"},
	{"rbac.authorization.k8s.io/v1", "RoleBinding", karpenterNamespace, "karpenter"},
	{"rbac.authorization.k8s.io/v1", "RoleBinding", karpenterNamespace, "karpenter-dns"},
	{"v1", "Service", karpenterNamespace, "karpenter"},
	{"apps/v1", "Deployment", karpenterNamespace, "karpenter"},
}

// karpenterCRDNames is exactly the set of CRDs the vendored CRD bundle may contain; the chart ClusterRole
// scopes the operator's CRD writes to these names.
var karpenterCRDNames = []string{
	"ec2nodeclasses.karpenter.k8s.aws",
	"nodeclaims.karpenter.sh",
	"nodeoverlays.karpenter.sh",
	"nodepools.karpenter.sh",
}

// validateKarpenterInstallValues checks the two CR values the controller bundle carries. An empty value
// is not checked here: it is a missing prerequisite with its own condition. The error names the field
// and the rule but never echoes the value, which may be hostile.
func validateKarpenterInstallValues(clusterName, roleARN string) error {
	if clusterName != "" && (len(clusterName) > installClusterNameMaxLength || !installClusterNamePattern.MatchString(clusterName)) {
		return fmt.Errorf("spec.capacity.karpenter.installClusterName is not a valid EKS cluster name: it must be 1-%d characters, a letter or digit first, then only letters, digits, '-' or '_'",
			installClusterNameMaxLength)
	}
	if roleARN != "" && (len(roleARN) > installRoleARNMaxLength || !installRoleARNPattern.MatchString(roleARN)) {
		return fmt.Errorf("spec.capacity.karpenter.controllerRoleARN is not a valid IAM role ARN: it must be arn:aws[-cn|-us-gov]:iam::<12-digit account>:role/<path and name>, at most %d characters, IAM path and name characters only",
			installRoleARNMaxLength)
	}
	return nil
}

// decodeManifests parses every YAML document in raw into unstructured objects, skipping empty documents.
func decodeManifests(raw []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		if isEmptyManifestDoc(obj) { // skip empty / non-object docs (leading `---`, comment-only chunks)
			continue
		}
		out = append(out, obj)
	}
}

// requireExactObjects fails unless objs holds each key in want exactly once and nothing else.
func requireExactObjects(objs []*unstructured.Unstructured, want []karpenterObjectKey) error {
	expected := make(map[karpenterObjectKey]bool, len(want))
	for _, key := range want {
		expected[key] = false
	}
	for _, obj := range objs {
		key := karpenterObjectKey{obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName()}
		seen, ok := expected[key]
		if !ok {
			return fmt.Errorf("unexpected object %s in the Karpenter bundle", key)
		}
		if seen {
			return fmt.Errorf("object %s appears twice in the Karpenter bundle", key)
		}
		expected[key] = true
	}
	for _, key := range want {
		if !expected[key] {
			return fmt.Errorf("object %s is missing from the Karpenter bundle", key)
		}
	}
	return nil
}

// karpenterCRDObjects decodes the vendored CRD bundle and checks it holds exactly karpenterCRDNames.
func karpenterCRDObjects() ([]*unstructured.Unstructured, error) {
	objs, err := decodeManifests(karpenterCRDManifests)
	if err != nil {
		return nil, err
	}
	want := make([]karpenterObjectKey, 0, len(karpenterCRDNames))
	for _, name := range karpenterCRDNames {
		want = append(want, karpenterObjectKey{"apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name})
	}
	if err := requireExactObjects(objs, want); err != nil {
		return nil, err
	}
	return objs, nil
}

// karpenterControllerTemplate decodes the vendored controller bundle (placeholders intact) and checks it
// holds exactly karpenterControllerObjects.
func karpenterControllerTemplate() ([]*unstructured.Unstructured, error) {
	objs, err := decodeManifests(karpenterControllerManifests)
	if err != nil {
		return nil, err
	}
	if err := requireExactObjects(objs, karpenterControllerObjects); err != nil {
		return nil, err
	}
	return objs, nil
}

// renderKarpenterController builds the controller bundle for one CR. The template is parsed first; the
// two values are then set as structured fields (the ServiceAccount's IRSA annotation and the
// Deployment's CLUSTER_NAME env var), each of which must hold its placeholder. A value is a plain string
// in one field: it cannot add a document, close a string or reach another field.
func renderKarpenterController(clusterName, roleARN string) ([]*unstructured.Unstructured, error) {
	if clusterName == "" || roleARN == "" {
		return nil, errors.New("installClusterName and controllerRoleARN are both required to render the Karpenter controller")
	}
	if err := validateKarpenterInstallValues(clusterName, roleARN); err != nil {
		return nil, err
	}
	objs, err := karpenterControllerTemplate()
	if err != nil {
		return nil, err
	}
	if err := setKarpenterInstallValues(objs, clusterName, roleARN); err != nil {
		return nil, err
	}
	if err := requireExactObjects(objs, karpenterControllerObjects); err != nil {
		return nil, err
	}
	return objs, nil
}

// setKarpenterInstallValues sets the two values on the parsed template objects. It does not validate
// them (renderKarpenterController does), so a test can show that even a hostile value lands only in its
// own field.
func setKarpenterInstallValues(objs []*unstructured.Unstructured, clusterName, roleARN string) error {
	var setARN, setName bool
	for _, obj := range objs {
		if obj.GetNamespace() != karpenterNamespace || obj.GetName() != karpenterControllerName {
			continue
		}
		switch {
		case obj.GetAPIVersion() == "v1" && obj.GetKind() == "ServiceAccount":
			annotations := obj.GetAnnotations()
			if annotations[installRoleARNAnnotation] != installRoleARNPlaceholder {
				return fmt.Errorf("ServiceAccount %s/%s: annotation %s does not hold its placeholder", karpenterNamespace, karpenterControllerName, installRoleARNAnnotation)
			}
			annotations[installRoleARNAnnotation] = roleARN
			obj.SetAnnotations(annotations)
			setARN = true
		case obj.GetAPIVersion() == "apps/v1" && obj.GetKind() == "Deployment":
			if err := setControllerClusterName(obj, clusterName); err != nil {
				return err
			}
			setName = true
		}
	}
	if !setARN || !setName {
		return errors.New("the Karpenter controller template lacks the ServiceAccount or the Deployment the CR values go in")
	}
	return nil
}

// setControllerClusterName sets env CLUSTER_NAME on the Deployment's controller container. The variable
// must exist exactly once and hold the placeholder.
func setControllerClusterName(deploy *unstructured.Unstructured, clusterName string) error {
	containers, found, err := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return fmt.Errorf("deployment %s/%s has no containers: %v", karpenterNamespace, karpenterControllerName, err)
	}
	set := false
	for i, rawContainer := range containers {
		container, ok := rawContainer.(map[string]interface{})
		if !ok || container["name"] != installControllerContainer {
			continue
		}
		env, _, err := unstructured.NestedSlice(container, "env")
		if err != nil {
			return err
		}
		for j, rawVar := range env {
			envVar, ok := rawVar.(map[string]interface{})
			if !ok || envVar["name"] != installClusterNameEnv {
				continue
			}
			if set || envVar["value"] != installClusterNamePlaceholder {
				return fmt.Errorf("deployment %s/%s: env %s must hold its placeholder exactly once", karpenterNamespace, karpenterControllerName, installClusterNameEnv)
			}
			envVar["value"] = clusterName
			env[j] = envVar
			set = true
		}
		if err := unstructured.SetNestedSlice(container, env, "env"); err != nil {
			return err
		}
		containers[i] = container
	}
	if !set {
		return fmt.Errorf("deployment %s/%s: container %q has no env %s", karpenterNamespace, karpenterControllerName, installControllerContainer, installClusterNameEnv)
	}
	return unstructured.SetNestedSlice(deploy.Object, containers, "spec", "template", "spec", "containers")
}
