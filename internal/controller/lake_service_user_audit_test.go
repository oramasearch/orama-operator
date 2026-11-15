/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bufio"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeUserListers records the operator's listings of Valkey and Kafka users (operator#1172).
type fakeUserListers struct {
	err     error
	valkey  []string
	kafka   []string
	calls   int
	lastCAs []string
	users   []string
}

func (f *fakeUserListers) valkeyACLUsers(_ context.Context, _ catalogService, user, _ string) ([]string, error) {
	f.calls++
	f.users = append(f.users, user)
	return valkeyUnmanagedUsers([][]string{f.valkey}), f.err
}

func (f *fakeUserListers) kafkaSCRAMUsers(_ context.Context, _ catalogService, user, _, caPEM string) ([]string, error) {
	f.calls++
	f.users = append(f.users, user)
	f.lastCAs = append(f.lastCAs, caPEM)
	return f.kafka, f.err
}

func auditCatalogCase(t *testing.T, kind string) catalogCase {
	t.Helper()
	for _, c := range catalogCases() {
		if c.kind == kind {
			return c
		}
	}
	t.Fatalf("no catalog case for %s", kind)
	return catalogCase{}
}

// readyAuditedService brings c's service to Ready with the given users in the store.
func readyAuditedService(t *testing.T, c catalogCase, valkey, kafka []string, err error) (*serviceHarness, *fakeServiceStore) {
	t.Helper()
	store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3,"storage_gi":15}`), false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.userListers.valkey, h.userListers.kafka, h.userListers.err = valkey, kafka, err
	h.poll()
	h.giveUIDs(c)
	if c.markPartlyReady != nil {
		c.markPartlyReady(h, c)
	}
	c.markReady(h, c)
	if c.upstreamSecret != nil {
		c.upstreamSecret(h, c)
	}
	h.poll()
	return h, store
}

// Users the operator did not make are listed once with its own identity, recorded, reported in
// the status message, and never dropped; a steady service makes no further call.
func TestUnmanagedValkeyAndKafkaUsersAreReportedOnce(t *testing.T) {
	for _, tc := range []struct {
		kind, identity string
		valkey, kafka  []string
	}{
		{serviceKindValkey, valkeyOperatorUser, []string{"default", "app", "orama_operator", "_operator", "_exporter", "_replication", "_operator_acl_revision", "backdoor"}, nil},
		{serviceKindKafka, "prod-svc-666666666666-operator", nil, []string{"prod-svc-666666666666-app", "prod-svc-666666666666-operator", "backdoor"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			c := auditCatalogCase(t, tc.kind)
			h, store := readyAuditedService(t, c, tc.valkey, tc.kafka, nil)
			// The Kafka's KafkaUsers have no status in the fake, so they are matched by name.
			want := "service ready; users and roles the operator did not make: backdoor"
			expectStatus(t, store.status(t, c.id), ServicePhaseReady, want, c.endpoint)
			if h.userListers.calls != 1 || h.userListers.users[0] != tc.identity {
				t.Fatalf("listings = %d as %v", h.userListers.calls, h.userListers.users)
			}
			if tc.kind == serviceKindKafka && h.userListers.lastCAs[0] != "cluster-ca-pem" {
				t.Fatalf("listed without the cluster CA: %q", h.userListers.lastCAs[0])
			}
			secret := h.secret(c.operatorSecret)
			if secret.Annotations[serviceUsersAuditedAnnotation] == "" || secret.Annotations[serviceAuditedUsersAnnotation] != "backdoor" {
				t.Fatalf("record = %v", secret.Annotations)
			}
			h.poll()
			h.poll()
			if h.userListers.calls != 1 {
				t.Fatalf("a steady service listed its users again: %d", h.userListers.calls)
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseReady, want, c.endpoint)
		})
	}
}

// A listing that fails holds back nothing and is tried again.
func TestAFailedUserListingIsRetried(t *testing.T) {
	c := auditCatalogCase(t, serviceKindValkey)
	h, store := readyAuditedService(t, c, nil, nil, errors.New("connection refused"))
	expectStatus(t, store.status(t, c.id), ServicePhaseReady, "service ready", c.endpoint)
	if _, found := h.credentials()[c.slug+".json"]; !found {
		t.Fatal("a failed listing held back the connection")
	}
	if h.secret(c.operatorSecret).Annotations[serviceUsersAuditedAnnotation] != "" {
		t.Fatal("a failed listing was recorded")
	}
	h.userListers.err = nil
	h.poll()
	if h.userListers.calls != 2 || h.secret(c.operatorSecret).Annotations[serviceUsersAuditedAnnotation] == "" {
		t.Fatalf("not retried: %d calls", h.userListers.calls)
	}
}

// The operator's identities are least privilege: Valkey's may only run ACL USERS, Kafka's may only
// describe the cluster.
func TestTheOperatorsListingIdentitiesAreLeastPrivilege(t *testing.T) {
	users := valkeyUsers("x-credentials", "x-operator")
	var operator map[string]interface{}
	for _, user := range users {
		if u := user.(map[string]interface{}); u["name"] == valkeyOperatorUser {
			operator = u
		}
	}
	if operator == nil {
		t.Fatalf("no %s user in %v", valkeyOperatorUser, users)
	}
	if want := map[string]interface{}{"allow": []interface{}{"acl|users"}}; !reflect.DeepEqual(operator["commands"], want) {
		t.Fatalf("operator commands = %v", operator["commands"])
	}
	if _, found := operator["keys"]; found {
		t.Fatal("the operator's Valkey user can reach keys")
	}
	if _, found := operator["channels"]; found {
		t.Fatal("the operator's Valkey user can reach channels")
	}

	in := workspaceKafkaService()
	objects := builtKafkaObjects(t, in)
	user := objects["KafkaUser/"+in.name+kafkaOperatorUserSuffix]
	if user == nil {
		t.Fatalf("no operator KafkaUser in %v", objects)
	}
	acls, _, _ := unstructured.NestedSlice(user.Object, "spec", "authorization", "acls")
	want := []interface{}{map[string]interface{}{"type": "allow", "resource": map[string]interface{}{"type": "cluster"}, "operations": []interface{}{"Describe"}}}
	if !reflect.DeepEqual(acls, want) {
		t.Fatalf("operator KafkaUser ACLs = %v", acls)
	}
	secretName, _, _ := unstructured.NestedString(user.Object, "spec", "authentication", "password", "valueFrom", "secretKeyRef", "name")
	if secretName != in.name+"-operator-credentials" {
		t.Fatalf("operator KafkaUser password Secret = %q", secretName)
	}
	if app := objects["KafkaUser"]; app == nil || app.GetName() != in.name+kafkaUserSuffix {
		t.Fatal("the tenant's KafkaUser is not the first KafkaUser")
	}
}

func TestUnmanagedUserFilters(t *testing.T) {
	if got := valkeyUnmanagedUsers([][]string{{"default", "app", "b"}, {"a", "b", "_exporter"}}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("valkey = %v", got)
	}
	if got := kafkaUnmanagedUsers([]string{"z", "mine", "a"}, map[string]bool{"mine": true}); !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("kafka = %v", got)
	}
}

func TestRESPReplies(t *testing.T) {
	if got := string(respCommand("ACL", "USERS")); got != "*2\r\n$3\r\nACL\r\n$5\r\nUSERS\r\n" {
		t.Fatalf("command = %q", got)
	}
	reply, err := readRESP(bufio.NewReader(strings.NewReader("*3\r\n$7\r\ndefault\r\n$3\r\napp\r\n$0\r\n\r\n")))
	if err != nil || !reflect.DeepEqual(reply, []string{"default", "app", ""}) {
		t.Fatalf("array = %v, %v", reply, err)
	}
	if reply, err := readRESP(bufio.NewReader(strings.NewReader("+OK\r\n"))); err != nil || reply != "OK" {
		t.Fatalf("simple = %v, %v", reply, err)
	}
	if _, err := readRESP(bufio.NewReader(strings.NewReader("-WRONGPASS invalid username-password pair\r\n"))); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("error reply = %v", err)
	}
	if _, err := readRESP(bufio.NewReader(strings.NewReader("*99999999999\r\n"))); err == nil {
		t.Fatal("an unbounded array was accepted")
	}
}
