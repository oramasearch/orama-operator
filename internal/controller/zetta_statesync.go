/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// One Orama symbiosis v0 — coarse operator-state sync into Zetta.
//
// The operator continuously mirrors its OBSERVED state (the CR statuses it already computes) into the
// Zetta `orama_control.components` table, at the SAME granularity + anti-flap discipline as the k8s
// status subresource (operator principle #7): one row per managed component, written ONLY when its
// status/detail actually changes. Kubernetes remains the source of truth for DESIRED state (the CRs);
// this is the durable, SQL-queryable projection of what's running.
//
// The deployed Zetta SQL engine is append-only (no UPDATE yet — oramasearch/zetta#735), so a change
// APPENDS a row (latest-wins on read by updated_at). Anti-flap keeps that append rate low. Best-effort:
// the node's HTTP server is flaky (zetta#731), so a failed write is simply retried next reconcile.

// componentRow is one coarse component state destined for orama_control.components. `operator` is this
// operator's unique identity — Zetta's cluster can span many k8s clusters, each with its own operator,
// all writing to the same store, so every row is namespaced by operator to keep their states distinct.
type componentRow struct {
	operator, name, kind, owner, status, detail string
}

// key identifies the component across reconciles (for the anti-flap cache), namespaced by operator.
func (c componentRow) key() string {
	return c.operator + "|" + c.owner + "|" + c.kind + "|" + c.name
}

// fingerprint is the SEMANTIC state (no timestamp) — the anti-flap comparison.
func (c componentRow) fingerprint() string { return c.status + "|" + c.detail }

// zettaBaseURL is where the operator reaches this cluster's node: the public host (reachable from a
// locally-run operator) when set, else the in-cluster client Service DNS.
func (r *OramaZettaClusterReconciler) zettaBaseURL(z *oramav1alpha1.OramaZettaCluster, clientSvc string) string {
	if z.Spec.Host != "" {
		return "https://" + z.Spec.Host
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", clientSvc, z.Namespace, zettaAPIPort)
}

// reconcileOperatorStateSync gathers the coarse state of every operator-managed CR in the namespace
// and appends the CHANGED rows to orama_control.components. Best-effort: never fails the reconcile.
func (r *OramaZettaClusterReconciler) reconcileOperatorStateSync(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientSvc string) {
	log := logf.FromContext(ctx)
	// The engine's SQL gate is always on (zetta#799): authenticate with the seeded admin token. If the
	// Secret isn't present yet (race — reconciled just above, but etcd may lag), skip this round and retry
	// on the next reconcile. NEVER fall back to an unauthenticated call — the auth-on engine would 401.
	token, err := r.zettaAdminToken(ctx, z)
	if err != nil {
		log.V(1).Info("zetta state sync deferred — admin token not ready", "err", err.Error())
		return
	}
	rows := r.gatherOperatorState(ctx, z.Namespace, r.operatorID(ctx))
	base := r.zettaBaseURL(z, clientSvc)
	now := time.Now().UTC().Format(time.RFC3339)
	synced := 0
	for _, row := range rows {
		if prev, ok := r.stateCache.Load(row.key()); ok && prev == row.fingerprint() {
			continue // unchanged — anti-flap (no write, no control-plane churn)
		}
		if err := zettaSQL(ctx, base, token, "orama_control", insertComponentSQL(row, now)); err != nil {
			// Flaky node / unreachable: leave the cache untouched so we retry next reconcile.
			log.V(1).Info("zetta state sync deferred", "component", row.key(), "err", err.Error())
			continue
		}
		r.stateCache.Store(row.key(), row.fingerprint())
		synced++
	}
	if synced > 0 {
		log.Info("synced operator state into zetta", "database", "orama_control", "rowsChanged", synced)
	}
}

// gatherOperatorState reads the coarse component state from every managed CR — the same
// component-granular statuses the operator already maintains (anti-flapped in k8s).
func (r *OramaZettaClusterReconciler) gatherOperatorState(ctx context.Context, ns, operator string) []componentRow {
	var rows []componentRow

	var v1s oramav1alpha1.OramaCloudV1List
	if err := r.List(ctx, &v1s, client.InNamespace(ns)); err == nil {
		for i := range v1s.Items {
			oc := &v1s.Items[i]
			rows = append(rows, componentRow{
				operator: operator, name: oc.Name, kind: "OramaCloudV1", owner: oc.Name,
				status: orDefault(oc.Status.Phase, "Unknown"), detail: oc.Status.ComponentsReady,
			})
			for _, c := range oc.Status.Components {
				rows = append(rows, componentRow{
					operator: operator, name: c.Name, kind: "component", owner: oc.Name,
					status: readyWord(c.Ready), detail: c.Detail,
				})
			}
		}
	}

	var zettas oramav1alpha1.OramaZettaClusterList
	if err := r.List(ctx, &zettas, client.InNamespace(ns)); err == nil {
		for i := range zettas.Items {
			zc := &zettas.Items[i]
			rows = append(rows, componentRow{
				operator: operator, name: zc.Name, kind: "OramaZettaCluster", owner: zc.Name,
				status: orDefault(zc.Status.Phase, "Unknown"),
				detail: fmt.Sprintf("%d/%d", zc.Status.ReadyNodes, zc.Status.TotalNodes),
			})
		}
	}
	return rows
}

// operatorID is this operator's unique identity — one operator per k8s cluster, but Zetta's cluster can
// span many k8s clusters, so each namespaces its state by this id. Defaults to the kube-system Namespace
// UID (stable + unique per cluster); override with the ORAMA_OPERATOR_ID env var. Computed once.
//
// The kube-system UID is read through the uncached reader (operator#106): a cached Get on a cluster-scoped
// Namespace would make controller-runtime start a cluster-wide Namespace list/watch informer just for this
// one-time read — over-broad RBAC that crashlooped the operator this session ("failed to wait for
// namespaces caches to sync"). A single uncached GET needs only `namespaces get`, no list/watch.
func (r *OramaZettaClusterReconciler) operatorID(ctx context.Context) string {
	r.operatorIDOnce.Do(func() {
		if v := os.Getenv("ORAMA_OPERATOR_ID"); v != "" {
			r.operatorIDValue = v
			return
		}
		var ns corev1.Namespace
		if err := r.reader().Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns); err == nil && ns.UID != "" {
			r.operatorIDValue = string(ns.UID)
			return
		}
		r.operatorIDValue = "unknown"
	})
	return r.operatorIDValue
}

