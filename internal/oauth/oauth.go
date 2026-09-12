// Package oauth implements the browser sign-in this service uses to obtain its OWN
// credentials for each connected identity.
//
// Why independent credentials, evidenced rather than assumed: the Codex client's refresh
// tokens rotate and are single-use. codex-rs/login/auth/manager.rs carries a distinct
// failure reason for a reused refresh token ("your refresh token was already used"), so two
// holders of one refresh token invalidate each other. We therefore never read, copy, or
// compete over Codex's tokens; every connected identity gets its own chain.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultIssuer is the OpenAI auth issuer used by Codex.
	DefaultIssuer = "https://auth.openai.com"

	// ClientID is the Codex CLI's public PKCE client.
	//
	// Constraint discovered in Stage A: the redirect URI allow-list for this client only
	// permits http://localhost:1455 and :1457 (codex-rs/login/src/server.rs keeps these "in
	// sync with the Codex CLI Hydra redirect URI allow-list"). We cannot pick an arbitrary
	// port, and we cannot hold the port at the same time as `codex login`. Both facts are
	// surfaced to the user rather than hidden.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	PrimaryPort  = 1455
	FallbackPort = 1457

	scopes = "openid profile email offline_access"
)

// ErrPortBusy means neither allow-listed callback port could be bound.
var ErrPortBusy = errors.New("the sign-in callback port is in use")

// Tokens is one completed credential chain.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
}

// Identity is what the id_token tells us about who signed in.
//
// Account and workspace are distinct identities: ChatGPTUserID identifies the person,
// ChatGPTAccountID identifies the workspace. Email is a property, never a key.
type Identity struct {
	Email            string
	ChatGPTUserID    string
	ChatGPTAccountID string
	PlanType         string
	IsFedRAMP        bool
}

// Flow is a single in-progress browser sign-in.
type Flow struct {
	AuthURL string
	Port    int

	issuer   string
	verifier string
	state    string
	redirect string

	srv    *http.Server
	result chan result
	once   sync.Once
}

type result struct {
	tokens Tokens
	err    error
}

// Start binds the callback listener and returns the URL the user should open.
// The listener is bound before the URL is handed out, so the browser can never race it.
func Start(issuer string) (*Flow, error) {
	if issuer == "" {
		issuer = DefaultIssuer
	}
	verifier, err := randomString(64)
	if err != nil {
		return nil, err
	}
	state, err := randomString(32)
	if err != nil {
		return nil, err
	}

	ln, port, err := bindCallback()
	if err != nil {
		return nil, err
	}

	f := &Flow{
		Port:     port,
		issuer:   strings.TrimSuffix(issuer, "/"),
		verifier: verifier,
		state:    state,
		redirect: fmt.Sprintf("http://localhost:%d/auth/callback", port),
		result:   make(chan result, 1),
	}

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", f.redirect)
	q.Set("scope", scopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("state", state)
	f.AuthURL = f.issuer + "/oauth/authorize?" + q.Encode()

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", f.handleCallback)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// bindCallback tries the two allow-listed ports in order.
func bindCallback() (net.Listener, int, error) {
	var lastErr error
	for _, port := range []int{PrimaryPort, FallbackPort} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf(
		"%w: sign-in needs port %d or %d, which OpenAI allows for this application. "+
			"Close anything using them (a running `codex login` uses %d) and try again (%v)",
		ErrPortBusy, PrimaryPort, FallbackPort, PrimaryPort, lastErr)
}

func (f *Flow) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		f.finish(result{err: fmt.Errorf("sign-in was refused: %s", e)})
		writePage(w, "Sign-in was cancelled", "You can close this tab and try again in codex-relay.")
		return
	}
	if q.Get("state") != f.state {
		// A mismatched state is a cross-site attempt, not a user error.
		f.finish(result{err: errors.New("sign-in state did not match; the request was discarded")})
		writePage(w, "Sign-in could not be verified", "Close this tab and start again from codex-relay.")
		return
	}
	code := q.Get("code")
	if code == "" {
		f.finish(result{err: errors.New("sign-in returned no authorization code")})
		writePage(w, "Sign-in did not complete", "Close this tab and try again.")
		return
	}
	tok, err := f.exchange(r.Context(), code)
	f.finish(result{tokens: tok, err: err})
	if err != nil {
		writePage(w, "Sign-in could not be completed", "Close this tab and check codex-relay for the reason.")
		return
	}
	writePage(w, "Connected", "You can close this tab and return to codex-relay.")
}

func (f *Flow) finish(res result) {
	f.once.Do(func() {
		f.result <- res
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = f.srv.Shutdown(ctx)
		}()
	})
}

// Wait blocks until the browser completes the flow, the caller cancels, or it times out.
func (f *Flow) Wait(ctx context.Context) (Tokens, error) {
	select {
	case res := <-f.result:
		return res.tokens, res.err
	case <-ctx.Done():
		f.Cancel()
		return Tokens{}, ctx.Err()
	}
}

// Cancel abandons the flow and releases the port.
func (f *Flow) Cancel() {
	f.once.Do(func() {
		f.result <- result{err: errors.New("sign-in was cancelled")}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = f.srv.Shutdown(ctx)
	})
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (f *Flow) exchange(ctx context.Context, code string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", f.redirect)
	form.Set("code_verifier", f.verifier)
	return postToken(ctx, f.issuer+"/oauth/token", form)
}

// Refresh exchanges a refresh token for a new chain.
//
// The returned refresh token REPLACES the old one. Callers must persist it before the next
// refresh, because the previous token is now spent.
func Refresh(ctx context.Context, issuer, refreshToken string) (Tokens, error) {
	if issuer == "" {
		issuer = DefaultIssuer
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", ClientID)
	form.Set("refresh_token", refreshToken)
	return postToken(ctx, strings.TrimSuffix(issuer, "/")+"/oauth/token", form)
}

func postToken(ctx context.Context, endpoint string, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("contacting the sign-in service failed: %w", err)
	}
	defer resp.Body.Close()

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil || resp.StatusCode >= 400 {
		if resp.StatusCode >= 400 {
			return Tokens{}, fmt.Errorf("sign-in service returned %d", resp.StatusCode)
		}
		return Tokens{}, fmt.Errorf("sign-in response was unreadable: %w", err)
	}
	if tr.AccessToken == "" {
		return Tokens{}, errors.New("sign-in response contained no access token")
	}
	exp := time.Now().UTC().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.ExpiresIn == 0 {
		exp = time.Now().UTC().Add(time.Hour)
	}
	return Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		ExpiresAt:    exp,
	}, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writePage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<style>body{font:15px -apple-system,BlinkMacSystemFont,Segoe UI,sans-serif;margin:0;display:grid;place-items:center;height:100vh;background:#f6f7f9;color:#16181d}
.card{background:#fff;border:1px solid #e3e5e9;border-radius:12px;padding:28px 32px;max-width:380px;text-align:center}
h1{font-size:17px;margin:0 0 8px}p{margin:0;color:#5b6270}</style>
<div class="card"><h1>%s</h1><p>%s</p></div>`, title, title, body)
}
