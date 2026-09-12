package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lgoyal6/codex-relay/internal/integration"
)

// ChangeRecorder persists integration changes in SQLite so rollback is precise across
// restarts.
type ChangeRecorder struct{ Svc *Service }

func (r ChangeRecorder) Record(c integration.Change) error {
	_, err := r.Svc.DB.SQL().Exec(`
		INSERT INTO integration_changes (at, target_path, backup_path, before_sha256, after_sha256, description)
		VALUES (?,?,?,?,?,?)`,
		c.At.Format(time.RFC3339Nano), c.TargetPath, c.BackupPath, c.BeforeSHA256, c.AfterSHA256, c.Description)
	return err
}

func (r ChangeRecorder) Latest(target string) (integration.Change, bool, error) {
	var c integration.Change
	var at string
	err := r.Svc.DB.SQL().QueryRowContext(context.Background(), `
		SELECT id, at, target_path, backup_path, before_sha256, after_sha256, description
		FROM integration_changes
		WHERE target_path = ? AND rolled_back_at IS NULL
		ORDER BY id DESC LIMIT 1`, target).
		Scan(&c.ID, &at, &c.TargetPath, &c.BackupPath, &c.BeforeSHA256, &c.AfterSHA256, &c.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return integration.Change{}, false, nil
	}
	if err != nil {
		return integration.Change{}, false, err
	}
	c.At, _ = time.Parse(time.RFC3339Nano, at)
	return c, true, nil
}

func (r ChangeRecorder) MarkRolledBack(id int64, at time.Time) error {
	_, err := r.Svc.DB.SQL().Exec(`UPDATE integration_changes SET rolled_back_at = ? WHERE id = ?`,
		at.Format(time.RFC3339Nano), id)
	return err
}

var _ integration.Recorder = ChangeRecorder{}
