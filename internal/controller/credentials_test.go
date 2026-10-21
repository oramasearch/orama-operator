/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// stackOC is engineOC with every credential consumer enabled: the in-cluster broker and cache,
// ingestion with HARVEST, workers, the dashboard and analytics-processor.
func stackOC(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		c := &o.Spec.Components
		c.Broker = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		c.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		c.Ingestion = oramav1alpha1.IngestionSpec{Enabled: true, Harvest: true}
		c.Workers = oramav1alpha1.WorkersSpec{Enabled: true}
		c.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true}}
		c.AnalyticsProcessor = oramav1alpha1.AnalyticsProcessorSpec{Toggle: oramav1alpha1.Toggle{Enabled: true}}
		if mut != nil {
			mut(o)
		}
	})
}

// reconcileV1 runs Reconcile for the stored CR v1 and returns its result.
func (h *engineHarness) reconcileV1() (ctrl.Result, error) {
	return h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1", Namespace: "orama-system"}})
}

func (h *engineHarness) mustRun() {
	h.t.Helper()
	if err := h.run(); err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
}

// templates is every owned Deployment's and StatefulSet's pod template, keyed by component.
func (h *engineHarness) templates() map[string]corev1.PodTemplateSpec {
	h.t.Helper()
	got := map[string]corev1.PodTemplateSpec{}
	var deps appsv1.DeploymentList
	var sts appsv1.StatefulSetList
	if err := h.cl.List(context.Background(), &deps); err != nil {
		h.t.Fatal(err)
	}
	if err := h.cl.List(context.Background(), &sts); err != nil {
		h.t.Fatal(err)
	}
	for _, d := range deps.Items {
		got[d.Labels[componentLabel]] = d.Spec.Template
	}
	for _, s := range sts.Items {
		got[s.Labels[componentLabel]] = s.Spec.Template
	}
	return got
}

func (h *engineHarness) secret(name string) *corev1.Secret {
	h.t.Helper()
	sec := &corev1.Secret{}
	if !h.get(name, sec) {
		h.t.Fatalf("Secret %s not found", name)
	}
	return sec
}

func (h *engineHarness) readyCondition() *metav1.Condition {
	return meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
}

// containerIn finds a container, init containers included, of a pod template by name.
func containerIn(t *testing.T, spec corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, c := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %s", name)
	return corev1.Container{}
}

// expandEnv resolves a container's env the way the kubelet does: a secretKeyRef from secrets, and a
// $(VAR) reference only to an entry before it. A reference to a later or missing entry fails.
func expandEnv(t *testing.T, env []corev1.EnvVar, secrets map[string]*corev1.Secret) map[string]string {
	t.Helper()
	got := map[string]string{}
	ref := regexp.MustCompile(`\$\(([A-Z_]+)\)`)
	for _, e := range env {
		if e.ValueFrom != nil {
			// Secrets the test does not supply (the control DB's) resolve to "".
			if k := e.ValueFrom.SecretKeyRef; k != nil && secrets[k.Name] != nil {
				got[e.Name] = string(secrets[k.Name].Data[k.Key])
			}
			continue
		}
		got[e.Name] = ref.ReplaceAllStringFunc(e.Value, func(m string) string {
			v, ok := got[ref.FindStringSubmatch(m)[1]]
			if !ok {
				t.Errorf("%s references %s, which no earlier entry defines", e.Name, m)
			}
			return v
		})
	}
	return got
}

var hex48 = regexp.MustCompile(`^[0-9a-f]{48}$`)

// The broker password literal is gone from the operator's source: only tests may name it.
func TestNoBrokerPasswordLiteralOutsideTests(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "dev-rabbitmq-"+"password") {
			t.Errorf("%s holds the old broker password literal", f)
		}
	}
}

// The broker and cache Secrets are generated once with 48-character hex passwords, labelled so no
// teardown removes them, kept across disable and re-enable, and never changed afterwards. No
// generated value reaches a ConfigMap, pod template, annotation, status or topology.
func TestGeneratedCredentialSecrets(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	broker, cache := h.secret("v1-broker-admin"), h.secret("v1-cache-auth")
	if string(broker.Data["username"]) != "orama" || !hex48.Match(broker.Data["password"]) || !hex48.Match(cache.Data["password"]) {
		t.Fatalf("generated credentials have the wrong shape: username %q, password lengths %d/%d",
			broker.Data["username"], len(broker.Data["password"]), len(cache.Data["password"]))
	}
	if string(broker.Data["password"]) == string(cache.Data["password"]) {
		t.Error("the broker and cache share a password")
	}
	for _, s := range []*corev1.Secret{broker, cache} {
		if s.Labels[componentLabel] != credentialsComponent || !metav1.IsControlledBy(s, h.stored()) {
			t.Errorf("%s: component %q, controlled by the CR %v", s.Name, s.Labels[componentLabel], metav1.IsControlledBy(s, h.stored()))
		}
	}

	values := []string{string(broker.Data["password"]), string(cache.Data["password"]), testMasterKey}
	var dump []byte
	for _, list := range []client.ObjectList{&corev1.ConfigMapList{}, &appsv1.StatefulSetList{}, &appsv1.DeploymentList{},
		&batchv1.JobList{}, &oramav1alpha1.RabbitMQTopologyList{}, &oramav1alpha1.OramaCloudV1List{}} {
		if err := h.cl.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(list)
		dump = append(dump, b...)
	}
	for _, v := range values {
		if strings.Contains(string(dump), v) {
			t.Error("a credential value is in a ConfigMap, workload, topology or the CR")
		}
	}

	h.mustRun()
	if got := h.secret("v1-broker-admin"); got.ResourceVersion != broker.ResourceVersion {
		t.Error("a second reconcile changed v1-broker-admin")
	}
	cacheHash := h.templates()["cache"].Annotations[credentialsHashKey]

	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Broker.Enabled, o.Spec.Components.Cache.Enabled = false, false
	})
	h.mustRun()
	if _, ok := h.templates()["cache"]; ok {
		t.Fatal("disabling the cache kept its StatefulSet")
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Broker.Enabled, o.Spec.Components.Cache.Enabled = true, true
	})
	h.mustRun()
	if !reflect.DeepEqual(h.secret("v1-broker-admin").Data, broker.Data) || !reflect.DeepEqual(h.secret("v1-cache-auth").Data, cache.Data) {
		t.Error("disable and re-enable changed a generated credential")
	}
	if got := h.templates()["cache"].Annotations[credentialsHashKey]; got != cacheHash {
		t.Error("the re-enabled cache does not read the password it had")
	}
}

