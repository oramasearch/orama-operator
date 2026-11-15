/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeZetta answers the backup routes of zetta#1521, or 404 like an engine without them.
type fakeZetta struct {
	absent    bool
	released  bool
	restored  []byte
	discarded string
	// expired: the restore answers one record of collection "sessions" as expired.
	expired bool
}

func (f *fakeZetta) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer admin-token" {
		http.Error(w, `{"error":{"code":"unauthorized"}}`, http.StatusForbidden)
		return
	}
	if f.absent {
		http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/backup_snapshot":
		// The engine's envelope, with snapshot and summary also at the top level.
		fmt.Fprint(w, `{"ok":true,"value":{"snapshot":"s1"},"snapshot":"s1","summary":{"table:lake.events":12,"bucket:drive":3,"collection:sessions":2}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/backup_snapshot/s1":
		fmt.Fprint(w, "ARCHIVE-BYTES")
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/backup_snapshot/s1":
		f.released = true
	case r.Method == http.MethodPost && r.URL.Path == "/v1/restore_snapshot":
		f.restored, _ = io.ReadAll(r.Body)
		sessions, expired := 2, "{}"
		if f.expired {
			sessions, expired = 1, `{"sessions":1}`
		}
		fmt.Fprintf(w, `{"ok":true,"value":{"snapshot":"s1","details":{"node_id":"n7","expired_records":%s}},"summary":{"table:lake.events":12,"bucket:drive":3,"collection:sessions":%d}}`, expired, sessions)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/restore_snapshot:discard":
		body, _ := io.ReadAll(r.Body)
		f.discarded = string(body)
	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func zettaStore(t *testing.T, server *httptest.Server) Datastore {
	u, _ := url.Parse(server.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	return Datastore{Host: host, Port: port, Password: "admin-token"}
}

func TestZettaSnapshotsArchivesReleasesAndRestores(t *testing.T) {
	engine := &fakeZetta{}
	server := httptest.NewServer(engine)
	defer server.Close()
	objects := &fakeStore{objects: map[string][]byte{}, pageMax: 10}
	bucketServer := httptest.NewServer(objects)
	defer bucketServer.Close()
	target := Target{Bucket: Bucket{Endpoint: bucketServer.URL, Name: "backups", Region: "auto", AccessKeyID: "key-id", SecretAccessKey: "s"}, Prefix: "ws-1/zetta/z/b1/"}

	result, err := Zetta{}.Backup(context.Background(), zettaStore(t, server), target)
	if err != nil {
		t.Fatal(err)
	}
	if string(objects.objects["ws-1/zetta/z/b1/"+zettaArchiveKey]) != "ARCHIVE-BYTES" || !engine.released || result.Summary["table:lake.events"] != Exact(12) {
		t.Fatalf("result %+v, released %v, objects %v", result, engine.released, objects.objects)
	}
	manifest := Manifest{Kind: "Zetta", Backup: "b1", Summary: result.Summary, Extra: result.Extra}
	// A record that expired between the backup and the restore is allowed for, as the engine does.
	engine.expired = true
	restored, err := Zetta{}.Restore(context.Background(), zettaStore(t, server), target, manifest)
	if err != nil || string(engine.restored) != "ARCHIVE-BYTES" {
		t.Fatalf("restore: %v, body %q", err, engine.restored)
	}
	if _, err := Verify(manifest, restored.Observed); err != nil {
		t.Fatal(err)
	}
	if err := (Zetta{}).Cleanup(context.Background(), zettaStore(t, server), manifest, restored); err != nil {
		t.Fatal(err)
	}
	if engine.discarded != `{"node_id":"n7","snapshot":"s1"}` {
		t.Fatalf("discard body = %q", engine.discarded)
	}
}

func TestZettaDefaultsToTheMCPListenPort(t *testing.T) {
	if got := (Zetta{}).url(Datastore{Host: "z"}, "/v1/backup_snapshot"); got != "http://z:4800/v1/backup_snapshot" {
		t.Fatalf("url = %s", got)
	}
}

func TestZettaWithoutTheBackupRoutesSaysSo(t *testing.T) {
	server := httptest.NewServer(&fakeZetta{absent: true})
	defer server.Close()
	_, err := Zetta{}.Backup(context.Background(), zettaStore(t, server), Target{Bucket: Bucket{Endpoint: "http://unused", Name: "b"}, Prefix: "p/"})
	if err == nil || !strings.Contains(err.Error(), "zetta#1521") {
		t.Fatalf("err = %v", err)
	}
}
