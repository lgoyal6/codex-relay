// Package integration detects the user's Codex installation and applies the minimal
// supported configuration, recording every change so it can be rolled back precisely.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// ProviderID is the model_providers key we own. Rollback only ever removes this block.
const ProviderID = "codexrelay"

// Detection is what we found on this machine.
type Detection struct {
	CodexHome    string `json:"codex_home"`
	ConfigPath   string `json:"config_path"`
	ConfigExists bool   `json:"config_exists"`
	CodexOnPath  bool   `json:"codex_on_path"`
	CodexVersion string `json:"codex_version"`
	// ExistingProvider names a model_provider already selected, so we can warn instead of
	// silently taking it over.
	ExistingProvider string `json:"existing_provider"`
	// ManagedByUs is true when our provider block is already present.
	ManagedByUs bool `json:"managed_by_us"`
	// ConflictingTool names another proxy we can see, so migration is explicit.
	ConflictingTool string   `json:"conflicting_tool"`
	Notes           []string `json:"notes"`
}

// CodexHome resolves the Codex configuration directory the same way Codex does.
func CodexHome() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// Detect inspects the installation without modifying anything.
func Detect() Detection {
	d := Detection{CodexHome: CodexHome()}
	if d.CodexHome == "" {
		d.Notes = append(d.Notes, "Could not determine your home directory, so Codex configuration could not be located.")
		return d
	}
	d.ConfigPath = filepath.Join(d.CodexHome, "config.toml")

	raw, err := os.ReadFile(d.ConfigPath)
	if err == nil {
		d.ConfigExists = true
		text := string(raw)
		if m := reModelProvider.FindStringSubmatch(text); m != nil {
			d.ExistingProvider = m[1]
		}
		d.ManagedByUs = strings.Contains(text, "["+providerTable()+"]")
		if d.ExistingProvider != "" && d.ExistingProvider != ProviderID {
			d.ConflictingTool = d.ExistingProvider
			d.Notes = append(d.Notes, fmt.Sprintf(
				"Codex is currently using the model provider %q. Applying codex-relay will change this, and rollback will restore it.", d.ExistingProvider))
		}
	}

	if p, err := lookCodex(); err == nil {
		d.CodexOnPath = true
		d.CodexVersion = p
	} else {
		d.Notes = append(d.Notes, "The `codex` command was not found on PATH. codex-relay can still be configured, but setup cannot verify routing automatically.")
	}
	if runtime.GOOS == "windows" {
		d.Notes = append(d.Notes, "On Windows, a Codex installed inside WSL has its own separate ~/.codex and must be configured from inside WSL.")
	}
	return d
}

var reModelProvider = regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"([^"]+)"`)

func providerTable() string { return "model_providers." + ProviderID }

