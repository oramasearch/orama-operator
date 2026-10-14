/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func selectorMatches(t *testing.T, selector metav1.LabelSelector, podLabels map[string]string) bool {
	t.Helper()
	compiled, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Matches(labels.Set(podLabels))
}

func assertSandboxExcludes(t *testing.T, kube client.Client, namespace string, foreignPods ...map[string]string) {
	t.Helper()
	var policies networkingv1.NetworkPolicyList
	if err := kube.List(context.Background(), &policies, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) < 2 {
		t.Fatal("expected default-deny and allowances")
	}
	for _, policy := range policies.Items {
		for _, podLabels := range foreignPods {
			if selectorMatches(t, policy.Spec.PodSelector, podLabels) {
				t.Errorf("policy %s selects foreign pod %v", policy.Name, podLabels)
			}
		}
	}
}

func TestOramaCloudNetworkSandboxDoesNotSelectOperatorOrUnownedPods(t *testing.T) {
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	stack := newOCV1(nil)
	objects := append([]client.Object{stack}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
	if err := r.reconcileNetworkSandbox(ctx, stack); err != nil {
		t.Fatal(err)
	}
	assertSandboxExcludes(t, kube, "orama-system",
		map[string]string{"app.kubernetes.io/name": "orama-operator"},
		nil,
		map[string]string{"app.kubernetes.io/instance": stack.Name},
		map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator"},
		map[string]string{"app.kubernetes.io/instance": "another-stack", "app.kubernetes.io/managed-by": "orama-cloud-v1-operator"},
	)
	var deny networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: networkSandboxDefaultDenyName}, &deny); err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"writer", "reader", "dashboard", "controldb", "demo-seed"} {
		if !selectorMatches(t, deny.Spec.PodSelector, labelsFor(stack, component)) {
			t.Errorf("default deny misses stack component %s", component)
		}
	}
}

func TestNetworkSandboxSelectorIntersectionNeverWidensScope(t *testing.T) {
	scope := metav1.LabelSelector{
		MatchLabels:      map[string]string{"owner": "ours"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "managed", Operator: metav1.LabelSelectorOpExists}},
	}
	before := scope.DeepCopy()
	for _, component := range []metav1.LabelSelector{
		{},
		{MatchLabels: map[string]string{"owner": "theirs"}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "owner", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"ours"}}}},
	} {
		original := component.DeepCopy()
		result := intersectSandboxSelectors(component, scope)
		for _, pod := range []map[string]string{nil, {"owner": "theirs", "managed": "true"}, {"owner": "ours"}} {
			if selectorMatches(t, result, pod) {
				t.Errorf("intersection %v selects foreign pod %v", result, pod)
			}
		}
		owned := map[string]string{"owner": "ours", "managed": "true"}
		if selectorMatches(t, result, owned) != selectorMatches(t, component, owned) {
			t.Errorf("intersection ignored component restriction %v", component)
		}
		if !reflect.DeepEqual(component, *original) || !reflect.DeepEqual(scope, *before) {
			t.Fatal("intersection mutated its inputs")
		}
		if empty := intersectSandboxSelectors(component, metav1.LabelSelector{}); !reflect.DeepEqual(empty, component) {
			t.Fatalf("workspace selector changed: %v -> %v", component, empty)
		}
	}
}

func TestNetworkSandboxCiliumSelectorIsOwnerScoped(t *testing.T) {
	scope := metav1.LabelSelector{MatchLabels: map[string]string{"owner": "ours"}}
	for _, endpoint := range []map[string]interface{}{
		{},
		{"matchLabels": map[string]interface{}{"k8s:owner": "theirs"}},
	} {
		spec := map[string]interface{}{"endpointSelector": endpoint}
		if err := scopeCiliumSandboxPolicy(spec, scope); err != nil {
			t.Fatal(err)
		}
		var selector metav1.LabelSelector
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec["endpointSelector"].(map[string]interface{}), &selector); err != nil {
			t.Fatal(err)
		}
		// Cilium's source prefix is not part of a Kubernetes label key. Check it explicitly,
		// then evaluate the Kubernetes-label projection with Kubernetes' selector implementation.
		projected := metav1.LabelSelector{MatchLabels: map[string]string{}}
		for key, value := range selector.MatchLabels {
			if !strings.HasPrefix(key, "k8s:") {
				t.Fatalf("scope does not pin the Kubernetes source: %s", key)
			}
			projected.MatchLabels[strings.TrimPrefix(key, "k8s:")] = value
		}
		for _, expression := range selector.MatchExpressions {
			expression.Key = strings.TrimPrefix(expression.Key, "k8s:")
			projected.MatchExpressions = append(projected.MatchExpressions, expression)
		}
		if selectorMatches(t, projected, nil) || selectorMatches(t, projected, map[string]string{"owner": "theirs"}) {
			t.Fatalf("Cilium selector escaped owner scope: %v", selector)
		}
	}
	if err := scopeCiliumSandboxPolicy(map[string]interface{}{"nodeSelector": map[string]interface{}{}}, scope); err == nil {
		t.Fatal("pod sandbox accepted a node selector")
	}
}