// insertComponentSQL builds an append for one component row, namespaced by operator.
func insertComponentSQL(row componentRow, now string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return fmt.Sprintf(
		"INSERT INTO components (operator, name, kind, owner, status, detail, updated_at) "+
			"VALUES (%s, %s, %s, %s, %s, %s, %s)",
		q(row.operator), q(row.name), q(row.kind), q(row.owner), q(row.status), q(row.detail), q(now))
}

// zettaAdminToken reads the seeded admin bearer (Secret `<name>-zetta-admin`, key `token`) that the
// operator uses to authenticate its Zetta SQL client — the engine's auth is mandatory (zetta#799), so an
// unauthenticated SQL call 401s. Read through r.reader() (uncached in-cluster) so we always see the freshly
// minted Secret. Returns an error when the Secret isn't present yet or carries no token, so the best-effort
// caller can retry rather than fall back to an unauthenticated request.
func (r *OramaZettaClusterReconciler) zettaAdminToken(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (string, error) {
	var sec corev1.Secret
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: z.Name + "-zetta-admin"}, &sec); err != nil {
		return "", err
	}
	token := string(sec.Data["token"])
	if token == "" {
		return "", fmt.Errorf("admin token Secret %s-zetta-admin has no token yet", z.Name)
	}
	return token, nil
}

// zettaSQL runs one statement against the node's GET /v1/sql, authenticated with the seeded admin bearer
// (auth is always enforced — zetta#799). Short timeout; returns an error on any non-ok reply so the caller
// can retry.
func zettaSQL(ctx context.Context, base, token, database, statement string) error {
	u := base + "/v1/sql?statement=" + url.QueryEscape(statement)
	if database != "" {
		u += "&database=" + url.QueryEscape(database)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 || !strings.Contains(string(body), `"ok":true`) {
		msg := string(body)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("zetta sql %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// zettaSQLResponse is the subset of the /v1/sql success envelope the operator reads back. The engine wraps
// a capability's output as `{"ok":true,"value":<output>,...}` (src/core/api/rest.cpp rest_envelope), and the
// `sql` capability's output carries `columns` + `rows`, each row an object keyed by column name
// (src/core/api/capabilities.cpp sql_output_schema). A failure comes back `{"ok":false,"error":{...}}`.
type zettaSQLResponse struct {
	OK    bool `json:"ok"`
	Value struct {
		Rows []map[string]any `json:"rows"`
	} `json:"value"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// zettaSQLQuery runs one SELECT against the node's GET /v1/sql and returns the result rows (each a
// column→string map). Like zettaSQL it authenticates with the seeded admin bearer (auth is mandatory —
// zetta#799); unlike it, it PARSES `.value.rows` so the caller can read data back (zettaSQL only checks
// ok:true). Short timeout; returns an error on any non-ok reply so the best-effort caller can retry.
func zettaSQLQuery(ctx context.Context, base, token, database, statement string) ([]map[string]string, error) {
	u := base + "/v1/sql?statement=" + url.QueryEscape(statement)
	if database != "" {
		u += "&database=" + url.QueryEscape(database)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		msg := string(body)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("zetta sql %d: %s", resp.StatusCode, msg)
	}
	var parsed zettaSQLResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("zetta sql: decode response: %w", err)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("zetta sql not ok: %s", parsed.Error.Message)
	}
	out := make([]map[string]string, 0, len(parsed.Value.Rows))
	for _, row := range parsed.Value.Rows {
		m := make(map[string]string, len(row))
		for k, v := range row {
			m[k] = coerceSQLString(v)
		}
		out = append(out, m)
	}
	return out, nil
}

// coerceSQLString renders one JSON cell as a string (our topology columns are all TEXT, so a cell is
// normally a JSON string; a null becomes "" and any other scalar is formatted verbatim).
func coerceSQLString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

func readyWord(ready bool) string {
	if ready {
		return "Ready"
	}
	return "NotReady"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
