/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// Package server is lake-sqlite's HTTP API over one SQLite database: POST /v1/query, /v1/exec and
// /v1/batch behind a bearer token, and an unauthenticated GET /healthz. The contract is lake's
// docs/control-plane/services.md (the sqlite kind); the operator's docs/lake/lake-sqlite.md
// documents this implementation.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// DefaultMaxRows caps the rows one statement returns.
	DefaultMaxRows = 10000
	// DefaultMaxBodyBytes caps a request body: 16 MiB.
	DefaultMaxBodyBytes = 16 << 20
	// DefaultRequestTimeout bounds every request, the SQL included.
	DefaultRequestTimeout = 30 * time.Second
	// BusyTimeoutMillis is how long a statement waits for SQLite's write lock.
	BusyTimeoutMillis = 5000

	healthTimeout = 2 * time.Second
	// maxOpenConns: WAL lets readers run beside the one writer; writers queue on busy_timeout.
	maxOpenConns = 4
)

// Config configures a Server.
type Config struct {
	// Path is the database file. Its directory must exist and be writable.
	Path string
	// Token is the bearer token every route but /healthz requires.
	Token string
	// MaxRows, MaxBodyBytes and RequestTimeout default to the constants above when zero.
	MaxRows        int
	MaxBodyBytes   int64
	RequestTimeout time.Duration
	// Logger receives one line per request; nil discards them.
	Logger *slog.Logger
}

// Server serves the API over one database.
type Server struct {
	db        *sql.DB
	tokenHash [sha256.Size]byte
	maxRows   int
	maxBody   int64
	timeout   time.Duration
	log       *slog.Logger
}

// DSN is the driver connection string for path: WAL, a 5 s busy timeout, foreign keys on, and
// transactions that take the write lock when they begin, so two batches never deadlock upgrading a
// read lock.
func DSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeoutMillis))
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Set("_txlock", "immediate")
	return "file:" + path + "?" + query.Encode()
}

// Open opens the database and checks it answers SELECT 1.
func Open(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Path == "" {
		return nil, errors.New("database path is empty")
	}
	if cfg.Token == "" {
		return nil, errors.New("token is empty")
	}
	db, err := sql.Open("sqlite", DSN(cfg.Path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	s := &Server{
		db:        db,
		tokenHash: sha256.Sum256([]byte(cfg.Token)),
		maxRows:   cfg.MaxRows,
		maxBody:   cfg.MaxBodyBytes,
		timeout:   cfg.RequestTimeout,
		log:       cfg.Logger,
	}
	if s.maxRows <= 0 {
		s.maxRows = DefaultMaxRows
	}
	if s.maxBody <= 0 {
		s.maxBody = DefaultMaxBodyBytes
	}
	if s.timeout <= 0 {
		s.timeout = DefaultRequestTimeout
	}
	if s.log == nil {
		s.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if err := s.check(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DB is the underlying pool, for settings the caller applies once (the soft heap limit).
func (s *Server) DB() *sql.DB { return s.db }

// Close closes the database; the last connection to close checkpoints the WAL.
func (s *Server) Close() error { return s.db.Close() }

func (s *Server) check(ctx context.Context) error {
	var one int64
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return fmt.Errorf("SELECT 1 returned %d", one)
	}
	return nil
}

// Handler is the whole API with request logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/v1/query", s.api(s.handleQuery))
	mux.Handle("/v1/exec", s.api(s.handleExec))
	mux.Handle("/v1/batch", s.api(s.handleBatch))
	mux.Handle("/", s.authorized(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})))
	return s.logged(mux)
}

// apiError is the one error shape: {"error": {"code", "message"}}.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	type detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	writeJSON(w, status, struct {
		Error detail `json:"error"`
	}{detail{code, message}})
}

func writeAPIError(w http.ResponseWriter, err *apiError) {
	writeError(w, err.status, err.code, err.message)
}

