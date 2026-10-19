/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeOrderDB is one member's control Postgres: the cloud_orders rows and their events. Claim
// and the moves out of provisioning have the pgx statements' semantics, under one lock, so two
// reconcilers sharing it race as two pollers on one database do.
type fakeOrderDB struct {
	mu      sync.Mutex
	orders  map[string]*CloudOrder
	reasons map[string]string
	events  []string // "<id> <from>-><to>"
	missing bool     // no cloud_orders table yet
	opens   int
}

func newFakeOrderDB(orders ...CloudOrder) *fakeOrderDB {
	db := &fakeOrderDB{orders: map[string]*CloudOrder{}, reasons: map[string]string{}}
	for i := range orders {
		o := orders[i]
		db.orders[o.ID] = &o
	}
	return db
}

func (db *fakeOrderDB) order(id string) CloudOrder {
	db.mu.Lock()
	defer db.mu.Unlock()
	return *db.orders[id]
}

func (db *fakeOrderDB) eventsOf(id string) []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []string
	for _, e := range db.events {
		if strings.HasPrefix(e, id+" ") {
			out = append(out, strings.TrimPrefix(e, id+" "))
		}
	}
	return out
}

type fakeOrderStore struct{ db *fakeOrderDB }

func (s fakeOrderStore) ListOrders(_ context.Context, claimant string) ([]CloudOrder, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if s.db.missing {
		return nil, errCloudOrdersTableMissing
	}
	var out []CloudOrder
	for _, o := range s.db.orders {
		if o.State == cloudOrderPaid || (o.ClaimedBy == claimant && o.State != cloudOrderPaid) {
			out = append(out, *o)
		}
	}
	return out, nil
}

func (s fakeOrderStore) Claim(_ context.Context, id, claimant string) (bool, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	o := s.db.orders[id]
	if o == nil || o.State != cloudOrderPaid {
		return false, nil
	}
	o.State, o.ClaimedBy = cloudOrderProvisioning, claimant
	s.db.events = append(s.db.events, id+" paid->provisioning")
	return true, nil
}

func (s fakeOrderStore) finish(id, claimant, to, host, reason string) (bool, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	o := s.db.orders[id]
	if o == nil || o.State != cloudOrderProvisioning || o.ClaimedBy != claimant {
		return false, nil
	}
	o.State = to
	if host != "" {
		o.Host = host
	}
	if reason != "" {
		s.db.reasons[id] = reason
	}
	s.db.events = append(s.db.events, id+" provisioning->"+to)
	return true, nil
}

func (s fakeOrderStore) MarkReady(_ context.Context, id, claimant, host string) (bool, error) {
	return s.finish(id, claimant, cloudOrderReady, host, "")
}

func (s fakeOrderStore) MarkFailed(_ context.Context, id, claimant, reason string) (bool, error) {
	return s.finish(id, claimant, cloudOrderFailed, "", reason)
}

func (s fakeOrderStore) Close(context.Context) error { return nil }

// fakeOrderDBs opens each member's database by the member's Lake name.
func fakeOrderDBs(dbs map[string]*fakeOrderDB) CloudOrderStoreOpener {
	return func(_ context.Context, _ client.Client, member orderSourceMember) (CloudOrderStore, error) {
		db := dbs[member.Name]
		db.mu.Lock()
		db.opens++
		db.mu.Unlock()
		return fakeOrderStore{db: db}, nil
	}
}

const (
	warehouseOrder = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	dedicatedOrder = "1b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e"
)

