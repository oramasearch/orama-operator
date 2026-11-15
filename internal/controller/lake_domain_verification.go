/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// A tenant's custom domain is routed only once its claim is verified (lake#949).
//
// lake keeps one claim per host, across every account of the Lake, in its control Postgres
// (`domain_claims`, migration 0106_domain_claims). A claim is `pending` until the tenant publishes
// the TXT record `_lake-challenge.<host>` with the claim's token and lake's verify_domain reads it;
// then it is `verified`. lake writes a domain row to orama_control.domains only for a verified
// claim, but rows written before claims existed, or by a lake image that predates them, are still
// in the table. So the operator checks every domain row against the verified claims itself and
// refuses, with the reason on the domain's status row, a row whose host has no verified claim of
// that repository and environment. A Lake whose schema has no domain_claims table yet has no
// verified claims: every domain is refused until lake's migration applies and its tenants verify.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// LakeDomainClaim is one verified row of lake's domain_claims: the host and the environment that
// holds it, by lake's repository id and, when liquid gave the repository one, its liquid id (a
// domain row names the repository by the deployment's own value, which may be either).
type LakeDomainClaim struct {
	Host         string
	Environment  string
	RepositoryID string
	LiquidID     string
}

// LakeDomainClaimReader reads a Lake's verified claims. It returns errDomainClaimsTableMissing
// while the Lake's schema is older than 0106_domain_claims.
type LakeDomainClaimReader func(ctx context.Context, lake *oramav1alpha1.Lake) ([]LakeDomainClaim, error)

// errDomainClaimsTableMissing: lake's schema has not reached 0106_domain_claims yet, so no claim is
// verified and no domain is routed.
var errDomainClaimsTableMissing = errors.New("the domain_claims table does not exist yet (lake schema before 0106_domain_claims)")

// listVerifiedDomainClaimsSQL is every verified claim, with its repository's liquid id.
const listVerifiedDomainClaimsSQL = `SELECT c.host, c.environment, c.repository_id::text, COALESCE(r.liquid_id, '')
  FROM domain_claims c
  LEFT JOIN repositories r ON r.id = c.repository_id
 WHERE c.state = 'verified'
 ORDER BY c.host`

// lakeVerifiedDomains is the set of (repository, environment, host) keys a verified claim allows,
// repository lowercased, in domainRowKey's layout.
type lakeVerifiedDomains map[string]bool

func verifiedDomainKey(repository, environment, host string) string {
	return strings.Join([]string{strings.ToLower(repository), environment, strings.ToLower(host)}, "\n")
}

// newLakeVerifiedDomains is the keys of claims: each claim allows its host under lake's repository
// id and under its liquid id.
func newLakeVerifiedDomains(claims []LakeDomainClaim) lakeVerifiedDomains {
	verified := make(lakeVerifiedDomains, 2*len(claims))
	for _, claim := range claims {
		verified[verifiedDomainKey(claim.RepositoryID, claim.Environment, claim.Host)] = true
		if claim.LiquidID != "" {
			verified[verifiedDomainKey(claim.LiquidID, claim.Environment, claim.Host)] = true
		}
	}
	return verified
}

// allows reports whether a verified claim holds row's host for row's repository and environment.
func (v lakeVerifiedDomains) allows(row map[string]string) bool {
	return v[verifiedDomainKey(row["repository"], row["environment"], row["host"])]
}

// unverifiedDomainReason is the status message of a domain row without a verified claim.
func unverifiedDomainReason(host string) string {
	return fmt.Sprintf("%q has no verified claim on this Lake: publish the TXT record _lake-challenge.%s that create_domain answered, then call verify_domain", host, host)
}

func (r *OramaZettaClusterReconciler) domainClaims() LakeDomainClaimReader {
	if r.DomainClaims != nil {
		return r.DomainClaims
	}
	return func(ctx context.Context, lake *oramav1alpha1.Lake) ([]LakeDomainClaim, error) {
		return readLakeDomainClaims(ctx, r.Client, lake)
	}
}

// readLakeDomainClaims reads the verified claims from the Lake's control Postgres, with the uri of
// its control DB app Secret.
func readLakeDomainClaims(ctx context.Context, c client.Reader, lake *oramav1alpha1.Lake) ([]LakeDomainClaim, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeControlDB(lake.Name) + "-app"}
	if err := c.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("the control DB app Secret %s is not created yet", key.Name)
		}
		return nil, fmt.Errorf("read the control DB app Secret %s: %w", key.Name, err)
	}
	uri := string(secret.Data["uri"])
	if uri == "" {
		return nil, fmt.Errorf("the control DB app Secret %s has no uri yet", key.Name)
	}
	passCtx, cancel := context.WithTimeout(ctx, servicePassTimeout)
	defer cancel()
	config, err := pgx.ParseConfig(uri)
	if err != nil {
		return nil, fmt.Errorf("parse the control DB uri: %w", err)
	}
	config.ConnectTimeout = serviceStoreConnectTimeout
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["application_name"] = "orama-operator"
	conn, err := pgx.ConnectConfig(passCtx, config)
	if err != nil {
		return nil, fmt.Errorf("connect to the control DB: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(passCtx, listVerifiedDomainClaimsSQL)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
			return nil, errDomainClaimsTableMissing
		}
		return nil, err
	}
	defer rows.Close()
	var claims []LakeDomainClaim
	for rows.Next() {
		var claim LakeDomainClaim
		if err := rows.Scan(&claim.Host, &claim.Environment, &claim.RepositoryID, &claim.LiquidID); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}
