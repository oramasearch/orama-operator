// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	publicWriterHost    = "writer-dev.nonprod.example"
	publicReaderHost    = "reader-dev.nonprod.example"
	publicDashboardHost = "app-dev.nonprod.example"
)

// publicStack is the worked example of #385: v1-dev in a primary region with public writer, reader
// and dashboard hosts, Envoy on, and Cloudflare DNS managed with the cloudflare-api-token Secret.
func publicStack(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return originTLSStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Region = oramav1alpha1.RegionSpec{Name: "eu", Role: "primary"}
		o.Spec.Components.Engine = oramav1alpha1.EngineSpec{
			Enabled: true, Envoy: true,
			Writer: oramav1alpha1.WriterSpec{Host: publicWriterHost},
			Reader: oramav1alpha1.ReaderSpec{Host: publicReaderHost},
		}
		o.Spec.Components.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true, Host: publicDashboardHost}}
		o.Spec.Cloudflare.ManageDNS = true
		if mut != nil {
			mut(o)
		}
	})
}

// publicHarness is a fake cluster for the public endpoints. It counts writes, and fails every Get
// of the object named failGet.
type publicHarness struct {
	t       *testing.T
	cl      client.Client
	r       *OramaCloudV1Reconciler
	oc      *oramav1alpha1.OramaCloudV1
	writes  int
	failGet string
}

func newPublicHarness(t *testing.T, oc *oramav1alpha1.OramaCloudV1, objs ...client.Object) *publicHarness {
	t.Helper()
	s := ocv1TestScheme(t)
	h := &publicHarness{t: t, oc: oc}
	count := func() { h.writes++ }
	h.cl = fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&networkingv1.Ingress{}, &oramav1alpha1.CloudflareDNSRecord{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if h.failGet != "" && key.Name == h.failGet {
					return errors.New("injected read failure")
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				count()
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				count()
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				count()
				return c.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				count()
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	h.r = &OramaCloudV1Reconciler{Client: h.cl, Scheme: s}
	return h
}

// publish runs the two steps that own the public endpoints: the Ingresses, then the DNS records.
func (h *publicHarness) publish() error {
	if err := h.r.reconcileEngineIngresses(context.Background(), h.oc); err != nil {
		return err
	}
	return h.r.reconcileCloudflare(context.Background(), h.oc)
}

func (h *publicHarness) mustPublish() {
	h.t.Helper()
	if err := h.publish(); err != nil {
		h.t.Fatal(err)
	}
}

// steady publishes again and fails on any write.
func (h *publicHarness) steady() {
	h.t.Helper()
	before := h.writes
	h.mustPublish()
	if h.writes != before {
		h.t.Fatalf("an unchanged reconcile wrote %d times", h.writes-before)
	}
}

func (h *publicHarness) get(name string, obj client.Object) bool {
	h.t.Helper()
	err := h.cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "v1-dev"}, obj)
	if err != nil && client.IgnoreNotFound(err) != nil {
		h.t.Fatalf("get %s: %v", name, err)
	}
	return err == nil
}

func (h *publicHarness) ingress(component string) (*networkingv1.Ingress, bool) {
	ing := &networkingv1.Ingress{}
	return ing, h.get("v1-dev-"+component, ing)
}

func (h *publicHarness) record(component string) (*oramav1alpha1.CloudflareDNSRecord, bool) {
	rec := &oramav1alpha1.CloudflareDNSRecord{}
	return rec, h.get("v1-dev-"+component+"-dns", rec)
}

func (h *publicHarness) mustRecord(component string) *oramav1alpha1.CloudflareDNSRecord {
	h.t.Helper()
	rec, ok := h.record(component)
	if !ok {
		h.t.Fatalf("no %s DNS record", component)
	}
	return rec
}

// setAddress replaces the load-balancer status of Ingress <cr>-<component>.
func (h *publicHarness) setAddress(component string, lbs ...networkingv1.IngressLoadBalancerIngress) {
	h.t.Helper()
	ing, ok := h.ingress(component)
	if !ok {
		h.t.Fatalf("no %s Ingress", component)
	}
	ing.Status.LoadBalancer.Ingress = lbs
	if err := h.cl.Status().Update(context.Background(), ing); err != nil {
		h.t.Fatal(err)
	}
}