// orderWorld is the management cluster as lake.live has it: two production clusters running the
// lake-prod set, the Azure provider, and prod-03's platform and operator as the template.
func orderWorld(withProvider bool) []client.Object {
	cluster := func(name, region string) *oramav1alpha1.KubernetesCluster {
		count := int32(1)
		return &oramav1alpha1.KubernetesCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: oramav1alpha1.KubernetesClusterSpec{ProviderRef: "azure-orama", Region: region, KubernetesVersion: "1.35",
				NodePools:      []oramav1alpha1.ClusterNodePool{{Name: "agentpool", Role: oramav1alpha1.ClusterNodePoolSystem, MachineSize: "Standard_D8as_v7", Count: &count}},
				DeletionPolicy: oramav1alpha1.ClusterDeletionRetain,
				Tags:           map[string]string{clusterPurposeTag: "production", "role": "leaf"}},
		}
	}
	objects := []client.Object{
		cluster("orama-production-cluster-02", "swedencentral"),
		cluster("orama-production-cluster-03", "eastus"),
		&oramav1alpha1.ClusterHelmRelease{ObjectMeta: metav1.ObjectMeta{Name: "prod3-cert-manager"},
			Spec: oramav1alpha1.ClusterHelmReleaseSpec{ClusterRef: "orama-production-cluster-03", ReleaseName: "cert-manager", Namespace: "cert-manager",
				Chart: oramav1alpha1.ClusterHelmChart{Repository: "https://charts.jetstack.io", Name: "cert-manager", Version: "v1.21.2"}}},
		&oramav1alpha1.ClusterHelmRelease{ObjectMeta: metav1.ObjectMeta{Name: "prod3-strimzi"},
			Spec: oramav1alpha1.ClusterHelmReleaseSpec{ClusterRef: "orama-production-cluster-03", ReleaseName: "strimzi", Namespace: "strimzi",
				Chart:     oramav1alpha1.ClusterHelmChart{Repository: "https://strimzi.io/charts/", Name: "strimzi-kafka-operator", Version: "1.2.0"},
				DependsOn: []string{"prod3-cert-manager"}}},
		&oramav1alpha1.ClusterHelmRelease{ObjectMeta: metav1.ObjectMeta{Name: "prod2-cert-manager"},
			Spec: oramav1alpha1.ClusterHelmReleaseSpec{ClusterRef: "orama-production-cluster-02", ReleaseName: "cert-manager", Namespace: "cert-manager",
				Chart: oramav1alpha1.ClusterHelmChart{Repository: "https://charts.jetstack.io", Name: "cert-manager", Version: "v1.21.2"}}},
		&oramav1alpha1.OramaOperator{ObjectMeta: metav1.ObjectMeta{Name: "orama-production-cluster-03"},
			Spec: oramav1alpha1.OramaOperatorSpec{ClusterRef: "orama-production-cluster-03", Version: "v0.6.1", Namespace: "orama-system",
				ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "oramasearch-dockerhub", Namespace: "orama-system"}}},
		&oramav1alpha1.LakeSet{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod"},
			Spec: oramav1alpha1.ManagedSetSpec{
				Template: *rawJSON(`{"image":"oramasearch/lake:v0.2.0","ingressEnabled":true,"dnsSecretRef":{"name":"cloudflare-api-token-lake-live"}}`),
				Secrets:  []oramav1alpha1.SetSecret{{Name: "oramasearch-dockerhub", Namespace: "lake"}},
				Clusters: []oramav1alpha1.SetMember{
					{ClusterRef: "orama-production-cluster-02", Name: "shared", Spec: rawJSON(`{"host":"eu.lake.live"}`)},
					{ClusterRef: "orama-production-cluster-03", Name: "shared-us", Spec: rawJSON(`{"host":"us.lake.live"}`)},
				},
				DeletionPolicy: oramav1alpha1.SetDeletionRetain,
			}},
		&oramav1alpha1.LakeOrderSource{ObjectMeta: metav1.ObjectMeta{Name: "lake-live"},
			Spec: oramav1alpha1.LakeOrderSourceSpec{LakeSetRef: "lake-prod", HostDomain: "lake.live",
				Member:           rawJSON(`{"clerk":{"secretKeySecretRef":{"name":"clerk-secret-key"}}}`),
				MemberSecrets:    []oramav1alpha1.SetSecret{{Name: "clerk-lake-prod", Namespace: "lake", As: "clerk-secret-key"}},
				DedicatedCluster: &oramav1alpha1.LakeOrderDedicatedCluster{ProviderRef: "azure-orama", TemplateClusterRef: "orama-production-cluster-03"}}},
	}
	if withProvider {
		objects = append(objects, &oramav1alpha1.KubernetesClusterProvider{ObjectMeta: metav1.ObjectMeta{Name: "azure-orama"},
			Spec: oramav1alpha1.KubernetesClusterProviderSpec{Type: oramav1alpha1.KubernetesClusterProviderAKS,
				CredentialsSecretRef: oramav1alpha1.ClusterSecretReference{Name: "azure-cluster-management", Namespace: "orama-system"},
				AKS:                  &oramav1alpha1.AksProviderSettings{SubscriptionID: "aff019e7-3b91-47a8-8cd7-4ceef2ddb47e"}}})
	}
	return objects
}

