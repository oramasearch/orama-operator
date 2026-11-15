/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The credential a tenant receives for a catalog service is never the service's administrator
// (operator#771). These tests read what build writes and what credential hands out.

func leastPrivilegeService(name string) catalogService {
	return catalogService{lake: testWorkspaceLake(), namespace: testWorkspaceName, name: name,
		spec: lakeServiceSpec{Instances: 1, StorageGi: 10}}
}

func deliveredUser(t *testing.T, body []byte) string {
	t.Helper()
	var credential struct {
		User string `json:"user"`
	}
	if err := json.Unmarshal(body, &credential); err != nil {
		t.Fatalf("credential %s: %v", body, err)
	}
	return credential.User
}

func TestValkeyAppUserCannotRunAdminOrDangerousCommands(t *testing.T) {
	in := leastPrivilegeService("prod-svc-aaaaaaaaaaaa")
	resources := (valkeyServiceKind{}).build(in, "secret-password")
	var users []interface{}
	for _, field := range resources[0].fields {
		if reflect.DeepEqual(field.path, []string{"spec", "users"}) {
			users = field.value.([]interface{})
		}
	}
	var app map[string]interface{}
	for _, user := range users {
		u := user.(map[string]interface{})
		switch u["name"] {
		case valkeyUser:
			app = u
		case "default":
			if u["enabled"] != false {
				t.Fatalf("the built-in default user is enabled: %v", u)
			}
		}
	}
	if app == nil {
		t.Fatalf("no %q user in %v", valkeyUser, users)
	}
	commands := app["commands"].(map[string]interface{})
	deny := map[string]bool{}
	for _, category := range commands["deny"].([]interface{}) {
		deny[category.(string)] = true
	}
	if !deny["@admin"] || !deny["@dangerous"] {
		t.Fatalf("app user commands = %v, want @admin and @dangerous denied", commands)
	}
	body, _, err := (valkeyServiceKind{}).credential(context.Background(), nil, in, nil, "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	if user := deliveredUser(t, body); user != valkeyUser || user == "default" {
		t.Fatalf("delivered user = %q, want the restricted %q user", user, valkeyUser)
	}
}

func TestClickHouseAppUserHoldsOnlyItsDatabase(t *testing.T) {
	in := leastPrivilegeService("prod-svc-777777777777")
	var chi map[string]interface{}
	for _, resource := range (clickHouseServiceKind{}).build(in, "secret-password") {
		if resource.gvk != clickHouseInstallationGVK {
			continue
		}
		for _, field := range resource.fields {
			if reflect.DeepEqual(field.path, []string{"spec", "configuration"}) {
				chi = field.value.(map[string]interface{})
			}
		}
	}
	if chi == nil {
		t.Fatal("no ClickHouseInstallation configuration written")
	}
	users := chi["users"].(map[string]interface{})
	grants, _ := users[clickHouseUser+"/grants/query"].([]interface{})
	if len(grants) != 1 || grants[0] != "GRANT ALL ON default.*" {
		t.Fatalf("app grants = %v, want only ALL on its database", grants)
	}
	for key, value := range users {
		if !strings.HasPrefix(key, clickHouseUser+"/") {
			t.Fatalf("configuration defines user setting %s outside the app user", key)
		}
		if strings.Contains(key, "access_management") || strings.Contains(key, "allow_databases") {
			t.Fatalf("app user sets %s = %v", key, value)
		}
		if s, ok := value.(string); ok && strings.Contains(s, "secret-password") {
			t.Fatalf("the password reached the spec at %s", key)
		}
	}
	if users[clickHouseUser+"/profile"] != clickHouseProfile {
		t.Fatalf("app profile = %v, want %q", users[clickHouseUser+"/profile"], clickHouseProfile)
	}
	profiles := chi["profiles"].(map[string]interface{})
	if profiles[clickHouseProfile+"/allow_introspection_functions"] != int64(0) || profiles[clickHouseProfile+"/readonly"] != int64(0) {
		t.Fatalf("app profile settings = %v", profiles)
	}
	body, _, err := (clickHouseServiceKind{}).credential(context.Background(), nil, in, map[string]*unstructured.Unstructured{}, "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	if user := deliveredUser(t, body); user != clickHouseUser || user == "default" || user == "clickhouse_operator" {
		t.Fatalf("delivered user = %q, want the restricted %q user", user, clickHouseUser)
	}
}