func ip(addr string) networkingv1.IngressLoadBalancerIngress {
	return networkingv1.IngressLoadBalancerIngress{IP: addr}
}

// testChildMark tags a DNS child so a test can tell it from a replacement (the fake client assigns
// no UIDs). The parent patches only the spec, so the mark survives its updates.
const testChildMark = "test.orama.cloud/first-child"

func (h *publicHarness) mark(component string) {
	h.t.Helper()
	rec := h.mustRecord(component)
	rec.Annotations = map[string]string{testChildMark: "true"}
	if err := h.cl.Update(context.Background(), rec); err != nil {
		h.t.Fatal(err)
	}
}

func marked(rec *oramav1alpha1.CloudflareDNSRecord) bool {
	return rec.Annotations[testChildMark] == "true"
}

// provision does what the DNS controller does once Cloudflare has the record: it holds the cleanup
// finalizer and reports the record ID. It also marks the child.
func (h *publicHarness) provision(component, id string) {
	h.t.Helper()
	rec := h.mustRecord(component)
	rec.Finalizers = []string{cfDNSFinalizer}
	rec.Annotations = map[string]string{testChildMark: "true"}
	if err := h.cl.Update(context.Background(), rec); err != nil {
		h.t.Fatal(err)
	}
	rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: id, OwnerComment: cfDNSOwnerMarker(rec), Ready: true, Message: "reconciled"}
	if err := h.cl.Status().Update(context.Background(), rec); err != nil {
		h.t.Fatal(err)
	}
}

// finishCleanup does what the DNS controller's finalizer does once the Cloudflare record is gone.
func (h *publicHarness) finishCleanup(component string) {
	h.t.Helper()
	rec := h.mustRecord(component)
	if rec.DeletionTimestamp.IsZero() {
		h.t.Fatalf("%s DNS record is not being deleted", component)
	}
	rec.Finalizers = nil
	if err := h.cl.Update(context.Background(), rec); err != nil {
		h.t.Fatal(err)
	}
	if _, ok := h.record(component); ok {
		h.t.Fatalf("%s DNS record remains after its finalizer", component)
	}
}

func (h *publicHarness) update(mut func(*oramav1alpha1.OramaCloudV1)) {
	mut(h.oc)
}

func ingressBackend(ing *networkingv1.Ingress) (string, int32) {
	svc := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	return svc.Name, svc.Port.Number
}

// The worked example: both Ingresses with origin TLS, then a proxied record per endpoint pointing at
// its own Ingress's address, and nothing written on a steady reconcile.
func TestPublicEngineEndpointsPublishIngressesAndDNS(t *testing.T) {
	h := newPublicHarness(t, publicStack(nil))
	h.mustPublish()
	for component, want := range map[string]struct {
		host string
		port int32
	}{"writer": {publicWriterHost, oramacorePort}, "reader": {publicReaderHost, envoyPort}} {
		ing, ok := h.ingress(component)
		if !ok {
			t.Fatalf("no %s Ingress", component)
		}
		if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != want.host || ing.Spec.Rules[0].HTTP.Paths[0].Path != "/" {
			t.Errorf("%s rules = %+v, want / on %s", component, ing.Spec.Rules, want.host)
		}
		if svc, port := ingressBackend(ing); svc != "v1-dev-"+component || port != want.port {
			t.Errorf("%s backend = %s:%d, want v1-dev-%s:%d", component, svc, port, component, want.port)
		}
		assertIngressTLS(t, ing, "v1-dev-letsencrypt")
		if !metav1.IsControlledBy(ing, h.oc) {
			t.Errorf("%s Ingress is not controlled by the CR", component)
		}
		if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != defaultIngressClassName {
			t.Errorf("%s class = %v, want the stack's default", component, ing.Spec.IngressClassName)
		}
		if _, ok := h.record(component); ok {
			t.Errorf("%s record exists before its Ingress has an address", component)
		}
	}

	h.setAddress("writer", ip("192.0.2.10"))
	h.setAddress("reader", ip("192.0.2.11"))
	h.mustPublish()
	for component, want := range map[string]oramav1alpha1.CloudflareDNSRecordSpec{
		"writer": {SecretRef: "cloudflare-api-token", Name: publicWriterHost, Content: "192.0.2.10", Proxied: true},
		"reader": {SecretRef: "cloudflare-api-token", Name: publicReaderHost, Content: "192.0.2.11", Proxied: true},
	} {
		rec := h.mustRecord(component)
		if rec.Spec != want {
			t.Errorf("%s record spec = %+v, want %+v", component, rec.Spec, want)
		}
		if !metav1.IsControlledBy(rec, h.oc) {
			t.Errorf("%s record is not controlled by the CR", component)
		}
	}
	h.steady()
}

