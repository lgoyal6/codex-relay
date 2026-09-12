package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/secrets"
)

// countingStore wraps a store and counts reads, so a test can assert how many times the OS
// credential store was actually consulted.
type countingStore struct {
	inner *secrets.Memory
	gets  int64
}

func (c *countingStore) Kind() string { return c.inner.Kind() }
func (c *countingStore) Set(ref string, cr secrets.Credential) error {
	return c.inner.Set(ref, cr)
}
func (c *countingStore) Get(ref string) (secrets.Credential, error) {
	atomic.AddInt64(&c.gets, 1)
	return c.inner.Get(ref)
}
func (c *countingStore) Delete(ref string) error { return c.inner.Delete(ref) }
func (c *countingStore) Probe() secrets.Health   { return c.inner.Probe() }

// TestValidCredentialIsReadFromTheStoreOnce is the latency fix. On macOS each store read
// spawns /usr/bin/security at roughly 18 ms, and this happens on the first-token path of
// every turn, so repeated reads were directly visible as added latency.
func TestValidCredentialIsReadFromTheStoreOnce(t *testing.T) {
	st := &countingStore{inner: secrets.NewMemory()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = st.Set("ws1", secrets.Credential{AccessToken: "good", RefreshToken: "r", ExpiresAt: now.Add(time.Hour)})
	m := NewCredentialManager(st, clock.NewFake(now), "")

	for i := 0; i < 50; i++ {
		got, err := m.Access(context.Background(), "ws1")
		if err != nil || got.AccessToken != "good" {
			t.Fatalf("read %d: %q %v", i, got.AccessToken, err)
		}
	}
	if n := atomic.LoadInt64(&st.gets); n != 1 {
		t.Fatalf("the credential store was consulted %d times for 50 turns; it must be 1", n)
	}
}

// TestForgetDropsTheCachedCopy: a removed or reconnected workspace must never keep serving
// the old token out of memory.
func TestForgetDropsTheCachedCopy(t *testing.T) {
	st := &countingStore{inner: secrets.NewMemory()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = st.Set("ws1", secrets.Credential{AccessToken: "old", RefreshToken: "r", ExpiresAt: now.Add(time.Hour)})
	m := NewCredentialManager(st, clock.NewFake(now), "")

	if got, _ := m.Access(context.Background(), "ws1"); got.AccessToken != "old" {
		t.Fatalf("got %q", got.AccessToken)
	}
	// Replace the stored credential behind the manager's back, as a reconnect does.
	_ = st.Set("ws1", secrets.Credential{AccessToken: "new", RefreshToken: "r", ExpiresAt: now.Add(time.Hour)})
	if got, _ := m.Access(context.Background(), "ws1"); got.AccessToken != "old" {
		t.Fatal("precondition: without Forget the cache is expected to still hold the old value")
	}
	m.Forget("ws1")
	got, err := m.Access(context.Background(), "ws1")
	if err != nil || got.AccessToken != "new" {
		t.Fatalf("after Forget the fresh credential must be read: %q %v", got.AccessToken, err)
	}
}

// TestDeletedCredentialIsNotServedFromCache guards the removal path specifically.
func TestDeletedCredentialIsNotServedFromCache(t *testing.T) {
	st := &countingStore{inner: secrets.NewMemory()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = st.Set("ws1", secrets.Credential{AccessToken: "tok", RefreshToken: "r", ExpiresAt: now.Add(time.Hour)})
	m := NewCredentialManager(st, clock.NewFake(now), "")
	_, _ = m.Access(context.Background(), "ws1")

	m.Forget("ws1")
	_ = st.Delete("ws1")
	if _, err := m.Access(context.Background(), "ws1"); err == nil {
		t.Fatal("a deleted credential must not be served from the cache")
	}
}

// TestExpiredCachedCredentialStillRefreshes: the cache must not defeat expiry.
func TestExpiredCachedCredentialStillRefreshes(t *testing.T) {
	ri := newRotatingIssuer("seed")
	defer ri.handler.Close()
	st := &countingStore{inner: secrets.NewMemory()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := clock.NewFake(now)
	_ = st.Set("ws1", secrets.Credential{AccessToken: "old", RefreshToken: "seed", ExpiresAt: now.Add(time.Hour)})
	m := NewCredentialManager(st, fake, ri.handler.URL)

	if got, _ := m.Access(context.Background(), "ws1"); got.AccessToken != "old" {
		t.Fatalf("got %q", got.AccessToken)
	}
	fake.Advance(2 * time.Hour) // now past expiry
	got, err := m.Access(context.Background(), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "access-1" {
		t.Fatalf("an expired cached credential must be refreshed, got %q", got.AccessToken)
	}
	stored, _ := st.Get("ws1")
	if stored.RefreshToken != "refresh-1" {
		t.Fatalf("the rotated refresh token must still be persisted, got %q", stored.RefreshToken)
	}
}
