# SPDX-License-Identifier: MIT

# The e2e canary and its collection prune: one run of CronJob <cr>-e2e-canary (operator#17), run by
# /bin/sh in the canary image, which provides monitor-test, curl and jq. Ported from Cloud v1's
# original canary timer and collection prune crons.
#
# 1. monitor-test signs in through the dashboard's /api/user/jwt with the canary project's private
#    API key, inserts a probe document, checks it on every reader and deletes the oldest tracked one.
#    It keeps the titles it tracks in test-state.json in its working directory, the state volume.
# 2. The prune runs once a day: in the first run at or after 04:30 UTC, or the first run after a
#    missed day (Prod 1's OnCalendar=04:30 UTC with Persistent=true). When the canary collection holds
#    more than 200 documents it deletes the leaked `monitor <hex>` probes, keeping the titles
#    monitor-test tracks, so the collection stays under its 500-document plan cap. It refuses to
#    prune with an empty keep-list, which would delete the canary's live documents.
#
# Both write Prometheus textfile metrics under $E2E_CANARY_STATE_DIR/metrics. Secrets are read from
# the environment inside jq and reach curl on stdin, never as arguments; nothing logs a secret or a
# response body. The run fails when either part fails.
#
# Environment: E2E_CANARY_COLLECTION_ID, E2E_CANARY_INDEX_ID, E2E_CANARY_API_KEY (from the Secret),
# E2E_CANARY_JWT_URL, E2E_CANARY_READER_URL, E2E_CANARY_STATE_DIR, E2E_CANARY_RUN_DIR.
set -u
set +x
umask 077

state=$E2E_CANARY_STATE_DIR
metrics=$state/metrics
config=$E2E_CANARY_RUN_DIR/config.json
cid=$E2E_CANARY_COLLECTION_ID
iid=$E2E_CANARY_INDEX_ID
prune_above=200
batch=50
started=$(date -u +%s)

log() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*"; }

mkdir -p "$metrics" || exit 1

# ------------------------------------------------------------------ canary
jq -n --arg metrics "$metrics/oramacore-test-e2e.prom" '{
  authJwtURL: env.E2E_CANARY_JWT_URL,
  collectionID: env.E2E_CANARY_COLLECTION_ID,
  indexID: env.E2E_CANARY_INDEX_ID,
  apiKey: env.E2E_CANARY_API_KEY,
  additionalReaderURLs: [{name: "in-cluster reader", url: env.E2E_CANARY_READER_URL}],
  metricsFilePath: $metrics
}' > "$config" || { log "ERROR: cannot render the canary config"; exit 1; }

(cd "$state" && CONFIG_PATH="$config" monitor-test)
canary=$?
log "canary exit $canary"

# ------------------------------------------------------------------ prune
emit() { # $1 success (0/1), $2 document count at the start ("" when unknown), $3 deleted
	tmp=$(mktemp "$metrics/.prune.XXXXXX") || return 1
	{
		echo "# HELP oramacore_collection_prune_success Whether the last auto-prune run completed (1) or failed (0)"
		echo "# TYPE oramacore_collection_prune_success gauge"
		echo "oramacore_collection_prune_success $1"
		echo "# HELP oramacore_collection_prune_last_count Document count observed at the start of the last run"
		echo "# TYPE oramacore_collection_prune_last_count gauge"
		if [ -n "$2" ]; then echo "oramacore_collection_prune_last_count{collection_id=\"$cid\"} $2"; fi
		echo "# HELP oramacore_collection_prune_last_deleted Documents deleted on the last run"
		echo "# TYPE oramacore_collection_prune_last_deleted gauge"
		echo "oramacore_collection_prune_last_deleted $3"
		echo "# HELP oramacore_collection_prune_last_run_timestamp Unix time of the last run"
		echo "# TYPE oramacore_collection_prune_last_run_timestamp gauge"
		echo "oramacore_collection_prune_last_run_timestamp $(date -u +%s)"
	} > "$tmp"
	chmod 644 "$tmp"
	mv "$tmp" "$metrics/oramacore-collection-prune.prom"
}

