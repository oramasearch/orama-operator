/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// dashOC is a CR whose only enabled component is the dashboard, referencing testMasterSecret
// (newEngineHarness creates it) so the dashboard is not skipped for a missing master key (#307).
func dashOC(mut func(*oramav1alpha1.DashboardSpec)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
		o.Spec.Components.Dashboard.Enabled = true
		if mut != nil {
			mut(&o.Spec.Components.Dashboard)
		}
	})
}

func configMapOf(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orama-system"}, Data: data}
}

func secretSource(name string) corev1.EnvFromSource {
	return corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}
}

func configMapSource(name string) corev1.EnvFromSource {
	return corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}
}

func configMapKeyEnv(env, name, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}}
}

func optional() *bool { t := true; return &t }

// dashboard is the stored dashboard Deployment, or nil when there is none.
func (h *engineHarness) dashboard() *appsv1.Deployment {
	h.t.Helper()
	dep := &appsv1.Deployment{}
	if !h.get("v1-dashboard", dep) {
		return nil
	}
	return dep
}

func (h *engineHarness) dashboardHash() string {
	h.t.Helper()
	dep := h.dashboard()
	if dep == nil {
		h.t.Fatal("dashboard Deployment v1-dashboard not found")
	}
	return dep.Spec.Template.Annotations[dashboardEnvHashAnnotation]
}

func (h *engineHarness) put(obj client.Object) {
	h.t.Helper()
	if err := h.cl.Update(context.Background(), obj); apierrors.IsNotFound(err) {
		obj.SetResourceVersion("")
		err = h.cl.Create(context.Background(), obj)
	} else if err != nil {
		h.t.Fatal(err)
	}
}

// effectiveEnv resolves the dashboard container's environment the way the kubelet does: envFrom in
// order (a later source wins, prefix applied), then env in order. A missing object is skipped.
func (h *engineHarness) effectiveEnv(c corev1.Container) map[string]string {
	h.t.Helper()
	read := func(kind, name string) map[string]string {
		data, _, err := h.r.readEnvObject(context.Background(), "orama-system", kind, name)
		if err != nil {
			h.t.Fatal(err)
		}
		return data
	}
	out := map[string]string{}
	for _, src := range c.EnvFrom {
		kind, name, _ := envFromRef(src)
		for k, v := range read(kind, name) {
			out[src.Prefix+k] = v
		}
	}
	for _, e := range c.Env {
		if kind, name, key, _, ok := envKeyRef(e); ok {
			out[e.Name] = read(kind, name)[key]
		} else if e.ValueFrom == nil {
			out[e.Name] = e.Value
		}
	}
	return out
}

// operatorDashboardEnvFrom and operatorDashboardEnv are the dashboard container's operator-owned
// sources and variables (the master key from #307, the control DB), unchanged by #311.
var operatorDashboardEnvFrom = []corev1.EnvFromSource{configMapSource("v1-config"), secretSource("v1-dashboard-secret"), secretSource("v1-jwt")}

func operatorDashboardEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	return []corev1.EnvVar{masterAPIKeyEnv(oc), pgPasswordEnv(oc, "POSTGRES_PASSWORD"), pgURIEnv(oc, "DATABASE_URL")}
}

