// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// recordedReq is one management-API call the fake broker saw.
type recordedReq struct {
	method string
	path   string
	body   map[string]interface{}
	user   string
	pass   string
}

// fakeBroker is an httptest server standing in for the RabbitMQ management API: it records every
// request and answers 2xx (a stream GET returns 200 so applyTopology marks it present).
func fakeBroker(t *testing.T) (*httptest.Server, *[]recordedReq) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		u, p, _ := r.BasicAuth()
		mu.Lock()
		// EscapedPath keeps %2F (the encoded default vhost); r.URL.Path would decode it back to "/".
		reqs = append(reqs, recordedReq{method: r.Method, path: r.URL.EscapedPath(), body: body, user: u, pass: p})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func rmqTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := oramav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add orama scheme: %v", err)
	}
	return s
}

// TestApplyTopology asserts the reconciler issues the full idempotent PUT sequence (vhost → user →
// permission → stream) against the management API, sources the user password from its Secret, sends
// basic auth, declares the stream as x-queue-type=stream, and returns accurate applied counts.
func TestApplyTopology(t *testing.T) {
	srv, reqs := fakeBroker(t)

	scheme := rmqTestScheme(t)
	pwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "broker-users", Namespace: "ns"},
		Data:       map[string][]byte{"writer-password": []byte("s3cret")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pwSecret).Build()
	r := &RabbitMQTopologyReconciler{Client: c, Scheme: scheme}

	topo := &oramav1alpha1.RabbitMQTopology{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns", Generation: 7},
		Spec: oramav1alpha1.RabbitMQTopologySpec{
			BrokerService:  "cr-broker",
			ManagementPort: 15672,
			AdminSecretRef: "broker-admin",
			VHosts:         []string{"/"},
			Users: []oramav1alpha1.RabbitMQUser{
				{Name: "oramacore-writer", PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: "broker-users", Key: "writer-password"}},
			},
			Permissions: []oramav1alpha1.RabbitMQPermission{
				{User: "oramacore-writer", VHost: "/", Configure: "^oramacore-operations$", Write: "^oramacore-operations$", Read: "^oramacore-operations$"},
			},
			Streams: []oramav1alpha1.RabbitMQStream{{Name: "oramacore-operations", VHost: "/"}},
		},
	}

	st, err := r.applyTopology(context.Background(), srv.URL, "admin", "pw", topo)
	if err != nil {
		t.Fatalf("applyTopology: %v", err)
	}
	if st.VHosts != 1 || st.Users != 1 || st.Permissions != 1 || len(st.Streams) != 1 {
		t.Fatalf("unexpected counts: %+v", st)
	}
	if !st.Streams[0].Present {
		t.Fatalf("stream should be marked present (GET returned 200)")
	}
	if st.ObservedGeneration != 7 {
		t.Fatalf("ObservedGeneration = %d, want 7", st.ObservedGeneration)
	}

	// The default vhost "/" must be percent-encoded to %2F in the path.
	want := []struct{ method, path string }{
		{http.MethodPut, "/api/vhosts/%2F"},
		{http.MethodPut, "/api/users/oramacore-writer"},
		{http.MethodPut, "/api/permissions/%2F/oramacore-writer"},
		{http.MethodPut, "/api/queues/%2F/oramacore-operations"},
		{http.MethodGet, "/api/queues/%2F/oramacore-operations"},
	}
	for _, w := range want {
		if !sawReq(*reqs, w.method, w.path) {
			t.Errorf("missing %s %s in %v", w.method, w.path, reqPaths(*reqs))
		}
	}

	// The user PUT must carry the password sourced from the Secret + basic auth to the broker.
	for _, rq := range *reqs {
		// httptest decodes %2F back to "/", so match on the raw request path.
		if rq.method == http.MethodPut && rq.path == "/api/users/oramacore-writer" {
			if rq.body["password"] != "s3cret" {
				t.Errorf("user PUT password = %v, want s3cret", rq.body["password"])
			}
		}
		if rq.user != "admin" || rq.pass != "pw" {
			t.Errorf("basic auth = %q/%q, want admin/pw", rq.user, rq.pass)
		}
	}

	// The stream must be declared as a stream queue.
	for _, rq := range *reqs {
		if rq.method == http.MethodPut && rq.path == "/api/queues/%2F/oramacore-operations" {
			args, _ := rq.body["arguments"].(map[string]interface{})
			if args["x-queue-type"] != "stream" {
				t.Errorf("stream declare args = %v, want x-queue-type=stream", args)
			}
		}
	}
}

// TestApplyTopologyWaitsForPasswordSecret asserts a user whose password Secret is missing parks the
// topology in a soft wait (errRMQWaiting) rather than erroring.
func TestApplyTopologyWaitsForPasswordSecret(t *testing.T) {
	srv, _ := fakeBroker(t)
	scheme := rmqTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no secret seeded
	r := &RabbitMQTopologyReconciler{Client: c, Scheme: scheme}

	topo := &oramav1alpha1.RabbitMQTopology{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
		Spec: oramav1alpha1.RabbitMQTopologySpec{
			Users: []oramav1alpha1.RabbitMQUser{
				{Name: "oramacore-writer", PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: "missing", Key: "writer-password"}},
			},
		},
	}
	if _, err := r.applyTopology(context.Background(), srv.URL, "admin", "pw", topo); err == nil {
		t.Fatal("expected a wait error when the password secret is missing")
	} else if !isRMQWaiting(err) {
		t.Fatalf("expected errRMQWaiting, got %v", err)
	}
}

func TestRMQBaseURL(t *testing.T) {
	topo := &oramav1alpha1.RabbitMQTopology{ObjectMeta: metav1.ObjectMeta{Namespace: "orama"}}
	topo.Spec.BrokerService = "cr-broker"
	if got := rmqBaseURL(topo); got != "http://cr-broker.orama.svc.cluster.local:15672" {
		t.Errorf("default port URL = %q", got)
	}
	topo.Spec.ManagementPort = 25672
	if got := rmqBaseURL(topo); got != "http://cr-broker.orama.svc.cluster.local:25672" {
		t.Errorf("explicit port URL = %q", got)
	}
}

func TestRMQIsNotFound(t *testing.T) {
	if !rmqIsNotFound(&rmqAPIError{StatusCode: http.StatusNotFound}) {
		t.Error("404 should be not-found")
	}
	if rmqIsNotFound(&rmqAPIError{StatusCode: http.StatusInternalServerError}) {
		t.Error("500 is not not-found")
	}
	if rmqIsNotFound(io.EOF) {
		t.Error("a transport error is not a management-API not-found")
	}
}

// ---- test helpers ----

func isRMQWaiting(err error) bool {
	for err != nil {
		if err == errRMQWaiting {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func sawReq(reqs []recordedReq, method, path string) bool {
	for _, r := range reqs {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

func reqPaths(reqs []recordedReq) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.method+" "+r.path)
	}
	return out
}
