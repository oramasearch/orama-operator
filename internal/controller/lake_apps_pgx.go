/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

var errAppsTableMissing = errors.New("the apps table is not installed yet")

// LakeAppRow is lake's desired apps record, including the last observed state. JSON fields
// contain configuration and references only, never plaintext credentials.
type LakeAppRow struct {
	ID, CustomerID, Name, Mode                                     string
	SourceRepository, SourceRevision, SourcePath, SourceEntrypoint string
	Image, Framework                                               string
	Port, Replicas                                                 int32
	Host, Auth                                                     string
	BindingsJSON, EnvJSON, SecretEnvRefs                           string
	Desired                                                        string
	Generation                                                     int64
	Status                                                         LakeAppRowStatus
	TokenHashes                                                    *oramav1alpha1.LakeAppTokenHashes
}

type LakeAppRowStatus struct {
	Phase              string
	URL                string
	Ready              bool
	Message            string
	ObservedGeneration int64
}

// LakeAppsStore follows the Services connection contract. Conditional writes refuse a row
// changed or removed after the list, so old observations never acknowledge newer desired state.
type LakeAppsStore interface {
	ListApps(context.Context) ([]LakeAppRow, error)
	ListServices(context.Context) ([]LakeService, error)
	UpdateAppStatus(context.Context, *LakeAppRow, LakeAppRowStatus) (bool, error)
	UpdateAppTokenHashes(context.Context, *LakeAppRow, *oramav1alpha1.LakeAppTokenHashes) (bool, error)
	Close(context.Context) error
}

type LakeAppsStoreOpener func(context.Context, string) (LakeAppsStore, error)

type pgxAppsStore struct {
	*pgxServiceStore
}

func openPgxAppsStore(ctx context.Context, uri string) (LakeAppsStore, error) {
	store, err := openPgxServiceStore(ctx, uri)
	if err != nil {
		return nil, errors.New("cannot connect to Lake control Postgres")
	}
	return &pgxAppsStore{pgxServiceStore: store.(*pgxServiceStore)}, nil
}

const listAppsSQL = `SELECT id::text, customer_id::text, name, mode,
 COALESCE(source_repository, ''), COALESCE(source_revision, ''), COALESCE(source_path, ''),
 COALESCE(source_entrypoint, ''), COALESCE(image, ''), COALESCE(framework, ''),
 COALESCE(port, 8080), COALESCE(replicas, 1), COALESCE(host, ''), COALESCE(auth, 'lake'),
 COALESCE(bindings_json, '{}'::jsonb)::text, COALESCE(env_json, '{}'::jsonb)::text,
 COALESCE(secret_env_refs, '[]'::jsonb)::text, desired, generation,
 COALESCE(phase, 'Pending'), COALESCE(url, ''), COALESCE(ready, false), COALESCE(message, ''),
 COALESCE(observed_generation, 0), COALESCE(token_hashes_json, 'null'::jsonb)::text
 FROM apps ORDER BY id`

func (s *pgxAppsStore) ListApps(ctx context.Context) ([]LakeAppRow, error) {
	rows, err := s.db.Query(ctx, listAppsSQL)
	if err != nil {
		return nil, appsReadError(err)
	}
	defer rows.Close()
	var apps []LakeAppRow
	for rows.Next() {
		var app LakeAppRow
		var hashes string
		err := rows.Scan(&app.ID, &app.CustomerID, &app.Name, &app.Mode,
			&app.SourceRepository, &app.SourceRevision, &app.SourcePath, &app.SourceEntrypoint,
			&app.Image, &app.Framework, &app.Port, &app.Replicas, &app.Host, &app.Auth,
			&app.BindingsJSON, &app.EnvJSON, &app.SecretEnvRefs, &app.Desired, &app.Generation,
			&app.Status.Phase, &app.Status.URL, &app.Status.Ready, &app.Status.Message,
			&app.Status.ObservedGeneration, &hashes)
		if err != nil {
			return nil, errors.New("cannot decode apps row")
		}
		if err := json.Unmarshal([]byte(hashes), &app.TokenHashes); err != nil {
			return nil, errors.New("cannot decode app token hashes")
		}
		apps = append(apps, app)
	}
	if rows.Err() != nil {
		return nil, appsReadError(rows.Err())
	}
	return apps, nil
}

func appsReadError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errAppsTableMissing
	}
	return errors.New("apps list did not complete")
}

const updateAppStatusSQL = `UPDATE apps SET phase=$5, url=$6, ready=$7, message=$8,
 observed_generation=$9, updated_at=now()
 WHERE id=$1::uuid AND customer_id=$2::uuid AND generation=$3 AND desired=$4`

func (s *pgxAppsStore) UpdateAppStatus(ctx context.Context, app *LakeAppRow, status LakeAppRowStatus) (bool, error) {
	result, err := s.db.Exec(ctx, updateAppStatusSQL, app.ID, app.CustomerID, app.Generation, app.Desired,
		status.Phase, status.URL, status.Ready, status.Message, status.ObservedGeneration)
	return appRowWriteResult(result, err)
}

const updateAppTokenHashesSQL = `UPDATE apps SET token_hashes_json=$5::jsonb
 WHERE id=$1::uuid AND customer_id=$2::uuid AND generation=$3 AND desired=$4`

func (s *pgxAppsStore) UpdateAppTokenHashes(ctx context.Context, app *LakeAppRow, hashes *oramav1alpha1.LakeAppTokenHashes) (bool, error) {
	encoded, err := json.Marshal(hashes)
	if err != nil {
		return false, errors.New("cannot encode app token hashes")
	}
	result, err := s.db.Exec(ctx, updateAppTokenHashesSQL, app.ID, app.CustomerID, app.Generation, app.Desired, string(encoded))
	return appRowWriteResult(result, err)
}

func appRowWriteResult(result pgconn.CommandTag, err error) (bool, error) {
	if err != nil {
		return false, errors.New("cannot update apps row")
	}
	return result.RowsAffected() == 1, nil
}
