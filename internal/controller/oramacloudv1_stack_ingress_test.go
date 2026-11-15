/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Each OramaCloudV1 stack runs its own ingress-nginx (operator#573, LSR-210 operator#805).

func stackIngressFake(t *testing.T, objs ...client.Object) (*OramaCloudV1Reconciler, client.Client) {
	t.Helper()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &OramaCloudV1Reconciler{Client: cl, Scheme: s}, cl
}

func TestStackIngressClassIsDerivedFromTheStack(t *testing.T) {
	if got := stackIngressClassFor("v1-eu", "v1-eu"); got != "orama-v1-v1-eu-v1-eu" {
		t.Errorf("class = %q", got)
	}
	long := stackIngressClassFor(strings.Repeat("n", 40), strings.Repeat("s", 40))
	if len(long) != 63 || long == stackIngressClassFor(strings.Repeat("n", 40), strings.Repeat("s", 41)) {
		t.Errorf("long class %q (%d) is not cut to 63 with a distinguishing hash", long, len(long))
	}
	oc := publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.IngressClassName = "zetta-tunnel" })
	if ingressClassName(oc) != stackIngressClassName(oc) {
		t.Error("spec.components.ingressClassName still chooses the class")
	}
	if stackIngressControllerValue(oc) != "orama.cloud/"+stackIngressClassName(oc) {
		t.Errorf("controller value = %q", stackIngressControllerValue(oc))
	}
}

// A stack with a host gets its IngressClass, a controller that watches only its namespace and serves
// only its class, a LoadBalancer of its own, and an account bound in its namespace alone.
func TestStackIngressRendersItsOwnController(t *testing.T) {
	ctx := context.Background()
	oc := publicStack(nil)
	r, cl := stackIngressFake(t, oc)
	if err := r.reconcileStackIngress(ctx, oc); err != nil {
		t.Fatal(err)
	}
	class := &networkingv1.IngressClass{}
	if err := cl.Get(ctx, client.ObjectKey{Name: stackIngressClassName(oc)}, class); err != nil {
		t.Fatal(err)
	}
	if class.Spec.Controller != stackIngressControllerValue(oc) || class.Labels[stackIngressStackNamespaceLabel] != "v1-dev" ||
		class.Labels[stackIngressStackNameLabel] != "v1-dev" || class.Annotations["ingressclass.kubernetes.io/is-default-class"] != "" {
		t.Errorf("IngressClass = %+v", class)
	}

	name := client.ObjectKey{Namespace: "v1-dev", Name: "v1-dev-ingress-nginx"}
	dep := &appsv1.Deployment{}
	if err := cl.Get(ctx, name, dep); err != nil {
		t.Fatal(err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	for _, want := range []string{
		"--watch-namespace=$(POD_NAMESPACE)",
		"--ingress-class=" + stackIngressClassName(oc),
		"--controller-class=" + stackIngressControllerValue(oc),
		"--ingress-class-by-name=true",
		"--publish-service=$(POD_NAMESPACE)/v1-dev-ingress-nginx",
		"--configmap=$(POD_NAMESPACE)/v1-dev-ingress-nginx",
		"--http-port=8080", "--https-port=8443",
	} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("controller args %v lack %s", c.Args, want)
		}
	}
	for _, arg := range c.Args {
		if strings.HasPrefix(arg, "--validating-webhook") || strings.HasPrefix(arg, "--default-ssl-certificate") {
			t.Errorf("controller arg %s", arg)
		}
	}
	if c.Image != stackIngressDefaultImage || !strings.Contains(c.Image, "@sha256:") {
		t.Errorf("image = %s", c.Image)
	}
	if sc := c.SecurityContext; sc == nil || len(sc.Capabilities.Add) != 0 || !slices.Contains(sc.Capabilities.Drop, "ALL") ||
		sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || *sc.AllowPrivilegeEscalation {
		t.Errorf("security context = %+v", c.SecurityContext)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != "v1-dev-ingress-nginx" ||
		dep.Spec.Template.Labels["app.kubernetes.io/component"] != stackIngressComponent {
		t.Errorf("pod = %+v", dep.Spec.Template)
	}
	assertV1StorageBounds(t, dep.Spec.Template.Spec)

	svc := &corev1.Service{}
	if err := cl.Get(ctx, name, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal ||
		len(svc.Spec.Ports) != 2 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[1].Port != 443 {
		t.Errorf("Service = %+v", svc.Spec)
	}

	binding := &rbacv1.RoleBinding{}
	if err := cl.Get(ctx, name, binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != stackIngressClusterRole ||
		len(binding.Subjects) != 1 || binding.Subjects[0].Name != "v1-dev-ingress-nginx" || binding.Subjects[0].Namespace != "v1-dev" {
		t.Errorf("RoleBinding = %+v", binding)
	}
	sa := &corev1.ServiceAccount{}
	if err := cl.Get(ctx, name, sa); err != nil || sa.AutomountServiceAccountToken == nil || !*sa.AutomountServiceAccountToken {
		t.Errorf("ServiceAccount = %+v %v", sa, err)
	}
	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, name, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["allow-snippet-annotations"] != "false" || cm.Data["forwarded-for-header"] != "CF-Connecting-IP" {
		t.Errorf("config = %v", cm.Data)
	}

	// Steady: a second pass changes nothing it wrote.
	before := dep.ResourceVersion
	if err := r.reconcileStackIngress(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, name, dep); err != nil || dep.ResourceVersion != before {
		t.Errorf("a steady stack rewrote its controller: %v", err)
	}
}

// Two stacks in two namespaces get two classes, and neither controller serves the other's.
func TestTwoStacksGetTwoClasses(t *testing.T) {
	ctx := context.Background()
	a := publicStack(nil)
	b := publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Name, o.Namespace = "v1-eu", "v1-eu" })
	r, cl := stackIngressFake(t, a, b)
	for _, oc := range []*oramav1alpha1.OramaCloudV1{a, b} {
		if err := r.reconcileStackIngress(ctx, oc); err != nil {
			t.Fatal(err)
		}
	}
	var classes networkingv1.IngressClassList
	if err := cl.List(ctx, &classes); err != nil || len(classes.Items) != 2 {
		t.Fatalf("classes = %d (%v)", len(classes.Items), err)
	}
	if stackIngressClassName(a) == stackIngressClassName(b) {
		t.Fatal("two stacks share a class")
	}
	dep := &appsv1.Deployment{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "v1-eu", Name: "v1-eu-ingress-nginx"}, dep); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, "--ingress-class="+stackIngressClassName(a)) {
		t.Error("v1-eu's controller serves v1-dev's class")
	}
}

