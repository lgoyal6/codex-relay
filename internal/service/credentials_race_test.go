package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/secrets"
)

// tokenServer stands in for the issuer and counts how many times a refresh is actually
// performed. It rotates the refresh token on every call, like the real issuer, and refuses a
// token it has already spent.
func tokenServer(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var calls, reuse atomic.Int64
	var mu sync.Mutex
	spent := map[string]bool{}
	gen := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		rt := r.Form.Get("refresh_token")

		mu.Lock()
		if spent[rt] {
			reuse.Add(1)
			mu.Unlock()
			// This is what the real issuer does with a spent single-use token.
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		spent[rt] = true
		gen++
		// Snapshot the counter while still holding the lock; reading it after unlocking is
		// itself a race, and the detector correctly called that out.
		n := gen
		next := fmt.Sprintf("refresh-gen-%d", n)
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("access-gen-%d", n),
			"refresh_token": next,
			"id_token":      "",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &reuse
}

// The single most damaging bug this codebase could have: two concurrent turns each refreshing
// the same chain. The issuer rotates refresh tokens and refuses a spent one, so a second
// refresh does not merely waste a call, it burns the chain and locks the account out until
// the user signs in again.
//
// Forty concurrent callers on one expired credential must produce exactly one refresh.
func TestConcurrentAccessRefreshesAChainExactlyOnce(t *testing.T) {
	srv, calls, reuse := tokenServer(t)
	sec := secrets.NewMemory()
	clk := clock.System()
	m := NewCredentialManager(sec, clk, srv.URL)

	const ref = "workspace/acct:ws"
	if err := sec.Set(ref, secrets.Credential{
		AccessToken:  "stale",
		RefreshToken: "refresh-gen-0",
		ExpiresAt:    time.Now().Add(-time.Hour), // already expired
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	const racers = 40
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	tokens := make(chan string, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := m.Access(context.Background(), ref)
			if err != nil {
				errs <- err
				return
			}
			tokens <- c.AccessToken
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(tokens)

	for err := range errs {
		t.Errorf("a caller failed while another was refreshing: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the issuer was called %d times for one expired chain; every call past the first spends a rotated token", n)
	}
	if n := reuse.Load(); n != 0 {
		t.Fatalf("a spent refresh token was replayed %d times; the chain would be dead", n)
	}

	// Every caller must come away with the SAME refreshed credential.
	first := ""
	for tok := range tokens {
		if first == "" {
			first = tok
		}
		if tok != first {
			t.Fatalf("callers got different access tokens (%q and %q) from one refresh", first, tok)
		}
	}
	if first != "access-gen-1" {
		t.Errorf("callers got %q, want the refreshed token", first)
	}

	// And the rotated refresh token must be what is persisted, not the spent one.
	stored, err := sec.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "refresh-gen-1" {
		t.Fatalf("stored refresh token is %q; the spent one would lock the account out", stored.RefreshToken)
	}
}

// Different chains must refresh in parallel. Serialising all of them behind one lock would
// make every workspace wait on the slowest, on the path that produces a turn's first token.
func TestSeparateChainsRefreshIndependently(t *testing.T) {
	srv, calls, _ := tokenServer(t)
	sec := secrets.NewMemory()
	m := NewCredentialManager(sec, clock.System(), srv.URL)

	refs := []string{"workspace/a:1", "workspace/b:2", "workspace/c:3"}
	for i, ref := range refs {
		if err := sec.Set(ref, secrets.Credential{
			AccessToken:  "stale",
			RefreshToken: fmt.Sprintf("chain-%d-token", i),
			ExpiresAt:    time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, ref := range refs {
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			if _, err := m.Access(context.Background(), ref); err != nil {
				t.Errorf("%s: %v", ref, err)
			}
		}(ref)
	}
	wg.Wait()
	if n := calls.Load(); n != int64(len(refs)) {
		t.Fatalf("issuer called %d times for %d independent chains", n, len(refs))
	}
}

// A failed refresh must not leave a cached credential behind that a later caller would treat
// as good, and it must not silently succeed.
func TestAFailedRefreshDoesNotPoisonTheCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	sec := secrets.NewMemory()
	m := NewCredentialManager(sec, clock.System(), srv.URL)
	const ref = "workspace/dead:chain"
	if err := sec.Set(ref, secrets.Credential{
		AccessToken: "stale", RefreshToken: "spent",
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := m.Access(context.Background(), ref); err == nil {
			t.Fatalf("attempt %d: a dead chain reported success", i)
		}
	}
}
