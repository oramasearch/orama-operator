// SPDX-License-Identifier: MIT

package controller

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The control-plane sections of the functional e2e suite (assets/e2e-control-plane.sh, operator#61
// and #63) run here against local doubles of the dashboard, the writer, the reader and the edge
// workers. A correct stack must give fail=0 with every refusal asserted; each defect the issues name
// (a worker serving an unauthenticated call, an unauthenticated dashboard route, a write token
// accepted on another tenant's collection or past its plan cap, a read-only member handed the private
// API key) must make the suite fail. The live run against a stack is the OramaCloudV1E2ETest itself.

const (
	cpFixtureColl    = "fixture-coll"
	cpFixtureIndex   = "fixture-index"
	cpFixtureKey     = "fixture-private-key"
	cpReaderSession  = "reader-session"
	cpReaderAPIKey   = "fixture-read-key"
	cpSuiteColl      = "e2e-test"
	cpSuiteIndex     = "e2e-index"
	cpSuiteKey       = "e2e-rw-key"
	cpMaxDocs        = 5
	cpWriterAudience = "https://writer.example"
)

// cpDefects turns on the defects a correct stack does not have.
type cpDefects struct {
	workersServe        bool // every wired worker answers 200 to an unauthenticated call
	dashboardOpenUpdate bool // PUT indexes/<id>/update runs with no session
	writerIgnoresSub    bool // the writer accepts a token on any collection
	writerIgnoresCap    bool // the writer ignores lmt.max_doc_count
	readerGetsKey       bool // a read-only member is handed the private API key
}

type cpStack struct {
	t       *testing.T
	key     *rsa.PrivateKey
	defects cpDefects
	mu      sync.Mutex
	docs    map[string]map[string]bool // collection -> document ids
	mints   int
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s *cpStack) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	p, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		s.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

// verify returns the claims of a valid RS256 token, or an HTTP status and message.
func (s *cpStack) verify(token string) (map[string]any, int, string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, 401, "JWT Auth error: malformed"
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	var h, c map[string]any
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(hb, &h) != nil || json.Unmarshal(pb, &c) != nil {
		return nil, 401, "JWT Auth error: undecodable"
	}
	if h["alg"] != "RS256" {
		return nil, 401, "JWT Auth error: algorithm"
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return nil, 401, "JWT Auth error: InvalidSignature"
	}
	if exp, _ := c["exp"].(float64); int64(exp) < time.Now().Unix() {
		return nil, 401, "JWT token is expired"
	}
	return c, 0, ""
}

func cpJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *cpStack) dashboard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// src/middleware.ts: CSRF on state-changing API calls, except webhooks.
		if r.Method != http.MethodGet && !strings.HasPrefix(p, "/api/webhooks/") {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				cpJSON(w, 415, map[string]string{"error": "Unsupported Media Type"})
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "https://app.example" {
				cpJSON(w, 403, map[string]string{"error": "Forbidden"})
				return
			}
		}
		session := ""
		if c, err := r.Cookie("_orama_session"); err == nil && c.Value == cpReaderSession {
			session = c.Value
		}
		switch {
		case p == "/api/.well-known/jwks.json":
			pub := s.key.PublicKey
			cpJSON(w, 200, map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
			}}})
		case p == "/api/user/jwt":
			var body struct{ CollectionID, PrivateAPIKey, Scope string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Scope != "write" {
				cpJSON(w, 400, map[string]string{"error": "Invalid scope"})
				return
			}
			if body.PrivateAPIKey != cpFixtureKey || body.CollectionID != cpFixtureColl {
				cpJSON(w, 500, map[string]string{"error": "Internal server error"})
				return
			}
			// The first token (cp_prepare's, for the expiry check) lives three seconds; the rest ten minutes.
			now := time.Now().Unix()
			s.mu.Lock()
			s.mints++
			life := int64(600)
			if s.mints == 1 {
				life = 3
			}
			s.mu.Unlock()
			cpJSON(w, 200, map[string]any{
				"jwt": s.sign(map[string]any{"sub": cpFixtureColl, "scope": "write", "lmt": map[string]int{"max_doc_count": cpMaxDocs},
					"iat": now, "exp": now + life, "iss": "https://app.example", "aud": cpWriterAudience}),
				"writerURL": cpWriterAudience, "readerURL": "https://reader.example", "readerApiKey": cpReaderAPIKey,
			})
		case p == "/api/webhooks/stripe":
			cpJSON(w, 400, map[string]string{"error": "Invalid signature"})
		case strings.HasSuffix(p, "/update") && s.defects.dashboardOpenUpdate:
			cpJSON(w, 200, map[string]bool{"success": true})
		case strings.HasPrefix(p, "/api/private/"), strings.HasPrefix(p, "/api/airbyte/"):
			cpJSON(w, 401, map[string]string{"error": "Unauthorized"})
		case session == "":
			cpJSON(w, 401, map[string]string{"error": "Unauthorized"})
		case p == "/api/user/teams":
			cpJSON(w, 200, []string{})
		case p == "/api/user/private-api-key/get-for-project" && s.defects.readerGetsKey:
			cpJSON(w, 200, map[string]string{"privateApiKey": cpFixtureKey})
		default: // the read-only member: read access to the fixture project only
			cpJSON(w, 403, map[string]string{"error": "You do not have permission"})
		}
	})
}

