/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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
// self-hosted Pixie Cloud, done by the operator on the hub's cluster, idempotently, on every pass.
//
//  1. The metadata index exists in OpenSearch (the indexer runs with manual index management).
//  2. The administrator exists in Kratos, with the generated password in Secret pixie-admin.
//  3. The organisation exists, and the administrator belongs to it.
//  4. Each agent cluster has its own deploy key, owned by that organisation.
//
// Steps 3 and 4 write the rows Pixie's own services write (profile's orgs and users, vzmgr's
// vizier_deployment_keys), with vzmgr's own SQL for the key, so Pixie reads them exactly as its
// own. They run only once Pixie's migrations have created the tables, and the vendored migrations
// they depend on are pinned by TestPixieBootstrapMatchesVendoredSchema.

// pixieOrgNamespace makes the organisation's id a function of its name (UUIDv5), so a rebuilt hub
// gets the same id.
var pixieOrgNamespace = uuid.MustParse("6a8c2c0e-4d83-4f2e-9b37-0f2ac3c0b8f1")

// pixieAgentState is what the hub knows of one agent cluster.
type pixieAgentState struct {
	Registered bool
	Connected  bool
}

// pixieHubBackend is everything the bootstrap does against the hub's services. The real one is
// pixieHubClient; tests inject a fake.
type pixieHubBackend interface {
	// Ready reports whether the services the bootstrap writes to accept requests yet, and what is
	// still missing.
	Ready(ctx context.Context) (bool, string, error)
	EnsureIndex(ctx context.Context, name string) error
	// EnsureAdmin returns the administrator's Kratos identity id.
	EnsureAdmin(ctx context.Context, email, password string) (string, error)
	// EnsureOrg returns the organisation's id and the administrator's user id.
	EnsureOrg(ctx context.Context, name, adminEmail, identityID string) (orgID, userID string, err error)
	EnsureDeployKey(ctx context.Context, orgID, userID, key, description string) error
	DeleteDeployKey(ctx context.Context, key string) error
	AgentState(ctx context.Context, orgID, clusterName string) (pixieAgentState, error)
	Close()
}

// pixieHubCredentials are the Secrets' values the real backend signs in with.
type pixieHubCredentials struct {
	ProfilePassword, VzmgrPassword, VzmgrDatabaseKey string
	SearchUser, SearchPassword                       string
	// ServiceCA verifies OpenSearch and Kratos, whose certificates the operator issued.
	ServiceCA []byte
}

// pixieHubClient reaches the hub's Postgres, Kratos and OpenSearch from the operator's pod, on the
// hub's own cluster.
type pixieHubClient struct {
	creds   pixieHubCredentials
	http    *http.Client
	profile *pgx.Conn
	vzmgr   *pgx.Conn
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
	}, nil
}

func (h *pixieHubClient) Close() {
	for _, conn := range []*pgx.Conn{h.profile, h.vzmgr} {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}
}

