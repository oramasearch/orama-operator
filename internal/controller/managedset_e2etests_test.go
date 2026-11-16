/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const gateImage = "oramasearch/lake-e2e@sha256:4a5b"

func gateTest(spec string) oramav1alpha1.SetE2ETest {
	if spec == "" {
		spec = `{"image":"` + gateImage + `","runID":"v0.4.0","env":[{"name":"LAKE_E2E_CLERK_SECRET_KEY","valueFrom":{"secretKeyRef":{"name":"clerk-secret-key","key":"secretKey"}}}]}`
	}
	return oramav1alpha1.SetE2ETest{Name: "testing-gate", Member: "staging", Spec: *rawJSON(spec)}
}

func gateSet(tests ...oramav1alpha1.SetE2ETest) *oramav1alpha1.LakeSet {
	set := nonprodLakeSet()
	set.Spec.E2ETests = tests
	return set
}

// Every file in config/clusters is applied to the managing cluster, but a LakeE2ETest runs on the
// cluster of the Lake it tests. A declared run is applied there after its member, as a
// cluster-scoped LakeE2ETest with spec.member naming the set and the member, and its result comes
// back into the set's status. It never changes the set's phase.
func TestALakeSetRunsItsE2ETestOnItsMembersCluster(t *testing.T) {
	members := &fakeMembers{missing: true}
	c := setTestClient(t, append(nonprodObjects(), gateSet(gateTest("")))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	if len(members.e2eApplied) != 0 {
		t.Fatal("a run was applied before its member")
	}
	got := getLakeSet(t, c, "lake-nonprod").Status.E2ETests
	if len(got) != 1 || got[0].Phase != e2ePhasePending || !strings.Contains(got[0].Message, "waiting for its member to be applied") || got[0].Cluster != "" {
		t.Fatalf("waiting run: %+v", got)
	}

	members = &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}, e2eStatus: map[string]map[string]any{
		"testing-gate": {"phase": "Running", "observedGeneration": int64(1), "jobName": "testing-gate-e2e-1", "message": "run 1 running against Lake staging"}}}
	result, err := reconcileLakeSet(t, c, members, "lake-nonprod")
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("a running test is looked at again soon: %v, %v", result, err)
	}
	if len(members.e2eApplied) != 1 {
		t.Fatalf("applied %d runs", len(members.e2eApplied))
	}
	req := members.e2eApplied[0]
	if req.Kind != lakeE2EKind || req.CRD != lakeE2ECRD || req.Name != "testing-gate" || req.ObjectNamespace != "" ||
		req.Namespace != "lake-staging" || req.Owner != "LakeSet.lake-nonprod" || len(req.Secrets) != 0 {
		t.Fatalf("request: %+v", req)
	}
	member, _ := req.Spec["member"].(map[string]any)
	if member["lakeSet"] != "lake-nonprod" || member["name"] != "staging" || req.Spec["image"] != gateImage || req.Spec["runID"] != "v0.4.0" {
		t.Fatalf("spec: %+v", req.Spec)
	}
	if env, _ := req.Spec["env"].([]any); len(env) != 1 {
		t.Fatalf("the Clerk key is passed through: %+v", req.Spec["env"])
	}
	set := getLakeSet(t, c, "lake-nonprod")
	if set.Status.Phase != clusterPhaseReady {
		t.Fatalf("a run does not change the set's phase: %+v", set.Status.ManagedSetStatus)
	}
	if got := set.Status.E2ETests[0]; got.Phase != e2ePhaseRunning || got.Cluster != "example-nonprod" || got.JobName != "testing-gate-e2e-1" {
		t.Fatalf("running: %+v", got)
	}

	members.e2eStatus["testing-gate"] = map[string]any{"phase": "Failed", "observedGeneration": int64(1), "jobName": "testing-gate-e2e-1",
		"runID": "1a2b3c4d-uid", "lakeRevision": "89968edc", "pass": int64(311), "fail": int64(22), "skip": int64(3), "message": "22 steps failed"}
	if result, err = reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil || result.RequeueAfter != leafOperatorSteadyRecheck {
		t.Fatalf("a finished run is not polled: %v, %v", result, err)
	}
	set = getLakeSet(t, c, "lake-nonprod")
	if got := set.Status.E2ETests[0]; got.Phase != e2ePhaseFailed || got.Pass != 311 || got.Fail != 22 || got.Skip != 3 ||
		got.LakeRevision != "89968edc" || got.RunID != "1a2b3c4d-uid" || got.Message != "22 steps failed" {
		t.Fatalf("finished: %+v", got)
	}
	if set.Status.E2ESummary != "testing-gate Failed 311/22/3" || set.Status.Phase != clusterPhaseReady {
		t.Fatalf("summary %q, phase %s", set.Status.E2ESummary, set.Status.Phase)
	}
}

