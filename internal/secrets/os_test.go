package secrets

import (
	"os"
	"testing"
)

// requireOSStore keeps local development friendly while letting platform CI turn a skipped
// credential-store check into a hard failure. Cross-compilation proves only that the backend
// builds; CODEXRELAY_REQUIRE_OS_KEYRING makes the runner prove a real write/read/delete.
func requireOSStore(t *testing.T) Store {
	t.Helper()
	s := NewOS()
	if h := s.Probe(); !h.OK {
		if os.Getenv("CODEXRELAY_REQUIRE_OS_KEYRING") == "1" {
			t.Fatalf("%s is required for this platform job: %s", h.Kind, h.Detail)
		}
		t.Skipf("credential store unavailable here: %s", h.Detail)
	}
	return s
}
