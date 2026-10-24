/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func rehearsalEntry(name, member, spec string) oramav1alpha1.SetRestoreRehearsal {
	return oramav1alpha1.SetRestoreRehearsal{Name: name, Member: member, Spec: *rawJSON(spec)}
}

func rehearsalSet(rehearsals ...oramav1alpha1.SetRestoreRehearsal) *oramav1alpha1.LakeSet {
	set := nonprodLakeSet()
	set.Spec.RestoreRehearsals = rehearsals
	return set
}

// Every file in config/clusters is applied to the managing cluster, but a LakeRestoreRehearsal
// runs on the cluster of the Lake it rehearses. A declared rehearsal is applied there after its
// member, as a cluster-scoped LakeRestoreRehearsal with spec.lakeRef set to the member's Lake, and
// its outcome comes back into the set's status. It never changes the set's phase.
func TestALakeSetRunsItsRestoreRehearsalOnItsMembersCluster(t *testing.T) {
	entry := rehearsalEntry("staging-store-restore-20261024", "staging", `{"service":"store","operation":"restore","from":"latest"}`)
	members := &fakeMembers{missing: true}
	c := setTestClient(t, append(nonprodObjects(), rehearsalSet(entry))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	if len(members.rehearsalApplied) != 0 {
		t.Fatal("a rehearsal was applied before its member")
	}
	got := getLakeSet(t, c, "lake-nonprod").Status.RestoreRehearsals
	if len(got) != 1 || got[0].Phase != e2ePhasePending || !strings.Contains(got[0].Message, "waiting for its member to be applied") {
		t.Fatalf("waiting rehearsal: %+v", got)
	}

	members = &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}, rehearsalStatus: map[string]map[string]any{
		entry.Name: {"phase": "Running", "message": "selected VolumeSnapshot lake-staging-store-20261024t030000z", "selectedSnapshot": "lake-staging-store-20261024t030000z"}}}
	result, err := reconcileLakeSet(t, c, members, "lake-nonprod")
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("a running rehearsal is looked at again soon: %v, %v", result, err)
	}
	if len(members.rehearsalApplied) != 1 {
		t.Fatalf("applied %d rehearsals", len(members.rehearsalApplied))
	}
	req := members.rehearsalApplied[0]
	if req.Kind != lakeRehearsalKind || req.CRD != lakeRehearsalCRD || req.Name != entry.Name || req.ObjectNamespace != "" ||
		req.Namespace != "lake-staging" || req.Owner != "LakeSet.lake-nonprod" || len(req.Secrets) != 0 {
		t.Fatalf("request: %+v", req)
	}
	if req.Spec["lakeRef"] != "staging" || req.Spec["service"] != "store" || req.Spec["operation"] != "restore" {
		t.Fatalf("spec: %+v", req.Spec)
	}
	set := getLakeSet(t, c, "lake-nonprod")
	if set.Status.Phase != clusterPhaseReady {
		t.Fatalf("a rehearsal does not change the set's phase: %+v", set.Status.ManagedSetStatus)
	}
	if got := set.Status.RestoreRehearsals[0]; got.Phase != "Running" || got.Cluster != "orama-nonprod-cluster-01" ||
		got.Selected != "lake-staging-store-20261024t030000z" {
		t.Fatalf("running: %+v", got)
	}

	members.rehearsalStatus[entry.Name] = map[string]any{"phase": "Succeeded", "message": "store restore rehearsal succeeded using VolumeSnapshot lake-staging-store-20261024t030000z",
		"selectedSnapshot": "lake-staging-store-20261024t030000z", "evidence": "files=12 kib=340 top=tuf", "completedAt": "2026-10-24T12:00:00Z"}
	if result, err = reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil || result.RequeueAfter != leafOperatorSteadyRecheck {
		t.Fatalf("a finished rehearsal is not polled: %v, %v", result, err)
	}
	got = getLakeSet(t, c, "lake-nonprod").Status.RestoreRehearsals
	if got[0].Phase != "Succeeded" || got[0].Evidence != "files=12 kib=340 top=tuf" || got[0].CompletedAt == nil {
		t.Fatalf("finished: %+v", got[0])
	}
}

