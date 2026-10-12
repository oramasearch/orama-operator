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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	now      time.Time
}

func controllerPod(name string, uid types.UID, started time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ingress-nginx", UID: uid, Labels: map[string]string{
			"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/component": "controller",
		}},
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
	objects := append([]client.Object{app, controllerPod("controller-a", "pod-a", start.Add(-24*time.Hour))}, extra...)
	rig := &appTransferRig{
		t: t, store: store,
		ingress:  ingressKey{atWorkspaceNS, lakeAppWorkloadObjectName(app)},
		counters: map[string]map[ingressKey]ingressBytes{},
		failing:  map[string]bool{},
		now:      start.Add(10 * time.Minute),
	}
	rig.h = newServiceHarness(t, lake, store, objects...)
	rig.h.reconciler.AppTransferScrape = func(_ context.Context, pod *corev1.Pod) (map[ingressKey]ingressBytes, bool, error) {
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
