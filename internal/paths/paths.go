// Package paths resolves per-user data locations on each supported OS.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// DataDir is the per-user directory holding the database and logs.
//
// Per-user, not system-wide: this is a personal tool holding personal credentials, and a
// per-user install avoids needing administrator rights.
func DataDir() (string, error) {
	if v := os.Getenv("CODEXRELAY_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine your home directory: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "codex-relay"), nil
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "codex-relay"), nil
		}
		return filepath.Join(home, "AppData", "Local", "codex-relay"), nil
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "codex-relay"), nil
		}
		return filepath.Join(home, ".local", "share", "codex-relay"), nil
	}
}

// EnsureDataDir creates the data directory with owner-only permissions.
func EnsureDataDir() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("could not create %s: %w", dir, err)
	}
	return dir, nil
}

func DatabasePath() (string, error) {
	dir, err := EnsureDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "codex-relay.db"), nil
}
