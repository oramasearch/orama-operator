/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"reflect"
	"regexp"
	"slices"
	"testing"
)

// TestDashboardSchemaTables checks that the vendored dashboard schema creates every control-DB table,
// including the PostgresSchema guard table.
func TestDashboardSchemaTables(t *testing.T) {
	createTable := regexp.MustCompile(`(?m)^CREATE TABLE (?:IF NOT EXISTS )?"([a-z_]+)"`)
	var got []string
	for _, match := range createTable.FindAllStringSubmatch(dashboardSchemaSQL, -1) {
		got = append(got, match[1])
	}
	slices.Sort(got)

	want := []string{
		"crawler_configs", "deleted_indexes", "indexes", "limits_errors", "llms", "oramacore_clusters",
		"pinning_rules_map", "private_api_key", "projects", "stripe_addon_prices", "subscription_items",
		"subscription_plans", "team_invitations", "team_members", "team_migrations", "team_projects",
		"teams", "uploaded_files", "users",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dashboard schema creates %v, want %v", got, want)
	}
	if !slices.Contains(got, controlDBSchemaGuardTable) {
		t.Fatalf("guard table %q is not created by the dashboard schema", controlDBSchemaGuardTable)
	}
}
