package service

import (
	"context"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/proxy"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

// The decisions table is capped and pruned, so past the cap every older turn is deleted along
// with any record of what it cost. The rollup is the only thing that outlives it, and this is
// the property that makes long-term usage answerable at all.
func TestRollupsSurviveHistoryPruning(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	ctx := context.Background()
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('a','u','a@e.com','plus','2026-09-12T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws','a','cg','WS',0,'r',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)

	at := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	const written = historyLimit + 50
	for i := 0; i < written; i++ {
		if err := svc.writeDecision(proxy.Record{
			At: at, ThreadID: "t", Model: "m",
			Decision: policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws", Summary: "s"},
			Attempt:  1, StatusCode: 200,
			Usage: &upstream.TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
		}); err != nil {
			t.Fatal(err)
		}
	}

	var rawRows int
	if err := svc.DB.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions`).Scan(&rawRows); err != nil {
		t.Fatal(err)
	}
	if rawRows > historyLimit {
		t.Fatalf("pruning did not run: %d raw rows", rawRows)
	}
	if rawRows == written {
		t.Fatal("nothing was pruned, so this test is not exercising the case it exists for")
	}

	buckets, err := svc.DB.Rollups(ctx, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var turns, tokens int64
	for _, b := range buckets {
		turns += b.Turns
		tokens += b.TotalTokens
	}
	// Every turn is counted, including the ones whose raw rows are gone.
	if turns != int64(written) {
		t.Fatalf("rollup counted %d turns, want all %d including the pruned ones", turns, written)
	}
	if tokens != int64(written)*12 {
		t.Fatalf("rollup totalled %d tokens, want %d", tokens, int64(written)*12)
	}
}

// Buckets are keyed by UTC hour, so a machine changing timezone or crossing daylight saving
// does not split one hour into two or merge two into one.
func TestRollupBucketsAreUTCHours(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	ctx := context.Background()
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('a','u','a@e.com','plus','2026-09-12T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws','a','cg','WS',0,'r',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)

	utc := time.Date(2026, 9, 13, 14, 30, 0, 0, time.UTC)
	// The same instant expressed in another zone must land in the same bucket.
	elsewhere := utc.In(time.FixedZone("UTC-7", -7*3600))
	for _, moment := range []time.Time{utc, elsewhere, utc.Add(20 * time.Minute)} {
		if err := svc.writeDecision(proxy.Record{
			At:       moment,
			Decision: policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws", Summary: "s"},
			Attempt:  1, StatusCode: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	buckets, err := svc.DB.Rollups(ctx, utc.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("three turns in one UTC hour produced %d buckets", len(buckets))
	}
	if buckets[0].Turns != 3 {
		t.Fatalf("bucket counted %d turns, want 3", buckets[0].Turns)
	}
}