// A supplied credential Secret is used as it is: not adopted, not rewritten, not filled in.
func TestSuppliedCredentialSecretIsNeverChanged(t *testing.T) {
	supplied := secretOf("v1-cache-auth", map[string]string{"password": "Supplied-pass_0.1~"})
	h := newEngineHarness(t, stackOC(nil), engineSecret(), supplied.DeepCopy())
	h.mustRun()
	got := h.secret("v1-cache-auth")
	if !reflect.DeepEqual(got.Data, supplied.Data) || len(got.OwnerReferences) != 0 || got.Labels != nil {
		t.Errorf("the supplied Secret was changed: %+v", got.ObjectMeta)
	}
	if want := secretDataHash(got); h.templates()["cache"].Annotations[credentialsHashKey] != want {
		t.Error("the cache does not carry the supplied Secret's hash")
	}
}

// A credential Secret missing a key, or holding an empty or unsupported value, fails the
// reconcile with Ready=False naming the Secret and key, and is left as it was.
func TestBrokenCredentialSecretFailsWithoutOverwrite(t *testing.T) {
	for name, c := range map[string]struct {
		data map[string]string
		want string
	}{
		"missing key":  {map[string]string{"other": "x"}, "credential Secret orama-system/v1-cache-auth has no key password"},
		"empty value":  {map[string]string{"password": ""}, "credential Secret orama-system/v1-cache-auth key password is empty"},
		"unsupported":  {map[string]string{"password": "p@ss:w/rd"}, "key password holds characters outside A-Z a-z 0-9 - . _ ~"},
		"line break":   {map[string]string{"password": "line\nbreak"}, "key password holds characters outside"},
		"single quote": {map[string]string{"password": "it's"}, "key password holds characters outside"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := secretOf("v1-cache-auth", c.data)
			h := newEngineHarness(t, stackOC(nil), engineSecret(), bad.DeepCopy())
			if err := h.run(); err == nil {
				t.Fatal("Reconcile accepted a broken credential Secret")
			}
			ready := h.readyCondition()
			if ready == nil || ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, c.want) {
				t.Fatalf("Ready = %+v, want False naming %q", ready, c.want)
			}
			for _, v := range c.data {
				if v != "" && strings.Contains(ready.Message, v) {
					t.Error("the Ready message holds the value")
				}
			}
			if got := h.secret("v1-cache-auth"); !reflect.DeepEqual(got.Data, bad.Data) {
				t.Error("the broken Secret was rewritten")
			}
		})
	}
}

// Two writers creating <cr>-cache-auth at once: the operator loses the race and uses the winner.
func TestCredentialSecretCreateRace(t *testing.T) {
	s := ocv1TestScheme(t)
	oc := stackOC(nil)
	winner := secretOf("v1-cache-auth", map[string]string{"password": strings.Repeat("ab", 24)})
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if sec, ok := obj.(*corev1.Secret); ok && sec.Name == "v1-cache-auth" {
				if err := c.Create(ctx, winner.DeepCopy()); err != nil {
					return err
				}
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, sec.Name)
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	if err := r.reconcileBootstrap(context.Background(), oc); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := r.reconcileCache(context.Background(), oc); err != nil {
		t.Fatalf("cache: %v", err)
	}
	got := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(winner), got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Data, winner.Data) {
		t.Error("the operator overwrote the Secret that won the race")
	}
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "orama-system", Name: "v1-cache"}, sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Annotations[credentialsHashKey] != secretDataHash(got) {
		t.Error("the cache's hash is not the winning Secret's")
	}
}