// orderHarness runs the scenarios against a fake client or a real API server alike.
type orderHarness struct {
	t   *testing.T
	c   client.Client
	dbs map[string]*fakeOrderDB
}

func (h *orderHarness) reconciler() *LakeOrderSourceReconciler {
	return &LakeOrderSourceReconciler{Client: h.c, Scheme: h.c.Scheme(), Orders: fakeOrderDBs(h.dbs)}
}

func (h *orderHarness) reconcile() {
	h.t.Helper()
	if _, err := h.reconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "lake-live"}}); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

func (h *orderHarness) source() *oramav1alpha1.LakeOrderSource {
	h.t.Helper()
	var source oramav1alpha1.LakeOrderSource
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "lake-live"}, &source); err != nil {
		h.t.Fatal(err)
	}
	return &source
}

func (h *orderHarness) get(name string, obj client.Object) bool {
	h.t.Helper()
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: name}, obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false
		}
		h.t.Fatal(err)
	}
	return true
}

// providerReady marks the Azure provider's credentials as checked, as its controller does.
func (h *orderHarness) providerReady() {
	h.t.Helper()
	var provider oramav1alpha1.KubernetesClusterProvider
	h.get("azure-orama", &provider)
	provider.Status = oramav1alpha1.KubernetesClusterProviderStatus{Ready: true, AuthMethod: "workload-identity", ObservedGeneration: provider.Generation}
	if err := h.c.Status().Update(context.Background(), &provider); err != nil {
		h.t.Fatal(err)
	}
}

// setReady marks an order's LakeSet Ready, as the LakeSet controller does once its Lake is.
func (h *orderHarness) setReady(name string) {
	h.t.Helper()
	var set oramav1alpha1.LakeSet
	if !h.get(name, &set) {
		h.t.Fatalf("LakeSet %s was not made", name)
	}
	set.Status.Phase, set.Status.ObservedGeneration = clusterPhaseReady, set.Generation
	if err := h.c.Status().Update(context.Background(), &set); err != nil {
		h.t.Fatal(err)
	}
}

func orderFakeClient(t *testing.T, objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.LakeOrderSource{}, &oramav1alpha1.LakeSet{}, &oramav1alpha1.KubernetesCluster{},
			&oramav1alpha1.KubernetesClusterProvider{}, &oramav1alpha1.ClusterHelmRelease{}, &oramav1alpha1.OramaOperator{}).Build()
}

// The scenarios, run against the fake client always and against envtest's API server when its
// binaries are there (so every object made is also checked against its CRD's schema).
var orderScenarios = []struct {
	name string
	run  func(h *orderHarness)
}{
	{"a private warehouse goes paid, provisioning, ready with its host", scenarioPrivateWarehouse},
	{"a dedicated cluster goes paid, provisioning, ready with its host", scenarioDedicatedCluster},
	{"an order that cannot be provisioned as written fails with the reason", scenarioFailures},
	{"a cluster without the cloud identity fails a dedicated cluster order", scenarioNoCloudIdentity},
	{"each order is provisioned exactly once across concurrent pollers and restarts", scenarioExactlyOnce},
	{"nothing is claimed before lake's table exists, and a bad source claims nothing", scenarioNothingClaimed},
}

func TestLakeOrders(t *testing.T) {
	for _, scenario := range orderScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			scenario.run(&orderHarness{t: t, c: orderFakeClient(t, orderWorld(true)...)})
		})
	}
}

