package httpapi

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestActivityCSVExportsEveryRetainedRowAndQuotesFields(t *testing.T) {
	a, h := testAPI(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := a.Svc.DB.SQL().ExecContext(ctx, `INSERT INTO accounts
		(id, chatgpt_user_id, email, plan_type, created_at) VALUES ('acct','user','person@example.com','free',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Svc.DB.SQL().ExecContext(ctx, `INSERT INTO workspaces
		(id, account_id, chatgpt_account_id, display_name, credential_ref, created_at, updated_at)
		VALUES ('ws','acct','chatgpt','Workspace, One','credential-secret-sentinel',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	tx, err := a.Svc.DB.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 501; i++ {
		summary := "ordinary"
		if i == 0 {
			summary = "hello, \"world\"\nnext"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO decisions
			(at, thread_id, model, outcome, workspace_id, primary_reason, summary, detail_json,
			 state_version, attempt, status_code, quota_snapshot_json)
			VALUES (?, ?, 'gpt-test', 'selected', 'ws', 'default_workspace', ?, '{}', 1, 1, 200, ?)`,
			now, "thread-full-id", summary,
			`{"captured_at":"2026-09-20T12:00:00Z","workspace_id":"ws","windows":[]}`); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/activity.csv", nil)
	r.Host = "127.0.0.1:7788"
	r.Header.Set("X-Codex-Pool-Token", a.Guard.Token())
	rec := serve(t, h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="codexrelay-activity.csv"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("CSV is not parseable: %v", err)
	}
	if len(records) != 502 {
		t.Fatalf("CSV rows = %d, want header + all 501 retained rows", len(records))
	}
	header := records[0]
	for _, want := range []string{"time", "thread_id", "request_kind", "workspace_name", "account_email", "plan", "quota_snapshot_json", "output_tokens_per_second"} {
		if !containsCSVField(header, want) {
			t.Errorf("CSV header is missing %q: %v", want, header)
		}
	}
	if !strings.Contains(rec.Body.String(), "hello, \"\"world\"\"") {
		t.Fatal("quoted summary was not encoded as CSV")
	}
	for _, forbidden := range []string{"credential-secret-sentinel", "prompt-secret-sentinel"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("CSV exposed %q", forbidden)
		}
	}
}

func TestActivityCSVMarksMissingHistoricalQuota(t *testing.T) {
	a, h := testAPI(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.Svc.DB.SQL().ExecContext(t.Context(), `INSERT INTO decisions
		(at, outcome, primary_reason, summary, detail_json, state_version, attempt)
		VALUES (?, 'blocked', 'no_eligible_workspace', 'blocked', '{}', 1, 1)`, now); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/activity.csv", nil)
	r.Host = "127.0.0.1:7788"
	r.Header.Set("X-Codex-Pool-Token", a.Guard.Token())
	rec := serve(t, h, r)
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("CSV rows = %d, body = %s", len(records), rec.Body.String())
	}
	idx := -1
	for i, field := range records[0] {
		if field == "quota_snapshot_json" {
			idx = i
		}
	}
	if idx < 0 || records[1][idx] != "not recorded" {
		t.Fatalf("quota snapshot field = %q", records[1][idx])
	}
}

func TestModelsEndpointReturnsReport(t *testing.T) {
	a, h := testAPI(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.Svc.DB.SQL().ExecContext(t.Context(), `INSERT INTO decisions
		(at, model, outcome, primary_reason, summary, detail_json, state_version, attempt, first_token_ms, total_ms, output_tokens, total_tokens)
		VALUES (?, 'gpt-test', 'selected', 'default_workspace', 'served', '{}', 1, 1, 100, 1100, 5, 8)`, now); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	r.Host = "127.0.0.1:7788"
	r.Header.Set("X-Codex-Pool-Token", a.Guard.Token())
	rec := serve(t, h, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"gpt-test"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func containsCSVField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}