var cpInsertPath = regexp.MustCompile(`^/v1/collections/([^/]+)/indexes/([^/]+)/(insert|delete)$`)

func (s *cpStack) writer() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := cpInsertPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			cpJSON(w, 404, map[string]string{"message": "not found"})
			return
		}
		coll := m[1]
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims, code, msg := s.verify(token)
		if code != 0 {
			cpJSON(w, code, map[string]string{"message": msg})
			return
		}
		if claims["sub"] != coll && !s.defects.writerIgnoresSub {
			cpJSON(w, 400, map[string]string{"message": "Collection not found or invalid write api key"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.docs[coll] == nil {
			s.docs[coll] = map[string]bool{}
		}
		if m[3] == "delete" {
			var ids []string
			_ = json.NewDecoder(r.Body).Decode(&ids)
			for _, id := range ids {
				delete(s.docs[coll], id)
			}
			cpJSON(w, 200, map[string]any{})
			return
		}
		var docs []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&docs)
		limit := int(claims["lmt"].(map[string]any)["max_doc_count"].(float64))
		if len(s.docs[coll])+len(docs) > limit && !s.defects.writerIgnoresCap {
			cpJSON(w, 402, map[string]string{"message": "Document limit exceeded"})
			return
		}
		for _, d := range docs {
			s.docs[coll][fmt.Sprint(d["id"])] = true
		}
		cpJSON(w, 200, map[string]int{"inserted": len(docs), "failed": 0})
	})
}

func (s *cpStack) reader() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/collections/"+cpFixtureColl+"/stats" && r.URL.Query().Get("api-key") == cpReaderAPIKey:
			cpJSON(w, 200, map[string]int{"document_count": len(s.docs[cpFixtureColl])})
		case r.URL.Path == "/v1/collections/"+cpSuiteColl+"/search":
			cpJSON(w, 200, map[string]int{"count": len(s.docs[cpSuiteColl])})
		default:
			cpJSON(w, 401, map[string]string{"message": "missing api key"})
		}
	})
}

// worker is an edge worker: "scaffold" answers the in-cluster scaffold's 501, "wired" refuses with
// 401, and with workersServe it answers 200 as the openai-decrypter does on every failure.
func (s *cpStack) worker(mode string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			cpJSON(w, 200, map[string]string{"status": "ok"})
		case mode == "scaffold":
			cpJSON(w, 501, map[string]string{"error": "CF worker not yet wired in-cluster"})
		case s.defects.workersServe:
			cpJSON(w, 200, map[string]any{})
		default:
			cpJSON(w, 401, map[string]string{"error": "Unauthorized"})
		}
	})
}

type cpRun struct {
	pass, fail, skip int
	out              string
}

type cpOptions struct {
	defects     cpDefects
	workerMode  string // "", "scaffold" or "wired"
	noDashboard bool
	noFixture   bool
	noSession   bool
}

