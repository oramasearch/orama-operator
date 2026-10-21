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
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// secretsReferenced is every Secret a pod template names: env and envFrom of every container, init
// containers included, and secret volumes.
func secretsReferenced(spec corev1.PodSpec) map[string]bool {
	got := map[string]bool{}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				got[e.ValueFrom.SecretKeyRef.Name] = true
			}
		}
		for _, f := range c.EnvFrom {
			if f.SecretRef != nil {
				got[f.SecretRef.Name] = true
			}
		}
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil {
			got[v.Secret.SecretName] = true
		}
	}
	return got
}

// renderReader runs the reader pod's render-config script the way the init container does, with env
// as its environment, and returns the rendered config.yaml.
func renderReader(t *testing.T, h *engineHarness, env map[string]string) string {
	t.Helper()
	spec := h.reader().Spec.Template.Spec
	cm := h.engineConfig()
	dir := t.TempDir()
	tmpl, out := filepath.Join(dir, "tmpl"), filepath.Join(dir, "etc")
	for _, d := range []string{tmpl, out} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range cm.Data {
		if err := os.WriteFile(filepath.Join(tmpl, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.NewReplacer("/tmpl/", tmpl+"/", "/etc/oramacore/", out+"/").Replace(renderInit(t, spec).Command[2])
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reader render-config: %v: %s", err, b)
	}
	b, err := os.ReadFile(filepath.Join(out, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// LSR-133 (#795): no reader holds the writer's master key, in either image mode. The reader-only
// config has no writer_side at all; the combined image, which must start a writer in every pod, gets
// a writer_side whose master key is generated at pod start and known to nothing else. The reader's
// render-config reads no master key Secret, and masks a MASTER_API_KEY an older engine Secret still
// carries (engineKeys leaves one there), so not even the init container's environment holds it.
func TestReaderNeverHoldsTheWriterMasterKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*oramav1alpha1.OramaCloudV1)
	}{
		{"combined image", nil},
		{"combined image with Envoy and writer JWT", func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Envoy = true
			o.Spec.JWTIssuer = testJWTIssuer
			o.Spec.Components.Engine.Writer.JWT = &oramav1alpha1.WriterJWTSpec{Audiences: []string{"https://writer.example.invalid"}}
		}},
		{"side-only images", withSideImages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngineHarness(t, stackOC(tc.mut), engineSecret())
			h.mustRun()
			oc := h.stored()
			reader := h.reader().Spec.Template
			key := renderSource(t, reader.Spec)
			tmpl := h.engineConfig().Data[key]
			if strings.Contains(tmpl, keyPlaceholder(masterAPIKeyEnvName)) {
				t.Errorf("the readers' template %s holds the master key placeholder", key)
			}
			if engineSideFor(oc, "reader").readsMasterAPIKey() {
				t.Error("the reader side reads the master key")
			}
			if secretsReferenced(reader.Spec)[testMasterSecret] {
				t.Errorf("a reader container reads the master key Secret %s", testMasterSecret)
			}
			init := renderInit(t, reader.Spec)
			masked := false
			for _, e := range init.Env {
				if e.Name == masterAPIKeyEnvName {
					masked = e.ValueFrom == nil && e.Value == ""
				}
			}
			if !masked {
				t.Error("the reader's render-config does not mask MASTER_API_KEY from the engine Secret's envFrom")
			}

			// Rendered with the master key in the environment anyway, it is still nowhere in the file.
			env := map[string]string{masterAPIKeyEnvName: testMasterKey, brokerPasswordEnv: "synthetic-broker-89ab",
				analyticsKeyEnvName: "synthetic-analytics-cdef"}
			for k, v := range engineKeys {
				if k != masterAPIKeyEnvName {
					env[k] = v
				}
			}
			first := renderReader(t, h, env)
			if strings.Contains(first, testMasterKey) || strings.Contains(first, "__") {
				t.Fatalf("the rendered reader config holds the master key or an unfilled placeholder:\n%s", first)
			}
			ws, ok := parseYAML(t, first)["writer_side"].(map[string]any)
			if engineSidesSeparated(oc) {
				if ok {
					t.Error("the reader-only config has a writer_side")
				}
				return
			}
			if !ok {
				t.Fatal("the combined reader config has no writer_side, which the combined image requires")
			}
			local, _ := ws["master_api_key"].(string)
			if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(local) {
				t.Errorf("the combined reader's writer master key %q is not 48 generated hex characters", local)
			}
			second, _ := parseYAML(t, renderReader(t, h, env))["writer_side"].(map[string]any)
			if second["master_api_key"] == local {
				t.Error("two reader starts generated the same writer master key")
			}
			if _, ok := ws["jwt"]; ok {
				t.Error("the combined reader's writer accepts the dashboard's JWTs")
			}
		})
	}
}