func TestCloudNetworkSandboxLabelsReachDatabaseAndSchemaJobs(t *testing.T) {
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	stack := newOCV1(nil)
	stack.Spec.Components.ControlDB.Enabled = true
	stack.Spec.DemoSeed = true
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
	if err := r.reconcileControlDB(ctx, stack); err != nil {
		t.Fatal(err)
	}
	cluster := newCNPGCluster(stack.Namespace, stack.Name+"-controldb")
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	inherited, _, err := unstructured.NestedStringMap(cluster.Object, "spec", "inheritedMetadata", "labels")
	if err != nil || !reflect.DeepEqual(inherited, labelsFor(stack, "controldb")) {
		t.Fatalf("CNPG instance/bootstrap labels = %v, error %v", inherited, err)
	}
	var schemas oramav1alpha1.PostgresSchemaList
	if err := kube.List(ctx, &schemas); err != nil || len(schemas.Items) != 1 {
		t.Fatalf("schema resources = %v, error %v", schemas.Items, err)
	}
	ps := &schemas.Items[0]
	job := pgMigrationJobSpec(ps)
	if job.Template.Labels["app.kubernetes.io/instance"] != stack.Name || job.Template.Labels["app.kubernetes.io/managed-by"] != "orama-cloud-v1-operator" {
		t.Fatalf("schema Job labels = %v", job.Template.Labels)
	}
	if err := r.reconcileDemoSeed(ctx, stack); err != nil {
		t.Fatal(err)
	}
	var seed batchv1.Job
	if err := kube.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: stack.Name + "-demo-seed"}, &seed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seed.Spec.Template.Labels, labelsFor(stack, "demo-seed")) {
		t.Fatalf("seed Job labels = %v", seed.Spec.Template.Labels)
	}
}

func TestLakeNetworkSandboxLabelsCoverOnlyOwnedWorkloads(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := agentsTestLake()
	lake.Namespace = ""
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}).Build()
	applyLakeSchemaForWorkloadTest(t, kube, scheme, lake)
	reconcileLake(t, kube, scheme, lake.Name)
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertSandboxExcludes(t, kube, lakeNamespace(lake), nil,
		map[string]string{"app.kubernetes.io/name": "orama-operator"}, map[string]string{lakeInstanceLabel: "another-lake"})
	var deployments appsv1.DeploymentList
	if err := kube.List(ctx, &deployments); err != nil || len(deployments.Items) != 3 {
		t.Fatalf("Lake and library deployments = %d, error %v", len(deployments.Items), err)
	}
	for _, deployment := range deployments.Items {
		if !selectorMatches(t, sandbox.Spec.PodSelector, deployment.Spec.Template.Labels) {
			t.Errorf("sandbox misses deployment %s", deployment.Name)
		}
		if deployment.Spec.Selector.MatchLabels[lakeInstanceLabel] != "" {
			t.Errorf("immutable deployment selector changed: %s", deployment.Name)
		}
	}
	assertLakeDatabaseAndZettaScope(t, kube, lake, sandbox.Spec.PodSelector)
}

