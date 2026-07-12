/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// TestInsertTopologySQLWellFormed asserts the topology INSERT is well-formed, carries every column in order,
// is namespaced by operator + cluster, and single-quote-escapes values (SQL-injection / apostrophe safety).
func TestInsertTopologySQLWellFormed(t *testing.T) {
	row := topoRow{
		operator: "op-A", cluster: "cluster-X", node: "z0-zetta-0",
		address: "9.9.9.9:7777", reachability: oramav1alpha1.ReachabilityWANAccessible,
		nodeType: "permanent", status: topoStatusActive,
	}
	got := insertTopologySQL(row, "2026-07-12T00:00:00Z")

	if !strings.HasPrefix(got, "INSERT INTO topology (operator, cluster, node, address, reachability, node_type, status, updated_at) VALUES (") {
		t.Fatalf("unexpected INSERT prologue: %q", got)
	}
	// Namespacing + values are present and single-quoted.
	for _, want := range []string{
		"'op-A'", "'cluster-X'", "'z0-zetta-0'", "'9.9.9.9:7777'",
		"'wan-accessible'", "'permanent'", "'active'", "'2026-07-12T00:00:00Z'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("INSERT missing %s: %q", want, got)
		}
	}

	// An embedded apostrophe must be doubled, never left to break out of the string literal.
	evil := insertTopologySQL(topoRow{operator: "o'p", cluster: "c", node: "n", status: topoStatusActive}, "t")
	if !strings.Contains(evil, "'o''p'") {
		t.Errorf("apostrophe not escaped by doubling: %q", evil)
	}
}

// TestSelectTopologySQLFiltersByCluster asserts the read is scoped to the given global cluster name so a big
// shared store's read stays small and only sibling slices are considered.
func TestSelectTopologySQLFiltersByCluster(t *testing.T) {
	got := selectTopologySQL("cluster-X")
	if !strings.Contains(got, "SELECT operator, cluster, node, address, reachability, node_type, status, updated_at") {
		t.Errorf("SELECT missing full column list: %q", got)
	}
	if !strings.Contains(got, "FROM topology WHERE cluster = 'cluster-X'") {
		t.Errorf("SELECT not filtered by cluster: %q", got)
	}
}

// TestDeriveExternalPeersFromTopology is the core read→derive logic: from a fake cross-cluster row set the
// operator must dial the OTHER slices' wan-accessible active endpoints and NOTHING else — own-cluster nodes,
// dial-out nodes, departed (gone / latest-wins-superseded) nodes, other logical clusters, and our own
// advertised addresses are all excluded. It also asserts the union with an explicit spec.externalPeers.
func TestDeriveExternalPeersFromTopology(t *testing.T) {
	const ourOp, ourCluster = "op-A", "cluster-X"
	wan := oramav1alpha1.ReachabilityWANAccessible
	dialOut := oramav1alpha1.ReachabilityDialOutOnly

	rows := []topoRow{
		// Sibling slice (different operator, same cluster), wan + active → INCLUDED.
		{operator: "op-B", cluster: ourCluster, node: "z1-zetta-0", address: "10.0.0.1:7777", reachability: wan, status: topoStatusActive, updatedAt: "2026-07-12T00:00:00Z"},
		{operator: "op-B", cluster: ourCluster, node: "z1-zetta-1", address: "10.0.0.2:7777", reachability: wan, status: topoStatusActive, updatedAt: "2026-07-12T00:00:00Z"},
		// Our OWN slice's node (our operator) — EXCLUDED (never peer to ourselves).
		{operator: ourOp, cluster: ourCluster, node: "z0-zetta-0", address: "9.9.9.9:7777", reachability: wan, status: topoStatusActive, updatedAt: "2026-07-12T00:00:00Z"},
		// Another operator's own advertised address that also happens to equal OURS — EXCLUDED via ownAddrs.
		{operator: "op-C", cluster: ourCluster, node: "zc-zetta-0", address: "9.9.9.9:7777", reachability: wan, status: topoStatusActive, updatedAt: "2026-07-12T00:00:00Z"},
		// A dial-out-only sibling (no dialable address) — EXCLUDED.
		{operator: "op-D", cluster: ourCluster, node: "zd-zetta-0", address: "", reachability: dialOut, status: topoStatusActive, updatedAt: "2026-07-12T00:00:00Z"},
		// A sibling that was active then went gone (later updated_at wins) — EXCLUDED (failover).
		{operator: "op-E", cluster: ourCluster, node: "ze-zetta-0", address: "10.0.0.9:7777", reachability: wan, status: topoStatusActive, updatedAt: "2026-07-12T00:00:00Z"},
		{operator: "op-E", cluster: ourCluster, node: "ze-zetta-0", address: "", reachability: wan, status: topoStatusGone, updatedAt: "2026-07-12T01:00:00Z"},
		// A wan node in a DIFFERENT logical cluster — EXCLUDED.
		{operator: "op-F", cluster: "cluster-Y", node: "zf-zetta-0", address: "10.9.9.9:7777", reachability: wan, status: topoStatusActive, updatedAt: "2026-07-12T00:00:00Z"},
	}

	ownAddrs := map[string]bool{"9.9.9.9:7777": true}
	derived := deriveExternalPeers(rows, ourOp, ourCluster, ownAddrs)

	want := []string{"10.0.0.1:7777", "10.0.0.2:7777"}
	if strings.Join(derived, ",") != strings.Join(want, ",") {
		t.Fatalf("derived peers = %v, want %v", derived, want)
	}

	// Union with an explicit spec.externalPeers: the explicit entries come FIRST (override wins) and derived
	// fills the gap, deduped (the explicit 10.0.0.1 is not repeated by the derived one).
	explicit := []string{"1.2.3.4:7777", "10.0.0.1:7777"}
	union := zettaPeerSet(explicit, derived)
	wantUnion := []string{"1.2.3.4:7777", "10.0.0.1:7777", "10.0.0.2:7777"}
	if strings.Join(union, ",") != strings.Join(wantUnion, ",") {
		t.Fatalf("peer union = %v, want %v", union, wantUnion)
	}
}

