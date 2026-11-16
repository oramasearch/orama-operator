// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// An ingress-nginx metrics answer, cut to the two families the pass reads plus one it ignores.
const ingressNginxMetrics = `# HELP nginx_ingress_controller_request_size The request length (including request line, header, and request body)
# TYPE nginx_ingress_controller_request_size histogram
nginx_ingress_controller_request_size_bucket{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="GET",namespace="ws-ns",path="/",service="app-x",status="200",le="+Inf"} 3
nginx_ingress_controller_request_size_sum{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="GET",namespace="ws-ns",path="/",service="app-x",status="200"} 300
nginx_ingress_controller_request_size_count{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="GET",namespace="ws-ns",path="/",service="app-x",status="200"} 3
nginx_ingress_controller_request_size_bucket{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="POST",namespace="ws-ns",path="/",service="app-x",status="404",le="+Inf"} 1
nginx_ingress_controller_request_size_sum{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="POST",namespace="ws-ns",path="/",service="app-x",status="404"} 50
nginx_ingress_controller_request_size_count{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="POST",namespace="ws-ns",path="/",service="app-x",status="404"} 1
nginx_ingress_controller_request_size_bucket{controller_class="k8s.io/ingress-nginx",host="lake.example",ingress="lake-prod",method="GET",namespace="lake",path="/",service="lake",status="200",le="+Inf"} 1
nginx_ingress_controller_request_size_sum{controller_class="k8s.io/ingress-nginx",host="lake.example",ingress="lake-prod",method="GET",namespace="lake",path="/",service="lake",status="200"} 11
nginx_ingress_controller_request_size_count{controller_class="k8s.io/ingress-nginx",host="lake.example",ingress="lake-prod",method="GET",namespace="lake",path="/",service="lake",status="200"} 1
# HELP nginx_ingress_controller_response_size The response length (including request line, header, and request body)
# TYPE nginx_ingress_controller_response_size histogram
nginx_ingress_controller_response_size_bucket{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="GET",namespace="ws-ns",path="/",service="app-x",status="200",le="+Inf"} 3
nginx_ingress_controller_response_size_sum{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="GET",namespace="ws-ns",path="/",service="app-x",status="200"} 9000
nginx_ingress_controller_response_size_count{controller_class="k8s.io/ingress-nginx",host="a.example",ingress="app-x",method="GET",namespace="ws-ns",path="/",service="app-x",status="200"} 3
# HELP nginx_ingress_controller_requests The total number of client requests
# TYPE nginx_ingress_controller_requests counter
nginx_ingress_controller_requests{ingress="app-x",namespace="ws-ns",status="200"} 3
`

func TestParseIngressBytesSumsEveryIngressSeries(t *testing.T) {
	counters, found, err := parseIngressBytes(strings.NewReader(ingressNginxMetrics))
	if err != nil || !found {
		t.Fatalf("parse: found=%v err=%v", found, err)
	}
	if got := counters[ingressKey{"ws-ns", "app-x"}]; got.in != 350 || got.out != 9000 {
		t.Fatalf("app-x = %+v, want in 350 (two series) and out 9000", got)
	}
	if got := counters[ingressKey{"lake", "lake-prod"}]; got.in != 11 || got.out != 0 {
		t.Fatalf("lake-prod = %+v", got)
	}
	// A controller with its metrics off answers only process and Go metrics.
	_, found, err = parseIngressBytes(strings.NewReader("# TYPE go_goroutines gauge\ngo_goroutines 12\n"))
	if err != nil || found {
		t.Fatalf("metrics off: found=%v err=%v", found, err)
	}
	if counterIncrease(100, 140) != 40 || counterIncrease(100, 30) != 30 {
		t.Fatal("a counter that went down restarted from zero")
	}
}

const (
	atAppID       = "bbbbbbbb-0000-4000-8000-000000000001"
	atWorkspaceID = "bbbbbbbb-0000-4000-8000-000000000002"
	atWorkspaceNS = "ws-transfer"
)

type appTransferRig struct {
	t        *testing.T
	h        *serviceHarness
	store    *fakeServiceStore
	ingress  ingressKey
	counters map[string]map[ingressKey]ingressBytes // by controller pod name
	failing  map[string]bool
	scraped  map[string]int
	now      time.Time
}

func controllerPod(name string, uid types.UID, started time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ingress-nginx", UID: uid, Labels: map[string]string{
			"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/component": "controller",
		}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
			&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "ingress-controller-rs", UID: "rs-uid"}}, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1", StartTime: &metav1.Time{Time: started}},
	}
}

