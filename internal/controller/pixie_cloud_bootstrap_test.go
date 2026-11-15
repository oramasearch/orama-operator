/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The token is what Pixie's auth service issues for a user (claims.go GenerateJWTForUser) and what
// its API checks (authcontext UseJWTAuth: HS256 with the signing key, audience the domain).
func TestPixieUserTokenIsWhatPixieAccepts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	as := pixieIdentity{OrgID: "org-1", UserID: "user-1", Email: "ops@example"}
	token, err := pixieUserToken("signing-key", "pixie.example", as, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	mac := hmac.New(sha256.New, []byte("signing-key"))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != parts[2] {
		t.Fatal("the token is not HS256 with the hub's signing key")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"aud": "pixie.example", "iss": "PL", "sub": "user-1", "Scopes": "user", "UserID": "user-1", "OrgID": "org-1", "Email": "ops@example", "IsAPIUser": false} {
		if claims[key] != want {
			t.Errorf("claim %s = %v, want %v", key, claims[key], want)
		}
	}
	if exp := int64(claims["exp"].(float64)); exp != now.Add(5*time.Minute).Unix() {
		t.Errorf("the token lives until %d, want five minutes", exp)
	}
}

// fakePixieAPI is Pixie Cloud's GraphQL endpoint: it checks the bearer token and keeps keys.
func fakePixieAPI(t *testing.T, keys map[string]string, fail string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Use-Bearer") != "true" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		reply := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
		switch {
		case fail != "" && strings.Contains(req.Query, fail):
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "permission denied"}}})
		case strings.Contains(req.Query, "CreateDeploymentKey"):
			id := "id-" + string(rune('a'+len(keys)))
			keys[id] = "px-dep-" + id
			reply(map[string]any{"CreateDeploymentKey": map[string]any{"id": id, "key": keys[id]}})
		case strings.Contains(req.Query, "DeleteDeploymentKey"):
			id, _ := req.Variables["id"].(string)
			_, ok := keys[id]
			delete(keys, id)
			reply(map[string]any{"DeleteDeploymentKey": ok})
		case strings.Contains(req.Query, "deploymentKeys"):
			var list []any
			for id := range keys {
				list = append(list, map[string]any{"id": id})
			}
			reply(map[string]any{"deploymentKeys": list})
		case strings.Contains(req.Query, "clusters"):
			reply(map[string]any{"clusters": []any{
				map[string]any{"clusterName": "prod2", "status": "CS_HEALTHY", "lastHeartbeatMs": 1200.0},
				map[string]any{"clusterName": "prod3", "status": "CS_DISCONNECTED", "lastHeartbeatMs": 900000.0},
			}})
		}
	}))
}

func TestPixieAPIDeployKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	keys := map[string]string{}
	server := fakePixieAPI(t, keys, "")
	defer server.Close()
	hub := &pixieHubClient{creds: pixieHubCredentials{SigningKey: "k", Domain: "pixie.example"}, http: server.Client(), apiURL: server.URL, now: time.Now}
	as := pixieIdentity{OrgID: "org-1", UserID: "user-1", Email: "ops@example"}

	key, err := hub.CreateDeployKey(ctx, as)
	if err != nil || key.ID == "" || keys[key.ID] != key.Key {
		t.Fatalf("create: %+v %v", key, err)
	}
	ids, err := hub.DeployKeyIDs(ctx, as)
	if err != nil || !ids[key.ID] {
		t.Fatalf("list: %v %v", ids, err)
	}
	if err := hub.DeleteDeployKey(ctx, as, key.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := hub.DeleteDeployKey(ctx, as, key.ID); err == nil {
		t.Error("deleting a key Pixie no longer has reported success")
	}
	states, err := hub.Clusters(ctx, as)
	if err != nil || !states["prod2"].Connected || states["prod3"].Connected || !states["prod3"].Registered {
		t.Fatalf("clusters: %+v %v", states, err)
	}
}