// The issue's worked example: the user's sources follow the operator's, the explicit selector wins
// over every source, and without it the later source wins.
func TestDashboardEnvWorkedExample(t *testing.T) {
	oc := dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("dashboard-extra"), secretSource("dashboard-later")}
		d.Env = []corev1.EnvVar{secretKeyEnv("STRIPE_SECRET_KEY", "stripe-test", "api-key")}
	})
	h := newEngineHarness(t, oc,
		secretOf("v1-dashboard-secret", map[string]string{"STRIPE_SECRET_KEY": "seeded"}),
		secretOf("dashboard-extra", map[string]string{"STRIPE_SECRET_KEY": "test-first"}),
		secretOf("dashboard-later", map[string]string{"STRIPE_SECRET_KEY": "test-last"}),
		secretOf("stripe-test", map[string]string{"api-key": "test-selected"}))
	h.mustRun()

	c := h.dashboard().Spec.Template.Spec.Containers[0]
	wantFrom := append(slices.Clone(operatorDashboardEnvFrom), secretSource("dashboard-extra"), secretSource("dashboard-later"))
	if !reflect.DeepEqual(c.EnvFrom, wantFrom) {
		t.Fatalf("envFrom = %+v, want %+v", c.EnvFrom, wantFrom)
	}
	wantEnv := append(operatorDashboardEnv(oc), secretKeyEnv("STRIPE_SECRET_KEY", "stripe-test", "api-key"))
	if !reflect.DeepEqual(c.Env, wantEnv) {
		t.Fatalf("env = %+v, want %+v", c.Env, wantEnv)
	}
	if got := h.effectiveEnv(c)["STRIPE_SECRET_KEY"]; got != "test-selected" {
		t.Errorf("effective STRIPE_SECRET_KEY = %q, want the selector's", got)
	}
	hash := h.dashboardHash()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		t.Errorf("hash annotation = %q, want 64 lowercase hex characters", hash)
	}

	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Env = nil })
	h.mustRun()
	c = h.dashboard().Spec.Template.Spec.Containers[0]
	if !reflect.DeepEqual(c.Env, operatorDashboardEnv(oc)) {
		t.Errorf("env without the explicit entry = %+v, want the operator's only", c.Env)
	}
	if got := h.effectiveEnv(c)["STRIPE_SECRET_KEY"]; got != "test-last" {
		t.Errorf("effective STRIPE_SECRET_KEY = %q, want the later source's", got)
	}
}

func slicesClone[T any](s []T) []T { return append([]T(nil), s...) }

func TestOverrideEnv(t *testing.T) {
	lit := func(n, v string) corev1.EnvVar { return corev1.EnvVar{Name: n, Value: v} }
	base := []corev1.EnvVar{lit("A", "op"), lit("STRIPE_SECRET_KEY", "op"), lit("B", "op")}
	cases := map[string]struct {
		base, extra, want []corev1.EnvVar
	}{
		"no extra":          {base, nil, base},
		"empty extra":       {base, []corev1.EnvVar{}, base},
		"append":            {base, []corev1.EnvVar{lit("C", "u")}, append(slices.Clone(base), lit("C", "u"))},
		"replace operator":  {base, []corev1.EnvVar{lit("STRIPE_SECRET_KEY", "u")}, []corev1.EnvVar{lit("A", "op"), lit("B", "op"), lit("STRIPE_SECRET_KEY", "u")}},
		"last user wins":    {base, []corev1.EnvVar{lit("X", "1"), lit("Y", "1"), lit("X", "2")}, append(slices.Clone(base), lit("Y", "1"), lit("X", "2"))},
		"repeat over base":  {base, []corev1.EnvVar{lit("STRIPE_SECRET_KEY", "1"), lit("C", "1"), lit("STRIPE_SECRET_KEY", "2")}, []corev1.EnvVar{lit("A", "op"), lit("B", "op"), lit("C", "1"), lit("STRIPE_SECRET_KEY", "2")}},
		"selector replaces": {base, []corev1.EnvVar{secretKeyEnv("STRIPE_SECRET_KEY", "s", "k")}, []corev1.EnvVar{lit("A", "op"), lit("B", "op"), secretKeyEnv("STRIPE_SECRET_KEY", "s", "k")}},
		"nil base":          {nil, []corev1.EnvVar{lit("X", "1"), lit("X", "2")}, []corev1.EnvVar{lit("X", "2")}},
	}
	for name, c := range cases {
		before := slices.Clone(c.base)
		if got := overrideEnv(c.base, c.extra); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: overrideEnv = %+v, want %+v", name, got, c.want)
		}
		if !reflect.DeepEqual(c.base, before) {
			t.Errorf("%s: overrideEnv changed its base", name)
		}
	}
}