func TestLakeOrdersEnvtest(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the order provisioning envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	for _, scenario := range orderScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			kube, err := client.New(config, client.Options{Scheme: clusterTestScheme(t)})
			if err != nil {
				t.Fatal(err)
			}
			// Each scenario starts from the same world: what the last one made is removed.
			ctx := context.Background()
			for _, list := range []client.ObjectList{&oramav1alpha1.LakeOrderSourceList{}, &oramav1alpha1.LakeSetList{}, &oramav1alpha1.KubernetesClusterList{},
				&oramav1alpha1.ClusterHelmReleaseList{}, &oramav1alpha1.OramaOperatorList{}, &oramav1alpha1.KubernetesClusterProviderList{}} {
				if err := kube.List(ctx, list); err != nil {
					t.Fatal(err)
				}
				items, _ := metaItems(list)
				for _, item := range items {
					if err := kube.Delete(ctx, item); err != nil && client.IgnoreNotFound(err) != nil {
						t.Fatal(err)
					}
				}
			}
			for _, obj := range orderWorld(true) {
				obj.SetResourceVersion("")
				if err := kube.Create(ctx, obj); err != nil {
					t.Fatalf("create %s: %v", obj.GetName(), err)
				}
			}
			scenario.run(&orderHarness{t: t, c: kube})
		})
	}
}

func metaItems(list client.ObjectList) ([]client.Object, error) {
	var out []client.Object
	switch l := list.(type) {
	case *oramav1alpha1.LakeOrderSourceList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *oramav1alpha1.LakeSetList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *oramav1alpha1.KubernetesClusterList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *oramav1alpha1.ClusterHelmReleaseList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *oramav1alpha1.OramaOperatorList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *oramav1alpha1.KubernetesClusterProviderList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	}
	return out, nil
}

func expectEvents(t *testing.T, db *fakeOrderDB, id string, want ...string) {
	t.Helper()
	if got := db.eventsOf(id); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events of %s are %v, want %v", id, got, want)
	}
}

func scenarioPrivateWarehouse(h *orderHarness) {
	t := h.t
	eu := newFakeOrderDB(CloudOrder{ID: warehouseOrder, Offer: offerPrivateWarehouse, Region: "swedencentral", State: cloudOrderPaid})
	h.dbs = map[string]*fakeOrderDB{"shared": eu, "shared-us": newFakeOrderDB()}

	h.reconcile()
	if o := eu.order(warehouseOrder); o.State != cloudOrderProvisioning || o.ClaimedBy != "orama-operator/LakeOrderSource/lake-live" {
		t.Fatalf("after the first pass the order is %+v, want provisioning, claimed by the source", o)
	}
	var set oramav1alpha1.LakeSet
	if !h.get("order-0a1b2c3d4e5f", &set) {
		t.Fatal("no LakeSet was made for the order")
	}
	if set.Labels[orderIDLabel] != warehouseOrder || set.Labels[orderSourceLabel] != "lake-live" || !strings.Contains(set.Annotations[orderOwnerAnnotation], warehouseOrder) {
		t.Fatalf("the LakeSet does not say which order it is for: %v %v", set.Labels, set.Annotations)
	}
	if len(set.Spec.Clusters) != 1 || set.Spec.Clusters[0].ClusterRef != "orama-production-cluster-02" || set.Spec.Clusters[0].Name != "order-0a1b2c3d4e5f" {
		t.Fatalf("a private warehouse in swedencentral runs on prod-02 as its own member: %+v", set.Spec.Clusters)
	}
	member := string(set.Spec.Clusters[0].Spec.Raw)
	for _, want := range []string{`"host":"order-0a1b2c3d4e5f.lake.live"`, `"--node","order-0a1b2c3d4e5f"`, `"clerk"`} {
		if !strings.Contains(member, want) {
			t.Fatalf("member spec %s lacks %s", member, want)
		}
	}
	if string(set.Spec.Template.Raw) == "" || !strings.Contains(string(set.Spec.Template.Raw), "oramasearch/lake:v0.2.0") {
		t.Fatalf("the template is not lake-prod's: %s", set.Spec.Template.Raw)
	}
	if set.Spec.DeletionPolicy != oramav1alpha1.SetDeletionRetain || set.Spec.Clusters[0].Secrets[0].As != "clerk-secret-key" || set.Spec.Secrets[0].Name != "oramasearch-dockerhub" {
		t.Fatalf("policy or secrets not carried: %+v", set.Spec)
	}
	if s := h.source(); s.Status.Provisioning != 1 || s.Status.Phase != setPhaseProgressing || len(s.Status.Orders) != 1 || s.Status.Orders[0].LakeSet != "order-0a1b2c3d4e5f" {
		t.Fatalf("source status: %+v", s.Status)
	}

	// Not Ready yet: it stays provisioning, and nothing is written.
	h.reconcile()
	expectEvents(t, eu, warehouseOrder, "paid->provisioning")

	h.setReady("order-0a1b2c3d4e5f")
	h.reconcile()
	if o := eu.order(warehouseOrder); o.State != cloudOrderReady || o.Host != "order-0a1b2c3d4e5f.lake.live" {
		t.Fatalf("a Ready LakeSet makes the order ready with its host, got %+v", o)
	}
	expectEvents(t, eu, warehouseOrder, "paid->provisioning", "provisioning->ready")
	if s := h.source(); s.Status.ReadyOrders != 1 || s.Status.Provisioning != 0 || s.Status.Phase != clusterPhaseReady {
		t.Fatalf("source status: %+v", s.Status)
	}
	// A ready order is left ready; its resources are kept.
	h.reconcile()
	expectEvents(t, eu, warehouseOrder, "paid->provisioning", "provisioning->ready")
}

