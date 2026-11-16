/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
)

// StatusPage: external checks, alerts on a component's change of state, and a static public page
// (docs/infrastructure/status-pages.md). The checks run in the operator that
// holds the resource, so they see a service the way a customer does only when that operator is
// outside the service's own cluster: a hosted Lake's page lives on its managing cluster.
//
// Each run fetches every check at once, counts consecutive failures in memory, and marks a check
// failing at spec.failureThreshold (one pass clears it). The status subresource is written only on
// a change of component state, a new UTC day in the history, or a condition change. status.json is
// written to the bucket on every run: its "generated" time is the page's heartbeat, so a page whose
// prober stopped shows Unknown instead of a stale Operational.

const (
	statusOperational = "Operational"
	statusDegraded    = "Degraded"
	statusOutage      = "Outage"
	statusUnknown     = "Unknown"

	statusHistoryDays = 90
	statusBodyLimit   = 1 << 20
)

var statusSecretRef = regexp.MustCompile(`\$\(([A-Za-z0-9_.-]+)\)`)

// StatusPageReconciler reconciles StatusPage.
type StatusPageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// HTTP sends the checks, the webhook and the bucket writes; nil means a client per call with
	// the check's timeout. Tests point it at fakes.
	HTTP *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// SendMail delivers an email alert; nil means SMTP (sendStatusMail). Tests replace it.
	SendMail func(ctx context.Context, account statusMailAccount, subject, body string) error

	mu    sync.Mutex
	pages map[types.NamespacedName]*statusPageRuntime
}

// statusPageRuntime is what one page's prober keeps between runs, in memory only.
type statusPageRuntime struct {
	generation int64
	lastRun    time.Time
	failures   map[string]int // "<component>/<check>" -> consecutive failed runs
	pending    []statusAlert  // alerts not yet delivered
	indexHash  string         // the last index.html written to the bucket
}

type statusAlert struct {
	Component, Title, Group, From, To, Message string
	At                                         time.Time
}

//+kubebuilder:rbac:groups=orama.cloud,resources=statuspages,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=statuspages/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *StatusPageReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *StatusPageReconciler) runtimeFor(key types.NamespacedName) *statusPageRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pages == nil {
		r.pages = map[types.NamespacedName]*statusPageRuntime{}
	}
	state := r.pages[key]
	if state == nil {
		state = &statusPageRuntime{failures: map[string]int{}}
		r.pages[key] = state
	}
	return state
}

