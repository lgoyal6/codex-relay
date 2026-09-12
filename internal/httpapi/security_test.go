package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func guard(t *testing.T) *Guard {
	t.Helper()
	g, err := NewGuard("7788")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestDNSRebindingIsRefused is the attack this guard exists for: a name the attacker
// controls resolves to 127.0.0.1, so the request reaches us with their Host header.
func TestDNSRebindingIsRefused(t *testing.T) {
	g := guard(t)
	for _, host := range []string{"evil.example.com:7788", "rebind.attacker.test:7788", "myapp.local:7788"} {
		r := httptest.NewRequest("GET", "/api/state", nil)
		r.Host = host
		if err := g.CheckHost(r); err == nil {
			t.Errorf("Host %q must be refused: it resolves to loopback but is not a loopback literal", host)
		}
	}
	for _, host := range []string{"127.0.0.1:7788", "[::1]:7788", "localhost:7788"} {
		r := httptest.NewRequest("GET", "/api/state", nil)
		r.Host = host
		if err := g.CheckHost(r); err != nil {
			t.Errorf("Host %q must be allowed: %v", host, err)
		}
	}
}

func TestCrossOriginIsRefused(t *testing.T) {
	g := guard(t)
	r := httptest.NewRequest("POST", "/api/rules", nil)
	r.Host = "127.0.0.1:7788"
	r.Header.Set("Origin", "https://evil.example.com")
	if err := g.CheckOrigin(r); err == nil {
		t.Fatal("a cross-origin page must not be able to call the API")
	}
	r.Header.Set("Origin", "http://127.0.0.1:7788")
	if err := g.CheckOrigin(r); err != nil {
		t.Fatalf("the dashboard's own origin must be allowed: %v", err)
	}
}

func TestTokenIsRequired(t *testing.T) {
	g := guard(t)
	r := httptest.NewRequest("POST", "/api/rules", nil)
	r.Host = "127.0.0.1:7788"
	if err := g.CheckToken(r); err == nil {
		t.Fatal("a request without the session token must be refused")
	}
	r.Header.Set("X-Codex-Pool-Token", g.Token())
	if err := g.CheckToken(r); err != nil {
		t.Fatalf("the issued token must be accepted: %v", err)
	}
	r.Header.Set("X-Codex-Pool-Token", g.Token()+"x")
	if err := g.CheckToken(r); err == nil {
		t.Fatal("a wrong token must be refused")
	}
}

func TestWrapEnforcesAllThree(t *testing.T) {
	g := guard(t)
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	// Rebound host, correct token: still refused.
	r := httptest.NewRequest("POST", "/api/rules", nil)
	r.Host = "evil.example.com:7788"
	r.Header.Set("X-Codex-Pool-Token", g.Token())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("rebound host should be 403, got %d", w.Code)
	}

	// Good host, no token: refused.
	r = httptest.NewRequest("POST", "/api/rules", nil)
	r.Host = "127.0.0.1:7788"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token should be 401, got %d", w.Code)
	}

	// Everything right: allowed.
	r = httptest.NewRequest("POST", "/api/rules", nil)
	r.Host = "127.0.0.1:7788"
	r.Header.Set("Origin", "http://localhost:7788")
	r.Header.Set("X-Codex-Pool-Token", g.Token())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a legitimate dashboard request should pass, got %d", w.Code)
	}
}