func scenarioDedicatedCluster(h *orderHarness) {
	t := h.t
	us := newFakeOrderDB(CloudOrder{ID: dedicatedOrder, Offer: offerDedicatedCluster, CloudProvider: "azure", Region: "canadacentral", State: cloudOrderPaid})
	h.dbs = map[string]*fakeOrderDB{"shared": newFakeOrderDB(), "shared-us": us}
	h.providerReady()

	h.reconcile()
	name := "order-1b2c3d4e5f6a"
	var cluster oramav1alpha1.KubernetesCluster
	if !h.get(name, &cluster) {
		t.Fatal("no KubernetesCluster was made for the order")
	}
	if cluster.Spec.Region != "canadacentral" || cluster.Spec.ProviderRef != "azure-orama" || cluster.Spec.DeletionPolicy != oramav1alpha1.ClusterDeletionRetain ||
		cluster.Spec.KubernetesVersion != "1.35" || len(cluster.Spec.NodePools) != 1 || cluster.Spec.Tags[orderClusterTag] != dedicatedOrder ||
		cluster.Spec.Tags[clusterPurposeTag] != "production" || cluster.Labels[orderIDLabel] != dedicatedOrder {
		t.Fatalf("the cluster is not the template's in the ordered region: %+v %v", cluster.Spec, cluster.Labels)
	}
	var certManager, strimzi oramav1alpha1.ClusterHelmRelease
	if !h.get(name+"-cert-manager", &certManager) || !h.get(name+"-strimzi", &strimzi) {
		t.Fatal("the template cluster's platform was not copied")
	}
	if certManager.Spec.ClusterRef != name || strimzi.Spec.DependsOn[0] != name+"-cert-manager" || strimzi.Spec.Chart.Version != "1.2.0" {
		t.Fatalf("the platform copies point at the wrong cluster or dependency: %+v %+v", certManager.Spec, strimzi.Spec)
	}
	var releases oramav1alpha1.ClusterHelmReleaseList
	if err := h.c.List(context.Background(), &releases, client.MatchingLabels{orderIDLabel: dedicatedOrder}); err != nil {
		t.Fatal(err)
	}
	if len(releases.Items) != 2 {
		t.Fatalf("%d releases copied, want prod-03's two and none of prod-02's", len(releases.Items))
	}
	var operator oramav1alpha1.OramaOperator
	if !h.get(name, &operator) || operator.Spec.ClusterRef != name || operator.Spec.Version != "v0.6.1" {
		t.Fatalf("the template's operator was not copied: %+v", operator.Spec)
	}
	var set oramav1alpha1.LakeSet
	if !h.get(name, &set) || set.Spec.Clusters[0].ClusterRef != name {
		t.Fatalf("the Lake's set does not run on the new cluster: %+v", set.Spec.Clusters)
	}
	if o := us.order(dedicatedOrder); o.State != cloudOrderProvisioning {
		t.Fatalf("order is %s, want provisioning", o.State)
	}
	h.setReady(name)
	h.reconcile()
	if o := us.order(dedicatedOrder); o.State != cloudOrderReady || o.Host != name+".lake.live" {
		t.Fatalf("order %+v, want ready at %s.lake.live", o, name)
	}
	expectEvents(t, us, dedicatedOrder, "paid->provisioning", "provisioning->ready")
}

