/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const pilotOwner = "OramaCloudV1Set.v1-nonprod"

func pilotMigration(name string, secrets ...string) oramav1alpha1.SetMigration {
	migration := oramav1alpha1.SetMigration{Name: name, Member: "v1-dev",
		Spec: *rawJSON(`{"teamID":"7e57a000-0000-4000-8000-000000000001","projectIDs":["7e57b000-0000-4000-8000-000000000002"],"runID":"` + name + `","step":"plan"}`)}
	for _, secret := range secrets {
		migration.Secrets = append(migration.Secrets, oramav1alpha1.SetSecret{Name: secret, Namespace: "lake"})
	}
	return migration
}

// pilotSet is v1-nonprod with one member, v1-dev, and the pilot's migration into it.
func pilotSet(migrations ...oramav1alpha1.SetMigration) *oramav1alpha1.OramaCloudV1Set {
	if migrations == nil {
		migrations = []oramav1alpha1.SetMigration{pilotMigration("pilot-1", "v1-nonprod-prod1-db-ro", "team-7e57a000-evidence")}
	}
	return &oramav1alpha1.OramaCloudV1Set{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-nonprod"},
		Spec: oramav1alpha1.OramaCloudV1SetSpec{
			ManagedSetSpec: oramav1alpha1.ManagedSetSpec{
				Template: *rawJSON(`{"profile":"light","images":{"tag":"v1-sandbox"}}`),
				Secrets:  []oramav1alpha1.SetSecret{{Name: "oramasearch-dockerhub", Namespace: "lake"}},
				Clusters: []oramav1alpha1.SetMember{{ClusterRef: "orama-nonprod-cluster-01", Name: "v1-dev", Namespace: "v1-dev",
					Secrets: []oramav1alpha1.SetSecret{{Name: "v1-nonprod-llm-keys", Namespace: "lake"}}}},
				DeletionPolicy: oramav1alpha1.SetDeletionRetain,
			},
			Migrations: migrations,
		},
	}
}

func pilotObjects() []client.Object {
	objects := managedCluster("orama-nonprod-cluster-01", "westus2", "nonprod")
	for _, name := range []string{"oramasearch-dockerhub", "v1-nonprod-llm-keys", "v1-nonprod-prod1-db-ro", "other-db-ro",
		"team-7e57a000-evidence", "team-2-evidence"} {
		objects = append(objects, setSecret(name, name+"-value"))
	}
	return objects
}

func reconcileV1Set(t *testing.T, c client.Client, members *fakeMembers) (ctrl.Result, error) {
	t.Helper()
	r := &OramaCloudV1SetReconciler{Client: c, Scheme: clusterTestScheme(t), Applier: members}
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1-nonprod"}})
}

func getV1Set(t *testing.T, c client.Client) *oramav1alpha1.OramaCloudV1Set {
	t.Helper()
	var set oramav1alpha1.OramaCloudV1Set
	if err := c.Get(context.Background(), types.NamespacedName{Name: "v1-nonprod"}, &set); err != nil {
		t.Fatal(err)
	}
	return &set
}

func editV1Set(t *testing.T, c client.Client, edit func(*oramav1alpha1.OramaCloudV1Set)) {
	t.Helper()
	set := getV1Set(t, c)
	edit(set)
	if err := c.Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
}

func secretNames(secrets []*corev1.Secret) []string {
	var names []string
	for _, secret := range secrets {
		names = append(names, secret.Name)
	}
	return names
}

