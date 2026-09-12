package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMigrationDetectsCodexLBAndRequiresReauthentication is the contract's migration rule in
// code: detect the other tool, back up, and REQUIRE independent browser re-authentication
// rather than copying its sign-ins. Refresh tokens rotate and are single-use, so importing
// one would break whichever holder refreshed second.
func TestMigrationDetectsCodexLBAndRequiresReauthentication(t *testing.T) {
	cfg := writeCfg(t, `model_provider = "codex-lb"

[model_providers.codex-lb]
name = "openai"
base_url = "http://127.0.0.1:2455/backend-api/codex"
`)
	p, err := PlanMigration(cfg, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Detected || p.From != "codex-lb" {
		t.Fatalf("codex-lb was not detected: %+v", p)
	}
	joined := strings.Join(p.Manual, " ")
	if !strings.Contains(joined, "Sign in to each workspace again") {
		t.Fatal("migration must require independent re-authentication")
	}
	if !strings.Contains(strings.ToLower(joined), "not copied") {
		t.Fatal("migration must state plainly that sign-ins are not copied")
	}
	if !strings.Contains(strings.Join(p.Steps, " "), "Back up") {
		t.Fatal("migration must back up the config before changing it")
	}
	// The other tool's own provider block must survive, so its rollback still works.
	if strings.Contains(p.Diff, "- [model_providers.codex-lb]") {
		t.Fatal("migration must not delete the other tool's provider definition")
	}
}

func TestMigrationNamesIncompatibleSettings(t *testing.T) {
	cfg := writeCfg(t, `model_provider = "codex-lb"
http_responses_session_bridge_advertise_base_url = "http://example"
dashboard_auth_mode = "standard"

[model_providers.codex-lb]
base_url = "http://127.0.0.1:2455/backend-api/codex"
`)
	p, _ := PlanMigration(cfg, "127.0.0.1:7788", true)
	if len(p.Incompatible) < 2 {
		t.Fatalf("settings with no codex-relay equivalent must be named, got %v", p.Incompatible)
	}
}

func TestNoMigrationWhenNoOtherToolIsPresent(t *testing.T) {
	cfg := writeCfg(t, "model = \"gpt-5.1-codex\"\n")
	p, err := PlanMigration(cfg, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Detected {
		t.Fatalf("nothing should be detected in a plain config: %+v", p)
	}
}

// TestUninstallExplainsRetentionAndCredentialEffects: the contract requires uninstall to say
// what it keeps and what it destroys BEFORE doing it.
func TestUninstallExplainsRetentionAndCredentialEffects(t *testing.T) {
	p := DescribeUninstall("/home/u/.codex/config.toml", "/home/u/.local/share/codex-relay", 2, 3, true)
	all := strings.Join(p.Effects, " ")
	if !strings.Contains(all, "restored") {
		t.Error("uninstall must say the Codex config is restored")
	}
	if !strings.Contains(all, "deleted from your OS credential store") {
		t.Error("uninstall must say stored sign-ins are deleted")
	}
	if !strings.Contains(all, "cannot continue") {
		t.Error("uninstall must warn that bound conversations cannot continue")
	}
	keeps := strings.Join(p.Keeps, " ")
	if !strings.Contains(keeps, "untouched") {
		t.Error("uninstall must say what it leaves alone")
	}
	if !strings.Contains(keeps, "another tool owns") {
		t.Error("uninstall must promise not to touch another tool's data")
	}
}

func TestUninstallOnACleanInstallSaysSoRatherThanClaimingAnUndo(t *testing.T) {
	p := DescribeUninstall("/cfg", "/data", 0, 0, false)
	if p.ConfigWillRevert {
		t.Error("nothing was applied, so nothing should be reverted")
	}
	if !strings.Contains(strings.Join(p.Effects, " "), "nothing there needs undoing") {
		t.Errorf("should state plainly that there is no config change to undo: %v", p.Effects)
	}
}