func TestPixieAPIErrorsAreReturned(t *testing.T) {
	server := fakePixieAPI(t, map[string]string{}, "CreateDeploymentKey")
	defer server.Close()
	hub := &pixieHubClient{creds: pixieHubCredentials{SigningKey: "k", Domain: "pixie.example"}, http: server.Client(), apiURL: server.URL, now: time.Now}
	if _, err := hub.CreateDeployKey(context.Background(), pixieIdentity{OrgID: "o", UserID: "u"}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a GraphQL error was not returned: %v", err)
	}
	if _, err := (&pixieHubClient{http: &http.Client{}, apiURL: server.URL, now: time.Now}).DeployKeyIDs(context.Background(), pixieIdentity{}); err == nil {
		t.Error("a server whose certificate is not trusted was used")
	}
}

// The organisation and administrator adapter writes columns the pinned profile schema has, and
// leaves out only columns that are nullable or defaulted (assets/pixie/profile-schema, the
// migrations of the vendored commit).
func TestPixieBootstrapMatchesVendoredSchema(t *testing.T) {
	files, err := filepath.Glob("assets/pixie/profile-schema/*.up.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no vendored profile migrations: %v", err)
	}
	sort.Strings(files)
	if !strings.HasPrefix(filepath.Base(files[len(files)-1]), "0000") {
		t.Fatalf("unexpected migration names: %v", files)
	}
	var latest int
	for _, file := range files {
		if n, err := strconv.Atoi(strings.SplitN(filepath.Base(file), "_", 2)[0]); err == nil && n > latest {
			latest = n
		}
	}
	if latest < pixieProfileMigrationNeeded {
		t.Fatalf("the vendored profile schema stops at %d, before the adapter's %d", latest, pixieProfileMigrationNeeded)
	}
	columns := map[string]map[string]string{"orgs": {}, "users": {}, "user_attributes": {}, "user_settings": {}}
	create := regexp.MustCompile(`(?is)CREATE TABLE (\w+) \((.*?)\n\);`)
	add := regexp.MustCompile(`(?i)ALTER TABLE (\w+)\s+ADD COLUMN\s+(\w+)([^;]*);`)
	drop := regexp.MustCompile(`(?i)ALTER TABLE (\w+)\s+DROP COLUMN\s+(\w+)`)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		sql := string(data)
		for _, m := range create.FindAllStringSubmatch(sql, -1) {
			if table, ok := columns[m[1]]; ok {
				for _, line := range strings.Split(m[2], "\n") {
					fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ","))
					if len(fields) >= 2 && !strings.HasPrefix(fields[0], "--") && !strings.EqualFold(fields[0], "PRIMARY") && !strings.EqualFold(fields[0], "UNIQUE") {
						table[fields[0]] = strings.ToUpper(strings.Join(fields[1:], " "))
					}
				}
			}
		}
		for _, m := range add.FindAllStringSubmatch(sql, -1) {
			if table, ok := columns[m[1]]; ok {
				table[m[2]] = strings.ToUpper(m[3])
			}
		}
		for _, m := range drop.FindAllStringSubmatch(sql, -1) {
			if table, ok := columns[m[1]]; ok {
				delete(table, m[2])
			}
		}
	}
	written := map[string][]string{
		"orgs":  {"id", "org_name", "domain_name"},
		"users": {"org_id", "first_name", "last_name", "email", "is_approved", "identity_provider", "auth_provider_id"},
	}
	for _, table := range pixieUserRowTables {
		written[table] = []string{"user_id"}
		// Keyed by user_id, so ON CONFLICT (user_id) has its unique index.
		if !strings.Contains(columns[table]["user_id"], "UUID") {
			t.Errorf("%s.user_id is not in the pinned schema", table)
		}
	}
	for table, names := range written {
		set := map[string]bool{}
		for _, name := range names {
			set[name] = true
			if _, ok := columns[table][name]; !ok {
				t.Errorf("the adapter writes %s.%s, which the pinned schema does not have", table, name)
			}
		}
		for name, definition := range columns[table] {
			if !set[name] && strings.Contains(definition, "NOT NULL") && !strings.Contains(definition, "DEFAULT") {
				t.Errorf("%s.%s is NOT NULL without a default and the adapter does not write it", table, name)
			}
		}
	}
	// The index body the operator creates is the indexer's own.
	var body map[string]any
	if err := json.Unmarshal(pixieMetadataIndexBody, &body); err != nil || body["settings"] == nil || body["mappings"] == nil {
		t.Fatalf("assets/pixie/md-index.json is not the indexer's index body: %v", err)
	}
}