// A changed spec (the next release's runID) is a new generation on the cluster. Until that run
// starts, the previous run's result is not reported as this one's.
func TestALakeSetDoesNotReportThePreviousRunAsTheLatest(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}, e2eGen: map[string]int64{"testing-gate": 2},
		e2eStatus: map[string]map[string]any{"testing-gate": {"phase": "Succeeded", "observedGeneration": int64(1), "pass": int64(336)}}}
	c := setTestClient(t, append(nonprodObjects(), gateSet(gateTest("")))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	got := getLakeSet(t, c, "lake-nonprod").Status.E2ETests[0]
	if got.Phase != e2ePhasePending || got.Pass != 0 || !strings.Contains(got.Message, "previous run was Succeeded") {
		t.Fatalf("stale result: %+v", got)
	}
}

// The set fills in spec.member; a test that targets something else is refused and nothing is
// applied. The members are applied all the same.
func TestALakeSetRefusesAnE2ETestThatTargetsAnythingButItsMember(t *testing.T) {
	for name, spec := range map[string]string{
		"targetRef":      `{"image":"` + gateImage + `","targetRef":"prod"}`,
		"another member": `{"image":"` + gateImage + `","member":{"lakeSet":"lake-nonprod","name":"dev"}}`,
		"not an object":  `["image"]`,
	} {
		members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}}
		c := setTestClient(t, append(nonprodObjects(), gateSet(gateTest(spec)))...)
		if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		set := getLakeSet(t, c, "lake-nonprod")
		if len(members.e2eApplied) != 0 || len(members.applied) != 2 {
			t.Fatalf("%s: applied %d runs and %d members", name, len(members.e2eApplied), len(members.applied))
		}
		if got := set.Status.E2ETests[0]; got.Phase != e2ePhaseBlocked || !strings.Contains(set.Status.Message, "e2e test testing-gate against staging") {
			t.Fatalf("%s: %+v, message %q", name, got, set.Status.Message)
		}
	}
	// A spec.member that says exactly what the set would is accepted.
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}}
	c := setTestClient(t, append(nonprodObjects(), gateSet(gateTest(`{"image":"`+gateImage+`","member":{"lakeSet":"lake-nonprod","name":"staging"}}`)))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil || len(members.e2eApplied) != 1 {
		t.Fatalf("matching member: %v, applied %d", err, len(members.e2eApplied))
	}
}

// A member cluster whose operator predates LakeE2ETest is waited for, not failed.
func TestALakeSetWaitsForAClusterWithoutTheE2EKind(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}, e2eMissing: true}
	c := setTestClient(t, append(nonprodObjects(), gateSet(gateTest("")))...)
	result, err := reconcileLakeSet(t, c, members, "lake-nonprod")
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("%v, %v", result, err)
	}
	if got := getLakeSet(t, c, "lake-nonprod").Status.E2ETests[0]; got.Phase != e2ePhasePending || !strings.Contains(got.Message, lakeE2ECRD) || got.Cluster != "" {
		t.Fatalf("%+v", got)
	}
}

// Removing an entry deletes its LakeE2ETest on the member's cluster (its Job goes with it); one
// the cluster will not delete yet stays reported as Removing and is tried again. Deleting the set
// deletes its runs before the set is released.
func TestRemovingAnE2ETestOrTheSetDeletesItOnTheCluster(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true},
		e2eStatus: map[string]map[string]any{"testing-gate": {"phase": "Succeeded", "observedGeneration": int64(1)}}}
	c := setTestClient(t, append(nonprodObjects(), gateSet(gateTest("")))...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}

	set := getLakeSet(t, c, "lake-nonprod")
	set.Spec.E2ETests = nil
	if err := c.Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	members.e2eDeleteErr = errors.New("the cluster refused")
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err == nil {
		t.Fatal("a refused delete is retried")
	}
	set = getLakeSet(t, c, "lake-nonprod")
	if len(set.Status.E2ETests) != 1 || set.Status.E2ETests[0].Phase != e2ePhaseRemoving || set.Status.E2ESummary != "" {
		t.Fatalf("still being removed: %+v, summary %q", set.Status.E2ETests, set.Status.E2ESummary)
	}
	members.e2eDeleteErr = nil
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	if set = getLakeSet(t, c, "lake-nonprod"); len(set.Status.E2ETests) != 0 {
		t.Fatalf("removed: %+v", set.Status.E2ETests)
	}
	if n := len(members.e2eDeleted); n != 2 || members.e2eDeleted[1].Name != "testing-gate" || members.e2eDeleted[1].Owner != "LakeSet.lake-nonprod" || members.e2eDeleted[1].ObjectNamespace != "" {
		t.Fatalf("deletes: %+v", members.e2eDeleted)
	}

	set.Spec.E2ETests = []oramav1alpha1.SetE2ETest{gateTest("")}
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
	var gone oramav1alpha1.LakeSet
	if err := c.Get(context.Background(), types.NamespacedName{Name: "lake-nonprod"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the set was not released: %v", err)
	}
	if n := len(members.e2eDeleted); n != 3 {
		t.Fatalf("deleting the set deleted %d runs in all", n)
	}
}
