/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A volume is copied file by file and restored into an empty volume with the same files, bytes and
// digest; a non-empty target is refused.
func TestVolumeCopiesAndRestoresEveryFile(t *testing.T) {
	objects := &fakeStore{objects: map[string][]byte{}, pageMax: 10}
	bucketServer := httptest.NewServer(objects)
	defer bucketServer.Close()
	target := Target{Bucket: Bucket{Endpoint: bucketServer.URL, Name: "backups", Region: "auto", AccessKeyID: "key-id", SecretAccessKey: "s"}, Prefix: "lake/volume/store/b1/"}

	source := t.TempDir()
	writeTree(t, source, map[string]string{"repos/a/HEAD": "ref", "drive/x.bin": strings.Repeat("x", 100000), "lost+found/junk": "skip"})
	if err := os.Symlink("repos/a", filepath.Join(source, "current")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVolumePath, source)
	result, err := Volume{}.Backup(context.Background(), Datastore{}, target)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary["files"] != Exact(2) || result.Summary["bytes"] != Exact(100003) {
		t.Fatalf("summary = %v", result.Summary)
	}
	manifest := Manifest{Kind: "Volume", Backup: "b1", Summary: result.Summary, Extra: result.Extra}

	restored := t.TempDir()
	t.Setenv(EnvVolumePath, restored)
	got, err := Volume{}.Restore(context.Background(), Datastore{}, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(manifest, got.Observed); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(restored, "repos/a/HEAD")); string(data) != "ref" {
		t.Fatalf("restored HEAD = %q", data)
	}
	link, _ := os.Readlink(filepath.Join(restored, "current"))
	if link != "repos/a" {
		t.Fatalf("symlink = %q", link)
	}
	if _, err := (Volume{}).Restore(context.Background(), Datastore{}, target, manifest); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("restore into a non-empty volume: %v", err)
	}
}

func TestVolumeFormatsCheckTheEngineHeader(t *testing.T) {
	if !SQLiteFormat.Valid([]byte("SQLite format 3\x00rest"), nil) || SQLiteFormat.Valid([]byte("garbage"), nil) {
		t.Fatal("sqlite header")
	}
	if !DuckDBFormat.Valid([]byte("\x00\x00\x00\x00\x00\x00\x00\x00DUCK\x00\x00"), nil) {
		t.Fatal("duckdb header")
	}
	if !PolarsFormat.Valid([]byte("PAR1...."), []byte("....PAR1")) || PolarsFormat.Valid([]byte("PAR1"), []byte("trunc")) {
		t.Fatal("parquet header")
	}
}