func scenarioFailures(h *orderHarness) {
	t := h.t
	const (
		aws      = "20000000-0000-4000-8000-000000000001"
		nowhere  = "20000000-0000-4000-8000-000000000002"
		onPrem   = "20000000-0000-4000-8000-000000000003"
		farAway  = "20000000-0000-4000-8000-000000000004"
		noRegion = "20000000-0000-4000-8000-000000000005"
	)
	db := newFakeOrderDB(
		CloudOrder{ID: aws, Offer: offerDedicatedCluster, CloudProvider: "aws", Region: "us-east-1", State: cloudOrderPaid},
		CloudOrder{ID: nowhere, Offer: offerPrivateWarehouse, Region: "westeurope", State: cloudOrderPaid},
		CloudOrder{ID: onPrem, Offer: "on_prem", Region: "eastus", State: cloudOrderPaid},
		CloudOrder{ID: farAway, Offer: offerDedicatedCluster, CloudProvider: "azure", Region: "atlantis", State: cloudOrderPaid},
		CloudOrder{ID: noRegion, Offer: offerPrivateWarehouse, State: cloudOrderPaid},
	)
	h.dbs = map[string]*fakeOrderDB{"shared": db, "shared-us": newFakeOrderDB()}
	h.providerReady()
	h.reconcile()
	for id, want := range map[string]string{
		aws:      "Azure only",
		nowhere:  `no cluster of LakeSet lake-prod runs in region "westeurope"`,
		onPrem:   "order form",
		farAway:  `region "atlantis" is not one the operator creates clusters in`,
		noRegion: "names no region",
	} {
		if o := db.order(id); o.State != cloudOrderFailed || !strings.Contains(db.reasons[id], want) {
			t.Errorf("order %s is %s with reason %q, want failed with %q", id, o.State, db.reasons[id], want)
		}
		expectEvents(t, db, id, "paid->provisioning", "provisioning->failed")
	}
	if h.get("order-200000000000", &oramav1alpha1.LakeSet{}) || h.get("order-200000000000", &oramav1alpha1.KubernetesCluster{}) {
		t.Fatal("a failed order must make nothing")
	}
	if s := h.source(); s.Status.Failed != 5 {
		t.Fatalf("source counts %d failed, want 5", s.Status.Failed)
	}
}