// The broker renders rabbitmq.conf and its boot definitions from <cr>-broker-admin into a private
// emptyDir, mounts them read-only beside the unchanged enabled_plugins, and the engine and
// nothing else reads that Secret: the engine signs in as its own users and harvest-master as harvest.
// Redis and its clients share <cr>-cache-auth.
func TestBrokerAndCacheCredentialWiring(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	broker, cache, harvest := h.secret("v1-broker-admin"), h.secret("v1-cache-auth"), h.secret("v1-broker-harvest")
	secrets := map[string]*corev1.Secret{broker.Name: broker, cache.Name: cache, harvest.Name: harvest, testMasterSecret: h.secret(testMasterSecret)}
	tmpls := h.templates()
	brokerAdminEnv := []corev1.EnvVar{secretKeyEnv("BROKER_USERNAME", "v1-broker-admin", "username"), secretKeyEnv("BROKER_PASSWORD", "v1-broker-admin", "password")}
	// The writer signs in as oramacore-writer and the readers as oramacore-reader (#99, #795;
	// broker_users_test.go).
	engineBrokerEnv := map[string]corev1.EnvVar{"writer": secretKeyEnv("BROKER_PASSWORD", "v1-broker-users", "writer-password"),
		"reader": secretKeyEnv("BROKER_PASSWORD", "v1-broker-users", "reader-password")}

	// Broker: templates in the ConfigMap, rendered at pod start.
	cm := &corev1.ConfigMap{}
	h.get("v1-broker-config", cm)
	if got := dataKeys(cm); !reflect.DeepEqual(got, []string{"credentials.json", "enabled_plugins", "rabbitmq.conf"}) {
		t.Fatalf("broker ConfigMap keys = %v", got)
	}
	spec := tmpls["broker"].Spec
	init := containerIn(t, spec, "render-config")
	if !reflect.DeepEqual(init.Env, brokerAdminEnv) {
		t.Errorf("broker render-config env = %+v", init.Env)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range containerIn(t, spec, "broker").VolumeMounts {
		mounts[m.MountPath] = m
	}
	for path, vol := range map[string]string{"/etc/rabbitmq/rabbitmq.conf": "rendered-config", "/etc/rabbitmq/credentials.json": "rendered-config",
		"/etc/rabbitmq/enabled_plugins": "config"} {
		m, ok := mounts[path]
		if !ok || m.Name != vol || !m.ReadOnly || m.SubPath != filepath.Base(path) {
			t.Errorf("%s mount = %+v, want read-only subPath of %s", path, m, vol)
		}
	}
	dir := t.TempDir()
	for _, d := range []string{"tmpl", "rendered"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range cm.Data {
		if err := os.WriteFile(filepath.Join(dir, "tmpl", k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.NewReplacer("/tmpl/", dir+"/tmpl/", "/rendered/", dir+"/rendered/").Replace(init.Command[2])
	cmd := exec.Command("sh", "-c", script)
	for k, v := range expandEnv(t, init.Env, secrets) {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH"))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("broker render-config: %v: %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "rendered", "enabled_plugins")); err == nil {
		t.Error("render-config rendered enabled_plugins, which mounts from the ConfigMap")
	}
	conf, _ := os.ReadFile(filepath.Join(dir, "rendered", "rabbitmq.conf"))
	for _, line := range []string{"default_user = orama", "default_pass = " + string(broker.Data["password"]),
		"definitions.import_backend = local_filesystem", "definitions.local.path = /etc/rabbitmq/credentials.json"} {
		if !strings.Contains(string(conf), line+"\n") {
			t.Errorf("rendered rabbitmq.conf lacks %q", strings.SplitN(line, " = ", 2)[0])
		}
	}
	var defs struct {
		Users []struct {
			Name, Password string
			Tags           []string
		}
		Vhosts      []struct{ Name string }
		Permissions []struct{ User, Vhost, Configure, Write, Read string }
	}
	b, _ := os.ReadFile(filepath.Join(dir, "rendered", "credentials.json"))
	if err := json.Unmarshal(b, &defs); err != nil {
		t.Fatalf("rendered credentials.json: %v", err)
	}
	if len(defs.Users) != 1 || defs.Users[0].Name != "orama" || defs.Users[0].Password != string(broker.Data["password"]) ||
		!reflect.DeepEqual(defs.Users[0].Tags, []string{"administrator"}) || len(defs.Vhosts) != 1 || defs.Vhosts[0].Name != "/" ||
		len(defs.Permissions) != 1 || defs.Permissions[0].User != "orama" || defs.Permissions[0].Configure != ".*" {
		t.Errorf("rendered boot definitions are not the admin user, / and its permissions")
	}

	// Engine: its broker user's password as render-config env on both sides.
	for _, side := range []string{"writer", "reader"} {
		init := containerIn(t, tmpls[side].Spec, "render-config")
		if want := engineBrokerEnv[side]; !slices.ContainsFunc(init.Env, func(e corev1.EnvVar) bool { return reflect.DeepEqual(e, want) }) {
			t.Errorf("%s render-config lacks %s from v1-broker-users", side, want.Name)
		}
	}

	// Redis: --requirepass from the cache Secret; clients' URLs expand from the entries before them.
	redis := containerIn(t, tmpls["cache"].Spec, "cache")
	env := expandEnv(t, redis.Env, secrets)
	args := strings.Join(redis.Args, " ")
	if args != "redis-server --appendonly yes --requirepass $(REDIS_PASSWORD)" || env["REDIS_PASSWORD"] != string(cache.Data["password"]) {
		t.Errorf("redis args %q, REDIS_PASSWORD from the cache Secret: %v", args, env["REDIS_PASSWORD"] == string(cache.Data["password"]))
	}
	redisURL := "redis://:" + string(cache.Data["password"]) + "@v1-cache:6379"
	hm := expandEnv(t, containerIn(t, tmpls["harvest-master"].Spec, "harvest-master").Env, secrets)
	if hm["AMQP_HOST"] != "amqp://harvest:"+string(harvest.Data["password"])+"@v1-broker:5672" || hm["REDIS_HOST"] != redisURL {
		t.Error("harvest-master's AMQP_HOST or REDIS_HOST does not expand to the generated credentials")
	}
	if ib := expandEnv(t, containerIn(t, tmpls["index-builder"].Spec, "index-builder").Env, secrets); ib["REDIS_URL"] != redisURL {
		t.Error("index-builder's REDIS_URL does not expand to the cache password")
	}
}

// Every master key consumer takes MASTER_API_KEY from the reference, with a custom key, as an
// explicit entry that overrides a stale value an older component Secret still supplies. The
// reader-only config reads no master key.
func TestMasterAPIKeyConsumersUseTheReference(t *testing.T) {
	want := secretKeyEnv("MASTER_API_KEY", "shared-master", "token")
	stale := secretOf("v1-workers-secret", map[string]string{"MASTER_API_KEY": "dev-master-api-key", "OPENAI_API_KEY": "x"})
	for _, mode := range []string{"combined", "side-only"} {
		t.Run(mode, func(t *testing.T) {
			oc := stackOC(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "shared-master", Key: "token"}
				if mode == "side-only" {
					withSideImages(o)
				}
			})
			h := newEngineHarness(t, oc, engineSecret(), stale.DeepCopy(), secretOf("shared-master", map[string]string{"token": "Pr0d-1:key/&#'\"\\"}))
			h.mustRun()
			tmpls := h.templates()
			consumers := map[string]string{"writer": "render-config",
				"index-builder": "index-builder", "workers": "workers", "dashboard": "dashboard"}
			for comp, ctr := range consumers {
				c := containerIn(t, tmpls[comp].Spec, ctr)
				i := slices.IndexFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "MASTER_API_KEY" })
				if i < 0 || !reflect.DeepEqual(c.Env[i], want) {
					t.Errorf("%s/%s MASTER_API_KEY is not shared-master/token", comp, ctr)
				}
				if tmpls[comp].Annotations[credentialsHashKey] == "" {
					t.Errorf("%s carries no credentials hash", comp)
				}
			}
			// No reader's render-config reads the master key, in either mode (#795): it masks the
			// variable with an empty value instead.
			init := containerIn(t, tmpls["reader"].Spec, "render-config")
			if !slices.ContainsFunc(init.Env, func(e corev1.EnvVar) bool { return e.Name == "MASTER_API_KEY" && e.Value == "" && e.ValueFrom == nil }) ||
				strings.Contains(init.Command[2], "MASTER_API_KEY") {
				t.Error("the reader render-config reads the master key")
			}
			for _, comp := range []string{"engine", "analytics-processor", "index-builder", "dashboard", "workers"} {
				if _, ok := componentSecretData(comp)["MASTER_API_KEY"]; ok {
					t.Errorf("%s's component Secret is still seeded with a master key", comp)
				}
			}
			if got := h.secret("v1-workers-secret"); string(got.Data["MASTER_API_KEY"]) != "dev-master-api-key" {
				t.Error("the older workers Secret lost its stale master key")
			}
			if got := h.readyCondition(); got != nil && strings.HasPrefix(got.Reason, "MasterAPIKey") {
				t.Errorf("Ready = %+v, want the master key accepted", got)
			}
		})
	}
}