// TestZettaSQLQueryParsesRows asserts the read client sends the admin bearer AND parses `.value.rows` out of
// the engine's /v1/sql success envelope (mirrors TestZettaSQLSendsBearer, but for the row-reading path).
func TestZettaSQLQueryParsesRows(t *testing.T) {
	var gotAuth, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, gotURL = req.Header.Get("Authorization"), req.URL.String()
		// The real engine envelope: {"ok":true,"value":{"messages":[],"columns":[...],"rows":[{...}]}}.
		_, _ = w.Write([]byte(`{"ok":true,"value":{"messages":[],"columns":["operator","node","address"],` +
			`"rows":[{"operator":"op-B","cluster":"cluster-X","node":"z1-zetta-0","address":"10.0.0.1:7777",` +
			`"reachability":"wan-accessible","node_type":"permanent","status":"active","updated_at":"2026-07-12T00:00:00Z"}]}}`))
	}))
	defer srv.Close()

	rows, err := zettaSQLQuery(context.Background(), srv.URL, "sekret-token", "orama_control", selectTopologySQL("cluster-X"))
	if err != nil {
		t.Fatalf("zettaSQLQuery: %v", err)
	}
	if gotAuth != "Bearer sekret-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sekret-token")
	}
	if !strings.Contains(gotURL, "database=orama_control") {
		t.Errorf("request URL missing database param: %q", gotURL)
	}
	if len(rows) != 1 {
		t.Fatalf("parsed %d rows, want 1", len(rows))
	}
	if rows[0]["address"] != "10.0.0.1:7777" || rows[0]["status"] != "active" {
		t.Errorf("row not parsed correctly: %+v", rows[0])
	}

	// End-to-end: the parsed rows derive into the sibling's endpoint.
	derived := deriveExternalPeers(topoRowsFromSQL(rows), "op-A", "cluster-X", nil)
	if strings.Join(derived, ",") != "10.0.0.1:7777" {
		t.Errorf("derived from parsed rows = %v, want [10.0.0.1:7777]", derived)
	}
}

// captureSQL is an httptest server that records every `statement` it receives and returns a bare ok — the
// fake node's /v1/sql write endpoint.
func captureSQL(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var stmts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		stmts = append(stmts, req.URL.Query().Get("statement"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"value":{"messages":[],"columns":[],"rows":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &stmts
}

// TestWriteZettaTopologyAppendsAndMarksGone drives the WRITE half against a fake node: a wan slice appends one
// active row per node (namespaced by operator/cluster), an unchanged re-write appends NOTHING (anti-flap),
// and scaling the slice down appends a `gone` marker for the departed node (the failover write).
func TestWriteZettaTopologyAppendsAndMarksGone(t *testing.T) {
	srv, stmts := captureSQL(t)
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	ctx := context.Background()

	// A wan-accessible slice whose address comes from spec.wanAddress (so zettaWANAddresses needs no client).
	z := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "z0", Namespace: "orama-system", UID: types.UID("uid-z0")},
		Spec: oramav1alpha1.OramaZettaClusterSpec{
			Replicas:     2,
			Reachability: oramav1alpha1.ReachabilityWANAccessible,
			WanAddress:   "9.9.9.9",
		},
	}

	r.writeZettaTopology(ctx, z, srv.URL, "tok", "op-A", "cluster-X", "z0-zetta-peer", "z0-zetta")
	inserts := filterContains(*stmts, "INSERT INTO topology")
	if len(inserts) != 2 {
		t.Fatalf("first write: %d topology INSERTs, want 2 (one per node)\n%v", len(inserts), inserts)
	}
	// Both nodes present, namespaced, active, at the wan address with the peer port appended.
	joined := strings.Join(inserts, "\n")
	for _, want := range []string{"'z0-zetta-0'", "'z0-zetta-1'", "'op-A'", "'cluster-X'", "'9.9.9.9:7777'", "'active'"} {
		if !strings.Contains(joined, want) {
			t.Errorf("active INSERTs missing %s:\n%s", want, joined)
		}
	}

	// An identical re-write is a no-op (anti-flap).
	r.writeZettaTopology(ctx, z, srv.URL, "tok", "op-A", "cluster-X", "z0-zetta-peer", "z0-zetta")
	if got := len(filterContains(*stmts, "INSERT INTO topology")); got != 2 {
		t.Fatalf("anti-flap: re-write appended rows (%d INSERTs, want 2)", got)
	}

	// Scale down to 1 node → node-1 departed → a single `gone` marker appended for it (failover).
	z.Spec.Replicas = 1
	r.writeZettaTopology(ctx, z, srv.URL, "tok", "op-A", "cluster-X", "z0-zetta-peer", "z0-zetta")
	all := filterContains(*stmts, "INSERT INTO topology")
	if len(all) != 3 {
		t.Fatalf("after scale-down: %d INSERTs, want 3 (2 active + 1 gone)\n%v", len(all), all)
	}
	last := all[len(all)-1]
	if !strings.Contains(last, "'gone'") || !strings.Contains(last, "'z0-zetta-1'") {
		t.Errorf("scale-down did not mark z0-zetta-1 gone: %q", last)
	}
	if strings.Contains(last, "'z0-zetta-0'") {
		t.Errorf("node-0 (still present) must not be marked gone: %q", last)
	}
}

func filterContains(all []string, sub string) []string {
	var out []string
	for _, s := range all {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}
