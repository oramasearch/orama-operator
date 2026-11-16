// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// readyBrokerTopology is the RabbitMQTopology <name>-broker as the RabbitMQTopology controller leaves
// it once the broker took every user: Ready for its generation, with the engine users and harvest applied.
// Tests that reconcile a whole stack with an in-cluster broker include it, since no topology
// controller runs in them.
func readyBrokerTopology(name string) *oramav1alpha1.RabbitMQTopology {
	return &oramav1alpha1.RabbitMQTopology{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-broker", Namespace: "orama-system"},
		Spec: oramav1alpha1.RabbitMQTopologySpec{Users: []oramav1alpha1.RabbitMQUser{
			{Name: brokerWriterUser}, {Name: brokerReaderUser}, {Name: brokerHarvestUser},
		}},
		Status: oramav1alpha1.RabbitMQTopologyStatus{Users: 3, Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", LastTransitionTime: metav1.Now()},
		}},
	}
}

// brokerUserOf returns the user a rendered engine config section signs in to the broker as.
func brokerUserOf(t *testing.T, doc, section, key string) string {
	t.Helper()
	sec, _ := parseYAML(t, doc)[section].(map[string]any)
	io, _ := sec[key].(map[string]any)
	user, _ := io["user"].(string)
	return user
}

// The writer signs in as oramacore-writer and every reader pod, of either image, as oramacore-reader,
// in every section of its config (#99, #795): the writer a combined reader starts produces as
// oramacore-reader too, to its own stream. Each takes its own password from <cr>-broker-users; no
// engine pod reads the broker admin Secret.
func TestEngineSignsInAsItsOwnBrokerUser(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		mut                    func(*oramav1alpha1.OramaCloudV1)
		writerUser, readerUser string
	}{
		{"side-only images", withSideImages, brokerWriterUser, brokerReaderUser},
		{"combined image", nil, brokerWriterUser, brokerReaderUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngineHarness(t, stackOC(tc.mut), engineSecret())
			h.mustRun()
			cm := h.engineConfig()
			for side, want := range map[string]string{"writer": tc.writerUser, "reader": tc.readerUser} {
				s := engineSideFor(stackOC(tc.mut), side)
				doc := cm.Data[s.configKey]
				if strings.Contains(doc, "__BROKER_USERNAME__") {
					t.Errorf("%s config still takes the broker user from a placeholder", side)
				}
				if section, key := "writer_side", "output"; strings.Contains(doc, section+":") {
					if got := brokerUserOf(t, doc, section, key); got != want {
						t.Errorf("%s config writer_side.output.user = %q, want %q", side, got, want)
					}
				}
				if section, key := "reader_side", "input"; strings.Contains(doc, section+":") {
					if got := brokerUserOf(t, doc, section, key); got != want {
						t.Errorf("%s config reader_side.input.user = %q, want %q", side, got, want)
					}
				}
				tmpl := h.templates()[side]
				init := containerIn(t, tmpl.Spec, "render-config")
				wantEnv := secretKeyEnv("BROKER_PASSWORD", "v1-broker-users", brokerUserPasswordKey(want))
				if !slices.ContainsFunc(init.Env, func(e corev1.EnvVar) bool { return reflect.DeepEqual(e, wantEnv) }) {
					t.Errorf("%s render-config lacks BROKER_PASSWORD from v1-broker-users %s", side, brokerUserPasswordKey(want))
				}
				for _, c := range append(tmpl.Spec.InitContainers, tmpl.Spec.Containers...) {
					for _, e := range c.Env {
						if e.Name == "BROKER_USERNAME" || (e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == "v1-broker-admin") {
							t.Errorf("%s container %s reads %s from the broker admin Secret", side, c.Name, e.Name)
						}
					}
				}
			}
		})
	}
}

