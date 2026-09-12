package upstream

import (
	"net/http"
	"testing"
)

// The header names and value shapes here are the ones captured from a live codex-cli 0.154.0
// run against the compatibility harness, not names carried over from the original handoff
// (which used a `reset-after-seconds` family that does not exist in this client).
func TestParsesTheLiveHeaderFamily(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "24.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", "1789106944")
	h.Set("x-codex-secondary-used-percent", "72.0")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-at", "1789535344")
	h.Set("x-codex-credits-has-credits", "false")
	h.Set("x-codex-credits-unlimited", "false")

	got := ParseRateLimits(h)
	if len(got) != 1 || got[0].LimitID != "codex" {
		t.Fatalf("expected one codex family, got %+v", got)
	}
	s := got[0]
	if s.Primary == nil || s.Primary.UsedPercent != 24.5 || s.Primary.Minutes != 300 {
		t.Fatalf("primary parsed wrong: %+v", s.Primary)
	}
	if s.Primary.ResetsAt == nil || s.Primary.ResetsAt.Unix() != 1789106944 {
		t.Fatalf("reset-at must be an absolute unix timestamp, got %+v", s.Primary.ResetsAt)
	}
	if s.Secondary == nil || s.Secondary.Minutes != 10080 {
		t.Fatalf("secondary parsed wrong: %+v", s.Secondary)
	}
	if s.Credits == nil || s.Credits.HasCredits || s.Credits.Unlimited {
		t.Fatalf("credits parsed wrong: %+v", s.Credits)
	}
}

func TestDiscoversAdditionalLimitFamilies(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "12.5")
	h.Set("x-codex-secondary-primary-used-percent", "80")
	h.Set("x-codex-secondary-primary-window-minutes", "1440")
	got := ParseRateLimits(h)
	if len(got) != 2 {
		t.Fatalf("expected the default family plus codex_secondary, got %d: %+v", len(got), got)
	}
	if got[1].LimitID != "codex_secondary" {
		t.Fatalf("second family id = %q", got[1].LimitID)
	}
}

func TestAbsentWindowStaysAbsent(t *testing.T) {
	// No headers at all must not become "0% used", which would read as full quota.
	got := ParseRateLimits(http.Header{})
	if len(got) != 1 {
		t.Fatalf("expected the default family placeholder, got %+v", got)
	}
	if got[0].Primary != nil || got[0].Secondary != nil {
		t.Fatal("missing headers must yield no window, never a fabricated 0% reading")
	}
}

// The real backend sends NO rate-limit headers on a WebSocket upgrade (verified against
// chatgpt.com: the 101 carries only Cloudflare and proxy headers). Quota arrives in-band as
// this event instead, so parsing it is the only way a real turn ever updates quota.
func TestParsesAnInStreamRateLimitEvent(t *testing.T) {
	frame := []byte(`{"type":"codex.rate_limits","plan_type":"prolite","rate_limits":` +
		`{"primary":{"used_percent":31.25,"window_minutes":300,"reset_at":1789106944},` +
		`"secondary":{"used_percent":64,"window_minutes":10080,"reset_at":1789535344}},` +
		`"credits":{"has_credits":false,"unlimited":false,"balance":""}}`)
	s, ok := MaybeRateLimitEvent(frame)
	if !ok {
		t.Fatal("a codex.rate_limits event must be recognised")
	}
	if s.Primary == nil || s.Primary.UsedPercent != 31.25 || s.Primary.Minutes != 300 {
		t.Fatalf("primary parsed wrong: %+v", s.Primary)
	}
	if s.Primary.ResetsAt == nil || s.Primary.ResetsAt.Unix() != 1789106944 {
		t.Fatalf("reset_at must be an absolute unix timestamp: %+v", s.Primary.ResetsAt)
	}
	if s.Secondary == nil || s.Secondary.Minutes != 10080 {
		t.Fatalf("secondary parsed wrong: %+v", s.Secondary)
	}
	if s.Credits == nil || s.Credits.HasCredits {
		t.Fatalf("credits parsed wrong: %+v", s.Credits)
	}
}

