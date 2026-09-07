/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// rmqTopoFinalizer removes the declared users + streams from the broker before the CR is released,
// so tearing down the owning OramaCloudV1 cascades to real broker cleanup. (The default "/" vhost is
// never deleted — that would break the whole broker.)
const rmqTopoFinalizer = "orama.cloud/rabbitmq-topology-cleanup"

// rmqTopoRequeue is the steady-state resync — long enough that a settled topology is quiet, short
// enough to re-assert the topology if something drifts it out-of-band. Status is written ONLY on a
// real change (anti-flap, principle #7), so a steady topology produces zero control-plane traffic.
const rmqTopoRequeue = 5 * time.Minute

// rmqTopoWaitRequeue is the retry cadence while the broker (or its admin/user Secret) is not yet
// ready — ordered readiness gating: the topology settles as soon as the broker becomes reachable,
// without crash-looping in the meantime.
const rmqTopoWaitRequeue = 20 * time.Second

// errRMQWaiting is returned when a referenced Secret (admin creds or a user password) is not present
// yet — a soft "wait", not a reconcile failure.
var errRMQWaiting = errors.New("waiting for a referenced secret")

// RabbitMQTopologyReconciler reconciles the broker's declared topology (vhosts / users /
// permissions / streams) against the RabbitMQ management HTTP API, and reflects applied state in
// status. It is the operator's OWN topology controller (principle #3 / #41) — same "external
// resource as an owned CRD" shape as CloudflareDNSRecord: idempotent upserts on reconcile,
// delete-from-broker on finalize.
type RabbitMQTopologyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=rabbitmqtopologies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=rabbitmqtopologies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=rabbitmqtopologies/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *RabbitMQTopologyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var topo oramav1alpha1.RabbitMQTopology
	if err := r.Get(ctx, req.NamespacedName, &topo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	adminUser, adminPass, err := r.adminCreds(ctx, topo.Namespace, topo.Spec.AdminSecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	base := rmqBaseURL(&topo)

	// Deletion: run the finalizer (delete users + streams from the broker), then release.
	if !topo.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&topo, rmqTopoFinalizer) {
			if adminUser != "" && adminPass != "" {
				// Best-effort external cleanup — a broker already gone (its StatefulSet torn down with
				// the owning CR) must not wedge the finalizer, so transport errors are tolerated.
				if err := r.teardown(ctx, base, adminUser, adminPass, &topo); err != nil {
					logf.FromContext(ctx).Error(err, "rabbitmq topology teardown failed (continuing to release finalizer)", "rabbitmqtopology", topo.Name)
				}
			}
			controllerutil.RemoveFinalizer(&topo, rmqTopoFinalizer)
			if err := r.Update(ctx, &topo); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Register the finalizer before creating any external state.
	if !controllerutil.ContainsFinalizer(&topo, rmqTopoFinalizer) {
		controllerutil.AddFinalizer(&topo, rmqTopoFinalizer)
		if err := r.Update(ctx, &topo); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Safe no-op until the broker admin Secret is present. Surface it as a clear condition rather than
	// an error so the loop stays green and keeps waiting (ordered gating).
	if adminUser == "" || adminPass == "" {
		r.setNotReady(ctx, &topo, "WaitingForCredentials",
			"waiting for the broker admin Secret ("+topo.Spec.AdminSecretRef+") with username+password")
		return ctrl.Result{RequeueAfter: rmqTopoWaitRequeue}, nil
	}

	desired, err := r.applyTopology(ctx, base, adminUser, adminPass, &topo)
	if err != nil {
		// Waiting on a referenced Secret (a user password not provisioned yet) → soft wait.
		if errors.Is(err, errRMQWaiting) {
			r.setNotReady(ctx, &topo, "WaitingForCredentials", err.Error())
			return ctrl.Result{RequeueAfter: rmqTopoWaitRequeue}, nil
		}
		// Broker not reachable yet (transport error, not an API error) → soft wait (ordered gating):
		// the topology reconciles as soon as the broker's management API comes up.
		var apiErr *rmqAPIError
		if !errors.As(err, &apiErr) {
			r.setNotReady(ctx, &topo, "BrokerUnavailable",
				"broker management API not reachable yet at "+base+": "+err.Error())
			return ctrl.Result{RequeueAfter: rmqTopoWaitRequeue}, nil
		}
		// A real management-API error (e.g. a stream declared with conflicting properties). Surface it
		// and back off — this degrades only THIS CR, never the main stack loop.
		r.setNotReady(ctx, &topo, "ReconcileError", err.Error())
		return ctrl.Result{}, err
	}

	r.setReady(ctx, &topo, desired)
	return ctrl.Result{RequeueAfter: rmqTopoRequeue}, nil
}

func (r *RabbitMQTopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.RabbitMQTopology{}).Complete(r)
}

// applyTopology makes the broker topology idempotent via the management API: ensure each vhost,
// user (password from its Secret), permission grant, and stream — in that order (users before the
// permissions that reference them, vhosts before the resources inside them). Returns the computed
// status so the caller can apply it atomically under the anti-flap guard.
func (r *RabbitMQTopologyReconciler) applyTopology(ctx context.Context, base, adminUser, adminPass string, topo *oramav1alpha1.RabbitMQTopology) (oramav1alpha1.RabbitMQTopologyStatus, error) {
	st := oramav1alpha1.RabbitMQTopologyStatus{ObservedGeneration: topo.Generation}

	// 1) vhosts (idempotent PUT; "/" already exists but re-asserting is harmless).
	for _, vh := range topo.Spec.VHosts {
		if err := rmqDo(ctx, base, adminUser, adminPass, http.MethodPut, "/api/vhosts/"+pesc(vh), nil, nil); err != nil {
			return st, err
		}
		st.VHosts++
	}

	// 2) users — password sourced from the referenced Secret (never from spec). Least-privilege
	//    service accounts stay untagged.
	for _, u := range topo.Spec.Users {
		pw, err := r.secretValue(ctx, topo.Namespace, u.PasswordSecretRef)
		if err != nil {
			return st, err
		}
		body := map[string]interface{}{"password": pw, "tags": u.Tags}
		if err := rmqDo(ctx, base, adminUser, adminPass, http.MethodPut, "/api/users/"+pesc(u.Name), body, nil); err != nil {
			return st, err
		}
		st.Users++
	}

	// 3) permissions — scope each user's configure/write/read on a vhost (least-privilege).
	for _, p := range topo.Spec.Permissions {
		vh := orDefault(p.VHost, "/")
		body := map[string]interface{}{
			"configure": orDefault(p.Configure, "^$"),
			"write":     orDefault(p.Write, "^$"),
			"read":      orDefault(p.Read, "^$"),
		}
		if err := rmqDo(ctx, base, adminUser, adminPass, http.MethodPut, "/api/permissions/"+pesc(vh)+"/"+pesc(p.User), body, nil); err != nil {
			return st, err
		}
		st.Permissions++
	}

	// 4) streams — declared as queues with x-queue-type=stream. Retention args are only applied at
	//    declaration (RabbitMQ cannot change them later via policy); declare them matching any native
	//    stream-protocol client that also creates the stream, or the client's Create will conflict.
	for _, s := range topo.Spec.Streams {
		vh := orDefault(s.VHost, "/")
		args := map[string]interface{}{"x-queue-type": "stream"}
		if s.MaxAge != "" {
			args["x-max-age"] = s.MaxAge
		}
		if s.MaxLengthBytes > 0 {
			args["x-max-length-bytes"] = s.MaxLengthBytes
		}
		if s.MaxSegmentSizeBytes > 0 {
			args["x-stream-max-segment-size-bytes"] = s.MaxSegmentSizeBytes
		}
		if s.LeaderLocator != "" {
			args["x-queue-leader-locator"] = s.LeaderLocator
		}
		body := map[string]interface{}{"durable": true, "auto_delete": false, "arguments": args}
		path := "/api/queues/" + pesc(vh) + "/" + pesc(s.Name)
		if err := rmqDo(ctx, base, adminUser, adminPass, http.MethodPut, path, body, nil); err != nil {
			return st, err
		}
		st.Streams = append(st.Streams, oramav1alpha1.RabbitMQStreamStatus{Name: s.Name, Present: rmqExists(ctx, base, adminUser, adminPass, path)})
	}
	return st, nil
}

// teardown deletes the declared streams then users from the broker (404s tolerated). vhosts are left
// alone — deleting "/" would break the broker, and a non-default vhost may hold state we don't own.
func (r *RabbitMQTopologyReconciler) teardown(ctx context.Context, base, adminUser, adminPass string, topo *oramav1alpha1.RabbitMQTopology) error {
	for _, s := range topo.Spec.Streams {
		vh := orDefault(s.VHost, "/")
		if err := rmqDo(ctx, base, adminUser, adminPass, http.MethodDelete, "/api/queues/"+pesc(vh)+"/"+pesc(s.Name), nil, nil); err != nil && !rmqIsNotFound(err) {
			return err
		}
	}
	for _, u := range topo.Spec.Users {
		if err := rmqDo(ctx, base, adminUser, adminPass, http.MethodDelete, "/api/users/"+pesc(u.Name), nil, nil); err != nil && !rmqIsNotFound(err) {
			return err
		}
	}
	return nil
}

// ---- credential + secret helpers ----

// adminCreds reads the broker admin username+password from the referenced Secret (nil-safe: a
// missing Secret is a no-op, not an error, so the topology waits until it's configured).
func (r *RabbitMQTopologyReconciler) adminCreds(ctx context.Context, ns, name string) (user, pass string, err error) {
	if name == "" {
		return "", "", nil
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return string(sec.Data["username"]), string(sec.Data["password"]), nil
}

// secretValue reads a single key from a Secret. A missing Secret/key is a soft wait (errRMQWaiting),
// so a user whose password Secret has not been provisioned yet parks the topology in
// WaitingForCredentials instead of failing.
func (r *RabbitMQTopologyReconciler) secretValue(ctx context.Context, ns string, ref oramav1alpha1.RabbitMQSecretKeyRef) (string, error) {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: secret %s/%s", errRMQWaiting, ns, ref.Name)
		}
		return "", err
	}
	v, ok := sec.Data[ref.Key]
	if !ok || len(v) == 0 {
		return "", fmt.Errorf("%w: secret %s/%s key %q", errRMQWaiting, ns, ref.Name, ref.Key)
	}
	return string(v), nil
}

