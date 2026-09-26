/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-0123456789abcdef"

type harness struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(t.TempDir(), "main.db")
	}
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	logs := &bytes.Buffer{}
	cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	srv, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, server: srv, http: httptest.NewServer(srv.Handler()), logs: logs}
	t.Cleanup(func() {
		h.http.Close()
		_ = srv.Close()
	})
	return h
}

// call sends body (a string, or a value marshalled to JSON) and decodes the answer.
func (h *harness) call(method, path, token string, body interface{}) (int, map[string]interface{}) {
	h.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.http.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	var decoded map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		h.t.Fatalf("%s %s: status %d, body %q is not a JSON object: %v", method, path, resp.StatusCode, raw, err)
	}
	return resp.StatusCode, decoded
}

func (h *harness) post(path string, body interface{}) (int, map[string]interface{}) {
	h.t.Helper()
	return h.call(http.MethodPost, path, testToken, body)
}

func (h *harness) mustExec(sql string, params ...interface{}) map[string]interface{} {
	h.t.Helper()
	if params == nil {
		params = []interface{}{}
	}
	status, body := h.post("/v1/exec", map[string]interface{}{"sql": sql, "params": params})
	if status != http.StatusOK {
		h.t.Fatalf("exec %q: %d %v", sql, status, body)
	}
	return body
}

func errorCode(body map[string]interface{}) string {
	detail, _ := body["error"].(map[string]interface{})
	code, _ := detail["code"].(string)
	return code
}

// canonical renders v as compact JSON, for comparing decoded answers.
func canonical(t *testing.T, v interface{}) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAuth(t *testing.T) {
	h := newHarness(t, Config{})
	query := map[string]interface{}{"sql": "SELECT 1", "params": []interface{}{}}
	cases := []struct {
		name   string
		method string
		path   string
		header string
		body   interface{}
		want   int
		code   string
	}{
		{"no header", http.MethodPost, "/v1/query", "", query, http.StatusUnauthorized, "unauthorized"},
		{"wrong token", http.MethodPost, "/v1/query", "Bearer nope", query, http.StatusUnauthorized, "unauthorized"},
		{"token prefix", http.MethodPost, "/v1/query", "Bearer " + testToken[:10], query, http.StatusUnauthorized, "unauthorized"},
		{"token with suffix", http.MethodPost, "/v1/query", "Bearer " + testToken + "x", query, http.StatusUnauthorized, "unauthorized"},
		{"basic scheme", http.MethodPost, "/v1/query", "Basic " + testToken, query, http.StatusUnauthorized, "unauthorized"},
		{"empty bearer", http.MethodPost, "/v1/query", "Bearer ", query, http.StatusUnauthorized, "unauthorized"},
		{"exec without token", http.MethodPost, "/v1/exec", "", query, http.StatusUnauthorized, "unauthorized"},
		{"batch without token", http.MethodPost, "/v1/batch", "", map[string]interface{}{"statements": []interface{}{query}}, http.StatusUnauthorized, "unauthorized"},
		{"unknown route without token", http.MethodGet, "/v1/tables", "", nil, http.StatusUnauthorized, "unauthorized"},
		{"unknown route with token", http.MethodGet, "/v1/tables", "Bearer " + testToken, nil, http.StatusNotFound, "not_found"},
		{"good token", http.MethodPost, "/v1/query", "Bearer " + testToken, query, http.StatusOK, ""},
		{"wrong method", http.MethodGet, "/v1/query", "Bearer " + testToken, nil, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"healthz needs no token", http.MethodGet, "/healthz", "", nil, http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reader io.Reader
			if tc.body != nil {
				raw, _ := json.Marshal(tc.body)
				reader = bytes.NewReader(raw)
			}
			req, _ := http.NewRequest(tc.method, h.http.URL+tc.path, reader)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := h.http.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want || errorCode(body) != tc.code {
				t.Fatalf("got %d %v, want %d %q", resp.StatusCode, body, tc.want, tc.code)
			}
		})
	}
}