// Every refused variable, invalid reference and missing reference fails the actual reconcile with
// Ready=False naming the field and the object or variable, never a value; on a running stack the
// dashboard Deployment keeps its last valid template, and on a fresh one it is not created.
func TestDashboardEnvInvalidFailsReconcile(t *testing.T) {
	const value = "do-not-print-0001"
	type tc struct {
		dash    func(*oramav1alpha1.DashboardSpec)
		objects []client.Object
		want    string
	}
	cases := map[string]tc{
		"env DATABASE_URL": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{{Name: "DATABASE_URL", Value: "postgres://" + value}}
		}, want: "dashboard.env[0] sets DATABASE_URL, which the operator owns"},
		"env JWT_PRIVATE_KEY from a Secret": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{{Name: "OK", Value: "x"}, secretKeyEnv("JWT_PRIVATE_KEY", "keys", "pk")}
		}, objects: []client.Object{secretOf("keys", map[string]string{"pk": value})},
			want: "dashboard.env[1] sets JWT_PRIVATE_KEY, which the operator owns"},
		"refused name shadowed by a later entry": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{{Name: "MASTER_API_KEY", Value: value}, {Name: "MASTER_API_KEY", Value: value}}
		}, want: "dashboard.env[0] sets MASTER_API_KEY"},
		"envFrom Secret with MASTER_API_KEY": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{secretSource("extra")}
		}, objects: []client.Object{secretOf("extra", map[string]string{"STRIPE_SECRET_KEY": "s", "MASTER_API_KEY": value})},
			want: "dashboard.envFrom[0] (Secret extra) sets MASTER_API_KEY, which the operator owns"},
		"prefix makes JWT_PRIVATE_KEY, optional or not": {dash: func(d *oramav1alpha1.DashboardSpec) {
			src := secretSource("jwtish")
			src.Prefix, src.SecretRef.Optional = "JWT_", optional()
			d.EnvFrom = []corev1.EnvFromSource{src}
		}, objects: []client.Object{secretOf("jwtish", map[string]string{"PRIVATE_KEY": value})},
			want: "dashboard.envFrom[0] (Secret jwtish) sets JWT_PRIVATE_KEY"},
		"refused key shadowed by a later source": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{configMapSource("first"), secretSource("later")}
		}, objects: []client.Object{configMapOf("first", map[string]string{"POSTGRES_PASSWORD": value}),
			secretOf("later", map[string]string{"POSTGRES_PASSWORD": value})},
			want: "dashboard.envFrom[0] (ConfigMap first) sets POSTGRES_PASSWORD"},
		"both refs": {dash: func(d *oramav1alpha1.DashboardSpec) {
			src := secretSource("a")
			src.ConfigMapRef = configMapSource("b").ConfigMapRef
			d.EnvFrom = []corev1.EnvFromSource{secretSource("ok"), src}
		}, objects: []client.Object{secretOf("ok", nil)}, want: "dashboard.envFrom[1] must set exactly one of secretRef and configMapRef"},
		"no ref": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{{Prefix: "X_"}}
		}, want: "dashboard.envFrom[0] must set exactly one of secretRef and configMapRef"},
		"empty name": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{configMapSource("")}
		}, want: "dashboard.envFrom[0] needs a name"},
		"selector without a key": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{configMapKeyEnv("STRIPE_SECRET_KEY", "cm", "")}
		}, want: "dashboard.env[0] (STRIPE_SECRET_KEY): the ConfigMap key reference needs a name and a key"},
		"missing Secret": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{secretSource("absent")}
		}, want: "dashboard.envFrom[0]: Secret orama-system/absent does not exist"},
		"missing ConfigMap": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{configMapSource("absent")}
		}, want: "dashboard.envFrom[0]: ConfigMap orama-system/absent does not exist"},
		"missing selected Secret": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{secretKeyEnv("STRIPE_SECRET_KEY", "absent", "k")}
		}, want: "dashboard.env STRIPE_SECRET_KEY: Secret orama-system/absent does not exist"},
		"missing selected key": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{configMapKeyEnv("SLACK_CHANNEL_STRIPE", "cm", "channel")}
		}, objects: []client.Object{configMapOf("cm", map[string]string{"other": value})},
			want: "dashboard.env SLACK_CHANNEL_STRIPE: ConfigMap orama-system/cm has no key channel"},
		"binaryData is not a key": {dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{configMapKeyEnv("BLOB", "bin", "blob")}
		}, objects: []client.Object{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "bin", Namespace: "orama-system"},
			BinaryData: map[string][]byte{"blob": []byte(value)}}},
			want: "dashboard.env BLOB: ConfigMap orama-system/bin has no key blob"},
	}
	for _, name := range dashboardOwnedNames() {
		cases["env "+name] = tc{dash: func(d *oramav1alpha1.DashboardSpec) {
			d.Env = []corev1.EnvVar{{Name: name, Value: value}}
		}, want: "dashboard.env[0] sets " + name + ", which the operator owns"}
		cases["envFrom ConfigMap "+name] = tc{dash: func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{configMapSource("owned")}
		}, objects: []client.Object{configMapOf("owned", map[string]string{name: value})},
			want: "dashboard.envFrom[0] (ConfigMap owned) sets " + name}
	}
	for name, c := range cases {
		for _, existing := range []bool{false, true} {
			label := name + " on a fresh stack"
			if existing {
				label = name + " on a running stack"
			}
			t.Run(label, func(t *testing.T) {
				h := newEngineHarness(t, append([]client.Object{dashOC(nil)}, c.objects...)...)
				var dep *appsv1.Deployment
				if existing {
					h.mustRun()
					dep = h.dashboard()
				}
				h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { c.dash(&o.Spec.Components.Dashboard) })
				err := h.run()
				if err == nil {
					t.Fatal("Reconcile accepted the invalid dashboard env")
				}
				ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
				if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReconcileError" ||
					!strings.HasPrefix(ready.Message, "dashboard: ") || !strings.Contains(ready.Message, c.want) {
					t.Fatalf("Ready = %+v, want False/ReconcileError containing %q", ready, c.want)
				}
				if strings.Contains(ready.Message, value) || strings.Contains(err.Error(), value) {
					t.Error("the error holds a referenced value")
				}
				got := h.dashboard()
				switch {
				case !existing && got != nil:
					t.Error("a refused reconcile created the dashboard Deployment")
				case existing && got.ResourceVersion != dep.ResourceVersion:
					t.Error("a refused reconcile changed the dashboard Deployment")
				}
			})
		}
	}
}