// An unusable master key holds the engine at zero on a fresh or running stack, keeps its volumes
// and credentials, reports the cause in Ready without the value, and recovers on its own once the
// Secret is fixed.
func TestMasterAPIKeyGate(t *testing.T) {
	type tc struct {
		ref    *oramav1alpha1.MasterAPIKeySecretRef
		secret map[string]string // testMasterSecret's data; nil deletes it
		reason string
		want   string
	}
	ref := &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
	cases := map[string]tc{
		"secret gone": {ref, nil, "MasterAPIKeySecretMissing", "Secret orama-system/v1-master key MASTER_API_KEY"},
		"key absent":  {ref, map[string]string{"token": testMasterKey}, "MasterAPIKeyKeyMissing", "Secret orama-system/v1-master key MASTER_API_KEY"},
		"custom key absent": {&oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret, Key: "token"},
			map[string]string{"MASTER_API_KEY": testMasterKey}, "MasterAPIKeyKeyMissing", "Secret orama-system/v1-master key token"},
		"empty":       {ref, map[string]string{"MASTER_API_KEY": ""}, "MasterAPIKeyEmpty", "Secret orama-system/v1-master key MASTER_API_KEY is empty"},
		"placeholder": {ref, map[string]string{"MASTER_API_KEY": "dev-master-api-key"}, "MasterAPIKeyPlaceholder", "Secret orama-system/v1-master key MASTER_API_KEY is a dev-* placeholder"},
		"line break":  {ref, map[string]string{"MASTER_API_KEY": "abc\ndef"}, "MasterAPIKeyInvalid", "contains a line break"},
	}
	breakIt := func(h *engineHarness, c tc) {
		h.t.Helper()
		h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.MasterAPIKeySecretRef = c.ref })
		master := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testMasterSecret, Namespace: "orama-system"}}
		if c.secret == nil {
			if err := h.cl.Delete(context.Background(), master); err != nil {
				h.t.Fatal(err)
			}
			return
		}
		h.get(testMasterSecret, master)
		master.Data = secretOf("", c.secret).Data
		if err := h.cl.Update(context.Background(), master); err != nil {
			h.t.Fatal(err)
		}
	}
	checkHeld := func(h *engineHarness, c tc) {
		h.t.Helper()
		res, err := h.reconcileV1()
		if err != nil {
			h.t.Fatalf("Reconcile: %v", err)
		}
		if res.RequeueAfter != 20*time.Second {
			h.t.Errorf("RequeueAfter = %v, want the 20s readiness interval", res.RequeueAfter)
		}
		got := h.stored()
		ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
		if got.Status.Phase != "Degraded" || ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != c.reason || !strings.Contains(ready.Message, c.want) {
			h.t.Fatalf("phase %q, Ready = %+v, want Degraded/%s containing %q", got.Status.Phase, ready, c.reason, c.want)
		}
		for _, v := range c.secret {
			if v != "" && strings.Contains(ready.Message, v) {
				h.t.Error("the Ready message holds the key value")
			}
		}
	}
	for name, c := range cases {
		t.Run(name+" on a fresh stack", func(t *testing.T) {
			h := newEngineHarness(t, stackOC(nil), engineSecret())
			breakIt(h, c)
			checkHeld(h, c)
			for _, n := range []string{"v1-writer", "v1-reader"} {
				if h.get(n, &appsv1.StatefulSet{}) || h.get(n, &appsv1.Deployment{}) {
					t.Errorf("%s was created without a usable master key", n)
				}
			}
		})
		t.Run(name+" on a running stack", func(t *testing.T) {
			h := newEngineHarness(t, stackOC(nil), engineSecret())
			h.mustRun()
			writer, reader := h.writer(), h.reader()
			broker := h.secret("v1-broker-admin")
			breakIt(h, c)
			checkHeld(h, c)
			heldW, heldR := h.writer(), h.reader()
			if replicasOf(heldW.Spec.Replicas) != 0 || replicasOf(heldR.Spec.Replicas) != 0 {
				t.Fatalf("writer/reader replicas = %d/%d, want 0/0", replicasOf(heldW.Spec.Replicas), replicasOf(heldR.Spec.Replicas))
			}
			if !reflect.DeepEqual(heldW.Spec.Template, writer.Spec.Template) || !reflect.DeepEqual(heldW.Spec.VolumeClaimTemplates, writer.Spec.VolumeClaimTemplates) ||
				!reflect.DeepEqual(heldR.Spec.Template, reader.Spec.Template) {
				t.Error("holding the engine changed more than its replica counts")
			}
			if !reflect.DeepEqual(h.secret("v1-broker-admin").Data, broker.Data) {
				t.Error("holding the engine changed a credential")
			}
			checkHeld(h, c) // a second pass changes nothing and still reports the cause

			// Fixing the Secret (or the reference) is picked up without a spec edit.
			fix := tc{ref: ref, secret: map[string]string{"MASTER_API_KEY": testMasterKey}}
			if c.secret == nil {
				if err := h.cl.Create(context.Background(), secretOf(testMasterSecret, fix.secret)); err != nil {
					t.Fatal(err)
				}
				if reqs := h.r.requestsForSecret(context.Background(), secretOf(testMasterSecret, nil)); len(reqs) != 1 || reqs[0].Name != "v1" {
					t.Fatalf("creating the Secret mapped to %v, want v1", reqs)
				}
			} else {
				breakIt(h, fix)
			}
			h.mustRun()
			if replicasOf(h.writer().Spec.Replicas) != 1 || replicasOf(h.reader().Spec.Replicas) != 3 {
				t.Errorf("replicas = %d/%d after the fix, want 1/3", replicasOf(h.writer().Spec.Replicas), replicasOf(h.reader().Spec.Replicas))
			}
			if ready := h.readyCondition(); ready == nil || strings.HasPrefix(ready.Reason, "MasterAPIKey") {
				t.Errorf("Ready = %+v after the fix, want the master key cause gone", ready)
			}
		})
	}
}

