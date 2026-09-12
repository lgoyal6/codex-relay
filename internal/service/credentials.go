package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/oauth"
	"github.com/lgoyal6/codex-relay/internal/secrets"
)

// refreshSkew is how early we refresh before expiry. It matches the client's own five
// minute window so our token is never the one that expires mid-turn.
const refreshSkew = 5 * time.Minute

// CredentialManager hands out usable access tokens, refreshing when needed.
//
// Refresh is serialized PER CREDENTIAL CHAIN. This is not a performance choice: the issuer
// rotates refresh tokens and rejects a reused one, so two concurrent refreshes of the same
// chain would spend the same token twice and permanently break that identity. A per-chain
// mutex plus a re-read after acquiring it means the second caller uses the first caller's
// freshly rotated token instead of racing it.
type CredentialManager struct {
	store  secrets.Store
	clk    clock.Clock
	issuer string

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// cache holds credentials already read from the OS store.
	//
	// This is a latency fix, not a convenience. On macOS the platform store is reached by
	// spawning /usr/bin/security, measured at ~18.6 ms per read (BenchmarkOSGet). Identity
	// resolution happens on every turn, between receiving upstream headers and writing the
	// first byte to the client, so without this cache every turn paid that cost in
	// first-token latency.
	//
	// It does not weaken credential storage. The OS store remains the only durable location;
	// nothing is written to disk here. The token is necessarily in this process's memory
	// while it is being attached to a request anyway. Entries are written through on
	// refresh and dropped by Forget, so a rotated or deleted credential is never served
	// from a stale copy.
	cacheMu sync.RWMutex
	cache   map[string]secrets.Credential
}

func NewCredentialManager(store secrets.Store, clk clock.Clock, issuer string) *CredentialManager {
	return &CredentialManager{
		store: store, clk: clk, issuer: issuer,
		locks: map[string]*sync.Mutex{},
		cache: map[string]secrets.Credential{},
	}
}

// Forget drops a cached credential. Call it whenever the stored credential is removed or
// replaced outside this manager, so nothing is served from a stale copy.
func (m *CredentialManager) Forget(ref string) {
	m.cacheMu.Lock()
	delete(m.cache, ref)
	m.cacheMu.Unlock()
}

func (m *CredentialManager) cached(ref string) (secrets.Credential, bool) {
	m.cacheMu.RLock()
	c, ok := m.cache[ref]
	m.cacheMu.RUnlock()
	return c, ok
}

func (m *CredentialManager) remember(ref string, c secrets.Credential) {
	m.cacheMu.Lock()
	m.cache[ref] = c
	m.cacheMu.Unlock()
}

func (m *CredentialManager) chainLock(ref string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.locks[ref]; ok {
		return l
	}
	l := &sync.Mutex{}
	m.locks[ref] = l
	return l
}

// Access returns a valid access token for a credential reference, refreshing if it is at or
// near expiry. The rotated refresh token is persisted before the new access token is
// returned, so a crash cannot leave us holding a spent token.
func (m *CredentialManager) Access(ctx context.Context, ref string) (secrets.Credential, error) {
	// The fast path: a cached, still-valid credential needs no platform-store round trip.
	if c, ok := m.cached(ref); ok && !m.needsRefresh(c) {
		return c, nil
	}
	cur, err := m.store.Get(ref)
	if err != nil {
		return secrets.Credential{}, err
	}
	if !m.needsRefresh(cur) {
		m.remember(ref, cur)
		return cur, nil
	}

	l := m.chainLock(ref)
	l.Lock()
	defer l.Unlock()

	// Re-read under the lock: another caller may have already rotated this chain. The
	// cache is checked first because that caller wrote through to it.
	if c, ok := m.cached(ref); ok && !m.needsRefresh(c) {
		return c, nil
	}
	cur, err = m.store.Get(ref)
	if err != nil {
		return secrets.Credential{}, err
	}
	if !m.needsRefresh(cur) {
		m.remember(ref, cur)
		return cur, nil
	}
	if cur.RefreshToken == "" {
		return secrets.Credential{}, fmt.Errorf("this workspace has no refresh token stored; sign in again")
	}

	tok, err := oauth.Refresh(ctx, m.issuer, cur.RefreshToken)
	if err != nil {
		return secrets.Credential{}, fmt.Errorf("could not refresh this workspace's sign-in: %w", err)
	}

	next := cur
	next.AccessToken = tok.AccessToken
	next.ExpiresAt = tok.ExpiresAt
	next.ObtainedAt = m.clk.Now()
	if tok.IDToken != "" {
		next.IDToken = tok.IDToken
	}
	// The issuer rotates the refresh token. Persisting the new one is mandatory; dropping
	// it would leave the stored chain pointing at a token that is now spent.
	if tok.RefreshToken != "" {
		next.RefreshToken = tok.RefreshToken
	}
	// Persist before caching: a cached credential that never reached the OS store would be
	// lost on restart while the spent refresh token was already consumed.
	if err := m.store.Set(ref, next); err != nil {
		m.Forget(ref)
		return secrets.Credential{}, fmt.Errorf("refreshed sign-in could not be saved: %w", err)
	}
	m.remember(ref, next)
	return next, nil
}

func (m *CredentialManager) needsRefresh(c secrets.Credential) bool {
	if c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt.IsZero() {
		return false // no expiry information; use it until the upstream rejects it
	}
	return !m.clk.Now().Add(refreshSkew).Before(c.ExpiresAt)
}
