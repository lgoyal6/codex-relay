package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KnownTool describes another Codex router we can recognise in a config.
type KnownTool struct {
	ProviderID string
	Name       string
	DataDir    string
}

// knownTools are the routers we can detect. Detection is by the provider id the tool writes
// into config.toml, which is the only thing it reliably owns.
var knownTools = []KnownTool{
	{ProviderID: "codex-lb", Name: "codex-lb", DataDir: ".codex-lb"},
}

// MigrationPlan is what a migration would do, presented before anything is changed.
type MigrationPlan struct {
	From       string `json:"from"`
	Detected   bool   `json:"detected"`
	ConfigPath string `json:"config_path"`
	BackupPath string `json:"backup_path"`
	// Steps are what codex-relay will do.
	Steps []string `json:"steps"`
	// Manual are the things a person must do that no tool can do for them.
	Manual []string `json:"manual"`
	// Incompatible names settings the other tool has that codex-relay does not carry over.
	Incompatible []string `json:"incompatible"`
	Diff         string   `json:"diff"`
}

// PlanMigration inspects the Codex config for another router and describes the move.
//
// It deliberately never reads, copies, or reuses the other tool's credentials. Codex's and
// codex-lb's refresh tokens rotate and are single-use, so importing one would break whichever
// holder refreshed second. Re-authentication in the browser is a required manual step, not an
// inconvenience we could engineer away.
func PlanMigration(configPath, proxyAddr string, supportsWebSockets bool) (MigrationPlan, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return MigrationPlan{}, fmt.Errorf("could not read %s: %w", configPath, err)
	}
	text := string(raw)

	p := MigrationPlan{ConfigPath: configPath}
	for _, tool := range knownTools {
		if !strings.Contains(text, "model_providers."+tool.ProviderID) &&
			!strings.Contains(text, `"`+tool.ProviderID+`"`) {
			continue
		}
		p.Detected = true
		p.From = tool.Name

		p.Steps = append(p.Steps,
			fmt.Sprintf("Back up %s before any change.", configPath),
			fmt.Sprintf("Point Codex at codex-relay on %s, leaving the %s provider block in place so its own rollback still works.", proxyAddr, tool.Name),
			"Record the change so `codexrelay rollback` can undo exactly this and nothing else.",
		)
		p.Manual = append(p.Manual,
			fmt.Sprintf("Sign in to each workspace again in codex-relay. %s's stored sign-ins are NOT copied: refresh tokens rotate and are single-use, so sharing one would break both tools.", tool.Name),
			fmt.Sprintf("Stop %s if you do not want it running alongside, and remove its background service yourself. codex-relay does not touch it.", tool.Name),
		)
		if home, herr := os.UserHomeDir(); herr == nil {
			dir := filepath.Join(home, tool.DataDir)
			if _, serr := os.Stat(dir); serr == nil {
				p.Manual = append(p.Manual,
					fmt.Sprintf("%s's data stays at %s. codex-relay never reads or deletes it; remove it yourself when you are satisfied.", tool.Name, dir))
			}
		}
		p.Incompatible = incompatibleSettings(text, tool)
		break
	}

	if !p.Detected {
		return p, nil
	}
	plan, err := BuildPlan(configPath, proxyAddr, supportsWebSockets)
	if err != nil {
		return p, err
	}
	p.Diff = plan.Diff
	return p, nil
}

// incompatibleSettings names configuration the other tool supports that codex-relay does not,
// so nobody assumes it silently carried over.
func incompatibleSettings(text string, tool KnownTool) []string {
	var out []string
	checks := []struct{ needle, note string }{
		{"http_responses_session_bridge", tool.Name + " session bridging is not carried over; codex-relay has no equivalent in this release."},
		{"CODEX_LB_DATABASE_URL", tool.Name + " can use PostgreSQL; codex-relay uses a local SQLite file only."},
		{"dashboard_auth", tool.Name + " dashboard authentication has no equivalent: codex-relay binds to loopback and uses a per-session token instead."},
		{"firewall", tool.Name + " firewall and remote-access settings are out of scope; codex-relay is loopback only."},
	}
	for _, c := range checks {
		if strings.Contains(text, c.needle) {
			out = append(out, c.note)
		}
	}
	return out
}

// UninstallPlan describes exactly what removing codex-relay touches.
type UninstallPlan struct {
	ConfigPath       string   `json:"config_path"`
	ConfigWillRevert bool     `json:"config_will_revert"`
	DataDir          string   `json:"data_dir"`
	WorkspaceCount   int      `json:"workspace_count"`
	BoundThreads     int      `json:"bound_threads"`
	Effects          []string `json:"effects"`
	Keeps            []string `json:"keeps"`
}

// DescribeUninstall explains retention and credential effects BEFORE anything is removed.
func DescribeUninstall(configPath, dataDir string, workspaces, boundThreads int, managed bool) UninstallPlan {
	p := UninstallPlan{
		ConfigPath:       configPath,
		ConfigWillRevert: managed,
		DataDir:          dataDir,
		WorkspaceCount:   workspaces,
		BoundThreads:     boundThreads,
	}
	if managed {
		p.Effects = append(p.Effects,
			"Your Codex configuration is restored to what it was before codex-relay changed it, and Codex goes back to whatever provider it used before.")
	} else {
		p.Effects = append(p.Effects,
			"Your Codex configuration does not currently contain a codex-relay block, so nothing there needs undoing.")
	}
	if workspaces > 0 {
		p.Effects = append(p.Effects,
			fmt.Sprintf("%d stored sign-in(s) are deleted from your OS credential store. That cannot be undone; you would sign in again to reconnect.", workspaces))
	}
	if boundThreads > 0 {
		p.Effects = append(p.Effects,
			fmt.Sprintf("%d conversation(s) are currently bound to a pooled workspace. After removal they cannot continue through codex-relay; start new conversations for them.", boundThreads))
	}
	p.Effects = append(p.Effects,
		fmt.Sprintf("codex-relay's own data at %s is deleted: quota readings, rules, ownership records and history.", dataDir))
	p.Keeps = append(p.Keeps,
		"Your ChatGPT accounts and their real usage are untouched. Removing codex-relay does not sign you out of ChatGPT or Codex.",
		"Anything another tool owns is left alone, including its configuration block and its data directory.",
		"Codex's own sign-in is untouched: codex-relay never read or replaced it.")
	return p
}
