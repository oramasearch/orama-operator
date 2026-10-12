/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for #307: the broker and Redis passwords come from one Secret each, the master API key from
// one Secret, and CredentialsReady reports seeded or missing credentials.

// envRef is "<secret>/<key>" for an env entry read from a Secret, "" otherwise.
func envRef(e corev1.EnvVar) string {
	if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
		return ""
	}
	return e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
}

// envIndex is the position of the env entry named name, or -1.
func envIndex(env []corev1.EnvVar, name string) int {
	return slices.IndexFunc(env, func(e corev1.EnvVar) bool { return e.Name == name })
}

// assertEnvFromSecret checks that env has name read from ref, before position `before` (when >= 0),
// so a $(name) reference later in the list expands.
func assertEnvFromSecret(t *testing.T, what string, env []corev1.EnvVar, name, ref string, before int) {
	t.Helper()
	i := envIndex(env, name)
	if i < 0 {
		t.Errorf("%s has no %s env", what, name)
		return
	}
	if got := envRef(env[i]); got != ref {
		t.Errorf("%s %s comes from %q, want Secret key %s", what, name, got, ref)
	}
	if before >= 0 && i > before {
		t.Errorf("%s defines %s after the entry that references it", what, name)
	}
}

// runRender runs a render-config command with its /tmpl and output paths pointed at dir.
func runRender(t *testing.T, command []string, outDir string, dir string, env ...string) {
	t.Helper()
	script := strings.NewReplacer("/tmpl/", dir+"/in-", outDir, dir+"/out-").Replace(command[2])
	cmd := exec.Command(command[0], command[1], script)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render command: %v\n%s", err, out)
	}
}

func TestBrokerConfigRendersAdminPasswordFromSecret(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))

	cm := getObj(t, cl, "v1-broker-config", &corev1.ConfigMap{})
	tmpl := cm.Data["rabbitmq.conf"]
	if !strings.Contains(tmpl, "default_user = __BROKER_USERNAME__\n") || !strings.Contains(tmpl, "default_pass = __BROKER_PASSWORD__\n") {
		t.Fatalf("rabbitmq.conf template must hold the credential placeholders:\n%s", tmpl)
	}

	sts := getObj(t, cl, "v1-broker", &appsv1.StatefulSet{})
	pod := sts.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "render-config" {
		t.Fatalf("broker init containers = %+v, want render-config", pod.InitContainers)
	}
	render := pod.InitContainers[0]
	assertEnvFromSecret(t, "broker render-config", render.Env, "BROKER_USERNAME", "v1-broker-admin/username", -1)
	assertEnvFromSecret(t, "broker render-config", render.Env, "BROKER_PASSWORD", "v1-broker-admin/password", -1)
	if !slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == "rendered-config" && m.MountPath == "/etc/rabbitmq/rabbitmq.conf" && m.SubPath == "rabbitmq.conf"
	}) {
		t.Errorf("broker does not mount the rendered rabbitmq.conf: %+v", pod.Containers[0].VolumeMounts)
	}
	if sts.Spec.Template.Annotations["orama.cloud/config-hash"] == "" {
		t.Error("a broker template change must roll the broker (no config-hash annotation)")
	}

	// Run the init container's command: the Secret's values replace the placeholders, sed specials
	// included.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in-rabbitmq.conf"), []byte(tmpl), 0o600); err != nil {
		t.Fatal(err)
	}
	runRender(t, render.Command, "/rendered/", dir, "BROKER_USERNAME=admin", `BROKER_PASSWORD=p&a#s\s`)
	rendered, err := os.ReadFile(filepath.Join(dir, "out-rabbitmq.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "default_user = admin\ndefault_pass = p&a#s\\s\n") {
		t.Errorf("rendered rabbitmq.conf does not carry the Secret's credentials:\n%s", rendered)
	}
}