// An SSE frame arrives as "data: {...}", not as a bare object.
func TestParsesARateLimitEventInsideAnSSELine(t *testing.T) {
	frame := []byte(`data: {"type":"codex.rate_limits","rate_limits":{"primary":` +
		`{"used_percent":12,"window_minutes":300,"reset_at":1789106944}}}`)
	s, ok := MaybeRateLimitEvent(frame)
	if !ok || s.Primary == nil || s.Primary.UsedPercent != 12 {
		t.Fatalf("an SSE-wrapped event must parse, got ok=%v %+v", ok, s.Primary)
	}
}

// Everything else on the stream must be rejected cheaply and never mistaken for quota.
func TestIgnoresFramesThatAreNotRateLimitEvents(t *testing.T) {
	for _, frame := range []string{
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1"}}`,
		`{"type":"codex.rate_limits"`, // truncated, not valid JSON
		`plain text`,
		``,
		`{"type":"other","rate_limits":{"primary":{"used_percent":99}}}`,
	} {
		if _, ok := MaybeRateLimitEvent([]byte(frame)); ok {
			t.Errorf("frame should not be read as quota: %q", frame)
		}
	}
}

// A zero-valued window in an EVENT is a real reading the server chose to send, unlike an
// all-zero header set which means the headers were simply absent.
func TestZeroUsageEventIsStillAReading(t *testing.T) {
	frame := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"reset_at":1789106944}}}`)
	s, ok := MaybeRateLimitEvent(frame)
	if !ok || s.Primary == nil || s.Primary.UsedPercent != 0 {
		t.Fatalf("a zero-usage event is a real reading: ok=%v %+v", ok, s.Primary)
	}
}

// TestClassifiesTheRealUsageLimitRefusal uses the EXACT message a real exhausted account
// returned. A WebSocket turn that fails still upgraded with 101, so without classifying the
// frame the dashboard showed an out-of-quota account as perfectly healthy.
func TestClassifiesTheRealUsageLimitRefusal(t *testing.T) {
	frame := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error",` +
		`"message":"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), ` +
		`visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Sep 17th, 2026 4:37 PM."}}`)
	f, ok := MaybeStreamError(frame)
	if !ok {
		t.Fatal("a streamed error frame must be recognised")
	}
	if f.Class != "quota" {
		t.Fatalf("a usage-limit refusal must classify as quota, got %q", f.Class)
	}
	if f.Message == "" {
		t.Fatal("the upstream message must be preserved so the user can act on it")
	}
}

func TestClassifiesOtherStreamedFailures(t *testing.T) {
	cases := []struct{ frame, want string }{
		{`{"type":"error","status":400,"error":{"message":"The 'gpt-5.1-codex' model is not supported when using Codex with a ChatGPT account."}}`, "request"},
		{`{"type":"error","status":401,"error":{"message":"Could not validate your token."}}`, "credential"},
		{`{"type":"error","status":429,"error":{"message":"slow down"}}`, "quota"},
		{`{"type":"error","status":503,"error":{"message":"upstream unavailable"}}`, "upstream"},
	}
	for _, c := range cases {
		f, ok := MaybeStreamError([]byte(c.frame))
		if !ok {
			t.Errorf("not recognised: %s", c.frame)
			continue
		}
		if f.Class != c.want {
			t.Errorf("classified %q, want %q for: %s", f.Class, c.want, c.frame)
		}
	}
}

func TestOrdinaryFramesAreNotTreatedAsErrors(t *testing.T) {
	for _, frame := range []string{
		`{"type":"response.output_text.delta","delta":"the type:error string appears nowhere"}`,
		`{"type":"response.completed"}`,
		`data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":5}}}`,
		``,
	} {
		if f, ok := MaybeStreamError([]byte(frame)); ok {
			t.Errorf("frame wrongly read as a failure (%s): %s", f.Class, frame)
		}
	}
}