func (r *StatusPageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var page oramav1alpha1.StatusPage
	if err := r.Get(ctx, req.NamespacedName, &page); err != nil {
		if apierrors.IsNotFound(err) {
			r.mu.Lock()
			delete(r.pages, req.NamespacedName)
			r.mu.Unlock()
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !page.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	interval := time.Duration(defaultInt32(page.Spec.IntervalSeconds, 60)) * time.Second
	state := r.runtimeFor(req.NamespacedName)
	now := r.now()
	// A watch event between runs (an edit, a restart's initial list) must not add a run: wait
	// out the interval unless the spec changed.
	if state.generation == page.Generation && !state.lastRun.IsZero() && now.Sub(state.lastRun) < interval-time.Second {
		return ctrl.Result{RequeueAfter: interval - now.Sub(state.lastRun)}, nil
	}
	if state.generation != page.Generation {
		seedStatusFailures(state, &page)
		state.generation = page.Generation
	}
	state.lastRun = now

	results := r.runChecks(ctx, &page)
	desired := page.Status.DeepCopy()
	desired.ObservedGeneration = page.Generation
	alerts := applyStatusResults(&page, desired, state, results, now)
	state.pending = append(state.pending, alerts...)

	unchecked := []string{}
	for _, c := range desired.Components {
		for _, check := range c.UncheckedChecks {
			unchecked = append(unchecked, c.Name+"/"+check)
		}
	}
	if len(unchecked) == 0 {
		setStatusCondition(desired, "ChecksConfigured", metav1.ConditionTrue, "AllChecksRun", "every check ran", page.Generation)
	} else {
		setStatusCondition(desired, "ChecksConfigured", metav1.ConditionFalse, "SecretMissing",
			"not checked, their Secret or key is missing: "+strings.Join(unchecked, ", "), page.Generation)
	}

	// Alerts: deliver everything pending; keep what failed for the next run.
	if len(state.pending) > 0 {
		delivered, err := r.deliverAlerts(ctx, &page, state.pending)
		state.pending = state.pending[delivered:]
		switch {
		case errors.Is(err, errNoAlertChannel):
			setStatusCondition(desired, "AlertsDelivered", metav1.ConditionFalse, "NoChannel", err.Error(), page.Generation)
		case err != nil:
			log.Error(err, "status page alert not delivered", "statusPage", req.NamespacedName)
			setStatusCondition(desired, "AlertsDelivered", metav1.ConditionFalse, "DeliveryFailed", err.Error(), page.Generation)
		default:
			setStatusCondition(desired, "AlertsDelivered", metav1.ConditionTrue, "Delivered", "every change of state was sent", page.Generation)
		}
	} else if apimeta.FindStatusCondition(desired.Conditions, "AlertsDelivered") == nil {
		if page.Spec.Alerts.Webhook == nil && page.Spec.Alerts.Email == nil {
			setStatusCondition(desired, "AlertsDelivered", metav1.ConditionFalse, "NoChannel", errNoAlertChannel.Error(), page.Generation)
		} else {
			setStatusCondition(desired, "AlertsDelivered", metav1.ConditionTrue, "NothingToSend", "no change of state yet", page.Generation)
		}
	}

	// The page: status.json on every run (the heartbeat), index.html when it changes.
	if page.Spec.Publish == nil {
		apimeta.RemoveStatusCondition(&desired.Conditions, "Published")
	} else if err := r.publish(ctx, &page, desired, state, now); err != nil {
		log.Error(err, "status page not published", "statusPage", req.NamespacedName)
		setStatusCondition(desired, "Published", metav1.ConditionFalse, "PublishFailed", err.Error(), page.Generation)
	} else {
		setStatusCondition(desired, "Published", metav1.ConditionTrue, "Published", "index.html and status.json written", page.Generation)
	}

	if !equality.Semantic.DeepEqual(*desired, page.Status) {
		page.Status = *desired
		if err := r.Status().Update(ctx, &page); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// seedStatusFailures starts the counters from the last written status, so a check that was failing
// before a restart or an edit is still failing after it, and its recovery is a real one.
func seedStatusFailures(state *statusPageRuntime, page *oramav1alpha1.StatusPage) {
	threshold := int(defaultInt32(page.Spec.FailureThreshold, 3))
	state.failures = map[string]int{}
	for _, c := range page.Status.Components {
		for _, check := range c.FailingChecks {
			state.failures[c.Name+"/"+check] = threshold
		}
	}
}

// statusCheckResult is one check's answer in one run.
type statusCheckResult struct {
	Component, Check string
	OK, Unchecked    bool
	Message          string
}

func (r *StatusPageReconciler) runChecks(ctx context.Context, page *oramav1alpha1.StatusPage) []statusCheckResult {
	timeout := time.Duration(defaultInt32(page.Spec.TimeoutSeconds, 10)) * time.Second
	secrets := map[string]map[string][]byte{}
	for _, component := range page.Spec.Components {
		for _, check := range component.Checks {
			if check.SecretRef == "" {
				continue
			}
			if _, done := secrets[check.SecretRef]; done {
				continue
			}
			var secret corev1.Secret
			if err := r.Get(ctx, client.ObjectKey{Namespace: page.Namespace, Name: check.SecretRef}, &secret); err != nil {
				secrets[check.SecretRef] = nil
				continue
			}
			secrets[check.SecretRef] = secret.Data
		}
	}
	var results []statusCheckResult
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, component := range page.Spec.Components {
		for _, check := range component.Checks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var data map[string][]byte
				if check.SecretRef != "" {
					data = secrets[check.SecretRef]
					if data == nil {
						mu.Lock()
						results = append(results, statusCheckResult{Component: component.Name, Check: check.Name, Unchecked: true,
							Message: fmt.Sprintf("Secret %s not found", check.SecretRef)})
						mu.Unlock()
						return
					}
				}
				result := runStatusCheck(ctx, r.HTTP, check, data, timeout, r.now())
				result.Component, result.Check = component.Name, check.Name
				mu.Lock()
				results = append(results, result)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	return results
}

// expandStatusRefs replaces $(KEY) with the Secret's value; a missing key is reported.
func expandStatusRefs(text string, data map[string][]byte) (string, error) {
	var missing []string
	out := statusSecretRef.ReplaceAllStringFunc(text, func(ref string) string {
		key := statusSecretRef.FindStringSubmatch(ref)[1]
		value, ok := data[key]
		if !ok {
			missing = append(missing, key)
			return ref
		}
		return string(value)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("Secret has no key %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// runStatusCheck sends one check and judges the answer. Messages name the check's URL as written,
// never with Secret values filled in.
func runStatusCheck(ctx context.Context, httpClient *http.Client, check oramav1alpha1.StatusPageCheck, data map[string][]byte, timeout time.Duration, now time.Time) statusCheckResult {
	unchecked := func(err error) statusCheckResult {
		return statusCheckResult{Unchecked: true, Message: err.Error()}
	}
	fail := func(format string, args ...any) statusCheckResult {
		return statusCheckResult{Message: fmt.Sprintf(format, args...)}
	}
	target, err := expandStatusRefs(check.URL, data)
	if err != nil {
		return unchecked(err)
	}
	body, err := expandStatusRefs(check.Body, data)
	if err != nil {
		return unchecked(err)
	}
	method := check.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != "" && method == http.MethodPost {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return unchecked(fmt.Errorf("invalid request: %s", check.URL))
	}
	req.Header.Set("User-Agent", "orama-operator-status/1")
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range check.Headers {
		expanded, err := expandStatusRefs(value, data)
		if err != nil {
			return unchecked(err)
		}
		req.Header.Set(name, expanded)
	}
	if check.BearerTokenKey != "" {
		token, ok := data[check.BearerTokenKey]
		if !ok || len(token) == 0 {
			return unchecked(fmt.Errorf("Secret %s has no key %s", check.SecretRef, check.BearerTokenKey))
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fail("%s %s: %v", method, check.URL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, statusBodyLimit))
	if err != nil {
		return fail("%s %s: reading the body: %v", method, check.URL, err)
	}
	wantStatus := int(defaultInt32(check.Expect.Status, 200))
	if resp.StatusCode != wantStatus {
		return fail("%s %s: status %d, want %d%s", method, check.URL, resp.StatusCode, wantStatus, statusErrorCode(raw))
	}
	for _, want := range check.Expect.BodyContains {
		if !strings.Contains(string(raw), want) {
			return fail("%s %s: body does not contain %q", method, check.URL, want)
		}
	}
	if len(check.Expect.JSON) > 0 {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fail("%s %s: body is not JSON", method, check.URL)
		}
		for _, want := range check.Expect.JSON {
			value, ok := statusJSONPath(doc, want.Path)
			if !ok || value == nil && want.Equals == nil {
				return fail("%s %s: %s is missing%s", method, check.URL, want.Path, statusErrorCode(raw))
			}
			if want.Equals != nil {
				if got := statusJSONText(value); got != *want.Equals {
					return fail("%s %s: %s = %s, want %s%s", method, check.URL, want.Path, got, *want.Equals, statusErrorCode(raw))
				}
			}
		}
	}
	if days := check.Expect.MinCertificateDays; days > 0 {
		if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
			return fail("%s %s: no TLS certificate", method, check.URL)
		}
		left := resp.TLS.PeerCertificates[0].NotAfter.Sub(now)
		if left < time.Duration(days)*24*time.Hour {
			return fail("%s %s: TLS certificate expires in %d days, want at least %d", method, check.URL, int(left.Hours()/24), days)
		}
	}
	return statusCheckResult{OK: true}
}

// statusErrorCode names lake's error code from a JSON body, when there is one.
func statusErrorCode(raw []byte) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error.Code != "" {
		return " (error " + body.Error.Code + ")"
	}
	return ""
}

func statusJSONPath(doc any, path string) (any, bool) {
	current := doc
	for _, part := range strings.Split(path, ".") {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func statusJSONText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// applyStatusResults folds one run into the desired status and returns the alerts it causes.
func applyStatusResults(page *oramav1alpha1.StatusPage, desired *oramav1alpha1.StatusPageStatus, state *statusPageRuntime, results []statusCheckResult, now time.Time) []statusAlert {
	threshold := int(defaultInt32(page.Spec.FailureThreshold, 3))
	byCheck := map[string]statusCheckResult{}
	for _, result := range results {
		byCheck[result.Component+"/"+result.Check] = result
	}
	previous := map[string]oramav1alpha1.StatusPageComponentStatus{}
	for _, c := range desired.Components {
		previous[c.Name] = c
	}
	var alerts []statusAlert
	components := make([]oramav1alpha1.StatusPageComponentStatus, 0, len(page.Spec.Components))
	operational := 0
	worst := statusOperational
	for _, spec := range page.Spec.Components {
		var failing, unchecked, messages []string
		known := 0
		for _, check := range spec.Checks {
			key := spec.Name + "/" + check.Name
			result := byCheck[key]
			switch {
			case result.Unchecked:
				unchecked = append(unchecked, check.Name)
				messages = append(messages, check.Name+" not checked: "+result.Message)
				continue
			case result.OK:
				state.failures[key] = 0
			default:
				state.failures[key]++
			}
			known++
			if state.failures[key] >= threshold {
				failing = append(failing, check.Name)
				messages = append(messages, check.Name+": "+result.Message)
			}
		}
		next := statusOperational
		switch {
		case known == 0:
			next = statusUnknown
		case len(failing) == known:
			next = statusOutage
		case len(failing) > 0:
			next = statusDegraded
		}
		old, existed := previous[spec.Name]
		current := oramav1alpha1.StatusPageComponentStatus{
			Name: spec.Name, State: next, Since: old.Since, FailingChecks: failing, UncheckedChecks: unchecked,
			Message: truncateStatusMessage(strings.Join(messages, "; ")), History: old.History,
		}
		if !existed || old.State != next || current.Since == nil {
			since := metav1.NewTime(now.Truncate(time.Second))
			current.Since = &since
			if existed && old.State != "" && old.State != next {
				alerts = append(alerts, statusAlert{Component: spec.Name, Title: spec.Title, Group: spec.Group,
					From: old.State, To: next, Message: current.Message, At: now})
			}
		}
		current.History = recordStatusDay(current.History, now, next)
		if next == statusOperational {
			operational++
		}
		worst = worseStatus(worst, next)
		components = append(components, current)
	}
	desired.Components = components
	desired.Phase = worst
	desired.Operational = fmt.Sprintf("%d/%d", operational, len(components))
	return alerts
}

// statusRank orders states for "worst of the day": Unknown ranks below Degraded, since it says
// nothing was seen, not that something failed.
func statusRank(state string) int {
	switch state {
	case statusOutage:
		return 3
	case statusDegraded:
		return 2
	case statusUnknown:
		return 1
	}
	return 0
}

func worseStatus(a, b string) string {
	if statusRank(b) > statusRank(a) {
		return b
	}
	return a
}

// recordStatusDay keeps today's worst state, and the last 90 days.
func recordStatusDay(history []oramav1alpha1.StatusPageDay, now time.Time, state string) []oramav1alpha1.StatusPageDay {
	today := now.Format("2006-01-02")
	history = slices.Clone(history)
	if n := len(history); n > 0 && history[n-1].Date == today {
		history[n-1].State = worseStatus(history[n-1].State, state)
	} else {
		history = append(history, oramav1alpha1.StatusPageDay{Date: today, State: state})
	}
	if len(history) > statusHistoryDays {
		history = history[len(history)-statusHistoryDays:]
	}
	return history
}

func truncateStatusMessage(message string) string {
	const limit = 1024
	if len(message) <= limit {
		return message
	}
	return message[:limit] + "…"
}

func setStatusCondition(status *oramav1alpha1.StatusPageStatus, kind string, value metav1.ConditionStatus, reason, message string, generation int64) {
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: kind, Status: value, Reason: reason, Message: truncateStatusMessage(message), ObservedGeneration: generation,
	})
}

var errNoAlertChannel = errors.New("spec.alerts names no webhook and no email: changes of state are recorded in status only")

// deliverAlerts sends alerts in order through every configured channel and returns how many were
// delivered by all of them.
func (r *StatusPageReconciler) deliverAlerts(ctx context.Context, page *oramav1alpha1.StatusPage, alerts []statusAlert) (int, error) {
	if page.Spec.Alerts.Webhook == nil && page.Spec.Alerts.Email == nil {
		return len(alerts), errNoAlertChannel
	}
	var webhook string
	if ref := page.Spec.Alerts.Webhook; ref != nil {
		data, err := r.secretData(ctx, page.Namespace, ref.SecretRef)
		if err != nil {
			return 0, err
		}
		if webhook = strings.TrimSpace(string(data["url"])); webhook == "" {
			return 0, fmt.Errorf("Secret %s has no key url", ref.SecretRef)
		}
	}
	var mail *statusMailAccount
	if ref := page.Spec.Alerts.Email; ref != nil {
		data, err := r.secretData(ctx, page.Namespace, ref.SecretRef)
		if err != nil {
			return 0, err
		}
		account, err := statusMailAccountFrom(data)
		if err != nil {
			return 0, fmt.Errorf("Secret %s: %w", ref.SecretRef, err)
		}
		mail = &account
	}
	for i, alert := range alerts {
		subject, text := statusAlertText(page, alert)
		if webhook != "" {
			if err := r.postWebhook(ctx, webhook, page, alert, text); err != nil {
				return i, err
			}
		}
		if mail != nil {
			send := r.SendMail
			if send == nil {
				send = sendStatusMail
			}
			if err := send(ctx, *mail, subject, text); err != nil {
				return i, fmt.Errorf("email alert: %w", err)
			}
		}
	}
	return len(alerts), nil
}

func (r *StatusPageReconciler) secretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
		return nil, fmt.Errorf("Secret %s: %w", name, err)
	}
	return secret.Data, nil
}

func statusAlertText(page *oramav1alpha1.StatusPage, alert statusAlert) (string, string) {
	name := alert.Title
	if alert.Group != "" {
		name = alert.Group + " " + alert.Title
	}
	subject := fmt.Sprintf("[%s] %s: %s", page.Spec.Title, name, alert.To)
	var text strings.Builder
	fmt.Fprintf(&text, "%s: %s is %s (was %s) since %s UTC.", page.Spec.Title, name, alert.To, alert.From, alert.At.Format("2006-01-02 15:04"))
	if alert.Message != "" {
		fmt.Fprintf(&text, "\n%s", alert.Message)
	}
	if page.Spec.Publish != nil && page.Spec.Publish.PublicURL != "" {
		fmt.Fprintf(&text, "\nPage: %s", page.Spec.Publish.PublicURL)
	}
	fmt.Fprintf(&text, "\nkubectl get statuspage %s -n %s -o yaml", page.Name, page.Namespace)
	return subject, text.String()
}

func (r *StatusPageReconciler) postWebhook(ctx context.Context, target string, page *oramav1alpha1.StatusPage, alert statusAlert, text string) error {
	payload, err := json.Marshal(map[string]any{
		"text": text, "content": text,
		"statusPage": page.Namespace + "/" + page.Name, "component": alert.Component,
		"from": alert.From, "to": alert.To, "at": alert.At.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(payload)))
	if err != nil {
		return errors.New("webhook alert: the url in the Secret is not a valid URL")
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := r.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err // the URL is a credential; never log it
		}
		return fmt.Errorf("webhook alert: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook alert: status %d", resp.StatusCode)
	}
	return nil
}

// publish writes status.json on every run and index.html when its bytes change.
func (r *StatusPageReconciler) publish(ctx context.Context, page *oramav1alpha1.StatusPage, status *oramav1alpha1.StatusPageStatus, state *statusPageRuntime, now time.Time) error {
	data, err := r.secretData(ctx, page.Namespace, page.Spec.Publish.BucketSecretRef)
	if err != nil {
		return err
	}
	bucket, err := backup.BucketFromSecret(data)
	if err != nil {
		return fmt.Errorf("Secret %s: %w", page.Spec.Publish.BucketSecretRef, err)
	}
	store := &backup.S3{Bucket: bucket, HTTP: r.HTTP, Now: r.Now}
	index := renderStatusIndex(page)
	sum := sha256.Sum256(index)
	hash := hex.EncodeToString(sum[:])
	if hash != state.indexHash {
		if err := store.PutWithHeaders(ctx, "index.html", index, "text/html; charset=utf-8", "public, max-age=300"); err != nil {
			return err
		}
		state.indexHash = hash
	}
	doc, err := renderStatusJSON(page, status, now)
	if err != nil {
		return err
	}
	return store.PutWithHeaders(ctx, "status.json", doc, "application/json", "no-store, max-age=0")
}

func defaultInt32(value, fallback int32) int32 {
	if value <= 0 {
		return fallback
	}
	return value
}

func (r *StatusPageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only spec changes start a run early; the reconciler's own status writes do not.
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.StatusPage{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