func scenarioNoCloudIdentity(h *orderHarness) {
	t := h.t
	// The same operator and the same source, on a cluster with no KubernetesClusterProvider: a
	// leaf, or a management cluster whose provider was never applied.
	var provider oramav1alpha1.KubernetesClusterProvider
	if h.get("azure-orama", &provider) {
		if err := h.c.Delete(context.Background(), &provider); err != nil {
			t.Fatal(err)
		}
	}
	db := newFakeOrderDB(CloudOrder{ID: dedicatedOrder, Offer: offerDedicatedCluster, CloudProvider: "azure", Region: "canadacentral", State: cloudOrderPaid},
		CloudOrder{ID: warehouseOrder, Offer: offerPrivateWarehouse, Region: "eastus", State: cloudOrderPaid})
	h.dbs = map[string]*fakeOrderDB{"shared": newFakeOrderDB(), "shared-us": db}
	h.reconcile()
	if o := db.order(dedicatedOrder); o.State != cloudOrderFailed || !strings.Contains(db.reasons[dedicatedOrder], "no cloud identity") {
		t.Fatalf("order %+v reason %q, want failed for want of a cloud identity", o, db.reasons[dedicatedOrder])
	}
	if h.get("order-1b2c3d4e5f6a", &oramav1alpha1.KubernetesCluster{}) {
		t.Fatal("no cluster may be made without the cloud identity")
	}
	// A private warehouse needs no cloud identity: it is still provisioned.
	if o := db.order(warehouseOrder); o.State != cloudOrderProvisioning {
		t.Fatalf("the private warehouse is %s, want provisioning", o.State)
	}

	// A provider whose credentials cannot be read is no identity either.
	provider = oramav1alpha1.KubernetesClusterProvider{ObjectMeta: metav1.ObjectMeta{Name: "azure-orama"},
		Spec: oramav1alpha1.KubernetesClusterProviderSpec{Type: oramav1alpha1.KubernetesClusterProviderAKS,
			CredentialsSecretRef: oramav1alpha1.ClusterSecretReference{Name: "azure-cluster-management", Namespace: "orama-system"},
			AKS:                  &oramav1alpha1.AksProviderSettings{SubscriptionID: "aff019e7-3b91-47a8-8cd7-4ceef2ddb47e"}}}
	if err := h.c.Create(context.Background(), &provider); err != nil {
		t.Fatal(err)
	}
	provider.Status = oramav1alpha1.KubernetesClusterProviderStatus{Ready: false, ObservedGeneration: provider.Generation,
		Message: "credentials Secret orama-system/azure-cluster-management does not exist"}
	if err := h.c.Status().Update(context.Background(), &provider); err != nil {
		t.Fatal(err)
	}
	const second = "30000000-0000-4000-8000-000000000001"
	db.mu.Lock()
	db.orders[second] = &CloudOrder{ID: second, Offer: offerDedicatedCluster, CloudProvider: "azure", Region: "canadacentral", State: cloudOrderPaid}
	db.mu.Unlock()
	h.reconcile()
	if o := db.order(second); o.State != cloudOrderFailed || !strings.Contains(db.reasons[second], "has no usable credentials") {
		t.Fatalf("order %+v reason %q, want failed for unusable credentials", o, db.reasons[second])
	}
}

func scenarioExactlyOnce(h *orderHarness) {
	t := h.t
	eu := newFakeOrderDB(CloudOrder{ID: warehouseOrder, Offer: offerPrivateWarehouse, Region: "swedencentral", State: cloudOrderPaid})
	h.dbs = map[string]*fakeOrderDB{"shared": eu, "shared-us": newFakeOrderDB()}
	// Four pollers at once: one claim.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.reconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "lake-live"}})
		}()
	}
	wg.Wait()
	expectEvents(t, eu, warehouseOrder, "paid->provisioning")
	var sets oramav1alpha1.LakeSetList
	if err := h.c.List(context.Background(), &sets, client.MatchingLabels{orderIDLabel: warehouseOrder}); err != nil {
		t.Fatal(err)
	}
	if len(sets.Items) != 1 {
		t.Fatalf("%d LakeSets for one order", len(sets.Items))
	}
	// A restart: a new reconciler, the same rows. It resumes its claim and claims nothing again.
	h.reconcile()
	h.setReady("order-0a1b2c3d4e5f")
	h.reconcile()
	h.reconcile()
	expectEvents(t, eu, warehouseOrder, "paid->provisioning", "provisioning->ready")

	// A claim held by another source is never touched.
	const other = "40000000-0000-4000-8000-000000000001"
	eu.mu.Lock()
	eu.orders[other] = &CloudOrder{ID: other, Offer: offerPrivateWarehouse, Region: "swedencentral", State: cloudOrderProvisioning, ClaimedBy: "orama-operator/LakeOrderSource/elsewhere"}
	eu.mu.Unlock()
	h.reconcile()
	if h.get(orderResourceName(other), &oramav1alpha1.LakeSet{}) || len(eu.eventsOf(other)) != 0 {
		t.Fatal("an order another source holds was provisioned")
	}
}