// ---- status helpers (anti-flap: write the subresource ONLY on a real transition) ----

func (r *RabbitMQTopologyReconciler) setReady(ctx context.Context, topo *oramav1alpha1.RabbitMQTopology, desired oramav1alpha1.RabbitMQTopologyStatus) {
	desired.Message = fmt.Sprintf("topology reconciled (%d vhost(s), %d user(s), %d permission(s), %d stream(s))",
		desired.VHosts, desired.Users, desired.Permissions, len(desired.Streams))
	r.applyStatus(ctx, topo, desired, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: desired.Message,
	})
}

func (r *RabbitMQTopologyReconciler) setNotReady(ctx context.Context, topo *oramav1alpha1.RabbitMQTopology, reason, msg string) {
	// Preserve any applied counts/streams already discovered; only the condition + message move.
	desired := oramav1alpha1.RabbitMQTopologyStatus{
		ObservedGeneration: topo.Generation,
		VHosts:             topo.Status.VHosts,
		Users:              topo.Status.Users,
		Permissions:        topo.Status.Permissions,
		Streams:            topo.Status.Streams,
		Message:            msg,
	}
	r.applyStatus(ctx, topo, desired, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: msg,
	})
}

// applyStatus deep-compares the computed status (minus conditions) and merges the condition; it
// calls Status().Update ONLY when something actually changed, so a steady topology is silent.
func (r *RabbitMQTopologyReconciler) applyStatus(ctx context.Context, topo *oramav1alpha1.RabbitMQTopology, desired oramav1alpha1.RabbitMQTopologyStatus, cond metav1.Condition) {
	cond.ObservedGeneration = topo.Generation
	changed := topo.Status.VHosts != desired.VHosts ||
		topo.Status.Users != desired.Users ||
		topo.Status.Permissions != desired.Permissions ||
		topo.Status.ObservedGeneration != desired.ObservedGeneration ||
		topo.Status.Message != desired.Message ||
		!reflect.DeepEqual(topo.Status.Streams, desired.Streams)
	// Carry existing conditions forward so SetStatusCondition can detect a real change.
	desired.Conditions = topo.Status.Conditions
	if apimeta.SetStatusCondition(&desired.Conditions, cond) {
		changed = true
	}
	if !changed {
		return
	}
	topo.Status = desired
	if err := r.Status().Update(ctx, topo); err != nil {
		logf.FromContext(ctx).Error(err, "status update failed", "rabbitmqtopology", topo.Name)
	}
}

