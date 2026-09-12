package secrets

import (
	"testing"
	"time"
)

// BenchmarkOSGet measures a single credential read from the OS credential store. On macOS
// go-keyring shells out to /usr/bin/security, so this is a process spawn, not a memory read.
func BenchmarkOSGet(b *testing.B) {
	s := NewOS()
	ref := "bench-probe"
	if err := s.Set(ref, Credential{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		b.Skipf("credential store unavailable: %v", err)
	}
	b.Cleanup(func() { _ = s.Delete(ref) })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Get(ref); err != nil {
			b.Fatal(err)
		}
	}
}