// A declared migration is applied after its member, in the member's namespace, with targetRef
// filled in and its Secrets copied and labelled; its status comes back into the set's. The set's
// phase is its members': a Blocked migration does not make it Degraded.
func TestASetAppliesItsMigrationIntoItsMember(t *testing.T) {
	members := &fakeMembers{missing: true}
	c := setTestClient(t, append(pilotObjects(), pilotSet())...)
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if len(members.migrations) != 0 {
		t.Fatal("a migration was applied before its member")
	}
	if got := getV1Set(t, c).Status.Migrations[0]; got.Phase != oramav1alpha1.MigrationPhasePending || !strings.Contains(got.Message, "waiting for its member to be applied") || got.Cluster != "" {
		t.Fatalf("waiting migration: %+v", got)
	}

	members = &fakeMembers{ready: map[string]bool{"v1-dev": true}, migrationStatus: map[string]map[string]any{
		"pilot-1": {"phase": "Running", "step": "plan", "message": "Job pilot-1-plan-0 is running"}}}
	result, err := reconcileV1Set(t, c, members)
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("a running migration is looked at again soon: %v, %v", result, err)
	}
	if len(members.applied) != 1 || len(members.migrations) != 1 {
		t.Fatalf("applied %d members and %d migrations", len(members.applied), len(members.migrations))
	}
	req := members.migrations[0]
	if req.Kind != migrationKind || req.CRD != migrationCRD || req.Name != "pilot-1" || req.Namespace != "v1-dev" || req.ObjectNamespace != "v1-dev" || req.Owner != pilotOwner {
		t.Fatalf("request: %+v", req)
	}
	if req.Spec["targetRef"] != "v1-dev" || req.Spec["step"] != "plan" || req.Spec["runID"] != "pilot-1" {
		t.Fatalf("spec: %+v", req.Spec)
	}
	if names := secretNames(req.Secrets); !slices.Equal(names, []string{"v1-nonprod-prod1-db-ro", "team-7e57a000-evidence"}) {
		t.Fatalf("a migration gets its own Secrets and not the stack's: %v", names)
	}
	for _, secret := range req.Secrets {
		if string(secret.Data["key"]) != secret.Name+"-value" || secret.Labels[managedBySetLabel] != pilotOwner {
			t.Fatalf("secret %s: data %q labels %v", secret.Name, secret.Data["key"], secret.Labels)
		}
	}

	set := getV1Set(t, c)
	want := oramav1alpha1.SetMigrationStatus{Name: "pilot-1", Member: "v1-dev", Cluster: "orama-nonprod-cluster-01", Namespace: "v1-dev",
		Step: "plan", Phase: "Running", Message: "Job pilot-1-plan-0 is running", Secrets: []string{"team-7e57a000-evidence", "v1-nonprod-prod1-db-ro"}}
	if len(set.Status.Migrations) != 1 || !equalMigrationStatus(set.Status.Migrations[0], want) {
		t.Fatalf("status.migrations:\n got %+v\nwant %+v", set.Status.Migrations, want)
	}
	if set.Status.MigrationSummary != "0/1 Succeeded" || set.Status.Phase != clusterPhaseReady || set.Status.Message != "" {
		t.Fatalf("set: summary %q phase %q message %q", set.Status.MigrationSummary, set.Status.Phase, set.Status.Message)
	}

	members.migrationStatus["pilot-1"] = map[string]any{"phase": "Succeeded", "step": "plan"}
	if result, err = reconcileV1Set(t, c, members); err != nil || result.RequeueAfter != leafOperatorSteadyRecheck {
		t.Fatalf("got %v, %v", result, err)
	}
	if set = getV1Set(t, c); set.Status.MigrationSummary != "1/1 Succeeded" {
		t.Fatalf("summary %q", set.Status.MigrationSummary)
	}

	members.migrationStatus["pilot-1"] = map[string]any{"phase": "Blocked", "step": "seed", "message": "seed needs spec.evidenceSecretRef"}
	if _, err = reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	set = getV1Set(t, c)
	if set.Status.Phase != clusterPhaseReady || set.Status.Migrations[0].Phase != oramav1alpha1.MigrationPhaseBlocked || set.Status.MigrationSummary != "0/1 Succeeded" {
		t.Fatalf("a Blocked migration changed the set: phase %q, migrations %+v", set.Status.Phase, set.Status.Migrations)
	}
}

func equalMigrationStatus(a, b oramav1alpha1.SetMigrationStatus) bool {
	return a.Name == b.Name && a.Member == b.Member && a.Cluster == b.Cluster && a.Namespace == b.Namespace &&
		a.Step == b.Step && a.Phase == b.Phase && a.Message == b.Message && slices.Equal(a.Secrets, b.Secrets)
}