func (h *pixieHubClient) connect(ctx context.Context, database, password string) (*pgx.Conn, error) {
	uri := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(database, password),
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

func (h *pixieHubClient) databases(ctx context.Context) error {
	var err error
	if h.profile == nil {
		if h.profile, err = h.connect(ctx, "profile", h.creds.ProfilePassword); err != nil {
			return fmt.Errorf("connect to the profile database: %w", err)
		}
	}
	if h.vzmgr == nil {
		if h.vzmgr, err = h.connect(ctx, "vzmgr", h.creds.VzmgrPassword); err != nil {
			return fmt.Errorf("connect to the vzmgr database: %w", err)
		}
	}
	return nil
}

// pixieMigrationsNeeded are the migration versions the bootstrap's SQL needs: profile's users
// without usernames (22) and vzmgr's encrypted deployment keys (25).
var pixieMigrationsNeeded = map[string]int{"profile_service_migrations": 22, "vzmgr_service_migrations": 25}

func (h *pixieHubClient) Ready(ctx context.Context) (bool, string, error) {
	if err := h.databases(ctx); err != nil {
		return false, err.Error(), nil
	}
	for table, version := range pixieMigrationsNeeded {
		conn := h.profile
		if strings.HasPrefix(table, "vzmgr") {
			conn = h.vzmgr
		}
		var current int
		var dirty bool
		err := conn.QueryRow(ctx, "SELECT version, dirty FROM "+pgx.Identifier{table}.Sanitize()+" LIMIT 1").Scan(&current, &dirty)
		if err != nil || dirty || current < version {
			return false, fmt.Sprintf("Pixie's %s are not at version %d yet", table, version), nil
		}
	}
	return true, "", nil
}

func (h *pixieHubClient) request(ctx context.Context, method, rawURL string, body any, basicAuth bool) (*http.Response, error) {
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
	if basicAuth {
		req.SetBasicAuth(h.creds.SearchUser, h.creds.SearchPassword)
	}
	return h.http.Do(req)
}

func (h *pixieHubClient) EnsureIndex(ctx context.Context, name string) error {
	base := fmt.Sprintf("https://%s.%s.svc:%d/%s", pixieSearchService, oramav1alpha1.PixieCloudNamespace, pixieSearchPort, url.PathEscape(name))
	resp, err := h.request(ctx, http.MethodHead, base, nil, true)
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
	// A single node holds no replicas. The indexer writes the mapping itself.
	resp, err = h.request(ctx, http.MethodPut, base, map[string]any{"settings": map[string]any{"number_of_replicas": 0}}, true)
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
		resp, err := h.request(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=250&page=%d", admin, page), nil, false)
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
	resp, err := h.request(ctx, http.MethodPost, admin, body, false)
	if err != nil {
		return "", fmt.Errorf("create the Kratos administrator: %w", err)
	}
	defer resp.Body.Close()
	var created kratosIdentity
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("create the Kratos administrator: Kratos answered %s: %s", resp.Status, data)
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.ID == "" {
		return "", fmt.Errorf("create the Kratos administrator: no identity id in the answer")
	}
	return created.ID, nil
}

func (h *pixieHubClient) EnsureOrg(ctx context.Context, name, adminEmail, identityID string) (string, string, error) {
	if err := h.databases(ctx); err != nil {
		return "", "", err
	}
	id := uuid.NewSHA1(pixieOrgNamespace, []byte(name)).String()
	if _, err := h.profile.Exec(ctx, `INSERT INTO orgs (id, org_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, name); err != nil {
		return "", "", fmt.Errorf("create organisation %s: %w", name, err)
	}
	var orgID string
	if err := h.profile.QueryRow(ctx, `SELECT id FROM orgs WHERE org_name = $1`, name).Scan(&orgID); err != nil {
		return "", "", fmt.Errorf("read organisation %s: %w", name, err)
	}
	if _, err := h.profile.Exec(ctx,
		`INSERT INTO users (org_id, first_name, last_name, email, is_approved, identity_provider, auth_provider_id)
		 SELECT $1, 'Hub', 'Administrator', $2, true, 'kratos', $3
		 WHERE NOT EXISTS (SELECT 1 FROM users WHERE email = $2)`,
		orgID, adminEmail, identityID); err != nil {
		return "", "", fmt.Errorf("create the administrator's user: %w", err)
	}
	var userID, userOrg string
	if err := h.profile.QueryRow(ctx, `SELECT id, COALESCE(org_id::text, '') FROM users WHERE email = $1`, adminEmail).Scan(&userID, &userOrg); err != nil {
		return "", "", fmt.Errorf("read the administrator's user: %w", err)
	}
	if userOrg != orgID {
		return "", "", fmt.Errorf("user %s belongs to organisation %q, not %s (%s)", adminEmail, userOrg, name, orgID)
	}
	return orgID, userID, nil
}

// pixieDeployKeyLookup and pixieDeployKeyInsert are vzmgr's own statements
// (src/cloud/vzmgr/deploymentkey/deployment_keys.go), so the parameters are typed as vzmgr's are.
const (
	pixieDeployKeyLookup = `SELECT id FROM vizier_deployment_keys
                WHERE hashed_key=sha256($1) AND PGP_SYM_DECRYPT(encrypted_key::bytea, $2::text)::bytea=$1`
	pixieDeployKeyInsert = `INSERT INTO vizier_deployment_keys(org_id, user_id, hashed_key, encrypted_key, description)
                VALUES($1, $2, sha256($3), PGP_SYM_ENCRYPT($3::text, $4::text), $5)
              RETURNING id`
)

func (h *pixieHubClient) EnsureDeployKey(ctx context.Context, orgID, userID, key, description string) error {
	if err := h.databases(ctx); err != nil {
		return err
	}
	var id string
	err := h.vzmgr.QueryRow(ctx, pixieDeployKeyLookup, []byte(key), h.creds.VzmgrDatabaseKey).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("look up a deploy key: %w", err)
	}
	if err := h.vzmgr.QueryRow(ctx, pixieDeployKeyInsert, orgID, userID, []byte(key), h.creds.VzmgrDatabaseKey, description).Scan(&id); err != nil {
		return fmt.Errorf("create a deploy key: %w", err)
	}
	return nil
}

func (h *pixieHubClient) DeleteDeployKey(ctx context.Context, key string) error {
	if err := h.databases(ctx); err != nil {
		return err
	}
	_, err := h.vzmgr.Exec(ctx, `DELETE FROM vizier_deployment_keys WHERE id IN (`+pixieDeployKeyLookup+`)`, []byte(key), h.creds.VzmgrDatabaseKey)
	return err
}

// pixieConnectedStatuses are vzmgr's states of a Vizier that is reachable.
var pixieConnectedStatuses = map[string]bool{"HEALTHY": true, "CONNECTED": true, "DEGRADED": true, "UPDATING": true}

func (h *pixieHubClient) AgentState(ctx context.Context, orgID, clusterName string) (pixieAgentState, error) {
	if err := h.databases(ctx); err != nil {
		return pixieAgentState{}, err
	}
	var status string
	var age *int64
	err := h.vzmgr.QueryRow(ctx,
		`SELECT i.status::text, (EXTRACT(EPOCH FROM age(now(), i.last_heartbeat)))::bigint
		   FROM vizier_cluster c JOIN vizier_cluster_info i ON i.vizier_cluster_id = c.id
		  WHERE c.org_id = $1 AND c.cluster_name = $2`, orgID, clusterName).Scan(&status, &age)
	if errors.Is(err, pgx.ErrNoRows) {
		return pixieAgentState{}, nil
	}
	if err != nil {
		return pixieAgentState{}, fmt.Errorf("read cluster %s: %w", clusterName, err)
	}
	// A Vizier heartbeats every few seconds; five minutes of silence is disconnected.
	return pixieAgentState{Registered: true, Connected: pixieConnectedStatuses[status] && age != nil && *age < 300}, nil
}
