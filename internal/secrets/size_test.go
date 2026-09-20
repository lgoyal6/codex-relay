package secrets

import (
	"strings"
	"testing"
	"time"
)

// realisticCredential is the size a REAL ChatGPT OAuth chain produces: three JWTs, commonly
// 3 to 5 KB in total. Every earlier test in this package used short synthetic strings, which
// is precisely why a hard size limit in the platform backend survived until a real sign-in
// failed. This test exists so that cannot happen again.
func realisticCredential() Credential {
	return Credential{
		AccessToken:  "eyJ" + strings.Repeat("a", 1600),
		RefreshToken: strings.Repeat("r", 600),
		IDToken:      "eyJ" + strings.Repeat("i", 1200),
		AccountID:    "acct_user-EXAMPLE000000000000000000",
		ExpiresAt:    time.Unix(1789106944, 0).UTC(),
		ObtainedAt:   time.Unix(1789100000, 0).UTC(),
	}
}

func TestOSStoreRoundTripsARealisticCredential(t *testing.T) {
	s := requireOSStore(t)
	const ref = "realistic-size-test"
	t.Cleanup(func() { _ = s.Delete(ref) })

	want := realisticCredential()
	if err := s.Set(ref, want); err != nil {
		t.Fatalf("storing a realistically sized credential failed: %v", err)
	}
	got, err := s.Get(ref)
	if err != nil {
		t.Fatalf("reading it back failed: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken ||
		got.IDToken != want.IDToken || got.AccountID != want.AccountID {
		t.Fatal("the credential did not survive the round trip intact")
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("expiry drifted: %v vs %v", got.ExpiresAt, want.ExpiresAt)
	}
}

// TestOSStoreHandlesSizesAcrossTheChunkBoundary walks sizes either side of the chunk size,
// because off-by-one splitting is the obvious way to break this.
func TestOSStoreHandlesSizesAcrossTheChunkBoundary(t *testing.T) {
	s := requireOSStore(t)
	const ref = "chunk-boundary-test"
	t.Cleanup(func() { _ = s.Delete(ref) })

	for _, n := range []int{0, 1, chunkSize - 200, chunkSize, chunkSize + 1, 2*chunkSize - 1, 2 * chunkSize, 5 * chunkSize} {
		want := Credential{AccessToken: strings.Repeat("x", n), AccountID: "a"}
		if err := s.Set(ref, want); err != nil {
			t.Fatalf("token of %d bytes: set failed: %v", n, err)
		}
		got, err := s.Get(ref)
		if err != nil {
			t.Fatalf("token of %d bytes: get failed: %v", n, err)
		}
		if got.AccessToken != want.AccessToken {
			t.Fatalf("token of %d bytes: round trip corrupted the value (got %d bytes)", n, len(got.AccessToken))
		}
	}
}

// TestShrinkingACredentialLeavesNoStaleTail: a long credential replaced by a short one must
// not leave orphan chunks that a later read could splice back on.
func TestShrinkingACredentialLeavesNoStaleTail(t *testing.T) {
	s := requireOSStore(t)
	const ref = "shrink-test"
	t.Cleanup(func() { _ = s.Delete(ref) })

	long := Credential{AccessToken: strings.Repeat("L", 5000), AccountID: "a"}
	if err := s.Set(ref, long); err != nil {
		t.Fatalf("set long: %v", err)
	}
	short := Credential{AccessToken: "short", AccountID: "a"}
	if err := s.Set(ref, short); err != nil {
		t.Fatalf("set short: %v", err)
	}
	got, err := s.Get(ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessToken != "short" {
		t.Fatalf("expected the short value, got %d bytes", len(got.AccessToken))
	}
}

// TestDeleteRemovesEveryChunk: a removed workspace must leave nothing behind in the OS store.
func TestDeleteRemovesEveryChunk(t *testing.T) {
	s := requireOSStore(t)
	const ref = "delete-chunks-test"
	if err := s.Set(ref, realisticCredential()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Delete(ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ref); err == nil {
		t.Fatal("the credential is still readable after delete")
	}
	// No chunk may survive either.
	if _, err := s.Get(ref + "#0"); err == nil {
		t.Fatal("a chunk survived delete, so token material was left in the OS store")
	}
}

func TestSplitCoversTheWholeString(t *testing.T) {
	for _, n := range []int{0, 1, 7, 1023, 1024, 1025, 4097} {
		in := strings.Repeat("z", n)
		parts := split(in, chunkSize)
		if joined := strings.Join(parts, ""); joined != in {
			t.Fatalf("split/join lost data at %d bytes: got %d", n, len(joined))
		}
		for i, p := range parts {
			if len(p) > chunkSize {
				t.Fatalf("part %d is %d bytes, over the chunk size", i, len(p))
			}
		}
	}
}
