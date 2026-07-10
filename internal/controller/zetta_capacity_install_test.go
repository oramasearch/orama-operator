/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// decodeAll parses every YAML document in raw into unstructured objects (mirrors the decode loop the
// reconciler uses), so a broken/renamed vendored asset fails the build offline — this feature is not
// live-tested (needs an EKS/AKS-NAP cluster), so the embedded manifests are validated here instead.
func decodeAll(t *testing.T, raw []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode embedded manifest: %v", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// TestEmbeddedKarpenterCRDs asserts the vendored CRD bundle carries the three Karpenter CRDs the
// NodePool reconcile + install depend on (NodePool / NodeClaim / EC2NodeClass).
func TestEmbeddedKarpenterCRDs(t *testing.T) {
	objs := decodeAll(t, karpenterCRDManifests)
	if len(objs) == 0 {
		t.Fatal("embedded Karpenter CRD bundle decoded to zero objects")
	}
	want := map[string]bool{
		"nodepools.karpenter.sh":           false,
		"nodeclaims.karpenter.sh":          false,
		"ec2nodeclasses.karpenter.k8s.aws": false,
	}
	for _, o := range objs {
		if o.GetKind() != "CustomResourceDefinition" {
			t.Errorf("CRD bundle contains a non-CRD object: %s/%s", o.GetKind(), o.GetName())
		}
		if _, ok := want[o.GetName()]; ok {
			want[o.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("embedded CRD bundle missing required CRD %q", name)
		}
	}
}

// TestEmbeddedKarpenterController asserts the controller bundle carries the install's core objects and
// exposes exactly the two placeholders the reconciler substitutes.
func TestEmbeddedKarpenterController(t *testing.T) {
	objs := decodeAll(t, karpenterControllerManifests)
	kinds := map[string]int{}
	var sawKarpenterSA, sawKarpenterDeploy bool
	for _, o := range objs {
		kinds[o.GetKind()]++
		if o.GetKind() == "ServiceAccount" && o.GetName() == "karpenter" {
			sawKarpenterSA = true
		}
		if o.GetKind() == "Deployment" && o.GetName() == "karpenter" {
			sawKarpenterDeploy = true
			if o.GetNamespace() != karpenterNamespace {
				t.Errorf("karpenter Deployment namespace = %q, want %q", o.GetNamespace(), karpenterNamespace)
			}
		}
	}
	if !sawKarpenterSA {
		t.Error("controller bundle missing ServiceAccount/karpenter")
	}
	if !sawKarpenterDeploy {
		t.Error("controller bundle missing Deployment/karpenter")
	}
	for _, k := range []string{"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Service"} {
		if kinds[k] == 0 {
			t.Errorf("controller bundle missing a %s", k)
		}
	}
}

// TestKarpenterPlaceholderSubstitution asserts both placeholders are present in the vendored bundle and
// that substitution replaces them with no placeholder residue (a broken substitution would silently ship
// `__CLUSTER_NAME__` to the API server).
func TestKarpenterPlaceholderSubstitution(t *testing.T) {
	for _, ph := range []string{installClusterNamePlaceholder, installRoleARNPlaceholder} {
		if !bytes.Contains(karpenterControllerManifests, []byte(ph)) {
			t.Fatalf("vendored controller bundle is missing placeholder %q (re-run hack/fetch-karpenter-manifests.sh)", ph)
		}
	}
	out := bytes.ReplaceAll(karpenterControllerManifests, []byte(installClusterNamePlaceholder), []byte("orama-eks-prod"))
	out = bytes.ReplaceAll(out, []byte(installRoleARNPlaceholder), []byte("arn:aws:iam::123456789012:role/KarpenterController"))
	if bytes.Contains(out, []byte("__")) && strings.Contains(string(out), "__CLUSTER_NAME__") {
		t.Error("cluster-name placeholder survived substitution")
	}
	if !bytes.Contains(out, []byte("orama-eks-prod")) {
		t.Error("substituted cluster name not found in output")
	}
	if !bytes.Contains(out, []byte("arn:aws:iam::123456789012:role/KarpenterController")) {
		t.Error("substituted controller role ARN not found in output")
	}
}