// A disabled engine is not held on its master key's value, and a CR with no master key consumer
// needs no reference at all.
func TestMasterAPIKeyGateDisabledEngine(t *testing.T) {
	for name, oc := range map[string]*oramav1alpha1.OramaCloudV1{
		"no consumers, no reference": newOCV1(nil),
		"dashboard with a dev-* key": newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Dashboard.Enabled = true
			o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "dev-key"}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, oc, secretOf("dev-key", map[string]string{"MASTER_API_KEY": "dev-master-api-key"}))
			h.mustRun()
			if ready := h.readyCondition(); ready != nil && strings.HasPrefix(ready.Reason, "MasterAPIKey") {
				t.Errorf("Ready = %+v, want no master key failure", ready)
			}
		})
	}
	// A consumer on a stack that names no master key reads the generated one (#615).
	h := newEngineHarness(t, newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Workers.Enabled = true }))
	h.mustRun()
	if ready := h.readyCondition(); ready != nil && strings.HasPrefix(ready.Reason, "MasterAPIKey") {
		t.Errorf("Ready = %+v, want no master key failure", ready)
	}
	workers := &appsv1.Deployment{}
	if !h.get("v1-workers", workers) {
		t.Fatal("workers were not created with the generated master key")
	}
	found := false
	for _, e := range workers.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "MASTER_API_KEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil &&
			e.ValueFrom.SecretKeyRef.Name == "v1-master-api-key" && e.ValueFrom.SecretKeyRef.Key == "MASTER_API_KEY" {
			found = true
		}
	}
	if !found {
		t.Error("workers do not read MASTER_API_KEY from the generated v1-master-api-key")
	}
}

// TestGeneratedMasterAPIKey checks that a stack naming no master key gets <cr>-master-api-key, a
// random 48-character hex MASTER_API_KEY, created once and never replaced, that the engine runs on
// it, and that a stack naming its own Secret gets no generated one (#615).
func TestGeneratedMasterAPIKey(t *testing.T) {
	oc := stackOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.MasterAPIKeySecretRef = nil })
	h := newEngineHarness(t, oc, engineSecret())
	h.mustRun()
	sec := h.secret("v1-master-api-key")
	key := string(sec.Data["MASTER_API_KEY"])
	if len(key) != 48 || strings.Trim(key, "0123456789abcdef") != "" {
		t.Fatalf("generated MASTER_API_KEY has %d characters, want 48 lowercase hex", len(key))
	}
	if ready := h.readyCondition(); ready != nil && strings.HasPrefix(ready.Reason, "MasterAPIKey") {
		t.Errorf("Ready = %+v, want no master key failure", ready)
	}
	if w := h.writer(); replicasOf(w.Spec.Replicas) == 0 {
		t.Error("the writer is held with a generated master key")
	}
	h.mustRun()
	if got := string(h.secret("v1-master-api-key").Data["MASTER_API_KEY"]); got != key {
		t.Error("the generated master key changed on a second reconcile")
	}

	named := newEngineHarness(t, stackOC(nil), engineSecret())
	named.mustRun()
	if named.get("v1-master-api-key", &corev1.Secret{}) {
		t.Error("a stack naming its master key Secret got a generated one")
	}
}

