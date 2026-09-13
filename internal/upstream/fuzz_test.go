package upstream

import (
	"math"
	"net/http"
	"testing"
	"time"
)

// Every one of these parsers reads bytes that came from the network. A panic in any of them
// takes down a turn, and on the WebSocket path it would take down the bridge goroutine for a
// conversation that was working a moment earlier. None of them may panic on any input.

func FuzzParseUsage(f *testing.F) {
	f.Add([]byte(`{"rate_limit":{"primary_window":{"used_percent":50,"limit_window_seconds":18000,"reset_at":1789263574}}}`))
	f.Add([]byte(`{"rate_limit":{}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`{"rate_limit":{"primary_window":{"used_percent":-1e308,"limit_window_seconds":-9223372036854775808,"reset_at":-1}}}`))
	f.Add([]byte(`{"rate_limit":{"primary_window":{"used_percent":1e400,"limit_window_seconds":9223372036854775807,"reset_at":9223372036854775807}}}`))

	now := time.Unix(1_700_000_000, 0).UTC()
	f.Fuzz(func(t *testing.T, body []byte) {
		snap, err := ParseUsage(body, now)
		if err != nil {
			return
		}
		for _, w := range []*Window{snap.Primary, snap.Secondary} {
			if w == nil {
				continue
			}
			// A window that parsed must be usable by the evaluator, which divides by the
			// period and compares remaining against a threshold.
			if w.Minutes <= 0 {
				t.Fatalf("accepted a window with minutes=%d", w.Minutes)
			}
			if math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) {
				t.Fatalf("accepted used_percent=%v", w.UsedPercent)
			}
			if w.ResetsAt != nil && w.ResetsAt.IsZero() {
				t.Fatalf("accepted a zero reset time")
			}
		}
	})
}

func FuzzMaybeRateLimitEvent(f *testing.F) {
	f.Add([]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":10,"window_minutes":300,"resets_at":1}}}`))
	f.Add([]byte(`{"type":"codex.rate_limits"}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, frame []byte) {
		snap, ok := MaybeRateLimitEvent(frame)
		if !ok {
			return
		}
		for _, w := range []*Window{snap.Primary, snap.Secondary} {
			if w != nil && (math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0)) {
				t.Fatalf("accepted used_percent=%v", w.UsedPercent)
			}
		}
	})
}

func FuzzMaybeStreamError(f *testing.F) {
	f.Add([]byte(`{"type":"error","error":{"message":"usage limit reached"}}`))
	f.Add([]byte(`{"type":"response.failed","response":{"error":{"code":"429"}}}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, frame []byte) {
		fail, ok := MaybeStreamError(frame)
		if ok && fail.Class == "" {
			t.Fatalf("a stream failure was reported with no class, so it cannot be acted on")
		}
	})
}

func FuzzMaybeTokenUsage(f *testing.F) {
	f.Add([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`))
	f.Add([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":-5}}}`))
	f.Fuzz(func(t *testing.T, frame []byte) {
		u, ok := MaybeTokenUsage(frame)
		if !ok {
			return
		}
		// Negative counts would be written straight into the history and the cost estimate.
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.TotalTokens < 0 || u.CachedInputTokens < 0 {
			t.Fatalf("accepted negative token counts: %+v", u)
		}
	})
}

func FuzzParseRateLimits(f *testing.F) {
	f.Add("codex", "50", "300", "1789263574")
	f.Add("", "", "", "")
	f.Add("codex_bengalfox", "not-a-number", "-1", "999999999999999999999")
	f.Fuzz(func(t *testing.T, family, pct, mins, reset string) {
		h := http.Header{}
		h.Set("x-"+family+"-primary-used-percent", pct)
		h.Set("x-"+family+"-primary-window-minutes", mins)
		h.Set("x-"+family+"-primary-reset-at", reset)
		for _, snap := range ParseRateLimits(h) {
			for _, w := range []*Window{snap.Primary, snap.Secondary} {
				if w != nil && (math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0)) {
					t.Fatalf("accepted used_percent=%v from header %q", w.UsedPercent, pct)
				}
			}
		}
	})
}