// authorized checks Authorization: Bearer <token>. Both sides are hashed first so the comparison
// takes the same time whatever the presented token's length.
func (s *Server) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		hash := sha256.Sum256([]byte(presented))
		if !ok || presented == "" || subtle.ConstantTimeCompare(hash[:], s.tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lake-sqlite"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// api wraps a POST JSON route: the token, the method, the body cap and the request timeout.
func (s *Server) api(handle func(ctx context.Context, body []byte) (interface{}, *apiError)) http.Handler {
	return s.authorized(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			return
		}
		if r.ContentLength > s.maxBody {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("the body is over %d bytes", s.maxBody))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("the body is over %d bytes", s.maxBody))
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request", "cannot read the body")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		result, apiErr := handle(ctx, body)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := s.check(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the database does not answer SELECT 1")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// statement is one {sql, params}.
type statement struct {
	SQL    string        `json:"sql"`
	Params []interface{} `json:"params"`
}

type batchRequest struct {
	Statements []statement `json:"statements"`
}

// queryResult answers /v1/query and a batch statement that returns columns.
type queryResult struct {
	Columns   []string        `json:"columns"`
	Rows      [][]interface{} `json:"rows"`
	Truncated bool            `json:"truncated,omitempty"`
}

// execResult answers /v1/exec and a batch statement that returns no columns.
type execResult struct {
	RowsAffected int64 `json:"rows_affected"`
	LastInsertID int64 `json:"last_insert_id"`
}

// decode reads a JSON body strictly: unknown fields and trailing data are refused, and numbers stay
// json.Number so an integer parameter binds as an integer.
func decode(body []byte, into interface{}) *apiError {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return &apiError{http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid JSON body: %v", err)}
	}
	if decoder.More() {
		return &apiError{http.StatusBadRequest, "invalid_request", "invalid JSON body: trailing data"}
	}
	return nil
}

func (st statement) args() ([]interface{}, *apiError) {
	if strings.TrimSpace(st.SQL) == "" {
		return nil, &apiError{http.StatusBadRequest, "invalid_request", "sql is required"}
	}
	args, err := decodeParams(st.Params)
	if err != nil {
		return nil, &apiError{http.StatusBadRequest, "invalid_params", err.Error()}
	}
	return args, nil
}

func (s *Server) handleQuery(ctx context.Context, body []byte) (interface{}, *apiError) {
	var st statement
	if err := decode(body, &st); err != nil {
		return nil, err
	}
	args, apiErr := st.args()
	if apiErr != nil {
		return nil, apiErr
	}
	rows, err := s.db.QueryContext(ctx, st.SQL, args...)
	if err != nil {
		return nil, sqlError(ctx, err)
	}
	result, err := s.readRows(rows)
	if err != nil {
		return nil, sqlError(ctx, err)
	}
	return result, nil
}

func (s *Server) handleExec(ctx context.Context, body []byte) (interface{}, *apiError) {
	var st statement
	if err := decode(body, &st); err != nil {
		return nil, err
	}
	args, apiErr := st.args()
	if apiErr != nil {
		return nil, apiErr
	}
	res, err := s.db.ExecContext(ctx, st.SQL, args...)
	if err != nil {
		return nil, sqlError(ctx, err)
	}
	return execResultOf(res)
}

func execResultOf(res sql.Result) (execResult, *apiError) {
	affected, err := res.RowsAffected()
	if err != nil {
		return execResult{}, &apiError{http.StatusInternalServerError, "internal", "cannot read rows affected"}
	}
	lastID, err := res.LastInsertId()
	if err != nil {
		return execResult{}, &apiError{http.StatusInternalServerError, "internal", "cannot read the last insert id"}
	}
	return execResult{RowsAffected: affected, LastInsertID: lastID}, nil
}

