// Package service wires storage, policy, credentials and the proxy into one local process.
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/proxy"
	"github.com/lgoyal6/codex-relay/internal/routing"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/store"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

// historyLimit bounds the activity table. Retention is bounded by design; unbounded local
// history is a liability, not a feature.
const historyLimit = 2000

type Service struct {
	DB       *store.DB
	Registry *routing.Registry
	Creds    *CredentialManager
	Secrets  secrets.Store
	Clock    clock.Clock
	Log      *slog.Logger

	// audit serializes best-effort history writes off the turn path. A full queue drops
	// rows rather than slowing a stream: history is optional, routing is not.
	audit chan proxy.Record
	once  sync.Once
	stop  chan struct{}
}

func New(db *store.DB, reg *routing.Registry, creds *CredentialManager, sec secrets.Store, clk clock.Clock, log *slog.Logger) *Service {
	return &Service{
		DB: db, Registry: reg, Creds: creds, Secrets: sec, Clock: clk, Log: log,
		audit: make(chan proxy.Record, 256),
		stop:  make(chan struct{}),
	}
}

// Start launches the background audit writer.
func (s *Service) Start() {
	s.once.Do(func() {
		go func() {
			for {
				select {
				case rec := <-s.audit:
					if err := s.writeDecision(rec); err != nil {
						s.Log.Warn("could not record activity", "error", err)
					}
				case <-s.stop:
					return
				}
			}
		}()
	})
}

func (s *Service) Close() { close(s.stop) }

// --- proxy.Selector ---

func (s *Service) Decide(_ context.Context, req policy.Request) policy.Decision {
	return s.Registry.Evaluate(req)
}

func (s *Service) OwnerOf(ctx context.Context, threadID string) (string, bool, error) {
	return s.DB.OwnerOf(ctx, threadID)
}

func (s *Service) Claim(ctx context.Context, threadID, workspaceID string) error {
	_, err := s.DB.ClaimThread(ctx, threadID, workspaceID, s.Clock.Now())
	return err
}

// Identity resolves a workspace into a usable credential.
func (s *Service) Identity(ctx context.Context, workspaceID string) (proxy.Identity, error) {
	var ref, chatgptAccountID string
	err := s.DB.SQL().QueryRowContext(ctx,
		`SELECT credential_ref, chatgpt_account_id FROM workspaces WHERE id = ?`, workspaceID).
		Scan(&ref, &chatgptAccountID)
	if err != nil {
		return proxy.Identity{}, fmt.Errorf("workspace %s is not connected", workspaceID)
	}
	cred, err := s.Creds.Access(ctx, ref)
	if err != nil {
		s.markCredential(ctx, workspaceID, false, friendlyCredentialError(err))
		return proxy.Identity{}, err
	}
	s.markCredential(ctx, workspaceID, true, "")
	return proxy.Identity{
		WorkspaceID:      workspaceID,
		ChatGPTAccountID: chatgptAccountID,
		AccessToken:      cred.AccessToken,
	}, nil
}

func friendlyCredentialError(err error) string {
	return "This workspace needs to be reconnected: " + err.Error()
}

// markCredential records whether a workspace's credential is usable.
//
// It is called on every turn, from the path between the upstream response and the first byte
// sent to the client, so it must do nothing when nothing changed. Writing and republishing
// the routing snapshot on every turn was measurable and pointless: the value is the same
// every time until a credential actually breaks.
func (s *Service) markCredential(ctx context.Context, workspaceID string, ok bool, note string) {
	v := 0
	if ok {
		v = 1
	}
	var curOK int
	var curNote string
	if err := s.DB.SQL().QueryRowContext(ctx,
		`SELECT credential_ok, credential_note FROM workspaces WHERE id = ?`, workspaceID).
		Scan(&curOK, &curNote); err == nil && curOK == v && curNote == note {
		return // unchanged; no write, no republish
	}
	if _, err := s.DB.SQL().ExecContext(ctx,
		`UPDATE workspaces SET credential_ok = ?, credential_note = ?, updated_at = ? WHERE id = ?`,
		v, note, s.Clock.Now().Format(time.RFC3339Nano), workspaceID); err != nil {
		s.Log.Warn("could not record credential state", "error", err)
		return
	}
	go func() {
		if err := s.Refresh(context.Background()); err != nil {
			s.Log.Warn("could not refresh routing snapshot", "error", err)
		}
	}()
}

