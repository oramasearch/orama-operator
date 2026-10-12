package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// ingressRoutes returns host → "service:port" for every rule of an Ingress.
func ingressRoutes(ing *networkingv1.Ingress) map[string]string {
	routes := map[string]string{}
	for _, rule := range ing.Spec.Rules {
		svc := rule.HTTP.Paths[0].Backend.Service
		routes[rule.Host] = fmt.Sprintf("%s:%d", svc.Name, svc.Port.Number)
	}
	return routes
}

func TestDashboardIngressExtraHostsAndTLS(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Host = "app.orama.ing"
		o.Spec.Components.Dashboard.ExtraHosts = []string{"app.orama.com"}
	})
	cl, r := reconcileOCV1(t, oc)
	ing := getObj(t, cl, "v1-dashboard", &networkingv1.Ingress{})
	want := map[string]string{"app.orama.ing": "v1-dashboard:3000", "app.orama.com": "v1-dashboard:3000"}
	if got := ingressRoutes(ing); len(ing.Spec.Rules) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	if ing.Spec.TLS != nil {
		t.Errorf("TLS without tlsSecretName = %+v", ing.Spec.TLS)
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.TLSSecretName = "origin-cert" })
	ing = getObj(t, cl, "v1-dashboard", &networkingv1.Ingress{})
	wantTLS := []networkingv1.IngressTLS{{Hosts: []string{"app.orama.ing", "app.orama.com"}, SecretName: "origin-cert"}}
	if !reflect.DeepEqual(ing.Spec.TLS, wantTLS) {
		t.Errorf("TLS = %+v, want %+v", ing.Spec.TLS, wantTLS)
	}

	// Clearing both returns today's single-rule, TLS-less Ingress.
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.TLSSecretName = ""
		o.Spec.Components.Dashboard.ExtraHosts = nil
	})
	ing = getObj(t, cl, "v1-dashboard", &networkingv1.Ingress{})
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "app.orama.ing" || ing.Spec.TLS != nil {
		t.Errorf("cleared Ingress = %+v", ing.Spec)
	}
}

func TestEngineIngressesHonourExtraHostsAndTLS(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		e := &o.Spec.Components.Engine
		e.Writer.Host = "writer.prod2.cluster.orama.ing"
		e.Writer.IngressHostsSpec = oramav1alpha1.IngressHostsSpec{ExtraHosts: []string{"writer.orama.com"}, TLSSecretName: "writer-cert"}
		e.Reader.Host = "collections.orama.ing"
		e.Reader.IngressHostsSpec = oramav1alpha1.IngressHostsSpec{ExtraHosts: []string{"collections.orama.com"}, TLSSecretName: "reader-cert"}
	})
	cl, _ := reconcileOCV1(t, oc)
	for _, c := range []struct {
		name, host, extra, backend, secret string
	}{
		{"v1-writer", "writer.prod2.cluster.orama.ing", "writer.orama.com", "v1-writer:8080", "writer-cert"},
		{"v1-reader", "collections.orama.ing", "collections.orama.com", "v1-reader:80", "reader-cert"},
	} {
		ing := getObj(t, cl, c.name, &networkingv1.Ingress{})
		if got, want := ingressRoutes(ing), map[string]string{c.host: c.backend, c.extra: c.backend}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s routes = %v, want %v", c.name, got, want)
		}
		wantTLS := []networkingv1.IngressTLS{{Hosts: []string{c.host, c.extra}, SecretName: c.secret}}
		if !reflect.DeepEqual(ing.Spec.TLS, wantTLS) {
			t.Errorf("%s TLS = %+v, want %+v", c.name, ing.Spec.TLS, wantTLS)
		}
	}
}

