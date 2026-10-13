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

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeMembers stands in for the managed clusters. An instance becomes Ready when the test says
// so; until then it is as a cluster's operator leaves a new one.
type fakeMembers struct {
	applied   []memberRequest
	deleted   []string
	ready     map[string]bool // by instance name
	missing   bool            // the instance's CRD is not on the cluster
	applyErr  error
	kubeconfs []string
}

func (f *fakeMembers) ApplyMember(_ context.Context, kubeconfig []byte, req memberRequest) (memberState, error) {
	f.kubeconfs = append(f.kubeconfs, string(kubeconfig))
	if f.missing {
		return memberState{MissingCRD: true}, nil
	}
	if f.applyErr != nil {
		return memberState{}, f.applyErr
	}
	f.applied = append(f.applied, req)
	obj := memberObject(req)
	obj.SetGeneration(3)
	obj.Object["spec"] = req.Spec
	if f.ready[req.Name] {
		obj.Object["status"] = map[string]any{"phase": "Ready", "observedGeneration": int64(3),
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(3)}}}
	} else {
		obj.Object["status"] = map[string]any{"phase": "Progressing",
			"conditions": []any{map[string]any{"type": "Ready", "status": "False", "observedGeneration": int64(3), "message": "the control database is starting"}}}
	}
	return memberState{Object: obj}, nil
}

func (f *fakeMembers) DeleteMember(_ context.Context, _ []byte, req memberRequest) error {
	f.deleted = append(f.deleted, req.Name)
	return nil
}

func managedCluster(name, region, purpose string) []client.Object {
	tags := map[string]string{}
	if purpose != "" {
		tags[clusterPurposeTag] = purpose
	}
	return []client.Object{
		&oramav1alpha1.KubernetesCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       oramav1alpha1.KubernetesClusterSpec{Region: region, Tags: tags},
			Status: oramav1alpha1.KubernetesClusterStatus{Phase: clusterPhaseReady,
				KubeconfigSecretRef: &oramav1alpha1.ClusterSecretReference{Name: name + "-kubeconfig", Namespace: "orama-system"}},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-kubeconfig", Namespace: "orama-system"},
			Data: map[string][]byte{kubeconfigSecretKey: []byte("kubeconfig-of-" + name)}},
	}
}

func setSecret(name string, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lake"}, Data: map[string][]byte{"key": []byte(value)}}
}

func setTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.LakeSet{}, &oramav1alpha1.OramaCloudV1Set{}).Build()
}

func rawJSON(text string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(text)} }

func nonprodLakeSet() *oramav1alpha1.LakeSet {
	return &oramav1alpha1.LakeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-nonprod"},
		Spec: oramav1alpha1.ManagedSetSpec{
			Template: *rawJSON(`{"image":"oramasearch/lake:v0.1.0","postgres":{"instances":1,"storageSize":"50Gi"},"env":[{"name":"A","value":"1"},{"name":"B","value":"2"}]}`),
			Secrets:  []oramav1alpha1.SetSecret{{Name: "oramasearch-dockerhub", Namespace: "lake"}, {Name: "lake-prod-openai", Namespace: "lake"}},
			Clusters: []oramav1alpha1.SetMember{
				{ClusterRef: "orama-nonprod-cluster-01", Name: "dev", Spec: rawJSON(`{"host":"dev.lake.example"}`)},
				{ClusterRef: "orama-nonprod-cluster-01", Name: "staging",
					Spec:    rawJSON(`{"host":"staging.lake.example","image":"oramasearch/lake:next","postgres":{"instances":3},"env":[{"name":"C","value":"3"}]}`),
					Secrets: []oramav1alpha1.SetSecret{{Name: "staging-openai", Namespace: "lake", As: "lake-prod-openai"}}},
			},
			DeletionPolicy: oramav1alpha1.SetDeletionRetain,
		},
	}
}

func reconcileLakeSet(t *testing.T, c client.Client, members *fakeMembers, name string) (ctrl.Result, error) {
	t.Helper()
	r := &LakeSetReconciler{Client: c, Scheme: clusterTestScheme(t), Applier: members}
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
}

func getLakeSet(t *testing.T, c client.Client, name string) *oramav1alpha1.LakeSet {
	t.Helper()
	var set oramav1alpha1.LakeSet
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &set); err != nil {
		t.Fatal(err)
	}
	return &set
}

func nonprodObjects() []client.Object {
	return append(managedCluster("orama-nonprod-cluster-01", "westus2", "nonprod"),
		setSecret("oramasearch-dockerhub", "hub"), setSecret("lake-prod-openai", "shared-key"), setSecret("staging-openai", "staging-key"))
}

