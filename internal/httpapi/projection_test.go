package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

// A fresh install has no workspaces. A nil Go slice marshals to JSON null, and the dashboard
// treats these fields as arrays, so null here rendered a black screen on the very first
// screen a new user sees. The contract is an empty list.
func TestEmptyStateMarshalsListsNotNull(t *testing.T) {
	st := &policy.State{Workspaces: map[string]*policy.WorkspaceState{}}
	now := time.Now()

	for _, tc := range []struct {
		name string
		v    any
	}{
		{"projections", buildProjections(st, now)},
		{"pool_totals", buildPoolTotals(st, now, map[string]string{})},
	} {
		raw, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(raw) != "[]" {
			t.Errorf("%s marshalled to %s, want []", tc.name, raw)
		}
	}
}

// The same guarantee at the level the dashboard actually consumes: the whole state document.
func TestStateResponseHasNoNullLists(t *testing.T) {
	var resp StateResponse
	resp.Projections = buildProjections(&policy.State{Workspaces: map[string]*policy.WorkspaceState{}}, time.Now())
	resp.PoolTotals = buildPoolTotals(&policy.State{Workspaces: map[string]*policy.WorkspaceState{}}, time.Now(), map[string]string{})

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"projections":null`, `"pool_totals":null`} {
		if strings.Contains(string(raw), field) {
			t.Errorf("state carries %s; the dashboard calls .length on it", field)
		}
	}
}
