package policy

import (
	"testing"
	"time"
)

var base = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// signatureRule is the exact workflow named in the build contract:
// "Use Personal normally. When its weekly quota remaining is at or below 30% and its weekly
// reset is at least four days away, protect Personal and prefer Work."
func signatureRule() Rule {
	return Rule{
		ID: "r1", Kind: KindReserve, Enabled: true,
		SourceWorkspaceID:    "personal",
		Window:               WindowRef{Minutes: 10080},
		Comparison:           AtOrBelow,
		RemainingPercent:     30,
		ResetComparison:      AtLeast,
		ResetHours:           96,
		PreferredWorkspaceID: "work",
		NoAlternative:        StopAndExplain,
	}
}

// stateWith builds a two-workspace state. usedPercent and resetIn describe Personal's weekly
// window; Work is always healthy.
func stateWith(usedPercent float64, resetIn time.Duration, observedAgo time.Duration) *State {
	reset := base.Add(resetIn)
	workReset := base.Add(48 * time.Hour)
	return &State{
		Version:            1,
		Order:              []string{"personal", "work"},
		DefaultWorkspaceID: "personal",
		StaleAfter:         10 * time.Minute,
		Rules:              []Rule{signatureRule()},
		Workspaces: map[string]*WorkspaceState{
			"personal": {
				ID: "personal", Name: "Personal", AccountID: "acct_a", CredentialOK: true,
				Windows: map[int64]Window{
					10080: {Minutes: 10080, UsedPercent: usedPercent, ResetsAt: &reset, ObservedAt: base.Add(-observedAgo)},
					300:   {Minutes: 300, UsedPercent: 10, ResetsAt: &workReset, ObservedAt: base.Add(-observedAgo)},
				},
			},
			"work": {
				ID: "work", Name: "Acme Work", AccountID: "acct_b", CredentialOK: true,
				Windows: map[int64]Window{
					10080: {Minutes: 10080, UsedPercent: 38, ResetsAt: &workReset, ObservedAt: base},
				},
			},
		},
	}
}

func TestExactThresholdEqualityProtects(t *testing.T) {
	// remaining == 30 with `at or below 30` must match. This is the boundary the contract
	// calls out: at-or-below, not below.
	st := stateWith(70, 120*time.Hour, 0) // remaining exactly 30, reset 5 days away
	d := Evaluate(st, Request{Model: "gpt-5.1-codex"}, base)
	if d.WorkspaceID != "work" {
		t.Fatalf("remaining exactly 30 must trigger protection and select work, got %q (%s)", d.WorkspaceID, d.Summary)
	}
	if d.Primary != ReasonPreferred {
		t.Fatalf("expected preferred reason, got %s", d.Primary)
	}
}

func TestJustAboveThresholdDoesNotProtect(t *testing.T) {
	// remaining 30.01 must NOT match, and Personal stays the default.
	st := stateWith(69.99, 120*time.Hour, 0)
	d := Evaluate(st, Request{Model: "gpt-5.1-codex"}, base)
	if d.WorkspaceID != "personal" {
		t.Fatalf("remaining 30.01 must not protect; got %q", d.WorkspaceID)
	}
}

func TestBelowComparisonExcludesEquality(t *testing.T) {
	st := stateWith(70, 120*time.Hour, 0)
	r := signatureRule()
	r.Comparison = Below
	st.Rules = []Rule{r}
	d := Evaluate(st, Request{Model: "gpt-5.1-codex"}, base)
	if d.WorkspaceID != "personal" {
		t.Fatalf("`below 30` must not match remaining exactly 30; got %q", d.WorkspaceID)
	}
}

func TestResetTimeBoundaryIsExactNotCalendarRounded(t *testing.T) {
	// Exactly 96h away satisfies ">= 96h".
	st := stateWith(75, 96*time.Hour, 0)
	if d := Evaluate(st, Request{}, base); d.WorkspaceID != "work" {
		t.Fatalf("reset exactly 96h away must satisfy >= 96h; got %q", d.WorkspaceID)
	}
	// One second under 96h does not.
	st = stateWith(75, 96*time.Hour-time.Second, 0)
	if d := Evaluate(st, Request{}, base); d.WorkspaceID != "personal" {
		t.Fatalf("reset 1s under 96h must not satisfy >= 96h; got %q", d.WorkspaceID)
	}
}

