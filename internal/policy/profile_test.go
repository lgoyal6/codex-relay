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

func TestSubagentHelperOverridesParentOrderOnlyForDelegatedLuna(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 80, 80)
	st.ActiveProfile = &RoutingProfile{
		ID: "helpers", Name: "Hers first", Command: "hers", Mode: ProfilePriority,
		PriorityWorkspaceIDs: []string{"hers", "mine", "free"}, HandoffBelowPercent: 2,
		SubagentHelperEnabled: true, SubagentHelperWorkspaceID: "free", SubagentHelperModel: "gpt-5.6-luna",
	}
	parent := Evaluate(st, Request{Model: "gpt-5.6-luna"}, now)
	if parent.WorkspaceID != "hers" || parent.Primary != ReasonProfilePreferred {
		t.Fatalf("ordinary Luna parent selected %q/%s", parent.WorkspaceID, parent.Primary)
	}
	helper := Evaluate(st, Request{Model: "gpt-5.6-luna", IsSubagent: true}, now)
	if helper.WorkspaceID != "free" || helper.Primary != ReasonSubagentHelper {
		t.Fatalf("Luna subagent selected %q/%s: %s", helper.WorkspaceID, helper.Primary, helper.Summary)
	}
	otherModel := Evaluate(st, Request{Model: "gpt-5.6-sol", IsSubagent: true}, now)
	if otherModel.WorkspaceID != "hers" {
		t.Fatalf("non-Luna subagent selected %q", otherModel.WorkspaceID)
	}
}

func TestSubagentHelperFallsBackWhenUnavailable(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := profileState(now, 80, 80)
	st.Workspaces["free"].Paused = true
	st.ActiveProfile = &RoutingProfile{
		ID: "helpers", Name: "Hers first", Command: "hers", Mode: ProfilePriority,
		PriorityWorkspaceIDs: []string{"hers", "mine", "free"}, HandoffBelowPercent: 2,
		SubagentHelperEnabled: true, SubagentHelperWorkspaceID: "free", SubagentHelperModel: "gpt-5.6-luna",
	}
	d := Evaluate(st, Request{Model: "gpt-5.6-luna", IsSubagent: true}, now)
	if d.WorkspaceID != "hers" || d.Outcome != OutcomeSelected {
		t.Fatalf("helper fallback selected %q/%s: %s", d.WorkspaceID, d.Outcome, d.Summary)
	}
	foundFallback := false
	for _, note := range d.Notes {
		if note.Code == ReasonSubagentHelper && note.WorkspaceID == "free" {
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Fatal("fallback explanation was not recorded")
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