// status.migrations follows the applied migration, and a pass that changes nothing writes nothing.
func TestSetMigrationStatusIsWrittenOnlyOnAChange(t *testing.T) {
	writes := 0
	c := fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(append(pilotObjects(), pilotSet())...).
		WithStatusSubresource(&oramav1alpha1.OramaCloudV1Set{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			writes++
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		}}).Build()
	members := &fakeMembers{ready: map[string]bool{"v1-dev": true}, migrationStatus: map[string]map[string]any{"pilot-1": {"phase": "Running", "step": "plan"}}}
	for range 2 {
		if _, err := reconcileV1Set(t, c, members); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatalf("two passes over the same state wrote the status %d times", writes)
	}
	members.migrationStatus["pilot-1"] = map[string]any{"phase": "Succeeded", "step": "plan"}
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if got := getV1Set(t, c).Status; writes != 2 || got.Migrations[0].Phase != oramav1alpha1.MigrationPhaseSucceeded || got.MigrationSummary != "1/1 Succeeded" {
		t.Fatalf("writes %d, status %+v", writes, got.Migrations)
	}
	members.migrationStatus["pilot-1"] = map[string]any{"phase": "Succeeded", "step": "plan", "observedGeneration": int64(0)}
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if got := getV1Set(t, c).Status.Migrations[0]; got.Phase != oramav1alpha1.MigrationPhasePending || got.Message != "the cluster's operator has not reconciled the latest settings yet" {
		t.Fatalf("a migration its cluster has not caught up with is not reported as its old phase: %+v", got)
	}
}

// A migration's Secret never replaces one the stack mounts, and one name in one member has one
// source. Two migrations may share a Secret from one source.
func TestAMigrationsSecretsNeverChangeWhatTheStackMounts(t *testing.T) {
	stack := pilotMigration("pilot-1", "v1-nonprod-prod1-db-ro")
	stack.Secrets = append(stack.Secrets, oramav1alpha1.SetSecret{Name: "team-7e57a000-evidence", Namespace: "lake", As: "v1-nonprod-llm-keys"},
		oramav1alpha1.SetSecret{Name: "team-7e57a000-evidence", Namespace: "lake", As: "oramasearch-dockerhub"})
	other := pilotMigration("pilot-2", "team-2-evidence")
	other.Secrets = append(other.Secrets, oramav1alpha1.SetSecret{Name: "other-db-ro", Namespace: "lake", As: "v1-nonprod-prod1-db-ro"})
	shared := pilotMigration("pilot-3", "v1-nonprod-prod1-db-ro", "team-2-evidence")

	for name, tc := range map[string]struct {
		migrations []oramav1alpha1.SetMigration
		refused    map[string][]string // migration → what its message says
	}{
		"a set or member secret": {
			migrations: []oramav1alpha1.SetMigration{stack, shared},
			refused: map[string][]string{"pilot-1": {
				"Secret oramasearch-dockerhub is one the stack itself gets from the set or its member, and a migration never changes what the stack mounts",
				"Secret v1-nonprod-llm-keys is one the stack itself gets"}},
		},
		"one name, two sources": {
			migrations: []oramav1alpha1.SetMigration{shared, other},
			refused: map[string][]string{
				"pilot-2": {"Secret v1-nonprod-prod1-db-ro is copied from lake/other-db-ro and lake/v1-nonprod-prod1-db-ro by migrations pilot-2, pilot-3"},
				"pilot-3": {"Secret v1-nonprod-prod1-db-ro is copied from lake/other-db-ro and lake/v1-nonprod-prod1-db-ro by migrations pilot-2, pilot-3"},
			},
		},
		"one secret, one source, two migrations": {
			migrations: []oramav1alpha1.SetMigration{pilotMigration("pilot-1", "v1-nonprod-prod1-db-ro", "team-7e57a000-evidence"), shared},
		},
	} {
		t.Run(name, func(t *testing.T) {
			members := &fakeMembers{ready: map[string]bool{"v1-dev": true}}
			c := setTestClient(t, append(pilotObjects(), pilotSet(tc.migrations...))...)
			if _, err := reconcileV1Set(t, c, members); err != nil {
				t.Fatalf("a refusal waits for an edit and is not retried: %v", err)
			}
			set := getV1Set(t, c)
			if set.Status.Phase != clusterPhaseReady {
				t.Fatalf("a refused migration changed the set's phase: %q", set.Status.Phase)
			}
			for _, entry := range set.Status.Migrations {
				applied := slices.ContainsFunc(members.migrations, func(req memberRequest) bool { return req.Name == entry.Name })
				wants, refused := tc.refused[entry.Name]
				if applied == refused {
					t.Fatalf("%s: applied %t, refused %t (%+v)", entry.Name, applied, refused, entry)
				}
				if !refused {
					continue
				}
				if entry.Phase != oramav1alpha1.MigrationPhaseBlocked || len(entry.Secrets) != 0 {
					t.Fatalf("%s: %+v", entry.Name, entry)
				}
				for _, want := range wants {
					if !strings.Contains(entry.Message, want) || !strings.Contains(set.Status.Message, "migration "+entry.Name+" into v1-dev: ") {
						t.Fatalf("%s:\n message %q\n set     %q\n want    %q", entry.Name, entry.Message, set.Status.Message, want)
					}
				}
			}
		})
	}
}