func TestTimeCrossingReevaluates(t *testing.T) {
	// Same state, later clock: as the reset approaches, the >=96h condition stops holding
	// and protection lifts. No latch, no hidden margin.
	st := stateWith(75, 100*time.Hour, 0)
	if d := Evaluate(st, Request{}, base); d.WorkspaceID != "work" {
		t.Fatalf("at t0 protection should hold; got %q", d.WorkspaceID)
	}
	later := base.Add(5 * time.Hour) // now 95h until reset
	st.StaleAfter = 0                // isolate the time condition from staleness
	if d := Evaluate(st, Request{}, later); d.WorkspaceID != "personal" {
		t.Fatalf("after the reset came within 96h, protection must lift; got %q", d.WorkspaceID)
	}
}

func TestAbsentWindowIsUnknownNotUnlimited(t *testing.T) {
	st := stateWith(75, 120*time.Hour, 0)
	delete(st.Workspaces["personal"].Windows, 10080) // plan does not expose a weekly window
	d := Evaluate(st, Request{}, base)
	if !d.UnknownEvidence {
		t.Fatal("a missing window must be reported as unknown evidence")
	}
	if d.WorkspaceID == "personal" {
		t.Fatal("a missing protection-sensitive reading must not be treated as 'not protected'")
	}
	if d.WorkspaceID != "work" {
		t.Fatalf("should fall to the preferred alternative; got %q", d.WorkspaceID)
	}
}

func TestMissingResetTimeIsUnknown(t *testing.T) {
	st := stateWith(75, 120*time.Hour, 0)
	w := st.Workspaces["personal"].Windows[10080]
	w.ResetsAt = nil
	st.Workspaces["personal"].Windows[10080] = w
	d := Evaluate(st, Request{}, base)
	if !d.UnknownEvidence || d.WorkspaceID != "work" {
		t.Fatalf("missing reset time must be unknown and hold the reserve; got unknown=%v ws=%q", d.UnknownEvidence, d.WorkspaceID)
	}
}

func TestStaleQuotaStillEvaluatesAndIsLabelled(t *testing.T) {
	st := stateWith(75, 120*time.Hour, time.Hour) // observed 1h ago, StaleAfter 10m
	d := Evaluate(st, Request{}, base)
	if d.WorkspaceID != "work" {
		t.Fatalf("stale but present evidence should still apply the reserve; got %q", d.WorkspaceID)
	}
	found := false
	for _, n := range d.Notes {
		if n.Code == ReasonProtected && contains(n.Message, "stale") {
			found = true
		}
	}
	if !found {
		t.Fatal("a stale reading must be labelled as stale in the explanation")
	}
}

func TestNoSilentFallbackOntoProtectedQuota(t *testing.T) {
	// Protection holds and the only alternative is unavailable. Default behaviour is to
	// stop and explain, never to quietly spend the protected quota.
	st := stateWith(75, 120*time.Hour, 0)
	st.Workspaces["work"].CredentialOK = false
	st.Workspaces["work"].CredentialNote = "Sign in to Acme Work again."
	d := Evaluate(st, Request{}, base)
	if d.Outcome != OutcomeBlocked {
		t.Fatalf("expected blocked, got %s selecting %q", d.Outcome, d.WorkspaceID)
	}
	if d.WorkspaceID == "personal" {
		t.Fatal("must not silently fall back onto the protected workspace")
	}
	if d.Primary != ReasonReserveNoAlternate {
		t.Fatalf("expected reserve-no-alternative reason, got %s", d.Primary)
	}
}