// ObserveQuota records rate-limit evidence from a real response and republishes the snapshot
// so a time or quota change is reflected without waiting for a poll.
func (s *Service) ObserveQuota(ctx context.Context, workspaceID string, snaps []upstream.Snapshot) {
	now := s.Clock.Now()
	for _, snap := range snaps {
		for _, w := range []*upstream.Window{snap.Primary, snap.Secondary} {
			if w == nil || w.Minutes <= 0 {
				continue
			}
			var resets any
			if w.ResetsAt != nil {
				resets = w.ResetsAt.Unix()
			}
			_, err := s.DB.SQL().ExecContext(ctx, `
				INSERT INTO quota_windows (workspace_id, limit_id, window_minutes, used_percent, resets_at, observed_at, source)
				VALUES (?, ?, ?, ?, ?, ?, 'response_headers')
				ON CONFLICT(workspace_id, limit_id, window_minutes) DO UPDATE SET
					used_percent = excluded.used_percent,
					resets_at = excluded.resets_at,
					observed_at = excluded.observed_at,
					source = excluded.source`,
				workspaceID, snap.LimitID, w.Minutes, w.UsedPercent, resets, now.Format(time.RFC3339Nano))
			if err != nil {
				s.Log.Warn("could not store quota reading", "error", err)
			}
		}
	}
	if err := s.Refresh(ctx); err != nil {
		s.Log.Warn("could not refresh routing snapshot", "error", err)
	}
}

