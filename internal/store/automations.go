package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Automation kinds. Kept to the few that are actually wanted on a timer; anything broader
// becomes a job scheduler, which is a different product.
const (
	// AutomationPauseDaily pauses a workspace at a minute past midnight UTC.
	AutomationPauseDaily = "pause_daily"
	// AutomationResumeDaily resumes at a minute past midnight UTC.
	AutomationResumeDaily = "resume_daily"
	// AutomationResumeOnReset resumes a paused workspace once its quota window has rolled
	// over, so a reset at an awkward hour does not need a person watching for it.
	AutomationResumeOnReset = "resume_on_reset"
)

// ErrAutomationNotFound is returned when no such automation exists.
var ErrAutomationNotFound = errors.New("no such automation")

type Automation struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	WorkspaceID string     `json:"workspace_id"`
	AtMinute    *int64     `json:"at_minute"`
	Enabled     bool       `json:"enabled"`
	LastRunAt   *time.Time `json:"last_run_at"`
	LastResult  string     `json:"last_result,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (d *DB) CreateAutomation(ctx context.Context, a Automation, now time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO automations (id, name, kind, workspace_id, at_minute, enabled, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Kind, a.WorkspaceID, a.AtMinute, boolToInt(a.Enabled),
		now.UTC().Format(time.RFC3339Nano))
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (d *DB) ListAutomations(ctx context.Context) ([]Automation, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, name, kind, workspace_id, at_minute, enabled, last_run_at, last_result, created_at
		FROM automations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Automation{}
	for rows.Next() {
		var a Automation
		var created string
		var lastRun, lastResult sql.NullString
		var minute sql.NullInt64
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.WorkspaceID, &minute, &enabled,
			&lastRun, &lastResult, &created); err != nil {
			return nil, err
		}
		a.Enabled = enabled == 1
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if minute.Valid {
			v := minute.Int64
			a.AtMinute = &v
		}
		if lastRun.Valid {
			if t, err := time.Parse(time.RFC3339Nano, lastRun.String); err == nil {
				a.LastRunAt = &t
			}
		}
		a.LastResult = lastResult.String
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) DeleteAutomation(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM automations WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAutomationNotFound
	}
	return nil
}

func (d *DB) SetAutomationEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE automations SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAutomationNotFound
	}
	return nil
}

// MarkAutomationRun records an outcome. Best effort: a bookkeeping failure must not stop the
// action that already happened.
func (d *DB) MarkAutomationRun(ctx context.Context, id, result string, now time.Time) {
	_, _ = d.sql.ExecContext(ctx,
		`UPDATE automations SET last_run_at = ?, last_result = ? WHERE id = ?`,
		now.UTC().Format(time.RFC3339Nano), result, id)
}
