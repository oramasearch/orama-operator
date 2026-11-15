/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyAcceptsCountsWithinTheBackupsRange(t *testing.T) {
	manifest := Manifest{Backup: "b1", Summary: map[string]Count{"a.t": {Min: 10, Max: 12}, "a.u": Exact(3)}}
	if _, err := Verify(manifest, map[string]int64{"a.t": 11, "a.u": 3}); err != nil {
		t.Fatalf("within range: %v", err)
	}
	for name, observed := range map[string]map[string]int64{
		"too few rows": {"a.t": 9, "a.u": 3},
		"a lost table": {"a.t": 10},
		"an extra one": {"a.t": 10, "a.u": 3, "a.v": 1},
	} {
		if _, err := Verify(manifest, observed); err == nil {
			t.Fatalf("%s: verified", name)
		}
	}
}

// A ClickHouse restore refuses a server that already has the backup's tables, restores into an empty
// one, and reports what it restored.
func TestClickHouseRestoresIntoAnEmptyServerOnly(t *testing.T) {
	manifest := Manifest{Kind: "ClickHouse", Backup: "b1", Summary: map[string]Count{"events.hits": Exact(10)}}
	source := Target{Bucket: Bucket{Endpoint: "https://e", Name: "b", AccessKeyID: "k", SecretAccessKey: "s"}, Prefix: "p/b1/"}

	busy := &fakeClickHouse{t: t}
	server := httptest.NewServer(busy)
	defer server.Close()
	if _, err := (ClickHouse{}).Restore(context.Background(), clickHouseStore(t, server), source, manifest); err == nil || !strings.Contains(err.Error(), "already has table events.hits") {
		t.Fatalf("restore over existing tables: %v", err)
	}

	empty := &fakeClickHouse{t: t, empty: true}
	server2 := httptest.NewServer(empty)
	defer server2.Close()
	restored, err := (ClickHouse{}).Restore(context.Background(), clickHouseStore(t, server2), source, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Observed["events.hits"] != 10 {
		t.Fatalf("observed = %v", restored.Observed)
	}
	if _, err := Verify(manifest, restored.Observed); err != nil {
		t.Fatal(err)
	}
	if err := (ClickHouse{}).Cleanup(context.Background(), clickHouseStore(t, server2), manifest, Restored{}); err != nil {
		t.Fatal(err)
	}
	if len(empty.dropped) != 1 || !strings.Contains(empty.dropped[0], "`events`") {
		t.Fatalf("dropped = %q; want only the restored database", empty.dropped)
	}
}

// `/manager restore` reads the manifest, restores, verifies and reports Verified with its evidence.
func TestRestoreMainVerifiesAgainstTheManifest(t *testing.T) {
	objects := &fakeStore{objects: map[string][]byte{}, pageMax: 10}
	bucketServer := httptest.NewServer(objects)
	defer bucketServer.Close()
	manifest, _ := json.Marshal(Manifest{Kind: "ClickHouse", Backup: "b1", Summary: map[string]Count{"events.hits": Exact(10)}})
	objects.objects["ws/clickhouse/events/b1/"+ManifestKey] = manifest

	fake := &fakeClickHouse{t: t, empty: true}
	server := httptest.NewServer(fake)
	defer server.Close()
	store := clickHouseStore(t, server)
	env := map[string]string{
		EnvDatastoreHost: store.Host, EnvDatastorePort: store.Port, EnvDatastoreUsername: "backup", EnvDatastorePassword: "pw",
		EnvBucketEndpoint: bucketServer.URL, EnvBucketName: "backups", EnvBucketAccessKey: "key-id", EnvBucketSecretKey: "s",
	}
	termination := filepath.Join(t.TempDir(), "termination-log")
	var stderr bytes.Buffer
	code := RestoreMain([]string{"--datastore", "ClickHouse", "--restore", "r1", "--prefix", "ws/clickhouse/events/b1", "--cleanup"}, func(k string) string { return env[k] }, termination, &stderr)
	data, _ := os.ReadFile(termination)
	report, ok := ParseReport(string(data))
	if code != 0 || !ok || !report.Succeeded || !report.Verified || report.Evidence == "" {
		t.Fatalf("exit %d, report %+v: %s", code, report, stderr.String())
	}
	if len(fake.dropped) != 1 {
		t.Fatalf("a scratch restore must be emptied afterwards; dropped %q", fake.dropped)
	}

	code = RestoreMain([]string{"--datastore", "Valkey", "--restore", "r2", "--prefix", "ws/clickhouse/events/b1"}, func(k string) string { return env[k] }, termination, &stderr)
	data, _ = os.ReadFile(termination)
	report, _ = ParseReport(string(data))
	if code != 1 || report.Succeeded {
		t.Fatalf("a restore of another kind's backup succeeded: %+v", report)
	}
}
