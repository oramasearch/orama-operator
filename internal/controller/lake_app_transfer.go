/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// App traffic through the ingress (lake#661; lake migration 0081_app_transfer; this repo's
// docs/lake/app-transfer.md). lake meters an app's bytes as `transfer`, but they never pass
// through lake: they go from the ingress controller to the app's pods. The ingress controller
// counts them per Ingress. ingress-nginx, the controller the clusters run, exposes them on its
// metrics port (10254) once its metrics are enabled (`--enable-metrics=true`, the chart's
// `controller.metrics.enabled=true`): `nginx_ingress_controller_request_size_sum` (the bytes
// clients sent: request line, headers and body) and `nginx_ingress_controller_response_size_sum`
// (the bytes sent back), labelled with the Ingress's `namespace` and `ingress`.
//
// Each pass, the operator reads every controller pod's counters, maps each app's Ingress to its
// app, and adds the increase since the previous pass to the app_transfer row of the app and the
// current UTC hour, in one statement. Counters are per controller pod and restart from zero with
// it, so the operator keeps the last value per pod and series: a lower value is a restart and
// counts whole; a pod or series it has not seen counts whole when it appeared after the operator
// started, and is the baseline otherwise. Only numbers are written.

// RBAC: pods get/list (already granted for workload resources, lake_workload_resources.go).

const (
	// appTransferMetricsPort is ingress-nginx's metrics port.
	appTransferMetricsPort = 10254
	// appTransferRetention is how long app_transfer rows are kept; lake charges an hour within
	// seven days of it.
	appTransferRetention     = 8 * 24 * time.Hour
	appTransferScrapeTimeout = 5 * time.Second
	// appTransferScrapeLimit bounds one metrics answer.
	appTransferScrapeLimit = 32 << 20

	ingressNginxRequestBytes  = "nginx_ingress_controller_request_size"
	ingressNginxResponseBytes = "nginx_ingress_controller_response_size"
)

// ingressControllerPodLabels select the ingress-nginx controller pods in any namespace: the
// labels its Helm chart puts on them.
var ingressControllerPodLabels = client.MatchingLabels{
	"app.kubernetes.io/name":      "ingress-nginx",
	"app.kubernetes.io/component": "controller",
}

var errAppTransferTableMissing = errors.New("the app_transfer table does not exist yet (lake schema before 0081_app_transfer)")

// AppTransfer is one app's traffic through the ingress added to one UTC hour.
type AppTransfer struct {
	AppID       string
	WorkspaceID string
	Hour        time.Time
	InBytes     int64
	OutBytes    int64
}

// ingressBytes are an Ingress's counters on one controller pod.
type ingressBytes struct{ in, out float64 }

// ingressKey names an Ingress.
type ingressKey struct{ namespace, name string }

// appTransferState holds, per Lake, the last counters read from each controller pod.
type appTransferState struct {
	mu      sync.Mutex
	started time.Time
	// counters[lake][pod] is every series of that pod at the last pass that wrote.
	counters map[types.UID]map[types.UID]map[ingressKey]ingressBytes
}

// begin returns when the operator started reading counters and the Lake's last counters.
func (s *appTransferState) begin(lake types.UID, now time.Time) (time.Time, map[types.UID]map[ingressKey]ingressBytes) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started.IsZero() {
		s.started = now
	}
	if s.counters == nil {
		s.counters = map[types.UID]map[types.UID]map[ingressKey]ingressBytes{}
	}
	return s.started, s.counters[lake]
}

func (s *appTransferState) commit(lake types.UID, counters map[types.UID]map[ingressKey]ingressBytes) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[lake] = counters
}

// parseIngressBytes reads the per-Ingress byte counters of one ingress-nginx metrics answer,
// summing every series of an Ingress (its hosts, paths, methods, statuses and services). found is
// false when the answer has neither family: the controller's metrics are off.
func parseIngressBytes(body io.Reader) (map[ingressKey]ingressBytes, bool, error) {
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(body)
	if err != nil {
		return nil, false, err
	}
	out := map[ingressKey]ingressBytes{}
	found := false
	add := func(name string, in bool) {
		family, ok := families[name]
		if !ok {
			return
		}
		found = true
		for _, metric := range family.GetMetric() {
			key, sum := ingressSeries(metric)
			if key.name == "" || key.namespace == "" || math.IsNaN(sum) || sum < 0 {
				continue
			}
			bytes := out[key]
			if in {
				bytes.in += sum
			} else {
				bytes.out += sum
			}
			out[key] = bytes
		}
	}
	add(ingressNginxRequestBytes, true)
	add(ingressNginxResponseBytes, false)
	return out, found, nil
}

// ingressSeries is the Ingress a histogram series counts for and its sum of bytes.
func ingressSeries(metric *dto.Metric) (ingressKey, float64) {
	var key ingressKey
	for _, label := range metric.GetLabel() {
		switch label.GetName() {
		case "namespace":
			key.namespace = label.GetValue()
		case "ingress":
			key.name = label.GetValue()
		}
	}
	if histogram := metric.GetHistogram(); histogram != nil {
		return key, histogram.GetSampleSum()
	}
	if summary := metric.GetSummary(); summary != nil {
		return key, summary.GetSampleSum()
	}
	return key, math.NaN()
}

