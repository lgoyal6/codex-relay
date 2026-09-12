package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/routing"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/store"
)

// This file drives the WHOLE connect path except the human in the browser: a fake issuer, a
// real PKCE flow with a real loopback callback, realistically sized JWTs, the real OS
// credential store, and the real database writes.
//
// It exists because two consecutive real sign-ins failed at two different steps that no test
// covered, and because the step AFTER the one just fixed - the workspace INSERT - had still
// never executed against real-shaped data. Asking a person to sign in again is the most
// expensive way to discover a bug, so this covers the path first.

// bigJWT builds an unsigned JWT whose payload carries the given claims, padded to about the
// size a real ChatGPT token has.
func bigJWT(claims map[string]any, padTo int) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	if padTo > 0 {
		claims["pad"] = strings.Repeat("p", padTo)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "." +
		strings.Repeat("s", 320)
}

type fakeBackend struct {
	issuer   *httptest.Server
	upstream *httptest.Server
	// accountsBody is what /wham/accounts/check returns.
	accountsBody string
	tokenHits    int
}

func newFakeBackend(t *testing.T, accountsBody string) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{accountsBody: accountsBody}

	idToken := bigJWT(map[string]any{
		"email": "lgoyal@example.edu",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-874ZeL6rtVBKj8XRP1345nTZ",
			"chatgpt_account_id": "9f2c1d84-3b6e-4a71-9c0d-27ab55e1f3aa",
			"chatgpt_plan_type":  "prolite",
		},
	}, 900)

	fb.issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.tokenHits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			// Realistic sizes: a long access token, a long id token, a shorter refresh token.
			"access_token":  bigJWT(map[string]any{"sub": "user-874Z"}, 1500),
			"refresh_token": strings.Repeat("R", 620),
			"id_token":      idToken,
			"expires_in":    3600,
		})
	}))
	t.Cleanup(fb.issuer.Close)

	fb.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/wham/accounts/check") {
			w.WriteHeader(404)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("accounts/check was called without a bearer token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fb.accountsBody)
	}))
	t.Cleanup(fb.upstream.Close)
	return fb
}

func connectService(t *testing.T, sec secrets.Store) *Service {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/connect.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := clock.System()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(db, routing.NewRegistry(clk), NewCredentialManager(sec, clk, ""), sec, clk, log)
	svc.Start()
	t.Cleanup(svc.Close)
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return svc
}

// driveFlow completes the browser half of the PKCE flow by calling the loopback callback
// with the state the authorize URL carries.
func driveFlow(t *testing.T, authURL string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	redirect := u.Query().Get("redirect_uri")
	if state == "" || redirect == "" {
		t.Fatalf("authorize URL is missing state or redirect_uri: %s", authURL)
	}
	if u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("the flow must use PKCE S256")
	}
	cb := fmt.Sprintf("%s?code=%s&state=%s", redirect, "test-auth-code", url.QueryEscape(state))
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("calling the loopback callback failed: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func runConnect(t *testing.T, accountsBody string, sec secrets.Store) ([]string, *Service, error) {
	t.Helper()
	fb := newFakeBackend(t, accountsBody)
	svc := connectService(t, sec)
	conn := NewConnector(svc, fb.issuer.URL, fb.upstream.URL)

	authURL, flowID, err := conn.BeginConnect()
	if err != nil {
		t.Skipf("cannot bind the OAuth callback port here: %v", err)
	}

	done := make(chan struct{})
	var ids []string
	var cerr error
	go func() {
		defer close(done)
		ids, cerr = conn.CompleteConnect(flowID)
	}()

	driveFlow(t, authURL)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("CompleteConnect did not finish")
	}
	return ids, svc, cerr
}