// Removing a migration from the set deletes it on its cluster, and its Secrets once it is gone,
// except one another migration still uses. It holds nothing else: the member and the other
// migrations keep being reconciled while it is held.
func TestRemovingAMigrationDeletesItThenItsSecrets(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{"v1-dev": true}, migrationStatus: map[string]map[string]any{
		"pilot-1": {"phase": "Succeeded", "step": "seed"}, "pilot-2": {"phase": "Succeeded", "step": "plan"}}}
	c := setTestClient(t, append(pilotObjects(), pilotSet(
		pilotMigration("pilot-1", "v1-nonprod-prod1-db-ro", "team-7e57a000-evidence"),
		pilotMigration("pilot-2", "v1-nonprod-prod1-db-ro", "team-2-evidence"),
	))...)
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	editV1Set(t, c, func(set *oramav1alpha1.OramaCloudV1Set) { set.Spec.Migrations = set.Spec.Migrations[1:] })

	members.held = map[string]map[string]any{"pilot-1": {"phase": "Blocked", "step": "remove-target", "message": "the team was switched to the target"}}
	members.applied, members.migrations = nil, nil
	result, err := reconcileV1Set(t, c, members)
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("got %v, %v", result, err)
	}
	if len(members.deletedMigrations) != 1 || members.deletedMigrations[0].Name != "pilot-1" || members.deletedMigrations[0].Namespace != "v1-dev" {
		t.Fatalf("deleted: %+v", members.deletedMigrations)
	}
	if names := secretNames(members.deletedMigrations[0].Secrets); !slices.Equal(names, []string{"team-7e57a000-evidence"}) {
		t.Fatalf("the Secrets to delete once it is gone are its own and not pilot-2's: %v", names)
	}
	if len(members.applied) != 1 || len(members.migrations) != 1 || members.migrations[0].Name != "pilot-2" {
		t.Fatalf("a held removal held back the member (%d) or the other migration (%+v)", len(members.applied), members.migrations)
	}
	set := getV1Set(t, c)
	if len(set.Status.Migrations) != 2 || set.Status.Migrations[1].Name != "pilot-1" || set.Status.Migrations[1].Phase != oramav1alpha1.MigrationPhaseBlocked ||
		set.Status.Migrations[1].Message != "the team was switched to the target" {
		t.Fatalf("the migration being removed is reported until it is gone: %+v", set.Status.Migrations)
	}
	if set.Status.Phase != clusterPhaseReady || set.Status.MigrationSummary != "1/1 Succeeded" {
		t.Fatalf("set: phase %q summary %q", set.Status.Phase, set.Status.MigrationSummary)
	}

	members.held = nil
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if got := getV1Set(t, c).Status.Migrations; len(got) != 1 || got[0].Name != "pilot-2" {
		t.Fatalf("a migration that is gone is still reported: %+v", got)
	}
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if len(members.deletedMigrations) != 2 {
		t.Fatalf("a migration that is gone was deleted again: %d", len(members.deletedMigrations))
	}
}

