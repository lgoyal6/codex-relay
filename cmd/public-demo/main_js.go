//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"syscall/js"
	"time"
)

type input struct {
	Remaining  float64 `json:"remaining"`
	ResetHours int     `json:"reset_hours"`
	Evidence   string  `json:"evidence"`
	WorkPaused bool    `json:"work_paused"`
}

func evaluate(raw []byte) (any, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if in.Remaining < 0 || in.Remaining > 100 || in.ResetHours < 1 || in.ResetHours > 168 {
		return nil, fmt.Errorf("Remaining must be 0-100 and reset 1-168 hours")
	}
	if in.Evidence != "fresh" && in.Evidence != "stale" && in.Evidence != "missing" {
		return nil, fmt.Errorf("Choose fresh, stale or missing evidence")
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Duration(in.ResetHours) * time.Hour)
	workReset := now.Add(48 * time.Hour)
	observed := now
	if in.Evidence == "stale" {
		observed = now.Add(-time.Hour)
	}
	windows := map[int64]policy.Window{10080: {Minutes: 10080, UsedPercent: 100 - in.Remaining, ResetsAt: &reset, ObservedAt: observed}}
	if in.Evidence == "missing" {
		windows = map[int64]policy.Window{}
	}
	st := &policy.State{Version: 1, Order: []string{"personal", "work"}, DefaultWorkspaceID: "personal", StaleAfter: 10 * time.Minute,
		Workspaces: map[string]*policy.WorkspaceState{
			"personal": {ID: "personal", Name: "Personal", AccountID: "synthetic-a", CredentialOK: true, Windows: windows},
			"work":     {ID: "work", Name: "Work", AccountID: "synthetic-b", CredentialOK: true, Paused: in.WorkPaused, Windows: map[int64]policy.Window{10080: {Minutes: 10080, UsedPercent: 38, ResetsAt: &workReset, ObservedAt: now}}}},
		Rules: []policy.Rule{{ID: "reserve", Kind: policy.KindReserve, Enabled: true, SourceWorkspaceID: "personal", Window: policy.WindowRef{Minutes: 10080}, Comparison: policy.AtOrBelow, RemainingPercent: 30, ResetComparison: policy.AtLeast, ResetHours: 96, PreferredWorkspaceID: "work", NoAlternative: policy.StopAndExplain}}}
	d := policy.Evaluate(st, policy.Request{}, now)
	d.Simulated = true
	return d, nil
}

func main() {
	callback := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 1 || args[0].Type() != js.TypeString {
			return `{"error":"Expected one JSON input"}`
		}
		raw := args[0].String()
		var result any
		if len(raw) > 32768 {
			result = map[string]string{"error": "Input exceeds 32 KiB"}
		} else {
			value, err := evaluate([]byte(raw))
			if err != nil {
				result = map[string]string{"error": err.Error()}
			} else {
				result = value
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return fmt.Sprintf(`{"error":%q}`, err.Error())
		}
		return string(encoded)
	})
	js.Global().Set("publicDemo", callback)
	select {}
}
