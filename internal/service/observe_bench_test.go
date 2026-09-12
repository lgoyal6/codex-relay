package service

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/routing"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/store"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

func benchService(tb testing.TB) *Service {
	tb.Helper()
	db, err := store.Open(tb.TempDir() + "/b.db")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO accounts (id, chatgpt_user_id, created_at) VALUES ('a','u1',?)`, now); err != nil {
		tb.Fatal(err)
	}
	for _, w := range []string{"personal", "work"} {
		if _, err := db.SQL().ExecContext(ctx, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, credential_ref, credential_ok, created_at, updated_at) VALUES (?,'a',?,?,?,1,?,?)`,
			w, "acct_"+w, w, "ref_"+w, now, now); err != nil {
			tb.Fatal(err)
		}
	}
	clk := clock.System()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(db, routing.NewRegistry(clk), NewCredentialManager(secrets.NewMemory(), clk, ""), secrets.NewMemory(), clk, log)
	svc.Start()
	tb.Cleanup(svc.Close)
	if err := svc.Refresh(ctx); err != nil {
		tb.Fatal(err)
	}
	return svc
}

func snaps() []upstream.Snapshot {
	reset := time.Now().Add(time.Hour)
	return []upstream.Snapshot{{
		LimitID:   "codex",
		Primary:   &upstream.Window{Minutes: 300, UsedPercent: 24.5, ResetsAt: &reset},
		Secondary: &upstream.Window{Minutes: 10080, UsedPercent: 72, ResetsAt: &reset},
	}}
}

// BenchmarkObserveQuota measures what quota ingestion costs. It matters because this call
// currently sits between receiving the upstream response headers and writing the first byte
// to the client, so whatever it costs is added to every turn's first-token latency.
func BenchmarkObserveQuota(b *testing.B) {
	svc := benchService(b)
	ctx := context.Background()
	s := snaps()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.ObserveQuota(ctx, "personal", s)
	}
}

func BenchmarkRefreshOnly(b *testing.B) {
	svc := benchService(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = svc.Refresh(ctx)
	}
}