// The set fills in spec.lakeRef with the member's Lake: an entry naming another Lake is refused,
// and nothing is applied. The members are applied all the same.
func TestALakeSetRefusesARehearsalOfAnotherLake(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}}
	c := setTestClient(t, append(nonprodObjects(), rehearsalSet(rehearsalEntry("r", "staging", `{"lakeRef":"dev","service":"postgres","operation":"restore"}`)))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	set := getLakeSet(t, c, "lake-nonprod")
	if len(members.rehearsalApplied) != 0 || len(members.applied) != 2 {
		t.Fatalf("applied %d rehearsals and %d members", len(members.rehearsalApplied), len(members.applied))
	}
	if got := set.Status.RestoreRehearsals[0]; got.Phase != e2ePhaseBlocked || !strings.Contains(set.Status.Message, "restore rehearsal r of staging") {
		t.Fatalf("%+v, message %q", got, set.Status.Message)
	}
}

// A rehearsal's spec is immutable on its cluster. An entry changed after it was applied is
// reported as Blocked with what the cluster still has, and is not retried as an error.
func TestALakeSetReportsAChangedRehearsalInsteadOfRetrying(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true},
		rehearsalSpec:   map[string]map[string]any{"r": {"lakeRef": "staging", "service": "postgres", "operation": "restore", "from": "latest"}},
		rehearsalStatus: map[string]map[string]any{"r": {"phase": "Failed", "message": "control-plane backups are disabled"}}}
	c := setTestClient(t, append(nonprodObjects(), rehearsalSet(rehearsalEntry("r", "staging", `{"service":"store","operation":"restore"}`)))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatalf("a changed rehearsal is reported, not retried: %v", err)
	}
	got := getLakeSet(t, c, "lake-nonprod").Status.RestoreRehearsals[0]
	if got.Phase != "Failed" || !strings.Contains(got.Message, "cannot change") || got.Cluster == "" {
		t.Fatalf("%+v", got)
	}
}

// A member cluster whose operator predates LakeRestoreRehearsal is waited for, not failed.
func TestALakeSetWaitsForAClusterWithoutTheRehearsalKind(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}, rehearsalMissing: true}
	c := setTestClient(t, append(nonprodObjects(), rehearsalSet(rehearsalEntry("r", "dev", `{"service":"store","operation":"restore"}`)))...)
	if result, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("%v, %v", result, err)
	}
	if got := getLakeSet(t, c, "lake-nonprod").Status.RestoreRehearsals[0]; got.Phase != e2ePhasePending || !strings.Contains(got.Message, lakeRehearsalCRD) {
		t.Fatalf("%+v", got)
	}
}

// Removing an entry deletes its rehearsal on the member's cluster; deleting the set deletes them
// before the set is released.
func TestRemovingARehearsalOrTheSetDeletesItOnTheCluster(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true},
		rehearsalStatus: map[string]map[string]any{"r": {"phase": "Succeeded"}}}
	c := setTestClient(t, append(nonprodObjects(), rehearsalSet(rehearsalEntry("r", "dev", `{"service":"store","operation":"restore"}`)))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	set := getLakeSet(t, c, "lake-nonprod")
	set.Spec.RestoreRehearsals = nil
	if err := c.Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	if set = getLakeSet(t, c, "lake-nonprod"); len(set.Status.RestoreRehearsals) != 0 {
		t.Fatalf("removed: %+v", set.Status.RestoreRehearsals)
	}
	if len(members.rehearsalDeleted) != 1 || members.rehearsalDeleted[0].Name != "r" || members.rehearsalDeleted[0].ObjectNamespace != "" {
		t.Fatalf("deletes: %+v", members.rehearsalDeleted)
	}

	set.Spec.RestoreRehearsals = []oramav1alpha1.SetRestoreRehearsal{rehearsalEntry("r2", "dev", `{"service":"store","operation":"restore"}`)}
	if err := c.Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), getLakeSet(t, c, "lake-nonprod")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	if n := len(members.rehearsalDeleted); n != 2 || members.rehearsalDeleted[1].Name != "r2" {
		t.Fatalf("deletes on set deletion: %+v", members.rehearsalDeleted)
	}
}