// TestConnectStoresARealisticCredentialAndRegistersTheWorkspace is the end-to-end case that
// the two failed real sign-ins would have caught. It uses the REAL OS credential store, with
// tokens the size real ones are.
func TestConnectStoresARealisticCredentialAndRegistersTheWorkspace(t *testing.T) {
	sec := secrets.NewOS()
	if h := sec.Probe(); !h.OK {
		t.Skipf("credential store unavailable here: %s", h.Detail)
	}
	// The shape observed live from the real backend: accounts as an ARRAY with one entry.
	body := `{"accounts":[{"id":"9f2c1d84-3b6e-4a71-9c0d-27ab55e1f3aa","name":"Personal","structure":"personal"}],
	          "account_ordering":["9f2c1d84-3b6e-4a71-9c0d-27ab55e1f3aa"],
	          "default_account_id":"9f2c1d84-3b6e-4a71-9c0d-27ab55e1f3aa"}`

	ids, svc, err := runConnect(t, body, sec)
	expectedRef := "workspace/acct_user-EXAMPLE000000000000000000:9f2c1d84-3b6e-4a71-9c0d-27ab55e1f3aa"
	t.Cleanup(func() { _ = sec.Delete(expectedRef) })

	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected exactly one connected workspace, got %d: %v", len(ids), ids)
	}

	// The database row must exist, which is the step that had never run live.
	var name, chatgptID, ref string
	var credOK int
	if err := svc.DB.SQL().QueryRow(
		`SELECT display_name, chatgpt_account_id, credential_ref, credential_ok FROM workspaces WHERE id = ?`,
		ids[0]).Scan(&name, &chatgptID, &ref, &credOK); err != nil {
		t.Fatalf("no workspace row was written: %v", err)
	}
	if name != "Personal" {
		t.Errorf("display name = %q, want the name from accounts/check", name)
	}
	if chatgptID != "9f2c1d84-3b6e-4a71-9c0d-27ab55e1f3aa" {
		t.Errorf("chatgpt_account_id = %q", chatgptID)
	}
	if credOK != 1 {
		t.Error("the workspace should be marked as having a usable credential")
	}

	// The credential must be readable back out of the real OS store, intact.
	cred, err := sec.Get(ref)
	if err != nil {
		t.Fatalf("the stored credential could not be read back: %v", err)
	}
	if len(cred.AccessToken) < 1500 {
		t.Errorf("access token came back truncated: %d bytes", len(cred.AccessToken))
	}
	if len(cred.RefreshToken) != 620 {
		t.Errorf("refresh token came back wrong: %d bytes, want 620", len(cred.RefreshToken))
	}
	if cred.AccountID != chatgptID {
		t.Errorf("credential account id = %q, want %q", cred.AccountID, chatgptID)
	}

	// And the routing snapshot must see it, which is what the dashboard reads.
	st := svc.Registry.Current()
	if len(st.Workspaces) != 1 {
		t.Fatalf("the routing snapshot has %d workspaces, want 1", len(st.Workspaces))
	}
	if st.DefaultWorkspaceID != ids[0] {
		t.Errorf("the first connected workspace should become the default, got %q", st.DefaultWorkspaceID)
	}
}

// TestConnectFallsBackWhenAccountsCheckIsEmpty covers the first live defect: an empty but
// successful account list must still connect the signed-in identity.
func TestConnectFallsBackWhenAccountsCheckIsEmpty(t *testing.T) {
	sec := secrets.NewMemory()
	ids, svc, err := runConnect(t, `{"accounts":{},"account_ordering":[]}`, sec)
	if err != nil {
		t.Fatalf("an empty account list must fall back, not fail: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected the signed-in identity to be connected, got %v", ids)
	}
	var structure string
	if err := svc.DB.SQL().QueryRow(`SELECT structure FROM workspaces WHERE id = ?`, ids[0]).Scan(&structure); err != nil {
		t.Fatalf("no workspace row: %v", err)
	}
	if structure != "personal" {
		t.Errorf("fallback workspace structure = %q, want personal", structure)
	}
}

// TestConnectReportsFailureWhenTheCredentialCannotBeStored proves a storage failure surfaces
// as an error rather than as a silent success, which is how the second real sign-in behaved.
func TestConnectReportsFailureWhenTheCredentialCannotBeStored(t *testing.T) {
	body := `{"accounts":[{"id":"ws-1","name":"Personal","structure":"personal"}]}`
	ids, svc, err := runConnect(t, body, failingStore{})
	if err == nil {
		t.Fatal("a credential that cannot be stored must fail the connect, not report success")
	}
	if len(ids) != 0 {
		t.Fatalf("nothing should be reported as connected, got %v", ids)
	}
	var n int
	_ = svc.DB.SQL().QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&n)
	if n != 0 {
		t.Fatalf("no workspace row should survive a failed credential write, found %d", n)
	}
	if !strings.Contains(err.Error(), "securely") {
		t.Errorf("the error should say the sign-in could not be stored securely, got: %v", err)
	}
}

type failingStore struct{}

func (failingStore) Kind() string { return "failing (test)" }
func (failingStore) Set(string, secrets.Credential) error {
	return fmt.Errorf("%w: simulated backend limit", secrets.ErrUnavailable)
}
func (failingStore) Get(string) (secrets.Credential, error) {
	return secrets.Credential{}, secrets.ErrNotFound
}
func (failingStore) Delete(string) error { return nil }
func (failingStore) Probe() secrets.Health {
	return secrets.Health{OK: true, Kind: "failing (test)"}
}