// counterIncrease is how much a counter rose since `previous`. A lower value is a restart: the
// counter counts from zero again, so all of it is new.
func counterIncrease(previous, current float64) float64 {
	if current < previous {
		return current
	}
	return current - previous
}

// scrapeIngressController reads one controller pod's metrics answer.
func (r *LakeReconciler) scrapeIngressController(ctx context.Context, pod *corev1.Pod) (map[ingressKey]ingressBytes, bool, error) {
	if r.AppTransferScrape != nil {
		return r.AppTransferScrape(ctx, pod)
	}
	ctx, cancel := context.WithTimeout(ctx, appTransferScrapeTimeout)
	defer cancel()
	url := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(appTransferMetricsPort)) + "/metrics"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("User-Agent", "orama-operator")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("metrics answered %s", response.Status)
	}
	return parseIngressBytes(io.LimitReader(response.Body, appTransferScrapeLimit))
}

// lakeAppIngresses maps each app Ingress of the Lake to its app: the LakeApps the apps sync
// created, their Ingress named and placed as reconcileLakeAppIngress does.
func (r *LakeReconciler) lakeAppIngresses(ctx context.Context, lake *oramav1alpha1.Lake) (map[ingressKey]oramav1alpha1.LakeAppSync, error) {
	var apps oramav1alpha1.LakeAppList
	if err := r.reader().List(ctx, &apps, client.InNamespace(lakeNamespace(lake))); err != nil {
		return nil, err
	}
	out := map[ingressKey]oramav1alpha1.LakeAppSync{}
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.LakeRef != lake.Name || app.Spec.Sync == nil || app.Spec.Sync.ID == "" || app.Spec.Sync.WorkspaceID == "" {
			continue
		}
		out[ingressKey{lakeAppNamespace(app), lakeAppWorkloadObjectName(app)}] = *app.Spec.Sync
	}
	return out, nil
}

// reconcileAppTransfer is one pass. A failed read of the apps or the controller pods writes
// nothing; a controller pod that cannot be read keeps its last counters for the next pass, so its
// bytes are counted then. Counters advance only once the rows are written.
func (r *LakeReconciler) reconcileAppTransfer(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore, now time.Time) error {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	ingresses, err := r.lakeAppIngresses(ctx, lake)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, ingressControllerPodLabels); err != nil {
		return fmt.Errorf("list ingress controller pods: %w", err)
	}
	started, previous := r.appTransfer.begin(lake.UID, now)

	next := map[types.UID]map[ingressKey]ingressBytes{}
	totals := map[string]*AppTransfer{}
	hour := now.UTC().Truncate(time.Hour)
	reporting := false
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		counters, found, err := r.scrapeIngressController(ctx, pod)
		if err != nil {
			log.Info("app transfer: cannot read an ingress controller's metrics", "pod", pod.Namespace+"/"+pod.Name, "err", err.Error())
			if last, ok := previous[pod.UID]; ok {
				next[pod.UID] = last
			}
			continue
		}
		reporting = reporting || found
		last, known := previous[pod.UID]
		// A pod first seen counts whole if it started after the operator began reading:
		// everything it counted is new. Otherwise its counters are the baseline.
		fresh := pod.Status.StartTime != nil && pod.Status.StartTime.After(started)
		mine := map[ingressKey]ingressBytes{}
		for key, current := range counters {
			app, ok := ingresses[key]
			if !ok {
				continue
			}
			mine[key] = current
			var in, out float64
			switch before, seen := last[key]; {
			case seen:
				in, out = counterIncrease(before.in, current.in), counterIncrease(before.out, current.out)
			case known || fresh:
				in, out = current.in, current.out
			}
			if in < 1 && out < 1 {
				continue
			}
			total := totals[app.ID]
			if total == nil {
				total = &AppTransfer{AppID: app.ID, WorkspaceID: app.WorkspaceID, Hour: hour}
				totals[app.ID] = total
			}
			total.InBytes += int64(in)
			total.OutBytes += int64(out)
		}
		next[pod.UID] = mine
	}
	if !reporting && len(ingresses) > 0 {
		log.Info("app transfer: no ingress controller reports per-Ingress byte counters; enable ingress-nginx's metrics (controller.metrics.enabled=true)")
	}

	rows := make([]AppTransfer, 0, len(totals))
	for _, total := range totals {
		rows = append(rows, *total)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].AppID < rows[j].AppID })
	if err := store.AddAppTransfer(ctx, rows, now.Add(-appTransferRetention)); err != nil {
		if errors.Is(err, errAppTransferTableMissing) {
			// Nothing can be recorded yet; keep the counters as the baseline.
			r.appTransfer.commit(lake.UID, next)
		}
		return err
	}
	r.appTransfer.commit(lake.UID, next)
	return nil
}
