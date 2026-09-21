package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type memRecorder struct {
	changes []Change
	nextID  int64
}

func (m *memRecorder) Record(c Change) error {
	m.nextID++
	c.ID = m.nextID
	m.changes = append(m.changes, c)
	return nil
}
func (m *memRecorder) Latest(path string) (Change, bool, error) {
	for i := len(m.changes) - 1; i >= 0; i-- {
		if m.changes[i].TargetPath == path {
			return m.changes[i], true, nil
		}
	}
	return Change{}, false, nil
}
func (m *memRecorder) MarkRolledBack(int64, time.Time) error { return nil }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPlanUsesAModelProviderNotChatgptBaseUrl(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	p, err := BuildPlan(cfg, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.After, `model_provider = "codexrelay"`) {
		t.Fatal("the plan must select our model provider")
	}
	if !strings.Contains(p.After, "[agents]\ndefault_subagent_model = \"gpt-5.6-luna\"") {
		t.Fatal("the plan must make delegated helpers use Luna")
	}
	if !strings.Contains(p.After, `base_url = "http://127.0.0.1:7788/backend-api/codex"`) {
		t.Fatalf("provider base_url is wrong:\n%s", p.After)
	}
	if !strings.Contains(p.After, "requires_openai_auth = true") {
		t.Fatal("requires_openai_auth must be set so Codex attaches its ChatGPT auth")
	}
	// chatgpt_base_url does not redirect generation in codex-cli 0.154.0, so the minimal
	// supported configuration must not rely on it.
	if strings.Contains(p.After, "chatgpt_base_url") {
		t.Fatal("the minimal configuration must not depend on chatgpt_base_url")
	}
}

