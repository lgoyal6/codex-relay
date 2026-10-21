package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestStateIncludesEachAccountPlan(t *testing.T) {
	a, _ := testAPI(t)
	ctx := context.Background()
	for _, plan := range []string{"prolite", "plus", "free", ""} {
		id := "account-" + plan
		if _, err := a.Svc.DB.SQL().ExecContext(ctx,
			`INSERT INTO accounts (id,chatgpt_user_id,plan_type,created_at) VALUES (?,?,?,?)`,
			id, id, plan, "2026-10-19T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Svc.DB.SQL().ExecContext(ctx,
			`INSERT INTO workspaces (id,account_id,chatgpt_account_id,display_name,credential_ref,credential_ok,created_at,updated_at)
			 VALUES (?,?,?,?,?,1,?,?)`, id, id, id, id, id, "2026-10-19T00:00:00Z", "2026-10-19T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	var result struct {
		Workspaces []struct {
			AccountID string `json:"account_id"`
			PlanType  string `json:"plan_type"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Workspaces) != 4 {
		t.Fatalf("workspaces = %d, want 4", len(result.Workspaces))
	}
	for _, ws := range result.Workspaces {
		if ws.AccountID != "account-"+ws.PlanType {
			t.Errorf("account %q lost its plan: %q", ws.AccountID, ws.PlanType)
		}
	}
}