// handleBatch runs every statement in one transaction, in order. A statement that returns columns
// answers like /v1/query, any other like /v1/exec. The first error rolls the whole batch back.
func (s *Server) handleBatch(ctx context.Context, body []byte) (interface{}, *apiError) {
	var req batchRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if len(req.Statements) == 0 {
		return nil, &apiError{http.StatusBadRequest, "invalid_request", "statements is empty"}
	}
	args := make([][]interface{}, len(req.Statements))
	for i, st := range req.Statements {
		a, apiErr := st.args()
		if apiErr != nil {
			apiErr.message = fmt.Sprintf("statements[%d]: %s", i, apiErr.message)
			return nil, apiErr
		}
		args[i] = a
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, sqlError(ctx, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	results := make([]interface{}, 0, len(req.Statements))
	for i, st := range req.Statements {
		result, apiErr := s.runInTx(ctx, tx, st.SQL, args[i])
		if apiErr != nil {
			apiErr.message = fmt.Sprintf("statements[%d]: %s", i, apiErr.message)
			return nil, apiErr
		}
		results = append(results, result)
	}
	if err := tx.Commit(); err != nil {
		return nil, sqlError(ctx, err)
	}
	committed = true
	return struct {
		Results []interface{} `json:"results"`
	}{results}, nil
}

// runInTx runs one batch statement. Every statement of a transaction runs on the same connection,
// so changes() and last_insert_rowid() read right after it are its own, exactly as the driver's
// Exec reads them.
func (s *Server) runInTx(ctx context.Context, tx *sql.Tx, query string, args []interface{}) (interface{}, *apiError) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, sqlError(ctx, err)
	}
	result, err := s.readRows(rows)
	if err != nil {
		return nil, sqlError(ctx, err)
	}
	if len(result.Columns) > 0 {
		return result, nil
	}
	var exec execResult
	if err := tx.QueryRowContext(ctx, "SELECT changes(), last_insert_rowid()").Scan(&exec.RowsAffected, &exec.LastInsertID); err != nil {
		return nil, sqlError(ctx, err)
	}
	return exec, nil
}

// readRows reads up to maxRows rows and closes rows. When there are more, it stops and marks the
// result truncated.
func (s *Server) readRows(rows *sql.Rows) (queryResult, error) {
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return queryResult{}, err
	}
	result := queryResult{Columns: columns, Rows: [][]interface{}{}}
	if result.Columns == nil {
		result.Columns = []string{}
	}
	for rows.Next() {
		if len(result.Rows) == s.maxRows {
			result.Truncated = true
			break
		}
		values := make([]interface{}, len(columns))
		pointers := make([]interface{}, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return queryResult{}, err
		}
		row := make([]interface{}, len(columns))
		for i, value := range values {
			row[i] = encodeValue(value)
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return queryResult{}, err
	}
	return result, rows.Close()
}

// sqlError maps a database error to its status: a timeout is 504, a locked database 503, a full
// disk 507, and any other SQL error 400 with SQLite's message.
func sqlError(ctx context.Context, err error) *apiError {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &apiError{http.StatusGatewayTimeout, "timeout", "the request ran past its time limit"}
	}
	if errors.Is(err, context.Canceled) {
		return &apiError{499, "canceled", "the request was canceled"}
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return &apiError{http.StatusServiceUnavailable, "busy", sqliteErr.Error()}
		case sqlite3.SQLITE_FULL:
			return &apiError{http.StatusInsufficientStorage, "disk_full", sqliteErr.Error()}
		case sqlite3.SQLITE_INTERRUPT:
			return &apiError{http.StatusGatewayTimeout, "timeout", "the request ran past its time limit"}
		}
		return &apiError{http.StatusBadRequest, "sql_error", sqliteErr.Error()}
	}
	return &apiError{http.StatusBadRequest, "sql_error", err.Error()}
}

// statusRecorder captures what a handler wrote, for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// logged writes one JSON line per request: method, path, status, response size and duration. It
// never logs a header, the body, the SQL or its parameters, so neither the token nor data reaches
// the log.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Int("bytes", recorder.bytes),
			slog.Int64("request_bytes", r.ContentLength),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}
