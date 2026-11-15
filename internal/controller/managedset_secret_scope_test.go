/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Production credentials never reach a non-production member (orama-operator#787, #789; LSR-038,
// LSR-040). The checks read only the resources: each member's cluster's purpose tag, the sets that
// copy a Secret, the migration field that names one, and the Secret's own purpose label.

func orgSecret(name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orama-system", Labels: labels},
		Data: map[string][]byte{"token": []byte(name + "-value")}}
}

// v1Sets is v1-prod on a production cluster and v1-nonprod on the nonprod cluster, each copying the
// named Secrets from orama-system.
func v1Sets(prodSecrets, nonprodSecrets []string) []client.Object {
	refs := func(names []string) []oramav1alpha1.SetSecret {
		var out []oramav1alpha1.SetSecret
		for _, name := range names {
			out = append(out, oramav1alpha1.SetSecret{Name: name, Namespace: "orama-system"})
		}
		return out
	}
	set := func(name, cluster, member string, secrets []string) *oramav1alpha1.OramaCloudV1Set {
		return &oramav1alpha1.OramaCloudV1Set{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: oramav1alpha1.OramaCloudV1SetSpec{ManagedSetSpec: oramav1alpha1.ManagedSetSpec{
				Template:       *rawJSON(`{"profile":"light","images":{"tag":"v1-sandbox"}}`),
				Secrets:        refs(secrets),
				Clusters:       []oramav1alpha1.SetMember{{ClusterRef: cluster, Name: member, Namespace: member}},
				DeletionPolicy: oramav1alpha1.SetDeletionRetain,
			}},
		}
	}
	objects := append(managedCluster("orama-nonprod-cluster-01", "westus2", "nonprod"),
		managedCluster("orama-production-cluster-02", "swedencentral", "production")...)
	return append(objects, set("v1-prod", "orama-production-cluster-02", "v1-eu", prodSecrets),
		set("v1-nonprod", "orama-nonprod-cluster-01", "v1-dev", nonprodSecrets))
}

func reconcileV1SetNamed(t *testing.T, c client.Client, members *fakeMembers, name string) error {
	t.Helper()
	r := &OramaCloudV1SetReconciler{Client: c, Scheme: clusterTestScheme(t), Applier: members}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	return err
}

func v1SetNamed(t *testing.T, c client.Client, name string) *oramav1alpha1.OramaCloudV1Set {
	t.Helper()
	var set oramav1alpha1.OramaCloudV1Set
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &set); err != nil {
		t.Fatal(err)
	}
	return &set
}

// LSR-040 (#789): v1-nonprod copied the same orama-system/cloudflare-api-token as v1-prod, so the
// nonprod cluster held the production DNS credential. A Secret a production set copies is never
// copied into a nonprod member; the production set keeps getting it, so production is unchanged.
func TestAProductionSetsSecretIsNeverCopiedIntoANonprodMember(t *testing.T) {
	objects := append(v1Sets([]string{"cloudflare-api-token"}, []string{"cloudflare-api-token"}), orgSecret("cloudflare-api-token", nil))
	c := setTestClient(t, objects...)

	nonprod := &fakeMembers{ready: map[string]bool{"v1-dev": true}}
	err := reconcileV1SetNamed(t, c, nonprod, "v1-nonprod")
	want := "Secret orama-system/cloudflare-api-token is copied into production members by OramaCloudV1Set v1-prod, so it is a production credential and is never copied into a nonprod member"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want the shared production Secret refused, got %v", err)
	}
	if len(nonprod.applied) != 0 {
		t.Fatalf("the nonprod member was applied with %v", secretNames(nonprod.applied[0].Secrets))
	}
	if got := v1SetNamed(t, c, "v1-nonprod"); got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.MemberStatuses[0].Message, want) {
		t.Fatalf("status: %+v", got.Status)
	}

	prod := &fakeMembers{ready: map[string]bool{"v1-eu": true}}
	if err := reconcileV1SetNamed(t, c, prod, "v1-prod"); err != nil {
		t.Fatalf("the production set is unchanged: %v", err)
	}
	if len(prod.applied) != 1 || !slices.Equal(secretNames(prod.applied[0].Secrets), []string{"cloudflare-api-token"}) {
		t.Fatalf("production member: %+v", prod.applied)
	}
}