func newAppTransferRig(t *testing.T, extra ...client.Object) *appTransferRig {
	t.Helper()
	lake := serviceTestLake()
	store := &fakeServiceStore{}
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app-transfer", Namespace: lakeNamespace(lake), OwnerReferences: lakeOwnerRef(lake)},
		Spec: oramav1alpha1.LakeAppSpec{LakeRef: lake.Name, Sync: &oramav1alpha1.LakeAppSync{
			ID: atAppID, CustomerID: "c", WorkspaceID: atWorkspaceID, WorkspaceNamespace: atWorkspaceNS,
		}},
	}
	start := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "ingress-nginx-controller", Namespace: "ingress-nginx", UID: "deployment-uid",
	}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "ingress-controller-rs", Namespace: "ingress-nginx", UID: "rs-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
	}}
	workspace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: atWorkspaceNS, Labels: map[string]string{
		workspaceLabel: atWorkspaceID, workspaceCustomerLabel: "c", workspaceLakeLabel: lake.Name, workspaceManagedByLabel: "orama-operator",
	}}}
	objects := append([]client.Object{app, workspace, deployment, rs, controllerPod("controller-a", "pod-a", start.Add(-24*time.Hour))}, extra...)
	rig := &appTransferRig{
		t: t, store: store,
		ingress:  ingressKey{atWorkspaceNS, lakeAppWorkloadObjectName(app)},
		counters: map[string]map[ingressKey]ingressBytes{},
		failing:  map[string]bool{},
		scraped:  map[string]int{},
		now:      start.Add(10 * time.Minute),
	}
	rig.h = newServiceHarness(t, lake, store, objects...)
	rig.h.reconciler.AppTransferScrape = func(_ context.Context, pod *corev1.Pod) (map[ingressKey]ingressBytes, bool, error) {
		rig.scraped[pod.Namespace+"/"+pod.Name]++
		if rig.failing[pod.Name] {
			return nil, false, errors.New("connection refused")
		}
		out := map[ingressKey]ingressBytes{
			// Another Ingress on the same controller, not an app of this Lake.
			{"lake", "lake-prod"}: {in: 1e6, out: 1e6},
		}
		for key, bytes := range rig.counters[pod.Name] {
			out[key] = bytes
		}
		return out, true, nil
	}
	return rig
}

func (r *appTransferRig) pass(after time.Duration) error {
	r.now = r.now.Add(after)
	return r.h.reconciler.reconcileAppTransfer(context.Background(), r.h.lake, r.store, r.now)
}

func (r *appTransferRig) hour(at time.Time) AppTransfer {
	return r.store.appTransfer[atAppID+"@"+at.UTC().Truncate(time.Hour).Format(time.RFC3339)]
}

