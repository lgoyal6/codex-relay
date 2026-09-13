// Package upstream talks to the ChatGPT backend and parses its quota protocol.
package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Window mirrors one reported quota window.
//
// Field names and semantics are taken from the Codex client's own parser
// (codex-rs/codex-api/src/rate_limits.rs) and confirmed against a live codex-cli 0.154.0 run:
// used_percent is a percentage USED, window_minutes is the window length, and reset_at is an
// ABSOLUTE unix timestamp in seconds, not a duration.
type Window struct {
	Minutes     int64
	UsedPercent float64
	ResetsAt    *time.Time
}

// Snapshot is one limit family's reading.
type Snapshot struct {
	LimitID   string
	LimitName string
	Primary   *Window
	Secondary *Window
	Credits   *Credits
	// ReachedType is set when the upstream says a limit was hit.
	ReachedType string
}

type Credits struct {
	HasCredits bool
	Unlimited  bool
	Balance    string
}

// ParseRateLimits reads every limit family present in a response.
//
// Families are discovered exactly the way the client does it: any header matching
// x-<limit>-primary-used-percent introduces a family. The default family is "codex".
func ParseRateLimits(h http.Header) []Snapshot {
	var out []Snapshot
	if s, ok := parseFamily(h, "codex"); ok {
		out = append(out, s)
	}
	seen := map[string]bool{"codex": true}
	for name := range h {
		l := strings.ToLower(name)
		rest, ok := strings.CutSuffix(l, "-primary-used-percent")
		if !ok {
			continue
		}
		limit, ok := strings.CutPrefix(rest, "x-")
		if !ok || seen[limit] {
			continue
		}
		seen[limit] = true
		if s, ok := parseFamily(h, limit); ok && s.hasData() {
			out = append(out, s)
		}
	}
	return out
}

func (s Snapshot) hasData() bool {
	return s.Primary != nil || s.Secondary != nil || s.Credits != nil
}

func parseFamily(h http.Header, limit string) (Snapshot, bool) {
	prefix := "x-" + strings.ReplaceAll(strings.ToLower(limit), "_", "-")
	s := Snapshot{
		LimitID:     strings.ReplaceAll(strings.ToLower(limit), "-", "_"),
		LimitName:   strings.TrimSpace(h.Get(prefix + "-limit-name")),
		Primary:     parseWindow(h, prefix+"-primary"),
		Secondary:   parseWindow(h, prefix+"-secondary"),
		Credits:     parseCredits(h),
		ReachedType: strings.TrimSpace(h.Get("x-codex-rate-limit-reached-type")),
	}
	return s, true
}

func parseWindow(h http.Header, prefix string) *Window {
	raw := h.Get(prefix + "-used-percent")
	if raw == "" {
		return nil
	}
	used, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return nil
	}
	w := &Window{UsedPercent: used}
	if m, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"-window-minutes")), 10, 64); err == nil {
		w.Minutes = m
	}
	if r, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"-reset-at")), 10, 64); err == nil {
		t := time.Unix(r, 0).UTC()
		w.ResetsAt = &t
	}
	// The client drops an all-zero window with no reset; mirror that so we do not invent a
	// "100% remaining" reading out of an empty header set.
	if w.UsedPercent == 0 && w.Minutes == 0 && w.ResetsAt == nil {
		return nil
	}
	return w
}

func parseCredits(h http.Header) *Credits {
	has, ok1 := parseBool(h.Get("x-codex-credits-has-credits"))
	unl, ok2 := parseBool(h.Get("x-codex-credits-unlimited"))
	if !ok1 || !ok2 {
		return nil
	}
	return &Credits{HasCredits: has, Unlimited: unl, Balance: strings.TrimSpace(h.Get("x-codex-credits-balance"))}
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	}
	return false, false
}

// --- in-stream quota events ---

// rateLimitEventMarker is the cheap pre-check. Scanning every streamed frame with a JSON
// parser would put a decoder on the hot path of every turn; a substring test does not.
const rateLimitEventMarker = `"codex.rate_limits"`

// eventWindow mirrors the wire shape of a window inside a codex.rate_limits event. Note the
// field is reset_at here, while the header family spells it -reset-at; both are absolute
// unix seconds.
type eventWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes"`
	ResetAt       *int64  `json:"reset_at"`
}

type rateLimitEvent struct {
	Type      string `json:"type"`
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Primary   *eventWindow `json:"primary"`
		Secondary *eventWindow `json:"secondary"`
	} `json:"rate_limits"`
	Credits *struct {
		HasCredits bool   `json:"has_credits"`
		Unlimited  bool   `json:"unlimited"`
		Balance    string `json:"balance"`
	} `json:"credits"`
	MeteredLimitName string `json:"metered_limit_name"`
	LimitName        string `json:"limit_name"`
}