// The fix for #789 in the set files: v1-nonprod copies its own token, under the name its stack
// already mounts, so the copy of the production token in v1-dev is overwritten in place.
func TestANonprodSetsOwnSecretIsCopiedUnderTheSameName(t *testing.T) {
	objects := append(v1Sets([]string{"cloudflare-api-token"}, nil), orgSecret("cloudflare-api-token", nil), orgSecret("cloudflare-api-token-oramadev", nil))
	c := setTestClient(t, objects...)
	editV1SetNamed(t, c, "v1-nonprod", func(set *oramav1alpha1.OramaCloudV1Set) {
		set.Spec.Secrets = []oramav1alpha1.SetSecret{{Name: "cloudflare-api-token-oramadev", Namespace: "orama-system", As: "cloudflare-api-token"}}
	})
	members := &fakeMembers{ready: map[string]bool{"v1-dev": true}}
	if err := reconcileV1SetNamed(t, c, members, "v1-nonprod"); err != nil {
		t.Fatal(err)
	}
	copied := members.applied[0].Secrets
	if len(copied) != 1 || copied[0].Name != "cloudflare-api-token" || string(copied[0].Data["token"]) != "cloudflare-api-token-oramadev-value" {
		t.Fatalf("copied: %+v", copied)
	}
}

func editV1SetNamed(t *testing.T, c client.Client, name string, edit func(*oramav1alpha1.OramaCloudV1Set)) {
	t.Helper()
	set := v1SetNamed(t, c, name)
	edit(set)
	if err := c.Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
}

// A Secret labelled with a purpose is copied only into members of that purpose, in either
// direction. An unlabelled Secret no production set copies is copied as before.
func TestALabelledSecretIsCopiedOnlyToItsPurpose(t *testing.T) {
	for name, tc := range map[string]struct {
		set, member, label, want string
	}{
		"production into nonprod": {"v1-nonprod", "v1-dev", "production", "Secret orama-system/llm-keys is labelled orama.cloud/purpose=production and the member's cluster is nonprod: it is never copied there"},
		"nonprod into production": {"v1-prod", "v1-eu", "nonprod", "Secret orama-system/llm-keys is labelled orama.cloud/purpose=nonprod and the member's cluster is production: it is never copied there"},
		"nonprod into nonprod":    {"v1-nonprod", "v1-dev", "nonprod", ""},
		"unlabelled":              {"v1-nonprod", "v1-dev", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			labels := map[string]string{}
			if tc.label != "" {
				labels[secretPurposeLabel] = tc.label
			}
			c := setTestClient(t, append(v1Sets([]string{"llm-keys"}[:boolIndex(tc.set == "v1-prod")], []string{"llm-keys"}[:boolIndex(tc.set == "v1-nonprod")]), orgSecret("llm-keys", labels))...)
			members := &fakeMembers{ready: map[string]bool{tc.member: true}}
			err := reconcileV1SetNamed(t, c, members, tc.set)
			if tc.want == "" {
				if err != nil || len(members.applied) != 1 {
					t.Fatalf("want it copied: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(members.applied) != 0 {
				t.Fatalf("want %q, got %v (applied %d)", tc.want, err, len(members.applied))
			}
		})
	}
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

// LSR-038 (#787): a migration into v1-dev was given Prod 1's engine master key, its read-only
// database URL and the settings IAM key. Those are Prod 1's, so production, whatever the Secret is
// called. They are never copied into a non-production member: the migration is refused and nothing
// is copied. A Secret standing in for Prod 1 that is labelled nonprod is a fixture, and is copied.
func TestAMigrationsProd1CredentialsNeverReachANonprodMember(t *testing.T) {
	const source = `"source":{"databaseSecretRef":{"name":"v1-nonprod-prod1-db-ro","key":"url"},` +
		`"masterKeySecretRef":{"name":"v1-nonprod-prod1-master-key","key":"key"},` +
		`"settings":{"credentialsSecretRef":{"name":"v1-nonprod-prod1-settings-ssm"}}}`
	for _, credential := range []string{"v1-nonprod-prod1-db-ro", "v1-nonprod-prod1-master-key", "v1-nonprod-prod1-settings-ssm"} {
		t.Run(credential, func(t *testing.T) {
			migration := pilotMigration("pilot-1", credential, "team-4e01bc49-evidence")
			migration.Spec = *rawJSON(`{"teamID":"4e01bc49-b388-43c2-8986-2395a0b8fe52","runID":"pilot-1","step":"plan",` + source + `}`)
			objects := append(pilotObjects(), setSecret("v1-nonprod-prod1-master-key", "prod1-master"), setSecret("v1-nonprod-prod1-settings-ssm", "prod1-iam"))
			c := setTestClient(t, append(objects, pilotSet(migration))...)
			members := &fakeMembers{ready: map[string]bool{"v1-dev": true}}
			if _, err := reconcileV1Set(t, c, members); err != nil {
				t.Fatalf("a refusal waits for an edit and is not retried: %v", err)
			}
			if len(members.migrations) != 0 {
				t.Fatalf("the migration was applied into v1-dev with %v", secretNames(members.migrations[0].Secrets))
			}
			entry := getV1Set(t, c).Status.Migrations[0]
			want := "Secret lake/" + credential + " is this migration's Prod 1 source credential, which is production, and is never copied into a nonprod member"
			if entry.Phase != oramav1alpha1.MigrationPhaseBlocked || !strings.Contains(entry.Message, want) || len(entry.Secrets) != 0 {
				t.Fatalf("migration: %+v", entry)
			}

			// A fixture labelled for nonprod is copied.
			var fixture corev1.Secret
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: credential}, &fixture); err != nil {
				t.Fatal(err)
			}
			fixture.Labels = map[string]string{secretPurposeLabel: "nonprod"}
			if err := c.Update(context.Background(), &fixture); err != nil {
				t.Fatal(err)
			}
			if _, err := reconcileV1Set(t, c, members); err != nil {
				t.Fatal(err)
			}
			if len(members.migrations) != 1 || !slices.Contains(secretNames(members.migrations[0].Secrets), credential) {
				t.Fatalf("the labelled fixture was not copied: %+v", members.migrations)
			}
		})
	}
}