// Deleting the set, or removing a member with migrations, deletes the migrations first and waits
// for them to be gone before the member: a stack deleted under a migration would skip its
// removal guard for a switched team.
func TestDeletingASetOrAMemberWaitsForItsMigrations(t *testing.T) {
	t.Run("the set", func(t *testing.T) {
		set := pilotSet()
		set.Spec.DeletionPolicy = oramav1alpha1.SetDeletionDelete
		members := &fakeMembers{ready: map[string]bool{"v1-dev": true}, migrationStatus: map[string]map[string]any{"pilot-1": {"phase": "Succeeded", "step": "seed"}}}
		c := setTestClient(t, append(pilotObjects(), set)...)
		if _, err := reconcileV1Set(t, c, members); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(context.Background(), getV1Set(t, c)); err != nil {
			t.Fatal(err)
		}
		members.held = map[string]map[string]any{"pilot-1": {"phase": "Blocked", "message": "the team was switched to the target"}}
		if _, err := reconcileV1Set(t, c, members); err != nil {
			t.Fatal(err)
		}
		if len(members.deletedMigrations) != 1 || len(members.deleted) != 0 {
			t.Fatalf("deleted migrations %d, members %v: the member went before its migration", len(members.deletedMigrations), members.deleted)
		}
		got := getV1Set(t, c)
		if want := "waiting for its migrations to be removed: pilot-1 into v1-dev (Blocked: the team was switched to the target)"; got.Status.Phase != clusterPhaseDeleting || got.Status.Message != want {
			t.Fatalf("phase %q message %q", got.Status.Phase, got.Status.Message)
		}

		members.held = nil
		if _, err := reconcileV1Set(t, c, members); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(members.deleted, []string{"v1-dev"}) {
			t.Fatalf("deleted members %v", members.deleted)
		}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "v1-nonprod"}, &oramav1alpha1.OramaCloudV1Set{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the set was not released: %v", err)
		}
	})

	t.Run("a member", func(t *testing.T) {
		staging := pilotMigration("staging-1", "team-2-evidence")
		staging.Member = "v1-staging"
		set := pilotSet(pilotMigration("pilot-1"), staging)
		set.Spec.DeletionPolicy = oramav1alpha1.SetDeletionDelete
		set.Spec.Clusters = append(set.Spec.Clusters, oramav1alpha1.SetMember{ClusterRef: "orama-nonprod-cluster-01", Name: "v1-staging"})
		members := &fakeMembers{ready: map[string]bool{"v1-dev": true, "v1-staging": true}}
		c := setTestClient(t, append(pilotObjects(), set)...)
		if _, err := reconcileV1Set(t, c, members); err != nil {
			t.Fatal(err)
		}
		editV1Set(t, c, func(set *oramav1alpha1.OramaCloudV1Set) {
			set.Spec.Clusters, set.Spec.Migrations = set.Spec.Clusters[:1], set.Spec.Migrations[:1]
		})
		members.held = map[string]map[string]any{"staging-1": {"phase": "Removing", "message": "deleting the run volume staging-1-runs"}}
		if _, err := reconcileV1Set(t, c, members); err != nil {
			t.Fatal(err)
		}
		if len(members.deletedMigrations) != 1 || members.deletedMigrations[0].Namespace != "v1-v1-staging" || len(members.deleted) != 0 {
			t.Fatalf("deleted migrations %+v, members %v", members.deletedMigrations, members.deleted)
		}
		got := getV1Set(t, c).Status.MemberStatuses
		if len(got) != 2 || got[1].Name != "v1-staging" || got[1].Phase != clusterPhaseDeleting {
			t.Fatalf("the member waiting for its migration is still reported: %+v", got)
		}

		members.held = nil
		if _, err := reconcileV1Set(t, c, members); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(members.deleted, []string{"v1-staging"}) || len(getV1Set(t, c).Status.MemberStatuses) != 1 {
			t.Fatalf("deleted members %v, statuses %+v", members.deleted, getV1Set(t, c).Status.MemberStatuses)
		}
	})
}

// A change the migration's kind refuses (an immutable field), or a move to another member, is
// reported on the set and the applied migration is left as it is: never deleted to force the
// change, since deleting it removes the copied team.
func TestAnImmutableChangeIsReportedAndTheMigrationIsKept(t *testing.T) {
	set := pilotSet()
	set.Spec.Clusters = append(set.Spec.Clusters, oramav1alpha1.SetMember{ClusterRef: "orama-nonprod-cluster-01", Name: "v1-staging"})
	members := &fakeMembers{ready: map[string]bool{"v1-dev": true, "v1-staging": true},
		migrationStatus: map[string]map[string]any{"pilot-1": {"phase": "Succeeded", "step": "seed"}}}
	c := setTestClient(t, append(pilotObjects(), set)...)
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}

	refused := errors.New(`the cluster refused OramaCloudV1Migration pilot-1: OramaCloudV1Migration.orama.cloud "pilot-1" is invalid: spec: Invalid value: "object": the target, team, projects, run and source are immutable; declare a new migration instead`)
	members.migrationErr = map[string]error{"pilot-1": refused}
	editV1Set(t, c, func(set *oramav1alpha1.OramaCloudV1Set) {
		set.Spec.Migrations[0].Spec = *rawJSON(`{"teamID":"00000000-0000-0000-0000-000000000000","projectIDs":["7e57b000-0000-4000-8000-000000000002"],"runID":"pilot-1","step":"plan"}`)
	})
	if _, err := reconcileV1Set(t, c, members); !errors.Is(err, refused) {
		t.Fatalf("want the refusal returned, got %v", err)
	}
	got := getV1Set(t, c)
	if entry := got.Status.Migrations[0]; entry.Phase != oramav1alpha1.MigrationPhaseSucceeded || entry.Message != refused.Error() {
		t.Fatalf("the refusal is on the migration, which keeps the phase it has: %+v", entry)
	}
	if got.Status.Phase != clusterPhaseReady || got.Status.Message != "migration pilot-1 into v1-dev: "+refused.Error() {
		t.Fatalf("set: phase %q message %q", got.Status.Phase, got.Status.Message)
	}

	members.migrationErr, members.migrations = nil, nil
	editV1Set(t, c, func(set *oramav1alpha1.OramaCloudV1Set) {
		set.Spec.Migrations[0] = pilotMigration("pilot-1")
		set.Spec.Migrations[0].Member = "v1-staging"
	})
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	entry := getV1Set(t, c).Status.Migrations[0]
	if len(members.migrations) != 0 || entry.Namespace != "v1-dev" || !strings.Contains(entry.Message, "it was applied into namespace v1-dev on orama-nonprod-cluster-01 and a migration does not move") {
		t.Fatalf("moved to another member: applied %+v, status %+v", members.migrations, entry)
	}
	if len(members.deletedMigrations) != 0 {
		t.Fatalf("a migration was deleted to get a change in: %+v", members.deletedMigrations)
	}
}

