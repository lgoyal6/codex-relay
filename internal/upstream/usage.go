package upstream

import (
	"encoding/json"
	"fmt"
	"time"
)

// ParseUsage reads a quota reading from a GET /wham/usage response body.
//
// This endpoint does NOT use the x-codex-* headers the generation path uses, and it does not
// use the field names the in-band codex.rate_limits events use either. It reports
// primary_window/secondary_window, and states the period as limit_window_seconds rather than
// window_minutes. Reusing the header parser here silently yields nothing, because none of
// those headers are present on this response.
//
// Shape confirmed against a live response from the real endpoint, not from a mock.
func ParseUsage(body []byte, now time.Time) (Snapshot, error) {
	var b struct {
		RateLimit struct {
			Allowed      bool         `json:"allowed"`
			LimitReached bool         `json:"limit_reached"`
			Primary      *usageWindow `json:"primary_window"`
			Secondary    *usageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
		ReachedType string `json:"rate_limit_reached_type"`
		Credits     *struct {
			HasCredits bool   `json:"has_credits"`
			Unlimited  bool   `json:"unlimited"`
			Balance    string `json:"balance"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return Snapshot{}, fmt.Errorf("usage response is not the expected JSON: %w", err)
	}

	snap := Snapshot{
		LimitID:     "codex",
		LimitName:   "codex",
		Primary:     b.RateLimit.Primary.window(now),
		Secondary:   b.RateLimit.Secondary.window(now),
		ReachedType: b.ReachedType,
	}
	if b.Credits != nil {
		snap.Credits = &Credits{
			HasCredits: b.Credits.HasCredits,
			Unlimited:  b.Credits.Unlimited,
			Balance:    b.Credits.Balance,
		}
	}
	if snap.Primary == nil && snap.Secondary == nil {
		return Snapshot{}, fmt.Errorf("usage response carried no rate-limit window")
	}
	return snap, nil
}

type usageWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt       int64   `json:"reset_at"`
	ResetAfter    int64   `json:"reset_after_seconds"`
}

// window converts to the internal representation, or nil when the window is absent.
//
// A window with no period is not a window: recording it would create a row that the
// evaluator cannot match to any rule, and that the dashboard would render as a nameless bar.
func (u *usageWindow) window(now time.Time) *Window {
	if u == nil || u.WindowSeconds <= 0 {
		return nil
	}
	w := &Window{
		Minutes:     u.WindowSeconds / 60,
		UsedPercent: u.UsedPercent,
	}
	// reset_at is absolute; reset_after_seconds is relative. Prefer the absolute value and
	// fall back, because a reading with no reset time is held rather than trusted.
	switch {
	case u.ResetAt > 0:
		t := time.Unix(u.ResetAt, 0).UTC()
		w.ResetsAt = &t
	case u.ResetAfter > 0:
		t := now.Add(time.Duration(u.ResetAfter) * time.Second).UTC()
		w.ResetsAt = &t
	}
	return w
}