func TestAppTransferAddsEachPassIncreaseToTheAppsHour(t *testing.T) {
	rig := newAppTransferRig(t)
	rig.counters["controller-a"] = map[ingressKey]ingressBytes{rig.ingress: {in: 5000, out: 70000}}
	// The first pass is the baseline: the controller counted that before the operator started.
	if err := rig.pass(0); err != nil {
		t.Fatal(err)
	}
	if len(rig.store.appTransfer) != 0 {
		t.Fatalf("baseline wrote %v", rig.store.appTransfer)
	}
	rig.counters["controller-a"] = map[ingressKey]ingressBytes{rig.ingress: {in: 5100, out: 71000}}
	if err := rig.pass(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	first := rig.now
	if got := rig.hour(first); got.InBytes != 100 || got.OutBytes != 1000 || got.WorkspaceID != atWorkspaceID {
		t.Fatalf("hour = %+v, want 100 in and 1000 out for the app's workspace", got)
	}
	// The controller restarted its counters: what it counted since is all new.
	rig.counters["controller-a"] = map[ingressKey]ingressBytes{rig.ingress: {in: 10, out: 20}}
	if err := rig.pass(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if got := rig.hour(first); got.InBytes != 110 || got.OutBytes != 1020 {
		t.Fatalf("after a restart = %+v", got)
	}
	// An unreadable controller writes nothing and keeps its counters: the next pass counts it all.
	rig.failing["controller-a"] = true
	rig.counters["controller-a"] = map[ingressKey]ingressBytes{rig.ingress: {in: 40, out: 50}}
	if err := rig.pass(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if got := rig.hour(first); got.InBytes != 110 {
		t.Fatalf("an unread controller added %+v", got)
	}
	rig.failing["controller-a"] = false
	if err := rig.pass(time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := rig.hour(rig.now); got.InBytes != 30 || got.OutBytes != 30 {
		t.Fatalf("the next hour = %+v, want what moved since the last read", got)
	}
	if got := rig.hour(first); got.InBytes != 110 {
		t.Fatalf("a closed hour changed: %+v", got)
	}
	// A failed write keeps the counters, so nothing is lost or counted twice.
	rig.counters["controller-a"] = map[ingressKey]ingressBytes{rig.ingress: {in: 45, out: 50}}
	rig.store.appTransferErr = errors.New("connection reset")
	if err := rig.pass(time.Second); err == nil {
		t.Fatal("the failed write is reported")
	}
	rig.store.appTransferErr = nil
	if err := rig.pass(time.Second); err != nil {
		t.Fatal(err)
	}
	if got := rig.hour(rig.now); got.InBytes != 35 || got.OutBytes != 30 {
		t.Fatalf("after a failed write = %+v", got)
	}
	// Rows older than the retention are deleted.
	if err := rig.pass(9 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(rig.store.appTransfer) != 0 {
		t.Fatalf("expired rows kept: %v", rig.store.appTransfer)
	}
}

func TestAppTransferCountsANewControllerWholeAndSkipsBeforeTheSchema(t *testing.T) {
	rig := newAppTransferRig(t)
	rig.counters["controller-a"] = map[ingressKey]ingressBytes{rig.ingress: {in: 100, out: 100}}
	rig.store.appTransferMissing = true
	if err := rig.pass(0); !errors.Is(err, errAppTransferTableMissing) {
		t.Fatalf("err = %v, want the missing table", err)
	}
	rig.store.appTransferMissing = false
	// A controller pod started after the operator began reading: all it counted is new.
	newPod := controllerPod("controller-b", "pod-b", rig.now.Add(time.Minute))
	if err := rig.h.reconciler.Client.Create(context.Background(), newPod); err != nil {
		t.Fatal(err)
	}
	rig.counters["controller-a"] = map[ingressKey]ingressBytes{rig.ingress: {in: 160, out: 100}}
	rig.counters["controller-b"] = map[ingressKey]ingressBytes{rig.ingress: {in: 7, out: 9}}
	if err := rig.pass(2 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := rig.hour(rig.now); got.InBytes != 67 || got.OutBytes != 9 {
		t.Fatalf("hour = %+v, want 60 from the baselined controller and 7 and 9 from the new one", got)
	}
}

func TestAppTransferScrapesOnlyPlatformControllerPods(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted bool
		mutate  func(*corev1.Pod, *appsv1.ReplicaSet)
	}{
		{"deployment", true, func(_ *corev1.Pod, _ *appsv1.ReplicaSet) {}},
		{"another namespace", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) { p.Namespace = "tenant" }},
		{"labels alone", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) { p.OwnerReferences = nil }},
		{"non-controller reference", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) { *p.OwnerReferences[0].Controller = false }},
		{"pod owner API version", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) { p.OwnerReferences[0].APIVersion = "untrusted/v1" }},
		{"missing ReplicaSet", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) { p.OwnerReferences[0].Name = "missing" }},
		{"replaced ReplicaSet", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) { p.OwnerReferences[0].UID = "old-rs" }},
		{"empty pod owner UID", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) { p.OwnerReferences[0].UID = "" }},
		{"unowned ReplicaSet", false, func(_ *corev1.Pod, rs *appsv1.ReplicaSet) { rs.OwnerReferences = nil }},
		{"unrelated Deployment", false, func(_ *corev1.Pod, rs *appsv1.ReplicaSet) { rs.OwnerReferences[0].Name = "other-controller" }},
		{"replaced Deployment", false, func(_ *corev1.Pod, rs *appsv1.ReplicaSet) { rs.OwnerReferences[0].UID = "old-deployment" }},
		{"empty Deployment UID", false, func(_ *corev1.Pod, rs *appsv1.ReplicaSet) { rs.OwnerReferences[0].UID = "" }},
		{"Deployment API version", false, func(_ *corev1.Pod, rs *appsv1.ReplicaSet) { rs.OwnerReferences[0].APIVersion = "untrusted/v1" }},
		{"non-controller Deployment reference", false, func(_ *corev1.Pod, rs *appsv1.ReplicaSet) { *rs.OwnerReferences[0].Controller = false }},
		{"direct Deployment reference", false, func(p *corev1.Pod, rs *appsv1.ReplicaSet) { p.OwnerReferences = rs.OwnerReferences }},
		{"DaemonSet", true, func(p *corev1.Pod, _ *appsv1.ReplicaSet) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ingress-nginx-controller", UID: "ds-uid", Controller: boolPtr(true)}}
		}},
		{"replaced DaemonSet", false, func(p *corev1.Pod, _ *appsv1.ReplicaSet) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ingress-nginx-controller", UID: "old-ds", Controller: boolPtr(true)}}
		}},
		{"ReplicaSet owned by DaemonSet", false, func(_ *corev1.Pod, rs *appsv1.ReplicaSet) {
			rs.OwnerReferences[0].Kind, rs.OwnerReferences[0].UID = "DaemonSet", "ds-uid"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAppTransferRig(t, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
				Name: "ingress-nginx-controller", Namespace: "ingress-nginx", UID: "ds-uid",
			}})
			ctx := context.Background()
			c := rig.h.reconciler.Client
			var pod corev1.Pod
			var rs appsv1.ReplicaSet
			if err := c.Get(ctx, client.ObjectKey{Namespace: "ingress-nginx", Name: "controller-a"}, &pod); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKey{Namespace: "ingress-nginx", Name: "ingress-controller-rs"}, &rs); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&pod, &rs)
			pod.ResourceVersion = ""
			if err := c.Create(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			if err := c.Update(ctx, &rs); err != nil {
				t.Fatal(err)
			}
			// An impostor advertises a known victim Ingress, then increases its counter.
			rig.counters[pod.Name] = map[ingressKey]ingressBytes{rig.ingress: {in: 100, out: 100}}
			if err := rig.pass(0); err != nil {
				t.Fatal(err)
			}
			rig.counters[pod.Name][rig.ingress] = ingressBytes{in: 110, out: 1000100}
			if err := rig.pass(time.Minute); err != nil {
				t.Fatal(err)
			}
			if tc.trusted {
				if got := rig.hour(rig.now); got.InBytes != 10 || got.OutBytes != 1000000 {
					t.Fatalf("trusted controller usage = %+v", got)
				}
			} else if rig.scraped[pod.Namespace+"/"+pod.Name] != 0 || len(rig.store.appTransfer) != 0 {
				t.Fatalf("untrusted pod scraped %d times, usage = %v", rig.scraped[pod.Namespace+"/"+pod.Name], rig.store.appTransfer)
			}
		})
	}
}

