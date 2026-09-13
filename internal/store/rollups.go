package store

import (
	"context"
	"time"
)

// RollupBucket is one hour of aggregated usage for one workspace and one client.
type RollupBucket struct {
	Hour         time.Time `json:"hour"`
	WorkspaceID  string    `json:"workspace_id,omitempty"`
	APIKeyID     string    `json:"api_key_id,omitempty"`
	Turns        int64     `json:"turns"`
	Blocked      int64     `json:"blocked"`
	Errors       int64     `json:"errors"`
	InputTokens  int64     `json:"input_tokens"`
	CachedTokens int64     `json:"cached_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalTokens  int64     `json:"total_tokens"`
}

// RollupDelta is a single turn's contribution.
type RollupDelta struct {
	At           time.Time
	WorkspaceID  string
	APIKeyID     string
	Served       bool
	Blocked      bool
	Errored      bool
	InputTokens  int64
	CachedTokens int64
	OutputTokens int64
	TotalTokens  int64
}

// HourKey is the bucket a moment belongs to, always UTC so buckets do not shift when the
// machine's timezone or daylight saving changes.
func HourKey(t time.Time) string { return t.UTC().Format("2006-01-02T15") }

// AddRollup folds one turn into its hour.
//
// Written on the same path that writes a decision, but into a table that is never pruned, so
// the aggregate outlives the raw row it came from.
func (d *DB) AddRollup(ctx context.Context, delta RollupDelta) error {
	turns, blocked, errors := int64(0), int64(0), int64(0)
	if delta.Served {
		turns = 1
	}
	if delta.Blocked {
		blocked = 1
	}
	if delta.Errored {
		errors = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO usage_rollups (hour_utc, workspace_id, api_key_id, turns, blocked, errors,
			input_tokens, cached_tokens, output_tokens, total_tokens)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hour_utc, workspace_id, api_key_id) DO UPDATE SET
			turns         = turns         + excluded.turns,
			blocked       = blocked       + excluded.blocked,
			errors        = errors        + excluded.errors,
			input_tokens  = input_tokens  + excluded.input_tokens,
			cached_tokens = cached_tokens + excluded.cached_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			total_tokens  = total_tokens  + excluded.total_tokens`,
		HourKey(delta.At), delta.WorkspaceID, delta.APIKeyID,
		turns, blocked, errors,
		delta.InputTokens, delta.CachedTokens, delta.OutputTokens, delta.TotalTokens)
	return err
}

// Rollups returns buckets from since onwards, oldest first.
func (d *DB) Rollups(ctx context.Context, since time.Time) ([]RollupBucket, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT hour_utc, workspace_id, api_key_id, turns, blocked, errors,
		       input_tokens, cached_tokens, output_tokens, total_tokens
		FROM usage_rollups WHERE hour_utc >= ? ORDER BY hour_utc`, HourKey(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RollupBucket{}
	for rows.Next() {
		var b RollupBucket
		var hour string
		if err := rows.Scan(&hour, &b.WorkspaceID, &b.APIKeyID, &b.Turns, &b.Blocked, &b.Errors,
			&b.InputTokens, &b.CachedTokens, &b.OutputTokens, &b.TotalTokens); err != nil {
			return nil, err
		}
		b.Hour, _ = time.Parse("2006-01-02T15", hour)
		out = append(out, b)
	}
	return out, rows.Err()
}
