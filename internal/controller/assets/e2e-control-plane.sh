# SPDX-License-Identifier: MIT

# Orama Cloud v1 e2e — the control plane around the engine.
#
# Sourced by e2e-functional.sh after lib.sh; shipped beside it in the OramaCloudV1E2ETest ConfigMap
# as control-plane.sh. It defines functions the suite calls:
#
#   cp_prepare       early in the run: mints the write token the expiry check uses at the end, so the
#                    engine sections' run time counts toward its ten minutes
#   cp_dashboard     the dashboard: JWKS, write-token issuance refusals, session RBAC refusals, the
#                    CSRF guard and the Stripe webhook signature (operator#63)
#   cp_write_tokens  a write token from the fixture project, used on the writer: accepted on its own
#                    collection, refused on another tenant's, refused when tampered with or unsigned,
#                    refused past its plan's document cap, and the read-only member refused (operator#63)
#   cp_workers       the in-cluster edge workers: each refuses requests without a key or with a wrong
#                    one, and never serves them (operator#61)
#   cp_expiry        last: the token minted by cp_prepare, once expired, is refused (operator#63)
#
# Inputs (besides the functional suite's): config.json's dashboardURL, writerJWT and workers, and the
# dashboard fixture from the test's spec.dashboardFixtureSecretRef:
#   DASHBOARD_FIXTURE_COLLECTION_ID, DASHBOARD_FIXTURE_INDEX_ID, DASHBOARD_FIXTURE_API_KEY
#     a dashboard project of this stack the suite may write to, and its private API key
#   DASHBOARD_FIXTURE_READER_SESSION (optional)
#     the _orama_session cookie of a member holding only read access to that project
#   E2E_JWT_EXPIRY_MAX_WAIT (optional, seconds, default 900): the longest cp_expiry waits
#
# Never weaken an assertion here to make the suite pass (operator CLAUDE.md, principle 9). Every
# secret stays in a variable: none is printed, and response bodies are cut before they are logged.

DASH=$(jq -r '.dashboardURL // ""' "$CONFIG")
WRITER_JWT=$(jq -r '.writerJWT // false' "$CONFIG")
FX_COLL="${DASHBOARD_FIXTURE_COLLECTION_ID:-}"
FX_INDEX="${DASHBOARD_FIXTURE_INDEX_ID:-}"
FX_KEY="${DASHBOARD_FIXTURE_API_KEY:-}"
FX_READER_SESSION="${DASHBOARD_FIXTURE_READER_SESSION:-}"
SESSION_COOKIE=_orama_session
# The writer verifies tokens with jwt-authorizer's default 60 s leeway (oramacore src/auth/jwt_manager.rs).
# E2E_JWT_LEEWAY overrides it only for the operator's local harness of this script.
JWT_LEEWAY="${E2E_JWT_LEEWAY:-60}"
EXP_JWT=""
EXP_AT=0

# mint_jwt COLLECTION PRIVATE_KEY — POST the dashboard's /api/user/jwt; $CODE and $RESP as req.
# The key travels in the body, built by jq from the environment so it is never an argument.
mint_jwt() {
  _body=$(MINT_C="$1" MINT_K="$2" jq -nc '{collectionId: env.MINT_C, privateApiKey: env.MINT_K, scope: "write"}')
  reqh POST "$DASH/api/user/jwt" "$_body" -H 'Content-Type: application/json'
}

# jwt_write TOKEN COLLECTION INDEX DOCS_JSON — insert with a bearer token on the in-cluster writer.
jwt_write() { req POST "$WRITER/v1/collections/$2/indexes/$3/insert" "$1" "$4"; }

# cp_fixture_ready — the dashboard runs, the writer verifies its tokens, and the fixture is given.
cp_fixture_ready() { [ -n "$DASH" ] && [ "$WRITER_JWT" = "true" ] && [ -n "$FX_COLL" ] && [ -n "$FX_INDEX" ] && [ -n "$FX_KEY" ]; }