func TestPlanReplacesAnExistingDottedSubagentModelWithoutDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	write(t, cfg, "agents.default_subagent_model = \"gpt-old\"\n\n[mcp_servers.tool]\ncommand = \"tool\"\n")
	p, err := BuildPlan(cfg, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(p.After, `default_subagent_model = "gpt-5.6-luna"`) != 1 {
		t.Fatalf("Luna default count is not one:\n%s", p.After)
	}
	if !strings.Contains(p.After, `# codex-relay disabled this line: agents.default_subagent_model = "gpt-old"`) {
		t.Fatalf("the previous setting was not preserved as a comment:\n%s", p.After)
	}
	if !strings.Contains(p.Diff, `- agents.default_subagent_model = "gpt-old"`) {
		t.Fatalf("the setup preview did not show the replaced helper model:\n%s", p.Diff)
	}

	rec := &memRecorder{}
	if _, err := Apply(p, rec, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(cfg)
	write(t, cfg, string(current)+"\n[profiles.later]\nmodel = \"gpt-later\"\n")
	if _, err := Rollback(rec, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cfg)
	if !strings.Contains(string(after), `agents.default_subagent_model = "gpt-old"`) {
		t.Fatalf("surgical rollback must restore the dotted helper model:\n%s", after)
	}
	if !strings.Contains(string(after), "[profiles.later]") {
		t.Fatalf("surgical rollback must preserve later edits:\n%s", after)
	}
}

func TestPlanInsertsSubagentModelIntoExistingAgentsTable(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	write(t, cfg, "[agents]\nmax_threads = 4\n\n[mcp_servers.tool]\ncommand = \"tool\"\n")
	p, err := BuildPlan(cfg, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(p.After, "[agents]") != 1 {
		t.Fatalf("setup must not create a duplicate [agents] table:\n%s", p.After)
	}
	agentsStart := strings.Index(p.After, "[agents]")
	mcpStart := strings.Index(p.After, "[mcp_servers.tool]")
	modelStart := strings.Index(p.After, `default_subagent_model = "gpt-5.6-luna"`)
	if agentsStart < 0 || modelStart < agentsStart || mcpStart < modelStart {
		t.Fatalf("the helper model must belong to the existing [agents] table:\n%s", p.After)
	}
	if !strings.Contains(p.After, "max_threads = 4") {
		t.Fatalf("existing agent settings must survive:\n%s", p.After)
	}
}

func TestPlanDisablesAndRestoresExistingAgentsTableModel(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	original := "[agents]\ndefault_subagent_model = \"gpt-old\"\nmax_threads = 4\n"
	write(t, cfg, original)
	p, err := BuildPlan(cfg, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.After, `# codex-relay disabled this line: default_subagent_model = "gpt-old"`) {
		t.Fatalf("the previous [agents] setting was not preserved:\n%s", p.After)
	}
	if !strings.Contains(p.Diff, `- default_subagent_model = "gpt-old"`) {
		t.Fatalf("the preview did not show the replaced [agents] model:\n%s", p.Diff)
	}

	rec := &memRecorder{}
	if _, err := Apply(p, rec, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(cfg)
	write(t, cfg, string(current)+"\n[mcp_servers.later]\ncommand = \"later\"\n")
	res, err := Rollback(rec, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ConflictDetected {
		t.Fatal("the later edit must force surgical rollback")
	}
	after, _ := os.ReadFile(cfg)
	if !strings.Contains(string(after), original) {
		t.Fatalf("surgical rollback must restore the previous [agents] model:\n%s", after)
	}
	if !strings.Contains(string(after), "[mcp_servers.later]") {
		t.Fatalf("surgical rollback must preserve later edits:\n%s", after)
	}
}

func TestPlanUpgradesOldIncorrectManagedSubagentKey(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	old := `# >>> codex-relay managed block (do not edit inside) >>>
model_provider = "codexrelay"
default_subagent_model = "gpt-5.6-luna"
# <<< codex-relay managed block <<<

# >>> codex-relay managed provider (do not edit inside) >>>
[model_providers.codexrelay]
name = "openai"
base_url = "http://127.0.0.1:7788/backend-api/codex"
wire_api = "responses"
supports_websockets = true
requires_openai_auth = true
# <<< codex-relay managed provider <<<
`
	write(t, cfg, old)
	p, err := BuildPlan(cfg, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.After, "\ndefault_subagent_model = \"gpt-5.6-luna\"\n# <<< codex-relay managed block") {
		t.Fatalf("the obsolete top-level key must be removed from the main block:\n%s", p.After)
	}
	if !strings.Contains(p.After, "[agents]\ndefault_subagent_model = \"gpt-5.6-luna\"") {
		t.Fatalf("the upgrade must write the supported setting under [agents]:\n%s", p.After)
	}
}

func TestRollbackRestoresExactlyWhenUntouched(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	original := "model = \"gpt-5.1-codex\"\napproval_policy = \"never\"\n"
	write(t, cfg, original)

	p, _ := BuildPlan(cfg, "127.0.0.1:7788", true)
	rec := &memRecorder{}
	if _, err := Apply(p, rec, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	if !strings.Contains(string(got), "codexrelay") {
		t.Fatal("apply did not write the provider")
	}

	res, err := Rollback(rec, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Restored || res.ConflictDetected {
		t.Fatalf("clean rollback expected, got %+v", res)
	}
	after, _ := os.ReadFile(cfg)
	if string(after) != original {
		t.Fatalf("rollback must restore the original byte for byte.\ngot:  %q\nwant: %q", string(after), original)
	}
}

// TestRollbackPreservesUnrelatedLaterEdits is the contract's requirement that rollback
// restores only our changes and detects conflicts instead of overwriting them.
func TestRollbackPreservesUnrelatedLaterEdits(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	write(t, cfg, "model = \"gpt-5.1-codex\"\n")

	p, _ := BuildPlan(cfg, "127.0.0.1:7788", true)
	rec := &memRecorder{}
	if _, err := Apply(p, rec, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The user edits the file afterwards, adding something unrelated.
	current, _ := os.ReadFile(cfg)
	edited := string(current) + "\n[mcp_servers.my_tool]\ncommand = \"my-tool\"\n"
	write(t, cfg, edited)

	res, err := Rollback(rec, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ConflictDetected {
		t.Fatal("rollback must notice the file changed after we wrote it")
	}
	after, _ := os.ReadFile(cfg)
	s := string(after)
	if !strings.Contains(s, "[mcp_servers.my_tool]") {
		t.Fatal("the user's later edit must be preserved, not overwritten by the backup")
	}
	if strings.Contains(s, "codexrelay") {
		t.Fatalf("our managed block must be removed:\n%s", s)
	}
	if !strings.Contains(s, `model = "gpt-5.1-codex"`) {
		t.Fatal("unrelated original settings must survive")
	}
}

func TestExistingProviderIsDisabledNotDeletedAndIsRestored(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	original := "model_provider = \"codex-lb\"\n\n[model_providers.codex-lb]\nbase_url = \"http://127.0.0.1:2455/backend-api/codex\"\n"
	write(t, cfg, original)

	p, _ := BuildPlan(cfg, "127.0.0.1:7788", true)
	if !strings.Contains(p.After, `# codex-relay disabled this line: model_provider = "codex-lb"`) {
		t.Fatalf("a pre-existing provider must be commented out, not deleted:\n%s", p.After)
	}
	// The other tool's provider table is left intact so its own rollback still works.
	if !strings.Contains(p.After, "[model_providers.codex-lb]") {
		t.Fatal("the other tool's provider definition must be left alone")
	}

	rec := &memRecorder{}
	if _, err := Apply(p, rec, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Rollback(rec, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cfg)
	if string(after) != original {
		t.Fatalf("the previous provider selection must be restored.\ngot:  %q\nwant: %q", string(after), original)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	write(t, cfg, "model = \"m\"\n")
	rec := &memRecorder{}

	p1, _ := BuildPlan(cfg, "127.0.0.1:7788", true)
	if _, err := Apply(p1, rec, time.Now()); err != nil {
		t.Fatal(err)
	}
	p2, _ := BuildPlan(cfg, "127.0.0.1:7788", true)
	if !p2.NoChange {
		t.Fatalf("re-applying the same configuration should be a no-op, diff:\n%s", p2.Diff)
	}

	// Changing the port replaces our block in place rather than stacking a second one.
	p3, _ := BuildPlan(cfg, "127.0.0.1:9999", true)
	if p3.NoChange {
		t.Fatal("a different port must produce a change")
	}
	if strings.Count(p3.After, beginMarker) != 1 {
		t.Fatalf("the managed block must not be duplicated:\n%s", p3.After)
	}
}

func TestRollbackWithNoRecordedChangeIsSafe(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	write(t, cfg, "model = \"m\"\n")
	res, err := Rollback(&memRecorder{}, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored {
		t.Fatal("nothing was applied, so nothing should be restored")
	}
	after, _ := os.ReadFile(cfg)
	if string(after) != "model = \"m\"\n" {
		t.Fatal("rollback must not touch a file it never changed")
	}
}

// firstTableHeader returns the byte offset of the first TOML table header, or -1.
func firstTableHeader(s string) int {
	for i, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			return i
		}
	}
	return -1
}

func lineOf(s, want string) int {
	for i, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == want {
			return i
		}
	}
	return -1
}

// TestBareKeyStaysTopLevelWhenTheUserAlreadyHasTables is a regression test for a bug that
// made setup a silent no-op.
//
// In TOML a bare key belongs to whatever table precedes it. Appending model_provider to the
// end of a config that already contains any table (an MCP server, a profile, another
// provider) turns it into a key of THAT table: Codex keeps its old provider, nothing routes,
// and the user's table quietly grows a key that does not belong to it. Reproduced against
// codex-cli 0.154.0.
func TestBareKeyStaysTopLevelWhenTheUserAlreadyHasTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	before := `model = "gpt-5.1-codex"

[mcp_servers.filesystem]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem"]
`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(path, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}

	keyLine := lineOf(p.After, `model_provider = "codexrelay"`)
	if keyLine < 0 {
		t.Fatal("the provider selection line is missing entirely")
	}
	table := firstTableHeader(p.After)
	if table < 0 {
		t.Fatal("the user's table disappeared")
	}
	if keyLine > table {
		t.Fatalf("model_provider is on line %d, after the first table header on line %d: "+
			"TOML would read it as a key of that table and Codex would never switch provider:\n%s",
			keyLine, table, p.After)
	}
	if !strings.Contains(p.After, "[model_providers.codexrelay]") {
		t.Fatal("our own provider table is missing")
	}
	// The user's table must be untouched, and must not have acquired our key.
	if !strings.Contains(p.After, "[mcp_servers.filesystem]\ncommand = \"npx\"") {
		t.Fatalf("the user's table was altered:\n%s", p.After)
	}
	if !strings.Contains(p.After, `model = "gpt-5.1-codex"`) {
		t.Fatalf("the user's own top-level keys must survive:\n%s", p.After)
	}
}

// TestRollbackRemovesAllRegions covers the three-region layout: a rollback that omitted one
// would leave a partially configured file behind.
func TestRollbackRemovesAllRegions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	before := "model = \"gpt-5.1-codex\"\n\n[mcp_servers.fs]\ncommand = \"npx\"\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := BuildPlan(path, "127.0.0.1:7788", true)
	rec := &memRecorder{}
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := Apply(p, rec, now); err != nil {
		t.Fatal(err)
	}
	// A later third-party edit forces the surgical path rather than a wholesale restore.
	cur, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(cur, []byte("\n[mcp_servers.other]\ncommand = \"x\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Rollback(rec, path, now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ConflictDetected {
		t.Fatal("a later edit should be detected as a conflict")
	}
	after, _ := os.ReadFile(path)
	got := string(after)
	for _, leftover := range []string{beginMarker, endMarker, beginTableMarker, endTableMarker,
		beginAgentsMarker, endAgentsMarker, `model_provider = "codexrelay"`,
		"[model_providers.codexrelay]", `default_subagent_model = "gpt-5.6-luna"`} {
		if strings.Contains(got, leftover) {
			t.Fatalf("rollback left %q behind:\n%s", leftover, got)
		}
	}
	if !strings.Contains(got, "[mcp_servers.other]") || !strings.Contains(got, "[mcp_servers.fs]") {
		t.Fatalf("rollback must preserve the user's tables, including the later edit:\n%s", got)
	}
}

// TestProviderInsideAnotherTableIsNotDisabled protects unrelated configuration. Only a
// top-level model_provider is ours to comment out; a key of the same name inside a profile
// belongs to that profile.
func TestProviderInsideAnotherTableIsNotDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	before := "[profiles.work]\nmodel_provider = \"openai\"\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(path, "127.0.0.1:7788", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.After, "# codex-relay disabled this line") {
		t.Fatalf("a model_provider inside someone else's table must not be touched:\n%s", p.After)
	}
}