func TestEngineAndHarvestTakeBrokerPasswordFromSecret(t *testing.T) {
	oc := fullStackOCV1(nil)
	for _, tmpl := range []string{oramacoreConfigYAML(oc), oramacoreWriterConfigYAML(oc), oramacoreReaderConfigYAML(oc)} {
		if strings.Count(tmpl, "    password: '__BROKER_PASSWORD__'\n") != strings.Count(tmpl, "type: rabbitmq") {
			t.Errorf("every rabbitmq section must take its password from the placeholder:\n%s", tmpl)
		}
	}

	// The render-config command fills the broker user and password from its env.
	side := engineSideFor(oc, "reader")
	render := renderConfigInit(oc, side)
	assertEnvFromSecret(t, "engine render-config", render.Env, "BROKER_PASSWORD", "v1-broker-admin/password", -1)
	assertEnvFromSecret(t, "engine render-config", render.Env, "BROKER_USERNAME", "v1-broker-admin/username", -1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in-"+side.configKey), []byte(side.template), 0o600); err != nil {
		t.Fatal(err)
	}
	runRender(t, render.Command, "/etc/oramacore/", dir, "BROKER_USERNAME=orama", "BROKER_PASSWORD=br'o&ker#", "MASTER_API_KEY=m")
	rendered, err := os.ReadFile(filepath.Join(dir, "out-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"writer_side", "reader_side"} {
		cfg := parseEngineConfig(t, "rendered config", string(rendered))
		io := "output"
		if section == "reader_side" {
			io = "input"
		}
		conn := cfg[section].(map[string]any)[io].(map[string]any)
		if conn["user"] != "orama" || conn["password"] != "br'o&ker#" {
			t.Errorf("%s.%s user/password = %v/%v, want the Secret's", section, io, conn["user"], conn["password"])
		}
	}

	cl, _ := reconcileOCV1(t, oc)
	env := getObj(t, cl, "v1-harvest-master", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].Env
	amqp := envIndex(env, "AMQP_HOST")
	if amqp < 0 || env[amqp].Value != "amqp://$(BROKER_USERNAME):$(BROKER_PASSWORD)@v1-broker:5672" {
		t.Fatalf("harvest-master AMQP_HOST = %+v, want the credentials as references", env)
	}
	assertEnvFromSecret(t, "harvest-master", env, "BROKER_USERNAME", "v1-broker-admin/username", amqp)
	assertEnvFromSecret(t, "harvest-master", env, "BROKER_PASSWORD", "v1-broker-admin/password", amqp)
}

func TestCacheRequiresPasswordFromSecret(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))

	cache := getObj(t, cl, "v1-cache", &appsv1.StatefulSet{}).Spec.Template.Spec.Containers[0]
	if i := slices.Index(cache.Args, "--requirepass"); i < 0 || i+1 >= len(cache.Args) || cache.Args[i+1] != "$(REDIS_PASSWORD)" {
		t.Errorf("cache args = %v, want --requirepass $(REDIS_PASSWORD)", cache.Args)
	}
	assertEnvFromSecret(t, "cache", cache.Env, "REDIS_PASSWORD", "v1-cache-auth/password", -1)

	// Every client env entry that points at the cache carries the password reference.
	clients := map[string]string{"v1-harvest-master": "REDIS_HOST", "v1-index-builder": "REDIS_URL"}
	for dep, name := range clients {
		env := getObj(t, cl, dep, &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].Env
		i := envIndex(env, name)
		if i < 0 || env[i].Value != "redis://:$(REDIS_PASSWORD)@v1-cache:6379" {
			t.Errorf("%s %s = %+v, want the password as a reference", dep, name, env)
			continue
		}
		assertEnvFromSecret(t, dep, env, "REDIS_PASSWORD", "v1-cache-auth/password", i)
	}
	var deps appsv1.DeploymentList
	if err := cl.List(context.Background(), &deps, client.InNamespace("orama-system")); err != nil {
		t.Fatal(err)
	}
	for _, d := range deps.Items {
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			if strings.HasPrefix(e.Value, "redis://") && !strings.Contains(e.Value, "$(REDIS_PASSWORD)@") {
				t.Errorf("%s %s connects to Redis without the password: %s", d.Name, e.Name, e.Value)
			}
		}
	}
}

