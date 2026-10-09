/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Cross-cluster Zetta topology THROUGH Zetta (operator#105).
//
// Cross-cluster peering used to be wired by hand: an admin copied each slice's .status.wanAddress into its
// siblings' spec.externalPeers. This coordinates it THROUGH Zetta instead, using Zetta as the operator's
// own datastore (the same symbiosis as zetta_statesync.go):
//
//  1. WRITE  — each reconcile, this operator appends its slice's wan-accessible node endpoints (+ the global
//     cluster name, the operator id, reachability, node_type) to orama_control.topology, with the SAME
//     anti-flap discipline as components: a row is written ONLY when its semantic content changes.
//  2. SYNC   — Zetta CRDT-syncs orama_control.topology across every cluster for free (it is a table on the
//     one shared store). No operator work is needed for the sync itself.
//  3. READ   — each reconcile, this operator SELECTs the full topology back, takes latest-wins per node by
//     updated_at, and DERIVES its nodes' --peer set from the OTHER clusters' wan-accessible endpoints that
//     share this slice's global cluster name. spec.externalPeers, when set, still wins (explicit override);
//     the derived set fills the gap.
//  4. FAILOVER — when a wan node drops out (this operator scales down / the slice flips to dial-out), the
//     operator appends a `status='gone'` marker row; on read, latest-wins makes that node fall out of the
//     derived --peer set on the next reconcile.
//
// The engine's SQL is append-only (no UPDATE/DELETE — zetta#735) and has no IF NOT EXISTS, so this models
// state as append + latest-wins, exactly like components. Everything here is BEST-EFFORT + retried (the
// node's HTTP server is flaky — zetta#731): a failed write/read never fails the reconcile.
//
// OUT OF SCOPE (blocked): removing a departed node from the ENGINE's membership CRDT. That cannot be done
// over SQL (append-only) and there is no node-removal API yet — a `gone` marker row in orama_control.topology
// is the most the operator can express. It affects only the operator's DERIVED --peer set; it does NOT
// evict the node from Zetta's own node-list. See docs/zetta-datastore.md.

// topology row status values.
const (
	topoStatusActive = "active" // a wan-accessible node currently advertising a routable endpoint
	topoStatusGone   = "gone"   // a departed wan node (scaled down / flipped to dial-out) — a marker row
)

// topoRow is one node's cross-cluster topology entry destined for orama_control.topology. `operator` is
// this operator's unique identity and `cluster` is the GLOBAL Zetta cluster name (spec.clusterName, or the
// CR name when unset): rows sharing a `cluster` are SIBLING slices that should peer, and `operator` lets a
// reader exclude its OWN slice's nodes. `node` is the stable per-node id (the pod name), unique within a
// slice; (operator, cluster, node) is globally unique.
type topoRow struct {
	operator, cluster, node, address, reachability, nodeType, status, updatedAt string
}

// key identifies the node across reconciles (the anti-flap cache key + the latest-wins group key).
func (t topoRow) key() string { return t.operator + "|" + t.cluster + "|" + t.node }

// fingerprint is the SEMANTIC state (no timestamp) — the anti-flap comparison.
func (t topoRow) fingerprint() string {
	return t.address + "|" + t.reachability + "|" + t.nodeType + "|" + t.status
}

// effectiveClusterName is the GLOBAL Zetta cluster name this slice belongs to: spec.clusterName when set,
// else the CR name (a standalone cluster named after the CR — matching the CRD's ClusterName semantics).
// Sibling slices share this value; it is the `cluster` column + the read filter.
func effectiveClusterName(z *oramav1alpha1.OramaZettaCluster) string {
	if z.Spec.ClusterName != "" {
		return z.Spec.ClusterName
	}
	return z.Name
}

// zettaNodeType records the coarse role the operator actually knows for a slice — permanent vs ephemeral
// (burst/autoscaled capacity, spec.ephemeral). This is the "node_type if available" the topology carries;
// the operator does not know Zetta's internal node classification, so this is the closest honest signal.
func zettaNodeType(z *oramav1alpha1.OramaZettaCluster) string {
	if z.Spec.Ephemeral {
		return "ephemeral"
	}
	return "permanent"
}

// reconcileZettaTopology coordinates cross-cluster peering through Zetta (operator#105): it WRITES this
// slice's wan-accessible endpoints into orama_control.topology (anti-flap) and READS the cross-cluster set
// back, returning the DERIVED external peers (the wan endpoints of sibling slices) for the StatefulSet's
// --peer rendering. Best-effort: on any Zetta error it returns whatever it derived (possibly nil) and never
// fails the reconcile.
//
// It returns the derived peers that passed validation, the derived addresses it DROPPED because they are not a
// valid host:port (operator#279 — topology rows are written by every operator sharing the Zetta cluster, so
// they are untrusted input), and whether the topology was actually read this round.
func (r *OramaZettaClusterReconciler) reconcileZettaTopology(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientSvc, peerLBName, stsName string) (derivedPeers, droppedPeers []string, read bool) {
	log := logf.FromContext(ctx)
	// Auth is always enforced (zetta#799) — authenticate with the seeded admin token. If the Secret isn't
	// present yet (race), skip this round and retry on the next reconcile. NEVER fall back to unauthenticated.
	token, err := r.zettaAdminToken(ctx, z)
	if err != nil {
		log.V(1).Info("zetta topology deferred — admin token not ready", "err", err.Error())
		return nil, nil, false
	}
	base := r.zettaBaseURL(z, clientSvc)
	operator := r.operatorID(ctx)
	cluster := effectiveClusterName(z)

	// 1. WRITE this slice's wan endpoints (changed rows only) + gone-markers for departed nodes.
	r.writeZettaTopology(ctx, z, base, token, operator, cluster, peerLBName, stsName)

	// 3. READ the cross-cluster topology back and derive this slice's external peers.
	rows, err := zettaSQLQuery(ctx, base, token, "orama_control", selectTopologySQL(cluster))
	if err != nil {
		log.V(1).Info("zetta topology read deferred", "cluster", cluster, "err", err.Error())
		return nil, nil, false
	}
	ownAddrs := map[string]bool{}
	for _, a := range r.zettaWANAddresses(ctx, z, peerLBName, stsName) {
		if a != "" {
			ownAddrs[a] = true
		}
	}
	derived, dropped := deriveValidExternalPeers(topoRowsFromSQL(rows), operator, cluster, ownAddrs)
	r.noteDroppedTopologyPeers(log, z.Namespace+"/"+z.Name, cluster, dropped)
	if len(derived) > 0 {
		log.V(1).Info("derived cross-cluster zetta peers from topology", "cluster", cluster, "peers", derived)
	}
	return derived, dropped, true
}

// writeZettaTopology appends this slice's CHANGED wan-accessible node rows to orama_control.topology
// (anti-flap via r.topologyCache) and appends a `gone` marker for any previously-advertised node that has
// departed (scaled down, or the slice flipped away from wan-accessible). A dial-out-only slice has no
// routable address, so it writes nothing. Best-effort — a failed append leaves the cache untouched so the
// next reconcile retries.
func (r *OramaZettaClusterReconciler) writeZettaTopology(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, base, token, operator, cluster, peerLBName, stsName string) {
	log := logf.FromContext(ctx)
	now := time.Now().UTC().Format(time.RFC3339)
	reachability := zettaReachability(z)
	nodeType := zettaNodeType(z)

	// Wan endpoints IN ORDINAL ORDER (nil when not wan-accessible; "" for a still-pending address).
	addrs := r.zettaWANAddresses(ctx, z, peerLBName, stsName)
	wanReplicas := len(addrs) // 0 for a dial-out-only / ephemeral slice

	// Desired ACTIVE rows: one per ordinal with a resolved wan address.
	desiredActive := map[string]topoRow{}
	for ord, a := range addrs {
		if a == "" {
			continue // address still pending (LB IP unassigned) — not gone, just unknown yet; skip
		}
		row := topoRow{
			operator: operator, cluster: cluster, node: fmt.Sprintf("%s-%d", stsName, ord),
			address: a, reachability: reachability, nodeType: nodeType, status: topoStatusActive,
		}
		desiredActive[row.key()] = row
	}

	// Append the changed active rows (anti-flap: skip when the fingerprint is unchanged).
	for key, row := range desiredActive {
		if prev, ok := r.topologyCache.Load(key); ok && prev == row.fingerprint() {
			continue
		}
		if err := zettaSQL(ctx, base, token, "orama_control", insertTopologySQL(row, now)); err != nil {
			log.V(1).Info("zetta topology write deferred", "node", key, "err", err.Error())
			continue
		}
		r.topologyCache.Store(key, row.fingerprint())
	}

	// Gone detection: any cached node of THIS slice (same operator+cluster, node-name prefix) that is no
	// longer active AND whose ordinal is beyond the current wan range (scaled down, or the whole slice is no
	// longer wan-accessible → wanReplicas 0). A node still in range with a pending address is left as-is
	// (its last active row stands) so a transient LB blip never flaps it to gone.
	nodePrefix := stsName + "-"
	r.topologyCache.Range(func(k, v any) bool {
		key, _ := k.(string)
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 || parts[0] != operator || parts[1] != cluster {
			return true
		}
		node := parts[2]
		if !strings.HasPrefix(node, nodePrefix) {
			return true // a different slice's node (shared cache) — leave it to its own reconcile
		}
		if _, still := desiredActive[key]; still {
			return true // currently active — fine
		}
		ord, perr := strconv.Atoi(node[len(nodePrefix):])
		if perr != nil {
			return true
		}
		if ord < wanReplicas {
			return true // still within range → address pending, not gone
		}
		gone := topoRow{
			operator: operator, cluster: cluster, node: node,
			reachability: reachability, nodeType: nodeType, status: topoStatusGone,
		}
		if fp, _ := v.(string); fp == gone.fingerprint() {
			return true // already recorded gone — anti-flap
		}
		if err := zettaSQL(ctx, base, token, "orama_control", insertTopologySQL(gone, now)); err != nil {
			log.V(1).Info("zetta topology gone-marker deferred", "node", key, "err", err.Error())
			return true
		}
		r.topologyCache.Store(key, gone.fingerprint())
		log.Info("marked departed zetta node gone in cross-cluster topology", "node", node, "cluster", cluster)
		return true
	})
}

// deriveExternalPeers computes this slice's DERIVED --peer set from the full topology row set: latest-wins
// per node by updated_at, then the wan-accessible ACTIVE endpoints of OTHER operators sharing this cluster
// name — excluding this operator's own nodes, any non-wan/gone rows, and our own advertised addresses.
// Deterministic order (sorted by node key), addresses deduped.
func deriveExternalPeers(rows []topoRow, ourOperator, ourCluster string, ownAddrs map[string]bool) []string {
	// Latest-wins per (operator, cluster, node). updated_at is RFC3339 UTC, so a lexical compare is a time
	// compare (same format, same zone) — the same latest-wins contract the components read relies on.
	latest := map[string]topoRow{}
	for _, row := range rows {
		key := row.key()
		if cur, ok := latest[key]; !ok || row.updatedAt > cur.updatedAt {
			latest[key] = row
		}
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		row := latest[k]
		if row.cluster != ourCluster {
			continue // a different logical cluster (defensive — the SELECT already filters by cluster)
		}
		if row.operator == ourOperator {
			continue // our OWN slice's nodes — never peer to ourselves
		}
		if row.status != topoStatusActive {
			continue // departed (gone marker won latest-wins)
		}
		if row.reachability != oramav1alpha1.ReachabilityWANAccessible {
			continue // only a wan-accessible rendezvous is dialable (dial-out nodes advertise no address)
		}
		if row.address == "" || ownAddrs[row.address] || seen[row.address] {
			continue
		}
		seen[row.address] = true
		out = append(out, row.address)
	}
	return out
}

// deriveValidExternalPeers is deriveExternalPeers followed by peer-address validation (operator#279): it
// returns the derived peers that are a valid host:port, in order, and the derived addresses it dropped.
// A topology row is untrusted input — any operator sharing the Zetta cluster can append one.
func deriveValidExternalPeers(rows []topoRow, ourOperator, ourCluster string, ownAddrs map[string]bool) (valid, dropped []string) {
	trimmed := make([]topoRow, len(rows))
	for i, row := range rows {
		row.address = strings.TrimSpace(row.address)
		trimmed[i] = row
	}
	return splitValidPeers(deriveExternalPeers(trimmed, ourOperator, ourCluster, ownAddrs))
}

// topoRowsFromSQL maps the column→string result maps from zettaSQLQuery into topoRow values.
func topoRowsFromSQL(rows []map[string]string) []topoRow {
	out := make([]topoRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, topoRow{
			operator: r["operator"], cluster: r["cluster"], node: r["node"], address: r["address"],
			reachability: r["reachability"], nodeType: r["node_type"], status: r["status"],
			updatedAt: r["updated_at"],
		})
	}
	return out
}

// insertTopologySQL builds an append for one topology row, namespaced by operator + cluster.
func insertTopologySQL(row topoRow, now string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return fmt.Sprintf(
		"INSERT INTO topology (operator, cluster, node, address, reachability, node_type, status, updated_at) "+
			"VALUES (%s, %s, %s, %s, %s, %s, %s, %s)",
		q(row.operator), q(row.cluster), q(row.node), q(row.address),
		q(row.reachability), q(row.nodeType), q(row.status), q(now))
}

// selectTopologySQL reads every topology row for one global cluster name (latest-wins is applied on read in
// Go). Filtering by cluster keeps a big shared store's read small.
func selectTopologySQL(cluster string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return "SELECT operator, cluster, node, address, reachability, node_type, status, updated_at " +
		"FROM topology WHERE cluster = " + q(cluster)
}

// zettaPeerSet unions the explicit spec.externalPeers (the authoritative override, listed FIRST so it wins)
// with the topology-derived sibling endpoints (operator#105), trimming blanks and dropping duplicates while
// preserving order. This is the full --peer set rendered into the node entrypoint.
func zettaPeerSet(explicit, derived []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]string{explicit, derived} {
		for _, p := range group {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