# cp_fixture_skip DESC — skip with the precise reason the fixture checks cannot run.
cp_fixture_skip() {
  if [ -z "$DASH" ]; then skip "$1" "the target runs no dashboard (spec.components.dashboard.enabled is false)"
  elif [ "$WRITER_JWT" != "true" ]; then skip "$1" "the target's writer does not verify dashboard write tokens (spec.components.engine.writer.jwt is unset)"
  else skip "$1" "no dashboard fixture: set the test's spec.dashboardFixtureSecretRef (collectionID, indexID, apiKey of a project of this stack)"; fi
}

# =============================================================================
cp_prepare() {
  cp_fixture_ready || return 0
  mint_jwt "$FX_COLL" "$FX_KEY"
  EXP_JWT=$(printf '%s' "$RESP" | jq -r '.jwt // empty' 2>/dev/null)
  if [ -n "$EXP_JWT" ]; then
    EXP_AT=$(jwt_part "$EXP_JWT" 2 | jq -r '.exp // 0' 2>/dev/null)
    case "$EXP_AT" in ''|*[!0-9]*) EXP_AT=0;; esac
  fi
}

# =============================================================================
cp_dashboard() {
  section "21. DASHBOARD — JWKS, write-token issuance, session RBAC, CSRF (operator#63)"
  if [ -z "$DASH" ]; then
    skip "dashboard control plane" "the target runs no dashboard (spec.components.dashboard.enabled is false)"
    return 0
  fi

  # The JWKS the writer verifies write tokens with: an RSA public key, never a private member.
  reqh GET "$DASH/api/.well-known/jwks.json" ""
  assert_code 200 "JWKS served"
  assert_jq '(.keys | length) >= 1 and all(.keys[]; .kty == "RSA" and (.n | length) > 0 and (.e | length) > 0)' "JWKS holds RSA public keys"
  assert_jq '[.keys[] | (has("d") or has("p") or has("q") or has("dp") or has("dq") or has("qi"))] | any | not' "JWKS leaks no private key member"

  # Write-token issuance refuses what it must, and never hands out a token while refusing.
  reqh POST "$DASH/api/user/jwt" '{"collectionId":"e2e","privateApiKey":"e2e","scope":"read"}' -H 'Content-Type: application/json'
  assert_code 400 "write token: scope other than write refused"
  assert_jq '(.jwt // null) == null' "write token: no token for a refused scope"
  reqh POST "$DASH/api/user/jwt" '{"scope":"write"}' -H 'Content-Type: application/json'
  assert_not_2xx "write token: no private API key refused (not 200-with-error)"
  assert_jq '(.jwt // null) == null' "write token: no token without a private API key"
  mint_jwt "${FX_COLL:-$COLL}" "e2e-not-a-private-api-key"
  assert_not_2xx "write token: wrong private API key refused"
  assert_jq '(.jwt // null) == null' "write token: no token for a wrong private API key"
  if [ -n "$FX_KEY" ]; then
    # The fixture key is valid only for its own project: another tenant's collection gets nothing.
    mint_jwt "$COLL" "$FX_KEY"
    assert_not_2xx "write token: a project's key refused for another tenant's collection"
    assert_jq '(.jwt // null) == null' "write token: no token for another tenant's collection"
  fi

  # Session routes refuse a caller with no session, or a forged one (dashboard#106 for the routes
  # that once had no check: indexes update, airbyte/*; private/v1 needs PRIVATE_API_KEY).
  reqh GET "$DASH/api/user/teams" "";                                   assert_refused "no session: GET /api/user/teams"
  reqh GET "$DASH/api/projects/get-all?teamId=e2e" "";                  assert_refused "no session: GET /api/projects/get-all"
  reqh GET "$DASH/api/user/private-api-key/get-for-project?projectId=$COLL" ""
  assert_refused "no session: private API key not handed out"
  assert_jq '(.privateApiKey // null) == null' "no session: no private API key in the body"
  reqh GET "$DASH/api/projects/$COLL/indexes/get" "";                   assert_refused "no session: GET project indexes (read access)"
  reqh POST "$DASH/api/projects/create" '{}' -H 'Content-Type: application/json'
  assert_refused "no session: POST /api/projects/create"
  reqh POST "$DASH/api/user/private-api-key/regenerate" '{}' -H 'Content-Type: application/json'
  assert_refused "no session: POST private-api-key/regenerate"
  reqh PUT "$DASH/api/projects/$COLL/indexes/$INDEX/update" '{}' -H 'Content-Type: application/json'
  assert_refused "no session: PUT indexes/<id>/update (dashboard#106)"
  reqh GET "$DASH/api/airbyte/sources" "";                              assert_refused "no session: GET /api/airbyte/sources (dashboard#106)"
  reqh GET "$DASH/api/private/v1/get-job?jobId=e2e-none" "";            assert_refused "no PRIVATE_API_KEY: GET /api/private/v1/get-job"
  reqh GET "$DASH/api/user/teams" "" -H "Cookie: $SESSION_COOKIE=e2e.forged.session"
  assert_refused "forged session cookie refused"

  # CSRF guard (src/middleware.ts): a state-changing call from a foreign origin, or not JSON.
  reqh POST "$DASH/api/user/private-api-key/regenerate" '{}' -H 'Content-Type: application/json' -H 'Origin: https://e2e-attacker.example'
  assert_code 403 "CSRF: foreign Origin refused on a state-changing call"
  reqh POST "$DASH/api/user/private-api-key/regenerate" 'x=1' -H 'Content-Type: text/plain'
  assert_code 415 "CSRF: non-JSON body refused on a state-changing call"

  # The Stripe webhook acts only on a signed event.
  reqh POST "$DASH/api/webhooks/stripe" '{"type":"customer.subscription.deleted"}' -H 'Content-Type: application/json'
  assert_code 400 "Stripe webhook: unsigned event refused"
  reqh POST "$DASH/api/webhooks/stripe" '{"type":"customer.subscription.deleted"}' -H 'Content-Type: application/json' -H 'stripe-signature: t=1,v1=00'
  assert_not_2xx "Stripe webhook: badly signed event refused"
}

