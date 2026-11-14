// SPDX-License-Identifier: MIT

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The lake-prod restore rehearsals are committed commented out until the managing cluster's
// operator knows spec.restoreRehearsals (#127). Uncommented, they must be a valid set file: every
// entry names a member, carries no lakeRef (the set fills it in), and is a rehearsal the controller
// accepts. This is what Steve applies, so it is checked before he uncomments it.
func TestCommittedLakeProdRestoreRehearsalsAreReadyToUncomment(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(clustersDir(), "sets", "lake-prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`^  # (restoreRehearsals:|- name: |  member: |  spec: )`)
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if entry.MatchString(line) {
			line = "  " + strings.TrimPrefix(line, "  # ")
		}
		lines = append(lines, line)
	}
	var set oramav1alpha1.LakeSet
	if err := yaml.UnmarshalStrict([]byte(strings.Join(lines, "\n")), &set); err != nil {
		t.Fatalf("lake-prod.yaml with its rehearsals uncommented: %v", err)
	}
	if len(set.Spec.RestoreRehearsals) != 4 {
		t.Fatalf("want the store and Postgres rehearsals of both members, got %d", len(set.Spec.RestoreRehearsals))
	}
	members := map[string]bool{}
	for _, member := range set.Spec.Clusters {
		members[member.Name] = true
	}
	dnsLabel := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	stores := map[string]bool{}
	for _, rehearsal := range set.Spec.RestoreRehearsals {
		if !members[rehearsal.Member] || !dnsLabel.MatchString(rehearsal.Name) || len(rehearsal.Name) > 63 {
			t.Fatalf("entry %+v", rehearsal)
		}
		var spec oramav1alpha1.LakeRestoreRehearsalSpec
		if err := yaml.UnmarshalStrict(rehearsal.Spec.Raw, &spec); err != nil {
			t.Fatalf("%s: %v", rehearsal.Name, err)
		}
		if spec.LakeRef != "" {
			t.Fatalf("%s names lakeRef; the set fills it in", rehearsal.Name)
		}
		spec.LakeRef = rehearsal.Member
		lake := &oramav1alpha1.Lake{}
		enabled := true
		lake.Spec.Postgres.Enabled = &enabled
		if message := validateRehearsal(&oramav1alpha1.LakeRestoreRehearsal{Spec: spec}, lake); message != "" || spec.Operation != "restore" {
			t.Fatalf("%s: %q, operation %q", rehearsal.Name, message, spec.Operation)
		}
		if spec.Service == "store" {
			stores[rehearsal.Member] = true
		}
	}
	if !stores["shared"] || !stores["shared-us"] {
		t.Fatalf("a store rehearsal for each region: %v", stores)
	}
}
