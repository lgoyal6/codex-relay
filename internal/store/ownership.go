package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrOwnedByOther is returned when a thread is already bound to a different workspace.
// The caller must explain the constraint rather than moving the conversation.
var ErrOwnedByOther = errors.New("conversation is already owned by another workspace")

type Ownership struct {
	ThreadID    string
	WorkspaceID string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// ClaimThread binds a thread to a workspace, or confirms the existing binding.
//
// It is a single atomic statement plus a read inside one transaction, so two turns racing
// on the same new thread cannot both win: the loser observes the winner's row and is told
// which workspace actually owns the conversation. Ownership is persisted here, before any
// account-bound state is exposed to the client.
func (d *DB) ClaimThread(ctx context.Context, threadID, workspaceID string, now time.Time) (Ownership, error) {
	if threadID == "" || workspaceID == "" {
		return Ownership{}, fmt.Errorf("thread id and workspace id are required")
	}
	ts := now.UTC().Format(time.RFC3339Nano)

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Ownership{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// INSERT ... ON CONFLICT DO UPDATE only refreshes last_seen_at, and only when the
	// workspace already matches. A conflicting workspace leaves the row untouched.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO thread_ownership (thread_id, workspace_id, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(thread_id) DO UPDATE SET last_seen_at = excluded.last_seen_at
		WHERE thread_ownership.workspace_id = excluded.workspace_id
		  AND thread_ownership.released_at IS NULL`,
		threadID, workspaceID, ts, ts); err != nil {
		return Ownership{}, fmt.Errorf("claim thread: %w", err)
	}

	var got Ownership
	var first, last string
	var released sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT thread_id, workspace_id, first_seen_at, last_seen_at, released_at FROM thread_ownership WHERE thread_id = ?`,
		threadID).Scan(&got.ThreadID, &got.WorkspaceID, &first, &last, &released)
	if err != nil {
		return Ownership{}, fmt.Errorf("read ownership: %w", err)
	}
	got.FirstSeenAt, _ = time.Parse(time.RFC3339Nano, first)
	got.LastSeenAt, _ = time.Parse(time.RFC3339Nano, last)

	if got.WorkspaceID != workspaceID {
		return got, ErrOwnedByOther
	}
	if released.Valid && released.String != "" {
		// A released thread is not silently re-bound to a new workspace here; the caller
		// decides, and the release had to be explicit.
		if _, err := tx.ExecContext(ctx,
			`UPDATE thread_ownership SET released_at = NULL, release_reason = '', workspace_id = ?, last_seen_at = ? WHERE thread_id = ?`,
			workspaceID, ts, threadID); err != nil {
			return got, err
		}
		got.WorkspaceID = workspaceID
	}
	if err := tx.Commit(); err != nil {
		return got, err
	}
	return got, nil
}

// OwnerOf returns the workspace bound to a thread, or "" when the thread is new.
func (d *DB) OwnerOf(ctx context.Context, threadID string) (string, bool, error) {
	if threadID == "" {
		return "", false, nil
	}
	var ws string
	var released sql.NullString
	err := d.sql.QueryRowContext(ctx,
		`SELECT workspace_id, released_at FROM thread_ownership WHERE thread_id = ?`, threadID).
		Scan(&ws, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if released.Valid && released.String != "" {
		return "", false, nil
	}
	return ws, true, nil
}

// ReleaseThread is the only way a binding is given up, and it always records why.
// The build contract requires any release to be proven safe by compatibility tests before
// it is offered as an automatic behaviour; this call exists for explicit user action.
func (d *DB) ReleaseThread(ctx context.Context, threadID, reason string, now time.Time) error {
	if reason == "" {
		return fmt.Errorf("a release reason is required")
	}
	_, err := d.sql.ExecContext(ctx,
		`UPDATE thread_ownership SET released_at = ?, release_reason = ? WHERE thread_id = ?`,
		now.UTC().Format(time.RFC3339Nano), reason, threadID)
	return err
}

// RecordResource binds an account-bound resource id to the workspace that created it.
// Different identifier kinds have different scopes, so the kind is part of the key.
func (d *DB) RecordResource(ctx context.Context, kind, resourceID, workspaceID, threadID string, now time.Time) error {
	if kind == "" || resourceID == "" || workspaceID == "" {
		return fmt.Errorf("kind, resource id and workspace id are required")
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO resource_ownership (kind, resource_id, workspace_id, thread_id, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(kind, resource_id) DO NOTHING`,
		kind, resourceID, workspaceID, nullable(threadID), now.UTC().Format(time.RFC3339Nano))
	return err
}

// ResourceOwner returns the workspace that owns a resource id.
func (d *DB) ResourceOwner(ctx context.Context, kind, resourceID string) (string, bool, error) {
	var ws string
	err := d.sql.QueryRowContext(ctx,
		`SELECT workspace_id FROM resource_ownership WHERE kind = ? AND resource_id = ?`, kind, resourceID).Scan(&ws)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ws, true, nil
}

// ThreadsForWorkspace lists live bindings, used to explain what removing a workspace affects.
func (d *DB) ThreadsForWorkspace(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT thread_id FROM thread_ownership WHERE workspace_id = ? AND released_at IS NULL ORDER BY last_seen_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ReassignThread deliberately re-points a conversation at a different workspace.
//
// ClaimThread refuses this on purpose: an accidental re-bind is how a conversation ends up
// split across accounts, and that guard is worth keeping. A handoff is the one case where the
// move is intended, so it gets its own function rather than a flag that could be passed by
// mistake. The previous owner is returned so the caller can record what moved and from where.
func (d *DB) ReassignThread(ctx context.Context, threadID, workspaceID string, now time.Time) (previous string, err error) {
	if threadID == "" || workspaceID == "" {
		return "", fmt.Errorf("thread id and workspace id are required")
	}
	ts := now.UTC().Format(time.RFC3339Nano)

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	_ = tx.QueryRowContext(ctx,
		`SELECT workspace_id FROM thread_ownership WHERE thread_id = ?`, threadID).Scan(&previous)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO thread_ownership (thread_id, workspace_id, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(thread_id) DO UPDATE SET
			workspace_id = excluded.workspace_id,
			last_seen_at = excluded.last_seen_at,
			released_at = NULL,
			release_reason = ''`,
		threadID, workspaceID, ts, ts); err != nil {
		return previous, fmt.Errorf("reassign thread: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return previous, err
	}
	return previous, nil
}