func TestPublicEngineDNSFollowsOnlyItsOwnIngressAddress(t *testing.T) {
	ctx := context.Background()
	h := newPublicHarness(t, publicStack(nil))
	h.mustPublish()

	// The reader's address publishes the reader only; the writer waits for its own.
	h.setAddress("reader", ip("192.0.2.11"))
	h.mustPublish()
	if _, ok := h.record("writer"); ok {
		t.Fatal("the writer record was created from the reader's address")
	}

	// Hostname wins over IP within an entry, and an empty entry is skipped.
	h.setAddress("writer", networkingv1.IngressLoadBalancerIngress{}, networkingv1.IngressLoadBalancerIngress{IP: "192.0.2.10", Hostname: "lb.example.net"})
	h.mustPublish()
	if got := h.mustRecord("writer").Spec.Content; got != "lb.example.net" {
		t.Errorf("writer content = %q, want the Ingress hostname", got)
	}
	if got := h.mustRecord("reader").Spec.Content; got != "192.0.2.11" {
		t.Errorf("reader content = %q, want its own address", got)
	}

	// A temporary empty address leaves an existing record as it is.
	h.setAddress("reader")
	h.steady()
	if got := h.mustRecord("reader").Spec.Content; got != "192.0.2.11" {
		t.Errorf("reader content after the address went away = %q", got)
	}

	// A deleting Ingress is no target.
	stack := publicStack(nil)
	d := newPublicHarness(t, stack)
	d.mustPublish()
	d.setAddress("writer", ip("192.0.2.10"))
	ing, _ := d.ingress("writer")
	ing.Finalizers = []string{"example.com/hold"}
	if err := d.cl.Update(ctx, ing); err != nil {
		t.Fatal(err)
	}
	if err := d.cl.Delete(ctx, ing); err != nil {
		t.Fatal(err)
	}
	if err := d.r.reconcileCloudflare(ctx, stack); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.record("writer"); ok {
		t.Error("a record was created for a deleting Ingress")
	}

	// A failed Ingress read is an error, not an absent Ingress.
	h.failGet = "v1-dev-reader"
	if err := h.r.reconcileCloudflare(ctx, h.oc); err == nil || !strings.Contains(err.Error(), "injected read failure") {
		t.Errorf("err = %v, want the read failure returned", err)
	}
}

// A read region runs no writer, so it publishes none; its readers stay published.
func TestPublicEngineEndpointsReadRegion(t *testing.T) {
	h := newPublicHarness(t, publicStack(nil))
	h.mustPublish()
	h.setAddress("writer", ip("192.0.2.10"))
	h.setAddress("reader", ip("192.0.2.11"))
	h.mustPublish()
	h.provision("writer", "writer-record")

	h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Region.Role = "read" })
	h.mustPublish()
	if _, ok := h.ingress("writer"); ok {
		t.Error("a read region kept the writer Ingress")
	}
	h.finishCleanup("writer")
	h.mustPublish()
	if _, ok := h.record("writer"); ok {
		t.Error("a read region recreated the writer record")
	}
	if _, ok := h.ingress("reader"); !ok {
		t.Error("a read region dropped the reader Ingress")
	}
	if _, ok := h.record("reader"); !ok {
		t.Error("a read region dropped the reader record")
	}

	// A fresh read region never publishes its writer.
	fresh := newPublicHarness(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Region.Role = "read" }))
	fresh.mustPublish()
	if _, ok := fresh.ingress("writer"); ok {
		t.Error("a fresh read region rendered a writer Ingress")
	}
}

// An existing CR gets no engine Ingress or record until its hosts are set.
func TestPublicEngineEndpointsNeedAHost(t *testing.T) {
	h := newPublicHarness(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Host, o.Spec.Components.Engine.Reader.Host = "", ""
	}))
	h.mustPublish()
	for _, list := range []client.ObjectList{&networkingv1.IngressList{}, &oramav1alpha1.CloudflareDNSRecordList{}} {
		if err := h.cl.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		if n := meta.LenList(list); n != 0 {
			t.Errorf("%T has %d items without engine hosts", list, n)
		}
	}
}