// The same migration into a production member is copied: production credentials stay in
// production.
func TestAMigrationsProd1CredentialsReachAProductionMember(t *testing.T) {
	migration := pilotMigration("pilot-1", "v1-nonprod-prod1-db-ro")
	migration.Spec = *rawJSON(`{"teamID":"t","runID":"pilot-1","step":"plan","source":{"databaseSecretRef":{"name":"v1-nonprod-prod1-db-ro","key":"url"}}}`)
	set := pilotSet(migration)
	set.Spec.Clusters[0].ClusterRef = "orama-production-cluster-02"
	objects := append(pilotObjects(), managedCluster("orama-production-cluster-02", "swedencentral", "production")...)
	c := setTestClient(t, append(objects, set)...)
	members := &fakeMembers{ready: map[string]bool{"v1-dev": true}}
	if _, err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if len(members.migrations) != 1 {
		t.Fatalf("the production migration was not applied: %+v", getV1Set(t, c).Status.Migrations)
	}
}

// TestCommittedSetsAndOperatorsNeverShareASecretAcrossPurposes is the static form of the rule over
// the files in config/clusters/sets and config/clusters/<cluster>/operator.yaml (LSR-040's
// "statically list every Secret source per set and assert the prod and nonprod sets are disjoint",
// and orama-operator#832 for the OramaOperators' pull Secrets). A set's purpose is its clusters'
// purpose tag, read from config/clusters/<cluster>/cluster.yaml; an OramaOperator's is its cluster's.
func TestCommittedSetsAndOperatorsNeverShareASecretAcrossPurposes(t *testing.T) {
	root := filepath.Join("..", "..", "config", "clusters")
	purposeOf := map[string]string{}
	clusterFiles, err := filepath.Glob(filepath.Join(root, "*", "cluster.yaml"))
	if err != nil || len(clusterFiles) == 0 {
		t.Fatalf("no cluster files: %v", err)
	}
	for _, file := range clusterFiles {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var cluster oramav1alpha1.KubernetesCluster
		if err := yaml.Unmarshal(raw, &cluster); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		purposeOf[cluster.Name] = cluster.Spec.Tags[clusterPurposeTag]
	}
	setFiles, err := filepath.Glob(filepath.Join(root, "sets", "*.yaml"))
	if err != nil || len(setFiles) == 0 {
		t.Fatalf("no set files: %v", err)
	}
	usedBy := map[string]map[string][]string{} // source → purpose → set files
	for _, file := range setFiles {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var set oramav1alpha1.OramaCloudV1Set // a LakeSet's spec is its ManagedSetSpec, which this embeds
		if err := yaml.Unmarshal(raw, &set); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		purpose := ""
		for _, member := range set.Spec.Clusters {
			if purpose = purposeOf[member.ClusterRef]; purpose == "" {
				t.Fatalf("%s: member %s is on %s, which has no cluster file with a purpose tag", file, member.Name, member.ClusterRef)
			}
		}
		lists := [][]oramav1alpha1.SetSecret{set.Spec.Secrets}
		for _, member := range set.Spec.Clusters {
			lists = append(lists, member.Secrets)
		}
		for _, migration := range set.Spec.Migrations {
			lists = append(lists, migration.Secrets)
		}
		for _, list := range lists {
			for _, ref := range list {
				key := ref.Namespace + "/" + ref.Name
				if usedBy[key] == nil {
					usedBy[key] = map[string][]string{}
				}
				usedBy[key][purpose] = append(usedBy[key][purpose], filepath.Base(file))
			}
		}
	}
	operatorFiles, err := filepath.Glob(filepath.Join(root, "*", "operator.yaml"))
	if err != nil || len(operatorFiles) == 0 {
		t.Fatalf("no operator files: %v", err)
	}
	for _, file := range operatorFiles {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var operator oramav1alpha1.OramaOperator
		if err := yaml.Unmarshal(raw, &operator); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		purpose := purposeOf[operator.Spec.ClusterRef]
		if purpose == "" {
			t.Fatalf("%s: cluster %s has no cluster file with a purpose tag", file, operator.Spec.ClusterRef)
		}
		ref := operator.Spec.ImagePullSecretRef
		key := ref.Namespace + "/" + ref.Name
		if usedBy[key] == nil {
			usedBy[key] = map[string][]string{}
		}
		usedBy[key][purpose] = append(usedBy[key][purpose], filepath.Join(filepath.Base(filepath.Dir(file)), "operator.yaml"))
	}
	var crossing []string
	for source, purposes := range usedBy {
		if len(purposes) > 1 {
			crossing = append(crossing, source+" "+describePurposes(purposes))
		}
	}
	sort.Strings(crossing)
	if len(crossing) > 0 {
		t.Fatalf("Secrets copied by sets or OramaOperators of more than one purpose; nonprod gets its own:\n  %s", strings.Join(crossing, "\n  "))
	}
}

