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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testConnectorNamespace = "ws-0123456789ab"
	testConnectorOtherNS   = "ws-ba9876543210"
)

func connectorTestLake() *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")}}
}

// connectorRESTMapper knows every kind in scheme and, when upstream is set, the CloudNativePG and
// TiDB kinds, as a cluster with those operators installed would.
func connectorRESTMapper(scheme *runtime.Scheme, upstream bool) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	if upstream {
		mapper.Add(cnpgClusterGVK, meta.RESTScopeNamespace)
		mapper.Add(tidbClusterGVK, meta.RESTScopeNamespace)
	}
	return mapper
}

// connectorServiceObject is the object the operator writes for one Lake service of kind: its root
// object, labelled and controlled by lake unless owned is false.
func connectorServiceObject(lake *oramav1alpha1.Lake, gvk schema.GroupVersionKind, namespace, name, kind string, owned bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetLabels(map[string]string{serviceLakeLabel: lake.Name, serviceKindLabel: kind, serviceIDLabel: name})
	if owned {
		yes := true
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "Lake", Name: lake.Name, UID: lake.UID, Controller: &yes,
		}})
	}
	return obj
}

func connectorAllow(t *testing.T, kube client.Reader, lake *oramav1alpha1.Lake, namespace string) string {
	t.Helper()
	allow, err := lakeZettaConnectorAllow(context.Background(), kube, lake, namespace)
	if err != nil {
		t.Fatal(err)
	}
	return allow
}

// A workspace Zetta approves exactly the endpoints of its own namespace's postgres, tidb, sqlite,
// duckdb and polars services (operator#882): each the exact host:port lake hands the workspace,
// sorted, and nothing for another namespace's service, an object the Lake does not control, another
// kind, or a service being deleted. No entry is a wildcard or an address range.
func TestWorkspaceZettaApprovesExactlyItsOwnServices(t *testing.T) {
	scheme := newLakeScheme(t)
	lake := connectorTestLake()
	terminating := connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-00000000000e", serviceKindSQLite, true)
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	terminating.SetFinalizers([]string{"test/hold"})
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(connectorRESTMapper(scheme, true)).WithObjects(
		lake,
		connectorServiceObject(lake, cnpgClusterGVK, testConnectorNamespace, "prod-svc-000000000001", serviceKindPostgres, true),
		connectorServiceObject(lake, tidbClusterGVK, testConnectorNamespace, "prod-svc-000000000002", serviceKindTiDB, true),
		connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-000000000003", serviceKindSQLite, true),
		connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-000000000004", serviceKindDuckDB, true),
		connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-000000000005", serviceKindPolars, true),
		// Not this namespace's.
		connectorServiceObject(lake, statefulSetGVK, testConnectorOtherNS, "prod-svc-00000000000a", serviceKindSQLite, true),
		connectorServiceObject(lake, cnpgClusterGVK, testConnectorOtherNS, "prod-svc-00000000000b", serviceKindPostgres, true),
		// Carries the labels, but the Lake does not control it.
		connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-00000000000c", serviceKindSQLite, false),
		// A kind that is no connector source.
		connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-00000000000d", serviceKindKafka, true),
		terminating,
	).Build()

	got := connectorAllow(t, kube, lake, testConnectorNamespace)
	want := strings.Join([]string{
		"prod-svc-000000000001-rw.ws-0123456789ab.svc.cluster.local:5432",
		"prod-svc-000000000002-tidb.ws-0123456789ab.svc.cluster.local:4000",
		"prod-svc-000000000003.ws-0123456789ab.svc.cluster.local:8080",
		"prod-svc-000000000004.ws-0123456789ab.svc.cluster.local:8080",
		"prod-svc-000000000005.ws-0123456789ab.svc.cluster.local:8080",
	}, ",")
	if got != want {
		t.Fatalf("connector destinations = %q\nwant %q", got, want)
	}
	for _, entry := range strings.Split(got, ",") {
		if strings.ContainsAny(entry, "*/[") || !strings.HasSuffix(strings.SplitN(entry, ":", 2)[0], "."+testConnectorNamespace+".svc.cluster.local") {
			t.Errorf("entry %q is broader than one service of the namespace", entry)
		}
	}
	other := connectorAllow(t, kube, lake, testConnectorOtherNS)
	if other != "prod-svc-00000000000a.ws-ba9876543210.svc.cluster.local:8080,prod-svc-00000000000b-rw.ws-ba9876543210.svc.cluster.local:5432" {
		t.Fatalf("the other workspace's destinations = %q", other)
	}

	// Each entry is the authority of the endpoint the service's credential carries.
	in := catalogService{name: "prod-svc-000000000003", namespace: testConnectorNamespace}
	if endpoint := sqliteServiceKind.endpoint(in); endpoint != "http://prod-svc-000000000003.ws-0123456789ab.svc.cluster.local:8080" {
		t.Fatalf("the sqlite endpoint moved: %q", endpoint)
	}
	if endpoint := servicePostgresEndpoint("prod", "svc-000000000001", testConnectorNamespace); !strings.Contains(want, endpoint) {
		t.Fatalf("the postgres endpoint %q is not approved", endpoint)
	}
}

