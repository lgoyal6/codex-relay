package secrets

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// split cuts by BYTES, and the platform store holds strings. If a chunk ends in the middle of
// a multi-byte rune, a store that validates or re-encodes UTF-8 can replace the broken pair
// with U+FFFD, and the credential is destroyed on write with no error. Losing a credential
// silently is the worst failure this package has, so the boundary is swept rather than
// sampled: every offset where a 3-byte rune can straddle the chunk edge.
func TestMultiByteRunesStraddlingAChunkBoundaryRoundTrip(t *testing.T) {
	s := requireOSStore(t)
	const ref = "utf8-boundary-test"
	t.Cleanup(func() { _ = s.Delete(ref) })

	for pad := chunkSize - 8; pad <= chunkSize+8; pad++ {
		// "…" is three bytes; at some pad length it lands across the chunk edge.
		token := strings.Repeat("a", pad) + "…" + strings.Repeat("b", 40)
		want := Credential{AccessToken: token, AccountID: "acct"}
		if err := s.Set(ref, want); err != nil {
			t.Fatalf("pad %d: set failed: %v", pad, err)
		}
		got, err := s.Get(ref)
		if err != nil {
			t.Fatalf("pad %d: get failed: %v", pad, err)
		}
		if got.AccessToken != want.AccessToken {
			t.Fatalf("pad %d: credential corrupted across the chunk boundary\n  want %d bytes, valid utf8=%v\n  got  %d bytes, valid utf8=%v",
				pad, len(want.AccessToken), utf8.ValidString(want.AccessToken),
				len(got.AccessToken), utf8.ValidString(got.AccessToken))
		}
	}
}

// split must be lossless and must never produce an empty trailing piece, because the reader
// counts chunks and stops at the first gap. Checked over every length around the boundary,
// not just the ones chosen by hand.
func TestSplitIsLosslessAtEveryLengthNearTheBoundary(t *testing.T) {
	for n := 0; n <= 3*chunkSize+5; n++ {
		in := strings.Repeat("z", n)
		parts := split(in, chunkSize)
		if joined := strings.Join(parts, ""); joined != in {
			t.Fatalf("length %d: split lost data (%d bytes back)", n, len(joined))
		}
		for i, p := range parts {
			if len(p) > chunkSize {
				t.Fatalf("length %d: part %d is %d bytes, over the store's limit", n, i, len(p))
			}
			if p == "" && n > 0 {
				t.Fatalf("length %d: produced an empty part at %d, which reads as a gap", n, i)
			}
		}
	}
}

// A credential whose own contents look like the chunk manifest must not be mistaken for one.
// The manifest and a real credential share a key, so the only thing separating them is the
// prefix; a value that imitates it is the obvious confusion.
func TestCredentialThatLooksLikeAManifestIsNotMisread(t *testing.T) {
	s := requireOSStore(t)
	const ref = "manifest-lookalike-test"
	t.Cleanup(func() { _ = s.Delete(ref) })

	want := Credential{AccessToken: chunkManifest + "9999", AccountID: "acct"}
	if err := s.Set(ref, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ref)
	if err != nil {
		t.Fatalf("a credential containing the manifest prefix could not be read back: %v", err)
	}
	if got.AccessToken != want.AccessToken {
		t.Fatalf("got %q, want %q", got.AccessToken, want.AccessToken)
	}
}