func scenarioNothingClaimed(h *orderHarness) {
	t := h.t
	eu := newFakeOrderDB(CloudOrder{ID: warehouseOrder, Offer: offerPrivateWarehouse, Region: "swedencentral", State: cloudOrderPaid})
	us := newFakeOrderDB()
	us.missing = true
	h.dbs = map[string]*fakeOrderDB{"shared": eu, "shared-us": us}
	h.reconcile()
	if s := h.source(); s.Status.Phase != clusterPhasePending || !strings.Contains(s.Status.Message, "cloud_orders") || s.Status.Members[1].Reachable {
		t.Fatalf("a member without the table: %+v", s.Status)
	}
	// The other member's orders are still provisioned.
	if eu.order(warehouseOrder).State != cloudOrderProvisioning {
		t.Fatal("one member without the table must not hold back another")
	}

	// A source that sets what the operator sets per order reads no order at all.
	const later = "50000000-0000-4000-8000-000000000001"
	eu.mu.Lock()
	eu.orders[later] = &CloudOrder{ID: later, Offer: offerPrivateWarehouse, Region: "swedencentral", State: cloudOrderPaid}
	eu.mu.Unlock()
	source := h.source()
	source.Spec.Member = rawJSON(`{"env":[{"name":"LAKE_PUBLIC_HOST","value":"x.lake.live"}]}`)
	if err := h.c.Update(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if s := h.source(); s.Status.Phase != clusterPhaseDegraded || !strings.Contains(s.Status.Message, "LAKE_PUBLIC_HOST") {
		t.Fatalf("status %+v", s.Status)
	}
	if eu.order(later).State != cloudOrderPaid {
		t.Fatal("a refused source claimed an order")
	}
}

// orderResourceName is stable and a valid name for every kind it names.
func TestOrderResourceName(t *testing.T) {
	if got := orderResourceName("0A1B2C3D-4E5F-4A6B-8C7D-9E0F1A2B3C4D"); got != "order-0a1b2c3d4e5f" {
		t.Fatalf("got %s", got)
	}
}

// Lake's CHECKs on failure_reason and the event's detail (2000 characters): a longer text is cut,
// so the move to failed is not refused on every pass.
func TestCloudOrderTextFitsLakesLimit(t *testing.T) {
	if got := cloudOrderText("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", cloudOrderTextLimit+50)
	got := cloudOrderText(long)
	if n := len([]rune(got)); n != cloudOrderTextLimit {
		t.Fatalf("cut to %d characters, want %d", n, cloudOrderTextLimit)
	}
}

// Lake's CHECK on claimed_by (255 characters): a source whose claimant would be longer is refused
// and claims nothing, instead of failing the claim on every pass.
func TestLakeOrderSourceNameTooLongClaimsNothing(t *testing.T) {
	c := orderFakeClient(t, orderWorld(true)...)
	ctx := context.Background()
	var source oramav1alpha1.LakeOrderSource
	if err := c.Get(ctx, types.NamespacedName{Name: "lake-live"}, &source); err != nil {
		t.Fatal(err)
	}
	long := source.DeepCopy()
	long.ObjectMeta = metav1.ObjectMeta{Name: strings.Repeat("a", 225)}
	long.Status = oramav1alpha1.LakeOrderSourceStatus{}
	if err := c.Create(ctx, long); err != nil {
		t.Fatal(err)
	}
	eu := newFakeOrderDB(CloudOrder{ID: warehouseOrder, Offer: offerPrivateWarehouse, Region: "swedencentral", State: cloudOrderPaid})
	r := &LakeOrderSourceReconciler{Client: c, Scheme: c.Scheme(), Orders: fakeOrderDBs(map[string]*fakeOrderDB{"shared": eu, "shared-us": newFakeOrderDB()})}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: long.Name}}); err != nil {
		t.Fatal(err)
	}
	var got oramav1alpha1.LakeOrderSource
	if err := c.Get(ctx, types.NamespacedName{Name: long.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, "too long") {
		t.Fatalf("status %+v", got.Status)
	}
	if eu.order(warehouseOrder).State != cloudOrderPaid {
		t.Fatal("a refused source claimed an order")
	}
}
