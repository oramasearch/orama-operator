/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The hub's bootstrap (operator#15): what a person would otherwise do by hand after installing
// self-hosted Pixie Cloud, done by the hub cluster's operator, idempotently.
//
//  1. The metadata index exists in OpenSearch, created with the indexer's own body
//     (assets/pixie/md-index.json), so the indexer's migration finds nothing to change. This step
//     runs as soon as OpenSearch is ready, before the hub is: the indexer does not start without it.
//  2. The administrator exists in Kratos (Kratos' admin API), with the generated password.
//  3. The organisation exists and the administrator belongs to it. Pixie has no API that creates
//     the first organisation or user without someone signing in, so this one step is a schema
//     adapter: it reads first and inserts only what is missing, as a dedicated role ("bootstrap")
//     that may only read and insert those two tables, against the vendored, pinned profile schema
//     (TestPixieBootstrapMatchesVendoredSchema).
//  4. Each agent cluster's deploy key is created, listed and deleted through Pixie Cloud's own API
//     (GraphQL CreateDeploymentKey, deploymentKeys, DeleteDeploymentKey), as the administrator,
//     with a token the operator signs with the hub's signing key, as Pixie's auth service does.
//     Pixie generates the key; the operator only keeps it and its id.

//go:embed assets/pixie/md-index.json
var pixieMetadataIndexBody []byte

// pixieOrgNamespace makes the organisation's id a function of its name (UUIDv5), so a rebuilt hub
// gets the same id.
var pixieOrgNamespace = uuid.MustParse("6a8c2c0e-4d83-4f2e-9b37-0f2ac3c0b8f1")

// pixieBootstrapRole is the dedicated login role of step 3.
const pixieBootstrapRole = "bootstrap"

// pixieIdentity is the administrator Pixie's API is called as.
type pixieIdentity struct {
	OrgID, UserID, Email string
}

// pixieDeployKey is one key as Pixie's API returns it.
type pixieDeployKey struct {
	ID, Key string
}

// pixieAgentState is what the hub knows of one agent cluster.
type pixieAgentState struct {
	Registered bool
	Connected  bool
}

// pixieHubBackend is everything the bootstrap does against the hub's services. The real one is
// pixieHubClient; tests inject a fake.
type pixieHubBackend interface {
	// Ready reports whether the services the bootstrap uses accept requests yet, and what is not.
	Ready(ctx context.Context) (bool, string, error)
	EnsureIndex(ctx context.Context, name string) error
	// EnsureAdmin returns the administrator's Kratos identity id.
	EnsureAdmin(ctx context.Context, email, password string) (string, error)
	// EnsureOrg returns the organisation's and the administrator's ids.
	EnsureOrg(ctx context.Context, name, adminEmail, identityID string) (pixieIdentity, error)
	DeployKeyIDs(ctx context.Context, as pixieIdentity) (map[string]bool, error)
	CreateDeployKey(ctx context.Context, as pixieIdentity) (pixieDeployKey, error)
	DeleteDeployKey(ctx context.Context, as pixieIdentity, id string) error
	// Clusters is every cluster registered in the organisation, by name.
	Clusters(ctx context.Context, as pixieIdentity) (map[string]pixieAgentState, error)
	Close()
}

// pixieHubCredentials are what the real backend signs in and signs with.
type pixieHubCredentials struct {
	// ProfilePassword is the profile service's role, used only to check its migrations and to
	// grant the bootstrap role its two tables.
	ProfilePassword   string
	BootstrapPassword string
	SearchUser        string
	SearchPassword    string
	// SigningKey is the hub's JWT signing key (cloud-auth-secrets), Domain its domain (the
	// tokens' audience).
	SigningKey, Domain string
	// ServiceCA verifies OpenSearch, Kratos and the API, whose certificates the operator issued.
	ServiceCA []byte
}

// pixieHubClient reaches the hub's Postgres, Kratos, OpenSearch and API from the operator's pod, on
// the hub's own cluster. Every TLS connection verifies the hub's service CA.
type pixieHubClient struct {
	creds     pixieHubCredentials
	http      *http.Client
	profile   *pgx.Conn
	bootstrap *pgx.Conn
	// apiURL is the GraphQL endpoint; tests point it at a test server.
	apiURL string
	now    func() time.Time
}