# =============================================================================
cp_write_tokens() {
  section "22. WRITE TOKENS — issuance, tenancy, tampering, plan cap, role (operator#63)"
  if ! cp_fixture_ready; then
    cp_fixture_skip "write-token checks"
    return 0
  fi

  mint_jwt "$FX_COLL" "$FX_KEY"
  assert_code 200 "write token issued for the fixture project"
  _jwt=$(printf '%s' "$RESP" | jq -r '.jwt // empty' 2>/dev/null)
  _wurl=$(printf '%s' "$RESP" | jq -r '.writerURL // empty' 2>/dev/null)
  _rkey=$(printf '%s' "$RESP" | jq -r '.readerApiKey // empty' 2>/dev/null)
  if [ -z "$_jwt" ]; then
    fail "write token: response carries a token" "no .jwt: $(shown 200)"
    return 0
  fi
  RESP=$(jwt_part "$_jwt" 1)
  assert_jq_eq '.alg' "RS256" "write token: signed RS256"
  _kid=$(printf '%s' "$RESP" | jq -r '.kid // empty')
  RESP=$(jwt_part "$_jwt" 2)
  assert_jq_eq '.sub' "$FX_COLL" "write token: sub is the fixture project"
  assert_jq_eq '.scope' "write" "write token: scope write"
  assert_jq '(.lmt.max_doc_count | type) == "number"' "write token: carries the plan cap lmt.max_doc_count"
  assert_jq '(.exp - .iat) > 0 and (.exp - .iat) <= 600' "write token: lives at most ten minutes"
  assert_jq '(.iss | length) > 0' "write token: names its issuer"
  assert_jq_eq '.aud' "$_wurl" "write token: audience is the project's writer URL"
  _max=$(printf '%s' "$RESP" | jq -r '.lmt.max_doc_count // empty')
  if [ -n "$_kid" ]; then
    reqh GET "$DASH/api/.well-known/jwks.json" ""
    assert_jq "any(.keys[]; .kid == \"$_kid\")" "write token: its kid is in the JWKS"
  fi

  # Accepted on its own collection: the refusals below mean something only if this passes.
  _probe='[{"id":"e2e-jwt-probe","title":"e2e write-token probe"}]'
  jwt_write "$_jwt" "$FX_COLL" "$FX_INDEX" "$_probe"
  assert_ok "write token: accepted on its own collection"
  req POST "$WRITER/v1/collections/$FX_COLL/indexes/$FX_INDEX/delete" "$_jwt" '["e2e-jwt-probe"]'
  assert_ok "write token: deletes its probe document"

  # Refused on another tenant's collection (the suite's own), and nothing lands there.
  jwt_write "$_jwt" "$COLL" "$INDEX" '[{"id":"e2e-cross-tenant","title":"must not land"}]'
  assert_not_2xx "write token: refused on another tenant's collection"
  case "$CODE" in 400|401|403) pass "write token: cross-tenant refusal is a client error (HTTP $CODE)";; *) fail "write token: cross-tenant refusal is a client error" "HTTP $CODE";; esac
  # Give a wrongly accepted write the propagation budget to reach the reader before saying none did.
  # E2E_PROPAGATION_WAIT shortens it only for the operator's local harness of this script.
  if wait_until "curl -sS -m 10 -X POST \"$READER/v1/collections/$COLL/search\" -H \"Authorization: Bearer $RWKEY\" -H 'Content-Type: application/json' -d '{\"term\":\"must not land\",\"exact\":true}' | jq -e '(.count // 0) > 0' >/dev/null" "${E2E_PROPAGATION_WAIT:-10}"; then
    fail "write token: nothing landed in another tenant's collection" "a cross-tenant document is searchable"
  else
    pass "write token: nothing landed in another tenant's collection"
  fi

  # Tampered (sub rewritten to another tenant, signature kept), unsigned (alg none), and garbage.
  _h=$(printf '%s' "$_jwt" | cut -d. -f1)
  _s=$(printf '%s' "$_jwt" | cut -d. -f3)
  _p=$(jwt_part "$_jwt" 2 | jq -c --arg c "$COLL" '.sub = $c' | b64url_encode)
  jwt_write "$_h.$_p.$_s" "$COLL" "$INDEX" '[{"id":"e2e-tampered","title":"must not land"}]'
  assert_code 401 "write token: tampered payload refused"
  _none=$(printf '{"alg":"none","typ":"JWT"}' | b64url_encode)
  jwt_write "$_none.$_p." "$COLL" "$INDEX" '[{"id":"e2e-unsigned","title":"must not land"}]'
  assert_code 401 "write token: unsigned (alg none) refused"
  jwt_write "eyJlMmUiOjF9.eyJlMmUiOjF9.e2e" "$FX_COLL" "$FX_INDEX" '[{"id":"e2e-garbage","title":"must not land"}]'
  assert_code 401 "write token: garbage token refused"

  # Wrong plan: one document past the plan's cap is refused with 402, and none of the batch lands.
  reqh GET "$READER/v1/collections/$FX_COLL/stats?api-key=$_rkey" ""
  _count=$(printf '%s' "$RESP" | jq -r '.document_count // empty' 2>/dev/null)
  case "$_max:$_count" in
    *[!0-9:]*|:*|*:)
      skip "plan cap: one document past lmt.max_doc_count refused" "no document_count from the fixture's stats (HTTP $CODE)";;
    *)
      _need=$((_max - _count + 1))
      if [ "$_need" -lt 1 ]; then
        skip "plan cap: one document past lmt.max_doc_count refused" "the fixture is already past its cap ($_count of $_max)"
      elif [ "$_need" -gt 2000 ]; then
        skip "plan cap: one document past lmt.max_doc_count refused" "the fixture's plan allows $_max documents; exceeding it would take $_need inserts (use a free-plan fixture)"
      else
        _batch=$(jq -nc --argjson n "$_need" '[range(0; $n) | {id: "e2e-plan-\(.)", title: "e2e plan cap \(.)"}]')
        jwt_write "$_jwt" "$FX_COLL" "$FX_INDEX" "$_batch"
        assert_code 402 "plan cap: $_need documents onto $_count of $_max refused (402)"
        case "$CODE" in 2*)
          # The cap did not hold: remove what landed so the fixture stays under its cap.
          req POST "$WRITER/v1/collections/$FX_COLL/indexes/$FX_INDEX/delete" "$_jwt" "$(printf '%s' "$_batch" | jq -c '[.[].id]')";;
        esac
      fi;;
  esac

  # Wrong role: a member with read access only must not get the project's private API key (which
  # mints write tokens), nor reach a write route; and no project of another tenant at all.
  if [ -z "$FX_READER_SESSION" ]; then
    skip "wrong role: read-only member refused" "no readerSessionCookie in the dashboard fixture Secret"
  else
    _cookie="Cookie: $SESSION_COOKIE=$FX_READER_SESSION"
    reqh GET "$DASH/api/user/teams" "" -H "$_cookie"
    if [ "$CODE" != "200" ]; then
      fail "wrong role: the fixture's read-only session is valid" "GET /api/user/teams HTTP $CODE (the cookie lasts seven days; renew readerSessionCookie)"
    else
      reqh GET "$DASH/api/user/private-api-key/get-for-project?projectId=$FX_COLL" "" -H "$_cookie"
      assert_refused "wrong role: read-only member gets no private API key"
      assert_jq '(.privateApiKey // null) == null' "wrong role: no private API key in the body"
      reqh PUT "$DASH/api/projects/$FX_COLL/indexes/$FX_INDEX/update" '{}' -H 'Content-Type: application/json' -H "$_cookie"
      assert_code 403 "wrong role: read-only member refused on a write route"
      reqh GET "$DASH/api/projects/$COLL/indexes/get" "" -H "$_cookie"
      assert_code 403 "wrong tenant: member refused on another tenant's project"
    fi
  fi
}

