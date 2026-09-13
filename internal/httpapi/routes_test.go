package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/routing"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/service"
	"github.com/lgoyal6/codex-relay/internal/store"
)

type stubConnector struct{ err error }

func (s stubConnector) BeginConnect() (string, string, error)    { return "https://x", "f1", s.err }
func (s stubConnector) CompleteConnect(string) ([]string, error) { return []string{"ws"}, s.err }
func (s stubConnector) CancelConnect(string)                     {}
func (s stubConnector) SetPaused(string, bool) error             { return s.err }
func (s stubConnector) Rename(string, string) error              { return s.err }
func (s stubConnector) RefreshWorkspaces() error                 { return s.err }
func (s stubConnector) Remove(string) (service.RemovalEffect, error) {
	return service.RemovalEffect{}, s.err
}

func testAPI(t *testing.T) (*API, http.Handler) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := clock.System()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sec := secrets.NewMemory()
	svc := service.New(db, routing.NewRegistry(clk), service.NewCredentialManager(sec, clk, ""), sec, clk, log)
	svc.Start()
	t.Cleanup(svc.Close)
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := guard(t)
	a := &API{Svc: svc, Guard: g, ProxyAddr: "127.0.0.1:7788", Version: "test", Connector: stubConnector{}}
	return a, g.Wrap(a.Routes())
}

// routesFromSource reads the registered endpoints out of api.go, so an endpoint added later
// is covered by these tests automatically instead of being quietly exempt from them.
func routesFromSource(t *testing.T) [][2]string {
	t.Helper()
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) (/api/[^"]*)"`)
	var out [][2]string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	if len(out) < 10 {
		t.Fatalf("only found %d routes in api.go; the parser has drifted", len(out))
	}
	return out
}

// concrete replaces path wildcards with a value, so the request actually reaches the handler.
func concrete(p string) string {
	return strings.NewReplacer("{id}", "some-workspace-id").Replace(p)
}

// bounded gives a request a deadline. /api/events is a long-lived stream that only returns
// when its context ends, and any handler that blocks past this deadline is a finding rather
// than a reason to leave it untested.
func bounded(t *testing.T, r *http.Request) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.Context(), 400*time.Millisecond)
	t.Cleanup(cancel)
	return r.WithContext(ctx)
}

// serve runs the handler and fails if it does not return promptly.
func serve(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(rec, bounded(t, r)) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s %s did not return within 5s", r.Method, r.URL.Path)
	}
	return rec
}

// Every endpoint must require the session token. The service listens on loopback, which any
// other process on the machine can reach, so an unguarded endpoint is a real hole and not a
// theoretical one. Driven by the route list itself so a new endpoint cannot skip this.
func TestEveryEndpointRefusesAMissingToken(t *testing.T) {
	_, h := testAPI(t)
	for _, route := range routesFromSource(t) {
		method, path := route[0], concrete(route[1])
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Host = "127.0.0.1:7788"
		r.Header.Set("Content-Type", "application/json")
		rec := serve(t, h, r)

		if rec.Code < 400 {
			t.Errorf("%s %s answered %d without a token; it must be refused", method, path, rec.Code)
		}
	}
}

// A wrong token is refused exactly like a missing one, with no hint that distinguishes them.
func TestEveryEndpointRefusesAWrongToken(t *testing.T) {
	_, h := testAPI(t)
	for _, route := range routesFromSource(t) {
		method, path := route[0], concrete(route[1])
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Host = "127.0.0.1:7788"
		r.Header.Set("X-Codex-Pool-Token", "not-the-token")
		rec := serve(t, h, r)
		if rec.Code < 400 {
			t.Errorf("%s %s accepted a forged token (%d)", method, path, rec.Code)
		}
	}
}

// With a valid token, no endpoint may panic on a malformed body. A panic in a handler takes
// the whole service down, so every conversation dies because one request was badly formed.
func TestNoEndpointPanicsOnMalformedInput(t *testing.T) {
	a, h := testAPI(t)
	bodies := []string{
		``, `{`, `[]`, `null`, `"a string"`, `{"paused":"not-a-bool"}`,
		`{"id":` + strings.Repeat("9", 400) + `}`,
		strings.Repeat(`{"a":`, 500) + "1" + strings.Repeat("}", 500),
	}
	for _, route := range routesFromSource(t) {
		method, path := route[0], concrete(route[1])
		for _, body := range bodies {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Host = "127.0.0.1:7788"
			r.Header.Set("X-Codex-Pool-Token", a.Guard.Token())
			r.Header.Set("Content-Type", "application/json")
			rec := serve(t, h, r)

			if rec.Code >= 500 && rec.Code != http.StatusBadGateway && rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s returned %d on body %.40q; malformed input is the caller's fault, not a server fault",
					method, path, rec.Code, body)
			}
		}
	}
}
