// SPDX-License-Identifier: MIT

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeZetta is an in-memory Zetta /v1/sql with the engine's behaviour the orama_control contract
// depends on (zetta src/core/query/executor.cpp):
//   - rows are schemaless JSON: an INSERT stores exactly the columns it names, with their typed
//     values (a quoted string, TRUE/FALSE, NULL or a number);
//   - a SELECT naming a column that no row of a non-empty table carries is an error naming it
//     (zetta#1218); a table with no rows refuses nothing;
//   - one `WHERE column = literal` filter, and the selected columns projected, null when a row
//     lacks one;
//   - every request carries the bearer.
//
// It is how the tests prove that lake's own SELECT strings succeed against the rows the operator
// writes, which a statement-matching double cannot.
type fakeZetta struct {
	server *httptest.Server
	token  string

	mu           sync.Mutex
	tables       map[string][]map[string]any
	statements   []string
	authFailures int
}

func newFakeZetta(t *testing.T, token string) *fakeZetta {
	t.Helper()
	zetta := &fakeZetta{token: token, tables: map[string][]map[string]any{}}
	zetta.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bearer "+token {
			zetta.mu.Lock()
			zetta.authFailures++
			zetta.mu.Unlock()
			writer.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "error": map[string]string{"message": "unauthorized"}})
			return
		}
		query := request.URL.Query()
		rows, err := zetta.exec(query.Get("database"), query.Get("statement"))
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "error": map[string]string{"message": err.Error()}})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "value": map[string]any{"rows": rows}})
	}))
	t.Cleanup(zetta.server.Close)
	return zetta
}

var (
	fakeInsertPattern = regexp.MustCompile(`(?s)^INSERT INTO (\w+) \(([^)]*)\) VALUES \((.*)\)$`)
	fakeSelectPattern = regexp.MustCompile(`(?s)^SELECT (.+?) FROM (\w+)(?: WHERE (\w+) = (.+))?$`)
	fakeCreatePattern = regexp.MustCompile(`^CREATE (TABLE|DATABASE) `)
)

// exec runs one statement against database, as the HTTP surface does; tests call it directly to
// run lake's SQL.
func (zetta *fakeZetta) exec(database, statement string) ([]map[string]any, error) {
	zetta.mu.Lock()
	defer zetta.mu.Unlock()
	zetta.statements = append(zetta.statements, statement)
	if fakeCreatePattern.MatchString(statement) {
		return []map[string]any{}, nil
	}
	if match := fakeInsertPattern.FindStringSubmatch(statement); match != nil {
		columns := strings.Split(match[2], ", ")
		values, err := parseFakeLiterals(match[3])
		if err != nil {
			return nil, err
		}
		if len(values) != len(columns) {
			return nil, fmt.Errorf("INSERT has %d columns but %d values", len(columns), len(values))
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = values[i]
		}
		key := database + "." + match[1]
		zetta.tables[key] = append(zetta.tables[key], row)
		return []map[string]any{}, nil
	}
	if match := fakeSelectPattern.FindStringSubmatch(statement); match != nil {
		columns := strings.Split(match[1], ", ")
		rows := zetta.tables[database+"."+match[2]]
		named := append([]string(nil), columns...)
		if match[3] != "" {
			named = append(named, match[3])
		}
		if len(rows) > 0 {
			known := map[string]bool{}
			for _, row := range rows {
				for column := range row {
					known[column] = true
				}
			}
			for _, column := range named {
				if !known[column] {
					return nil, fmt.Errorf("column %q does not exist in %s", column, match[2])
				}
			}
		}
		var want any
		if match[3] != "" {
			literals, err := parseFakeLiterals(match[4])
			if err != nil || len(literals) != 1 {
				return nil, fmt.Errorf("bad WHERE literal %q", match[4])
			}
			want = literals[0]
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if match[3] != "" {
				value, found := row[match[3]]
				if !found || value != want {
					continue
				}
			}
			projected := make(map[string]any, len(columns))
			for _, column := range columns {
				projected[column] = row[column]
			}
			out = append(out, projected)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported statement: %s", statement)
}

func parseFakeLiterals(input string) ([]any, error) {
	values := make([]any, 0)
	for {
		input = strings.TrimLeft(input, " ")
		if input == "" {
			return values, nil
		}
		switch {
		case input[0] == '\'':
			var value strings.Builder
			i := 1
			for ; i < len(input); i++ {
				if input[i] != '\'' {
					value.WriteByte(input[i])
					continue
				}
				if i+1 < len(input) && input[i+1] == '\'' {
					value.WriteByte('\'')
					i++
					continue
				}
				break
			}
			if i >= len(input) {
				return nil, fmt.Errorf("unterminated string literal")
			}
			values = append(values, value.String())
			input = input[i+1:]
		default:
			end := strings.IndexByte(input, ',')
			if end < 0 {
				end = len(input)
			}
			token := strings.TrimSpace(input[:end])
			switch {
			case token == "TRUE":
				values = append(values, true)
			case token == "FALSE":
				values = append(values, false)
			case token == "NULL":
				values = append(values, nil)
			default:
				number, err := strconv.ParseFloat(token, 64)
				if err != nil {
					return nil, fmt.Errorf("bad literal %q", token)
				}
				values = append(values, number)
			}
			input = input[end:]
		}
		input = strings.TrimLeft(input, " ")
		if input == "" {
			return values, nil
		}
		if input[0] != ',' {
			return nil, fmt.Errorf("expected a comma at %q", input)
		}
		input = input[1:]
	}
}

func (zetta *fakeZetta) url() string { return zetta.server.URL }

// rows is a copy of a table's rows.
func (zetta *fakeZetta) rows(database, table string) []map[string]any {
	zetta.mu.Lock()
	defer zetta.mu.Unlock()
	return append([]map[string]any(nil), zetta.tables[database+"."+table]...)
}

// insertCount counts the INSERT statements run against a table.
func (zetta *fakeZetta) insertCount(table string) int {
	zetta.mu.Lock()
	defer zetta.mu.Unlock()
	count := 0
	for _, statement := range zetta.statements {
		if strings.HasPrefix(statement, "INSERT INTO "+table+" ") {
			count++
		}
	}
	return count
}

// mustExec runs SQL as lake would and fails the test on an engine error.
func (zetta *fakeZetta) mustExec(t *testing.T, statement string) []map[string]any {
	t.Helper()
	rows, err := zetta.exec("orama_control", statement)
	if err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	return rows
}

func TestFakeZettaRefusesAColumnNoRowHas(t *testing.T) {
	zetta := newFakeZetta(t, "token")
	if _, err := zetta.exec("orama_control", "SELECT nope FROM deployments WHERE lake = 'x'"); err != nil {
		t.Fatalf("an empty table refused a SELECT: %v", err)
	}
	zetta.mustExec(t, "INSERT INTO deployments (deployment_id, lake, deleted) VALUES ('d1', 'x', TRUE)")
	if _, err := zetta.exec("orama_control", "SELECT nope FROM deployments WHERE lake = 'x'"); err == nil || !strings.Contains(err.Error(), `"nope" does not exist`) {
		t.Fatalf("unknown column error = %v", err)
	}
	rows := zetta.mustExec(t, "SELECT deployment_id, deleted FROM deployments WHERE lake = 'x'")
	if len(rows) != 1 || rows[0]["deleted"] != true || rows[0]["deployment_id"] != "d1" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows := zetta.mustExec(t, "SELECT deployment_id FROM deployments WHERE lake = 'y'"); len(rows) != 0 {
		t.Fatalf("WHERE did not filter: %+v", rows)
	}
}
