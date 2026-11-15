// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var errVolumeSnapshotsTableMissing = errors.New("the volume_snapshot_policies table does not exist yet (lake schema before 0069_workspace_volumes)")

// LakeVolumeSnapshotPolicy is one row of lake's volume_snapshot_policies (migration 0069,
// operator#260) with the workspace it names, if any, and the status the operator last wrote.
// Target is "store" (lake's own store volume) or a workspace id.
type LakeVolumeSnapshotPolicy struct {
	Target      string
	Schedule    string
	Keep        int32
	RequestedAt *time.Time
	// WorkspaceDesired and WorkspaceNamespace come from the workspaces row a workspace target
	// names: empty for "store" and for a workspace lake no longer has.
	WorkspaceDesired   string
	WorkspaceNamespace string
	Status             *LakeVolumeSnapshotStatus
	// DBNow is Postgres's now() when the list was read: the one clock requested_at, schedules and
	// the snapshots' recorded instants are judged by, so the operator's clock skew cannot make a
	// request look already honored.
	DBNow time.Time
}

// LakeVolumeSnapshotStatus is one volume_snapshot_status row, written only by the operator.
type LakeVolumeSnapshotStatus struct {
	Target       string
	SnapshotName string
	ReadyToUse   bool
	SnapshotAt   *time.Time
	Message      string
}

func (s LakeVolumeSnapshotStatus) equal(other LakeVolumeSnapshotStatus) bool {
	sameTime := (s.SnapshotAt == nil) == (other.SnapshotAt == nil) &&
		(s.SnapshotAt == nil || s.SnapshotAt.Truncate(time.Microsecond).Equal(other.SnapshotAt.Truncate(time.Microsecond)))
	return s.Target == other.Target && s.SnapshotName == other.SnapshotName &&
		s.ReadyToUse == other.ReadyToUse && s.Message == other.Message && sameTime
}

// LakeVolumeSnapshotsStore reads the policies and writes their status in lake's control Postgres.
type LakeVolumeSnapshotsStore interface {
	ListVolumeSnapshotPolicies(context.Context) ([]LakeVolumeSnapshotPolicy, error)
	UpsertVolumeSnapshotStatus(context.Context, LakeVolumeSnapshotStatus) error
	Close(context.Context) error
}

type LakeVolumeSnapshotsStoreOpener func(context.Context, string) (LakeVolumeSnapshotsStore, error)

type pgxVolumeSnapshotsStore struct {
	*pgxServiceStore
}

func openPgxVolumeSnapshotsStore(ctx context.Context, uri string) (LakeVolumeSnapshotsStore, error) {
	store, err := openPgxServiceStore(ctx, uri)
	if err != nil {
		return nil, errors.New("cannot connect to Lake control Postgres")
	}
	return &pgxVolumeSnapshotsStore{pgxServiceStore: store.(*pgxServiceStore)}, nil
}

const listVolumeSnapshotPoliciesSQL = `SELECT p.target, p.schedule, p.keep, p.requested_at,
 COALESCE(w.desired, ''), COALESCE(w.namespace, ''),
 st.target IS NOT NULL, COALESCE(st.snapshot_name, ''), COALESCE(st.ready_to_use, false),
 st.snapshot_at, COALESCE(st.message, ''), now()
 FROM volume_snapshot_policies p
 LEFT JOIN workspaces w ON w.id::text = p.target
 LEFT JOIN volume_snapshot_status st ON st.target = p.target
 ORDER BY p.target <> 'store', p.target`

func (s *pgxVolumeSnapshotsStore) ListVolumeSnapshotPolicies(ctx context.Context) ([]LakeVolumeSnapshotPolicy, error) {
	rows, err := s.db.Query(ctx, listVolumeSnapshotPoliciesSQL)
	if err != nil {
		return nil, volumeSnapshotsReadError(err)
	}
	defer rows.Close()
	var result []LakeVolumeSnapshotPolicy
	for rows.Next() {
		var policy LakeVolumeSnapshotPolicy
		var hasStatus bool
		var status LakeVolumeSnapshotStatus
		if err := rows.Scan(
			&policy.Target,
			&policy.Schedule,
			&policy.Keep,
			&policy.RequestedAt,
			&policy.WorkspaceDesired,
			&policy.WorkspaceNamespace,
			&hasStatus,
			&status.SnapshotName,
			&status.ReadyToUse,
			&status.SnapshotAt,
			&status.Message,
			&policy.DBNow,
		); err != nil {
			return nil, errors.New("cannot decode volume snapshot policy row")
		}
		if hasStatus {
			status.Target = policy.Target
			policy.Status = &status
		}
		result = append(result, policy)
	}
	if err := rows.Err(); err != nil {
		return nil, volumeSnapshotsReadError(err)
	}
	return result, nil
}

func volumeSnapshotsReadError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errVolumeSnapshotsTableMissing
	}
	return errors.New("volume snapshot policies list did not complete")
}

const upsertVolumeSnapshotStatusSQL = `INSERT INTO volume_snapshot_status (target, snapshot_name, ready_to_use, snapshot_at, message, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (target) DO UPDATE SET
    snapshot_name = EXCLUDED.snapshot_name,
    ready_to_use = EXCLUDED.ready_to_use,
    snapshot_at = EXCLUDED.snapshot_at,
    message = EXCLUDED.message,
    updated_at = now()`

func (s *pgxVolumeSnapshotsStore) UpsertVolumeSnapshotStatus(ctx context.Context, status LakeVolumeSnapshotStatus) error {
	if _, err := s.db.Exec(ctx, upsertVolumeSnapshotStatusSQL, status.Target, status.SnapshotName,
		status.ReadyToUse, status.SnapshotAt, status.Message); err != nil {
		return errors.New("cannot write volume_snapshot_status")
	}
	return nil
}