func newPixieHubClient(creds pixieHubCredentials) (*pixieHubClient, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(creds.ServiceCA) {
		return nil, errors.New("the hub's service CA is not PEM")
	}
	return &pixieHubClient{
		creds: creds,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
		apiURL: fmt.Sprintf("https://api-service.%s.svc:51200/api/graphql", oramav1alpha1.PixieCloudNamespace),
		now:    time.Now,
	}, nil
}

func (h *pixieHubClient) Close() {
	for _, conn := range []*pgx.Conn{h.profile, h.bootstrap} {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}
}

func pixieConnect(ctx context.Context, user, password, database string) (*pgx.Conn, error) {
	uri := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     pixieDBHost() + ":5432",
		Path:     database,
		RawQuery: "sslmode=require&connect_timeout=10",
	}
	config, err := pgx.ParseConfig(uri.String())
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, config)
}

// pixieProfileMigrationNeeded is the profile migration the adapter's SQL was written against:
// 000022, which drops usernames (later migrations are checked by the vendored-schema test).
const pixieProfileMigrationNeeded = 22

func (h *pixieHubClient) Ready(ctx context.Context) (bool, string, error) {
	var err error
	if h.profile == nil {
		if h.profile, err = pixieConnect(ctx, "profile", h.creds.ProfilePassword, "profile"); err != nil {
			return false, "cannot reach the profile database: " + err.Error(), nil
		}
	}
	var version int
	var dirty bool
	if err := h.profile.QueryRow(ctx, `SELECT version, dirty FROM profile_service_migrations LIMIT 1`).Scan(&version, &dirty); err != nil || dirty || version < pixieProfileMigrationNeeded {
		return false, fmt.Sprintf("Pixie's profile migrations are not at %d yet", pixieProfileMigrationNeeded), nil
	}
	// The bootstrap role may read and insert organisations and users, nothing else. Granted by
	// the tables' owner, only when missing.
	var granted bool
	if err := h.profile.QueryRow(ctx,
		`SELECT has_table_privilege($1, 'orgs', 'INSERT') AND has_table_privilege($1, 'users', 'INSERT')
		    AND has_table_privilege($1, 'orgs', 'SELECT') AND has_table_privilege($1, 'users', 'SELECT')`,
		pixieBootstrapRole).Scan(&granted); err != nil {
		return false, "", fmt.Errorf("read the bootstrap role's privileges: %w", err)
	}
	if !granted {
		if _, err := h.profile.Exec(ctx, `GRANT SELECT, INSERT ON orgs, users TO `+pgx.Identifier{pixieBootstrapRole}.Sanitize()); err != nil {
			return false, "", fmt.Errorf("grant the bootstrap role: %w", err)
		}
	}
	if h.bootstrap == nil {
		if h.bootstrap, err = pixieConnect(ctx, pixieBootstrapRole, h.creds.BootstrapPassword, "profile"); err != nil {
			return false, "cannot sign in as the bootstrap role: " + err.Error(), nil
		}
	}
	return true, "", nil
}

func (h *pixieHubClient) request(ctx context.Context, method, rawURL string, body any, header func(*http.Request)) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if header != nil {
		header(req)
	}
	return h.http.Do(req)
}

func (h *pixieHubClient) searchAuth(req *http.Request) {
	req.SetBasicAuth(h.creds.SearchUser, h.creds.SearchPassword)
}

