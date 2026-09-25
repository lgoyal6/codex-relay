package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardCanBeFramed pins a decision rather than a behaviour.
//
// The dashboard binds loopback, so reading it from a phone means putting a proxy in
// front of it, and embedding that proxy in another page means the page is allowed to
// frame us. This test exists so that tightening frame-ancestors again is a change
// somebody makes on purpose, with this comment in front of them, rather than a
// hardening sweep that quietly breaks every remote dashboard.
func TestDashboardCanBeFramed(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	Dashboard("token", "test").ServeHTTP(w, r)

	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the dashboard must still send a Content-Security-Policy")
	}
	if strings.Contains(csp, "frame-ancestors 'none'") {
		t.Error("frame-ancestors 'none' stops the dashboard being embedded from another device")
	}
}

// TestDashboardStaysLocalOnly guards the part of the policy that was never relaxed.
// Allowing the page to be framed says nothing about where it may load code or send
// data, and those are the directives that keep credentials on this machine.
func TestDashboardStaysLocalOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	Dashboard("token", "test").ServeHTTP(w, r)

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy must keep %q; got %q", want, csp)
		}
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff must survive")
	}
}
