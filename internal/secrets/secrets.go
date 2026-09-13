// Package secrets stores OAuth credentials in OS-provided credential storage.
//
// The rule this package exists to enforce: we never silently fall back to plaintext. If the
// platform store is unavailable or locked, the caller is told, and the user is given an
// explicit supported setup path instead of a quiet downgrade.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// ServiceName is the keychain/credential-manager service these entries live under.
const ServiceName = "codex-relay"

// ErrUnavailable means the OS credential store could not be used. It is never converted
// into a plaintext write.
var ErrUnavailable = errors.New("os credential storage unavailable")

// ErrNotFound means no credential is stored under that reference.
var ErrNotFound = errors.New("credential not found")

// Credential is one identity's independent OAuth material. It is written only to OS
// credential storage and never to SQLite, logs, diagnostics or the frontend.
type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	AccountID    string    `json:"account_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	ObtainedAt   time.Time `json:"obtained_at"`
}

// Store is the credential backend.
type Store interface {
	Set(ref string, c Credential) error
	Get(ref string) (Credential, error)
	Delete(ref string) error
	// Probe reports whether the backend actually works right now, with a real round trip.
	Probe() Health
	Kind() string
}

// Health is the result of an end-to-end probe of the credential backend.
type Health struct {
	OK     bool   `json:"ok"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	// Remedy is the concrete supported setup path when OK is false.
	Remedy string `json:"remedy,omitempty"`
}

// chunkSize is how much of a credential goes into a single platform-store entry.
//
// This exists because the platform backends impose hard size limits that a REAL OAuth
// credential exceeds, while a short test credential does not:
//
//   - macOS: go-keyring drives /usr/bin/security and caps the whole command line at 4096
//     bytes, with the secret base64-encoded first, so roughly 2.9 KB of payload.
//   - Windows: the password is capped at 2560 bytes.
//   - Linux Secret Service: no library limit.
//
// A ChatGPT credential holds an access token, a refresh token and an id token, all JWTs.
// Together they are commonly 3 to 5 KB, so a single entry fails on macOS and Windows with
// "data passed to Set was too big". That is exactly what a real sign-in hit.
//
// 1024 bytes per chunk is comfortably inside every limit, base64 expansion included, and
// keeps the whole credential in the OS store. The alternative - a key in the keychain and
// ciphertext on disk - would have put token material on the filesystem, which this build
// has said throughout that it does not do.
const chunkSize = 1024

// chunkManifest marks a key whose value is a chunk count rather than a credential.
const chunkManifest = "codexrelay-chunked:v1:"

type osStore struct{}

// NewOS returns the platform credential store: macOS Keychain, Windows Credential Manager,
// or a Secret Service provider on Linux.
func NewOS() Store { return osStore{} }

func (osStore) Kind() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS Keychain"
	case "windows":
		return "Windows Credential Manager"
	case "linux":
		return "Secret Service (libsecret)"
	default:
		return runtime.GOOS + " credential store"
	}
}

