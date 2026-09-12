package secrets

import (
	"testing"
	"time"
)

type countingProbe struct {
	Store
	probes int
	health Health
}

func (c *countingProbe) Probe() Health {
	c.probes++
	return c.health
}

func TestProbeIsReusedWithinTheTTL(t *testing.T) {
	inner := &countingProbe{Store: NewMemory(), health: Health{OK: true, Kind: "test"}}
	c := NewCached(inner)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	for i := 0; i < 20; i++ {
		if h := c.Probe(); !h.OK {
			t.Fatal("probe should report ok")
		}
	}
	if inner.probes != 1 {
		t.Fatalf("the platform store was probed %d times for 20 reads; it must be 1", inner.probes)
	}

	now = now.Add(probeTTL + time.Second)
	_ = c.Probe()
	if inner.probes != 2 {
		t.Fatalf("after the TTL elapsed the store must be probed again, got %d", inner.probes)
	}
}

// TestStoreChangesInvalidateHealth: a credential write or delete is the most likely moment
// for the store's health to have changed, so a stale "ok" must not survive it.
func TestStoreChangesInvalidateHealth(t *testing.T) {
	inner := &countingProbe{Store: NewMemory(), health: Health{OK: true}}
	c := NewCached(inner)
	_ = c.Probe()
	if inner.probes != 1 {
		t.Fatal("setup")
	}
	_ = c.Set("ref", Credential{AccessToken: "t"})
	_ = c.Probe()
	if inner.probes != 2 {
		t.Fatalf("a Set must invalidate cached health, probes=%d", inner.probes)
	}
	_ = c.Delete("ref")
	_ = c.Probe()
	if inner.probes != 3 {
		t.Fatalf("a Delete must invalidate cached health, probes=%d", inner.probes)
	}
}

// TestCredentialReadsAreNeverCachedHere: only health is cached at this layer.
func TestCredentialReadsAreNeverCachedHere(t *testing.T) {
	mem := NewMemory()
	c := NewCached(mem)
	_ = mem.Set("ref", Credential{AccessToken: "first"})
	if got, _ := c.Get("ref"); got.AccessToken != "first" {
		t.Fatalf("got %q", got.AccessToken)
	}
	_ = mem.Set("ref", Credential{AccessToken: "second"})
	if got, _ := c.Get("ref"); got.AccessToken != "second" {
		t.Fatalf("Get must always reach the real store, got %q", got.AccessToken)
	}
}

func TestProbeNowAlwaysProbes(t *testing.T) {
	inner := &countingProbe{Store: NewMemory(), health: Health{OK: true}}
	c := NewCached(inner)
	_ = c.Probe()
	_ = c.ProbeNow()
	_ = c.ProbeNow()
	if inner.probes != 3 {
		t.Fatalf("ProbeNow must bypass the cache every time, probes=%d", inner.probes)
	}
}
