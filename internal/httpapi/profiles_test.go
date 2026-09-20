package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

func TestProfileAPIUsesOneSavedProfileForActivationAndState(t *testing.T) {
	a, handler := testAPI(t)
	ctx := context.Background()
	if _, err := a.Svc.DB.SQL().ExecContext(ctx,
		`INSERT INTO accounts (id, chatgpt_user_id, created_at) VALUES ('a','u','2026-09-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Svc.DB.SQL().ExecContext(ctx, `INSERT INTO workspaces
		(id, account_id, chatgpt_account_id, display_name, credential_ref, credential_ok, created_at, updated_at)
		VALUES
		('mine','a','cg-mine','Mine','ref-mine',1,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z'),
		('free','a','cg-free','Free','ref-free',1,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := a.Svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{
		"name":"Free first","command":"free","aliases":["his"],"mode":"priority",
		"priority_workspace_ids":["free"],"pace_workspace_ids":[],
		"target_remaining_percent":3,"default_workspace_id":"free",
		"handoff_below_percent":2,"disabled_workspace_ids":[]
	}`)
	post := httptest.NewRequest(http.MethodPost, "/api/profiles", bytes.NewReader(body))
	post.Host = "127.0.0.1:7788"
	post.Header.Set("X-Codex-Pool-Token", a.Guard.Token())
	post.Header.Set("Content-Type", "application/json")
	if rec := serve(t, handler, post); rec.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", rec.Code, rec.Body.String())
	}

	activate := httptest.NewRequest(http.MethodPost, "/api/profiles/his/activate", nil)
	activate.Host = "127.0.0.1:7788"
	activate.Header.Set("X-Codex-Pool-Token", a.Guard.Token())
	if rec := serve(t, handler, activate); rec.Code != http.StatusOK {
		t.Fatalf("activate returned %d: %s", rec.Code, rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	get.Host = "127.0.0.1:7788"
	get.Header.Set("X-Codex-Pool-Token", a.Guard.Token())
	rec := serve(t, handler, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("state returned %d: %s", rec.Code, rec.Body.String())
	}
	var state StateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Profiles) != 1 || !state.Profiles[0].Active {
		t.Fatalf("profiles = %+v", state.Profiles)
	}
	if state.Proposed.WorkspaceID != "free" {
		t.Fatalf("active profile selected %q", state.Proposed.WorkspaceID)
	}
	decision := a.Svc.Decide(ctx, policy.Request{OwnerWorkspaceID: "mine", ThreadID: "existing-chat"})
	if decision.WorkspaceID != "free" || decision.Primary != policy.ReasonHandoff {
		t.Fatalf("existing chat did not switch between turns: %+v", decision)
	}
}
