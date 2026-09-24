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

// proxyThrough runs one request through WrapProxy and reports whether it reached the proxy.
func proxyThrough(t *testing.T, g *Guard, r *http.Request) (reached bool, code int) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	g.WrapProxy(next).ServeHTTP(rec, r)
	return reached, rec.Code
}

func websocketUpgrade(host, origin string) *http.Request {
	r := httptest.NewRequest("GET", "/backend-api/codex/responses", nil)
	r.Host = host
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// TestProxyRefusesPagesThatCouldSpendQuota is the attack WrapProxy exists for. Browsers
// apply no CORS to WebSockets, so before this guard any page the user had open could
// open the socket, run a Codex turn on their account and read the reply.
func TestProxyRefusesPagesThatCouldSpendQuota(t *testing.T) {
	g := guard(t)
	for _, origin := range []string{
		"https://evil.example.com",
		"http://evil.example.com:7788", // a rebound page is still its own origin
		"null",                         // any page can produce this from a sandboxed frame
		"http://127.0.0.1:9999",        // another local server is not the dashboard
		"%zz not a url",
	} {
		reached, code := proxyThrough(t, g, websocketUpgrade("127.0.0.1:7788", origin))
		if reached || code != http.StatusForbidden {
			t.Errorf("WebSocket with Origin %q: reached=%v code=%d, want refused with 403", origin, reached, code)
		}
	}

	// The same page trying a plain cross-origin POST instead of a socket.
	r := httptest.NewRequest("POST", "/backend-api/codex/responses", nil)
	r.Host = "127.0.0.1:7788"
	r.Header.Set("Origin", "https://evil.example.com")
	if reached, code := proxyThrough(t, g, r); reached || code != http.StatusForbidden {
		t.Errorf("cross-origin POST: reached=%v code=%d, want refused with 403", reached, code)
	}
}

// TestProxyRefusesReboundHost covers DNS rebinding on the proxy, including the GET that
// carries no Origin and so would pass an Origin check alone.
func TestProxyRefusesReboundHost(t *testing.T) {
	g := guard(t)
	r := httptest.NewRequest("GET", "/backend-api/codex/models", nil)
	r.Host = "rebind.attacker.test:7788"
	if reached, code := proxyThrough(t, g, r); reached || code != http.StatusForbidden {
		t.Errorf("rebound Host: reached=%v code=%d, want refused with 403", reached, code)
	}
}

// TestProxyAdmitsCodex is the other half: the guard must not refuse the client it exists
// to serve. Codex is native and sends no Origin, on either transport and either spelling
// of loopback. The dashboard's own origin, and a local app embedding a browser, which
// sends a non-web scheme no page can forge, are also admitted.
func TestProxyAdmitsCodex(t *testing.T) {
	g := guard(t)
	cases := []*http.Request{
		websocketUpgrade("127.0.0.1:7788", ""),
		websocketUpgrade("localhost:7788", ""),
		websocketUpgrade("[::1]:7788", ""),
		websocketUpgrade("127.0.0.1:7788", "http://127.0.0.1:7788"),
		websocketUpgrade("127.0.0.1:7788", "app://codex"),
	}
	post := httptest.NewRequest("POST", "/backend-api/codex/responses", nil)
	post.Host = "127.0.0.1:7788"
	cases = append(cases, post)
	for _, r := range cases {
		if reached, code := proxyThrough(t, g, r); !reached || code != http.StatusOK {
			t.Errorf("%s Host=%q Origin=%q: reached=%v code=%d, want admitted",
				r.Method, r.Host, r.Header.Get("Origin"), reached, code)
		}
	}
}