func leafMigration(name string, labels map[string]string, finalizers ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(migrationKind)
	obj.SetName(name)
	obj.SetNamespace("v1-dev")
	obj.SetLabels(labels)
	obj.SetFinalizers(finalizers)
	obj.Object["spec"] = map[string]any{"targetRef": "v1-dev"}
	obj.Object["status"] = map[string]any{"phase": "Succeeded"}
	return obj
}

// On a member's cluster: a migration is deleted and its Secrets wait until its finalizer lets it
// go; then the ones this set copied are deleted, and no other. A refused apply returns what the
// cluster still has.
func TestDeleteMigrationWithDeletesItsSecretsOnceItIsGone(t *testing.T) {
	ctx := context.Background()
	ours := map[string]string{managedBySetLabel: pilotOwner}
	leaf := fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(
		leafMigration("pilot-1", ours, migrationFinalizer),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-nonprod-prod1-db-ro", Namespace: "v1-dev", Labels: ours}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-nonprod-llm-keys", Namespace: "v1-dev"}},
	).Build()
	req := memberRequest{Kind: migrationKind, CRD: migrationCRD, Name: "pilot-1", Namespace: "v1-dev", ObjectNamespace: "v1-dev", Owner: pilotOwner}
	for _, name := range []string{"v1-nonprod-prod1-db-ro", "v1-nonprod-llm-keys"} {
		req.Secrets = append(req.Secrets, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	secretExists := func(name string) bool {
		return leaf.Get(ctx, client.ObjectKey{Namespace: "v1-dev", Name: name}, &corev1.Secret{}) == nil
	}

	state, err := deleteMigrationWith(ctx, leaf, req)
	if err != nil || state.Object == nil || state.Object.GetDeletionTimestamp().IsZero() {
		t.Fatalf("a migration its finalizer keeps is returned, being deleted: %+v, %v", state, err)
	}
	if !secretExists("v1-nonprod-prod1-db-ro") {
		t.Fatal("a Secret was deleted while its migration was still there")
	}
	held := state.Object.DeepCopy()
	held.SetFinalizers(nil)
	if err := leaf.Update(ctx, held); err != nil {
		t.Fatal(err)
	}
	if state, err = deleteMigrationWith(ctx, leaf, req); err != nil || state.Object != nil {
		t.Fatalf("want it gone, got %+v, %v", state, err)
	}
	if secretExists("v1-nonprod-prod1-db-ro") || !secretExists("v1-nonprod-llm-keys") {
		t.Fatalf("after it went: the set's Secret exists %t, the Secret it did not copy exists %t", secretExists("v1-nonprod-prod1-db-ro"), secretExists("v1-nonprod-llm-keys"))
	}

	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	crd.SetName(migrationCRD)
	immutable := errors.New("the target, team, projects, run and source are immutable")
	leaf = fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(leafMigration("pilot-1", ours)).WithRuntimeObjects(crd).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return immutable
		}}).Build()
	req.Secrets, req.Spec = nil, map[string]any{"targetRef": "v1-dev"}
	state, err = applyMemberWith(ctx, leaf, req)
	if phase, _, _ := unstructured.NestedString(state.Object.Object, "status", "phase"); !errors.Is(err, immutable) || phase != "Succeeded" {
		t.Fatalf("want the refusal with the migration as it is, got phase %q, %v", phase, err)
	}
}