// Before Cloudflare has the record, a host or Secret edit updates the same child. After, the child is
// deleted with its spec intact, and the replacement waits for its finalizer.
func TestPublicEngineDNSHostAndSecretEdits(t *testing.T) {
	for name, edit := range map[string]func(*oramav1alpha1.OramaCloudV1){
		"host": func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Reader.Host = "search-dev.nonprod.example"
		},
		"secret": func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Cloudflare.APITokenSecretRef.Name = "cloudflare-api-token-2"
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPublicHarness(t, publicStack(nil))
			h.mustPublish()
			h.setAddress("reader", ip("192.0.2.11"))
			h.mustPublish()
			h.mark("reader")

			h.update(edit)
			h.mustPublish()
			edited := h.mustRecord("reader")
			if !marked(edited) {
				t.Fatal("an edit before provisioning replaced the child")
			}
			wantName, wantSecret := h.oc.Spec.Components.Engine.Reader.Host, h.oc.Spec.Cloudflare.APITokenSecretRef.Name
			if edited.Spec.Name != wantName || edited.Spec.SecretRef != wantSecret {
				t.Fatalf("edited spec = %+v", edited.Spec)
			}

			// Provisioned: the next edit is a replacement, back to the first values.
			h.provision("reader", "reader-record")
			provisioned := h.mustRecord("reader")
			h.oc = publicStack(nil)
			h.setAddress("reader") // the replacement must not depend on an address
			h.mustPublish()
			old := h.mustRecord("reader")
			if !marked(old) || old.DeletionTimestamp.IsZero() {
				t.Fatalf("the provisioned child was not marked for deletion: %+v", old.ObjectMeta)
			}
			if old.Spec != provisioned.Spec || old.Status != provisioned.Status {
				t.Fatalf("the deleting child changed: spec %+v status %+v", old.Spec, old.Status)
			}
			h.steady()
			if again := h.mustRecord("reader"); again.Spec != provisioned.Spec || again.Status.RecordID != "reader-record" {
				t.Fatalf("a reconcile during finalization changed the child: %+v", again)
			}

			h.finishCleanup("reader")
			h.mustPublish()
			if _, ok := h.record("reader"); ok {
				t.Fatal("a replacement was created without an Ingress address")
			}
			h.setAddress("reader", ip("192.0.2.11"))
			h.mustPublish()
			fresh := h.mustRecord("reader")
			if marked(fresh) || fresh.Status.RecordID != "" {
				t.Fatalf("the replacement is not a fresh child: %+v", fresh)
			}
			if fresh.Spec.Name != publicReaderHost || fresh.Spec.SecretRef != "cloudflare-api-token" {
				t.Fatalf("replacement spec = %+v", fresh.Spec)
			}
		})
	}
}

// Re-enabling an endpoint while its child is finalizing waits for the deletion to finish.
func TestPublicEngineDNSReenabledDuringFinalization(t *testing.T) {
	h := newPublicHarness(t, publicStack(nil))
	h.mustPublish()
	h.setAddress("reader", ip("192.0.2.11"))
	h.mustPublish()
	h.provision("reader", "reader-record")
	provisioned := h.mustRecord("reader")

	h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Host = "" })
	h.mustPublish()
	h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Host = publicReaderHost })
	h.mustPublish()
	h.setAddress("reader", ip("192.0.2.11"))
	h.mustPublish()
	deleting := h.mustRecord("reader")
	if !marked(deleting) || deleting.DeletionTimestamp.IsZero() || deleting.Spec != provisioned.Spec ||
		deleting.Status != provisioned.Status || len(deleting.Finalizers) != 1 {
		t.Fatalf("the finalizing child was touched: %+v", deleting)
	}
	h.finishCleanup("reader")
	h.mustPublish()
	if fresh := h.mustRecord("reader"); marked(fresh) || fresh.Spec.Name != publicReaderHost {
		t.Fatalf("no fresh child after finalization: %+v", fresh)
	}
}