func TestAppTransferRejectsAppsOutsideTheLakesWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*oramav1alpha1.LakeApp, *corev1.Namespace)
	}{
		{"missing workspace", func(app *oramav1alpha1.LakeApp, _ *corev1.Namespace) { app.Spec.Sync.WorkspaceNamespace = "missing" }},
		{"another Lake", func(_ *oramav1alpha1.LakeApp, ns *corev1.Namespace) { ns.Labels[workspaceLakeLabel] = "other-lake" }},
		{"another workspace", func(_ *oramav1alpha1.LakeApp, ns *corev1.Namespace) { ns.Labels[workspaceLabel] = "other-workspace" }},
		{"another customer", func(_ *oramav1alpha1.LakeApp, ns *corev1.Namespace) {
			ns.Labels[workspaceCustomerLabel] = "other-customer"
		}},
		{"unmanaged namespace", func(_ *oramav1alpha1.LakeApp, ns *corev1.Namespace) { delete(ns.Labels, workspaceManagedByLabel) }},
		{"unowned app", func(app *oramav1alpha1.LakeApp, _ *corev1.Namespace) { app.OwnerReferences = nil }},
		{"replaced Lake owner", func(app *oramav1alpha1.LakeApp, _ *corev1.Namespace) { app.OwnerReferences[0].UID = "other-lake-uid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAppTransferRig(t)
			ctx := context.Background()
			c := rig.h.reconciler.Client
			var app oramav1alpha1.LakeApp
			var ns corev1.Namespace
			if err := c.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(rig.h.lake), Name: "app-transfer"}, &app); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKey{Name: atWorkspaceNS}, &ns); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&app, &ns)
			if err := c.Update(ctx, &app); err != nil {
				t.Fatal(err)
			}
			if err := c.Update(ctx, &ns); err != nil {
				t.Fatal(err)
			}
			key := ingressKey{lakeAppNamespace(&app), lakeAppWorkloadObjectName(&app)}
			rig.counters["controller-a"] = map[ingressKey]ingressBytes{key: {in: 100, out: 100}}
			if err := rig.pass(0); err != nil {
				t.Fatal(err)
			}
			rig.counters["controller-a"][key] = ingressBytes{in: 1000, out: 1000}
			if err := rig.pass(time.Minute); err != nil {
				t.Fatal(err)
			}
			if len(rig.store.appTransfer) != 0 {
				t.Fatalf("untrusted workspace usage = %v", rig.store.appTransfer)
			}
		})
	}
}

func TestAppTransferOwnershipReadFailsClosed(t *testing.T) {
	rig := newAppTransferRig(t)
	rig.h.reconciler.APIReader = interceptor.NewClient(rig.h.reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.ReplicaSet); ok {
				return errors.New("ownership lookup forbidden")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := rig.pass(0); err == nil || !strings.Contains(err.Error(), "ownership lookup forbidden") {
		t.Fatalf("ownership failure = %v", err)
	}
	if len(rig.scraped) != 0 || len(rig.store.appTransfer) != 0 {
		t.Fatalf("ownership failure scraped %v, wrote %v", rig.scraped, rig.store.appTransfer)
	}
}