func TestExistingCredentialSecretsAreKept(t *testing.T) {
	secret := func(name string, data map[string]string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orama-system"}, Data: map[string][]byte{}}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	// A stack reconciled before #307: the broker still runs with the old literal password, and the
	// master key lives in the engine Secret.
	broker := secret("v1-broker-admin", map[string]string{"username": "orama", "password": "dev-rabbitmq-password"})
	cache := secret("v1-cache-auth", map[string]string{"password": "kept-cache-password"})
	engine := secret("v1-engine-secret", map[string]string{"MASTER_API_KEY": "legacy-master-key"})
	cl, r := reconcileOCV1(t, fullStackOCV1(nil), broker, cache, engine)
	reconcileAgain(t, cl, r, nil)

	for name, want := range map[string]map[string]string{
		"v1-broker-admin":   {"username": "orama", "password": "dev-rabbitmq-password"},
		"v1-cache-auth":     {"password": "kept-cache-password"},
		"v1-master-api-key": {"MASTER_API_KEY": "legacy-master-key"},
	} {
		got := getObj(t, cl, name, &corev1.Secret{})
		for k, v := range want {
			if secretValue(got, k) != v {
				t.Errorf("%s/%s changed on reconcile", name, k)
			}
		}
	}

	// A new stack gets random passwords, never a seeded value.
	cl, _ = reconcileOCV1(t, fullStackOCV1(nil))
	for name, key := range map[string]string{"v1-broker-admin": "password", "v1-cache-auth": "password", "v1-master-api-key": "MASTER_API_KEY"} {
		v := secretValue(getObj(t, cl, name, &corev1.Secret{}), key)
		if len(v) < 32 || strings.HasPrefix(v, seededPrefix) {
			t.Errorf("%s/%s is not a generated value", name, key)
		}
	}
}

func TestCredentialsReadyNamesSeededAndMissingCredentials(t *testing.T) {
	ctx := context.Background()
	oc := fullStackOCV1(nil)
	legacyBroker := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-broker-admin", Namespace: "orama-system"},
		Data: map[string][]byte{"username": []byte("orama"), "password": []byte("dev-rabbitmq-password")}}
	cl, r := reconcileOCV1(t, oc, legacyBroker)

	get := func() *oramav1alpha1.OramaCloudV1 { return getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}) }
	cond := apimeta.FindStatusCondition(get().Status.Conditions, credentialsConditionType)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("CredentialsReady = %+v, want False on a seeded stack", cond)
	}
	for _, want := range []string{"v1-engine-secret/GROQ_API_KEY", "v1-dashboard-secret/STRIPE_SECRET_KEY",
		"v1-index-builder-secret/ABLY_API_KEY", "v1-broker-admin/password"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("CredentialsReady message does not name %s: %s", want, cond.Message)
		}
	}
	if strings.Contains(cond.Message, "dev-") || strings.Contains(cond.Message, "sandbox") {
		t.Errorf("CredentialsReady message leaks a value: %s", cond.Message)
	}
	if get().Status.Phase == "Degraded" {
		t.Error("without requireRealCredentials seeded credentials must not degrade the phase")
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.RequireRealCredentials = true })
	if got := get(); got.Status.Phase != "Degraded" || apimeta.FindStatusCondition(got.Status.Conditions, "Ready").Reason != "CredentialsNotReady" {
		t.Fatalf("requireRealCredentials with seeded credentials: phase %q, conditions %+v", got.Status.Phase, got.Status.Conditions)
	}

	// A missing credential is named too.
	if err := cl.Delete(ctx, getObj(t, cl, "v1-cache-auth", &corev1.Secret{})); err != nil {
		t.Fatal(err)
	}
	findings, err := r.credentialFindings(ctx, get())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(findings, "v1-cache-auth/password") {
		t.Errorf("findings %v do not name the missing v1-cache-auth/password", findings)
	}

	// Replace every named value: the condition turns True and the phase leaves Degraded.
	reconcileAgain(t, cl, r, nil) // recreates v1-cache-auth with a generated password
	cond = apimeta.FindStatusCondition(get().Status.Conditions, credentialsConditionType)
	for _, ref := range strings.Split(strings.TrimPrefix(cond.Message, "seeded dummy or missing: "), ", ") {
		name, key, _ := strings.Cut(ref, "/")
		s := getObj(t, cl, name, &corev1.Secret{})
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data[key] = []byte("real-" + key)
		delete(s.StringData, key)
		if err := cl.Update(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	reconcileAgain(t, cl, r, nil)
	got := get()
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, credentialsConditionType); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("CredentialsReady = %+v after replacing every value, want True", cond)
	}
	if got.Status.Phase == "Degraded" {
		t.Errorf("phase is still Degraded after the credentials were replaced")
	}
}

func TestMasterAPIKeyHasOneSource(t *testing.T) {
	for component := range map[string]bool{"engine": true, "analytics-processor": true, "index-builder": true, "dashboard": true, "workers": true} {
		if _, ok := componentSecretData(component)["MASTER_API_KEY"]; ok {
			t.Errorf("%s Secret still seeds its own MASTER_API_KEY", component)
		}
	}

	check := func(t *testing.T, oc *oramav1alpha1.OramaCloudV1, wantRef string, objs ...client.Object) client.Client {
		cl, _ := reconcileOCV1(t, oc, objs...)
		readers := map[string]bool{}
		visit := func(workload string, pod corev1.PodSpec) {
			for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
				for _, e := range c.Env {
					if e.Name != "MASTER_API_KEY" {
						continue
					}
					readers[workload] = true
					if got := envRef(e); got != wantRef {
						t.Errorf("%s/%s MASTER_API_KEY comes from %q, want %s", workload, c.Name, got, wantRef)
					}
				}
			}
		}
		var deps appsv1.DeploymentList
		var sts appsv1.StatefulSetList
		if err := cl.List(context.Background(), &deps); err != nil {
			t.Fatal(err)
		}
		if err := cl.List(context.Background(), &sts); err != nil {
			t.Fatal(err)
		}
		for _, d := range deps.Items {
			visit(d.Name, d.Spec.Template.Spec)
		}
		for _, s := range sts.Items {
			visit(s.Name, s.Spec.Template.Spec)
		}
		for _, w := range []string{"v1-writer", "v1-reader", "v1-analytics-processor", "v1-index-builder", "v1-dashboard", "v1-workers"} {
			if !readers[w] {
				t.Errorf("%s does not get MASTER_API_KEY", w)
			}
		}
		return cl
	}

	check(t, fullStackOCV1(nil), "v1-master-api-key/MASTER_API_KEY")

	ref := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-key", Namespace: "orama-system"},
		Data: map[string][]byte{"key": []byte("real-master-key")}}
	cl := check(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "prod-key", Key: "key"}
	}), "prod-key/key", ref)
	if !absent(t, cl, "v1-master-api-key", &corev1.Secret{}) {
		t.Error("with masterAPIKeySecretRef set the operator must not create v1-master-api-key")
	}
}