func assertLakeDatabaseAndZettaScope(t *testing.T, kube client.Client, lake *oramav1alpha1.Lake, scope metav1.LabelSelector) {
	t.Helper()
	ctx := context.Background()
	cluster := newCNPGCluster(lakeNamespace(lake), lakeControlDB(lake.Name))
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	inherited, _, err := unstructured.NestedStringMap(cluster.Object, "spec", "inheritedMetadata", "labels")
	if err != nil || !selectorMatches(t, scope, inherited) || selectorMatches(t, lakePodSelector(lake), inherited) {
		t.Fatalf("CNPG scope or application Service selection incorrect: %v, error %v", inherited, err)
	}
	var ps oramav1alpha1.PostgresSchema
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeControlDB(lake.Name) + "-schema"}, &ps); err != nil {
		t.Fatal(err)
	}
	job := pgMigrationJobSpec(&ps)
	if !selectorMatches(t, scope, job.Template.Labels) || selectorMatches(t, lakePodSelector(lake), job.Template.Labels) {
		t.Fatalf("schema scope or application Service selection incorrect: %v", job.Template.Labels)
	}
	var zetta oramav1alpha1.OramaZettaCluster
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeZettaName(lake.Name)}, &zetta); err != nil {
		t.Fatal(err)
	}
	r := &OramaZettaClusterReconciler{Client: kube, Scheme: kube.Scheme()}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: zetta.Namespace, Name: "zetta"}}
	if err := r.mutateZettaStatefulSet(&zetta, sts, "headless", "zetta", zettaLabels(&zetta), "", nil); err != nil {
		t.Fatal(err)
	}
	if !selectorMatches(t, scope, sts.Spec.Template.Labels) || sts.Spec.Selector.MatchLabels[lakeInstanceLabel] != "" {
		t.Fatalf("Zetta scope/selector incorrect: %+v", sts.Spec)
	}
}

func TestPostgresSchemaReplacesUnscopedJobOnce(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	ps := pgSchemaCR()
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PostgresSchemaReconciler{Client: kube, Scheme: scheme}
	if _, err := r.ensureMigrationJob(ctx, ps); err != nil {
		t.Fatal(err)
	}
	ps.Labels = map[string]string{"app.kubernetes.io/instance": "v1", lakeInstanceLabel: "lake"}
	if job, err := r.ensureMigrationJob(ctx, ps); err != nil || job != nil {
		t.Fatalf("unscoped Job was not replaced: %v, %v", job, err)
	}
	job, err := r.ensureMigrationJob(ctx, ps)
	if err != nil || job == nil || !pgMigrationScopeCurrent(ps, job) {
		t.Fatalf("replacement Job missing scope: %v, %v", job, err)
	}
	stable, err := r.ensureMigrationJob(ctx, ps)
	if err != nil || stable == nil || !reflect.DeepEqual(stable.Spec, job.Spec) || stable.ResourceVersion != job.ResourceVersion {
		t.Fatalf("correctly scoped Job was rewritten: %v, %v", stable, err)
	}
}

func TestZettaControlPlaneDoesNotReplaceCurrentJobForSelectionLabels(t *testing.T) {
	ctx := context.Background()
	scheme := zettaTestScheme(t)
	zetta := newZettaTestCluster()
	zetta.Labels = map[string]string{lakeInstanceLabel: "prod"}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zetta).Build()
	r := &OramaZettaClusterReconciler{Client: kube, Scheme: scheme}
	if err := r.reconcileZettaControlPlane(ctx, zetta, "z0-zetta"); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: zetta.Namespace, Name: "z0-zetta-schema"}
	var job batchv1.Job
	if err := kube.Get(ctx, key, &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Labels[lakeInstanceLabel] != "prod" || !metav1.IsControlledBy(&job, zetta) {
		t.Fatal("new schema Job is missing the scope label or legacy owner")
	}
	// Model an existing immutable Job from before scope labels, at the current schema version.
	delete(job.Spec.Template.Labels, lakeInstanceLabel)
	job.CreationTimestamp = metav1.Now()
	job.UID = "existing-schema-job"
	if err := kube.Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileZettaControlPlane(ctx, zetta, "z0-zetta"); err != nil {
		t.Fatal(err)
	}
	var current batchv1.Job
	if err := kube.Get(ctx, key, &current); err != nil {
		t.Fatalf("current schema Job was removed for labels: %v", err)
	}
	if current.UID != job.UID || !reflect.DeepEqual(current.Spec, job.Spec) || !reflect.DeepEqual(current.OwnerReferences, job.OwnerReferences) {
		t.Fatal("adding selection labels changed the existing Job's identity, spec or ownership")
	}
}