// A stack that serves no host runs no controller and keeps no class; a deleted stack's class is
// removed by the not-found path, and one left by a stack deleted while the operator was down by the
// sweep.
func TestStackIngressIsRemovedWithItsStack(t *testing.T) {
	ctx := context.Background()
	oc := publicStack(nil)
	r, cl := stackIngressFake(t, oc)
	if err := r.reconcileStackIngress(ctx, oc); err != nil {
		t.Fatal(err)
	}
	oc.Spec.Components.Dashboard.Enabled = false
	oc.Spec.Components.Engine.Enabled = false
	if err := r.reconcileStackIngress(ctx, oc); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &rbacv1.RoleBinding{}, &corev1.ServiceAccount{}} {
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "v1-dev", Name: "v1-dev-ingress-nginx"}, obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T outlived the last host: %v", obj, err)
		}
	}
	if err := cl.Get(ctx, client.ObjectKey{Name: stackIngressClassName(oc)}, &networkingv1.IngressClass{}); !apierrors.IsNotFound(err) {
		t.Errorf("class outlived the last host: %v", err)
	}

	// The not-found path.
	oc = publicStack(nil)
	r, cl = stackIngressFake(t, oc)
	if err := r.reconcileStackIngress(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKey{Name: stackIngressClassName(oc)}, &networkingv1.IngressClass{}); !apierrors.IsNotFound(err) {
		t.Errorf("class outlived its stack: %v", err)
	}

	// The sweep: an orphan is deleted, a live stack's class and a foreign class are kept.
	live := publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Name, o.Namespace = "v1-eu", "v1-eu" })
	orphan := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: stackIngressClassFor("gone", "gone"), Labels: map[string]string{
		"app.kubernetes.io/managed-by": stackIngressManagedBy, "app.kubernetes.io/component": stackIngressComponent,
		stackIngressStackNamespaceLabel: "gone", stackIngressStackNameLabel: "gone"}}}
	foreign := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx"}}
	r, cl = stackIngressFake(t, live, orphan, foreign)
	if err := r.reconcileStackIngress(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKey{Name: orphan.Name}, &networkingv1.IngressClass{}); !apierrors.IsNotFound(err) {
		t.Errorf("orphan class kept: %v", err)
	}
	for _, name := range []string{"nginx", stackIngressClassName(live)} {
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, &networkingv1.IngressClass{}); err != nil {
			t.Errorf("class %s removed: %v", name, err)
		}
	}
}