// LSR-133: a master key change rolls the writer and the master key's other consumers, never a reader,
// because no reader reads it.
func TestMasterKeyChangeDoesNotRollTheReaders(t *testing.T) {
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined image": nil, "side-only images": withSideImages} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, stackOC(mut), engineSecret())
			h.mustRun()
			before := h.templates()
			h.setSecret(testMasterSecret, masterAPIKeyEnvName, "rotated-master-0123")
			h.mustRun()
			after := h.templates()
			if before["reader"].Annotations[credentialsHashKey] != after["reader"].Annotations[credentialsHashKey] {
				t.Error("a master key change rolled the readers")
			}
			if before["writer"].Annotations[credentialsHashKey] == after["writer"].Annotations[credentialsHashKey] {
				t.Error("a master key change did not roll the writer")
			}
		})
	}
}

// The combined image's readers sign in as oramacore-reader in both sections. Their writer, which
// the combined image starts and nothing addresses, produces to its own stream, oramacore-reader-local,
// which no reader consumes; the replication stream stays the writer's alone. The writer pod's config
// is unchanged: it is still the combined config, as oramacore-writer.
func TestCombinedReaderCannotPublishToTheReplicationStream(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	cm := h.engineConfig()
	reader := parseYAML(t, cm.Data[renderSource(t, h.reader().Spec.Template.Spec)])
	out, _ := reader["writer_side"].(map[string]any)["output"].(map[string]any)
	in, _ := reader["reader_side"].(map[string]any)["input"].(map[string]any)
	if out["stream_name"] != brokerReaderLocalStream || out["user"] != brokerReaderUser {
		t.Errorf("combined reader writer_side.output = %v as %v, want %s as %s", out["stream_name"], out["user"], brokerReaderLocalStream, brokerReaderUser)
	}
	if in["stream_name"] != brokerOperationsStream || in["user"] != brokerReaderUser {
		t.Errorf("combined reader reader_side.input = %v as %v, want %s as %s", in["stream_name"], in["user"], brokerOperationsStream, brokerReaderUser)
	}
	if out["client_provided_name"] == "oramacore-writer" {
		t.Error("the combined reader's producer names itself oramacore-writer")
	}
	if renderSource(t, h.writer().Spec.Template.Spec) != combinedConfigKey || cm.Data[combinedConfigKey] != combinedConfigFixture {
		t.Error("the combined writer no longer renders the unchanged combined config")
	}

	perms := brokerUserPermissions()
	if rmqAllowed(t, perms, brokerReaderUser, rmqAction{"write", brokerOperationsStream}) {
		t.Error("oramacore-reader may publish to the replication stream")
	}
	for _, a := range []rmqAction{{"configure", brokerReaderLocalStream}, {"write", brokerReaderLocalStream},
		{"configure", brokerOperationsStream}, {"read", brokerOperationsStream}} {
		if !rmqAllowed(t, perms, brokerReaderUser, a) {
			t.Errorf("oramacore-reader may not %s %s, which the combined reader needs", a.op, a.resource)
		}
	}

	// The topology declares the reader-local stream with OramaCore's own arguments, so neither side's
	// declare conflicts with the other's.
	topo := &oramav1alpha1.RabbitMQTopology{}
	if !h.get("v1-broker", topo) {
		t.Fatal("no topology")
	}
	streams := map[string]oramav1alpha1.RabbitMQStream{}
	for _, s := range topo.Spec.Streams {
		streams[s.Name] = s
	}
	for _, name := range []string{brokerOperationsStream, brokerReaderLocalStream} {
		s, ok := streams[name]
		if !ok || s.VHost != "/" || s.MaxLengthBytes != brokerOperationsStreamMaxBytes || s.LeaderLocator != brokerOperationsStreamLeaderLocator {
			t.Errorf("topology stream %s = %+v, want OramaCore's arguments", name, s)
		}
	}
}

