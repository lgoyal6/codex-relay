package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// lookCodex finds the codex binary and reads its version.
func lookCodex() (string, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Change is a recorded edit, enough to roll back precisely.
type Change struct {
	ID           int64     `json:"id"`
	At           time.Time `json:"at"`
	TargetPath   string    `json:"target_path"`
	BackupPath   string    `json:"backup_path"`
	BeforeSHA256 string    `json:"before_sha256"`
	AfterSHA256  string    `json:"after_sha256"`
	Description  string    `json:"description"`
}

// Recorder persists integration changes so rollback can be precise.
type Recorder interface {
	Record(c Change) error
	Latest(targetPath string) (Change, bool, error)
	MarkRolledBack(id int64, at time.Time) error
}

// Apply writes the plan, after backing up the original.
//
// Ordering: back up first, write second, record third. A crash between write and record is
// still recoverable because the backup file itself carries our marker suffix.
func Apply(p Plan, rec Recorder, now time.Time) (Change, error) {
	if p.NoChange {
		return Change{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(p.ConfigPath), 0o755); err != nil {
		return Change{}, fmt.Errorf("could not create the Codex config directory: %w", err)
	}

	backup := BackupPath(p.ConfigPath, now)
	if p.Before != "" {
		if err := os.WriteFile(backup, []byte(p.Before), 0o600); err != nil {
			return Change{}, fmt.Errorf("could not back up your Codex config: %w", err)
		}
	}
	if err := writeFileAtomic(p.ConfigPath, []byte(p.After), 0o600); err != nil {
		return Change{}, fmt.Errorf("could not write your Codex config: %w", err)
	}

	c := Change{
		At: now, TargetPath: p.ConfigPath, BackupPath: backup,
		BeforeSHA256: Sha256(p.Before), AfterSHA256: Sha256(p.After),
		Description: "added the codex-relay model provider",
	}
	if err := rec.Record(c); err != nil {
		return c, fmt.Errorf("the change was applied but could not be recorded for rollback: %w", err)
	}
	return c, nil
}

// RollbackResult explains exactly what rollback did.
type RollbackResult struct {
	Restored bool   `json:"restored"`
	Message  string `json:"message"`
	// ConflictDetected is true when the file changed after we wrote it, meaning someone
	// else edited it. We then remove only our block rather than overwriting their work.
	ConflictDetected bool `json:"conflict_detected"`
}

// Rollback restores only our changes.
//
// If the file still matches what we wrote, the backup is restored wholesale. If it has
// changed since, a third party (the user, or another tool) edited it, and blindly restoring
// the backup would destroy their edit. In that case we surgically remove our managed block
// and restore any line we commented out, leaving everything else untouched.
func Rollback(rec Recorder, configPath string, now time.Time) (RollbackResult, error) {
	c, ok, err := rec.Latest(configPath)
	if err != nil {
		return RollbackResult{}, err
	}
	if !ok {
		return RollbackResult{Message: "codex-relay has not changed your Codex configuration, so there is nothing to roll back."}, nil
	}

	current, err := os.ReadFile(configPath)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("could not read %s: %w", configPath, err)
	}

	if Sha256(string(current)) == c.AfterSHA256 {
		backup := []byte("")
		if c.BackupPath != "" {
			if b, err := os.ReadFile(c.BackupPath); err == nil {
				backup = b
			}
		}
		if err := writeFileAtomic(configPath, backup, 0o600); err != nil {
			return RollbackResult{}, err
		}
		_ = rec.MarkRolledBack(c.ID, now)
		return RollbackResult{Restored: true,
			Message: "Your Codex configuration was restored exactly as it was before codex-relay changed it."}, nil
	}

	cleaned := removeBlock(string(current))
	if cleaned == string(current) {
		_ = rec.MarkRolledBack(c.ID, now)
		return RollbackResult{ConflictDetected: true,
			Message: "Your Codex configuration no longer contains the codex-relay block, so nothing needed to be removed."}, nil
	}
	if err := writeFileAtomic(configPath, []byte(cleaned), 0o600); err != nil {
		return RollbackResult{}, err
	}
	_ = rec.MarkRolledBack(c.ID, now)
	return RollbackResult{Restored: true, ConflictDetected: true,
		Message: "Your Codex configuration was edited after codex-relay changed it, so only the codex-relay block was removed and your later edits were kept. A copy of the original is at " + c.BackupPath}, nil
}

// removeBlock deletes all managed regions and un-comments any line we disabled.
func removeBlock(s string) string {
	out := cutRegion(s, beginMarker, endMarker)
	out = cutRegion(out, beginTableMarker, endTableMarker)
	out = cutRegion(out, beginAgentsMarker, endAgentsMarker)
	out = strings.ReplaceAll(out, "# codex-relay disabled this line: ", "")
	return strings.TrimLeft(out, "\n")
}

func cutRegion(s, begin, end string) string {
	i := strings.Index(s, begin)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return s
	}
	stop := i + j + len(end)
	// Take the blank line we inserted with the region, so repeated apply/rollback cycles
	// do not accumulate empty lines.
	for stop < len(s) && s[stop] == '\n' {
		stop++
	}
	return s[:i] + s[stop:]
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".codexrelay-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
