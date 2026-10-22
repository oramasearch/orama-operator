# Orama Cloud v1 e2e — the HTTP and assertion helpers both suite scripts use.
#
# Sourced by e2e-functional.sh (and through it by e2e-control-plane.sh); shipped beside them in the
# OramaCloudV1E2ETest ConfigMap as lib.sh. The caller initialises PASS, FAIL, SKIP, RESP and CODE.
# Requires: sh, curl, jq.

# req METHOD URL KEY [JSON_BODY] -> sets $CODE (http status) and $RESP (body)
req() {
  _m=$1; _u=$2; _k=$3; _b=${4:-}
  if [ -n "$_b" ]; then
    RESP=$(curl -sS -m 45 -w '\n%{http_code}' -X "$_m" "$_u" \
      -H "Authorization: Bearer $_k" -H 'Content-Type: application/json' -d "$_b" 2>/dev/null)
  else
    RESP=$(curl -sS -m 45 -w '\n%{http_code}' -X "$_m" "$_u" \
      -H "Authorization: Bearer $_k" 2>/dev/null)
  fi
  CODE=$(printf '%s' "$RESP" | tail -n1)
  RESP=$(printf '%s' "$RESP" | sed '$d')
}

pass()    { PASS=$((PASS + 1)); printf '  PASS  %s\n' "$1"; }
fail()    { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; [ -n "${2:-}" ] && printf '          -> %s\n' "$2"; }
skip()    { SKIP=$((SKIP + 1)); printf '  SKIP  %s\n' "$1"; [ -n "${2:-}" ] && printf '          -> %s\n' "$2"; }
section() { printf '\n========== %s ==========\n' "$1"; }

# shown [N] — $RESP cut to N bytes (default 400) for a failure message, with every credential
# value (privateApiKey, apiKey, readerApiKey, writerApiKey, jwt) replaced, so a refusal that
# wrongly hands one out never puts it in the Job's logs.
shown() {
  printf '%s' "$RESP" | sed -E 's/"(privateApiKey|apiKey|readerApiKey|writerApiKey|jwt)"[[:space:]]*:[[:space:]]*"[^"]*"/"\1":"<redacted>"/g' | head -c "${1:-400}"
}

# assert_code EXPECTED DESC
assert_code() {
  if [ "$CODE" = "$1" ]; then pass "$2 (HTTP $CODE)"
  else fail "$2" "expected HTTP $1, got $CODE: $(shown 400)"; fi
}

# assert_ok DESC  (any 2xx)
assert_ok() {
  case "$CODE" in 2*) pass "$1 (HTTP $CODE)";; *) fail "$1" "got HTTP $CODE: $(shown 400)";; esac
}

# assert_jq FILTER DESC  — filter result must be non-empty, not "null", not "false"
assert_jq() {
  _v=$(printf '%s' "$RESP" | jq -r "$1" 2>/dev/null)
  if [ -n "$_v" ] && [ "$_v" != "false" ] && [ "$_v" != "null" ]; then pass "$2 ($1 => $_v)"
  else fail "$2" "jq '$1' => '${_v}' on: $(shown 400)"; fi
}

# assert_jq_eq FILTER EXPECTED DESC
assert_jq_eq() {
  _v=$(printf '%s' "$RESP" | jq -r "$1" 2>/dev/null)
  if [ "$_v" = "$2" ]; then pass "$3 ($1 == $2)"
  else fail "$3" "jq '$1' => '${_v}', expected '$2' on: $(shown 400)"; fi
}

# wait_until 'shell test' timeout_s  — polls every 2s until the command succeeds
wait_until() {
  _t=$2; _n=0
  while [ "$_n" -lt "$_t" ]; do
    if eval "$1" >/dev/null 2>&1; then return 0; fi
    _n=$((_n + 2)); sleep 2
  done
  return 1
}

# reqh METHOD URL [BODY] [HEADER...] -> $CODE and $RESP, with the given headers and no Authorization.
# For routes that authenticate by cookie, x-api-key, query string or not at all.
reqh() {
  _m=$1; _u=$2; _b=${3:-}
  if [ "$#" -ge 3 ]; then shift 3; else shift "$#"; fi
  set -- -sS -m 45 -w '\n%{http_code}' -X "$_m" "$_u" "$@"
  if [ -n "$_b" ]; then set -- "$@" -d "$_b"; fi
  RESP=$(curl "$@" 2>/dev/null)
  CODE=$(printf '%s' "$RESP" | tail -n1)
  RESP=$(printf '%s' "$RESP" | sed '$d')
}

# b64url_decode — base64url on stdin (no padding) to bytes on stdout.
b64url_decode() {
  _s=$(tr -d '\n' | tr '_-' '/+')
  case $(( ${#_s} % 4 )) in 2) _s="$_s==";; 3) _s="$_s=";; esac
  printf '%s' "$_s" | base64 -d 2>/dev/null
}

# b64url_encode — bytes on stdin to unpadded base64url on stdout.
b64url_encode() { base64 | tr -d '\n=' | tr '/+' '_-'; }

# jwt_part TOKEN N — the decoded JSON of a JWT's header (1) or payload (2).
jwt_part() { printf '%s' "$1" | cut -d. -f"$2" | b64url_decode; }

# assert_refused DESC — the request was refused: HTTP 401 or 403.
assert_refused() {
  case "$CODE" in 401|403) pass "$1 (HTTP $CODE)";; *) fail "$1" "expected HTTP 401 or 403, got $CODE: $(shown 300)";; esac
}

# assert_not_2xx DESC — the request did not succeed.
assert_not_2xx() {
  case "$CODE" in 2*) fail "$1" "expected a refusal, got HTTP $CODE: $(shown 300)";; *) pass "$1 (HTTP $CODE)";; esac
}
