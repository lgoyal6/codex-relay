package policy

import (
	"testing"
	"time"
)

// handoffState: two healthy workspaces, no rules, with Personal's 5-hour window at the given
// used percentage. Handoff is on unless the threshold is zero.
func handoffState(personalUsed float64, threshold float64) *State {
	reset := base.Add(2 * time.Hour)
	mk := func(id, name string, used float64) *WorkspaceState {
		return &WorkspaceState{
			ID: id, Name: name, AccountID: "acct_" + id, CredentialOK: true,
			Windows: map[int64]Window{
				300: {Minutes: 300, UsedPercent: used, ObservedAt: base, ResetsAt: &reset},
			},
		}
	}
	return &State{
		Version: 1, Order: []string{"personal", "work"}, DefaultWorkspaceID: "personal",
		StaleAfter: 10 * time.Minute, HandoffBelowPercent: threshold,
		Workspaces: map[string]*WorkspaceState{
			"personal": mk("personal", "Personal", personalUsed),
			"work":     mk("work", "Acme Work", 10),
		},
	}
}

// The headline behaviour: an owner down to its last 2% hands the conversation over instead of
// waiting for the turn that would fail on it.
func TestNearlyEmptyOwnerHandsOff(t *testing.T) {
	st := handoffState(99, 2) // 1% remaining, threshold 2%
	d := Evaluate(st, Request{OwnerWorkspaceID: "personal"}, base)

	if d.Outcome != OutcomeSelected {
		t.Fatalf("outcome = %v, want selected", d.Outcome)
	}
	if d.WorkspaceID != "work" {
		t.Fatalf("handed off to %q, want work", d.WorkspaceID)
	}
	if d.Primary != ReasonHandoff {
		t.Errorf("reason = %v, want %v", d.Primary, ReasonHandoff)
	}
}

// Above the threshold the conversation must not move. Moving early wastes the owner's quota
// and re-reads the whole conversation uncached on the new account.
func TestOwnerAboveThresholdStaysPut(t *testing.T) {
	st := handoffState(90, 2) // 10% remaining
	d := Evaluate(st, Request{OwnerWorkspaceID: "personal"}, base)
	if d.WorkspaceID != "personal" || d.Primary != ReasonOwnerBound {
		t.Fatalf("got %s/%v, want personal stays bound", d.WorkspaceID, d.Primary)
	}
}

// Exactly at the threshold hands off: the comparison is <=, pinned here in both directions.
func TestThresholdBoundaryIsInclusive(t *testing.T) {
	if d := Evaluate(handoffState(98, 2), Request{OwnerWorkspaceID: "personal"}, base); d.WorkspaceID != "work" {
		t.Errorf("at exactly 2%% remaining: stayed on %s, want handoff", d.WorkspaceID)
	}
	if d := Evaluate(handoffState(97.9, 2), Request{OwnerWorkspaceID: "personal"}, base); d.WorkspaceID != "personal" {
		t.Errorf("at 2.1%% remaining: handed off, want stay")
	}
}

// Zero disables the feature, and the old blocking behaviour must come back exactly.
func TestZeroThresholdRestoresBlocking(t *testing.T) {
	st := handoffState(100, 0)
	st.Workspaces["personal"].Paused = true
	d := Evaluate(st, Request{OwnerWorkspaceID: "personal"}, base)
	if d.Outcome != OutcomeBlocked || d.Primary != ReasonOwnerBlocked {
		t.Fatalf("got %v/%v, want blocked/owner_bound_but_blocked", d.Outcome, d.Primary)
	}
}

// A handoff must not become a way around a reserve rule. Work is protected, so a dead owner
// still blocks rather than spending protected quota.
func TestHandoffCannotBypassAReserveRule(t *testing.T) {
	st := handoffState(100, 2)
	st.Workspaces["personal"].Paused = true
	st.Workspaces["work"].Paused = true // stands in for any reason work is ineligible
	d := Evaluate(st, Request{OwnerWorkspaceID: "personal"}, base)
	if d.Outcome != OutcomeBlocked {
		t.Fatalf("outcome = %v, want blocked when no eligible alternative exists", d.Outcome)
	}
}

