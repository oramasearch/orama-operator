// SPDX-License-Identifier: MIT

package controller

import (
	"testing"
)

// The usage line zetta v0.5.0 prints for the join-token file (src/main.cpp, zetta#770).
const testZettaUsageTokenFile = "  ZETTA_CLUSTER_TOKEN_FILE File holding the join token (preferred)\n"

// An image whose usage lists ZETTA_CLUSTER_TOKEN_FILE reads the join token from the mounted file, so
// the token is not put in its environment (operator#721). Any other answer keeps the variable: an image
// that predates zetta#770 would otherwise mint its own token and split the cluster.
func TestTheClusterTokenStaysOutOfTheEnvironmentOfAnImageThatReadsItsFile(t *testing.T) {
	cluster := newZettaTestCluster()
	image := zettaImage(cluster)

	features := parseZettaImageFeatures(image, testZettaUsageTokenFile, 0)
	if !features.ClusterTokenFile || !zettaImageReadsClusterTokenFile(cluster, features) {
		t.Fatalf("an image listing ZETTA_CLUSTER_TOKEN_FILE is not read as reading it: %+v", features)
	}
	for name, other := range map[string]bool{
		"an older usage":              parseZettaImageFeatures(image, "  ZETTA_CLUSTER_TOKEN Join token\n", 0).ClusterTokenFile,
		"a usage that exits non-zero": parseZettaImageFeatures(image, testZettaUsageTokenFile, 1).ClusterTokenFile,
		"another image's answer":      zettaImageReadsClusterTokenFile(cluster, parseZettaImageFeatures("oramasearch/zetta:other", testZettaUsageTokenFile, 0)),
		"no answer yet":               zettaImageReadsClusterTokenFile(cluster, nil),
	} {
		if other {
			t.Errorf("%s reads as reading the token file", name)
		}
	}

	fileOnly := renderZettaWithIdentity(t, cluster, zettaIdentityRender{tokenFileOnly: true}).Spec.Template.Spec.Containers[0].Env
	if findEnv(fileOnly, "ZETTA_CLUSTER_TOKEN") != nil {
		t.Fatal("an image that reads the token file still gets ZETTA_CLUSTER_TOKEN")
	}
	if file := findEnv(fileOnly, zettaClusterTokenFileEnv); file == nil || file.Value != zettaClusterTokenFile {
		t.Fatalf("ZETTA_CLUSTER_TOKEN_FILE = %+v, want %s", file, zettaClusterTokenFile)
	}

	legacy := renderZettaWithIdentity(t, cluster, zettaIdentityRender{}).Spec.Template.Spec.Containers[0].Env
	token := findEnv(legacy, "ZETTA_CLUSTER_TOKEN")
	if token == nil || token.ValueFrom == nil || token.ValueFrom.SecretKeyRef == nil ||
		token.ValueFrom.SecretKeyRef.Name != cluster.Name+"-zetta-cluster" || token.ValueFrom.SecretKeyRef.Key != "token" {
		t.Fatalf("an image not known to read the file lost ZETTA_CLUSTER_TOKEN: %+v", token)
	}
	// The variable keeps its place right after the file's, so an image that still gets it renders the
	// pod template it had before this change and does not roll.
	for i, variable := range legacy {
		if variable.Name == zettaClusterTokenFileEnv {
			if i+1 >= len(legacy) || legacy[i+1].Name != "ZETTA_CLUSTER_TOKEN" {
				t.Fatalf("ZETTA_CLUSTER_TOKEN moved: %v", legacy)
			}
		}
	}
}
