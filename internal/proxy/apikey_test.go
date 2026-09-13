package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

// When the relay is locked, an unauthenticated caller must be refused BEFORE any credential
// is resolved and before anything reaches upstream. This is the hole the feature exists to
// close: without it any process on the machine could spend the user's real quota by POSTing
// to loopback with no credential at all.
func TestLockedRelayRefusesAnUnauthenticatedCallerBeforeSpending(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(200)
	}))
	defer up.Close()

	sel := &fakeSelector{
		decision: policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws"},
		identity: Identity{WorkspaceID: "ws", AccessToken: "secret-token"},
	}
	sel.authFn = func(r *http.Request) (string, error) {
		if r.Header.Get("X-Codex-Relay-Key") != "crl_good" {
			return "", errors.New("this relay requires an API key")
		}
		return "key-1", nil
	}
	p := newProxy(t, sel, up.URL)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("thread-id", "t1")
	p.ServeHTTP(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated caller got %d, want 401", rec.Code)
	}
	if reached {
		t.Fatal("the request reached upstream despite failing authentication; quota was spent")
	}
	if len(sel.claims) != 0 {
		t.Errorf("a conversation was bound for a caller that never authenticated: %v", sel.claims)
	}
}

// A valid key passes, and the turn is attributed to it so usage can be accounted per client.
func TestValidKeyIsAdmittedAndAttributed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: response.completed\ndata: {}\n\n"))
	}))
	defer up.Close()

	sel := &fakeSelector{
		decision: policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws"},
		identity: Identity{WorkspaceID: "ws", AccessToken: "tok"},
	}
	sel.authFn = func(r *http.Request) (string, error) {
		if r.Header.Get("X-Codex-Relay-Key") == "crl_good" {
			return "key-1", nil
		}
		return "", errors.New("nope")
	}
	p := newProxy(t, sel, up.URL)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("thread-id", "t2")
	r.Header.Set("X-Codex-Relay-Key", "crl_good")
	p.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("a valid key got %d", rec.Code)
	}
	var attributed bool
	for _, rec := range sel.records {
		if rec.APIKeyID == "key-1" {
			attributed = true
		}
	}
	if !attributed {
		t.Error("the turn was not attributed to the key that paid for it")
	}
}

// An unlocked relay must keep working with no key at all, or upgrading would break every
// existing client the moment the column appeared.
func TestUnlockedRelayStillServesAnonymousCallers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: response.completed\ndata: {}\n\n"))
	}))
	defer up.Close()

	sel := &fakeSelector{
		decision: policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws"},
		identity: Identity{WorkspaceID: "ws", AccessToken: "tok"},
	}
	// authFn nil means unlocked.
	p := newProxy(t, sel, up.URL)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("thread-id", "t3")
	p.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("an unlocked relay refused an anonymous caller with %d", rec.Code)
	}
}