// A new address updates only the child's content; its identity, finalizer and status stay. A change
// of the Secret's zone is the DNS controller's to handle, so the parent leaves the child alone.
func TestPublicEngineDNSAddressChangeKeepsTheChild(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-api-token", Namespace: "v1-dev"},
		Data: map[string][]byte{"token": []byte("t"), "zoneID": []byte("zone-1")}}
	h := newPublicHarness(t, publicStack(nil), secret)
	h.mustPublish()
	h.setAddress("writer", ip("192.0.2.10"))
	h.mustPublish()
	h.provision("writer", "writer-record")
	before := h.mustRecord("writer")

	h.setAddress("writer", ip("192.0.2.20"))
	h.mustPublish()
	after := h.mustRecord("writer")
	if !marked(after) || after.Status != before.Status || len(after.Finalizers) != 1 {
		t.Fatalf("an address change replaced the child or its status: %+v", after)
	}
	if after.Spec.Content != "192.0.2.20" {
		t.Fatalf("content = %q, want the new address", after.Spec.Content)
	}

	secret.Data["zoneID"] = []byte("zone-2")
	if err := h.cl.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	h.steady()
	if again := h.mustRecord("writer"); !marked(again) || !again.DeletionTimestamp.IsZero() {
		t.Fatal("a zone change made the parent replace the child")
	}
}

func TestPublicEngineEndpointsCleanup(t *testing.T) {
	ctx := context.Background()
	published := func(t *testing.T, mut func(*oramav1alpha1.OramaCloudV1)) *publicHarness {
		h := newPublicHarness(t, publicStack(mut))
		h.mustPublish()
		if err := h.r.upsertIngress(ctx, h.oc, "v1-dev-dashboard", "dashboard", ingressSpec(h.oc, "v1-dev-dashboard", publicDashboardHost, nil, 3000), ""); err != nil {
			t.Fatal(err)
		}
		h.setAddress("dashboard", ip("192.0.2.9"))
		h.setAddress("writer", ip("192.0.2.10"))
		h.setAddress("reader", ip("192.0.2.11"))
		h.mustPublish()
		for _, c := range []string{"dashboard", "writer", "reader"} {
			h.mustRecord(c)
		}
		return h
	}

	t.Run("cleared hosts", func(t *testing.T) {
		h := published(t, nil)
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.Host = "" })
		h.mustPublish()
		if _, ok := h.ingress("writer"); ok {
			t.Error("the writer Ingress outlived its host")
		}
		if _, ok := h.record("writer"); ok {
			t.Error("the writer record outlived its host")
		}
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Host = "" })
		h.mustPublish()
		if _, ok := h.ingress("reader"); ok {
			t.Error("the reader Ingress outlived its host")
		}
		if _, ok := h.record("reader"); ok {
			t.Error("the reader record outlived its host")
		}
		h.mustRecord("dashboard")
	})

	t.Run("engine disabled", func(t *testing.T) {
		h := published(t, nil)
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false })
		if err := h.r.reconcileEngine(ctx, h.oc); err != nil {
			t.Fatal(err)
		}
		if err := h.r.reconcileCloudflare(ctx, h.oc); err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{"writer", "reader"} {
			if _, ok := h.ingress(c); ok {
				t.Errorf("the %s Ingress outlived the engine", c)
			}
			if _, ok := h.record(c); ok {
				t.Errorf("the %s record outlived the engine", c)
			}
		}
	})

	for name, off := range map[string]func(*oramav1alpha1.OramaCloudV1){
		"manageDNS off":    func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Cloudflare.ManageDNS = false },
		"cloudflare off":   func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Cloudflare.Enabled = false },
		"token removed":    func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Cloudflare.APITokenSecretRef = nil },
		"token name empty": func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Cloudflare.APITokenSecretRef.Name = "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := published(t, nil)
			h.update(off)
			if err := h.r.reconcileCloudflare(ctx, h.oc); err != nil {
				t.Fatal(err)
			}
			for _, c := range []string{"dashboard", "writer", "reader"} {
				if _, ok := h.record(c); ok {
					t.Errorf("the %s record outlived %s", c, name)
				}
			}
		})
	}
}

