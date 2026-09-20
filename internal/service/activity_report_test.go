package service

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/proxy"
	"github.com/lgoyal6/codex-relay/internal/routing"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/store"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

func TestRecordDecisionCapturesQuotaBeforeAsyncWrite(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	sec := secrets.NewMemory()
	svc := New(db, routing.NewRegistry(clk), NewCredentialManager(sec, clk, ""), sec, clk,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('acct','u','person@example.com','plus','2026-09-20T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws','acct','cg','Personal',0,'ref',1,0,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`)
	if _, err := svc.DB.SQL().ExecContext(ctx, `INSERT INTO quota_windows (workspace_id, limit_id, window_minutes, used_percent, resets_at, observed_at, source)
		VALUES ('ws','codex',300,20,?,?,'usage_endpoint')`, now.Add(4*time.Hour).Unix(), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DB.SQL().ExecContext(ctx, `INSERT INTO quota_windows (workspace_id, limit_id, window_minutes, used_percent, resets_at, observed_at, source)
		VALUES ('ws','codex',10080,70,?,?,'usage_endpoint')`, now.Add(-time.Minute).Unix(), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	// The service is deliberately not started. RecordDecision must attach the snapshot before
	// enqueueing, while Close drains the queued row only after live state changes below.
	svc.RecordDecision(proxy.Record{
		At: now, Model: "gpt-test", Attempt: 1, StatusCode: 200,
		Decision: policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws", Summary: "served"},
	})
	if _, err := svc.DB.SQL().ExecContext(ctx, `UPDATE quota_windows SET used_percent=90, observed_at=? WHERE workspace_id='ws'`,
		now.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	svc.Close()

	rows, err := svc.Activity(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].QuotaSnapshot == nil {
		t.Fatalf("quota snapshot = %#v", rows)
	}
	snap := rows[0].QuotaSnapshot
	if len(snap.Windows) != 1 || snap.Windows[0].RemainingPercent != 80 {
		t.Fatalf("snapshot windows = %+v, want the 80%% remaining value from enqueue time and no expired window", snap.Windows)
	}
	if snap.Windows[0].Label != "5-hour" || snap.Windows[0].Evidence != policy.EvidenceFresh {
		t.Fatalf("snapshot metadata = %+v", snap.Windows[0])
	}
}

func TestActivityLeavesOldRowsWithoutQuotaSnapshot(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	if err := svc.writeDecision(proxy.Record{
		At: time.Now().UTC(), Attempt: 1,
		Decision: policy.Decision{Outcome: policy.OutcomeBlocked, Summary: "blocked"},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Activity(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].QuotaSnapshot != nil {
		t.Fatalf("old-style row quota snapshot = %#v, want nil", rows)
	}
}

func TestActivityAllowsTheFullRetainedHistoryLimit(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	ctx := context.Background()
	tx, err := svc.DB.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 501; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO decisions
			(at, outcome, primary_reason, summary, detail_json, state_version, attempt)
			VALUES (?, 'blocked', 'no_eligible_workspace', 'blocked', '{}', 1, 1)`,
			time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Activity(ctx, historyLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 501 {
		t.Fatalf("Activity(%d) returned %d rows, want all 501", historyLimit, len(rows))
	}
}

func TestModelReportUsesOnlyCompletedServedTurns(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	write := func(rec proxy.Record) {
		t.Helper()
		if err := svc.writeDecision(rec); err != nil {
			t.Fatal(err)
		}
	}
	write(proxy.Record{
		At: at, Model: "model-a", Attempt: 1, StatusCode: 200, FirstTokenMS: 100, TotalMS: 2100,
		Decision: policy.Decision{Outcome: policy.OutcomeSelected, Summary: "served"},
		Usage:    &upstream.TokenUsage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 20, TotalTokens: 30},
	})
	write(proxy.Record{
		At: at, Model: "model-a", Attempt: 1, StatusCode: 200, FirstTokenMS: 300, TotalMS: 300,
		Decision: policy.Decision{Outcome: policy.OutcomeSelected, Summary: "served"},
	})
	write(proxy.Record{
		At: at, Model: "model-a", Attempt: 1, StatusCode: 500, FirstTokenMS: 999, TotalMS: 9999,
		Decision: policy.Decision{Outcome: policy.OutcomeSelected, Summary: "failed"}, ErrorClass: "upstream",
		Usage: &upstream.TokenUsage{OutputTokens: 999, TotalTokens: 999},
	})
	write(proxy.Record{
		At: at, Model: "model-b", Attempt: 1, StatusCode: 200, FirstTokenMS: 50, TotalMS: 1050,
		Decision: policy.Decision{Outcome: policy.OutcomeSelected, Summary: "served"},
		Usage:    &upstream.TokenUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
	})

	report, err := svc.ModelReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 2 || report[0].Model != "model-a" || report[0].Turns != 2 {
		t.Fatalf("report ordering/counts = %+v", report)
	}
	a := report[0]
	if a.TurnsWithUsage != 1 || a.TotalTokens != 30 || a.OutputTokens != 20 {
		t.Fatalf("model-a usage = %+v; failed turn must be excluded", a)
	}
	if a.MedianFirstTokenMS == nil || *a.MedianFirstTokenMS != 200 {
		t.Fatalf("model-a median = %v, want 200", a.MedianFirstTokenMS)
	}
	if a.OutputTokensPerSecond == nil || *a.OutputTokensPerSecond != 10 {
		t.Fatalf("model-a throughput = %v, want 10", a.OutputTokensPerSecond)
	}
}