// The rendered passwords are the broker-users values, substituted by the init container's own script.
func TestEngineBrokerPasswordRenders(t *testing.T) {
	h := newEngineHarness(t, stackOC(withSideImages), engineSecret())
	h.mustRun()
	users := h.secret("v1-broker-users")
	cm := h.engineConfig()
	for side, user := range map[string]string{"writer": brokerWriterUser, "reader": brokerReaderUser} {
		tmpl := h.templates()[side]
		init := containerIn(t, tmpl.Spec, "render-config")
		secrets := map[string]*corev1.Secret{"v1-broker-users": users, testMasterSecret: h.secret(testMasterSecret),
			"v1-analytics-api-key": h.secret("v1-analytics-api-key"), "v1-engine-secret": h.secret("v1-engine-secret")}
		env := expandEnv(t, init.Env, secrets)
		if env["BROKER_PASSWORD"] != string(users.Data[brokerUserPasswordKey(user)]) {
			t.Errorf("%s BROKER_PASSWORD is not %s's password", side, user)
		}
		doc := cm.Data[engineSideFor(stackOC(withSideImages), side).configKey]
		rendered := strings.ReplaceAll(doc, "'__BROKER_PASSWORD__'", "'"+env["BROKER_PASSWORD"]+"'")
		if strings.Contains(rendered, "__BROKER_PASSWORD__") || !strings.Contains(renderConfigScript(stackOC(withSideImages), engineSideFor(stackOC(withSideImages), side)), "__BROKER_PASSWORD__") {
			t.Errorf("%s render-config does not substitute __BROKER_PASSWORD__", side)
		}
	}
}

// Until the topology reports the engine's users on the broker, no engine object is written; a running
// engine keeps its templates while the topology is not Ready, and is written again once it is.
func TestEngineWaitsForBrokerUsers(t *testing.T) {
	notReady := readyBrokerTopology("v1")
	notReady.Status.Users = 0
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	notReady.Status.Conditions[0].Reason = "BrokerUnavailable"
	h := newEngineHarness(t, stackOC(withSideImages), engineSecret(), notReady)
	h.mustRun()
	if h.get("v1-engine-config", &corev1.ConfigMap{}) {
		t.Fatal("the engine config was written before the broker users exist")
	}
	if h.get("v1-writer", &appsv1.StatefulSet{}) || h.get("v1-reader", &appsv1.StatefulSet{}) {
		t.Fatal("the engine was written before the broker users exist")
	}

	topo := &oramav1alpha1.RabbitMQTopology{}
	if !h.get("v1-broker", topo) {
		t.Fatal("no topology")
	}
	topo.Status.Users = 3
	apimeta.SetStatusCondition(&topo.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled"})
	if err := h.cl.Update(context.Background(), topo); err != nil {
		t.Fatal(err)
	}
	h.mustRun()
	before := h.templates()
	if _, ok := before["writer"]; !ok {
		t.Fatal("the writer was not written once the broker users exist")
	}

	// The broker goes away: the running engine is left as it is, even when its spec changes.
	topo = &oramav1alpha1.RabbitMQTopology{}
	h.get("v1-broker", topo)
	apimeta.SetStatusCondition(&topo.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "BrokerUnavailable"})
	if err := h.cl.Update(context.Background(), topo); err != nil {
		t.Fatal(err)
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Replicas = 5 })
	h.mustRun()
	if r := h.reader(); r.Spec.Replicas != nil && *r.Spec.Replicas == 5 {
		t.Error("the readers were changed while the broker users were not confirmed")
	}
	if !reflect.DeepEqual(before["writer"], h.templates()["writer"]) {
		t.Error("the writer template changed while the broker users were not confirmed")
	}
}

// A stack whose broker runs elsewhere has no topology here and does not wait for one.
func TestEngineDoesNotWaitWithoutLocalBroker(t *testing.T) {
	h := newEngineHarness(t, engineOC(withSideImages), engineSecret())
	h.mustRun()
	if _, ok := h.templates()["writer"]; !ok {
		t.Fatal("the writer waited for a broker that does not run here")
	}
}

// rmqAction is one thing an engine user does on the broker: an operation on a resource name in /.
type rmqAction struct{ op, resource string }

// rmqAllowed applies RabbitMQ's authorization rule for the internal backend: the user's permission
// on the vhost holds a regular expression per operation, matched against the resource name; "^$"
// matches nothing a client can name.
func rmqAllowed(t *testing.T, perms []oramav1alpha1.RabbitMQPermission, user string, a rmqAction) bool {
	t.Helper()
	for _, p := range perms {
		if p.User != user || p.VHost != "/" {
			continue
		}
		re := map[string]string{"configure": p.Configure, "write": p.Write, "read": p.Read}[a.op]
		if re == "" {
			re = "^$"
		}
		return regexp.MustCompile(re).MatchString(a.resource)
	}
	return false
}

