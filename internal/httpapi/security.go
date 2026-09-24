package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Guard protects the loopback dashboard API against cross-origin requests and DNS
// rebinding.
//
// A loopback listener is reachable from any page the user's browser happens to load, and an
// attacker-controlled DNS name can be made to resolve to 127.0.0.1. Binding locally is
// therefore not, by itself, protection. Three checks, all required for state-changing calls:
//
//  1. Host must be a literal loopback address with our port. A rebound name like
//     "evil.example.com:7788" resolves to us but does not match, so it is refused.
//  2. Origin, when present, must be a loopback origin on our port.
//  3. A session token, issued to the dashboard page itself, must be presented.
type Guard struct {
	port  string
	token string
}

func NewGuard(port string) (*Guard, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Guard{port: port, token: base64.RawURLEncoding.EncodeToString(b)}, nil
}

// Token is the value the dashboard page is given and must echo back.
func (g *Guard) Token() string { return g.token }

// CheckHost enforces the literal-loopback rule. This is the DNS rebinding defence.
func (g *Guard) CheckHost(r *http.Request) error {
	host := r.Host
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		h, p = host, ""
	}
	if p != "" && p != g.port {
		return fmt.Errorf("unexpected port in Host header")
	}
	if !isLoopbackLiteral(h) {
		// A hostname that merely resolves to 127.0.0.1 is exactly the rebinding case.
		return fmt.Errorf("requests must address this service as a loopback address, not as %q", h)
	}
	return nil
}

// CheckOrigin rejects cross-origin callers. A missing Origin is allowed only for safe
// methods, which covers a user typing the URL directly.
func (g *Guard) CheckOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if isSafeMethod(r.Method) {
			return nil
		}
		// A state-changing request with no Origin still has to prove itself with the token.
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("unreadable Origin")
	}
	h := u.Hostname()
	if !isLoopbackLiteral(h) || (u.Port() != "" && u.Port() != g.port) {
		return fmt.Errorf("this page is not allowed to call the codex-relay API")
	}
	return nil
}

// CheckToken enforces the per-session token in constant time.
func (g *Guard) CheckToken(r *http.Request) error {
	got := r.Header.Get("X-Codex-Pool-Token")
	if got == "" {
		if c, err := r.Cookie("codex_pool_session"); err == nil {
			got = c.Value
		}
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(g.token)) != 1 {
		return fmt.Errorf("this request did not carry a valid dashboard session")
	}
	return nil
}

// Wrap applies every check appropriate to the request.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := g.CheckHost(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err := g.CheckOrigin(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err := g.CheckToken(r); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WrapProxy guards the Codex traffic path, which the dashboard guard cannot cover.
//
// The proxy cannot ask for the dashboard token: its caller is Codex, a native client that
// never loads the dashboard page. Binding to loopback does not make that safe, for the
// same reason it does not make the dashboard safe: a loopback listener is reachable from
// every page the user's browser loads. The stakes are higher here. This path spends quota
// with a stored sign-in, and browsers apply no CORS to WebSockets, so a page that can open
// the socket can run a Codex turn on the user's account and read the answer back.
//
// What separates Codex from a page is that Codex sends no Origin, while a browser sends one
// on every request that could spend quota: every WebSocket upgrade and every cross-origin
// POST. So:
//
//  1. Host must be a literal loopback address with our port, exactly as for the dashboard.
//     This is the DNS rebinding defence, and it also covers a rebound GET, which carries
//     no Origin.
//  2. An Origin a web page could have produced is refused unless it is a loopback origin on
//     our port. That is every http and https origin, and "null", which any page can produce
//     from a sandboxed frame. An Origin with another scheme belongs to a local application
//     that embeds a browser, and no page can send one.
func (g *Guard) WrapProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := g.CheckHost(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err := g.checkProxyOrigin(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Guard) checkProxyOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	refused := fmt.Errorf("a web page is not allowed to send traffic through codex-relay")
	if origin == "null" {
		return refused
	}
	u, err := url.Parse(origin)
	if err != nil {
		return refused
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if !isLoopbackLiteral(u.Hostname()) || (u.Port() != "" && u.Port() != g.port) {
			return refused
		}
	}
	return nil
}

func isLoopbackLiteral(h string) bool {
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		// "localhost" is resolved by the OS and cannot be pointed elsewhere by a remote
		// DNS server in the rebinding scenario, so it is accepted alongside the literals.
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func isSafeMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "GET", "HEAD", "OPTIONS":
		return true
	}
	return false
}