// With no alternative, a nearly-empty owner keeps serving. Handing off to nowhere would turn
// a working conversation into a dead one.
func TestNearlyEmptyOwnerKeepsServingWhenAlone(t *testing.T) {
	st := handoffState(99, 2)
	delete(st.Workspaces, "work")
	st.Order = []string{"personal"}
	d := Evaluate(st, Request{OwnerWorkspaceID: "personal"}, base)
	if d.Outcome != OutcomeSelected || d.WorkspaceID != "personal" {
		t.Fatalf("got %v/%s, want personal keeps serving", d.Outcome, d.WorkspaceID)
	}
}

// An unknown reading must not trigger a handoff: moving conversations on no evidence is worse
// than staying put.
func TestUnknownQuotaDoesNotTriggerHandoff(t *testing.T) {
	st := handoffState(99, 2)
	st.Workspaces["personal"].Windows = map[int64]Window{} // no readings at all
	d := Evaluate(st, Request{OwnerWorkspaceID: "personal"}, base)
	if d.WorkspaceID != "personal" {
		t.Fatalf("handed off on unknown evidence to %s", d.WorkspaceID)
	}
}

// A workspace with nothing left must stop being selected, even when a rule prefers it.
//
// This is the defect found in real use: eligibility considered only pause, credential and
// model, so a preferred workspace sitting at 100% used kept taking new conversations and
// every one of them failed upstream. The router has to know it is empty BEFORE spending a
// turn to find out.
func TestExhaustedWorkspaceIsNotSelectedEvenWhenPreferred(t *testing.T) {
	st := handoffState(100, 2) // personal fully used
	st.Rules = []Rule{{ID: "p", Kind: KindPrefer, Enabled: true, SourceWorkspaceID: "personal"}}

	d := Evaluate(st, Request{}, base)
	if d.WorkspaceID == "personal" {
		t.Fatalf("selected an exhausted workspace because a rule preferred it (%s)", d.Primary)
	}
	if d.WorkspaceID != "work" {
		t.Fatalf("selected %q, want the workspace that still has quota", d.WorkspaceID)
	}
}

// With EVERY workspace empty, one is still selected rather than blocked.
//
// Refusing here would hand the user nothing; trying at least surfaces upstream's own answer,
// and the reading may be stale or the window may have just rolled over. This is the same
// judgement a reserve rule makes when it falls back to the workspace it was protecting.
func TestAllWorkspacesExhaustedStillServesRatherThanBlocking(t *testing.T) {
	st := handoffState(100, 2)
	st.Workspaces["work"].Windows[300] = Window{
		Minutes: 100, UsedPercent: 100, ObservedAt: base,
		ResetsAt: st.Workspaces["personal"].Windows[300].ResetsAt,
	}
	st.Workspaces["work"].Windows[300] = Window{
		Minutes: 300, UsedPercent: 100, ObservedAt: base,
		ResetsAt: st.Workspaces["personal"].Windows[300].ResetsAt,
	}
	d := Evaluate(st, Request{}, base)
	if d.Outcome != OutcomeSelected {
		t.Fatalf("outcome = %v, want a last-resort selection rather than a refusal", d.Outcome)
	}
}

// Unknown quota is not exhaustion. Excluding a workspace we simply have not observed would
// strand it, which is worse than trying it.
func TestUnknownQuotaIsNotTreatedAsExhausted(t *testing.T) {
	st := handoffState(100, 2)
	st.Workspaces["personal"].Windows = map[int64]Window{} // no readings at all
	st.Rules = []Rule{{ID: "p", Kind: KindPrefer, Enabled: true, SourceWorkspaceID: "personal"}}

	d := Evaluate(st, Request{}, base)
	if d.WorkspaceID != "personal" {
		t.Fatalf("an unobserved workspace was excluded as if it were empty (got %q)", d.WorkspaceID)
	}
}
