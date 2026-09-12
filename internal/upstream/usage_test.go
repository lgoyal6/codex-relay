package upstream

import (
	"os"
	"testing"
	"time"
)

// The fixture is a real response from the live endpoint with only the identity fields
// replaced. Parsing is tested against that rather than against a hand-written shape, because
// the mock originally had this endpoint's field names wrong in three places and a test built
// on the mock would have agreed with the mock.
func TestParseUsageFromRealResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/usage_real.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1789247645, 0).UTC()
	snap, err := ParseUsage(raw, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if snap.Primary == nil || snap.Secondary == nil {
		t.Fatal("both windows should be present")
	}
	// 18000s and 604800s are the 5 hour and weekly windows, in minutes.
	if snap.Primary.Minutes != 300 {
		t.Errorf("primary minutes = %d, want 300", snap.Primary.Minutes)
	}
	if snap.Secondary.Minutes != 10080 {
		t.Errorf("secondary minutes = %d, want 10080", snap.Secondary.Minutes)
	}
	if snap.Primary.UsedPercent != 68 {
		t.Errorf("primary used = %v, want 68", snap.Primary.UsedPercent)
	}
	if snap.Secondary.UsedPercent != 11 {
		t.Errorf("secondary used = %v, want 11", snap.Secondary.UsedPercent)
	}
	// reset_at is absolute, not a duration. Reading it as a duration would place the reset
	// roughly fifty-six thousand years out and no reserve rule would ever match.
	if snap.Primary.ResetsAt == nil || snap.Primary.ResetsAt.Unix() != 1789263574 {
		t.Errorf("primary reset = %v, want unix 1789263574", snap.Primary.ResetsAt)
	}
	if snap.Credits == nil || snap.Credits.HasCredits {
		t.Errorf("credits should be present and empty, got %+v", snap.Credits)
	}
}

// A response with no windows must be an error, not an empty snapshot that would overwrite a
// good reading with zeroes.
func TestParseUsageRejectsWindowlessResponse(t *testing.T) {
	if _, err := ParseUsage([]byte(`{"rate_limit":{"allowed":true}}`), time.Now()); err == nil {
		t.Fatal("want an error when no window is present")
	}
}

// reset_after_seconds is the fallback when reset_at is absent.
func TestParseUsageFallsBackToRelativeReset(t *testing.T) {
	now := time.Unix(1000000, 0).UTC()
	snap, err := ParseUsage([]byte(
		`{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":600}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Primary.ResetsAt == nil || snap.Primary.ResetsAt.Unix() != 1000600 {
		t.Fatalf("reset = %v, want now+600s", snap.Primary.ResetsAt)
	}
}