// A failed read of the master key Secret is an error, not a diagnosis.
func TestMasterAPIKeyReadError(t *testing.T) {
	s := ocv1TestScheme(t)
	oc := engineOC(nil)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc).WithStatusSubresource(oc).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok && key.Name == testMasterSecret {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	if cond, err := r.validateMasterAPIKey(context.Background(), oc); err == nil || cond != nil {
		t.Fatalf("validateMasterAPIKey = %v, %v; want an error and no condition", cond, err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err == nil {
		t.Fatal("Reconcile succeeded without reading the master key")
	}
	got := &oramav1alpha1.OramaCloudV1{}
	_ = cl.Get(context.Background(), client.ObjectKeyFromObject(oc), got)
	if ready := meta.FindStatusCondition(got.Status.Conditions, "Ready"); ready == nil || ready.Reason != "ReconcileError" {
		t.Errorf("Ready = %+v, want ReconcileError", ready)
	}
}

// A change to one credential Secret rolls exactly the workloads that read it. Unchanged data,
// rewritten data, metadata and unrelated Secrets roll nothing, and the engine's other annotations
// stay.
func TestCredentialChangeRollsOnlyItsConsumers(t *testing.T) {
	for _, mode := range []string{"combined", "side-only"} {
		t.Run(mode, func(t *testing.T) {
			oc := stackOC(func(o *oramav1alpha1.OramaCloudV1) {
				if mode == "side-only" {
					withSideImages(o)
				}
			})
			h := newEngineHarness(t, oc, engineSecret(), secretOf("unrelated", map[string]string{"X": "1"}))
			h.mustRun()
			// No reader reads the master key, in either mode (#795).
			masterReaders := []string{"dashboard", "index-builder", "workers", "writer"}
			before := h.templates()
			for _, a := range []string{configHashKey, llmSecretHashKey, credentialsHashKey} {
				if before["writer"].Annotations[a] == "" || before["reader"].Annotations[a] == "" {
					t.Fatalf("an engine template lacks %s", a)
				}
			}
			steady := func(what string) {
				t.Helper()
				h.mustRun()
				if got := h.templates(); !reflect.DeepEqual(got, before) {
					t.Errorf("%s changed a pod template", what)
				}
			}
			rolled := func(what string, want []string) {
				t.Helper()
				h.mustRun()
				got := h.templates()
				var changed []string
				for comp, tmpl := range got {
					if !reflect.DeepEqual(tmpl, before[comp]) {
						changed = append(changed, comp)
						for k, v := range before[comp].Annotations {
							if k != credentialsHashKey && tmpl.Annotations[k] != v {
								t.Errorf("%s: %s changed %s", what, comp, k)
							}
						}
					}
				}
				sort.Strings(changed)
				sort.Strings(want)
				if !reflect.DeepEqual(changed, want) {
					t.Errorf("%s rolled %v, want %v", what, changed, want)
				}
				before = got
			}

			steady("an unchanged reconcile")
			h.setSecret("unrelated", "X", "2")
			steady("an unrelated Secret change")
			for _, name := range []string{"v1-cache-auth", "v1-redis-http", "v1-broker-admin", "v1-broker-users", "v1-broker-harvest", "v1-analytics-api-key", testMasterSecret} {
				sec := h.secret(name)
				sec.Labels = map[string]string{"touched": "yes"}
				data := map[string][]byte{}
				keys := make([]string, 0, len(sec.Data))
				for k := range sec.Data {
					keys = append(keys, k)
				}
				for _, k := range slices.Backward(keys) {
					data[k] = slices.Clone(sec.Data[k])
				}
				sec.Data = data
				if err := h.cl.Update(context.Background(), sec); err != nil {
					t.Fatal(err)
				}
			}
			steady("metadata edits and rewritten data")

			h.setSecret("v1-cache-auth", "password", "rotatedcache0123")
			rolled("a cache password change", []string{"cache", "harvest-master", "index-builder", "redis-http"})
			// The shim requires the token and the dashboard sends it (#562).
			h.setSecret("v1-redis-http", "token", "rotatedredishttp0123")
			rolled("a redis-http token change", []string{"dashboard", "redis-http"})
			// The engine and harvest-master sign in as their own users (#99, #824), so the admin
			// password rolls only the broker, an engine user's password only the engine, and the
			// harvest password only harvest-master.
			h.setSecret("v1-broker-admin", "password", "rotatedbroker0123")
			rolled("a broker password change", []string{"broker"})
			h.setSecret("v1-broker-users", "reader-password", "rotatedreader0123")
			rolled("a broker user password change", []string{"reader", "writer"})
			h.setSecret("v1-broker-harvest", "password", "rotatedharvest0123")
			rolled("a harvest password change", []string{"harvest-master"})
			h.setSecret(testMasterSecret, "MASTER_API_KEY", "rotated-master-0123")
			rolled("a master key change", masterReaders)
			// Only the reader-only config reads the analytics key; the writer never does.
			analyticsReaders := []string{"analytics-processor"}
			if mode == "side-only" {
				analyticsReaders = append(analyticsReaders, "reader")
			}
			h.setSecret("v1-analytics-api-key", "ANALYTICS_API_KEY", "rotatedanalytics0123")
			rolled("an analytics key change", analyticsReaders)
			steady("a second unchanged reconcile")

			// Pointing the reference at another Secret rolls the master key readers onto it.
			if err := h.cl.Create(context.Background(), secretOf("next-master", map[string]string{"key": "next-master-4567"})); err != nil {
				t.Fatal(err)
			}
			h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "next-master", Key: "key"}
			})
			rolled("a new master key reference", masterReaders)
		})
	}
}

