package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/secrets"
)

// rotatingIssuer models the real contract: each refresh token may be spent exactly once and
// returns a NEW one. Reusing a spent token is an error, exactly as the issuer behaves
// (evidenced by the client's own "refresh token was already used" failure reason).
type rotatingIssuer struct {
	mu      sync.Mutex
	valid   map[string]bool
	spent   map[string]bool
	issued  int64
	reuses  int64
	handler *httptest.Server
}

func newRotatingIssuer(seed string) *rotatingIssuer {
	ri := &rotatingIssuer{valid: map[string]bool{seed: true}, spent: map[string]bool{}}
	ri.handler = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got := r.Form.Get("refresh_token")
		ri.mu.Lock()
		defer ri.mu.Unlock()
		if ri.spent[got] {
			atomic.AddInt64(&ri.reuses, 1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !ri.valid[got] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ri.spent[got] = true
		delete(ri.valid, got)
		n := atomic.AddInt64(&ri.issued, 1)
		next := "refresh-" + itoa(int(n))
		ri.valid[next] = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-" + itoa(int(n)),
			"refresh_token": next,
			"expires_in":    3600,
		})
	}))
	return ri
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestRefreshPersistsTheRotatedToken(t *testing.T) {
	ri := newRotatingIssuer("seed")
	defer ri.handler.Close()

	st := secrets.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := clock.NewFake(now)
	_ = st.Set("ws1", secrets.Credential{
		AccessToken: "old", RefreshToken: "seed", ExpiresAt: now.Add(time.Minute), // inside the skew
	})
	m := NewCredentialManager(st, fake, ri.handler.URL)

	got, err := m.Access(context.Background(), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "access-1" {
		t.Fatalf("access token = %q", got.AccessToken)
	}
	stored, _ := st.Get("ws1")
	if stored.RefreshToken != "refresh-1" {
		t.Fatalf("the rotated refresh token must be persisted, stored %q", stored.RefreshToken)
	}

	// A second refresh must use the rotated token, not the spent seed.
	fake.Advance(2 * time.Hour)
	if _, err := m.Access(context.Background(), "ws1"); err != nil {
		t.Fatalf("second refresh failed, which means we replayed a spent token: %v", err)
	}
	if n := atomic.LoadInt64(&ri.reuses); n != 0 {
		t.Fatalf("a spent refresh token was reused %d times", n)
	}
}

// TestConcurrentRefreshSpendsTheTokenOnce is the property that makes independent credentials
// survivable. Without per-chain serialization, N concurrent turns would each try to spend the
// same refresh token and all but one would permanently break the chain.
func TestConcurrentRefreshSpendsTheTokenOnce(t *testing.T) {
	ri := newRotatingIssuer("seed")
	defer ri.handler.Close()

	st := secrets.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = st.Set("ws1", secrets.Credential{AccessToken: "old", RefreshToken: "seed", ExpiresAt: now})
	m := NewCredentialManager(st, clock.NewFake(now), ri.handler.URL)

	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Access(context.Background(), "ws1")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&ri.issued); got != 1 {
		t.Fatalf("refresh ran %d times for one chain; it must be serialized to exactly 1", got)
	}
	if got := atomic.LoadInt64(&ri.reuses); got != 0 {
		t.Fatalf("a spent refresh token was replayed %d times", got)
	}
}

func TestValidTokenIsNotRefreshed(t *testing.T) {
	ri := newRotatingIssuer("seed")
	defer ri.handler.Close()
	st := secrets.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = st.Set("ws1", secrets.Credential{AccessToken: "fresh", RefreshToken: "seed", ExpiresAt: now.Add(time.Hour)})
	m := NewCredentialManager(st, clock.NewFake(now), ri.handler.URL)

	got, err := m.Access(context.Background(), "ws1")
	if err != nil || got.AccessToken != "fresh" {
		t.Fatalf("a valid token must be used as-is: %q %v", got.AccessToken, err)
	}
	if atomic.LoadInt64(&ri.issued) != 0 {
		t.Fatal("no refresh should have happened")
	}
}