func describePurposes(purposes map[string][]string) string {
	var parts []string
	for purpose, files := range purposes {
		parts = append(parts, purpose+": "+strings.Join(slices.Compact(slices.Sorted(slices.Values(files))), ", "))
	}
	sort.Strings(parts)
	return "(" + strings.Join(parts, "; ") + ")"
}

// lakeSets is lake-prod on a production cluster and lake-nonprod on the nonprod cluster, each
// copying the named Secrets from the lake namespace, with the Stripe and OpenAI Secrets the Lakes
// read (orama-operator#790, LSR-041).
func lakeSets(prodSecrets, nonprodSecrets []string) []client.Object {
	refs := func(names []string) []oramav1alpha1.SetSecret {
		var out []oramav1alpha1.SetSecret
		for _, name := range names {
			out = append(out, oramav1alpha1.SetSecret{Name: name, Namespace: "lake"})
		}
		return out
	}
	set := func(name, cluster, member string, secrets []string) *oramav1alpha1.LakeSet {
		return &oramav1alpha1.LakeSet{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: oramav1alpha1.LakeSetSpec{ManagedSetSpec: oramav1alpha1.ManagedSetSpec{
				Template:       *rawJSON(`{"image":"oramasearch/lake:v0.3.1"}`),
				Secrets:        refs(secrets),
				Clusters:       []oramav1alpha1.SetMember{{ClusterRef: cluster, Name: member, Spec: rawJSON(`{"host":"` + member + `.lake.example"}`)}},
				DeletionPolicy: oramav1alpha1.SetDeletionRetain,
			}},
		}
	}
	objects := append(managedCluster("orama-nonprod-cluster-01", "westus2", "nonprod"),
		managedCluster("orama-production-cluster-02", "swedencentral", "production")...)
	objects = append(objects, setSecret("lake-prod-stripe", "sk_live"), setSecret("lake-prod-openai", "sk-prod"),
		setSecret("lake-nonprod-stripe", "sk_test"), setSecret("clerk-lake-prod", "clerk-prod"))
	if prodSecrets != nil {
		objects = append(objects, set("lake-prod", "orama-production-cluster-02", "shared", prodSecrets))
	}
	if nonprodSecrets != nil {
		objects = append(objects, set("lake-nonprod", "orama-nonprod-cluster-01", "dev", nonprodSecrets))
	}
	return objects
}

