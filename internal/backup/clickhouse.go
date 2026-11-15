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
	"slices"
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

func clickHouseWhat(store Datastore) string {
	if store.Database != "" {
		return "DATABASE " + quoteIdentifier(store.Database)
	}
	return "ALL EXCEPT DATABASES " + strings.Join(quoteIdentifiers(clickHouseSystemDatabases), ", ")
}

// Backup implements Mechanism. The rows of every table are counted before and after the snapshot,
// so a restore can be verified while the server keeps taking writes (operator#741).
func (c ClickHouse) Backup(ctx context.Context, store Datastore, target Target) (Result, error) {
	before, err := c.rowCounts(ctx, store)
	if err != nil {
		return Result{}, fmt.Errorf("count rows before the backup: %w", err)
	}
	query := fmt.Sprintf("BACKUP %s TO S3(%s, %s, %s)", clickHouseWhat(store),
		quoteString(target.URL()), quoteString(target.Bucket.AccessKeyID), quoteString(target.Bucket.SecretAccessKey))
	out, err := c.query(ctx, store, query)
	if err != nil {
		return Result{}, redact(err, target.Bucket.SecretAccessKey)
	}
	after, err := c.rowCounts(ctx, store)
	if err != nil {
		return Result{}, fmt.Errorf("count rows after the backup: %w", err)
	}
	summary := map[string]Count{}
	for table, rows := range before {
		if later, ok := after[table]; ok {
			summary[table] = Count{Min: min(rows, later), Max: max(rows, later)}
		}
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
		return Result{Summary: summary, Message: fmt.Sprintf("clickhouse backup %s created; its size could not be read: %v", id, err)}, nil
	}
	return Result{SizeBytes: size, Summary: summary, Message: fmt.Sprintf("clickhouse backup %s created, %d tables", id, len(summary))}, nil
}

// Restore implements Mechanism: RESTORE ... FROM S3(...) into a server that has none of the backup's
// databases yet, then the restored rows per table.
func (c ClickHouse) Restore(ctx context.Context, store Datastore, source Target, manifest Manifest) (Restored, error) {
	existing, err := c.rowCounts(ctx, store)
	if err != nil {
		return Restored{}, fmt.Errorf("read the target's tables: %w", err)
	}
	for table := range manifest.Summary {
		if _, ok := existing[table]; ok {
			return Restored{}, fmt.Errorf("the target already has table %s: a restore goes into a server without the backup's tables", table)
		}
	}
	query := fmt.Sprintf("RESTORE %s FROM S3(%s, %s, %s)", clickHouseWhat(store),
		quoteString(source.URL()), quoteString(source.Bucket.AccessKeyID), quoteString(source.Bucket.SecretAccessKey))
	out, err := c.query(ctx, store, query)
	if err != nil {
		return Restored{}, redact(err, source.Bucket.SecretAccessKey)
	}
	fields := strings.Split(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), "\t")
	if len(fields) < 2 || fields[1] != "RESTORED" {
		return Restored{}, fmt.Errorf("clickhouse answered RESTORE with %q", strings.TrimSpace(out))
	}
	after, err := c.rowCounts(ctx, store)
	if err != nil {
		return Restored{}, fmt.Errorf("count the restored rows: %w", err)
	}
	observed := map[string]int64{}
	for table, rows := range after {
		if _, existed := existing[table]; !existed {
			observed[table] = rows
		}
	}
	return Restored{Observed: observed, Message: fmt.Sprintf("clickhouse restore %s: %d tables", fields[0], len(observed))}, nil
}

// Cleanup implements Cleaner: it drops the databases the manifest's tables are in, on the scratch
// target only (RestoreRehearsal), never a system database.
func (c ClickHouse) Cleanup(ctx context.Context, store Datastore, manifest Manifest, _ Restored) error {
	databases := map[string]bool{}
	for table := range manifest.Summary {
		if database, _, ok := strings.Cut(table, "."); ok {
			databases[database] = true
		}
	}
	for database := range databases {
		if slices.Contains(clickHouseSystemDatabases, database) || database == "default" {
			continue
		}
		if _, err := c.query(ctx, store, "DROP DATABASE IF EXISTS "+quoteIdentifier(database)+" SYNC"); err != nil {
			return err
		}
	}
	return nil
}

// rowCounts is every user table's row count, keyed <database>.<table>. Tables without a count
// (views, dictionaries, engines that do not keep one) are left out.
func (c ClickHouse) rowCounts(ctx context.Context, store Datastore) (map[string]int64, error) {
	filter := "database NOT IN (" + strings.Join(quoteStrings(clickHouseSystemDatabases), ", ") + ")"
	if store.Database != "" {
		filter = "database = " + quoteString(store.Database)
	}
	out, err := c.query(ctx, store, "SELECT database, name, total_rows FROM system.tables WHERE "+filter+" AND total_rows IS NOT NULL AND NOT is_temporary FORMAT JSONEachRow")
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("parse system.tables: %w", err)
		}
		database, _ := row["database"].(string)
		name, _ := row["name"].(string)
		if rows, ok := jsonInt(row["total_rows"]); ok && database != "" && name != "" {
			counts[database+"."+name] = rows
		}
	}
	return counts, nil
}

func quoteStrings(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = quoteString(value)
	}
	return out
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
