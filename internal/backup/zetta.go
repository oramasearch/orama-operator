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
// Zetta"): the Job asks the node for a snapshot of the whole cluster (POST /v1/backup_snapshot), which
// answers its id and a summary of what it holds, streams the snapshot's archive (GET
// /v1/backup_snapshot/<id>) to <prefix>zetta.archive as a multipart upload, and releases it (DELETE).
// A restore streams the archive back into a Zetta with no user data (POST /v1/restore_snapshot), which
// answers the same summary of what it restored. The engine never holds the bucket's credentials: the
// Job moves the bytes. The routes are specified in zetta#1518 and are not built yet; until they are,
// the engine's refusal is reported in its own words.
type Zetta struct {
	// HTTP is the client; nil means one without a timeout (the context bounds the run).
	HTTP *http.Client
}

const zettaArchiveKey = "zetta.archive"

func (z Zetta) client() *http.Client {
	if z.HTTP != nil {
		return z.HTTP
	}
	return &http.Client{}
}

func (z Zetta) url(store Datastore, path string) string {
	port := store.Port
	if port == "" {
		port = "8080"
	}
	return "http://" + net.JoinHostPort(store.Host, port) + path
}

func (z Zetta) call(ctx context.Context, store Datastore, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, z.url(store, path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+store.Password)
	resp, err := z.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		message := strings.TrimSpace(string(text))
		if resp.StatusCode == http.StatusNotFound && !strings.Contains(path, "/v1/backup_snapshot/") {
			return nil, fmt.Errorf("the Zetta engine has no backup routes yet (zetta#1518): %s %s answered 404 %s", method, path, message)
		}
		return nil, fmt.Errorf("zetta refused %s %s: %d %s", method, path, resp.StatusCode, redact(fmt.Errorf("%s", message), store.Password))
	}
	return resp, nil
}

type zettaSummary struct {
	Snapshot string           `json:"snapshot"`
	Summary  map[string]int64 `json:"summary"`
}

func decodeZettaSummary(resp *http.Response) (zettaSummary, error) {
	defer resp.Body.Close()
	var answer struct {
		zettaSummary
		// The REST envelope ({"data": ...}) or the bare object.
		Data *zettaSummary `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&answer); err != nil {
		return zettaSummary{}, fmt.Errorf("parse zetta's answer: %w", err)
	}
	if answer.Data != nil {
		return *answer.Data, nil
	}
	return answer.zettaSummary, nil
}

// Backup implements Mechanism.
func (z Zetta) Backup(ctx context.Context, store Datastore, target Target) (Result, error) {
	resp, err := z.call(ctx, store, http.MethodPost, "/v1/backup_snapshot", nil)
	if err != nil {
		return Result{}, err
	}
	snapshot, err := decodeZettaSummary(resp)
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
	return Result{SizeBytes: size, Summary: summary, Message: fmt.Sprintf("zetta snapshot %s, %d items counted", snapshot.Snapshot, len(summary))}, nil
}

// Restore implements Mechanism. The engine refuses a target that holds user data.
func (z Zetta) Restore(ctx context.Context, store Datastore, source Target, _ Manifest) (Restored, error) {
	body, err := (&S3{Bucket: source.Bucket}).GetStream(ctx, source.Prefix+zettaArchiveKey)
	if err != nil {
		return Restored{}, err
	}
	defer body.Close()
	resp, err := z.call(ctx, store, http.MethodPost, "/v1/restore_snapshot", body)
	if err != nil {
		return Restored{}, err
	}
	restored, err := decodeZettaSummary(resp)
	if err != nil {
		return Restored{}, err
	}
	return Restored{Observed: restored.Summary, Message: fmt.Sprintf("zetta restored %d items", len(restored.Summary))}, nil
}