// Members roll out in order, each in its own namespace, with its own settings over the template
// and its own Secrets over the set's.
func TestLakeSetAppliesMembersInOrderWithTheirOwnSettings(t *testing.T) {
	members := &fakeMembers{ready: map[string]bool{}}
	c := setTestClient(t, append(nonprodObjects(), nonprodLakeSet())...)

	result, err := reconcileLakeSet(t, c, members, "lake-nonprod")
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("got %v, %v", result, err)
	}
	if len(members.applied) != 1 || members.applied[0].Name != "dev" {
		t.Fatalf("only the first member is applied until it is Ready; applied %d", len(members.applied))
	}
	dev := members.applied[0]
	if dev.Namespace != "lake-dev" || dev.Spec["namespace"] != "lake-dev" || dev.ObjectNamespace != "" || dev.Owner != "LakeSet.lake-nonprod" {
		t.Fatalf("dev: namespace %q, spec.namespace %v, object namespace %q, owner %q", dev.Namespace, dev.Spec["namespace"], dev.ObjectNamespace, dev.Owner)
	}
	if dev.Spec["host"] != "dev.lake.example" || dev.Spec["image"] != "oramasearch/lake:v0.1.0" || members.kubeconfs[0] != "kubeconfig-of-orama-nonprod-cluster-01" {
		t.Fatalf("dev spec: %+v", dev.Spec)
	}
	if len(dev.Secrets) != 2 || string(dev.Secrets[1].Data["key"]) != "shared-key" {
		t.Fatalf("dev gets the set's secrets: %+v", dev.Secrets)
	}
	set := getLakeSet(t, c, "lake-nonprod")
	if set.Status.Phase != setPhaseProgressing || set.Status.Purpose != "nonprod" || set.Status.Members != 2 || set.Status.ReadyMembers != 0 {
		t.Fatalf("status: %+v", set.Status)
	}
	if got := set.Status.MemberStatuses; got[0].Phase != setPhaseProgressing || got[0].Message != "the control database is starting" ||
		got[1].Phase != clusterPhasePending || got[1].Message != "waiting for dev on orama-nonprod-cluster-01 to be Ready" {
		t.Fatalf("member statuses: %+v", got)
	}
	if !strings.Contains(set.Status.Message, "the control database is starting") {
		t.Fatalf("the set's message carries what the member's cluster says: %q", set.Status.Message)
	}

	members.ready["dev"] = true
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	staging := members.applied[len(members.applied)-1]
	if staging.Name != "staging" || staging.Namespace != "lake-staging" {
		t.Fatalf("staging follows once dev is Ready: %+v", staging)
	}
	postgres := staging.Spec["postgres"].(map[string]any)
	if staging.Spec["image"] != "oramasearch/lake:next" || postgres["instances"] != float64(3) || postgres["storageSize"] != "50Gi" {
		t.Fatalf("a member's field wins and an object is merged field by field: %+v", staging.Spec)
	}
	if env := staging.Spec["env"].([]any); len(env) != 1 || env[0].(map[string]any)["name"] != "C" {
		t.Fatalf("a member's list replaces the template's list: %+v", env)
	}
	if len(staging.Secrets) != 2 || staging.Secrets[1].Name != "lake-prod-openai" || string(staging.Secrets[1].Data["key"]) != "staging-key" {
		t.Fatalf("a member's secret replaces the set's of the same name: %+v", staging.Secrets)
	}

	members.ready["staging"] = true
	if result, err = reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil || result.RequeueAfter != leafOperatorSteadyRecheck {
		t.Fatalf("got %v, %v", result, err)
	}
	if set = getLakeSet(t, c, "lake-nonprod"); set.Status.Phase != clusterPhaseReady || set.Status.ReadyMembers != 2 || set.Status.MemberStatuses[1].Version != "oramasearch/lake:next" {
		t.Fatalf("status when all are ready: %+v", set.Status)
	}
}

// A set never combines nonprod and production, and never guesses what a cluster is for. A set
// that does is refused whole: nothing reaches any cluster.
func TestASetNeverCombinesPurposes(t *testing.T) {
	for name, tc := range map[string]struct {
		second, want string
	}{
		"mixed":      {"production", "names nonprod (orama-nonprod-cluster-01) and production (orama-production-cluster-02)"},
		"no purpose": {"", `KubernetesCluster orama-production-cluster-02 has no "purpose" tag`},
	} {
		t.Run(name, func(t *testing.T) {
			set := nonprodLakeSet()
			set.Spec.Clusters[1].ClusterRef = "orama-production-cluster-02"
			objects := append(nonprodObjects(), managedCluster("orama-production-cluster-02", "swedencentral", tc.second)...)
			members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}}
			c := setTestClient(t, append(objects, set)...)
			if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
				t.Fatalf("a set that must be edited is reported, not retried: %v", err)
			}
			got := getLakeSet(t, c, "lake-nonprod")
			if got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, tc.want) {
				t.Fatalf("phase %q message %q", got.Status.Phase, got.Status.Message)
			}
			if len(members.applied) != 0 {
				t.Fatal("something was applied to a cluster from a refused set")
			}
		})
	}
}

