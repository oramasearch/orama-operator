/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The pgx LakeServiceStore: lake's control Postgres, reached with the CloudNativePG application
// Secret lake-<lake>-controldb-app key uri. One connection per poll, closed when the poll ends.

const serviceStoreConnectTimeout = 10 * time.Second

// listServicesSQL reads every service with its status, oldest first, leaving out history
// (absent and already Deleted).
const listServicesSQL = `SELECT s.id::text, s.customer_id::text, s.kind, s.name, s.slug, s.is_default, s.spec::text,
       s.desired, s.generation, st.service_id IS NOT NULL,
       COALESCE(st.phase, ''), COALESCE(st.message, ''), COALESCE(st.endpoint, ''),
       COALESCE(st.observed_generation, 0)
  FROM services s
  LEFT JOIN service_status st ON st.service_id = s.id
 WHERE NOT (s.desired = 'absent' AND COALESCE(st.phase, '') = 'Deleted')
 ORDER BY s.created_at, s.id`

const upsertServiceStatusSQL = `INSERT INTO service_status (service_id, phase, message, endpoint, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, now())
ON CONFLICT (service_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    endpoint = EXCLUDED.endpoint,
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

// pgxQuerier is what the store needs from pgx: a *pgx.Conn in production, a pgx.Tx in the opt-in
// Postgres test so nothing it writes outlives the test.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type pgxServiceStore struct {
	db    pgxQuerier
	close func(ctx context.Context) error
}

func openPgxServiceStore(ctx context.Context, uri string) (LakeServiceStore, error) {
	config, err := pgx.ParseConfig(uri)
	if err != nil {
		return nil, err
	}
	config.ConnectTimeout = serviceStoreConnectTimeout
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["application_name"] = "orama-operator"
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	return &pgxServiceStore{db: conn, close: conn.Close}, nil
}

func (s *pgxServiceStore) ListServices(ctx context.Context) ([]LakeService, error) {
	rows, err := s.db.Query(ctx, listServicesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var services []LakeService
	for rows.Next() {
		var svc LakeService
		var hasStatus bool
		var status LakeServiceStatus
		if err := rows.Scan(&svc.ID, &svc.CustomerID, &svc.Kind, &svc.Name, &svc.Slug, &svc.IsDefault, &svc.Spec,
			&svc.Desired, &svc.Generation, &hasStatus,
			&status.Phase, &status.Message, &status.Endpoint, &status.ObservedGeneration); err != nil {
			return nil, err
		}
		if hasStatus {
			status.ServiceID = svc.ID
			svc.Status = &status
		}
		services = append(services, svc)
	}
	return services, rows.Err()
}

func (s *pgxServiceStore) UpsertServiceStatus(ctx context.Context, status LakeServiceStatus) error {
	_, err := s.db.Exec(ctx, upsertServiceStatusSQL,
		status.ServiceID, status.Phase, status.Message, status.Endpoint, status.ObservedGeneration)
	return err
}

func (s *pgxServiceStore) Close(ctx context.Context) error {
	if s.close == nil {
		return nil
	}
	return s.close(ctx)
}