func dashboardOwnedNames() []string {
	var names []string
	for n := range dashboardOwnedEnv {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// optional permits a missing object or key, not a read error.
func TestDashboardEnvOptionalDoesNotMaskAPIErrors(t *testing.T) {
	for name, dash := range map[string]func(*oramav1alpha1.DashboardSpec){
		"envFrom": func(d *oramav1alpha1.DashboardSpec) {
			src := secretSource("guarded")
			src.SecretRef.Optional = optional()
			d.EnvFrom = []corev1.EnvFromSource{src}
		},
		"selector": func(d *oramav1alpha1.DashboardSpec) {
			e := secretKeyEnv("STRIPE_SECRET_KEY", "guarded", "k")
			e.ValueFrom.SecretKeyRef.Optional = optional()
			d.Env = []corev1.EnvVar{e}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := ocv1TestScheme(t)
			h := &engineHarness{t: t}
			h.cl = fake.NewClientBuilder().WithScheme(s).
				WithStatusSubresource(&oramav1alpha1.OramaCloudV1{}).
				WithObjects(dashOC(dash)).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if key.Name == "guarded" {
							return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, nil)
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()
			h.r = &OramaCloudV1Reconciler{Client: h.cl, Scheme: s}
			if err := h.run(); err == nil || !apierrors.IsForbidden(err) {
				t.Fatalf("Reconcile = %v, want the Forbidden error", err)
			}
			ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
			if ready == nil || ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, "Secret orama-system/guarded") {
				t.Fatalf("Ready = %+v, want False naming the Secret", ready)
			}
		})
	}
}