// Objects at the endpoint names that this CR does not control are never adopted, changed or
// deleted, and publishing over one reports the conflict.
func TestPublicEngineEndpointsLeaveForeignObjects(t *testing.T) {
	ctx := context.Background()
	foreignIngress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "v1-dev-writer", Namespace: "v1-dev",
		Annotations: map[string]string{"team": "someone-else"}}}
	foreignRecord := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: "v1-dev-reader-dns", Namespace: "v1-dev"},
		Spec: oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "theirs", Name: "theirs.nonprod.example", Content: "198.51.100.1", Proxied: true}}
	h := newPublicHarness(t, publicStack(nil), foreignIngress, foreignRecord)
	unchanged := func() {
		t.Helper()
		ing, _ := h.ingress("writer")
		rec := h.mustRecord("reader")
		if len(ing.OwnerReferences) != 0 || ing.Annotations["team"] != "someone-else" || len(ing.Spec.Rules) != 0 || !ing.DeletionTimestamp.IsZero() {
			t.Fatalf("the foreign Ingress changed: %+v", ing)
		}
		if len(rec.OwnerReferences) != 0 || rec.Spec != foreignRecord.Spec || !rec.DeletionTimestamp.IsZero() {
			t.Fatalf("the foreign record changed: %+v", rec)
		}
	}

	if err := h.r.reconcileEngineIngresses(ctx, h.oc); err == nil || !strings.Contains(err.Error(), "Ingress v1-dev/v1-dev-writer") {
		t.Fatalf("err = %v, want a conflict naming the Ingress", err)
	}
	unchanged()

	// The writer goes internal, so the reader publishes and its record conflicts.
	h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.Host = "" })
	if err := h.r.reconcileEngineIngresses(ctx, h.oc); err != nil {
		t.Fatal(err)
	}
	h.setAddress("reader", ip("192.0.2.11"))
	if err := h.publish(); err == nil || !strings.Contains(err.Error(), "CloudflareDNSRecord v1-dev/v1-dev-reader-dns") {
		t.Fatalf("err = %v, want a conflict naming the record", err)
	}
	unchanged()

	// Cleanup leaves both alone.
	h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false })
	h.mustPublish()
	unchanged()
}

// The reader Ingress always targets the reader Service's port 80; Envoy decides only the pod port.
func TestPublicReaderIngressBackendIgnoresEnvoy(t *testing.T) {
	for _, envoy := range []bool{true, false} {
		h := newPublicHarness(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Envoy = envoy }))
		h.mustPublish()
		ing, _ := h.ingress("reader")
		if svc, port := ingressBackend(ing); svc != "v1-dev-reader" || port != envoyPort {
			t.Errorf("envoy=%v: reader backend = %s:%d, want v1-dev-reader:80", envoy, svc, port)
		}
	}
}