// Each instance lands in its own namespace.
func TestTwoMembersOnOneClusterCannotShareANamespace(t *testing.T) {
	set := nonprodLakeSet()
	set.Spec.Clusters[1].Namespace = "lake-dev"
	members := &fakeMembers{ready: map[string]bool{}}
	c := setTestClient(t, append(nonprodObjects(), set)...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatal(err)
	}
	got := getLakeSet(t, c, "lake-nonprod")
	if got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, "members dev and staging would share namespace lake-dev") || len(members.applied) != 0 {
		t.Fatalf("phase %q message %q applied %d", got.Status.Phase, got.Status.Message, len(members.applied))
	}
}

// A member is not applied to a cluster whose operator is not installed, and a Secret the set
// names that does not exist is an error naming it.
func TestASetWaitsForPrerequisitesAndNamesAMissingSecret(t *testing.T) {
	members := &fakeMembers{missing: true}
	c := setTestClient(t, append(nonprodObjects(), nonprodLakeSet())...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
		t.Fatalf("waiting for a prerequisite is not an error: %v", err)
	}
	got := getLakeSet(t, c, "lake-nonprod").Status.MemberStatuses[0]
	if got.Phase != clusterPhasePending || !strings.Contains(got.Message, "does not have the Lake kind (lakes.orama.cloud): its Orama operator is not installed yet") {
		t.Fatalf("member: %+v", got)
	}

	set := nonprodLakeSet()
	set.Spec.Secrets = append(set.Spec.Secrets, oramav1alpha1.SetSecret{Name: "lake-prod-stripe", Namespace: "lake"})
	members = &fakeMembers{ready: map[string]bool{}}
	c = setTestClient(t, append(nonprodObjects(), set)...)
	_, err := reconcileLakeSet(t, c, members, "lake-nonprod")
	if err == nil || !strings.Contains(err.Error(), "Secret lake/lake-prod-stripe does not exist on this cluster") || len(members.applied) != 0 {
		t.Fatalf("want the missing Secret named and nothing applied, got %v, applied %d", err, len(members.applied))
	}
	if got := getLakeSet(t, c, "lake-nonprod"); got.Status.Phase != clusterPhaseDegraded || got.Status.MemberStatuses[0].Phase != clusterPhaseDegraded {
		t.Fatalf("status: %+v", got.Status)
	}
}

// What a member's cluster refuses is recorded on the member and returned as an error.
func TestASetReportsWhatAMembersClusterRefuses(t *testing.T) {
	refused := errors.New(`the cluster refused Lake dev: Lake.orama.cloud "dev" is invalid: spec.replicas: Invalid value: -1`)
	members := &fakeMembers{applyErr: refused}
	c := setTestClient(t, append(nonprodObjects(), nonprodLakeSet())...)
	if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); !errors.Is(err, refused) {
		t.Fatalf("want the cluster's refusal returned, got %v", err)
	}
	got := getLakeSet(t, c, "lake-nonprod")
	if got.Status.MemberStatuses[0].Message != refused.Error() || got.Status.MemberStatuses[1].Phase != clusterPhasePending {
		t.Fatalf("statuses: %+v", got.Status.MemberStatuses)
	}
}

// Removing a member, or deleting the set, leaves its instance running unless the policy is Delete.
func TestRemovingAMemberOrTheSetFollowsTheDeletionPolicy(t *testing.T) {
	for _, policy := range []oramav1alpha1.SetDeletionPolicy{oramav1alpha1.SetDeletionRetain, oramav1alpha1.SetDeletionDelete} {
		set := nonprodLakeSet()
		set.Spec.DeletionPolicy = policy
		members := &fakeMembers{ready: map[string]bool{"dev": true, "staging": true}}
		c := setTestClient(t, append(nonprodObjects(), set)...)
		if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
			t.Fatal(err)
		}
		current := getLakeSet(t, c, "lake-nonprod")
		current.Spec.Clusters = current.Spec.Clusters[:1] // staging is removed
		if err := c.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
			t.Fatal(err)
		}
		wantDeleted := policy == oramav1alpha1.SetDeletionDelete
		if deleted := len(members.deleted) == 1 && members.deleted[0] == "staging"; deleted != wantDeleted {
			t.Fatalf("%s: removing a member deleted %v", policy, members.deleted)
		}
		if got := getLakeSet(t, c, "lake-nonprod"); len(got.Status.MemberStatuses) != 1 {
			t.Fatalf("%s: the removed member is still reported: %+v", policy, got.Status.MemberStatuses)
		}
		if err := c.Delete(context.Background(), getLakeSet(t, c, "lake-nonprod")); err != nil {
			t.Fatal(err)
		}
		if _, err := reconcileLakeSet(t, c, members, "lake-nonprod"); err != nil {
			t.Fatal(err)
		}
		var gone oramav1alpha1.LakeSet
		if err := c.Get(context.Background(), types.NamespacedName{Name: "lake-nonprod"}, &gone); !apierrors.IsNotFound(err) {
			t.Fatalf("%s: the set was not released: %v", policy, err)
		}
		if deletedDev := len(members.deleted) == 2; deletedDev != wantDeleted {
			t.Fatalf("%s: deleting the set deleted %v", policy, members.deleted)
		}
	}
}