// The hash follows the referenced data and nothing else, and holds no value.
func TestDashboardEnvHash(t *testing.T) {
	cm := func(data map[string]string) *corev1.ConfigMap { return configMapOf("cm", data) }
	oc := dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("extra"), configMapSource("cm")}
		d.Env = []corev1.EnvVar{secretKeyEnv("STRIPE_SECRET_KEY", "stripe", "api-key"), configMapKeyEnv("S3_BUCKET", "buckets", "main")}
	})
	values := []string{"extra-value-0001", "cm-value-0002", "stripe-value-0003", "bucket-value-0004", "unselected-value-0005"}
	h := newEngineHarness(t, oc,
		secretOf("extra", map[string]string{"RESEND_API_KEY": values[0]}),
		cm(map[string]string{"SLACK_CHANNEL_STRIPE": values[1]}),
		secretOf("stripe", map[string]string{"api-key": values[2], "unused": values[4]}),
		configMapOf("buckets", map[string]string{"main": values[3], "unused": values[4]}))
	h.mustRun()
	hash := h.dashboardHash()
	rv := h.dashboard().ResourceVersion

	h.mustRun()
	if h.dashboard().ResourceVersion != rv {
		t.Fatal("a repeated reconcile changed the dashboard Deployment")
	}

	type step struct {
		name   string
		change func()
		rolls  bool
	}
	steps := []step{
		{"label on the envFrom Secret", func() {
			s := secretOf("extra", map[string]string{"RESEND_API_KEY": values[0]})
			s.Labels = map[string]string{"x": "y"}
			h.put(s)
		}, false},
		{"unselected key in a selector-only Secret", func() {
			h.put(secretOf("stripe", map[string]string{"api-key": values[2], "unused": "changed"}))
		}, false},
		{"unselected key in a selector-only ConfigMap", func() {
			h.put(configMapOf("buckets", map[string]string{"main": values[3], "unused": "changed"}))
		}, false},
		{"binaryData on an envFrom ConfigMap", func() {
			c := cm(map[string]string{"SLACK_CHANNEL_STRIPE": values[1]})
			c.BinaryData = map[string][]byte{"blob": []byte("b")}
			h.put(c)
		}, false},
		{"envFrom Secret value", func() { h.setSecret("extra", "RESEND_API_KEY", "rotated") }, true},
		{"envFrom Secret new key", func() { h.setSecret("extra", "SLACK_TOKEN", "t") }, true},
		{"envFrom ConfigMap value", func() { h.put(cm(map[string]string{"SLACK_CHANNEL_STRIPE": "rotated"})) }, true},
		{"selected Secret key", func() { h.setSecret("stripe", "api-key", "rotated") }, true},
		{"selected ConfigMap key", func() { h.put(configMapOf("buckets", map[string]string{"main": "rotated", "unused": "changed"})) }, true},
		{"selected key emptied", func() { h.setSecret("stripe", "api-key", "") }, true},
	}
	for _, s := range steps {
		s.change()
		h.mustRun()
		got := h.dashboardHash()
		if (got != hash) != s.rolls {
			t.Errorf("%s: hash changed = %v, want %v", s.name, got != hash, s.rolls)
		}
		hash = got
	}

	stored := h.stored()
	for _, v := range values {
		if strings.Contains(hash, v) || strings.Contains(fmt.Sprintf("%+v", stored.Status), v) {
			t.Errorf("value %q reached the annotation or the status", v)
		}
	}
	for k, v := range h.dashboard().Spec.Template.Annotations {
		if k != dashboardEnvHashAnnotation && k != credentialsHashKey && k != jwtIssuerHashKey {
			t.Errorf("unexpected template annotation %s=%s", k, v)
		}
	}
}

