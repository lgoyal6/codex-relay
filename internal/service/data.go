package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/routing"
)

// --- settings ---

func (s *Service) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.SQL().QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Service) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.SQL().ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, s.Clock.Now().Format(time.RFC3339Nano))
	return err
}

// --- rules ---

func (s *Service) LoadRules(ctx context.Context) ([]policy.Rule, error) {
	rows, err := s.DB.SQL().QueryContext(ctx, `
		SELECT id, kind, enabled, priority, source_workspace_id, window_minutes, comparison,
		       remaining_percent, reset_comparison, reset_hours, preferred_workspace_id,
		       no_alternative, created_at, updated_at
		FROM rules ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []policy.Rule
	for rows.Next() {
		var r policy.Rule
		var enabled int
		var created, updated string
		var kind, cmp, rcmp, na string
		if err := rows.Scan(&r.ID, &kind, &enabled, &r.Priority, &r.SourceWorkspaceID,
			&r.Window.Minutes, &cmp, &r.RemainingPercent, &rcmp, &r.ResetHours,
			&r.PreferredWorkspaceID, &na, &created, &updated); err != nil {
			return nil, err
		}
		r.Kind = policy.RuleKind(kind)
		r.Comparison = policy.Comparison(cmp)
		r.ResetComparison = policy.ResetComparison(rcmp)
		r.NoAlternative = policy.NoAlternativeBehavior(na)
		r.Enabled = enabled == 1
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveRule validates, persists, and only then republishes. The dashboard must not claim a
// policy is active before this returns.
func (s *Service) SaveRule(ctx context.Context, r policy.Rule) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := s.checkConflicts(ctx, r); err != nil {
		return err
	}
	now := s.Clock.Now().Format(time.RFC3339Nano)
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := s.DB.SQL().ExecContext(ctx, `
		INSERT INTO rules (id, kind, enabled, priority, source_workspace_id, window_minutes, comparison,
			remaining_percent, reset_comparison, reset_hours, preferred_workspace_id, no_alternative,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, enabled=excluded.enabled, priority=excluded.priority,
			source_workspace_id=excluded.source_workspace_id, window_minutes=excluded.window_minutes,
			comparison=excluded.comparison, remaining_percent=excluded.remaining_percent,
			reset_comparison=excluded.reset_comparison, reset_hours=excluded.reset_hours,
			preferred_workspace_id=excluded.preferred_workspace_id, no_alternative=excluded.no_alternative,
			updated_at=excluded.updated_at`,
		r.ID, string(r.Kind), enabled, r.Priority, r.SourceWorkspaceID, r.Window.Minutes,
		string(r.Comparison), r.RemainingPercent, string(r.ResetComparison), r.ResetHours,
		r.PreferredWorkspaceID, string(r.NoAlternative), now, now)
	if err != nil {
		return err
	}
	return s.Refresh(ctx)
}

func (s *Service) DeleteRule(ctx context.Context, id string) error {
	if _, err := s.DB.SQL().ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Refresh(ctx)
}

// checkConflicts rejects configurations that cannot be satisfied together, with a message
// that names the other rule rather than failing silently at admission time.
func (s *Service) checkConflicts(ctx context.Context, r policy.Rule) error {
	existing, err := s.LoadRules(ctx)
	if err != nil {
		return err
	}
	for _, other := range existing {
		if other.ID == r.ID || !other.Enabled || !r.Enabled {
			continue
		}
		if r.Kind == policy.KindReserve && other.Kind == policy.KindReserve {
			// Two reserves that protect each other's alternative can deadlock into
			// "nothing is eligible" whenever both conditions hold.
			if other.SourceWorkspaceID == r.PreferredWorkspaceID && r.SourceWorkspaceID == other.PreferredWorkspaceID {
				return fmt.Errorf(
					"this conflicts with an existing rule: each workspace is the other's alternative, so when both conditions match nothing would be eligible")
			}
		}
		if r.Kind == policy.KindReserve && other.Kind == policy.KindPrefer &&
			other.SourceWorkspaceID == r.SourceWorkspaceID {
			return fmt.Errorf(
				"this conflicts with an existing rule that prefers the same workspace this rule protects; protection would always win, making the preference dead")
		}
	}
	return nil
}

// --- preview ---

// Scenario is a hypothetical override used by the rule editor.
type Scenario struct {
	WorkspaceID      string   `json:"workspace_id"`
	WindowMinutes    int64    `json:"window_minutes"`
	RemainingPercent *float64 `json:"remaining_percent"`
	ResetInHours     *float64 `json:"reset_in_hours"`
}

// Preview evaluates a candidate rule set against current data, or against a scenario.
//
// It runs the SAME evaluator, on a deep copy of the SAME published snapshot, at an explicit
// time. It cannot mutate live quota or ownership, and it consumes no model quota. The result
// is a point-in-time answer, not a reservation.
func (s *Service) Preview(ctx context.Context, rules []policy.Rule, model string, scenarios []Scenario) (policy.Decision, error) {
	st := routing.CloneForPreview(s.Registry.Current())
	if rules != nil {
		st.Rules = rules
	}
	now := s.Clock.Now()
	simulated := false

	for _, sc := range scenarios {
		ws := st.Workspaces[sc.WorkspaceID]
		if ws == nil {
			return policy.Decision{}, fmt.Errorf("unknown workspace %q in scenario", sc.WorkspaceID)
		}
		w := ws.Windows[sc.WindowMinutes]
		w.Minutes = sc.WindowMinutes
		w.ObservedAt = now
		if sc.RemainingPercent != nil {
			w.UsedPercent = 100 - *sc.RemainingPercent
			simulated = true
		}
		if sc.ResetInHours != nil {
			t := now.Add(time.Duration(*sc.ResetInHours * float64(time.Hour)))
			w.ResetsAt = &t
			simulated = true
		}
		ws.Windows[sc.WindowMinutes] = w
	}

	d := policy.Evaluate(st, policy.Request{Model: model}, now)
	d.Simulated = simulated
	return d, nil
}

// --- activity ---

type ActivityRow struct {
	ID          int64              `json:"id"`
	At          time.Time          `json:"at"`
	ThreadID    string             `json:"thread_id,omitempty"`
	Model       string             `json:"model,omitempty"`
	Outcome     string             `json:"outcome"`
	WorkspaceID string             `json:"workspace_id,omitempty"`
	Reason      string             `json:"reason"`
	Summary     string             `json:"summary"`
	Notes       []policy.Note      `json:"notes"`
	Candidates  []policy.Candidate `json:"candidates"`
	Attempt     int                `json:"attempt"`
	StatusCode  int                `json:"status_code,omitempty"`
	// Pointers, not omitempty: a turn whose first token arrived in under a millisecond is a
	// real 0 ms measurement, and `omitempty` silently dropped it so the dashboard could not
	// tell it from "never measured". null means not measured; 0 means measured as zero.
	FirstTokenMS *int64 `json:"first_token_ms"`
	TotalMS      *int64 `json:"total_ms"`
	ErrorClass   string `json:"error_class,omitempty"`

	// Diagnostic detail. ErrorClass buckets a failure; these say what actually happened.
	ErrorMessage string `json:"error_message,omitempty"`
	// FailurePhase is how far the turn got: admission, upstream_connect, upstream_status,
	// stream, client. A 200 that died mid-stream and a 429 refused before anything was sent
	// are different problems, and the status code cannot separate them.
	FailurePhase string `json:"failure_phase,omitempty"`
	// UpstreamStatus can differ from StatusCode: a quota refusal retried on another
	// workspace shows 429 here and 200 to the client.
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	Transport      string `json:"transport,omitempty"`
	// UpstreamMS is how long upstream took to answer at all: response headers on HTTP, the
	// upgrade on WebSocket. Subtracting it from FirstTokenMS does NOT give the relay's
	// own cost, because the model starts generating only after that point.
	UpstreamMS *int64 `json:"upstream_ms"`
}

func (s *Service) Activity(ctx context.Context, limit int) ([]ActivityRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.SQL().QueryContext(ctx, `
		SELECT id, at, thread_id, model, outcome, workspace_id, primary_reason, summary,
		       detail_json, attempt, status_code, first_token_ms, total_ms, error_class,
		       error_message, failure_phase, upstream_status, transport, upstream_ms
		FROM decisions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActivityRow{}
	for rows.Next() {
		var r ActivityRow
		var at, detail string
		var thread, model, ws, errClass, errMsg, phase, transport sql.NullString
		var status, first, total, upStatus, upMS sql.NullInt64
		if err := rows.Scan(&r.ID, &at, &thread, &model, &r.Outcome, &ws, &r.Reason, &r.Summary,
			&detail, &r.Attempt, &status, &first, &total, &errClass,
			&errMsg, &phase, &upStatus, &transport, &upMS); err != nil {
			return nil, err
		}
		r.ErrorMessage, r.FailurePhase, r.Transport = errMsg.String, phase.String, transport.String
		r.UpstreamStatus = int(upStatus.Int64)
		if upMS.Valid {
			v := upMS.Int64
			r.UpstreamMS = &v
		}
		r.At, _ = time.Parse(time.RFC3339Nano, at)
		r.ThreadID, r.Model, r.WorkspaceID, r.ErrorClass = thread.String, model.String, ws.String, errClass.String
		r.StatusCode = int(status.Int64)
		if first.Valid {
			v := first.Int64
			r.FirstTokenMS = &v
		}
		if total.Valid {
			v := total.Int64
			r.TotalMS = &v
		}
		var d struct {
			Notes      []policy.Note      `json:"notes"`
			Candidates []policy.Candidate `json:"candidates"`
		}
		_ = json.Unmarshal([]byte(detail), &d)
		r.Notes, r.Candidates = d.Notes, d.Candidates
		out = append(out, r)
	}
	return out, rows.Err()
}