# =============================================================================
# worker_refuses WORKER DESC METHOD URL BODY [HEADER...] — an unauthenticated or wrongly keyed call
# to an edge worker. A 2xx is always a failure: the worker served it (the openai-decrypter's 200 on
# every failure and the segmenter's fail-open are such defects). The in-cluster scaffold's explicit
# 501 "not yet wired in-cluster" is skipped with its tracker; any other answer than 401 or 403 fails.
worker_refuses() {
  _w=$1; _d=$2; shift 2
  reqh "$@"
  case "$CODE" in
    2*) fail "$_w: $_d" "the worker served it: HTTP $CODE $(shown 200)";;
    401|403) pass "$_w: $_d (HTTP $CODE)";;
    501)
      if printf '%s' "$RESP" | jq -e '(.error // "") | test("not yet wired in-cluster")' >/dev/null 2>&1; then
        skip "$_w: $_d" "in-cluster scaffold; route not wired yet (operator#6)"
      else fail "$_w: $_d" "HTTP 501: $(shown 200)"; fi;;
    *) fail "$_w: $_d" "expected HTTP 401 or 403, got $CODE: $(shown 200)";;
  esac
}

cp_workers() {
  section "23. EDGE WORKERS — auth refusals per worker (operator#61)"
  _n=$(jq -r '(.workers // []) | length' "$CONFIG")
  if [ "$_n" -eq 0 ]; then
    skip "edge workers" "the target runs neither the workers bundle nor the hosted-search workers"
  fi
  _i=0
  while [ "$_i" -lt "$_n" ]; do
    _name=$(jq -r ".workers[$_i].name" "$CONFIG")
    _url=$(jq -r ".workers[$_i].url" "$CONFIG")
    _i=$((_i + 1))
    reqh GET "$_url/health" ""
    assert_code 200 "$_name: /health"
    case "$_name" in
      orama-workers)
        # worker-segmenter: X-API-KEY on /v1/* (its check compares c.req.url, a full URL, to '/v1/').
        worker_refuses "$_name" "segmenter refuses no X-API-KEY" POST "$_url/v1/segment" '{"text":"e2e"}' -H 'Content-Type: application/json'
        worker_refuses "$_name" "segmenter refuses a wrong X-API-KEY" POST "$_url/v1/segment" '{"text":"e2e"}' -H 'Content-Type: application/json' -H 'X-API-KEY: e2e-wrong'
        ;;
      openai-decrypter)
        # x-api-key must equal API_SECRET; it hands out decrypted provider keys.
        worker_refuses "$_name" "refuses no x-api-key" GET "$_url/?ref=e2e" ""
        worker_refuses "$_name" "refuses a wrong x-api-key" GET "$_url/?ref=e2e" "" -H 'x-api-key: e2e-wrong'
        if printf '%s' "$RESP" | jq -e 'has("apiKey")' >/dev/null 2>&1; then
          fail "$_name: hands out no apiKey to a wrong x-api-key" "the body carries apiKey"
        fi
        ;;
      telemetry-worker)
        worker_refuses "$_name" "refuses /collect without api-key" POST "$_url/collect" '{"index":"e2e","events":[]}' -H 'Content-Type: application/json'
        worker_refuses "$_name" "refuses /collect with a wrong api-key" POST "$_url/collect?api-key=e2e-wrong" '{"index":"e2e","events":[]}' -H 'Content-Type: application/json'
        ;;
      large-index-worker)
        worker_refuses "$_name" "refuses search without api-key" POST "$_url/v1/indexes/e2e/search" '{"q":"e2e"}' -H 'Content-Type: application/json'
        worker_refuses "$_name" "refuses search with a wrong api-key" POST "$_url/v1/indexes/e2e/search?api-key=e2e-wrong" '{"q":"e2e"}' -H 'Content-Type: application/json'
        ;;
      index-shared-worker)
        worker_refuses "$_name" "refuses a request without api-key" GET "$_url/search?q=e2e" ""
        worker_refuses "$_name" "refuses a wrong api-key" GET "$_url/search?q=e2e&api-key=e2e-wrong" ""
        ;;
      *) skip "$_name: auth refusals" "no auth matrix defined for this worker";;
    esac
  done
  # Edge workers the operator does not deploy: they run on Cloudflare, and their own repositories'
  # tests cover them (operator#61).
  for _w in secure-proxy worker-mcp worker-telemetry; do
    skip "$_w: auth refusals" "not deployed by the operator (runs on Cloudflare); covered in its repository"
  done
}