func runControlPlane(t *testing.T, o cpOptions) cpRun {
	t.Helper()
	for _, bin := range []string{"sh", "curl", "jq", "base64"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: the control-plane script harness needs sh, curl, jq and base64", bin)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &cpStack{t: t, key: key, defects: o.defects, docs: map[string]map[string]bool{cpFixtureColl: {"seed": true}}}
	start := func(h http.Handler) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL
	}
	writer, reader := start(s.writer()), start(s.reader())
	dash := ""
	if !o.noDashboard {
		dash = start(s.dashboard())
	}
	workers := []e2eWorker{}
	if o.workerMode != "" {
		for _, name := range append([]string{"orama-workers"}, searchWorkerNames...) {
			workers = append(workers, e2eWorker{Name: name, URL: start(s.worker(o.workerMode))})
		}
	}
	cfg := e2eConfig{CollectionID: cpSuiteColl, IndexID: cpSuiteIndex, APIKey: cpSuiteKey, DashboardURL: dash, WriterJWT: true, Workers: workers}
	cfg.Cluster.WriterURL, cfg.Cluster.ReadURL = writer, reader
	dir := t.TempDir()
	cfgBytes, _ := json.Marshal(cfg)
	files := map[string]string{"config.json": string(cfgBytes), "lib.sh": e2eLibScript, "control-plane.sh": e2eControlPlaneScript}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	driver := `set -u
CONFIG="$E2E_DIR/config.json"
WRITER=$(jq -r '.cluster.writerURL' "$CONFIG"); READER=$(jq -r '.cluster.readURL' "$CONFIG")
COLL=$(jq -r '.collectionID' "$CONFIG"); INDEX=$(jq -r '.indexID' "$CONFIG"); RWKEY=$(jq -r '.apiKey' "$CONFIG")
MASTER=master
PASS=0; FAIL=0; SKIP=0; RESP=""; CODE=""
. "$E2E_DIR/lib.sh"
. "$E2E_DIR/control-plane.sh"
cp_prepare; cp_dashboard; cp_write_tokens; cp_workers; cp_expiry
printf 'pass=%s fail=%s skip=%s\n' "$PASS" "$FAIL" "$SKIP"
`
	cmd := exec.Command("sh", "-c", driver)
	cmd.Env = append(os.Environ(), "E2E_DIR="+dir, "E2E_JWT_LEEWAY=0", "E2E_PROPAGATION_WAIT=2")
	if !o.noFixture {
		cmd.Env = append(cmd.Env,
			"DASHBOARD_FIXTURE_COLLECTION_ID="+cpFixtureColl,
			"DASHBOARD_FIXTURE_INDEX_ID="+cpFixtureIndex,
			"DASHBOARD_FIXTURE_API_KEY="+cpFixtureKey)
		if !o.noSession {
			cmd.Env = append(cmd.Env, "DASHBOARD_FIXTURE_READER_SESSION="+cpReaderSession)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	var run cpRun
	run.out = string(out)
	m := regexp.MustCompile(`pass=(\d+) fail=(\d+) skip=(\d+)`).FindStringSubmatch(run.out)
	if m == nil {
		t.Fatalf("no summary:\n%s", out)
	}
	_, _ = fmt.Sscan(m[1], &run.pass)
	_, _ = fmt.Sscan(m[2], &run.fail)
	_, _ = fmt.Sscan(m[3], &run.skip)
	if strings.Contains(run.out, cpFixtureKey) {
		t.Errorf("the suite printed the fixture's private API key:\n%s", out)
	}
	return run
}

func (r cpRun) has(t *testing.T, verdict, desc string) {
	t.Helper()
	for _, line := range strings.Split(r.out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), verdict) && strings.Contains(line, desc) {
			return
		}
	}
	t.Errorf("no %s line containing %q in:\n%s", verdict, desc, r.out)
}