// Secret events reach every CR that reads the Secret, whoever owns it, and follow a changed
// reference; a Ready CR is enqueued like any other.
func TestRequestsForSecretCredentials(t *testing.T) {
	cr := func(ns, name, ref string, engine bool) *oramav1alpha1.OramaCloudV1 {
		oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		oc.Spec.Components.Engine.Enabled = engine
		oc.Spec.Components.Dashboard.Enabled = true
		if ref != "" {
			oc.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: ref, Key: "token"}
		}
		oc.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllComponentsReady"}}
		return oc
	}
	h := newEngineHarness(t, cr("orama-system", "a", "shared", true), cr("orama-system", "b", "shared", false),
		cr("orama-system", "c", "other", true), cr("orama-system", "d", "", true), cr("elsewhere", "a", "shared", true))
	eh := handler.EnqueueRequestsFromMapFunc(h.r.requestsForSecret)
	check := func(ns, name string, want ...string) {
		t.Helper()
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		for kind, fire := range map[string]func(workqueue.RateLimitingInterface){
			"create": func(q workqueue.RateLimitingInterface) {
				eh.Create(context.Background(), event.CreateEvent{Object: sec}, q)
			},
			"update": func(q workqueue.RateLimitingInterface) {
				eh.Update(context.Background(), event.UpdateEvent{ObjectOld: sec, ObjectNew: sec}, q)
			},
			"delete": func(q workqueue.RateLimitingInterface) {
				eh.Delete(context.Background(), event.DeleteEvent{Object: sec}, q)
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
			if !slices.Equal(got, want) {
				t.Errorf("%s %s/%s enqueued %v, want %v", kind, ns, name, got, want)
			}
		}
	}
	check("orama-system", "shared", "orama-system/a", "orama-system/b")
	check("orama-system", "other", "orama-system/c")
	check("orama-system", "a-broker-admin", "orama-system/a")
	check("orama-system", "c-cache-auth", "orama-system/c")
	check("orama-system", "d-redis-http", "orama-system/d")
	check("orama-system", "a-analytics-api-key", "orama-system/a")
	check("orama-system", "unrelated")
	check("elsewhere", "shared", "elsewhere/a")

	c := &oramav1alpha1.OramaCloudV1{}
	if err := h.cl.Get(context.Background(), types.NamespacedName{Namespace: "orama-system", Name: "c"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.MasterAPIKeySecretRef.Name = "shared"
	if err := h.cl.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	check("orama-system", "shared", "orama-system/a", "orama-system/b", "orama-system/c")
	check("orama-system", "other")
}

// The e2e runner waits for its target and the target's reference, then takes MASTER_API_KEY from
// that reference; an existing Job is never rewritten.
func TestE2ERunnerUsesTargetMasterKeyRef(t *testing.T) {
	s := ocv1TestScheme(t)
	test := &oramav1alpha1.OramaCloudV1E2ETest{ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "orama-system"},
		Spec: oramav1alpha1.OramaCloudV1E2ETestSpec{TargetRef: "v1"}}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(test).WithStatusSubresource(test).Build()
	r := &OramaCloudV1E2ETestReconciler{Client: cl, Scheme: s}
	run := func() ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(test)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	pending := func(want string) {
		t.Helper()
		if res := run(); res.RequeueAfter != e2eTargetWait {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, e2eTargetWait)
		}
		got := &oramav1alpha1.OramaCloudV1E2ETest{}
		_ = cl.Get(context.Background(), client.ObjectKeyFromObject(test), got)
		if got.Status.Phase != "Pending" || !strings.Contains(got.Status.Message, want) {
			t.Errorf("status = %q %q, want Pending %q", got.Status.Phase, got.Status.Message, want)
		}
		if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "orama-system", Name: "smoke-e2e"}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
			t.Errorf("a Job exists while waiting (%v)", err)
		}
	}
	pending("waiting for OramaCloudV1 orama-system/v1")
	target := newOCV1(nil)
	if err := cl.Create(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	// A target naming no master key reads the generated <cr>-master-api-key (#615).
	run()
	generated := &batchv1.Job{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "orama-system", Name: "smoke-e2e"}, generated); err != nil {
		t.Fatal(err)
	}
	wantGenerated := secretKeyEnv("MASTER_API_KEY", "v1-master-api-key", "MASTER_API_KEY")
	if env := generated.Spec.Template.Spec.Containers[0].Env; !slices.ContainsFunc(env, func(e corev1.EnvVar) bool { return reflect.DeepEqual(e, wantGenerated) }) {
		t.Errorf("runner env = %+v, want MASTER_API_KEY from the generated v1-master-api-key", env)
	}
	if err := cl.Delete(context.Background(), generated); err != nil {
		t.Fatal(err)
	}

	target.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "shared-master", Key: "token"}
	if err := cl.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	run()
	job := &batchv1.Job{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "orama-system", Name: "smoke-e2e"}, job); err != nil {
		t.Fatal(err)
	}
	want := secretKeyEnv("MASTER_API_KEY", "shared-master", "token")
	if env := job.Spec.Template.Spec.Containers[0].Env; !slices.ContainsFunc(env, func(e corev1.EnvVar) bool { return reflect.DeepEqual(e, want) }) {
		t.Errorf("runner env = %+v, want MASTER_API_KEY from shared-master/token", env)
	}

	target.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "rotated"}
	if err := cl.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	run()
	again := &batchv1.Job{}
	_ = cl.Get(context.Background(), client.ObjectKeyFromObject(job), again)
	if !reflect.DeepEqual(again.Spec, job.Spec) {
		t.Error("the existing Job was rewritten for a new reference")
	}
}

// <cr>-analytics-api-key is generated once on every stack: Opaque, one 48-character hex
// ANALYTICS_API_KEY, labelled credentials and controlled by the CR, so the owned-Secret watch
// enqueues the CR on a change. Reconciling again and disabling and re-enabling analytics-processor
// and ClickHouse keep it, and its consumers keep reading the same value.
func TestGeneratedAnalyticsKey(t *testing.T) {
	h := newEngineHarness(t, stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		withSideImages(o)
		o.Spec.Components.Analytics = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
	}), engineSecret())
	h.mustRun()
	key := h.secret("v1-analytics-api-key")
	if key.Namespace != "orama-system" || key.Type != corev1.SecretTypeOpaque || len(key.Data) != 1 || !hex48.Match(key.Data["ANALYTICS_API_KEY"]) {
		t.Fatalf("v1-analytics-api-key: namespace %q, type %q, %d keys, ANALYTICS_API_KEY length %d",
			key.Namespace, key.Type, len(key.Data), len(key.Data["ANALYTICS_API_KEY"]))
	}
	if key.Labels[componentLabel] != credentialsComponent || !metav1.IsControlledBy(key, h.stored()) {
		t.Errorf("component %q, controlled by the CR %v", key.Labels[componentLabel], metav1.IsControlledBy(key, h.stored()))
	}
	for _, other := range []string{"v1-broker-admin", "v1-cache-auth"} {
		if string(h.secret(other).Data["password"]) == string(key.Data["ANALYTICS_API_KEY"]) {
			t.Errorf("the analytics key equals %s's password", other)
		}
	}
	before := h.templates()

	h.mustRun()
	if got := h.secret("v1-analytics-api-key"); got.ResourceVersion != key.ResourceVersion {
		t.Error("a second reconcile changed v1-analytics-api-key")
	}
	if !reflect.DeepEqual(h.templates(), before) {
		t.Error("a second reconcile changed a pod template")
	}

	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.AnalyticsProcessor.Enabled, o.Spec.Components.Analytics.Enabled = false, false
	})
	h.mustRun()
	if _, ok := h.templates()["analytics-processor"]; ok {
		t.Fatal("disabling analytics-processor kept its Deployment")
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.AnalyticsProcessor.Enabled, o.Spec.Components.Analytics.Enabled = true, true
	})
	h.mustRun()
	if !reflect.DeepEqual(h.secret("v1-analytics-api-key").Data, key.Data) {
		t.Error("disable and re-enable changed the analytics key")
	}
	after := h.templates()
	for _, comp := range []string{"analytics-processor", "reader"} {
		if after[comp].Annotations[credentialsHashKey] != before[comp].Annotations[credentialsHashKey] {
			t.Errorf("the re-enabled stack's %s does not read the key it had", comp)
		}
	}
}

