package service

import (
	"strings"
	"testing"

	"github.com/lgoyal6/codex-relay/internal/oauth"
)

// TestKeepUsableDropsEntriesWithNoID: an entry with no workspace id cannot be registered, and
// silently skipping it is how a connect ends up registering nothing while reporting success.
func TestKeepUsableDropsEntriesWithNoID(t *testing.T) {
	got := keepUsable([]workspaceInfo{
		{ID: "acct_a", Name: "A"},
		{ID: "", Name: "no id"},
		{ID: "   ", Name: "blank id"},
		{ID: "acct_b", Name: "B"},
	})
	if len(got) != 2 || got[0].ID != "acct_a" || got[1].ID != "acct_b" {
		t.Fatalf("keepUsable = %+v, want only the two entries with ids", got)
	}
}

func TestKeepUsableOnEmptyInput(t *testing.T) {
	if got := keepUsable(nil); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// TestFallbackNameNeverEmpty: the fallback label is what a workspace is called when discovery
// gave us nothing, so it must never be blank.
func TestFallbackNameNeverEmpty(t *testing.T) {
	for _, id := range []oauth.Identity{
		{Email: "person@example.com"},
		{},
	} {
		if strings.TrimSpace(fallbackName(id)) == "" {
			t.Fatalf("fallbackName(%+v) is blank", id)
		}
	}
}

// TestDisplayNameFallsBackToAStableIdentifier: never invent a label, but never show blank.
func TestDisplayNameFallsBackToAStableIdentifier(t *testing.T) {
	if got := displayName("Acme Work", "acct_x", "person@example.com"); got != "Acme Work" {
		t.Fatalf("a real name must win, got %q", got)
	}
	// With no upstream name, the signed-in email is the most recognisable true label.
	if got := displayName("  ", "acct_abcdefghijkl", "person@example.com"); got != "person@example.com" {
		t.Fatalf("a blank name should fall back to the signed-in email, got %q", got)
	}
	// With neither, a stable identifier, never an invented label.
	got := displayName("  ", "acct_abcdefghijkl", "")
	if !strings.Contains(got, "acct_abcde") {
		t.Fatalf("with no name and no email, use a stable identifier, got %q", got)
	}
}

// TestJSONShapeHelpersRevealStructureNotContent backs the diagnostic logging: it must be
// possible to tell WHY discovery found nothing without writing workspace names or ids to a log.
func TestJSONShapeHelpersRevealStructureNotContent(t *testing.T) {
	cases := []struct {
		raw    string
		kind   string
		length int
	}{
		{`{}`, "object", 0},
		{`{"a":{},"b":{}}`, "object", 2},
		{`[]`, "array", 0},
		{`[1,2,3]`, "array", 3},
		{`null`, "absent", 0},
		{`"x"`, "scalar", 0},
	}
	for _, c := range cases {
		if got := jsonKind([]byte(c.raw)); got != c.kind {
			t.Errorf("jsonKind(%s) = %q, want %q", c.raw, got, c.kind)
		}
		if got := jsonLen([]byte(c.raw)); got != c.length {
			t.Errorf("jsonLen(%s) = %d, want %d", c.raw, got, c.length)
		}
	}
	keys := jsonKeys([]byte(`{"accounts":{},"account_ordering":[],"default_account_id":"x"}`))
	want := []string{"account_ordering", "accounts", "default_account_id"}
	if len(keys) != len(want) {
		t.Fatalf("jsonKeys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("jsonKeys = %v, want %v (sorted)", keys, want)
		}
	}
}