// MaybeRateLimitEvent extracts a quota reading from a streamed frame.
//
// This exists because the real backend sends NO rate-limit headers on a WebSocket upgrade
// (verified against chatgpt.com: the 101 response carries only Cloudflare and proxy headers).
// Real turns are WebSocket upgrades, so header parsing alone recorded nothing at all and the
// dashboard stayed empty after real turns. Quota arrives in-band instead, as a
// `codex.rate_limits` event, which is also how the Codex client itself learns it
// (codex-rs/codex-api/src/rate_limits.rs::parse_rate_limit_event).
//
// It returns false for anything that is not such an event, cheaply.
func MaybeRateLimitEvent(frame []byte) (Snapshot, bool) {
	if !bytes.Contains(frame, []byte(rateLimitEventMarker)) {
		return Snapshot{}, false
	}
	payload := frame
	// A frame may be an SSE line ("data: {...}") rather than a bare object.
	if i := bytes.IndexByte(payload, '{'); i > 0 {
		payload = payload[i:]
	}
	var ev rateLimitEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.Type != "codex.rate_limits" {
		return Snapshot{}, false
	}

	limit := strings.TrimSpace(ev.MeteredLimitName)
	if limit == "" {
		limit = strings.TrimSpace(ev.LimitName)
	}
	if limit == "" {
		limit = "codex"
	}
	s := Snapshot{LimitID: strings.ReplaceAll(strings.ToLower(limit), "-", "_")}
	if ev.RateLimit != nil {
		s.Primary = mapEventWindow(ev.RateLimit.Primary)
		s.Secondary = mapEventWindow(ev.RateLimit.Secondary)
	}
	if ev.Credits != nil {
		s.Credits = &Credits{
			HasCredits: ev.Credits.HasCredits,
			Unlimited:  ev.Credits.Unlimited,
			Balance:    strings.TrimSpace(ev.Credits.Balance),
		}
	}
	if !s.hasData() {
		return Snapshot{}, false
	}
	return s, true
}

func mapEventWindow(w *eventWindow) *Window {
	if w == nil {
		return nil
	}
	out := &Window{UsedPercent: w.UsedPercent}
	if w.WindowMinutes != nil {
		out.Minutes = *w.WindowMinutes
	}
	if w.ResetAt != nil {
		t := time.Unix(*w.ResetAt, 0).UTC()
		out.ResetsAt = &t
	}
	// Unlike the header family, an all-zero event window is still a real reading: the server
	// chose to send it. Only a completely absent window is dropped, which the nil check above
	// already handles.
	return out
}

// --- in-stream errors ---

const streamErrorMarker = `"type":"error"`

type streamError struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Error  struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// StreamFailure is an error the upstream delivered INSIDE the stream.
type StreamFailure struct {
	// Class is the shared failure vocabulary: quota, credential, upstream, request.
	Class   string
	Status  int
	Message string
}

// MaybeStreamError extracts an upstream failure carried as a stream frame.
//
// This matters because a WebSocket turn that fails still completed its HTTP upgrade with 101.
// Judged on the status line alone, an account that is entirely out of quota looks like a
// healthy turn, and the dashboard says everything is fine. Observed live: a real account that
// had hit its usage limit produced a refusal frame while codex-relay recorded outcome=selected
// with no error class at all.
func MaybeStreamError(frame []byte) (StreamFailure, bool) {
	if !bytes.Contains(frame, []byte(streamErrorMarker)) {
		return StreamFailure{}, false
	}
	payload := frame
	if i := bytes.IndexByte(payload, '{'); i > 0 {
		payload = payload[i:]
	}
	var e streamError
	if err := json.Unmarshal(payload, &e); err != nil || e.Type != "error" {
		return StreamFailure{}, false
	}

	msg := strings.TrimSpace(e.Error.Message)
	f := StreamFailure{Status: e.Status, Message: msg, Class: classifyStreamError(e.Status, e.Error.Code, msg)}
	return f, true
}

// classifyStreamError maps a streamed failure onto the same vocabulary the HTTP path uses.
// Quota is detected by message as well as status, because the upstream reports a spent
// allowance as a plain 400 with an explanatory message rather than a 429.
func classifyStreamError(status int, code, message string) string {
	m := strings.ToLower(message)
	switch {
	case strings.Contains(m, "usage limit"),
		strings.Contains(m, "rate limit"),
		strings.Contains(m, "quota"),
		strings.Contains(m, "purchase more credits"),
		status == 429:
		return "quota"
	case status == 401, status == 403,
		strings.Contains(m, "authentication"), strings.Contains(m, "sign in again"):
		return "credential"
	case status >= 500:
		return "upstream"
	case status >= 400, code != "":
		return "request"
	}
	return "upstream"
}

// --- token usage ---

const usageMarker = `"usage"`

// TokenUsage is what a completed turn reported consuming.
type TokenUsage struct {
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	ReasoningTokens   int64
	TotalTokens       int64
}

type usageEvent struct {
	Type     string `json:"type"`
	Response struct {
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

// MaybeTokenUsage extracts the token counts a completed turn reported.
//
// These are the model's own numbers, carried on the response.completed event. They are the
// only token figures this tool ever shows: nothing here is estimated from text length.
func MaybeTokenUsage(frame []byte) (TokenUsage, bool) {
	if !bytes.Contains(frame, []byte(usageMarker)) {
		return TokenUsage{}, false
	}
	payload := frame
	if i := bytes.IndexByte(payload, '{'); i > 0 {
		payload = payload[i:]
	}
	var ev usageEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.Response.Usage == nil {
		return TokenUsage{}, false
	}
	u := ev.Response.Usage
	out := TokenUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.InputTokensDetails != nil {
		out.CachedInputTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	// A negative count is not a measurement. These values are written into the history and
	// multiplied by a price, so one bad frame would show a negative spend and drag a running
	// total down. Reject the reading rather than clamping it: a clamped zero is
	// indistinguishable from a real zero, and this codebase keeps those distinct.
	if out.InputTokens < 0 || out.CachedInputTokens < 0 || out.OutputTokens < 0 ||
		out.ReasoningTokens < 0 || out.TotalTokens < 0 {
		return TokenUsage{}, false
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	if out.TotalTokens == 0 {
		return TokenUsage{}, false
	}
	return out, true
}