// Each engine user may do what OramaCore does with it and nothing else (principle 4). OramaCore's
// writer declares the stream with its stream creator (configure) and publishes to it (write); a
// reader declares it the same way and consumes it (read), with no server-side offset tracking
// (oramacore src/collection_manager/sides/operation/rabbit.rs: create_consumer has no consumer name).
func TestBrokerUserPermissionsAreLeastPrivilege(t *testing.T) {
	perms := brokerUserPermissions()
	stream := brokerOperationsStream
	local := brokerReaderLocalStream
	needs := map[string][]rmqAction{
		brokerWriterUser: {{"configure", stream}, {"write", stream}, {"read", stream}},
		// The combined image's readers also declare and produce to oramacore-reader-local (#795).
		brokerReaderUser: {{"configure", stream}, {"read", stream}, {"configure", local}, {"write", local}},
	}
	refused := map[string][]rmqAction{
		brokerWriterUser: {{"configure", local}, {"write", local}, {"read", local}},
		brokerReaderUser: {{"write", stream}, {"read", local}},
	}
	// Neither may reach any other queue or exchange: HARVEST's queues, the default and amq.* exchanges,
	// a name that only contains the stream's, or a name that a regexp without anchors would match.
	for _, user := range []string{brokerWriterUser, brokerReaderUser} {
		// The default exchange "" is authorized as amq.default (RabbitMQ access control docs).
		for _, res := range []string{"abc-123-queue", "amq.default", "amq.topic", "oramacore-operations-2",
			"x-oramacore-operations", "oramacore-operationsx", "oramacore_operations"} {
			for _, op := range []string{"configure", "write", "read"} {
				refused[user] = append(refused[user], rmqAction{op, res})
			}
		}
	}
	for user, actions := range needs {
		for _, a := range actions {
			if !rmqAllowed(t, perms, user, a) {
				t.Errorf("%s may not %s %q, which OramaCore needs", user, a.op, a.resource)
			}
		}
	}
	for user, actions := range refused {
		for _, a := range actions {
			if rmqAllowed(t, perms, user, a) {
				t.Errorf("%s may %s %q", user, a.op, a.resource)
			}
		}
	}
	// The users the configs name are the ones the topology declares, with these permissions.
	h := newEngineHarness(t, stackOC(withSideImages), engineSecret())
	h.mustRun()
	topo := &oramav1alpha1.RabbitMQTopology{}
	if !h.get("v1-broker", topo) {
		t.Fatal("no topology")
	}
	if !reflect.DeepEqual(topo.Spec.Permissions, perms) {
		t.Errorf("topology permissions = %+v, want %+v", topo.Spec.Permissions, perms)
	}
	for _, u := range topo.Spec.Users {
		if u.Name == brokerHarvestUser {
			continue // least_privilege_test.go
		}
		if u.PasswordSecretRef.Name != "v1-broker-users" || u.PasswordSecretRef.Key != brokerUserPasswordKey(u.Name) || len(u.Tags) != 0 {
			t.Errorf("topology user %+v is not untagged with its password in v1-broker-users", u)
		}
	}
}

// A changed broker user password rolls the engine sides that read it, and only the engine.
func TestBrokerUserPasswordChangeRollsTheEngine(t *testing.T) {
	h := newEngineHarness(t, stackOC(withSideImages), engineSecret())
	h.mustRun()
	before := h.templates()
	h.setSecret("v1-broker-users", brokerUserPasswordKey(brokerReaderUser), "rotatedreader0123")
	h.mustRun()
	after := h.templates()
	for comp := range after {
		changed := before[comp].Annotations[credentialsHashKey] != after[comp].Annotations[credentialsHashKey]
		if want := comp == "writer" || comp == "reader"; changed != want {
			t.Errorf("%s rolled = %v on a broker user password change, want %v", comp, changed, want)
		}
	}
}