// LSR-041 (#790): lake-nonprod copied lake-prod's Stripe and OpenAI Secrets into dev, testing and
// staging. A Secret labelled production is never copied into a nonprod Lake, even when no
// production set lists it.
func TestALakeSetNeverCopiesASecretLabelledProductionIntoANonprodMember(t *testing.T) {
	objects := lakeSets(nil, []string{"lake-prod-stripe"})
	for _, object := range objects {
		if object.GetName() == "lake-prod-stripe" {
			object.SetLabels(map[string]string{secretPurposeLabel: productionPurpose})
		}
	}
	c := setTestClient(t, objects...)
	members := &fakeMembers{ready: map[string]bool{"dev": true}}
	_, err := reconcileLakeSet(t, c, members, "lake-nonprod")
	want := "Secret lake/lake-prod-stripe is labelled orama.cloud/purpose=production and the member's cluster is nonprod: it is never copied there"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want the production Secret refused, got %v", err)
	}
	if len(members.applied) != 0 {
		t.Fatalf("the nonprod Lake was applied with %v", secretNames(members.applied[0].Secrets))
	}
	if got := getLakeSet(t, c, "lake-nonprod"); got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.MemberStatuses[0].Message, want) {
		t.Fatalf("status: %+v", got.Status)
	}
}

// An ordered LakeSet starts from its source set's template and secrets (orderLakeSet). One built
// from lake-prod onto a nonprod cluster, as a dedicated cluster made like a nonprod template would
// be, gets none of lake-prod's Secrets.
func TestAnOrderedLakeSetOnANonprodClusterGetsNoProductionSecret(t *testing.T) {
	objects := lakeSets([]string{"lake-prod-stripe", "lake-prod-openai"}, nil)
	var prod *oramav1alpha1.LakeSet
	for _, object := range objects {
		if set, ok := object.(*oramav1alpha1.LakeSet); ok && set.Name == "lake-prod" {
			prod = set
		}
	}
	source := &oramav1alpha1.LakeOrderSource{ObjectMeta: metav1.ObjectMeta{Name: "lake-live"},
		Spec: oramav1alpha1.LakeOrderSourceSpec{LakeSetRef: "lake-prod", HostDomain: "lake.live"}}
	const name = "order-0123456789ab"
	ordered, err := orderLakeSet(orderPass{source: source, set: prod, member: map[string]any{}},
		CloudOrder{ID: "0123456789ab"}, name, "orama-nonprod-cluster-01", name+".lake.live", false)
	if err != nil {
		t.Fatal(err)
	}
	c := setTestClient(t, append(objects, source, ordered)...)
	members := &fakeMembers{ready: map[string]bool{name: true}}
	_, err = reconcileLakeSet(t, c, members, name)
	want := "Secret lake/lake-prod-stripe is copied into production members by LakeSet lake-prod, so it is a production credential and is never copied into a nonprod member"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want lake-prod's Secrets refused for the ordered nonprod Lake, got %v", err)
	}
	if len(members.applied) != 0 {
		t.Fatalf("the ordered Lake was applied with %v", secretNames(members.applied[0].Secrets))
	}
}

// A LakeOrderSource's memberSecrets are copied into every Lake ordered from a production set, so
// they are production credentials before the first order exists. A nonprod set naming one is
// refused; one on a nonprod set's order source is not production.
func TestAProductionOrderSourcesMemberSecretIsNeverCopiedIntoANonprodMember(t *testing.T) {
	for name, tc := range map[string]struct {
		lakeSetRef, want string
	}{
		"source on lake-prod":    {"lake-prod", "Secret lake/clerk-lake-prod is copied into production members by LakeOrderSource lake-live, so it is a production credential and is never copied into a nonprod member"},
		"source on lake-nonprod": {"lake-nonprod", ""},
	} {
		t.Run(name, func(t *testing.T) {
			source := &oramav1alpha1.LakeOrderSource{ObjectMeta: metav1.ObjectMeta{Name: "lake-live"},
				Spec: oramav1alpha1.LakeOrderSourceSpec{LakeSetRef: tc.lakeSetRef, HostDomain: "lake.live",
					MemberSecrets: []oramav1alpha1.SetSecret{{Name: "clerk-lake-prod", Namespace: "lake", As: "clerk-secret-key"}}}}
			c := setTestClient(t, append(lakeSets([]string{"lake-prod-stripe"}, []string{"clerk-lake-prod"}), source)...)
			members := &fakeMembers{ready: map[string]bool{"dev": true}}
			_, err := reconcileLakeSet(t, c, members, "lake-nonprod")
			if tc.want == "" {
				if err != nil || len(members.applied) != 1 {
					t.Fatalf("want it copied: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(members.applied) != 0 {
				t.Fatalf("want %q, got %v (applied %d)", tc.want, err, len(members.applied))
			}
		})
	}
}