// LSR-134 (#796, #824): only the broker reads <cr>-broker-admin. The engine signs in as its own users
// (#99) and harvest-master as harvest, whose password is its own Secret.
func TestOnlyTheBrokerReadsTheBrokerAdmin(t *testing.T) {
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined image": nil, "side-only images": withSideImages} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, stackOC(mut), engineSecret())
			h.mustRun()
			tmpls := h.templates()
			if _, ok := tmpls["harvest-master"]; !ok {
				t.Fatal("no harvest-master")
			}
			for comp, tmpl := range tmpls {
				if secretsReferenced(tmpl.Spec)["v1-broker-admin"] != (comp == "broker") {
					t.Errorf("%s reads v1-broker-admin = %v, want %v", comp, !(comp == "broker"), comp == "broker")
				}
			}
		})
	}
}

// harvest-master signs in as harvest with the password in <cr>-broker-harvest; its AMQP URL expands
// to those credentials, and only a change to that Secret (or the cache's) rolls it.
func TestHarvestMasterSignsInAsHarvest(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	harvest, cache := h.secret("v1-broker-harvest"), h.secret("v1-cache-auth")
	tmpl := h.templates()["harvest-master"]
	env := expandEnv(t, containerIn(t, tmpl.Spec, "harvest-master").Env, map[string]*corev1.Secret{harvest.Name: harvest, cache.Name: cache})
	if want := "amqp://" + brokerHarvestUser + ":" + string(harvest.Data["password"]) + "@v1-broker:5672"; env["AMQP_HOST"] != want {
		t.Errorf("harvest-master AMQP_HOST does not expand to the harvest user's credentials")
	}
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).Match(harvest.Data["password"]) {
		t.Error("the harvest password is not 48 generated hex characters")
	}
	if want := secretDataHash(harvest, cache); tmpl.Annotations[credentialsHashKey] != want {
		t.Error("harvest-master's credentials hash is not its broker and cache Secrets'")
	}

	before := h.templates()
	h.setSecret("v1-broker-users", brokerUserPasswordKey(brokerWriterUser), "rotatedwriter0123")
	h.mustRun()
	if h.templates()["harvest-master"].Annotations[credentialsHashKey] != before["harvest-master"].Annotations[credentialsHashKey] {
		t.Error("an engine broker password change rolled harvest-master")
	}
	before = h.templates()
	h.setSecret("v1-broker-harvest", "password", "rotatedharvest0123")
	h.mustRun()
	after := h.templates()
	for comp := range after {
		changed := before[comp].Annotations[credentialsHashKey] != after[comp].Annotations[credentialsHashKey]
		if want := comp == "harvest-master"; changed != want {
			t.Errorf("%s rolled = %v on a harvest password change, want %v", comp, changed, want)
		}
	}
}

