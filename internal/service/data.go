package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/proxy"
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
	ID            int64              `json:"id"`
	At            time.Time          `json:"at"`
	ThreadID      string             `json:"thread_id,omitempty"`
	Model         string             `json:"model,omitempty"`
	Outcome       string             `json:"outcome"`
	WorkspaceID   string             `json:"workspace_id,omitempty"`
	WorkspaceName string             `json:"workspace_name,omitempty"`
	AccountEmail  string             `json:"account_email,omitempty"`
	AccountPlan   string             `json:"account_plan,omitempty"`
	Reason        string             `json:"reason"`
	Summary       string             `json:"summary"`
	Notes         []policy.Note      `json:"notes"`
	Candidates    []policy.Candidate `json:"candidates"`
	Attempt       int                `json:"attempt"`
	StatusCode    int                `json:"status_code,omitempty"`
	// Pointers, not omitempty: a turn whose first token arrived in under a millisecond is a
	// real 0 ms measurement, and `omitempty` silently dropped it so the dashboard could not
	// tell it from "never measured". null means not measured; 0 means measured as zero.
	FirstTokenMS *int64 `json:"first_token_ms"`
	TotalMS      *int64 `json:"total_ms"`
	ErrorClass   string `json:"error_class,omitempty"`
	// Token counts come from the model's response.completed usage event. null means the
	// turn did not report usage; zero is a reported zero and must remain distinguishable.
	InputTokens       *int64 `json:"input_tokens"`
	CachedInputTokens *int64 `json:"cached_input_tokens"`
	OutputTokens      *int64 `json:"output_tokens"`
	TotalTokens       *int64 `json:"total_tokens"`

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
	// OutputTokensPerSecond is measured only over the interval after first token. It remains
	// null unless both timings and output usage were reported.
	OutputTokensPerSecond *float64             `json:"output_tokens_per_second"`
	QuotaSnapshot         *proxy.QuotaSnapshot `json:"quota_snapshot"`
}