// Sources are hashed with their identity and order, and map insertion order does not matter.
func TestDashboardEnvHashInput(t *testing.T) {
	r := &OramaCloudV1Reconciler{Client: fake.NewClientBuilder().WithScheme(ocv1TestScheme(t)).Build()}
	oc := dashOC(nil)
	hash := func(srcs ...dashboardEnvSource) string {
		h, err := r.dashboardEnvHash(context.Background(), oc, srcs)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	a := map[string]string{}
	a["A"], a["B"], a["C"] = "1", "2", "3"
	b := map[string]string{}
	b["C"], b["B"], b["A"] = "3", "2", "1"
	x := dashboardEnvSource{Kind: "Secret", Name: "x", Data: a}
	y := dashboardEnvSource{Kind: "Secret", Name: "x", Data: b}
	if hash(x) != hash(y) {
		t.Error("map insertion order changed the hash")
	}
	if hash() != "" {
		t.Error("no source should give no hash")
	}
	distinct := map[string]string{}
	for name, srcs := range map[string][]dashboardEnvSource{
		"secret":         {x},
		"configmap":      {{Kind: "ConfigMap", Name: "x", Data: a}},
		"other name":     {{Kind: "Secret", Name: "z", Data: a}},
		"prefix":         {{Kind: "Secret", Name: "x", Prefix: "P_", Data: a}},
		"absent":         {{Kind: "Secret", Name: "x"}},
		"present, empty": {{Kind: "Secret", Name: "x", Data: map[string]string{}}},
		"order 1":        {x, {Kind: "Secret", Name: "w", Data: a}},
		"order 2":        {{Kind: "Secret", Name: "w", Data: a}, x},
	} {
		h := hash(srcs...)
		if other, seen := distinct[h]; seen {
			t.Errorf("%s and %s hash the same", name, other)
		}
		distinct[h] = name
	}
}

// Optional references are kept in the container spec; a missing optional object or selected key is
// hashed as absent, and its arrival (even empty) or removal rolls the dashboard.
func TestDashboardEnvOptionalReferences(t *testing.T) {
	src := secretSource("later-secret")
	src.SecretRef.Optional = optional()
	sel := configMapKeyEnv("SLACK_CHANNEL_NEW_USERS", "channels", "new-users")
	sel.ValueFrom.ConfigMapKeyRef.Optional = optional()
	h := newEngineHarness(t, dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{src}
		d.Env = []corev1.EnvVar{sel}
	}))
	h.mustRun()
	c := h.dashboard().Spec.Template.Spec.Containers[0]
	if last := c.EnvFrom[len(c.EnvFrom)-1]; !reflect.DeepEqual(last, src) {
		t.Errorf("last envFrom = %+v, want the optional reference as written", last)
	}
	if last := c.Env[len(c.Env)-1]; !reflect.DeepEqual(last, sel) {
		t.Errorf("last env = %+v, want the optional selector as written", last)
	}
	hash := h.dashboardHash()
	if hash == "" {
		t.Fatal("references, even absent ones, should carry a hash")
	}
	for _, s := range []struct {
		name   string
		change func()
		rolls  bool
	}{
		{"optional Secret created empty", func() { h.put(secretOf("later-secret", nil)) }, true},
		// The selected key is still absent, so the container's environment is the same.
		{"optional selector's ConfigMap created without the key", func() { h.put(configMapOf("channels", map[string]string{"other": "x"})) }, false},
		{"optional selected key added empty", func() { h.put(configMapOf("channels", map[string]string{"other": "x", "new-users": ""})) }, true},
		{"optional selected key removed", func() { h.put(configMapOf("channels", map[string]string{"other": "x"})) }, true},
		{"optional Secret deleted", func() {
			if err := h.cl.Delete(context.Background(), secretOf("later-secret", nil)); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		s.change()
		h.mustRun()
		got := h.dashboardHash()
		if (got != hash) != s.rolls {
			t.Errorf("%s: hash changed = %v, want %v", s.name, got != hash, s.rolls)
		}
		hash = got
	}

	// A non-optional selector accepts a present empty value.
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Env = []corev1.EnvVar{configMapKeyEnv("SLACK_CHANNEL_NEW_USERS", "empty", "k")}
	})
	h.put(configMapOf("empty", map[string]string{"k": ""}))
	h.mustRun()
}

