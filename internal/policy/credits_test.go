package policy

import (
	"testing"
	"time"
)

// An account whose plan window is empty but which has purchased credits keeps serving.
func TestCreditsKeepAnEmptyWorkspaceServing(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 0, 80)
	st.ActiveProfile = &RoutingProfile{
		ID: "p", Name: "Mine first", Command: "mine", Mode: ProfilePriority,
		PriorityWorkspaceIDs: []string{"mine", "hers", "free"}, HandoffBelowPercent: 2,
	}
	if d := Evaluate(st, Request{}, now); d.WorkspaceID == "mine" {
		t.Fatalf("empty workspace without credits was selected")
	}
	st.Workspaces["mine"].HasCredits = true
	if d := Evaluate(st, Request{}, now); d.WorkspaceID != "mine" {
		t.Fatalf("empty workspace with credits not selected: %q %s", d.WorkspaceID, d.Summary)
	}
	if d := Evaluate(st, Request{OwnerWorkspaceID: "mine"}, now); d.WorkspaceID != "mine" {
		t.Fatalf("owned conversation handed off despite credits: %q", d.WorkspaceID)
	}
}
