// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/oramasearch/orama-operator/internal/crdrollout"
)

// A field the previous release's CRDs declare and chart/crds no longer does is pruned by the API
// server from every stored object when the CRD is applied. Both install paths make that safe for
// the operator still running (they apply the CRDs widened first and exactly only after the roll,
// operator#543), but a removal is still a change to every resource on every cluster, and a field
// whose type changes cannot be made safe that way at all. So each one must be named here, with
// the reason it is safe, before it ships. docs/operations/crd-rollout.md has the rule.
//
// The key is crdrollout.Change.Key(): "<crd> <version> <field path> <what>". Clear the list when a
// release is tagged, in the pull request that merges main back into dev; an entry that no longer
// differs from the last release fails the test.
var acknowledgedCRDChanges = map[string]string{
	"observabilityagents.orama.cloud CRD removed":    retiredPixie,
	"observabilityhubs.orama.cloud CRD removed":      retiredPixie,
	"observabilityproviders.orama.cloud CRD removed": retiredPixie,
	"pixieclouds.orama.cloud CRD removed":            retiredPixie,
	"pixieviziers.orama.cloud CRD removed":           retiredPixie,
}

const retiredPixie = "Pixie is removed (operator#1192), with the observability kinds built on it. Its objects are deleted under the release that ships it before this one rolls: make check-crds fails while any exists (hack/retired-crds.txt)."

// lastRelease is the newest release tag behind HEAD, or "" outside a git checkout. A tag on HEAD
// itself is skipped: at a release's own tag the changes it carries are measured against the release
// before it, so the tagged commit passes with the list that release shipped with.
func lastRelease(t *testing.T) string {
	t.Helper()
	if err := exec.Command("git", "rev-parse", "--git-dir").Run(); err != nil {
		return ""
	}
	args := []string{"describe", "--tags", "--abbrev=0", "--match", "v[0-9]*"}
	own, err := exec.Command("git", "tag", "--points-at", "HEAD", "--list", "v[0-9]*").Output()
	if err != nil {
		t.Fatalf("git tag --points-at HEAD: %v", err)
	}
	for _, tag := range strings.Fields(string(own)) {
		args = append(args, "--exclude", tag)
	}
	out, err := exec.Command("git", append(args, "HEAD")...).Output()
	if err != nil {
		t.Fatalf("no release tag behind HEAD (fetch the tags: git fetch --tags): %v", err)
	}
	return strings.TrimSpace(string(out))
}

func crdsAt(t *testing.T, tag string) map[string]map[string]any {
	t.Helper()
	list, err := exec.Command("git", "ls-tree", "--name-only", tag, "../chart/crds/").Output()
	if err != nil {
		t.Fatalf("git ls-tree %s: %v", tag, err)
	}
	out := map[string]map[string]any{}
	for _, path := range strings.Fields(string(list)) {
		if !strings.HasSuffix(path, ".yaml") {
			continue
		}
		data, err := exec.Command("git", "show", tag+":"+path).Output()
		if err != nil {
			t.Fatalf("git show %s:%s: %v", tag, path, err)
		}
		crds, err := crdrollout.Parse(tag+":"+path, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, crd := range crds {
			out[crdrollout.Name(crd)] = crd
		}
	}
	return out
}

func chartCRDsNow(t *testing.T) map[string]map[string]any {
	t.Helper()
	files, err := filepath.Glob("../chart/crds/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no chart CRDs: %v", err)
	}
	out := map[string]map[string]any{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		crds, err := crdrollout.Parse(file, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, crd := range crds {
			out[crdrollout.Name(crd)] = crd
		}
	}
	return out
}

func TestNoCRDFieldThePreviousReleaseReadsDisappearsUnnoticed(t *testing.T) {
	tag := lastRelease(t)
	if tag == "" {
		t.Skip("not a git checkout: the previous release's CRDs cannot be read")
	}
	previous, now := crdsAt(t, tag), chartCRDsNow(t)
	seen := map[string]bool{}
	var unacknowledged []string
	names := make([]string, 0, len(previous))
	for name := range previous {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var changes []crdrollout.Change
		if next, ok := now[name]; ok {
			changes = crdrollout.Changes(previous[name], next)
		} else {
			changes = []crdrollout.Change{{CRD: name, What: "CRD removed"}}
		}
		for _, change := range changes {
			key := change.Key()
			seen[key] = true
			if _, ok := acknowledgedCRDChanges[key]; !ok {
				unacknowledged = append(unacknowledged, key)
			}
		}
	}
	if len(unacknowledged) > 0 {
		t.Errorf("chart/crds drops or retypes what %s declares, and the previous operator may read it:\n  %s\n"+
			"Read docs/operations/crd-rollout.md (Removing or changing a field). A type change or a change of meaning "+
			"needs a new field instead. A removal that is safe is named in acknowledgedCRDChanges with why.",
			tag, strings.Join(unacknowledged, "\n  "))
	}
	for key := range acknowledgedCRDChanges {
		if !seen[key] {
			t.Errorf("acknowledgedCRDChanges names %q, which does not differ from %s: remove the entry", key, tag)
		}
	}
}

// Every CRD this change removes is in hack/retired-crds.txt, so make check-crds refuses a roll while
// objects of it remain: a removed kind's objects have no controller after the roll, and one with a
// finalizer can never be deleted again.
func TestEveryRemovedCRDIsCheckedBeforeTheRoll(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "hack", "retired-crds.txt"))
	if err != nil {
		t.Fatal(err)
	}
	retired := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			retired[line] = true
		}
	}
	for key := range acknowledgedCRDChanges {
		if crd, ok := strings.CutSuffix(key, " CRD removed"); ok && !retired[crd] {
			t.Errorf("%s is removed from chart/crds but not listed in hack/retired-crds.txt", crd)
		}
	}
	now := chartCRDsNow(t)
	for crd := range retired {
		if _, ok := now[crd]; ok {
			t.Errorf("hack/retired-crds.txt lists %s, which chart/crds still ships", crd)
		}
	}
}
