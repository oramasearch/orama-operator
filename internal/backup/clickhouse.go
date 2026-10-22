/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// ClickHouse backs up through the server's own `BACKUP ... TO S3(...)` (ClickHouse, Apache-2.0):
// the server writes a consistent per-table snapshot straight to the bucket and `RESTORE ... FROM
// S3(...)` reads it back. The Job only sends SQL over the HTTP interface, so no ClickHouse tool is
// added to the operator image. The server itself needs egress to the bucket's endpoint.
type ClickHouse struct {
	// HTTP is the client; nil means one without a timeout (the context bounds the backup).
	HTTP *http.Client
}

// clickHouseSystemDatabases are never backed up: they are the server's own and are rebuilt.
var clickHouseSystemDatabases = []string{"system", "information_schema", "INFORMATION_SCHEMA"}

// Backup implements Mechanism.
func (c ClickHouse) Backup(ctx context.Context, store Datastore, target Target) (Result, error) {
	what := "ALL EXCEPT DATABASES " + strings.Join(quoteIdentifiers(clickHouseSystemDatabases), ", ")
	if store.Database != "" {
		what = "DATABASE " + quoteIdentifier(store.Database)
	}
	query := fmt.Sprintf("BACKUP %s TO S3(%s, %s, %s)", what,
		quoteString(target.URL()), quoteString(target.Bucket.AccessKeyID), quoteString(target.Bucket.SecretAccessKey))
	out, err := c.query(ctx, store, query)
	if err != nil {
		return Result{}, redact(err, target.Bucket.SecretAccessKey)
	}
	// The answer is one TSV row: the backup id and its status.
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return Result{}, fmt.Errorf("clickhouse answered BACKUP with %q, not an id and a status", line)
	}
	id, status := fields[0], fields[1]
	if status != "BACKUP_CREATED" {
		return Result{}, fmt.Errorf("clickhouse backup %s ended with status %s", id, status)
	}
	size, err := c.backupSize(ctx, store, id)
	if err != nil {
		// The backup is written; its size is only reported.
		return Result{Message: fmt.Sprintf("clickhouse backup %s created; its size could not be read: %v", id, err)}, nil
	}
	return Result{SizeBytes: size, Message: fmt.Sprintf("clickhouse backup %s created", id)}, nil
}

func (c ClickHouse) backupSize(ctx context.Context, store Datastore, id string) (int64, error) {
	out, err := c.query(ctx, store, fmt.Sprintf("SELECT * FROM system.backups WHERE id = %s FORMAT JSONEachRow", quoteString(id)))
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return 0, err
		}
		// total_size is the size of the backup's files; older servers report compressed_size.
		for _, column := range []string{"total_size", "compressed_size"} {
			if size, ok := jsonInt(row[column]); ok {
				return size, nil
			}
		}
	}
	return 0, fmt.Errorf("system.backups has no size for %s", id)
}

func (c ClickHouse) query(ctx context.Context, store Datastore, query string) (string, error) {
	port := store.Port
	if port == "" {
		port = "8123"
	}
	// The query, which carries the bucket credentials, is the POST body: never in the URL, so
	// never in an access log.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(store.Host, port)+"/", strings.NewReader(query))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-ClickHouse-User", store.Username)
	req.Header.Set("X-ClickHouse-Key", store.Password)
	client := c.HTTP
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("clickhouse refused: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func jsonInt(v any) (int64, bool) {
	switch value := v.(type) {
	case float64:
		return int64(value), true
	case string:
		n, err := strconv.ParseInt(value, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func quoteString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func quoteIdentifier(s string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(s) + "`"
}

func quoteIdentifiers(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = quoteIdentifier(name)
	}
	return out
}

// redact removes a secret from an error before it is reported anywhere.
func redact(err error, secret string) error {
	if err == nil || secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}