func TestQueryTypes(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE TABLE t (i INTEGER, r REAL, s TEXT, b BLOB, n TEXT, d DATE, ts DATETIME)")
	h.mustExec("INSERT INTO t VALUES (?, ?, ?, ?, ?, ?, ?)",
		9007199254740993, 1.0, "héllo", map[string]interface{}{"$base64": "AAEC/w=="}, nil, "2026-09-26", "2026-09-26 10:11:12")
	cases := []struct {
		name   string
		sql    string
		params []interface{}
		want   string
	}{
		{"every column type", "SELECT i, r, s, b, n FROM t", nil,
			`{"columns":["i","r","s","b","n"],"rows":[[9007199254740993,1.0,"héllo",{"$base64":"AAEC/w=="},null]]}`},
		{"integer parameter stays an integer", "SELECT ? AS v, typeof(?) AS t", []interface{}{42, 42},
			`{"columns":["v","t"],"rows":[[42,"integer"]]}`},
		{"float parameter stays a float", "SELECT ? AS v, typeof(?) AS t", []interface{}{2.5, 2.5},
			`{"columns":["v","t"],"rows":[[2.5,"real"]]}`},
		{"boolean parameter binds as an integer", "SELECT ? AS v", []interface{}{true},
			`{"columns":["v"],"rows":[[1]]}`},
		{"null parameter", "SELECT ? IS NULL AS v", []interface{}{nil},
			`{"columns":["v"],"rows":[[1]]}`},
		{"blob parameter", "SELECT length(?) AS v, typeof(?) AS t", []interface{}{map[string]interface{}{"$base64": "AAEC"}, map[string]interface{}{"$base64": "AAEC"}},
			`{"columns":["v","t"],"rows":[[3,"blob"]]}`},
		{"infinity", "SELECT 1e999 AS v", nil,
			`{"columns":["v"],"rows":[["Infinity"]]}`},
		{"date columns come back as text", "SELECT d, ts FROM t", nil,
			`{"columns":["d","ts"],"rows":[["2026-09-26","2026-09-26 10:11:12"]]}`},
		{"no rows", "SELECT i FROM t WHERE 0", nil,
			`{"columns":["i"],"rows":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := tc.params
			if params == nil {
				params = []interface{}{}
			}
			status, body := h.post("/v1/query", map[string]interface{}{"sql": tc.sql, "params": params})
			if status != http.StatusOK {
				t.Fatalf("status %d: %v", status, body)
			}
			if got := canonical(t, body); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestRequestErrors(t *testing.T) {
	h := newHarness(t, Config{MaxBodyBytes: 1024})
	cases := []struct {
		name string
		path string
		body interface{}
		want int
		code string
	}{
		{"SQL error", "/v1/query", map[string]interface{}{"sql": "SELECT * FROM missing", "params": []interface{}{}}, http.StatusBadRequest, "sql_error"},
		{"syntax error", "/v1/exec", map[string]interface{}{"sql": "CREATE TABL x", "params": []interface{}{}}, http.StatusBadRequest, "sql_error"},
		{"empty sql", "/v1/query", map[string]interface{}{"sql": " ", "params": []interface{}{}}, http.StatusBadRequest, "invalid_request"},
		{"not JSON", "/v1/query", "{", http.StatusBadRequest, "invalid_request"},
		{"unknown field", "/v1/query", map[string]interface{}{"sql": "SELECT 1", "args": []interface{}{}}, http.StatusBadRequest, "invalid_request"},
		{"nested array parameter", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{[]interface{}{1}}}, http.StatusBadRequest, "invalid_params"},
		{"object parameter", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{map[string]interface{}{"x": 1}}}, http.StatusBadRequest, "invalid_params"},
		{"bad base64", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{map[string]interface{}{"$base64": "***"}}}, http.StatusBadRequest, "invalid_params"},
		{"integer overflow", "/v1/query", `{"sql":"SELECT ?","params":[99999999999999999999]}`, http.StatusBadRequest, "invalid_params"},
		{"body over the cap", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{strings.Repeat("x", 2048)}}, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"empty batch", "/v1/batch", map[string]interface{}{"statements": []interface{}{}}, http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.post(tc.path, tc.body)
			if status != tc.want || errorCode(body) != tc.code {
				t.Fatalf("got %d %v, want %d %q", status, body, tc.want, tc.code)
			}
			if msg, _ := body["error"].(map[string]interface{})["message"].(string); msg == "" {
				t.Fatalf("error without a message: %v", body)
			}
		})
	}
}

func TestBodyCapIsSixteenMiB(t *testing.T) {
	h := newHarness(t, Config{})
	big := `{"sql":"SELECT 1","params":["` + strings.Repeat("x", DefaultMaxBodyBytes) + `"]}`
	status, body := h.post("/v1/query", big)
	if status != http.StatusRequestEntityTooLarge || errorCode(body) != "body_too_large" {
		t.Fatalf("got %d %v", status, body)
	}
}

func TestExec(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	cases := []struct {
		name   string
		sql    string
		params []interface{}
		want   string
	}{
		{"insert", "INSERT INTO t (v) VALUES (?)", []interface{}{"a"}, `{"last_insert_id":1,"rows_affected":1}`},
		{"insert two", "INSERT INTO t (v) VALUES (?), (?)", []interface{}{"b", "c"}, `{"last_insert_id":3,"rows_affected":2}`},
		{"update", "UPDATE t SET v = upper(v) WHERE id > ?", []interface{}{1}, `{"last_insert_id":3,"rows_affected":2}`},
		{"delete", "DELETE FROM t WHERE v = ?", []interface{}{"a"}, `{"last_insert_id":3,"rows_affected":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonical(t, h.mustExec(tc.sql, tc.params...)); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	_, body := h.post("/v1/query", map[string]interface{}{"sql": "SELECT id, v FROM t ORDER BY id", "params": []interface{}{}})
	if got := canonical(t, body["rows"]); got != `[[2,"B"],[3,"C"]]` {
		t.Fatalf("rows after exec: %s", got)
	}
}

func TestForeignKeysAndWAL(t *testing.T) {
	h := newHarness(t, Config{})
	_, body := h.post("/v1/query", map[string]interface{}{"sql": "PRAGMA journal_mode", "params": []interface{}{}})
	if got := canonical(t, body["rows"]); got != `[["wal"]]` {
		t.Fatalf("journal_mode = %s", got)
	}
	h.mustExec("CREATE TABLE parent (id INTEGER PRIMARY KEY)")
	h.mustExec("CREATE TABLE child (parent_id INTEGER REFERENCES parent(id))")
	status, body := h.post("/v1/exec", map[string]interface{}{"sql": "INSERT INTO child VALUES (7)", "params": []interface{}{}})
	if status != http.StatusBadRequest || errorCode(body) != "sql_error" {
		t.Fatalf("a foreign key violation was accepted: %d %v", status, body)
	}
}

func TestBatch(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)")
	count := func() string {
		_, body := h.post("/v1/query", map[string]interface{}{"sql": "SELECT count(*) FROM t", "params": []interface{}{}})
		return canonical(t, body["rows"])
	}
	stmt := func(sql string, params ...interface{}) map[string]interface{} {
		if params == nil {
			params = []interface{}{}
		}
		return map[string]interface{}{"sql": sql, "params": params}
	}
	cases := []struct {
		name       string
		statements []interface{}
		wantStatus int
		wantCode   string
		wantBody   string
		wantCount  string
	}{
		{
			name: "every statement commits",
			statements: []interface{}{
				stmt("INSERT INTO t (v) VALUES (?)", "a"),
				stmt("INSERT INTO t (v) VALUES (?)", "b"),
				stmt("SELECT v FROM t ORDER BY id"),
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"results":[{"last_insert_id":1,"rows_affected":1},{"last_insert_id":2,"rows_affected":1},{"columns":["v"],"rows":[["a"],["b"]]}]}`,
			wantCount:  `[[2]]`,
		},
		{
			name: "an error rolls every statement back",
			statements: []interface{}{
				stmt("INSERT INTO t (v) VALUES (?)", "c"),
				stmt("DELETE FROM t"),
				stmt("INSERT INTO t (v) VALUES (NULL)"),
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "sql_error",
			wantCount:  `[[2]]`,
		},
		{
			name: "a bad parameter runs nothing",
			statements: []interface{}{
				stmt("DELETE FROM t"),
				stmt("INSERT INTO t (v) VALUES (?)", []interface{}{1}),
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_params",
			wantCount:  `[[2]]`,
		},
		{
			name:       "returning answers rows",
			statements: []interface{}{stmt("INSERT INTO t (v) VALUES (?) RETURNING id", "d")},
			wantStatus: http.StatusOK,
			wantBody:   `{"results":[{"columns":["id"],"rows":[[3]]}]}`,
			wantCount:  `[[3]]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.post("/v1/batch", map[string]interface{}{"statements": tc.statements})
			if status != tc.wantStatus || errorCode(body) != tc.wantCode {
				t.Fatalf("got %d %v, want %d %q", status, body, tc.wantStatus, tc.wantCode)
			}
			if tc.wantBody != "" {
				if got := canonical(t, body); got != tc.wantBody {
					t.Fatalf("got  %s\nwant %s", got, tc.wantBody)
				}
			}
			if tc.wantCode != "" {
				if msg := body["error"].(map[string]interface{})["message"].(string); !strings.HasPrefix(msg, "statements[") {
					t.Fatalf("the error does not name its statement: %q", msg)
				}
			}
			if got := count(); got != tc.wantCount {
				t.Fatalf("count after batch = %s, want %s", got, tc.wantCount)
			}
		})
	}
}

func TestTruncation(t *testing.T) {
	cases := []struct {
		name      string
		maxRows   int
		rows      int
		wantRows  int
		truncated bool
	}{
		{"under the cap", 5, 4, 4, false},
		{"at the cap", 5, 5, 5, false},
		{"over the cap", 5, 6, 5, true},
		{"the default cap is 10000", 0, DefaultMaxRows + 1, DefaultMaxRows, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{MaxRows: tc.maxRows})
			sql := fmt.Sprintf("WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n WHERE x < %d) SELECT x FROM n", tc.rows)
			for _, path := range []string{"/v1/query", "/v1/batch"} {
				var status int
				var body map[string]interface{}
				if path == "/v1/query" {
					status, body = h.post(path, map[string]interface{}{"sql": sql, "params": []interface{}{}})
				} else {
					status, body = h.post(path, map[string]interface{}{"statements": []interface{}{map[string]interface{}{"sql": sql, "params": []interface{}{}}}})
					body = body["results"].([]interface{})[0].(map[string]interface{})
				}
				if status != http.StatusOK {
					t.Fatalf("%s: %d %v", path, status, body)
				}
				rows := body["rows"].([]interface{})
				truncated, _ := body["truncated"].(bool)
				if len(rows) != tc.wantRows || truncated != tc.truncated {
					t.Fatalf("%s: %d rows, truncated %v; want %d, %v", path, len(rows), truncated, tc.wantRows, tc.truncated)
				}
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t, Config{})
	status, body := h.call(http.MethodGet, "/healthz", "", nil)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthy: %d %v", status, body)
	}
	status, body = h.call(http.MethodPost, "/healthz", "", nil)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz: %d %v", status, body)
	}
	// A closed database no longer answers SELECT 1.
	_ = h.server.Close()
	status, body = h.call(http.MethodGet, "/healthz", "", nil)
	if status != http.StatusServiceUnavailable || errorCode(body) != "unavailable" {
		t.Fatalf("closed database: %d %v", status, body)
	}
}

func TestOpenRefusesAnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing directory", Config{Path: filepath.Join(dir, "missing", "main.db"), Token: testToken}},
		{"no token", Config{Path: filepath.Join(dir, "main.db")}},
		{"no path", Config{Token: testToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if srv, err := Open(context.Background(), tc.cfg); err == nil {
				_ = srv.Close()
				t.Fatal("Open succeeded")
			}
		})
	}
}

func TestLogsNeverCarryTokenSQLOrParams(t *testing.T) {
	h := newHarness(t, Config{})
	secret := "parameter-that-must-not-be-logged"
	h.mustExec("CREATE TABLE secrets_table_name (v TEXT)")
	h.mustExec("INSERT INTO secrets_table_name VALUES (?)", secret)
	h.call(http.MethodPost, "/v1/query", "wrong-token-value", map[string]interface{}{"sql": "SELECT 1", "params": []interface{}{}})
	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want one log line per request, got %d:\n%s", len(lines), h.logs.String())
	}
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		for _, forbidden := range []string{testToken, "wrong-token-value", secret, "secrets_table_name"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("log line carries %q: %s", forbidden, line)
			}
		}
		if entry["path"] == nil || entry["status"] == nil {
			t.Fatalf("log line lacks path or status: %s", line)
		}
	}
}

func TestRequestTimeout(t *testing.T) {
	h := newHarness(t, Config{RequestTimeout: 100 * time.Millisecond})
	// A query that runs for far longer than the timeout.
	sql := "WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n) SELECT count(*) FROM n"
	status, body := h.post("/v1/query", map[string]interface{}{"sql": sql, "params": []interface{}{}})
	if status != http.StatusGatewayTimeout || errorCode(body) != "timeout" {
		t.Fatalf("got %d %v", status, body)
	}
	// The server still answers afterwards.
	if status, body := h.call(http.MethodGet, "/healthz", "", nil); status != http.StatusOK {
		t.Fatalf("healthz after a timeout: %d %v", status, body)
	}
}

func TestDatabaseIsTheFileAtPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.db")
	h := newHarness(t, Config{Path: path})
	h.mustExec("CREATE TABLE t (v INTEGER)")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("no WAL file beside the database: %v", err)
	}
}