// ---- RabbitMQ management HTTP API ----

// rmqBaseURL is the broker management API base (http://<service>.<ns>.svc.cluster.local:<port>).
func rmqBaseURL(topo *oramav1alpha1.RabbitMQTopology) string {
	port := topo.Spec.ManagementPort
	if port == 0 {
		port = 15672
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", topo.Spec.BrokerService, topo.Namespace, port)
}

// pesc percent-encodes a single URL path segment (url.PathEscape escapes "/", so the default vhost
// "/" becomes "%2F" as the management API requires).
func pesc(seg string) string { return url.PathEscape(seg) }

// rmqAPIError is a non-2xx management-API response. It carries the status + body so callers can
// distinguish cases (e.g. a 404 on delete) via errors.As, while transport errors (broker not up)
// surface as plain errors that are NOT *rmqAPIError.
type rmqAPIError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *rmqAPIError) Error() string {
	return fmt.Sprintf("rabbitmq API %s %s: %d %s", e.Method, e.URL, e.StatusCode, e.Body)
}

// rmqIsNotFound reports whether err is a 404 from the management API (tolerated on delete).
func rmqIsNotFound(err error) bool {
	var apiErr *rmqAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// rmqDo performs one management-API call with HTTP basic auth. A non-2xx response becomes an
// *rmqAPIError; a transport failure (broker unreachable) is returned as-is.
func rmqDo(ctx context.Context, base, user, pass, method, path string, body, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &rmqAPIError{Method: method, URL: base + path, StatusCode: resp.StatusCode, Body: string(data)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// rmqExists reports whether a GET on path returns 2xx (used to confirm a stream is present for
// status). Best-effort: any error (including transport) reads as "not present".
func rmqExists(ctx context.Context, base, user, pass, path string) bool {
	return rmqDo(ctx, base, user, pass, http.MethodGet, path, nil, nil) == nil
}
