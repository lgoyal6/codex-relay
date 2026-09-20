package policy

import (
	"testing"
	"time"
)

func profileState(now time.Time, mineRemaining, hersRemaining float64) *State {
	reset := now.Add(6 * 24 * time.Hour)
	window := func(remaining float64) map[int64]Window {
		return map[int64]Window{
			WeeklyWindowMinutes: {
				Minutes: WeeklyWindowMinutes, UsedPercent: 100 - remaining,
				ResetsAt: &reset, ObservedAt: now,
			},
		}
	}
	return &State{
		Version: 1, StaleAfter: 10 * time.Minute, HandoffBelowPercent: 2,
		Order: []string{"mine", "hers", "free"},
		Workspaces: map[string]*WorkspaceState{
			"mine": {ID: "mine", Name: "Mine", CredentialOK: true, Windows: window(mineRemaining)},
			"hers": {ID: "hers", Name: "Hers", CredentialOK: true, Windows: window(hersRemaining)},
			"free": {ID: "free", Name: "Free", CredentialOK: true, Windows: map[int64]Window{}},
		},
		ActiveProfile: &RoutingProfile{
			ID: "pace", Name: "Weekly pace", Command: "pace", Mode: ProfilePace,
			PaceWorkspaceIDs: []string{"mine", "hers"}, OverflowWorkspaceID: "free",
			PriorityWorkspaceIDs:   []string{"mine", "hers", "free"},
			TargetRemainingPercent: 3, HandoffBelowPercent: 2,
		},
	}
}

func TestPaceUsesThePaidAccountFurthestAboveItsTargetCurve(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 98, 90)
	d := Evaluate(st, Request{}, now)
	if d.WorkspaceID != "mine" || d.Primary != ReasonWeeklyPace {
		t.Fatalf("pace selected %q/%s: %s", d.WorkspaceID, d.Primary, d.Summary)
	}
	if d.Candidates[0].Rank != 1 {
		t.Fatalf("mine rank = %d, want 1", d.Candidates[0].Rank)
	}
}

func TestPaceUsesOverflowWhenPaidAccountsAreOnSchedule(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 42, 53)
	d := Evaluate(st, Request{}, now)
	if d.WorkspaceID != "free" || d.Primary != ReasonPaceOverflow {
		t.Fatalf("pace selected %q/%s: %s", d.WorkspaceID, d.Primary, d.Summary)
	}
}

func TestPaceMovesAnExistingConversationBetweenTurns(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 42, 53)
	d := Evaluate(st, Request{OwnerWorkspaceID: "mine"}, now)
	if d.WorkspaceID != "free" || d.Primary != ReasonHandoff {
		t.Fatalf("existing conversation selected %q/%s: %s", d.WorkspaceID, d.Primary, d.Summary)
	}
}

func TestProfileCannotOverrideEligibility(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 42, 53)
	st.ActiveProfile.DisabledWorkspaceIDs = []string{"free"}
	d := Evaluate(st, Request{}, now)
	if d.WorkspaceID == "free" {
		t.Fatal("profile selected a workspace it also disabled")
	}
	for _, candidate := range d.Candidates {
		if candidate.WorkspaceID == "free" && candidate.Reason != ReasonProfileDisabled {
			t.Fatalf("disabled free reason = %s", candidate.Reason)
		}
	}
}

func TestPriorityProfileUsesCustomOrder(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 42, 53)
	st.ActiveProfile = &RoutingProfile{
		ID: "custom", Name: "Custom", Command: "weekend", Mode: ProfilePriority,
		PriorityWorkspaceIDs: []string{"hers", "free", "mine"}, HandoffBelowPercent: 2,
	}
	d := Evaluate(st, Request{}, now)
	if d.WorkspaceID != "hers" || d.Primary != ReasonProfilePreferred {
		t.Fatalf("priority profile selected %q/%s", d.WorkspaceID, d.Primary)
	}
}

func TestProfileValidationRejectsAliasCollisionsAndUnknownWorkspaces(t *testing.T) {
	known := map[string]bool{"mine": true, "hers": true}
	p := RoutingProfile{
		Name: "Mine", Command: "mine", Aliases: []string{"mine"}, Mode: ProfilePriority,
		PriorityWorkspaceIDs: []string{"mine"},
	}
	p.Normalize()
	if err := p.Validate(known); err == nil {
		t.Fatal("alias matching the command was accepted")
	}
	p.Aliases = nil
	p.PriorityWorkspaceIDs = []string{"missing"}
	if err := p.Validate(known); err == nil {
		t.Fatal("unknown workspace was accepted")
	}
}
