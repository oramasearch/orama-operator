// SPDX-License-Identifier: MIT

package controller

import (
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The Versions column names each member and its image tag, skipping members with no version yet
// (orama-operator#983).
func TestMemberVersionsNamesEachMemberAndItsTag(t *testing.T) {
	got := memberVersions([]oramav1alpha1.SetMemberStatus{
		{Name: "dev", Version: "oramasearch/lake:dev-aa9fefd5"},
		{Name: "testing", Version: "docker.io/oramasearch/lake:v0.7.0"},
		{Name: "staging"},
		{Name: "pinned", Version: "oramasearch/lake@sha256:abc"},
		{Name: "registry", Version: "localhost:5000/lake"},
		{Name: "v1-dev", Version: "v1-sandbox"},
	})
	want := "dev=dev-aa9fefd5, testing=v0.7.0, pinned=sha256:abc, registry=localhost:5000/lake, v1-dev=v1-sandbox"
	if got != want {
		t.Fatalf("memberVersions = %q, want %q", got, want)
	}
}