func (h *pixieHubClient) EnsureIndex(ctx context.Context, name string) error {
	base := fmt.Sprintf("https://%s.%s.svc:%d/%s", pixieSearchService, oramav1alpha1.PixieCloudNamespace, pixieSearchPort, url.PathEscape(name))
	resp, err := h.request(ctx, http.MethodHead, base, nil, h.searchAuth)
	if err != nil {
		return fmt.Errorf("read index %s: %w", name, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("read index %s: OpenSearch answered %s", name, resp.Status)
	}
	resp, err = h.request(ctx, http.MethodPut, base, json.RawMessage(pixieMetadataIndexBody), h.searchAuth)
	if err != nil {
		return fmt.Errorf("create index %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("create index %s: OpenSearch answered %s: %s", name, resp.Status, data)
	}
	return nil
}

type kratosIdentity struct {
	ID     string `json:"id"`
	Traits struct {
		Email string `json:"email"`
	} `json:"traits"`
}

func (h *pixieHubClient) EnsureAdmin(ctx context.Context, email, password string) (string, error) {
	admin := fmt.Sprintf("https://kratos.%s.svc:4434/admin/identities", oramav1alpha1.PixieCloudNamespace)
	for page := 1; page <= 50; page++ {
		resp, err := h.request(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=250&page=%d", admin, page), nil, nil)
		if err != nil {
			return "", fmt.Errorf("list Kratos identities: %w", err)
		}
		var identities []kratosIdentity
		err = json.NewDecoder(resp.Body).Decode(&identities)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("list Kratos identities: %s %v", resp.Status, err)
		}
		for _, identity := range identities {
			if strings.EqualFold(identity.Traits.Email, email) {
				return identity.ID, nil
			}
		}
		if len(identities) < 250 {
			break
		}
	}
	body := map[string]any{
		"schema_id":   "default",
		"traits":      map[string]any{"email": email},
		"credentials": map[string]any{"password": map[string]any{"config": map[string]any{"password": password}}},
	}
	resp, err := h.request(ctx, http.MethodPost, admin, body, nil)
	if err != nil {
		return "", fmt.Errorf("create the Kratos administrator: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("create the Kratos administrator: Kratos answered %s: %s", resp.Status, data)
	}
	var created kratosIdentity
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.ID == "" {
		return "", fmt.Errorf("create the Kratos administrator: no identity id in the answer")
	}
	return created.ID, nil
}

// EnsureOrg reads before it writes: a steady hub issues no INSERT.
func (h *pixieHubClient) EnsureOrg(ctx context.Context, name, adminEmail, identityID string) (pixieIdentity, error) {
	if h.bootstrap == nil {
		return pixieIdentity{}, errors.New("the bootstrap role is not signed in")
	}
	identity := pixieIdentity{Email: adminEmail}
	err := h.bootstrap.QueryRow(ctx, `SELECT id::text FROM orgs WHERE org_name = $1`, name).Scan(&identity.OrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		identity.OrgID = uuid.NewSHA1(pixieOrgNamespace, []byte(name)).String()
		if _, err := h.bootstrap.Exec(ctx, `INSERT INTO orgs (id, org_name) VALUES ($1, $2)`, identity.OrgID, name); err != nil {
			return pixieIdentity{}, fmt.Errorf("create organisation %s: %w", name, err)
		}
	} else if err != nil {
		return pixieIdentity{}, fmt.Errorf("read organisation %s: %w", name, err)
	}
	var userOrg string
	err = h.bootstrap.QueryRow(ctx, `SELECT id::text, COALESCE(org_id::text, '') FROM users WHERE email = $1`, adminEmail).Scan(&identity.UserID, &userOrg)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := h.bootstrap.QueryRow(ctx,
			`INSERT INTO users (org_id, first_name, last_name, email, is_approved, identity_provider, auth_provider_id)
			 VALUES ($1, 'Hub', 'Administrator', $2, true, 'kratos', $3) RETURNING id::text`,
			identity.OrgID, adminEmail, identityID).Scan(&identity.UserID); err != nil {
			return pixieIdentity{}, fmt.Errorf("create the administrator's user: %w", err)
		}
		return identity, nil
	}
	if err != nil {
		return pixieIdentity{}, fmt.Errorf("read the administrator's user: %w", err)
	}
	if userOrg != identity.OrgID {
		return pixieIdentity{}, fmt.Errorf("user %s belongs to organisation %q, not %s (%s): nothing is changed", adminEmail, userOrg, name, identity.OrgID)
	}
	return identity, nil
}

// pixieUserToken is a user token for Pixie's API, as Pixie's auth service issues one
// (src/shared/services/utils/claims.go GenerateJWTForUser, jwt.go ProtoToToken): HS256 with the hub's
// signing key, the hub's domain as audience, issuer PL, the user scope, valid for five minutes.
func pixieUserToken(signingKey, domain string, as pixieIdentity, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"aud":       domain,
		"exp":       now.Add(5 * time.Minute).Unix(),
		"iat":       now.Unix(),
		"nbf":       now.Add(-time.Minute).Unix(),
		"iss":       "PL",
		"jti":       uuid.NewString(),
		"sub":       as.UserID,
		"Scopes":    "user",
		"UserID":    as.UserID,
		"OrgID":     as.OrgID,
		"Email":     as.Email,
		"IsAPIUser": false,
	})
	if err != nil {
		return "", err
	}
	payload := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// graphql calls Pixie's API as the administrator and decodes data into out.
func (h *pixieHubClient) graphql(ctx context.Context, as pixieIdentity, query string, variables map[string]any, out any) error {
	token, err := pixieUserToken(h.creds.SigningKey, h.creds.Domain, as, h.now())
	if err != nil {
		return err
	}
	resp, err := h.request(ctx, http.MethodPost, h.apiURL, map[string]any{"query": query, "variables": variables}, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		// Bearer only, never a browser session (session_middleware.go).
		req.Header.Set("X-Use-Bearer", "true")
	})
	if err != nil {
		return fmt.Errorf("Pixie API: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Pixie API answered %s: %s", resp.Status, data)
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return fmt.Errorf("Pixie API: %w", err)
	}
	if len(answer.Errors) > 0 {
		return fmt.Errorf("Pixie API: %s", answer.Errors[0].Message)
	}
	return json.Unmarshal(answer.Data, out)
}