// An OramaCloudV1's region is its cluster's: never stated, and refused if stated differently.
func TestAnOramaCloudV1SetTakesTheRegionFromTheCluster(t *testing.T) {
	set := &oramav1alpha1.OramaCloudV1Set{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-prod"},
		Spec: oramav1alpha1.ManagedSetSpec{
			Template: *rawJSON(`{"profile":"light","region":{"role":"read"},"images":{"tag":"v1-sandbox"}}`),
			Clusters: []oramav1alpha1.SetMember{
				{ClusterRef: "orama-production-cluster-02", Name: "shared", Spec: rawJSON(`{"region":{"role":"primary"}}`)},
				{ClusterRef: "orama-production-cluster-02", Name: "customer-1", Spec: rawJSON(`{"region":{"name":"us-east"}}`)},
			},
		},
	}
	members := &fakeMembers{ready: map[string]bool{"shared": true}}
	c := setTestClient(t, append(managedCluster("orama-production-cluster-02", "swedencentral", "production"), set)...)
	r := &OramaCloudV1SetReconciler{Client: c, Scheme: clusterTestScheme(t), Applier: members}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1-prod"}})

	shared := members.applied[0]
	region := shared.Spec["region"].(map[string]any)
	if region["name"] != "swedencentral" || region["role"] != "primary" || shared.Namespace != "v1-shared" || shared.ObjectNamespace != "v1-shared" {
		t.Fatalf("shared: region %+v, namespace %q, object namespace %q", region, shared.Namespace, shared.ObjectNamespace)
	}
	if err == nil || !strings.Contains(err.Error(), `spec.region.name is "us-east", but the instance runs on orama-production-cluster-02, which is in swedencentral`) {
		t.Fatalf("want a stated region that is not the cluster's refused, got %v", err)
	}
	if len(members.applied) != 1 {
		t.Fatal("the member with the wrong region was applied")
	}
}

// A set does not take over an instance it did not create.
func TestASetDoesNotTakeOverAnInstanceItDidNotCreate(t *testing.T) {
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	crd.SetName("lakes.orama.cloud")
	existing := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", Labels: map[string]string{managedBySetLabel: "LakeSet.someone-else"}}}
	leaf := fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(existing).WithRuntimeObjects(crd).Build()
	req := memberRequest{Kind: lakeSetKind.instance, CRD: lakeSetKind.crd, Name: "prod", Namespace: "lake-prod", Owner: "LakeSet.lake-prod", Spec: map[string]any{"image": "x"}}

	if _, err := applyMemberWith(context.Background(), leaf, req); !errors.Is(err, errNotManagedBySet) {
		t.Fatalf("want the existing Lake left alone, got %v", err)
	}
	if err := deleteMemberWith(context.Background(), leaf, req); err != nil {
		t.Fatal(err)
	}
	if err := leaf.Get(context.Background(), types.NamespacedName{Name: "prod"}, &oramav1alpha1.Lake{}); err != nil {
		t.Fatalf("a Lake this set did not create was deleted: %v", err)
	}
	req.CRD = "nosuchkinds.orama.cloud"
	if state, err := applyMemberWith(context.Background(), leaf, req); err != nil || !state.MissingCRD {
		t.Fatalf("want a missing CRD reported and nothing applied, got %+v, %v", state, err)
	}
}

func TestMergeSpecReplacesListsAndMergesObjects(t *testing.T) {
	template := map[string]any{"a": 1, "o": map[string]any{"x": 1, "y": 2}, "l": []any{1, 2}}
	merged := mergeSpec(template, map[string]any{"o": map[string]any{"y": 3}, "l": []any{9}, "n": "new"})
	o := merged["o"].(map[string]any)
	if merged["a"] != 1 || o["x"] != 1 || o["y"] != 3 || len(merged["l"].([]any)) != 1 || merged["n"] != "new" {
		t.Fatalf("merged: %+v", merged)
	}
	if template["o"].(map[string]any)["y"] != 2 {
		t.Fatal("the template was written")
	}
}
