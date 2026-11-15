/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Zetta backs up through the engine's own consistent snapshot (operator#732, "our own, built into
// Zetta"; the routes are zetta#1521, docs/data/backup-snapshot.md there): the Job asks the node for
// a snapshot (POST /v1/backup_snapshot), which answers its id and a summary of what it holds, streams
// the snapshot's archive (GET /v1/backup_snapshot/<id>) to <prefix>zetta.archive as a multipart
// upload, and releases it (DELETE). A restore streams the archive back into a Zetta with no user data
// (POST /v1/restore_snapshot); the engine checks what it restored against the archive's own manifest
// and refuses (422) otherwise, allowing for records that expired since the backup, which it names
// (expired_records). The operator's comparison adds those back, so it agrees with the engine's. A
// scratch target is emptied afterwards with POST /v1/restore_snapshot:discard, which the engine
// allows only on a node started with ZETTA_RESTORE_TARGET=1. The engine never holds the bucket's
// credentials: the Job moves the bytes.
type Zetta struct {
	// HTTP is the client; nil means one without a timeout (the context bounds the run).
	HTTP *http.Client
}

const (
	zettaArchiveKey = "zetta.archive"
	// zettaDefaultPort is where the node's API listens by default (--mcp-listen).
	zettaDefaultPort = "4800"
)

func (z Zetta) client() *http.Client {
	if z.HTTP != nil {
		return z.HTTP
	}
	return &http.Client{}
}

func (z Zetta) url(store Datastore, path string) string {
	port := store.Port
	if port == "" {
		port = zettaDefaultPort
	}
	return "http://" + net.JoinHostPort(store.Host, port) + path
}

