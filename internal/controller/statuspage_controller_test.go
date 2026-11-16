// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeLake answers like a lake region: /readyz, /v1/server_info, and an authenticated
// /v1/workspace_engine_info. down makes every route answer 503.
type fakeLake struct {
	down   atomic.Bool
	region string
	tokens atomic.Int32
}

func (f *fakeLake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"ok":false,"error":{"code":"store_unavailable"}}`)
		return
	}
	switch r.URL.Path {
	case "/readyz":
		fmt.Fprint(w, `{"ok":true,"store":"postgres","store_root":"open"}`)
	case "/v1/server_info":
		fmt.Fprintf(w, `{"ok":true,"value":{"region":{"name":%q},"version":"0.6.1","online_payment":true}}`, f.region)
	case "/v1/workspace_engine_info":
		if r.Header.Get("Authorization") != "Bearer probe-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"ok":false,"error":{"code":"unauthenticated"}}`)
			return
		}
		f.tokens.Add(1)
		if r.URL.Query().Get("workspace_id") != "ws-probe" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"ok":false,"error":{"code":"not_found"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"value":{"engine":"zetta"}}`)
	default:
		http.NotFound(w, r)
	}
}

// fakeStatusBucket records S3 PUTs.
type fakeStatusBucket struct {
	mu      sync.Mutex
	puts    map[string]int
	objects map[string][]byte
	headers map[string]http.Header
	fail    bool
}

func (f *fakeStatusBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key-id/") ||
		!strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=cache-control;content-type;host;") {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	if f.fail {
		http.Error(w, "<Error>denied</Error>", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/status-bucket/")
	body, _ := io.ReadAll(r.Body)
	f.puts[key]++
	f.objects[key] = body
	f.headers[key] = r.Header.Clone()
	w.WriteHeader(http.StatusOK)
}

// fakeWebhook records alert bodies.
type fakeWebhook struct {
	mu     sync.Mutex
	bodies []map[string]any
	status int
}

func (f *fakeWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.bodies = append(f.bodies, body)
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

type statusFixture struct {
	r       *StatusPageReconciler
	kube    client.Client
	lake    *fakeLake
	bucket  *fakeStatusBucket
	webhook *fakeWebhook
	now     time.Time
	mails   []string
}

func newStatusFixture(t *testing.T, mutate func(*oramav1alpha1.StatusPage)) *statusFixture {
	t.Helper()
	f := &statusFixture{
		lake:    &fakeLake{region: "swedencentral"},
		bucket:  &fakeStatusBucket{puts: map[string]int{}, objects: map[string][]byte{}, headers: map[string]http.Header{}},
		webhook: &fakeWebhook{},
		now:     time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}
	lake := httptest.NewServer(f.lake)
	bucket := httptest.NewServer(f.bucket)
	hook := httptest.NewServer(f.webhook)
	t.Cleanup(func() { lake.Close(); bucket.Close(); hook.Close() })

	page := &oramav1alpha1.StatusPage{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-live", Namespace: "orama-system", Generation: 1},
		Spec: oramav1alpha1.StatusPageSpec{
			Title: "lakelive.example status", IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 3,
			Components: []oramav1alpha1.StatusPageComponent{{
				Name: "eu-api", Title: "API", Group: "Europe",
				Checks: []oramav1alpha1.StatusPageCheck{
					{Name: "readyz", URL: lake.URL + "/readyz", Expect: oramav1alpha1.StatusPageExpect{
						JSON: []oramav1alpha1.StatusPageJSONExpect{{Path: "ok", Equals: strPtr("true")}}}},
					{Name: "server-info", URL: lake.URL + "/v1/server_info", Expect: oramav1alpha1.StatusPageExpect{
						JSON: []oramav1alpha1.StatusPageJSONExpect{{Path: "value.region.name", Equals: strPtr("swedencentral")}}}},
					{Name: "workspace-zetta", URL: lake.URL + "/v1/workspace_engine_info?workspace_id=$(workspace_id)",
						SecretRef: "probe-eu", BearerTokenKey: "token"},
				},
			}},
			Publish: &oramav1alpha1.StatusPagePublish{BucketSecretRef: "status-bucket-s3", PublicURL: "https://status.lakelive.example"},
			Alerts: oramav1alpha1.StatusPageAlerts{
				Webhook: &oramav1alpha1.StatusPageWebhookAlert{SecretRef: "alerts-webhook"},
			},
		},
	}
	if mutate != nil {
		mutate(page)
	}
	objects := []client.Object{
		page,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe-eu", Namespace: "orama-system"},
			Data: map[string][]byte{"token": []byte("probe-token"), "workspace_id": []byte("ws-probe")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alerts-webhook", Namespace: "orama-system"},
			Data: map[string][]byte{"url": []byte(hook.URL + "/hook")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "status-bucket-s3", Namespace: "orama-system"},
			Data: map[string][]byte{"accessKeyId": []byte("key-id"), "secretAccessKey": []byte("secret"),
				"endpoint": []byte(bucket.URL), "bucket": []byte("status-bucket"), "region": []byte("auto")}},
	}
	scheme := backupTestScheme(t)
	f.kube = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.StatusPage{}).WithObjects(objects...).Build()
	f.r = &StatusPageReconciler{Client: f.kube, Scheme: scheme, Now: func() time.Time { return f.now }}
	f.r.SendMail = func(_ context.Context, account statusMailAccount, subject, body string) error {
		f.mails = append(f.mails, subject+"\n"+body)
		return nil
	}
	return f
}

// run advances the clock one interval and reconciles once.
func (f *statusFixture) run(t *testing.T) *oramav1alpha1.StatusPage {
	t.Helper()
	f.now = f.now.Add(time.Minute)
	result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "orama-system", Name: "lake-live"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Fatalf("requeue after %v, want one interval", result.RequeueAfter)
	}
	var page oramav1alpha1.StatusPage
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "lake-live"}, &page); err != nil {
		t.Fatal(err)
	}
	return &page
}

func TestStatusPageAllPassingIsOperationalAndPublished(t *testing.T) {
	f := newStatusFixture(t, nil)
	page := f.run(t)
	if page.Status.Phase != statusOperational || page.Status.Operational != "1/1" {
		t.Fatalf("phase %q operational %q, want Operational 1/1: %+v", page.Status.Phase, page.Status.Operational, page.Status.Components)
	}
	if f.lake.tokens.Load() != 1 {
		t.Fatalf("the authenticated check was sent %d times, want 1", f.lake.tokens.Load())
	}
	c := page.Status.Components[0]
	if len(c.History) != 1 || c.History[0] != (oramav1alpha1.StatusPageDay{Date: "2026-10-24", State: statusOperational}) {
		t.Fatalf("history %+v", c.History)
	}
	if cond := apimeta.FindStatusCondition(page.Status.Conditions, "Published"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Published condition %+v", cond)
	}
	if got := f.bucket.headers["index.html"].Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("index.html content type %q", got)
	}
	if got := f.bucket.headers["status.json"].Get("Cache-Control"); got != "no-store, max-age=0" {
		t.Fatalf("status.json cache control %q", got)
	}
	var doc statusJSONDocument
	if err := json.Unmarshal(f.bucket.objects["status.json"], &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Phase != statusOperational || len(doc.Components) != 1 || doc.Components[0].Group != "Europe" || doc.Generated != f.now.Format(time.RFC3339) {
		t.Fatalf("status.json %+v", doc)
	}
	if !strings.Contains(string(f.bucket.objects["index.html"]), "<title>lakelive.example status</title>") {
		t.Fatal("index.html has no title")
	}
	if f.webhook.count() != 0 {
		t.Fatal("the first state of a component is not a change and sends no alert")
	}
}

func TestStatusPageSteadyStateWritesNoStatusButRefreshesTheHeartbeat(t *testing.T) {
	f := newStatusFixture(t, nil)
	first := f.run(t)
	second := f.run(t)
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("a steady page wrote its status: %s -> %s", first.ResourceVersion, second.ResourceVersion)
	}
	if f.bucket.puts["index.html"] != 1 || f.bucket.puts["status.json"] != 2 {
		t.Fatalf("puts %v: index.html once, status.json every run", f.bucket.puts)
	}
}

func TestStatusPageOutageAfterThresholdAlertsOnceAndRecoversOnOnePass(t *testing.T) {
	f := newStatusFixture(t, nil)
	f.run(t)
	f.lake.down.Store(true)
	for i := 1; i <= 2; i++ {
		if page := f.run(t); page.Status.Phase != statusOperational {
			t.Fatalf("run %d below the threshold: phase %s", i, page.Status.Phase)
		}
	}
	page := f.run(t)
	if page.Status.Phase != statusOutage || len(page.Status.Components[0].FailingChecks) != 3 {
		t.Fatalf("after 3 failures: phase %s, %+v", page.Status.Phase, page.Status.Components[0])
	}
	msg := page.Status.Components[0].Message
	if !strings.Contains(msg, "status 503, want 200 (error store_unavailable)") || strings.Contains(msg, "ws-probe") || strings.Contains(msg, "probe-token") {
		t.Fatalf("message %q: must explain the failure and never carry a Secret value", msg)
	}
	if f.webhook.count() != 1 {
		t.Fatalf("%d alerts, want 1", f.webhook.count())
	}
	alert := f.webhook.bodies[0]
	if alert["to"] != statusOutage || alert["from"] != statusOperational || !strings.Contains(alert["text"].(string), "Europe API is Outage") ||
		alert["text"] != alert["content"] || !strings.Contains(alert["text"].(string), "https://status.lakelive.example") {
		t.Fatalf("alert %v", alert)
	}
	f.run(t)
	if f.webhook.count() != 1 {
		t.Fatal("a standing outage alerts again")
	}
	f.lake.down.Store(false)
	page = f.run(t)
	if page.Status.Phase != statusOperational || f.webhook.count() != 2 || f.webhook.bodies[1]["to"] != statusOperational {
		t.Fatalf("recovery: phase %s, alerts %v", page.Status.Phase, f.webhook.bodies)
	}
	if h := page.Status.Components[0].History; len(h) != 1 || h[0].State != statusOutage {
		t.Fatalf("the day's history keeps its worst state: %+v", h)
	}
	var doc statusJSONDocument
	_ = json.Unmarshal(f.bucket.objects["status.json"], &doc)
	raw := string(f.bucket.objects["status.json"])
	if strings.Contains(raw, "503") || strings.Contains(raw, "http") {
		t.Fatalf("status.json is public and must carry no URL or failure detail: %s", raw)
	}
}

func TestStatusPageOneFailingCheckIsDegraded(t *testing.T) {
	f := newStatusFixture(t, func(p *oramav1alpha1.StatusPage) {
		p.Spec.Components[0].Checks[1].Expect.JSON[0].Equals = strPtr("eastus")
		p.Spec.FailureThreshold = 1
	})
	page := f.run(t)
	c := page.Status.Components[0]
	if c.State != statusDegraded || len(c.FailingChecks) != 1 || c.FailingChecks[0] != "server-info" ||
		!strings.Contains(c.Message, `value.region.name = swedencentral, want eastus`) {
		t.Fatalf("component %+v", c)
	}
}

func TestStatusPageMissingSecretIsUncheckedNotFailing(t *testing.T) {
	f := newStatusFixture(t, func(p *oramav1alpha1.StatusPage) {
		p.Spec.Components[0].Checks[2].SecretRef = "probe-missing"
		p.Spec.Components = append(p.Spec.Components, oramav1alpha1.StatusPageComponent{
			Name: "eu-bus", Title: "Agent bus", Group: "Europe",
			Checks: []oramav1alpha1.StatusPageCheck{{Name: "receive", URL: "http://127.0.0.1:1/x?k=$(nokey)", SecretRef: "probe-eu"}},
		})
	})
	page := f.run(t)
	api, bus := page.Status.Components[0], page.Status.Components[1]
	if api.State != statusOperational || len(api.UncheckedChecks) != 1 || api.UncheckedChecks[0] != "workspace-zetta" {
		t.Fatalf("api %+v: a check that cannot run must not fail the component", api)
	}
	if bus.State != statusUnknown || !strings.Contains(bus.Message, "Secret has no key nokey") {
		t.Fatalf("bus %+v", bus)
	}
	if page.Status.Phase != statusUnknown {
		t.Fatalf("phase %s, want Unknown (worse than Operational, better than Degraded)", page.Status.Phase)
	}
	cond := apimeta.FindStatusCondition(page.Status.Conditions, "ChecksConfigured")
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "eu-api/workspace-zetta") {
		t.Fatalf("ChecksConfigured %+v", cond)
	}
}

func TestStatusPageKeepsFailuresAcrossARestart(t *testing.T) {
	f := newStatusFixture(t, nil)
	f.lake.down.Store(true)
	for range 3 {
		f.run(t)
	}
	alerts := f.webhook.count()
	// A new process: the counters start from the written status.
	f.r = &StatusPageReconciler{Client: f.kube, Scheme: f.r.Scheme, Now: f.r.Now, SendMail: f.r.SendMail}
	page := f.run(t)
	if page.Status.Phase != statusOutage || f.webhook.count() != alerts {
		t.Fatalf("after a restart: phase %s, alerts %d (was %d)", page.Status.Phase, f.webhook.count(), alerts)
	}
}

func TestStatusPageRetriesUndeliveredAlerts(t *testing.T) {
	f := newStatusFixture(t, func(p *oramav1alpha1.StatusPage) { p.Spec.FailureThreshold = 1 })
	f.run(t)
	f.webhook.status = http.StatusBadGateway
	f.lake.down.Store(true)
	page := f.run(t)
	cond := apimeta.FindStatusCondition(page.Status.Conditions, "AlertsDelivered")
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "status 502") || strings.Contains(cond.Message, "/hook") {
		t.Fatalf("AlertsDelivered %+v (and the webhook URL must never appear)", cond)
	}
	f.webhook.status = 0
	page = f.run(t)
	if f.webhook.count() != 1 || f.webhook.bodies[0]["to"] != statusOutage {
		t.Fatalf("the undelivered alert was not retried: %v", f.webhook.bodies)
	}
	if cond := apimeta.FindStatusCondition(page.Status.Conditions, "AlertsDelivered"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("AlertsDelivered %+v", cond)
	}
}

func TestStatusPageEmailAlert(t *testing.T) {
	f := newStatusFixture(t, func(p *oramav1alpha1.StatusPage) {
		p.Spec.FailureThreshold = 1
		p.Spec.Alerts = oramav1alpha1.StatusPageAlerts{Email: &oramav1alpha1.StatusPageEmailAlert{SecretRef: "alerts-smtp"}}
	})
	if err := f.kube.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alerts-smtp", Namespace: "orama-system"},
		Data: map[string][]byte{"host": []byte("smtp.example"), "username": []byte("u"), "password": []byte("p"),
			"from": []byte("Lake status <status@lakelive.example>"), "to": []byte("oncall@example.com")}}); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	f.lake.down.Store(true)
	f.run(t)
	if len(f.mails) != 1 || !strings.HasPrefix(f.mails[0], "[lakelive.example status] Europe API: Outage\n") {
		t.Fatalf("mails %q", f.mails)
	}
}

func TestStatusPageWithoutAChannelSaysSo(t *testing.T) {
	f := newStatusFixture(t, func(p *oramav1alpha1.StatusPage) { p.Spec.Alerts = oramav1alpha1.StatusPageAlerts{} })
	page := f.run(t)
	if cond := apimeta.FindStatusCondition(page.Status.Conditions, "AlertsDelivered"); cond == nil || cond.Reason != "NoChannel" {
		t.Fatalf("AlertsDelivered %+v", cond)
	}
}

func TestStatusPagePublishFailureDoesNotStopTheChecks(t *testing.T) {
	f := newStatusFixture(t, nil)
	f.bucket.fail = true
	page := f.run(t)
	if page.Status.Phase != statusOperational {
		t.Fatalf("phase %s", page.Status.Phase)
	}
	if cond := apimeta.FindStatusCondition(page.Status.Conditions, "Published"); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Published %+v", cond)
	}
	f.bucket.fail = false
	f.run(t)
	if f.bucket.puts["index.html"] != 1 {
		t.Fatalf("index.html is written once the bucket accepts it: %v", f.bucket.puts)
	}
}

func TestStatusPageWatchEventsDoNotAddRuns(t *testing.T) {
	f := newStatusFixture(t, nil)
	f.run(t)
	f.now = f.now.Add(10 * time.Second)
	result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "orama-system", Name: "lake-live"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 50*time.Second || f.bucket.puts["status.json"] != 1 {
		t.Fatalf("requeue %v, puts %v: an event between runs must wait out the interval", result.RequeueAfter, f.bucket.puts)
	}
}

func TestRecordStatusDayKeepsNinetyDaysAndTheWorstOfEach(t *testing.T) {
	var history []oramav1alpha1.StatusPageDay
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 100 {
		history = recordStatusDay(history, day.AddDate(0, 0, i), statusOperational)
	}
	if len(history) != statusHistoryDays || history[0].Date != "2026-01-11" {
		t.Fatalf("len %d first %s", len(history), history[0].Date)
	}
	last := day.AddDate(0, 0, 99)
	history = recordStatusDay(history, last, statusOutage)
	history = recordStatusDay(history, last, statusDegraded)
	if history[len(history)-1].State != statusOutage {
		t.Fatalf("today %+v", history[len(history)-1])
	}
}

func TestStatusCheckCertificateValidity(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer server.Close()
	notAfter := server.Certificate().NotAfter
	check := oramav1alpha1.StatusPageCheck{Name: "tls", URL: server.URL, Expect: oramav1alpha1.StatusPageExpect{MinCertificateDays: 14}}
	if result := runStatusCheck(context.Background(), server.Client(), check, nil, 5*time.Second, notAfter.Add(-30*24*time.Hour)); !result.OK {
		t.Fatalf("30 days left: %+v", result)
	}
	result := runStatusCheck(context.Background(), server.Client(), check, nil, 5*time.Second, notAfter.Add(-7*24*time.Hour))
	if result.OK || !strings.Contains(result.Message, "expires in 7 days, want at least 14") {
		t.Fatalf("7 days left: %+v", result)
	}
}

func TestStatusCheckBodyAndPost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || string(body) != `{"workspace_id":"ws-1"}` || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "text/html" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "<title>Lake Console</title>")
	}))
	defer server.Close()
	check := oramav1alpha1.StatusPageCheck{Name: "post", URL: server.URL, Method: http.MethodPost, Body: `{"workspace_id":"$(ws)"}`,
		SecretRef: "s", Headers: map[string]string{"Accept": "text/html"},
		Expect: oramav1alpha1.StatusPageExpect{BodyContains: []string{"<title>Lake Console</title>"}}}
	if result := runStatusCheck(context.Background(), nil, check, map[string][]byte{"ws": []byte("ws-1")}, 5*time.Second, time.Now()); !result.OK {
		t.Fatalf("%+v", result)
	}
	check.Expect.BodyContains = []string{"<title>Other</title>"}
	if result := runStatusCheck(context.Background(), nil, check, map[string][]byte{"ws": []byte("ws-1")}, 5*time.Second, time.Now()); result.OK || !strings.Contains(result.Message, "does not contain") {
		t.Fatalf("%+v", result)
	}
}

func TestStatusMailMessageRefusesHeaderInjection(t *testing.T) {
	account := statusMailAccount{From: "Lake status <status@lakelive.example>", To: []string{"a@example.com", "b@example.com"}}
	msg := string(statusMailMessage(account, "subject\r\nBcc: evil@example.com", "line one\nline two", time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)))
	if strings.Contains(msg, "\r\nBcc:") || !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") || !strings.Contains(msg, "line one\r\nline two") {
		t.Fatalf("message %q", msg)
	}
	if _, err := statusMailAccountFrom(map[string][]byte{"host": []byte("h"), "username": []byte("u"), "password": []byte("p"), "from": []byte("f")}); err == nil {
		t.Fatal("an account without recipients is refused")
	}
}