func TestExtraHostsGetNoCloudflareDNSRecord(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		c := &o.Spec.Components
		c.Dashboard.Host, c.Dashboard.ExtraHosts = "app.orama.ing", []string{"app.orama.com"}
		c.Engine.Reader.Host, c.Engine.Reader.ExtraHosts = "collections.orama.ing", []string{"collections.orama.com"}
		c.Engine.Writer.Host, c.Engine.Writer.ExtraHosts = "writer.prod2.cluster.orama.ing", []string{"writer.orama.com"}
		o.Spec.Cloudflare = oramav1alpha1.CloudflareSpec{Enabled: true, ManageDNS: true, APITokenSecretRef: &corev1.LocalObjectReference{Name: "cf"}}
	})
	cl, r := reconcileOCV1(t, oc)
	ctx := context.Background()
	for _, component := range []string{"dashboard", "writer", "reader"} {
		ing := getObj(t, cl, "v1-"+component, &networkingv1.Ingress{})
		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.10"}}
		if err := cl.Status().Update(ctx, ing); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.reconcileCloudflare(ctx, oc); err != nil {
		t.Fatal(err)
	}
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := cl.List(ctx, &records, client.InNamespace(oc.Namespace)); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, rec := range records.Items {
		names[rec.Spec.Name] = true
	}
	want := map[string]bool{"app.orama.ing": true, "collections.orama.ing": true, "writer.prod2.cluster.orama.ing": true}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("DNS record names = %v, want only the hosts %v", names, want)
	}
}

func TestIngressClassNameAppliesToEveryIngress(t *testing.T) {
	for _, c := range []struct{ class, want string }{{"", "nginx"}, {"traefik", "traefik"}} {
		oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.IngressClassName = c.class
			o.Spec.Components.Dashboard.Host = "app.orama.ing"
			o.Spec.Components.Docs.Host = "docs.orama.ing"
			o.Spec.Components.Engine.Writer.Host = "writer.prod2.cluster.orama.ing"
			o.Spec.Components.Engine.Reader.Host = "collections.orama.ing"
		})
		cl, _ := reconcileOCV1(t, oc)
		var ings networkingv1.IngressList
		if err := cl.List(context.Background(), &ings, client.InNamespace(oc.Namespace)); err != nil {
			t.Fatal(err)
		}
		// dashboard, docs, writer and reader, plus the writer's and reader's /metrics blocks.
		if len(ings.Items) != 6 {
			t.Fatalf("class %q: %d Ingresses, want 6", c.class, len(ings.Items))
		}
		for _, ing := range ings.Items {
			if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != c.want {
				t.Errorf("class %q: %s uses %v, want %s", c.class, ing.Name, ing.Spec.IngressClassName, c.want)
			}
		}
	}
}

func TestIngressHostConflictsFailReconcile(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*oramav1alpha1.OramaCloudV1)
		want string
	}{
		{"extra host repeats another component's host", func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Reader.Host = "collections.orama.ing"
			o.Spec.Components.Dashboard.ExtraHosts = []string{"collections.orama.ing"}
		}, `host "collections.orama.ing" is set in both dashboard.extraHosts and engine.reader.host`},
		{"same host on two components", func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Docs.Host = "app.orama.ing"
		}, `host "app.orama.ing" is set in both dashboard.host and docs.host`},
		{"extra host equal to host", func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Dashboard.ExtraHosts = []string{"APP.orama.ing"}
		}, `dashboard.extraHosts repeats dashboard.host "APP.orama.ing"`},
		{"extra hosts without host", func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Writer.ExtraHosts = []string{"writer.orama.com"}
		}, "engine.writer.extraHosts and engine.writer.tlsSecretName require engine.writer.host"},
	} {
		t.Run(c.name, func(t *testing.T) {
			oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Dashboard.Host = "app.orama.ing"
				c.mut(o)
			})
			s := ocv1TestScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc).WithStatusSubresource(oc).Build()
			r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			got := &oramav1alpha1.OramaCloudV1{}
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(oc), got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != "Degraded" || !strings.Contains(statusMessage(got), c.want) {
				t.Errorf("status = %s %q", got.Status.Phase, statusMessage(got))
			}
			if !absent(t, cl, "v1-dashboard", &networkingv1.Ingress{}) {
				t.Error("an Ingress was written before the hosts were validated")
			}
		})
	}
}

// statusMessage is the Ready condition's message.
func statusMessage(oc *oramav1alpha1.OramaCloudV1) string {
	for _, c := range oc.Status.Conditions {
		if c.Type == "Ready" {
			return c.Message
		}
	}
	return ""
}