# =============================================================================
cp_expiry() {
  section "24. EXPIRED write token (operator#63)"
  if ! cp_fixture_ready; then
    cp_fixture_skip "expired write token refused"
    return 0
  fi
  if [ -z "$EXP_JWT" ] || [ "$EXP_AT" -eq 0 ]; then
    fail "expired write token refused" "cp_prepare minted no token with an exp claim"
    return 0
  fi
  _until=$((EXP_AT + JWT_LEEWAY + 2))
  _wait=$((_until - $(date +%s)))
  _max=${E2E_JWT_EXPIRY_MAX_WAIT:-900}
  if [ "$_wait" -gt "$_max" ]; then
    skip "expired write token refused" "it expires in ${_wait}s, past E2E_JWT_EXPIRY_MAX_WAIT=${_max}s"
    return 0
  fi
  if [ "$_wait" -gt 0 ]; then
    printf '  ....  waiting %ss for the write token to expire (exp + %ss leeway)\n' "$_wait" "$JWT_LEEWAY"
    sleep "$_wait"
  fi
  jwt_write "$EXP_JWT" "$FX_COLL" "$FX_INDEX" '[{"id":"e2e-expired","title":"must not land"}]'
  assert_code 401 "expired write token refused"
  assert_jq '(.message // "") | test("expired"; "i")' "expired write token: the writer says it expired"
}
