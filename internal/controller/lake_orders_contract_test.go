/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fixtureTables reads each CREATE TABLE in the cloud_orders fixture (lake's 0089 migration) and
// returns its columns: the lines inside the table indented four spaces and starting with a name.
func fixtureTables(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("testdata/lake-cloud-orders.sql")
	if err != nil {
		t.Fatal(err)
	}
	create := regexp.MustCompile(`^CREATE TABLE IF NOT EXISTS ([a-z_]+) \($`)
	column := regexp.MustCompile(`^    ([a-z_][a-z0-9_]*)\s`)
	// 0097 adds columns with ALTER TABLE and defines the view the operator reads; the view's own
	// columns are cloud_orders' and the two it computes (`END AS <name>`).
	alter := regexp.MustCompile(`^ALTER TABLE ([a-z_]+) ADD COLUMN IF NOT EXISTS ([a-z_][a-z0-9_]*)\s`)
	view := regexp.MustCompile(`^CREATE OR REPLACE VIEW ([a-z_]+) AS$`)
	computed := regexp.MustCompile(`END AS ([a-z_]+)`)
	tables := map[string]map[string]bool{}
	var current map[string]bool
	var viewName string
	for _, line := range strings.Split(string(raw), "\n") {
		if m := alter.FindStringSubmatch(line); m != nil && tables[m[1]] != nil {
			tables[m[1]][m[2]] = true
			continue
		}
		if m := view.FindStringSubmatch(line); m != nil {
			viewName = m[1]
			tables[viewName] = map[string]bool{}
			continue
		}
		if viewName != "" {
			if m := computed.FindStringSubmatch(line); m != nil {
				tables[viewName][m[1]] = true
			}
		}
		if m := create.FindStringSubmatch(line); m != nil {
			current = map[string]bool{}
			tables[m[1]] = current
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(line, ")") {
			current = nil
			continue
		}
		if m := column.FindStringSubmatch(line); m != nil && m[1] != "constraint" {
			current[m[1]] = true
		}
	}
	if viewName != "" {
		for c := range tables["cloud_orders"] {
			tables[viewName][c] = true
		}
	}
	return tables
}

// sqlKeywords are the words in the store's SQL that are not a table or a column: SQL itself, the
// functions and types it calls, and the CTE names it defines.
var sqlKeywords = map[string]bool{
	"select": true, "from": true, "where": true, "or": true, "and": true, "in": true, "order": true,
	"by": true, "with": true, "as": true, "update": true, "set": true, "returning": true,
	"insert": true, "into": true, "is": true, "not": true, "null": true, "now": true,
	"coalesce": true, "to_regclass": true, "text": true, "uuid": true,
	"exists":  true,
	"claimed": true, "moved": true, // CTE names
}

// TestCloudOrdersFixtureMatchesTheStoresSQL fails when the store's SQL names a table or column
// that lake's cloud_orders migration (the fixture, copied from lake) does not have, or when the
// fixture loses a table or a column the operator relies on. It needs no database.
func TestCloudOrdersFixtureMatchesTheStoresSQL(t *testing.T) {
	tables := fixtureTables(t)
	for _, name := range []string{"cloud_orders", "cloud_order_events", "cloud_order_desired_state"} {
		if len(tables[name]) == 0 {
			t.Fatalf("the fixture has no CREATE TABLE %s with columns", name)
		}
	}
	known := map[string]bool{}
	for name, columns := range tables {
		known[name] = true
		for c := range columns {
			known[c] = true
		}
	}
	literal := regexp.MustCompile(`'[^']*'`)
	cast := regexp.MustCompile(`::[a-z]+`)
	param := regexp.MustCompile(`\$[0-9]+`)
	word := regexp.MustCompile(`[a-z_][a-z0-9_]*`)
	var unknown []string
	for name, sql := range map[string]string{
		"cloudOrdersTableExistsSQL": cloudOrdersTableExistsSQL,
		"listCloudOrdersSQL":        listCloudOrdersSQL,
		"claimCloudOrderSQL":        claimCloudOrderSQL,
		"finishCloudOrderSQL":       finishCloudOrderSQL,
		"readyCloudOrderSQL":        readyCloudOrderSQL,
		"listCloudOrdersSQL view":   cloudOrderDesiredStateSQL,
		"entitlements exist":        cloudOrderEntitlementsExistSQL,
	} {
		text := strings.ToLower(sql)
		if name != "cloudOrdersTableExistsSQL" && name != "entitlements exist" {
			// The table check names its tables as literals; everywhere else a literal is a value.
			text = literal.ReplaceAllString(text, " ")
		} else {
			text = strings.ReplaceAll(text, "'", " ")
		}
		text = param.ReplaceAllString(cast.ReplaceAllString(text, " "), " ")
		for _, w := range word.FindAllString(text, -1) {
			if !sqlKeywords[w] && !known[w] {
				unknown = append(unknown, name+": "+w)
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Fatalf("the store's SQL names what lake's cloud_orders migration does not have:\n%s", strings.Join(unknown, "\n"))
	}

	// What the operator reads and writes, by table: the fixture must keep every one.
	for table, columns := range map[string][]string{
		"cloud_orders":              {"id", "offer", "cloud_provider", "region", "state", "host", "claimed_by", "claimed_at", "failure_reason"},
		"cloud_order_events":        {"order_id", "at", "from_state", "to_state", "actor", "detail"},
		"cloud_order_desired_state": {"id", "offer", "cloud_provider", "region", "state", "host", "claimed_by", "desired_state"},
	} {
		for _, c := range columns {
			if !tables[table][c] {
				t.Errorf("lake's %s has no column %s, which the store uses", table, c)
			}
		}
	}
}