// Plan is the exact change we propose, shown to the user before anything is written.
type Plan struct {
	ConfigPath string `json:"config_path"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Diff       string `json:"diff"`
	// NoChange is true when the configuration already matches.
	NoChange bool `json:"no_change"`
}

// BuildPlan produces the minimal supported configuration for a proxy at proxyAddr.
//
// The integration point is a model_providers entry, not chatgpt_base_url. That was
// established by testing rather than assumed: with only chatgpt_base_url set, codex-cli
// 0.154.0 still sent generation to chatgpt.com. See docs/compatibility.md.
//
// supportsWebSockets is honest about what this build forwards.
func BuildPlan(configPath, proxyAddr string, supportsWebSockets bool) (Plan, error) {
	var before string
	if raw, err := os.ReadFile(configPath); err == nil {
		before = string(raw)
	} else if !os.IsNotExist(err) {
		return Plan{}, fmt.Errorf("could not read %s: %w", configPath, err)
	}

	after := applyBlock(before, renderKeyBlock(), renderTableBlock(proxyAddr, supportsWebSockets))

	p := Plan{ConfigPath: configPath, Before: before, After: after, NoChange: before == after}
	p.Diff = unifiedDiff(before, after)
	return p, nil
}

// Our edit is written as TWO managed regions, and the reason is a TOML rule with teeth: a
// bare key belongs to whatever table precedes it. Appending `model_provider = "codexrelay"`
// to the end of a file that already contains any table (an MCP server, a profile, another
// provider) silently makes it a key of THAT table. Codex would then never switch provider,
// setup would report success, and the user's table would carry a stray key. That failure was
// reproduced against codex-cli 0.154.0 before this split existed.
//
// So: the bare key goes above the first table header, and our own table goes at the end,
// where the only keys it can absorb are ours.
const beginMarker = "# >>> codex-relay managed block (do not edit inside) >>>"
const endMarker = "# <<< codex-relay managed block <<<"
const beginTableMarker = "# >>> codex-relay managed provider (do not edit inside) >>>"
const endTableMarker = "# <<< codex-relay managed provider <<<"

func renderKeyBlock() string {
	return fmt.Sprintf(`%s
# codex-relay routes Codex turns through a local service so quota rules can apply.
# Remove these blocks, or run `+"`codexrelay rollback`"+`, to restore the previous setting.
model_provider = "%s"
%s`, beginMarker, ProviderID, endMarker)
}

func renderTableBlock(proxyAddr string, ws bool) string {
	return fmt.Sprintf(`%s
[%s]
name = "openai"
base_url = "http://%s/backend-api/codex"
wire_api = "responses"
supports_websockets = %t
requires_openai_auth = true
%s`, beginTableMarker, providerTable(), proxyAddr, ws, endTableMarker)
}

// reTableHeader matches a TOML table or array-of-tables header at the start of a line.
var reTableHeader = regexp.MustCompile(`(?m)^\s*\[`)

// applyBlock inserts or replaces only our two managed regions, leaving everything else alone.
func applyBlock(existing, keyBlock, tableBlock string) string {
	out := disableExistingProvider(existing)
	out = replaceOrInsertKeyBlock(out, keyBlock)
	return replaceOrAppendTableBlock(out, tableBlock)
}

// disableExistingProvider comments out a pre-existing TOP-LEVEL model_provider line rather
// than deleting it, so rollback can restore it and the user can see what changed. A
// model_provider key inside someone's table (a profile, for instance) belongs to that table
// and is left completely alone.
func disableExistingProvider(existing string) string {
	limit := len(existing)
	if loc := reTableHeader.FindStringIndex(existing); loc != nil {
		limit = loc[0]
	}
	head := reModelProvider.ReplaceAllString(existing[:limit], "# codex-relay disabled this line: $0")
	return head + existing[limit:]
}

func replaceOrInsertKeyBlock(existing, block string) string {
	if i := strings.Index(existing, beginMarker); i >= 0 {
		if j := strings.Index(existing[i:], endMarker); j >= 0 {
			end := i + j + len(endMarker)
			return existing[:i] + block + existing[end:]
		}
	}
	// Above the first table header, so the key stays top-level.
	if loc := reTableHeader.FindStringIndex(existing); loc != nil {
		return existing[:loc[0]] + block + "\n\n" + existing[loc[0]:]
	}
	if existing == "" {
		return block + "\n"
	}
	if !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	return existing + "\n" + block + "\n"
}

func replaceOrAppendTableBlock(existing, block string) string {
	if i := strings.Index(existing, beginTableMarker); i >= 0 {
		if j := strings.Index(existing[i:], endTableMarker); j >= 0 {
			end := i + j + len(endTableMarker)
			return existing[:i] + block + existing[end:]
		}
	}
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	if existing != "" {
		existing += "\n"
	}
	return existing + block + "\n"
}

// Sha256 is used to detect third-party edits between apply and rollback.
func Sha256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// BackupPath is where we copy the original before writing.
func BackupPath(configPath string, now time.Time) string {
	return fmt.Sprintf("%s.codexrelay-backup.%s", configPath, now.UTC().Format("20060102-150405"))
}

func unifiedDiff(before, after string) string {
	if before == after {
		return ""
	}
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	var sb strings.Builder
	// A minimal, readable change summary: every line we add and any line we comment out.
	// This is a preview for a person, not a patch to be applied by a machine. Both managed
	// regions are shown: a preview that hid one of them would understate the change.
	for _, line := range b {
		if reModelProvider.MatchString(line) {
			sb.WriteString("- " + line + "\n")
		}
	}
	inBlock := false
	for _, line := range a {
		if strings.Contains(line, beginMarker) || strings.Contains(line, beginTableMarker) {
			if sb.Len() > 0 && inBlock {
				sb.WriteString("\n")
			}
			inBlock = true
		}
		if inBlock {
			sb.WriteString("+ " + line + "\n")
		}
		if strings.Contains(line, endMarker) || strings.Contains(line, endTableMarker) {
			inBlock = false
		}
	}
	return sb.String()
}