func (s osStore) Set(ref string, c Credential) error {
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}

	// Remove any previous chunks first, so shrinking a credential cannot leave a longer
	// stale tail behind that a later read would splice back on.
	s.deleteChunks(ref)

	if len(blob) <= chunkSize {
		if err := keyring.Set(ServiceName, ref, string(blob)); err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return nil
	}

	parts := split(string(blob), chunkSize)
	// Write the parts BEFORE the manifest. A crash midway then leaves a manifest-free set of
	// orphan chunks, which reads as "no credential", rather than a manifest pointing at
	// chunks that do not exist, which would read as a corrupt one.
	for i, part := range parts {
		if err := keyring.Set(ServiceName, chunkRef(ref, i), part); err != nil {
			s.deleteChunks(ref)
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}
	manifest := fmt.Sprintf("%s%d", chunkManifest, len(parts))
	if err := keyring.Set(ServiceName, ref, manifest); err != nil {
		s.deleteChunks(ref)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func chunkRef(ref string, i int) string { return fmt.Sprintf("%s#%d", ref, i) }

func split(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

// deleteChunks removes every chunk belonging to ref. It stops at the first missing index,
// which is safe because chunks are always written contiguously from zero.
func (s osStore) deleteChunks(ref string) {
	for i := 0; i < 4096; i++ {
		if _, err := keyring.Get(ServiceName, chunkRef(ref, i)); err != nil {
			return
		}
		_ = keyring.Delete(ServiceName, chunkRef(ref, i))
	}
}

func (s osStore) Get(ref string) (Credential, error) {
	raw, err := keyring.Get(ServiceName, ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	if count, ok := strings.CutPrefix(raw, chunkManifest); ok {
		n, convErr := strconv.Atoi(count)
		if convErr != nil || n <= 0 {
			return Credential{}, fmt.Errorf("stored credential index is unreadable")
		}
		var sb strings.Builder
		for i := 0; i < n; i++ {
			part, perr := keyring.Get(ServiceName, chunkRef(ref, i))
			if errors.Is(perr, keyring.ErrNotFound) {
				// An incomplete credential must not be returned half-formed.
				return Credential{}, fmt.Errorf(
					"stored credential is incomplete (part %d of %d is missing); sign in again", i+1, n)
			}
			if perr != nil {
				return Credential{}, fmt.Errorf("%w: %v", ErrUnavailable, perr)
			}
			sb.WriteString(part)
		}
		raw = sb.String()
	}

	var c Credential
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Credential{}, fmt.Errorf("stored credential is unreadable: %w", err)
	}
	return c, nil
}

func (s osStore) Delete(ref string) error {
	s.deleteChunks(ref)
	err := keyring.Delete(ServiceName, ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Probe performs a real write, read-back and delete. Reporting "available" without a round
// trip would be exactly the kind of unverified claim this build is not allowed to make.
func (s osStore) Probe() Health {
	h := Health{Kind: s.Kind()}
	const ref = "codex-relay-probe"
	want := Credential{AccessToken: "probe", ObtainedAt: time.Unix(0, 0).UTC()}
	if err := s.Set(ref, want); err != nil {
		h.Detail = err.Error()
		h.Remedy = remedy()
		return h
	}
	got, err := s.Get(ref)
	_ = s.Delete(ref)
	if err != nil {
		h.Detail = err.Error()
		h.Remedy = remedy()
		return h
	}
	if got.AccessToken != want.AccessToken {
		h.Detail = "the credential store returned a different value than was written"
		h.Remedy = remedy()
		return h
	}
	h.OK = true
	h.Detail = "verified by writing, reading back and deleting a probe entry"
	return h
}

func remedy() string {
	switch runtime.GOOS {
	case "linux":
		return "Install and unlock a Secret Service provider (for example gnome-keyring or KeepassXC with Secret Service enabled), then reconnect. Credentials are not written to disk in plaintext, so accounts cannot be connected until this is available."
	case "darwin":
		return "Unlock your login keychain and allow codex-relay to store items, then reconnect."
	case "windows":
		return "Sign in to your Windows user profile so Credential Manager is available, then reconnect."
	default:
		return "No supported OS credential store was found on this platform."
	}
}

// Memory is an in-process store used by tests only. It is never selected at runtime.
//
// It is mutex-guarded because it stands in for the platform store in tests, and the code
// under test refreshes credentials from several goroutines at once. An unguarded map here
// does not describe a production defect, but it reports a race of its own and drowns out the
// one the test is actually looking for.
type Memory struct {
	mu sync.Mutex
	m  map[string]Credential
}

func NewMemory() *Memory { return &Memory{m: map[string]Credential{}} }

func (s *Memory) Kind() string { return "in-memory (test only)" }
func (s *Memory) Set(ref string, c Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[ref] = c
	return nil
}
func (s *Memory) Get(ref string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[ref]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return c, nil
}
func (s *Memory) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ref)
	return nil
}
func (s *Memory) Probe() Health {
	return Health{OK: true, Kind: s.Kind(), Detail: "in-memory store, for tests"}
}