// The sandbox admits ingress-nginx alone, only to this stack's published writer and reader pods, on
// the port behind each Ingress backend.
func TestPublicEngineEndpointsSandboxRules(t *testing.T) {
	ctx := context.Background()
	policies := func(t *testing.T, oc *oramav1alpha1.OramaCloudV1) map[string]oramav1alpha1.NetworkSandboxPolicy {
		t.Helper()
		h := newPublicHarness(t, oc)
		if err := h.r.reconcileNetworkSandbox(ctx, oc); err != nil {
			t.Fatal(err)
		}
		sandbox := &oramav1alpha1.NetworkSandbox{}
		if !h.get(networkSandboxName, sandbox) {
			t.Fatal("no NetworkSandbox")
		}
		out := map[string]oramav1alpha1.NetworkSandboxPolicy{}
		for _, p := range sandbox.Spec.Policies {
			out[p.Name] = p
		}
		return out
	}
	assertIngressNginxRule := func(t *testing.T, p oramav1alpha1.NetworkSandboxPolicy, component string, port int32) {
		t.Helper()
		if got := p.Spec.PodSelector.MatchLabels; got["app.kubernetes.io/component"] != component || got["app.kubernetes.io/instance"] != "v1-dev" {
			t.Errorf("%s selects %v, want this stack's %s pods", p.Name, got, component)
		}
		if len(p.Spec.Egress) != 0 || len(p.Spec.Ingress) != 1 {
			t.Fatalf("%s rules = %+v, want one ingress rule", p.Name, p.Spec)
		}
		rule := p.Spec.Ingress[0]
		if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != port {
			t.Errorf("%s ports = %+v, want TCP %d", p.Name, rule.Ports, port)
		}
		var namespaces []string
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil || peer.PodSelector == nil || peer.IPBlock != nil {
				t.Errorf("%s peer %+v is not a namespaced pod peer", p.Name, peer)
				continue
			}
			namespaces = append(namespaces, peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
		}
		// ingress-nginx alone: the zetta-tunnel gateway never serves Cloud v1.
		if strings.Join(namespaces, ",") != "ingress-nginx" {
			t.Errorf("%s admits namespaces %v, want ingress-nginx only (no zetta-tunnel gateway peer)", p.Name, namespaces)
		}
	}

	got := policies(t, publicStack(nil))
	assertIngressNginxRule(t, got["allow-writer-ingress-nginx"], "writer", oramacorePort)
	assertIngressNginxRule(t, got["allow-reader-ingress-nginx"], "reader", envoyPort)
	for _, internal := range []string{"allow-writer-ingress", "allow-reader-ingress", "allow-engine-egress"} {
		if _, ok := got[internal]; !ok {
			t.Errorf("internal allowance %s is gone", internal)
		}
	}

	got = policies(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Envoy = false }))
	assertIngressNginxRule(t, got["allow-reader-ingress-nginx"], "reader", oramacorePort)

	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){
		"no hosts": func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Writer.Host, o.Spec.Components.Engine.Reader.Host = "", ""
		},
		"engine disabled": func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false },
	} {
		got := policies(t, publicStack(mut))
		for _, p := range []string{"allow-writer-ingress-nginx", "allow-reader-ingress-nginx"} {
			if _, ok := got[p]; ok {
				t.Errorf("%s: %s remains", name, p)
			}
		}
	}
	got = policies(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Region.Role = "read" }))
	if _, ok := got["allow-writer-ingress-nginx"]; ok {
		t.Error("a read region admits the gateway to a writer")
	}
	if _, ok := got["allow-reader-ingress-nginx"]; !ok {
		t.Error("a read region stopped admitting the gateway to its readers")
	}

	// Removing a host from a running stack removes its rule from the existing sandbox.
	oc := publicStack(nil)
	h := newPublicHarness(t, oc)
	if err := h.r.reconcileNetworkSandbox(ctx, oc); err != nil {
		t.Fatal(err)
	}
	oc.Spec.Components.Engine.Writer.Host = ""
	if err := h.r.reconcileNetworkSandbox(ctx, oc); err != nil {
		t.Fatal(err)
	}
	sandbox := &oramav1alpha1.NetworkSandbox{}
	h.get(networkSandboxName, sandbox)
	for _, p := range sandbox.Spec.Policies {
		if p.Name == "allow-writer-ingress-nginx" {
			t.Error("the writer rule outlived its host")
		}
	}
}

// With writer JWT verification configured, a public writer host must be an exact audience.
func TestPublicWriterHostJWTGuard(t *testing.T) {
	const want = "https://" + publicWriterHost
	withHost := func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.Host = publicWriterHost }
	for name, tc := range map[string]struct {
		audiences []string
		ok        bool
	}{
		"exact":          {[]string{want}, true},
		"among others":   {[]string{"https://writer.cloudv1.example", want}, true},
		"trailing slash": {[]string{want + "/"}, false},
		"http":           {[]string{"http://" + publicWriterHost}, false},
		"other host":     {[]string{"https://writer.cloudv1.example"}, false},
	} {
		err := validateWriterJWT(jwtOC(tc.audiences, withHost))
		if tc.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), `"`+want+`"`)) {
			t.Errorf("%s: err = %v, want the expected URL named", name, err)
		}
	}
	// No JWT block: no audience requirement.
	if err := validateWriterJWT(engineOC(withHost)); err != nil {
		t.Errorf("nil JWT with a writer host: %v", err)
	}

	// Through the controller: Ready=False/ReconcileError, and no writer Ingress.
	oc := jwtOC([]string{want + "/"}, withHost)
	oc.Spec.Cloudflare.APITokenSecretRef = &corev1.LocalObjectReference{Name: "cloudflare-api-token"}
	h := newEngineHarness(t, oc)
	if err := h.run(); err == nil {
		t.Fatal("Reconcile accepted a writer host missing from the audiences")
	}
	ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReconcileError" ||
		!strings.HasPrefix(ready.Message, "engine: ") || !strings.Contains(ready.Message, want) {
		t.Fatalf("Ready = %+v, want False/ReconcileError naming %s", ready, want)
	}
	if h.get("v1-writer", &networkingv1.Ingress{}) {
		t.Error("a writer Ingress was published with the misconfigured audience")
	}
}