prune() {
	resp=$(jq -nc '{collectionId: env.E2E_CANARY_COLLECTION_ID, privateApiKey: env.E2E_CANARY_API_KEY, scope: "write"}' |
		curl -s -m 25 -X POST "$E2E_CANARY_JWT_URL" -H 'Content-Type: application/json' --data-binary @-)
	jwt=$(printf '%s' "$resp" | jq -r '.jwt // empty' 2>/dev/null)
	rkey=$(printf '%s' "$resp" | jq -r '.readerApiKey // empty' 2>/dev/null)
	rurl=$(printf '%s' "$resp" | jq -r '.readerURL // empty' 2>/dev/null)
	wurl=$(printf '%s' "$resp" | jq -r '.writerURL // empty' 2>/dev/null)
	if [ -z "$jwt" ] || [ -z "$rkey" ] || [ -z "$rurl" ] || [ -z "$wurl" ]; then
		log "ERROR: prune: JWT exchange failed"; emit 0 "" 0; return 1
	fi

	# readerApiKey is the collection's public read key, which the engine takes in the query string.
	stats() { curl -s -m 20 "$rurl/v1/collections/$cid/stats?api-key=$rkey"; }

	count=$(stats | jq -r '.document_count // empty' 2>/dev/null)
	case $count in ''|*[!0-9]*) log "ERROR: prune: no document_count in stats"; emit 0 "" 0; return 1 ;; esac
	log "prune: document_count=$count (prune_above=$prune_above, cap=500)"
	if [ "$count" -le "$prune_above" ]; then
		log "prune: at or below the threshold; nothing to prune"; emit 1 "$count" 0; return 0
	fi

	keep=$(mktemp) && ids=$(mktemp) && dir=$(mktemp -d) || { emit 0 "$count" 0; return 1; }
	jq -r '.documentTitles[]? // empty' "$state/test-state.json" > "$keep" 2>/dev/null
	if [ ! -s "$keep" ]; then
		log "ERROR: prune: no tracked titles in test-state.json; refusing to prune (an empty keep-list would delete live documents)"
		rm -rf "$keep" "$ids" "$dir"; emit 0 "$count" 0; return 1
	fi

	# Each leaked `monitor <hex>` title, resolved to its document id; tracked titles are kept.
	stats | grep -oE 'monitor [0-9a-f]{8,}' | sort -u | while IFS= read -r title; do
		grep -qxF "$title" "$keep" && continue
		id=$(jq -nc --arg t "$title" '{term: "", where: {title: $t}}' |
			curl -s -m 12 -X POST "$rurl/v1/collections/$cid/search?api-key=$rkey" -H 'Content-Type: application/json' --data-binary @- |
			jq -r '.hits[0].id // empty' 2>/dev/null)
		if [ -n "$id" ]; then echo "$id" >> "$ids"; fi
	done

	deleted=0 failed=0
	if [ -s "$ids" ]; then
		split -l "$batch" "$ids" "$dir/batch."
		for b in "$dir"/batch.*; do
			jq -R . "$b" | jq -sc . > "$dir/body.json"
			http=$(printf 'Authorization: Bearer %s\n' "$jwt" |
				curl -s -m 30 -o /dev/null -w '%{http_code}' -X POST "$wurl/v1/collections/$cid/indexes/$iid/delete" \
					-H 'Content-Type: application/json' --header @- --data-binary @"$dir/body.json")
			if [ "$http" = 200 ]; then
				deleted=$((deleted + $(wc -l < "$b")))
			else
				failed=1; log "WARN: prune: a delete batch returned HTTP $http"
			fi
		done
	fi
	rm -rf "$keep" "$ids" "$dir"

	after=$(stats | jq -r '.document_count // empty' 2>/dev/null)
	log "prune: deleted $deleted documents; document_count $count -> ${after:-unknown} (failed batches: $failed)"
	# A failed batch fails the run, so an ineffective prune is visible.
	if [ "$failed" -eq 0 ]; then emit 1 "$count" "$deleted"; return 0; fi
	emit 0 "$count" "$deleted"; return 1
}

last=$(cat "$state/prune-last-run" 2>/dev/null)
case $last in ''|*[!0-9]*) last=0 ;; esac
slot=$((started - (started - 16200) % 86400)) # the latest 04:30 UTC at or before the start
pruned=0
if [ "$last" -lt "$slot" ]; then
	echo "$started" > "$state/prune-last-run"
	prune
	pruned=$?
	log "prune exit $pruned"
fi

[ "$canary" -eq 0 ] && [ "$pruned" -eq 0 ] || exit 1
