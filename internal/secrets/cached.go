package secrets

import (
	"sync"
	"time"
)

// probeTTL is how long a credential-storage health result is reused.
//
// Probing is not free: it is a real write, read-back and delete against the platform store,
// which on macOS means three /usr/bin/security process spawns, measured at roughly 18 ms
// each. The dashboard reads service state on a poll, so probing on every read cost about
// 55 ms per poll and churned the keychain continuously for an answer that changes rarely.
const probeTTL = 30 * time.Second

// Cached wraps a Store and reuses a recent Probe result.
//
// Only Probe is cached. Get, Set and Delete always go to the real store, because those carry
// credentials and must never be served from a stale copy at this layer.
type Cached struct {
	inner Store

	mu   sync.Mutex
	last Health
	at   time.Time
	now  func() time.Time
}

func NewCached(inner Store) *Cached {
	return &Cached{inner: inner, now: time.Now}
}

func (c *Cached) Kind() string                       { return c.inner.Kind() }
func (c *Cached) Get(ref string) (Credential, error) { return c.inner.Get(ref) }

// Set and Delete invalidate the cached health, because a failure or recovery there is the
// most likely reason the answer has changed.
func (c *Cached) Set(ref string, cr Credential) error {
	err := c.inner.Set(ref, cr)
	c.invalidate()
	return err
}

func (c *Cached) Delete(ref string) error {
	err := c.inner.Delete(ref)
	c.invalidate()
	return err
}

func (c *Cached) invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// Probe returns a recent health result, refreshing it when it is older than probeTTL.
func (c *Cached) Probe() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && c.now().Sub(c.at) < probeTTL {
		return c.last
	}
	h := c.inner.Probe()
	c.last, c.at = h, c.now()
	return h
}

// ProbeNow forces a fresh probe, for the diagnostics report where the point is to test the
// store right now rather than to render a page quickly.
func (c *Cached) ProbeNow() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.inner.Probe()
	c.last, c.at = h, c.now()
	return h
}

var _ Store = (*Cached)(nil)