// A dashboard with neither field set keeps exactly the template it had before #311: the operator's
// three sources, its two variables and no annotation. Literal or downward-API env adds no annotation,
// removing the last reference removes it, and a disabled dashboard is torn down without reading its
// references.
func TestDashboardEnvUnconfiguredTemplate(t *testing.T) {
	template := func(dash func(*oramav1alpha1.DashboardSpec)) corev1.PodTemplateSpec {
		h := newEngineHarness(t, dashOC(dash))
		h.mustRun()
		return h.dashboard().Spec.Template
	}
	base := template(nil)
	c := base.Spec.Containers[0]
	oc := dashOC(nil)
	// The only annotations are #307's credentials hash, for the master key, and #384's issuer hash.
	if !reflect.DeepEqual(slices.Sorted(maps.Keys(base.Annotations)), []string{credentialsHashKey, jwtIssuerHashKey}) || !reflect.DeepEqual(c.EnvFrom, operatorDashboardEnvFrom) || !reflect.DeepEqual(c.Env, operatorDashboardEnv(oc)) {
		t.Fatalf("unconfigured template: annotations %v, envFrom %+v, env %+v", base.Annotations, c.EnvFrom, c.Env)
	}
	if got := template(func(d *oramav1alpha1.DashboardSpec) {
		d.Env, d.EnvFrom = []corev1.EnvVar{}, []corev1.EnvFromSource{}
	}); !reflect.DeepEqual(got, base) {
		t.Errorf("empty lists changed the template:\n got %+v\nwant %+v", got, base)
	}
	literal := template(func(d *oramav1alpha1.DashboardSpec) {
		d.Env = []corev1.EnvVar{{Name: "NEXT_PUBLIC_APP_URL", Value: "https://app.example.test"},
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}
	})
	if !reflect.DeepEqual(literal.Annotations, base.Annotations) {
		t.Errorf("literal and fieldRef env added annotations %v", literal.Annotations)
	}
	if n := len(literal.Spec.Containers[0].Env); n != 5 {
		t.Errorf("literal env: %d variables, want the operator's three plus two", n)
	}

	h := newEngineHarness(t, dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("extra")}
	}), secretOf("extra", map[string]string{"SLACK_TOKEN": "t"}))
	h.mustRun()
	if h.dashboardHash() == "" {
		t.Fatal("a referenced Secret should add the hash")
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.EnvFrom = nil })
	h.mustRun()
	if got := h.dashboard().Spec.Template; !reflect.DeepEqual(got, base) {
		t.Errorf("removing the last reference left %+v, want the unconfigured template", got.Annotations)
	}

	// Disabled: torn down, with refused names and missing references never read.
	reads := 0
	s := ocv1TestScheme(t)
	h = &engineHarness{t: t}
	h.cl = fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&oramav1alpha1.OramaCloudV1{}).
		WithObjects(dashOC(nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == "absent" {
					reads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h.r = &OramaCloudV1Reconciler{Client: h.cl, Scheme: s}
	h.mustRun()
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		d := &o.Spec.Components.Dashboard
		d.Enabled = false
		d.EnvFrom = []corev1.EnvFromSource{secretSource("absent")}
		d.Env = []corev1.EnvVar{{Name: "DATABASE_URL", Value: "x"}, secretKeyEnv("STRIPE_SECRET_KEY", "absent", "k")}
	})
	h.mustRun()
	if h.dashboard() != nil {
		t.Error("the disabled dashboard's Deployment was not torn down")
	}
	if reads != 0 {
		t.Errorf("a disabled dashboard read its references %d times", reads)
	}
}