func (s *Service) Activity(ctx context.Context, limit int) ([]ActivityRow, error) {
	if limit <= 0 || limit > historyLimit {
		limit = 100
	}
	rows, err := s.DB.SQL().QueryContext(ctx, `
		SELECT d.id, d.at, d.thread_id, d.model, d.outcome, d.workspace_id,
		       w.display_name, a.email, a.plan_type,
		       d.primary_reason, d.summary, d.detail_json, d.attempt, d.status_code,
		       d.first_token_ms, d.total_ms, d.error_class,
		       d.input_tokens, d.cached_input_tokens, d.output_tokens, d.total_tokens,
		       d.error_message, d.failure_phase, d.upstream_status, d.transport, d.upstream_ms,
		       d.quota_snapshot_json
		FROM decisions d
		LEFT JOIN workspaces w ON w.id = d.workspace_id
		LEFT JOIN accounts a ON a.id = w.account_id
		ORDER BY d.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActivityRow{}
	for rows.Next() {
		var r ActivityRow
		var at, detail string
		var thread, model, ws, wsName, email, plan, errClass, errMsg, phase, transport, quotaJSON sql.NullString
		var status, first, total, input, cached, output, tokens, upStatus, upMS sql.NullInt64
		if err := rows.Scan(&r.ID, &at, &thread, &model, &r.Outcome, &ws,
			&wsName, &email, &plan, &r.Reason, &r.Summary,
			&detail, &r.Attempt, &status, &first, &total, &errClass,
			&input, &cached, &output, &tokens,
			&errMsg, &phase, &upStatus, &transport, &upMS, &quotaJSON); err != nil {
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
		r.WorkspaceName, r.AccountEmail, r.AccountPlan = wsName.String, email.String, plan.String
		r.StatusCode = int(status.Int64)
		if first.Valid {
			v := first.Int64
			r.FirstTokenMS = &v
		}
		if total.Valid {
			v := total.Int64
			r.TotalMS = &v
		}
		if input.Valid {
			v := input.Int64
			r.InputTokens = &v
		}
		if cached.Valid {
			v := cached.Int64
			r.CachedInputTokens = &v
		}
		if output.Valid {
			v := output.Int64
			r.OutputTokens = &v
		}
		if tokens.Valid {
			v := tokens.Int64
			r.TotalTokens = &v
		}
		if output.Valid && first.Valid && total.Valid && total.Int64 > first.Int64 {
			v := float64(output.Int64) / (float64(total.Int64-first.Int64) / 1000)
			r.OutputTokensPerSecond = &v
		}
		if quotaJSON.Valid {
			var snapshot proxy.QuotaSnapshot
			if json.Unmarshal([]byte(quotaJSON.String), &snapshot) == nil {
				if snapshot.Windows == nil {
					snapshot.Windows = []proxy.QuotaWindowSnapshot{}
				}
				r.QuotaSnapshot = &snapshot
			}
		}
		var d struct {
			Notes      []policy.Note      `json:"notes"`
			Candidates []policy.Candidate `json:"candidates"`
		}
		_ = json.Unmarshal([]byte(detail), &d)
		// Never nil. A nil slice marshals to JSON null and the dashboard maps over both of
		// these, so one row with no decision attached (an authentication refusal, for
		// instance) would black-screen the whole page. This is the class of the bug, not
		// just the instance: every list the dashboard iterates is guaranteed here.
		r.Notes, r.Candidates = d.Notes, d.Candidates
		if r.Notes == nil {
			r.Notes = []policy.Note{}
		}
		if r.Candidates == nil {
			r.Candidates = []policy.Candidate{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type ModelReportRow struct {
	Model                  string   `json:"model"`
	Turns                  int      `json:"turns"`
	TurnsWithUsage         int      `json:"turns_with_usage"`
	InputTokens            int64    `json:"input_tokens"`
	CachedInputTokens      int64    `json:"cached_input_tokens"`
	OutputTokens           int64    `json:"output_tokens"`
	TotalTokens            int64    `json:"total_tokens"`
	MedianFirstTokenMS     *float64 `json:"median_first_token_ms"`
	OutputTokensPerSecond  *float64 `json:"output_tokens_per_second"`
	firstTokenMeasurements []float64
	generationSeconds      float64
	measuredOutputTokens   int64
}

// ModelReport summarizes completed served turns across all retained decision rows. Errors,
// cancellations, and blocked decisions do not become performance data.
func (s *Service) ModelReport(ctx context.Context) ([]ModelReportRow, error) {
	activity, err := s.Activity(ctx, historyLimit)
	if err != nil {
		return nil, err
	}
	byModel := map[string]*ModelReportRow{}
	for _, row := range activity {
		if row.Outcome != string(policy.OutcomeSelected) || row.ErrorClass != "" || row.StatusCode >= 400 {
			continue
		}
		model := row.Model
		if model == "" {
			model = "unknown"
		}
		r := byModel[model]
		if r == nil {
			r = &ModelReportRow{Model: model}
			byModel[model] = r
		}
		r.Turns++
		if row.FirstTokenMS != nil {
			r.firstTokenMeasurements = append(r.firstTokenMeasurements, float64(*row.FirstTokenMS))
		}
		if row.TotalTokens != nil {
			r.TurnsWithUsage++
		}
		if row.InputTokens != nil {
			r.InputTokens += *row.InputTokens
		}
		if row.CachedInputTokens != nil {
			r.CachedInputTokens += *row.CachedInputTokens
		}
		if row.OutputTokens != nil {
			r.OutputTokens += *row.OutputTokens
		}
		if row.TotalTokens != nil {
			r.TotalTokens += *row.TotalTokens
		}
		if row.OutputTokens != nil && row.FirstTokenMS != nil && row.TotalMS != nil && *row.TotalMS > *row.FirstTokenMS {
			r.measuredOutputTokens += *row.OutputTokens
			r.generationSeconds += float64(*row.TotalMS-*row.FirstTokenMS) / 1000
		}
	}
	out := make([]ModelReportRow, 0, len(byModel))
	for _, row := range byModel {
		sort.Float64s(row.firstTokenMeasurements)
		if n := len(row.firstTokenMeasurements); n > 0 {
			median := row.firstTokenMeasurements[n/2]
			if n%2 == 0 {
				median = (row.firstTokenMeasurements[n/2-1] + median) / 2
			}
			row.MedianFirstTokenMS = &median
		}
		if row.generationSeconds > 0 {
			throughput := float64(row.measuredOutputTokens) / row.generationSeconds
			row.OutputTokensPerSecond = &throughput
		}
		row.firstTokenMeasurements = nil
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Turns != out[j].Turns {
			return out[i].Turns > out[j].Turns
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}