// An existing analytics key with a value of its own is used as it is.
func TestExistingAnalyticsKeyIsKept(t *testing.T) {
	existing := secretOf("v1-analytics-api-key", map[string]string{"ANALYTICS_API_KEY": "Custom-analytics_key.01~"})
	h := newEngineHarness(t, stackOC(withSideImages), engineSecret(), existing.DeepCopy())
	h.mustRun()
	got := h.secret("v1-analytics-api-key")
	if !reflect.DeepEqual(got.Data, existing.Data) {
		t.Fatal("the existing analytics key was changed")
	}
	tmpls := h.templates()
	if want := secretDataHash(h.secret("v1-broker-users"), got); tmpls["reader"].Annotations[credentialsHashKey] != want {
		t.Error("the readers do not carry the existing key's hash")
	}
	if want := secretDataHash(got, h.secret("v1-analytics-processor-secret")); tmpls["analytics-processor"].Annotations[credentialsHashKey] != want {
		t.Error("analytics-processor does not carry the existing key's hash")
	}
}

// An analytics key Secret without a usable ANALYTICS_API_KEY fails the reconcile, naming the
// Secret and key, is left as it was, and no reader or processor starts with another key.
func TestBrokenAnalyticsKeyFailsWithoutFallback(t *testing.T) {
	for name, c := range map[string]struct {
		data map[string]string
		want string
	}{
		"missing key": {map[string]string{"other": "x"}, "credential Secret orama-system/v1-analytics-api-key has no key ANALYTICS_API_KEY"},
		"empty value": {map[string]string{"ANALYTICS_API_KEY": ""}, "credential Secret orama-system/v1-analytics-api-key key ANALYTICS_API_KEY is empty"},
		"line break":  {map[string]string{"ANALYTICS_API_KEY": "line\nbreak"}, "key ANALYTICS_API_KEY holds characters outside"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := secretOf("v1-analytics-api-key", c.data)
			h := newEngineHarness(t, stackOC(withSideImages), engineSecret(), bad.DeepCopy())
			if err := h.run(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Reconcile = %v, want an error naming %q", err, c.want)
			}
			if ready := h.readyCondition(); ready == nil || ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, c.want) {
				t.Fatalf("Ready = %+v, want False naming %q", ready, c.want)
			}
			if got := h.secret("v1-analytics-api-key"); !reflect.DeepEqual(got.Data, bad.Data) {
				t.Error("the broken Secret was rewritten")
			}
			for comp := range h.templates() {
				t.Errorf("%s was created with an unusable analytics key", comp)
			}
		})
	}
}

// Two writers creating <cr>-analytics-api-key at once: the operator loses the race, and the readers
// and processor read the winner.
func TestAnalyticsKeyCreateRace(t *testing.T) {
	s := ocv1TestScheme(t)
	oc := stackOC(withSideImages)
	winner := secretOf("v1-analytics-api-key", map[string]string{"ANALYTICS_API_KEY": strings.Repeat("cd", 24)})
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc, secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": testMasterKey}), readyBrokerTopology("v1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if sec, ok := obj.(*corev1.Secret); ok && sec.Name == winner.Name {
					if err := c.Create(ctx, winner.DeepCopy()); err != nil {
						return err
					}
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, sec.Name)
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	ctx := context.Background()
	if err := r.reconcileBootstrap(ctx, oc); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := r.reconcileEngine(ctx, oc); err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := r.reconcileAnalyticsProcessor(ctx, oc); err != nil {
		t.Fatalf("analytics-processor: %v", err)
	}
	got := &corev1.Secret{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(winner), got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Data, winner.Data) {
		t.Fatal("the operator overwrote the Secret that won the race")
	}
	broker, component := &corev1.Secret{}, &corev1.Secret{}
	for name, sec := range map[string]*corev1.Secret{"v1-broker-users": broker, "v1-analytics-processor-secret": component} {
		if err := cl.Get(ctx, types.NamespacedName{Namespace: "orama-system", Name: name}, sec); err != nil {
			t.Fatal(err)
		}
	}
	sts, dep := &appsv1.StatefulSet{}, &appsv1.Deployment{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "orama-system", Name: "v1-reader"}, sts); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "orama-system", Name: "v1-analytics-processor"}, dep); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Annotations[credentialsHashKey] != secretDataHash(broker, got) {
		t.Error("the readers' hash is not the winning Secret's")
	}
	if dep.Spec.Template.Annotations[credentialsHashKey] != secretDataHash(got, component) {
		t.Error("the processor's hash is not the winning Secret's")
	}
}

// A read or create error on the analytics key fails bootstrap with that error and creates no
// consumer.
func TestAnalyticsKeyReadAndCreateErrors(t *testing.T) {
	boom := errors.New("boom")
	for name, funcs := range map[string]interceptor.Funcs{
		"read": {Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok && key.Name == "v1-analytics-api-key" {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		}},
		"create": {Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == "v1-analytics-api-key" {
				return boom
			}
			return c.Create(ctx, obj, opts...)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s := ocv1TestScheme(t)
			oc := stackOC(withSideImages)
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc).WithInterceptorFuncs(funcs).Build()
			r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
			if err := r.reconcileBootstrap(context.Background(), oc); !errors.Is(err, boom) || !strings.Contains(err.Error(), "v1-analytics-api-key") {
				t.Fatalf("bootstrap = %v, want the %s error naming the Secret", err, name)
			}
		})
	}
}