func TestRequestsForDashboardEnv(t *testing.T) {
	cr := func(ns, name string, enabled bool, mut func(*oramav1alpha1.DashboardSpec)) *oramav1alpha1.OramaCloudV1 {
		oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		oc.Spec.Components.Dashboard.Enabled = enabled
		mut(&oc.Spec.Components.Dashboard)
		return oc
	}
	shared := func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("shared")}
	}
	objs := []client.Object{
		cr("orama-system", "v1", true, shared),
		cr("orama-system", "v2", true, func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{configMapSource("settings")}
			d.Env = []corev1.EnvVar{secretKeyEnv("STRIPE_SECRET_KEY", "shared", "k"), configMapKeyEnv("S3_BUCKET", "buckets", "b"),
				{Name: "LITERAL", Value: "x"}}
		}),
		cr("orama-system", "v3", false, shared),
		cr("orama-system", "v4", true, func(d *oramav1alpha1.DashboardSpec) {
			// Only the last STRIPE_SECRET_KEY reaches the container.
			d.Env = []corev1.EnvVar{secretKeyEnv("STRIPE_SECRET_KEY", "shadowed", "k"), secretKeyEnv("STRIPE_SECRET_KEY", "winner", "k")}
		}),
		cr("other", "v1", true, shared),
	}
	h := newEngineHarness(t, objs...)
	eh := handler.EnqueueRequestsFromMapFunc(h.r.requestsForDashboardEnv)

	cases := []struct {
		obj  client.Object
		want []string
	}{
		{secretOf("shared", nil), []string{"orama-system/v1", "orama-system/v2"}},
		{configMapOf("shared", nil), nil},
		{configMapOf("settings", nil), []string{"orama-system/v2"}},
		{secretOf("settings", nil), nil},
		{configMapOf("buckets", nil), []string{"orama-system/v2"}},
		{secretOf("winner", nil), []string{"orama-system/v4"}},
		{secretOf("shadowed", nil), nil},
		{secretOf("unrelated", nil), nil},
		{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "shared"}}, []string{"other/v1"}},
		{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "shared"}}, nil},
	}
	for _, tc := range cases {
		for kind, fire := range map[string]func(workqueue.RateLimitingInterface){
			"create": func(q workqueue.RateLimitingInterface) {
				eh.Create(context.Background(), event.CreateEvent{Object: tc.obj}, q)
			},
			"update": func(q workqueue.RateLimitingInterface) {
				eh.Update(context.Background(), event.UpdateEvent{ObjectOld: tc.obj, ObjectNew: tc.obj}, q)
			},
			"delete": func(q workqueue.RateLimitingInterface) {
				eh.Delete(context.Background(), event.DeleteEvent{Object: tc.obj}, q)
			},
		} {
			q := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
			fire(q)
			var got []string
			for q.Len() > 0 {
				item, _ := q.Get()
				got = append(got, item.(reconcile.Request).String())
				q.Done(item)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s %T %s/%s enqueued %v, want %v", kind, tc.obj, tc.obj.GetNamespace(), tc.obj.GetName(), got, tc.want)
			}
		}
	}
}

// Deleting a referenced Secret fails the reconcile and keeps the template; recreating it maps back to
// the CR and the next reconcile recovers, with no edit to the CR.
func TestDashboardEnvMissingThenRestored(t *testing.T) {
	h := newEngineHarness(t, dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("extra")}
	}), secretOf("extra", map[string]string{"SLACK_TOKEN": "t"}))
	h.mustRun()
	dep := h.dashboard()

	if err := h.cl.Delete(context.Background(), secretOf("extra", nil)); err != nil {
		t.Fatal(err)
	}
	if err := h.run(); err == nil {
		t.Fatal("Reconcile accepted a missing Secret")
	}
	if ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionFalse ||
		!strings.Contains(ready.Message, "Secret orama-system/extra does not exist") {
		t.Fatalf("Ready = %+v, want False naming the Secret", ready)
	}
	if h.dashboard().ResourceVersion != dep.ResourceVersion {
		t.Error("the failed reconcile changed the dashboard Deployment")
	}

	restored := secretOf("extra", map[string]string{"SLACK_TOKEN": "t"})
	h.put(restored)
	if reqs := h.r.requestsForDashboardEnv(context.Background(), restored); len(reqs) != 1 || reqs[0].Name != "v1" {
		t.Fatalf("the restored Secret mapped to %v, want v1", reqs)
	}
	h.mustRun()
	if ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready"); ready != nil && ready.Reason == "ReconcileError" {
		t.Errorf("Ready = %+v after the Secret came back", ready)
	}
	if got := h.dashboard().Spec.Template.Annotations[dashboardEnvHashAnnotation]; got != dep.Spec.Template.Annotations[dashboardEnvHashAnnotation] {
		t.Error("the same data hashed differently after the round trip")
	}
}
