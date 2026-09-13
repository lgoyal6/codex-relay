package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The callback listens on loopback, which any process on the machine can reach, and it hands
// an authorization code straight to a token exchange. State is the only thing separating a
// real redirect from a forged one, so these are security properties rather than robustness
// ones.

func startFlow(t *testing.T, issuer string) *Flow {
	t.Helper()
	f, err := Start(issuer)
	if err != nil {
		t.Skipf("callback port unavailable here: %v", err)
	}
	t.Cleanup(f.Cancel)
	return f
}

// A forged callback carrying the wrong state must be discarded, and crucially must NOT reach
// the token exchange. If it did, any local process could spend an authorization code it
// guessed or observed.
func TestCallbackWithWrongStateIsRejectedWithoutExchanging(t *testing.T) {
	exchanged := false
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanged = true
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600}`))
	}))
	defer issuer.Close()

	f := startFlow(t, issuer.URL)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/auth/callback?code=stolen-code&state=not-the-state", nil)
	f.handleCallback(rec, r)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.Wait(ctx); err == nil {
		t.Fatal("a callback with a mismatched state was accepted")
	}
	if exchanged {
		t.Fatal("the token endpoint was called for a callback that failed state validation")
	}
}

// The real state must be accepted, or the property above is vacuous.
func TestCallbackWithCorrectStateCompletes(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok","refresh_token":"r","expires_in":3600}`))
	}))
	defer issuer.Close()

	f := startFlow(t, issuer.URL)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/auth/callback?code=good&state="+url.QueryEscape(f.state), nil)
	f.handleCallback(rec, r)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tok, err := f.Wait(ctx)
	if err != nil {
		t.Fatalf("a valid callback was rejected: %v", err)
	}
	if tok.AccessToken != "tok" {
		t.Fatalf("got %q", tok.AccessToken)
	}
}

// An empty code must not be exchanged. An issuer error response must surface as an error
// rather than hanging the caller waiting for a result that never comes.
func TestCallbackRejectsMissingCodeAndSurfacesIssuerErrors(t *testing.T) {
	for name, query := range map[string]string{
		"no code":      "?state=%s",
		"issuer error": "?error=access_denied&state=%s",
	} {
		t.Run(name, func(t *testing.T) {
			f := startFlow(t, "http://127.0.0.1:1")
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/auth/callback"+strings.Replace(query, "%s", url.QueryEscape(f.state), 1), nil)
			f.handleCallback(rec, r)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := f.Wait(ctx); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// Only the first callback may produce a result. A browser that retries, or a second forged
// request, must not deliver a second value: the result channel has one reader, so a second
// send would block a goroutine forever.
func TestOnlyTheFirstCallbackDeliversAResult(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"first","refresh_token":"r","expires_in":3600}`))
	}))
	defer issuer.Close()

	f := startFlow(t, issuer.URL)
	good := "/auth/callback?code=c&state=" + url.QueryEscape(f.state)
	for i := 0; i < 5; i++ {
		go f.handleCallback(httptest.NewRecorder(), httptest.NewRequest("GET", good, nil))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := f.Wait(ctx); err != nil {
		t.Fatalf("first callback did not complete: %v", err)
	}
	// A second Wait must time out rather than receive a duplicate result.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := f.Wait(ctx2); err == nil {
		t.Fatal("a second result was delivered; the flow completed twice")
	}
}

// PKCE has to be correct or it protects nothing: the challenge must be the URL-safe,
// unpadded base64 of the SHA-256 of the verifier, and the method must say S256. A plain
// challenge would let anyone who intercepts the code redeem it.
func TestPKCEChallengeMatchesTheVerifier(t *testing.T) {
	f := startFlow(t, "https://auth.example.com")
	u, err := url.Parse(f.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("challenge method is %q; anything but S256 weakens the exchange", got)
	}
	sum := sha256.Sum256([]byte(f.verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := q.Get("code_challenge"); got != want {
		t.Fatalf("challenge does not derive from the verifier\n  got  %s\n  want %s", got, want)
	}
	if strings.Contains(q.Get("code_challenge"), "=") {
		t.Error("challenge is padded; the spec requires unpadded base64url")
	}
	if q.Get("state") == "" || len(q.Get("state")) < 16 {
		t.Errorf("state is %q, too short to be unguessable", q.Get("state"))
	}
}

// Two flows must never share a state or a verifier.
func TestFlowsDoNotRepeatStateOrVerifier(t *testing.T) {
	seenState := map[string]bool{}
	seenVerifier := map[string]bool{}
	for i := 0; i < 20; i++ {
		f := startFlow(t, "https://auth.example.com")
		if seenState[f.state] {
			t.Fatalf("state repeated across flows: %q", f.state)
		}
		if seenVerifier[f.verifier] {
			t.Fatalf("PKCE verifier repeated across flows")
		}
		seenState[f.state] = true
		seenVerifier[f.verifier] = true
		f.Cancel()
	}
}