// ObserveModels records the model catalog one workspace reported.
//
// The set is REPLACED, not merged: a model that has disappeared from a workspace's catalog
// has genuinely stopped being eligible there, and keeping a stale entry would let the
// evaluator admit a request the workspace can no longer serve. An empty list is ignored, so
// a failed or unparsable catalog read leaves eligibility unknown rather than empty.
func (s *Service) ObserveModels(ctx context.Context, workspaceID string, slugs []string) {
	if len(slugs) == 0 {
		return
	}
	now := s.Clock.Now().Format(time.RFC3339Nano)
	tx, err := s.DB.SQL().BeginTx(ctx, nil)
	if err != nil {
		s.Log.Warn("could not record model eligibility", "error", err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM model_eligibility WHERE workspace_id = ?`, workspaceID); err != nil {
		s.Log.Warn("could not clear model eligibility", "error", err)
		return
	}
	for _, slug := range slugs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_eligibility (workspace_id, model_slug, observed_at) VALUES (?,?,?)`,
			workspaceID, slug, now); err != nil {
			s.Log.Warn("could not record model eligibility", "error", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.Log.Warn("could not record model eligibility", "error", err)
		return
	}
	if err := s.Refresh(ctx); err != nil {
		s.Log.Warn("could not refresh routing snapshot", "error", err)
	}
}

// RecordDecision enqueues an activity row. It never blocks the turn.
func (s *Service) RecordDecision(rec proxy.Record) {
	select {
	case s.audit <- rec:
	default:
		// The queue is full. Dropping an audit row is the correct trade: streamed output
		// must not wait on history.
		s.Log.Warn("activity queue full; dropped one row")
	}
}

func (s *Service) writeDecision(rec proxy.Record) error {
	detail, err := json.Marshal(struct {
		Notes      []policy.Note      `json:"notes"`
		Candidates []policy.Candidate `json:"candidates"`
		Unknown    bool               `json:"unknown_evidence"`
	}{rec.Decision.Notes, rec.Decision.Candidates, rec.Decision.UnknownEvidence})
	if err != nil {
		return err
	}
	var inTok, cachedTok, outTok, totTok any
	if u := rec.Usage; u != nil {
		inTok, cachedTok, outTok, totTok = u.InputTokens, u.CachedInputTokens, u.OutputTokens, u.TotalTokens
	}
	_, err = s.DB.SQL().Exec(`
		INSERT INTO decisions (at, thread_id, model, outcome, workspace_id, primary_reason, summary,
			detail_json, state_version, attempt, status_code, first_token_ms, total_ms, error_class,
			input_tokens, cached_input_tokens, output_tokens, total_tokens)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.At.Format(time.RFC3339Nano), nullIfEmpty(rec.ThreadID), nullIfEmpty(rec.Model),
		string(rec.Decision.Outcome), nullIfEmpty(rec.Decision.WorkspaceID),
		string(rec.Decision.Primary), rec.Decision.Summary, string(detail),
		rec.Decision.StateVersion, rec.Attempt, nullIfZero(rec.StatusCode),
		nullIfUnmeasured(rec.FirstTokenMS), nullIfUnmeasured(rec.TotalMS), rec.ErrorClass,
		inTok, cachedTok, outTok, totTok)
	if err != nil {
		return err
	}
	_, err = s.DB.SQL().Exec(
		`DELETE FROM decisions WHERE id NOT IN (SELECT id FROM decisions ORDER BY id DESC LIMIT ?)`, historyLimit)
	return err
}

// Refresh rebuilds the routing snapshot from the database and publishes it atomically.
func (s *Service) Refresh(ctx context.Context) error {
	st := &policy.State{
		Workspaces: map[string]*policy.WorkspaceState{},
		StaleAfter: 10 * time.Minute,
	}

	rows, err := s.DB.SQL().QueryContext(ctx, `
		SELECT w.id, w.display_name, w.account_id, w.paused, w.credential_ok, w.credential_note
		FROM workspaces w ORDER BY w.sort_order, w.id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, name, account, note string
		var paused, credOK int
		if err := rows.Scan(&id, &name, &account, &paused, &credOK, &note); err != nil {
			rows.Close()
			return err
		}
		st.Workspaces[id] = &policy.WorkspaceState{
			ID: id, Name: name, AccountID: account,
			Paused: paused == 1, CredentialOK: credOK == 1, CredentialNote: note,
			Windows: map[int64]policy.Window{},
		}
		st.Order = append(st.Order, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	qr, err := s.DB.SQL().QueryContext(ctx,
		`SELECT workspace_id, window_minutes, used_percent, resets_at, observed_at FROM quota_windows WHERE limit_id = 'codex'`)
	if err != nil {
		return err
	}
	for qr.Next() {
		var ws string
		var minutes int64
		var used float64
		var resets sql.NullInt64
		var observed string
		if err := qr.Scan(&ws, &minutes, &used, &resets, &observed); err != nil {
			qr.Close()
			return err
		}
		w := policy.Window{Minutes: minutes, UsedPercent: used}
		w.ObservedAt, _ = time.Parse(time.RFC3339Nano, observed)
		if resets.Valid {
			t := time.Unix(resets.Int64, 0).UTC()
			w.ResetsAt = &t
		}
		if target := st.Workspaces[ws]; target != nil {
			target.Windows[minutes] = w
		}
	}
	qr.Close()

	mr, err := s.DB.SQL().QueryContext(ctx, `SELECT workspace_id, model_slug, observed_at FROM model_eligibility`)
	if err != nil {
		return err
	}
	for mr.Next() {
		var ws, slug, observed string
		if err := mr.Scan(&ws, &slug, &observed); err != nil {
			mr.Close()
			return err
		}
		if target := st.Workspaces[ws]; target != nil {
			if target.EligibleModels == nil {
				target.EligibleModels = map[string]bool{}
			}
			target.EligibleModels[slug] = true
			if t, err := time.Parse(time.RFC3339Nano, observed); err == nil {
				target.ModelsObservedAt = &t
			}
		}
	}
	mr.Close()

	st.Rules, err = s.LoadRules(ctx)
	if err != nil {
		return err
	}
	st.DefaultWorkspaceID, _ = s.GetSetting(ctx, "default_workspace_id")

	s.Registry.Publish(st)
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullIfZero(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

// nullIfUnmeasured stores NULL only for a value that was never measured, which callers
// signal with a negative number. A measured zero is a real reading and is stored as zero.
func nullIfUnmeasured(i int64) any {
	if i < 0 {
		return nil
	}
	return i
}

var _ proxy.Selector = (*Service)(nil)
