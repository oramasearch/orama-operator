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
	ID, CustomerID, WorkspaceID, WorkspaceNamespace, Name, Mode    string
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
	// The stores apps mount (lake#666): the same statements as the services poll. ListStoreMounts
	// returns errStoreMountsTableMissing while lake's schema is older than 0079_store_mounts.
	ListStores(context.Context) ([]LakeStore, error)
	ListStoreMounts(context.Context) ([]LakeStoreMount, error)
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

const listAppsSQL = `SELECT a.id::text, a.customer_id::text, a.workspace_id::text, COALESCE(w.namespace, ''), a.name, a.mode,
 COALESCE(a.source_repository, ''), COALESCE(a.source_revision, ''), COALESCE(a.source_path, ''),
 COALESCE(a.source_entrypoint, ''), COALESCE(a.image, ''), COALESCE(a.framework, ''),
 COALESCE(a.port, 8080), COALESCE(a.replicas, 1), COALESCE(a.host, ''), COALESCE(a.auth, 'lake'),
 COALESCE(a.bindings_json, '{}'::jsonb)::text, COALESCE(a.env_json, '{}'::jsonb)::text,
 COALESCE(a.secret_env_refs, '{}'::jsonb)::text, a.desired, a.generation,
 COALESCE(a.phase, 'Pending'), COALESCE(a.url, ''), COALESCE(a.ready, false), COALESCE(a.message, ''),
 COALESCE(a.observed_generation, 0), COALESCE(a.token_hashes_json, 'null'::jsonb)::text
 FROM apps a
 JOIN workspaces w ON w.id = a.workspace_id
 ORDER BY a.id`

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
		err := rows.Scan(&app.ID, &app.CustomerID, &app.WorkspaceID, &app.WorkspaceNamespace, &app.Name, &app.Mode,
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