func TestControlPlaneSuitePassesOnACorrectStack(t *testing.T) {
	run := runControlPlane(t, cpOptions{workerMode: "wired"})
	if run.fail != 0 {
		t.Fatalf("fail=%d on a correct stack:\n%s", run.fail, run.out)
	}
	for _, desc := range []string{
		"JWKS leaks no private key member",
		"write token: wrong private API key refused",
		"write token: a project's key refused for another tenant's collection",
		"no session: private API key not handed out",
		"no session: PUT indexes/<id>/update (dashboard#106)",
		"forged session cookie refused",
		"CSRF: foreign Origin refused",
		"CSRF: non-JSON body refused",
		"Stripe webhook: unsigned event refused",
		"write token: sub is the fixture project",
		"write token: lives at most ten minutes",
		"write token: accepted on its own collection",
		"write token: refused on another tenant's collection",
		"write token: nothing landed in another tenant's collection",
		"write token: tampered payload refused",
		"write token: unsigned (alg none) refused",
		"plan cap:",
		"wrong role: read-only member gets no private API key",
		"wrong role: read-only member refused on a write route",
		"wrong tenant: member refused on another tenant's project",
		"orama-workers: segmenter refuses no X-API-KEY",
		"openai-decrypter: refuses a wrong x-api-key",
		"telemetry-worker: refuses /collect without api-key",
		"large-index-worker: refuses search with a wrong api-key",
		"index-shared-worker: refuses a request without api-key",
		"expired write token refused",
		"expired write token: the writer says it expired",
	} {
		run.has(t, "PASS", desc)
	}
	run.has(t, "SKIP", "secure-proxy: auth refusals")
}

func TestControlPlaneSuiteSkipsTheInClusterWorkerScaffold(t *testing.T) {
	run := runControlPlane(t, cpOptions{workerMode: "scaffold"})
	if run.fail != 0 {
		t.Fatalf("fail=%d:\n%s", run.fail, run.out)
	}
	run.has(t, "PASS", "openai-decrypter: /health")
	run.has(t, "SKIP", "openai-decrypter: refuses no x-api-key")
	if !strings.Contains(run.out, "route not wired yet (operator#6)") {
		t.Errorf("the scaffold skip does not name its tracker:\n%s", run.out)
	}
}

func TestControlPlaneSuiteFailsOnEachDefect(t *testing.T) {
	cases := []struct {
		name    string
		defects cpDefects
		failed  []string
	}{
		{"a worker serves unauthenticated calls", cpDefects{workersServe: true},
			[]string{"openai-decrypter: refuses no x-api-key", "orama-workers: segmenter refuses no X-API-KEY"}},
		{"an unauthenticated dashboard route", cpDefects{dashboardOpenUpdate: true},
			[]string{"no session: PUT indexes/<id>/update"}},
		{"a write token accepted on another tenant's collection", cpDefects{writerIgnoresSub: true},
			[]string{"write token: refused on another tenant's collection"}},
		{"the plan cap not enforced", cpDefects{writerIgnoresCap: true},
			[]string{"plan cap:"}},
		{"a read-only member handed the private API key", cpDefects{readerGetsKey: true},
			[]string{"wrong role: read-only member gets no private API key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := runControlPlane(t, cpOptions{defects: tc.defects, workerMode: "wired"})
			if run.fail == 0 {
				t.Fatalf("the suite passed with the defect:\n%s", run.out)
			}
			for _, desc := range tc.failed {
				run.has(t, "FAIL", desc)
			}
		})
	}
}

func TestControlPlaneSuiteSaysWhyItSkips(t *testing.T) {
	t.Run("no fixture", func(t *testing.T) {
		run := runControlPlane(t, cpOptions{noFixture: true})
		if run.fail != 0 {
			t.Fatalf("fail=%d:\n%s", run.fail, run.out)
		}
		run.has(t, "SKIP", "write-token checks")
		run.has(t, "SKIP", "expired write token refused")
		run.has(t, "PASS", "no session: GET /api/user/teams")
		if !strings.Contains(run.out, "spec.dashboardFixtureSecretRef") {
			t.Errorf("the skip does not say how to provide the fixture:\n%s", run.out)
		}
	})
	t.Run("no reader session", func(t *testing.T) {
		run := runControlPlane(t, cpOptions{noSession: true})
		if run.fail != 0 {
			t.Fatalf("fail=%d:\n%s", run.fail, run.out)
		}
		run.has(t, "SKIP", "wrong role: read-only member refused")
	})
	t.Run("no dashboard and no workers", func(t *testing.T) {
		run := runControlPlane(t, cpOptions{noDashboard: true})
		if run.fail != 0 {
			t.Fatalf("fail=%d:\n%s", run.fail, run.out)
		}
		run.has(t, "SKIP", "dashboard control plane")
		run.has(t, "SKIP", "edge workers")
	})
}