// The v1 sandbox admits only the stack's own ingress-nginx to its public backends; the platform's
// ingress-nginx and the zetta-tunnel gateway reach no v1 pod. The controller takes its public ports
// from anywhere and reaches only those backends.
func TestStackSandboxAdmitsOnlyItsOwnIngressNginx(t *testing.T) {
	ctx := context.Background()
	oc := publicStack(nil)
	h := newPublicHarness(t, oc)
	if err := h.r.reconcileNetworkSandbox(ctx, oc); err != nil {
		t.Fatal(err)
	}
	sandbox := &oramav1alpha1.NetworkSandbox{}
	h.get(networkSandboxName, sandbox)
	for _, p := range sandbox.Spec.Policies {
		for _, rule := range p.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector != nil {
					ns := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
					if ns == "ingress-nginx" || ns == zettaTunnelIngressNamespace {
						t.Errorf("%s admits %s", p.Name, ns)
					}
				}
			}
		}
	}
	public := allowanceByName(t, sandbox, "allow-ingress-nginx-public")
	if got := public.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"]; got != stackIngressComponent {
		t.Errorf("public allowance selects %s", got)
	}
	if len(public.Spec.Ingress) != 1 || len(public.Spec.Ingress[0].Ports) != 2 {
		t.Errorf("public allowance = %+v", public.Spec.Ingress)
	}
	egress := allowanceByName(t, sandbox, "allow-ingress-nginx-egress")
	if len(egress.Spec.Egress) != 4 {
		t.Errorf("controller egress = %+v, want the dashboard, docs, writer and reader", egress.Spec.Egress)
	}
	for _, rule := range egress.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil || peer.NamespaceSelector != nil {
				t.Errorf("controller egress leaves the stack: %+v", peer)
			}
		}
	}
	assertJustifiedNarrowAllowances(t, sandbox)
}

// The stack's controller trusts CF-Connecting-IP from exactly the ranges the platform's
// ingress-nginx does (helmfile.yaml), so the client address means the same on both.
func TestStackIngressTrustsTheSameProxiesAsThePlatform(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "helmfile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "proxy-real-ip-cidr:"); ok {
			found = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	if found == "" || found != stackIngressConfig()["proxy-real-ip-cidr"] {
		t.Errorf("helmfile proxy-real-ip-cidr %q, stack controller %q", found, stackIngressConfig()["proxy-real-ip-cidr"])
	}
}

// The chart's ClusterRole for the stack controllers is upstream's scoped Role: it writes only Ingress
// status, its lease and events, and the operator may bind it and no other role for them.
func TestChartStackIngressClusterRoleIsNarrow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "chart", "templates", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "{{") {
			kept = append(kept, line)
		}
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(strings.Join(kept, "\n"))), 4096)
	var role *rbacv1.ClusterRole
	for {
		var doc rbacv1.ClusterRole
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if doc.Kind == "ClusterRole" && doc.Name == stackIngressClusterRole {
			role = &doc
		}
	}
	if role == nil {
		t.Fatalf("chart has no ClusterRole %s", stackIngressClusterRole)
	}
	writes := map[string]bool{"create": true, "update": true, "patch": true, "delete": true, "*": true, "deletecollection": true}
	for _, rule := range role.Rules {
		for _, verb := range rule.Verbs {
			if !writes[verb] {
				continue
			}
			for _, res := range rule.Resources {
				if res != "ingresses/status" && res != "leases" && res != "events" {
					out, _ := yaml.Marshal(rule)
					t.Errorf("stack ingress role writes %s:\n%s", res, out)
				}
			}
		}
		if slices.Contains(rule.Verbs, "*") || slices.Contains(rule.Resources, "*") {
			t.Errorf("wildcard rule %+v", rule)
		}
	}
}