// The topology declares harvest untagged with its password in <cr>-broker-harvest, and harvest may
// do what HARVEST does on the broker and nothing else: declare, publish to through the default
// exchange, consume and delete its per-crawl queues, <workerID>-<indexID>-queue
// (HARVEST worker/src/models/scrapping_metadata.py), never touch either OramaCore stream.
func TestHarvestBrokerUserIsLeastPrivilege(t *testing.T) {
	perms := brokerUserPermissions()
	queue := "4f1c2d3e-5a6b-7c8d-9e0f-a1b2c3d4e5f6-cm1x2y3z4-queue"
	for _, a := range []rmqAction{{"configure", queue}, {"write", queue}, {"read", queue}, {"write", "amq.default"}} {
		if !rmqAllowed(t, perms, brokerHarvestUser, a) {
			t.Errorf("harvest may not %s %q, which HARVEST needs", a.op, a.resource)
		}
	}
	for _, res := range []string{brokerOperationsStream, brokerReaderLocalStream, "amq.topic", "amq.direct", "queue",
		"-queue", "a-queue-x", "a b-c-queue", "a-b-queue\n"} {
		for _, op := range []string{"configure", "write", "read"} {
			if rmqAllowed(t, perms, brokerHarvestUser, rmqAction{op, res}) {
				t.Errorf("harvest may %s %q", op, res)
			}
		}
	}
	for _, op := range []string{"configure", "read"} {
		if rmqAllowed(t, perms, brokerHarvestUser, rmqAction{op, "amq.default"}) {
			t.Errorf("harvest may %s amq.default", op)
		}
	}
	// Neither engine user may reach HARVEST's queues.
	for _, user := range []string{brokerWriterUser, brokerReaderUser} {
		for _, op := range []string{"configure", "write", "read"} {
			if rmqAllowed(t, perms, user, rmqAction{op, queue}) {
				t.Errorf("%s may %s a HARVEST queue", user, op)
			}
		}
	}

	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	topo := &oramav1alpha1.RabbitMQTopology{}
	if !h.get("v1-broker", topo) {
		t.Fatal("no topology")
	}
	var found bool
	for _, u := range topo.Spec.Users {
		if u.Name == brokerHarvestUser {
			found = true
			if u.PasswordSecretRef.Name != "v1-broker-harvest" || u.PasswordSecretRef.Key != "password" || len(u.Tags) != 0 {
				t.Errorf("topology user harvest = %+v, want untagged with its password in v1-broker-harvest", u)
			}
		}
	}
	if !found {
		t.Error("the topology declares no harvest user")
	}
}

// Rolling this onto a running stack changes no existing password: the broker admin, the engine
// users' and the cache's Secrets keep their values, and the harvest Secret is new.
func TestLeastPrivilegeChangesNoExistingPassword(t *testing.T) {
	existing := []*corev1.Secret{
		secretOf("v1-broker-admin", map[string]string{"username": "orama", "password": "existingadmin0123"}),
		secretOf("v1-broker-users", map[string]string{"writer-password": "existingwriter0123", "reader-password": "existingreader0123"}),
		secretOf("v1-cache-auth", map[string]string{"password": "existingcache0123"}),
	}
	h := newEngineHarness(t, stackOC(nil), engineSecret(), existing[0], existing[1], existing[2])
	h.mustRun()
	for _, want := range existing {
		got := h.secret(want.Name)
		if len(got.Data) != len(want.Data) {
			t.Errorf("Secret %s keys changed: %d, want %d", want.Name, len(got.Data), len(want.Data))
		}
		for k, v := range want.Data {
			if string(got.Data[k]) != string(v) {
				t.Errorf("Secret %s key %s changed", want.Name, k)
			}
		}
	}
	if len(h.secret("v1-broker-harvest").Data["password"]) == 0 {
		t.Error("no harvest password")
	}
}

// harvest-master is not written until the topology reports every user on the broker, harvest
// included; a running one keeps its template meanwhile.
func TestHarvestMasterWaitsForItsBrokerUser(t *testing.T) {
	notReady := readyBrokerTopology("v1")
	notReady.Status.Users = 0
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	notReady.Status.Conditions[0].Reason = "BrokerUnavailable"
	h := newEngineHarness(t, stackOC(nil), engineSecret(), notReady)
	h.mustRun()
	if h.get("v1-harvest-master", &appsv1.Deployment{}) {
		t.Fatal("harvest-master was written before its broker user exists")
	}
	topo := &oramav1alpha1.RabbitMQTopology{}
	h.get("v1-broker", topo)
	topo.Status = readyBrokerTopology("v1").Status
	if err := h.cl.Update(context.Background(), topo); err != nil {
		t.Fatal(err)
	}
	h.mustRun()
	if !h.get("v1-harvest-master", &appsv1.Deployment{}) {
		t.Fatal("harvest-master was not written once its broker user exists")
	}
}