// A namespace with no connector source gets an empty value, which approves no private destination;
// so does a cluster where neither CloudNativePG nor TiDB Operator is installed, for those kinds.
func TestAWorkspaceWithNoSourcesApprovesNothing(t *testing.T) {
	scheme := newLakeScheme(t)
	lake := connectorTestLake()
	empty := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(connectorRESTMapper(scheme, true)).WithObjects(lake).Build()
	if got := connectorAllow(t, empty, lake, testConnectorNamespace); got != "" {
		t.Fatalf("an empty namespace approved %q", got)
	}
	bare := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(connectorRESTMapper(scheme, false)).WithObjects(
		lake, connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-000000000003", serviceKindSQLite, true),
	).Build()
	if got := connectorAllow(t, bare, lake, testConnectorNamespace); got != "prod-svc-000000000003.ws-0123456789ab.svc.cluster.local:8080" {
		t.Fatalf("without the upstream operators = %q", got)
	}
}

// The workspace Zetta's OramaZettaCluster carries the value, and it follows the services: a service
// added appears on the next pass and one removed disappears. A value stated in
// spec.services.zettaEnv is dropped, so nothing outside the namespace's resources can widen it.
func TestWorkspaceZettaConnectorDestinationsFollowItsServices(t *testing.T) {
	scheme := newLakeScheme(t)
	lake := connectorTestLake()
	lake.Spec.Services.ZettaEnv = []corev1.EnvVar{{Name: zettaConnectorAllowEnv, Value: "10.0.0.0/8:5432"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(connectorRESTMapper(scheme, true)).WithObjects(lake).Build()
	reconciler := &LakeWorkspacesReconciler{Client: kube, Scheme: scheme}
	row := &LakeWorkspaceRow{ID: "ws-0123456789ab", CustomerID: "customer", Namespace: testConnectorNamespace, StorageGi: 10}
	ctx := context.Background()

	value := func() string {
		t.Helper()
		cluster, err := reconciler.reconcileWorkspaceZetta(ctx, lake, row, testConnectorNamespace, "zetta")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		got := ""
		for _, variable := range cluster.Spec.Env {
			if variable.Name == zettaConnectorAllowEnv {
				count++
				got = variable.Value
			}
		}
		if count != 1 {
			t.Fatalf("%s appears %d times in %v", zettaConnectorAllowEnv, count, cluster.Spec.Env)
		}
		return got
	}

	if got := value(); got != "" {
		t.Fatalf("no services, yet %q (the stated value must not survive)", got)
	}
	sqlite := connectorServiceObject(lake, statefulSetGVK, testConnectorNamespace, "prod-svc-000000000003", serviceKindSQLite, true)
	postgres := connectorServiceObject(lake, cnpgClusterGVK, testConnectorNamespace, "prod-svc-000000000001", serviceKindPostgres, true)
	for _, obj := range []client.Object{sqlite, postgres} {
		if err := kube.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	if got := value(); got != "prod-svc-000000000001-rw.ws-0123456789ab.svc.cluster.local:5432,prod-svc-000000000003.ws-0123456789ab.svc.cluster.local:8080" {
		t.Fatalf("after adding two services = %q", got)
	}
	if err := kube.Delete(ctx, sqlite); err != nil {
		t.Fatal(err)
	}
	if got := value(); got != "prod-svc-000000000001-rw.ws-0123456789ab.svc.cluster.local:5432" {
		t.Fatalf("after removing the sqlite service = %q", got)
	}
}

// A namespace that cannot be read keeps the value the cluster runs with, so a failed read never
// rolls the StatefulSet; with no value yet it approves nothing.
func TestConnectorDestinationsKeepTheCurrentValueWhenUnreadable(t *testing.T) {
	scheme := newLakeScheme(t)
	lake := connectorTestLake()
	broken := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(connectorRESTMapper(scheme, true)).WithObjects(lake).
		WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("api server unavailable")
		}}).Build()
	current := []corev1.EnvVar{{Name: zettaConnectorAllowEnv, Value: "kept.ws.svc.cluster.local:8080"}}
	got, err := workspaceZettaConnectorAllow(context.Background(), broken, lake, testConnectorNamespace, current)
	if err == nil || got != "kept.ws.svc.cluster.local:8080" {
		t.Fatalf("unreadable namespace gave %q, %v; want the current value and an error", got, err)
	}
	got, err = workspaceZettaConnectorAllow(context.Background(), broken, lake, testConnectorNamespace, nil)
	if err == nil || got != "" {
		t.Fatalf("unreadable namespace with no current value gave %q, %v", got, err)
	}
}