func (h *pixieHubClient) DeployKeyIDs(ctx context.Context, as pixieIdentity) (map[string]bool, error) {
	var out struct {
		DeploymentKeys []struct {
			ID string `json:"id"`
		} `json:"deploymentKeys"`
	}
	if err := h.graphql(ctx, as, `query { deploymentKeys { id } }`, nil, &out); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, key := range out.DeploymentKeys {
		ids[key.ID] = true
	}
	return ids, nil
}

func (h *pixieHubClient) CreateDeployKey(ctx context.Context, as pixieIdentity) (pixieDeployKey, error) {
	var out struct {
		CreateDeploymentKey struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"CreateDeploymentKey"`
	}
	if err := h.graphql(ctx, as, `mutation { CreateDeploymentKey { id key } }`, nil, &out); err != nil {
		return pixieDeployKey{}, err
	}
	if out.CreateDeploymentKey.ID == "" || out.CreateDeploymentKey.Key == "" {
		return pixieDeployKey{}, errors.New("Pixie API returned no deploy key")
	}
	return pixieDeployKey{ID: out.CreateDeploymentKey.ID, Key: out.CreateDeploymentKey.Key}, nil
}

func (h *pixieHubClient) DeleteDeployKey(ctx context.Context, as pixieIdentity, id string) error {
	var out struct {
		DeleteDeploymentKey bool `json:"DeleteDeploymentKey"`
	}
	if err := h.graphql(ctx, as, `mutation ($id: ID!) { DeleteDeploymentKey(id: $id) }`, map[string]any{"id": id}, &out); err != nil {
		return err
	}
	if !out.DeleteDeploymentKey {
		return fmt.Errorf("Pixie API did not delete deploy key %s", id)
	}
	return nil
}

// pixieConnectedStatuses are the API's states of a cluster that heartbeats.
var pixieConnectedStatuses = map[string]bool{"CS_HEALTHY": true, "CS_UNHEALTHY": true, "CS_DEGRADED": true, "CS_UPDATING": true, "CS_CONNECTED": true}

func (h *pixieHubClient) Clusters(ctx context.Context, as pixieIdentity) (map[string]pixieAgentState, error) {
	var out struct {
		Clusters []struct {
			ClusterName     string  `json:"clusterName"`
			Status          string  `json:"status"`
			LastHeartbeatMs float64 `json:"lastHeartbeatMs"`
		} `json:"clusters"`
	}
	if err := h.graphql(ctx, as, `query { clusters { clusterName status lastHeartbeatMs } }`, nil, &out); err != nil {
		return nil, err
	}
	states := map[string]pixieAgentState{}
	for _, cluster := range out.Clusters {
		// lastHeartbeatMs is the time since the last heartbeat; five minutes is disconnected.
		states[cluster.ClusterName] = pixieAgentState{Registered: true, Connected: pixieConnectedStatuses[cluster.Status] && cluster.LastHeartbeatMs < 5*60*1000}
	}
	return states, nil
}
