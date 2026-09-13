package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

// A paused workspace must still be polled. Pausing means "do not route here", and a paused
// workspace showing a quota figure from hours ago is the exact defect this poller exists to
// remove: the reading is what a user reads when deciding whether to unpause.
func TestPollUsageRefreshesPausedWorkspaces(t *testing.T) {
	raw, err := os.ReadFile("../upstream/testdata/usage_real.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotAuth, gotAccount string
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer up.Close()

	sec := secrets.NewMemory()
	svc := connectService(t, sec)
	svc.UpstreamBase = up.URL
	ctx := context.Background()

	if err := sec.Set("ws-ref", secrets.Credential{AccessToken: "tok-abc"}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('acct1','user-1','a@example.com','plus','2026-09-12T00:00:00Z')`)
	_, err = svc.DB.SQL().ExecContext(ctx, `
		INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws1','acct1','cg-acct-1','Paused One',1,'ws-ref',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	svc.PollUsage(ctx)

	if hits != 1 {
		t.Fatalf("a paused workspace must still be polled: %d requests", hits)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccount != "cg-acct-1" {
		t.Errorf("ChatGPT-Account-ID = %q, the poll must be scoped to the workspace", gotAccount)
	}

	var pct float64
	var source string
	err = svc.DB.SQL().QueryRowContext(ctx,
		`SELECT used_percent, source FROM quota_windows WHERE workspace_id='ws1' AND window_minutes=300`).
		Scan(&pct, &source)
	if err != nil {
		t.Fatalf("no reading stored: %v", err)
	}
	if pct != 68 {
		t.Errorf("used_percent = %v, want 68", pct)
	}
	// The source has to be distinguishable: a poll reports a running total, a turn reports
	// what that turn cost, and the dashboard says which one it is showing.
	if source != "usage_endpoint" {
		t.Errorf("source = %q, want usage_endpoint", source)
	}
}

// One broken workspace must not stop the rest. A single expired credential would otherwise
// freeze quota for every workspace in the pool.
func TestPollUsageContinuesPastAFailingWorkspace(t *testing.T) {
	raw, _ := os.ReadFile("../upstream/testdata/usage_real.json")
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(raw)
	}))
	defer up.Close()

	sec := secrets.NewMemory()
	svc := connectService(t, sec)
	svc.UpstreamBase = up.URL
	ctx := context.Background()
	_ = sec.Set("good-ref", secrets.Credential{AccessToken: "tok"})

	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('a','user-a','a@example.com','plus','2026-09-12T00:00:00Z'),
		       ('b','user-b','b@example.com','plus','2026-09-12T00:00:00Z')`)
	// 'broken' has no stored credential, so resolving its identity fails.
	_, err := svc.DB.SQL().ExecContext(ctx, `
		INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('broken','a','cg-a','Broken',0,'missing-ref',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z'),
		       ('good','b','cg-b','Good',0,'good-ref',1,1,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	svc.PollUsage(ctx)

	if hits != 1 {
		t.Fatalf("the healthy workspace should still have been polled, got %d requests", hits)
	}
	var n int
	_ = svc.DB.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM quota_windows WHERE workspace_id='good'`).Scan(&n)
	if n == 0 {
		t.Error("the healthy workspace recorded no reading")
	}
}

func mustExec(t *testing.T, svc *Service, q string) {
	t.Helper()
	if _, err := svc.DB.SQL().Exec(q); err != nil {
		t.Fatal(err)
	}
}

// Plan-specific limit families must not produce two rows for one window.
//
// A prolite account reports its family as "codex_bengalfox" in response headers, while the
// usage endpoint calls the same limit "codex". The DB keys on (workspace, limit_id, minutes)
// but the evaluator keys on minutes alone, so both rows survived and which one the evaluator
// saw depended on row order. Observed live on a real account.
func TestOneRowPerWindowAcrossLimitFamilies(t *testing.T) {
	sec := secrets.NewMemory()
	svc := connectService(t, sec)
	ctx := context.Background()
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('a','u','a@example.com','prolite','2026-09-12T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws','a','cg','WS',0,'r',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)

	win := func(pct float64) *upstream.Window {
		return &upstream.Window{Minutes: 300, UsedPercent: pct}
	}
	// Headers first, under the plan's own family name.
	svc.observeQuota(ctx, "ws", []upstream.Snapshot{{LimitID: "codex_bengalfox", Primary: win(10)}}, "response_headers")
	// Then the usage endpoint, which calls the same limit something else.
	svc.observeQuota(ctx, "ws", []upstream.Snapshot{{LimitID: "codex", Primary: win(42)}}, "usage_endpoint")

	rows, err := svc.DB.SQL().QueryContext(ctx,
		`SELECT limit_id, used_percent FROM quota_windows WHERE workspace_id='ws' AND window_minutes=300`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	var pct float64
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &pct); err == nil {
			got = append(got, id)
		}
	}
	if len(got) != 1 {
		t.Fatalf("the 5-hour window has %d rows (%v), want exactly 1", len(got), got)
	}
	if pct != 42 {
		t.Errorf("used_percent = %v, want the newest reading 42", pct)
	}
}