func (z Zetta) call(ctx context.Context, store Datastore, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, z.url(store, path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+store.Password)
	if body != nil {
		contentType := "application/octet-stream" // an archive
		if strings.HasSuffix(path, ":discard") {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := z.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		message := strings.TrimSpace(string(text))
		if resp.StatusCode == http.StatusNotFound && !strings.Contains(path, "/v1/backup_snapshot/") {
			return nil, fmt.Errorf("the Zetta engine has no backup routes yet (zetta#1521): %s %s answered 404 %s", method, path, message)
		}
		return nil, fmt.Errorf("zetta refused %s %s: %d %s", method, path, resp.StatusCode, redact(fmt.Errorf("%s", message), store.Password))
	}
	return resp, nil
}

// zettaAnswer is what the backup routes answer: snapshot and summary at the top level, or inside
// the envelope's value, with value.details for a restore.
type zettaAnswer struct {
	Snapshot string           `json:"snapshot"`
	Summary  map[string]int64 `json:"summary"`
	Details  struct {
		NodeID         string           `json:"node_id"`
		ExpiredRecords map[string]int64 `json:"expired_records"`
	} `json:"details"`
}

func decodeZettaAnswer(resp *http.Response) (zettaAnswer, error) {
	defer resp.Body.Close()
	var answer struct {
		zettaAnswer
		Value *zettaAnswer `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&answer); err != nil {
		return zettaAnswer{}, fmt.Errorf("parse zetta's answer: %w", err)
	}
	out := answer.zettaAnswer
	if value := answer.Value; value != nil {
		if out.Snapshot == "" {
			out.Snapshot = value.Snapshot
		}
		if out.Summary == nil {
			out.Summary = value.Summary
		}
		if out.Details.NodeID == "" {
			out.Details = value.Details
		}
	}
	return out, nil
}

// zettaBackupExtra is what a Zetta backup records in its manifest.
type zettaBackupExtra struct {
	Snapshot string `json:"snapshot"`
}

// Backup implements Mechanism.
func (z Zetta) Backup(ctx context.Context, store Datastore, target Target) (Result, error) {
	resp, err := z.call(ctx, store, http.MethodPost, "/v1/backup_snapshot", nil)
	if err != nil {
		return Result{}, err
	}
	snapshot, err := decodeZettaAnswer(resp)
	if err != nil {
		return Result{}, err
	}
	if snapshot.Snapshot == "" {
		return Result{}, fmt.Errorf("zetta answered backup_snapshot without a snapshot id")
	}
	defer func() {
		if resp, err := z.call(context.WithoutCancel(ctx), store, http.MethodDelete, "/v1/backup_snapshot/"+url.PathEscape(snapshot.Snapshot), nil); err == nil {
			resp.Body.Close()
		}
	}()
	archive, err := z.call(ctx, store, http.MethodGet, "/v1/backup_snapshot/"+url.PathEscape(snapshot.Snapshot), nil)
	if err != nil {
		return Result{}, err
	}
	defer archive.Body.Close()
	size, err := (&S3{Bucket: target.Bucket}).PutStream(ctx, target.Prefix+zettaArchiveKey, archive.Body)
	if err != nil {
		return Result{}, fmt.Errorf("upload the snapshot: %w", err)
	}
	summary := map[string]Count{}
	for name, n := range snapshot.Summary {
		summary[name] = Exact(n)
	}
	extra, _ := json.Marshal(zettaBackupExtra{Snapshot: snapshot.Snapshot})
	return Result{SizeBytes: size, Summary: summary, Extra: extra, Message: fmt.Sprintf("zetta snapshot %s, %d items counted", snapshot.Snapshot, len(summary))}, nil
}

// Restore implements Mechanism. The engine refuses a target that holds user data, and a restore whose
// counts are not the manifest's less the records that expired since.
func (z Zetta) Restore(ctx context.Context, store Datastore, source Target, manifest Manifest) (Restored, error) {
	body, err := (&S3{Bucket: source.Bucket}).GetStream(ctx, source.Prefix+zettaArchiveKey)
	if err != nil {
		return Restored{}, err
	}
	defer body.Close()
	resp, err := z.call(ctx, store, http.MethodPost, "/v1/restore_snapshot", body)
	if err != nil {
		return Restored{}, err
	}
	restored, err := decodeZettaAnswer(resp)
	if err != nil {
		return Restored{}, err
	}
	// The engine verified it, allowing for expired records; add them back so the operator's
	// comparison with the backup's summary agrees with the engine's.
	observed := map[string]int64{}
	for name, n := range restored.Summary {
		observed[name] = n
	}
	var expired int64
	for collection, n := range restored.Details.ExpiredRecords {
		name := "collection:" + collection
		if n > 0 && (observed[name] > 0 || manifest.Summary[name].Max > 0) {
			observed[name] += n
		}
		expired += n
	}
	snapshot := restored.Snapshot
	if snapshot == "" {
		var extra zettaBackupExtra
		_ = json.Unmarshal(manifest.Extra, &extra)
		snapshot = extra.Snapshot
	}
	message := fmt.Sprintf("zetta restored %d items and checked them against the archive's manifest", len(restored.Summary))
	if expired > 0 {
		message += fmt.Sprintf("; %d records had expired since the backup and were not written", expired)
	}
	return Restored{Observed: observed, Message: message,
		Details: map[string]string{"node_id": restored.Details.NodeID, "snapshot": snapshot}}, nil
}

// Cleanup implements Cleaner: it empties the scratch Zetta it restored into. The engine refuses it
// unless the node was started with ZETTA_RESTORE_TARGET=1 (RestoreRehearsal sets it on the scratch
// OramaZettaCluster) and nothing was written since the restore.
func (z Zetta) Cleanup(ctx context.Context, store Datastore, _ Manifest, restored Restored) error {
	node, snapshot := restored.Details["node_id"], restored.Details["snapshot"]
	if node == "" || snapshot == "" {
		return fmt.Errorf("the restore's answer named no node or snapshot to discard")
	}
	body, _ := json.Marshal(map[string]string{"node_id": node, "snapshot": snapshot})
	resp, err := z.call(ctx, store, http.MethodPost, "/v1/restore_snapshot:discard", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