func TestUnsupportedModelRemovesWorkspace(t *testing.T) {
	st := stateWith(10, 120*time.Hour, 0) // no protection
	st.Workspaces["personal"].EligibleModels = map[string]bool{"gpt-5.1-codex": true}
	st.Workspaces["work"].EligibleModels = map[string]bool{"gpt-5.1-codex": true}
	d := Evaluate(st, Request{Model: "some-unavailable-model"}, base)
	if d.Outcome != OutcomeBlocked {
		t.Fatalf("no workspace reports the model, so admission must stop; got %s -> %q", d.Outcome, d.WorkspaceID)
	}
}

func TestUnknownModelListDoesNotRemoveWorkspace(t *testing.T) {
	st := stateWith(10, 120*time.Hour, 0)
	d := Evaluate(st, Request{Model: "anything"}, base)
	if d.Outcome != OutcomeSelected {
		t.Fatal("an unobserved model list is unknown, not 'supports nothing'")
	}
}

func TestPausedIsNotTheSameAsCredentialLoss(t *testing.T) {
	st := stateWith(10, 120*time.Hour, 0)
	st.Workspaces["personal"].Paused = true
	d := Evaluate(st, Request{}, base)
	if d.WorkspaceID != "work" {
		t.Fatalf("paused workspace must not be admitted; got %q", d.WorkspaceID)
	}
	for _, c := range d.Candidates {
		if c.WorkspaceID == "personal" && c.Reason != ReasonPaused {
			t.Fatalf("paused must be reported as paused, not %s", c.Reason)
		}
	}
}

func TestOwnerBoundConversationDoesNotMigrate(t *testing.T) {
	// Personal is protected, but an existing conversation already owned by Personal keeps
	// running there rather than silently moving.
	st := stateWith(75, 120*time.Hour, 0)
	d := Evaluate(st, Request{OwnerWorkspaceID: "work"}, base)
	if d.WorkspaceID != "work" || d.Primary != ReasonOwnerBound {
		t.Fatalf("owner-bound conversation must stay on its owner; got %q/%s", d.WorkspaceID, d.Primary)
	}
}

func TestOwnerBoundBlockedExplainsRatherThanMigrating(t *testing.T) {
	st := stateWith(75, 120*time.Hour, 0)
	st.Workspaces["personal"].CredentialOK = false
	d := Evaluate(st, Request{OwnerWorkspaceID: "personal"}, base)
	if d.Outcome != OutcomeBlocked || d.Primary != ReasonOwnerBlocked {
		t.Fatalf("a blocked owner must be explained, not migrated; got %s/%s -> %q", d.Outcome, d.Primary, d.WorkspaceID)
	}
	if d.WorkspaceID != "" {
		t.Fatalf("no workspace may be selected for a blocked owner-bound turn; got %q", d.WorkspaceID)
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	st := stateWith(75, 120*time.Hour, 0)
	first := Evaluate(st, Request{Model: "m"}, base)
	for i := 0; i < 200; i++ {
		if got := Evaluate(st, Request{Model: "m"}, base); got.WorkspaceID != first.WorkspaceID || got.Primary != first.Primary {
			t.Fatalf("evaluation is not deterministic: %q/%s vs %q/%s", got.WorkspaceID, got.Primary, first.WorkspaceID, first.Primary)
		}
	}
}

func TestSentenceMatchesTheContractExample(t *testing.T) {
	names := map[string]string{"personal": "Personal", "work": "Work"}
	got := signatureRule().Sentence(func(id string) string { return names[id] })
	want := "Protect Personal and prefer Work when its weekly quota remaining is <= 30% and its reset is >= 4 days away. If Work cannot serve the request, stop and explain."
	if got != want {
		t.Fatalf("generated sentence drifted.\n got: %s\nwant: %s", got, want)
	}
}

func TestValidateRejectsSelfAlternative(t *testing.T) {
	r := signatureRule()
	r.PreferredWorkspaceID = r.SourceWorkspaceID
	if err := r.Validate(); err == nil {
		t.Fatal("a workspace cannot be its own alternative")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
