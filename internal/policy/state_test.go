package policy

import (
	"testing"
	"time"
)

// A reading is tied to the window it was taken in. Once that window resets, a recent
// reading is still worthless: the counter it measured no longer exists. Age alone cannot
// catch this, which is why it is tested at the boundary in both directions.
func TestWindowExpiresAtResetRegardlessOfAge(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	resets := now.Add(1 * time.Minute)
	st := &State{StaleAfter: 10 * time.Minute}
	ws := &WorkspaceState{
		ID: "w1",
		Windows: map[int64]Window{
			300: {Minutes: 300, UsedPercent: 95, ObservedAt: now, ResetsAt: &resets},
		},
	}

	// Just before the reset: a fresh reading is still describing a live window.
	if _, ev := st.EvidenceFor(ws, 300, resets.Add(-time.Second)); ev != EvidenceFresh {
		t.Fatalf("before reset: want fresh, got %v", ev)
	}
	// Exactly at the reset: the window is gone, even though the reading is one minute old.
	if _, ev := st.EvidenceFor(ws, 300, resets); ev != EvidenceMissing {
		t.Fatalf("at reset: want missing, got %v", ev)
	}
	// After the reset: still gone, and the 95%% must not leak out with it.
	w, ev := st.EvidenceFor(ws, 300, resets.Add(time.Second))
	if ev != EvidenceMissing {
		t.Fatalf("after reset: want missing, got %v", ev)
	}
	if w.UsedPercent != 0 {
		t.Fatalf("a voided window must not carry its old percentage, got %v", w.UsedPercent)
	}
}
