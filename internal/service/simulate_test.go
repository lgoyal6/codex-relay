package service

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/routing"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/store"
)

// simService builds a two-workspace pool: Personal on 60% weekly, Backup on 80%.
func simService(t *testing.T, now time.Time) *Service {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/sim.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := clock.NewFake(now)
	sec := secrets.NewMemory()
	svc := New(db, routing.NewRegistry(clk), NewCredentialManager(sec, clk, ""), sec, clk,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('acct','u','person@example.com','plus','2026-09-20T00:00:00Z')`)
	for _, w := range []struct {
		id, name string
		order    int
		used     float64
	}{
		{"personal", "Personal", 0, 40},
		{"backup", "Backup", 1, 20},
	} {
		if _, err := svc.DB.SQL().Exec(`INSERT INTO workspaces (id, account_id, chatgpt_account_id,
			display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
			VALUES (?,'acct',?,?,0,'ref',1,?,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`,
			w.id, w.id, w.name, w.order); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.DB.SQL().Exec(`INSERT INTO quota_windows (workspace_id, limit_id,
			window_minutes, used_percent, resets_at, observed_at, source)
			VALUES (?,'codex',10080,?,?,?,'usage_endpoint')`,
			w.id, w.used, now.Add(72*time.Hour).Unix(), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestSimulationFindsTheHandoverPoint is the whole point of the feature: a reserve threshold
// is invisible until the day it fires, and a person planning their week needs to know the
// number before then, not after.
func TestSimulationFindsTheHandoverPoint(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	svc := simService(t, now)
	ctx := context.Background()

	// Hold Personal back once its weekly drops to 25% with the reset still far off.
	mustExec(t, svc, `INSERT INTO rules (id, kind, enabled, priority, source_workspace_id,
		window_minutes, comparison, remaining_percent, reset_comparison, reset_hours,
		preferred_workspace_id, no_alternative, created_at, updated_at)
		VALUES ('r','reserve',1,20,'personal',10080,'at_or_below',25,'at_least',48,
		'backup','stop_and_explain','2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`)
	if err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	sim, err := svc.SimulateDrain(ctx, "personal", 10080, "")
	if err != nil {
		t.Fatal(err)
	}
	if sim.FromPercent != 60 {
		t.Fatalf("starts at %v%%, want the live reading of 60%%", sim.FromPercent)
	}
	if len(sim.Bands) < 2 {
		t.Fatalf("got %d band(s); a threshold in range must produce a change", len(sim.Bands))
	}

	first, second := sim.Bands[0], sim.Bands[1]
	if first.WorkspaceID != "personal" {
		t.Errorf("first band routes to %q, want personal while it is above the reserve", first.WorkspaceID)
	}
	if second.WorkspaceID != "backup" {
		t.Errorf("after the threshold it routes to %q, want backup", second.WorkspaceID)
	}
	// The rule reads "at or below 25", so 26 is the last value Personal serves.
	if first.ToPercent != 26 || second.FromPercent != 25 {
		t.Errorf("handover at %v -> %v, want the rule's own boundary 26 -> 25",
			first.ToPercent, second.FromPercent)
	}
	// The winning decision is "preferred by rule": the reserve rule names Backup as the
	// alternative. What explains the change is what happened to Personal, so the band has to
	// carry that too, or the boundary is a switch with no stated cause.
	if second.Primary != policy.ReasonPreferred {
		t.Errorf("the band names %q as the reason, want preferred_by_rule", second.Primary)
	}
	if !strings.Contains(second.DrainedDetail, "Protected") {
		t.Errorf("the band explains the drained workspace as %q, want it to say it is protected", second.DrainedDetail)
	}
}

// A pool with nothing to trigger must not invent a change.
func TestSimulationWithoutThresholdsStaysOnOneBand(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	svc := simService(t, now)

	sim, err := svc.SimulateDrain(context.Background(), "personal", 10080, "")
	if err != nil {
		t.Fatal(err)
	}
	// Personal is the default workspace and nothing protects it, so the only change allowed
	// is the one the handoff floor forces when it finally runs dry.
	for _, b := range sim.Bands[:len(sim.Bands)-1] {
		if b.WorkspaceID != "personal" {
			t.Fatalf("band %v%%-%v%% routed to %q with no rule to move it", b.FromPercent, b.ToPercent, b.WorkspaceID)
		}
	}
	last := sim.Bands[len(sim.Bands)-1]
	if last.ToPercent != 0 {
		t.Errorf("the walk stops at %v%%, want it to reach 0", last.ToPercent)
	}
}

func TestSimulationRejectsUnknownWorkspace(t *testing.T) {
	svc := simService(t, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	if _, err := svc.SimulateDrain(context.Background(), "nope", 10080, ""); err == nil {
		t.Fatal("an unknown workspace must be an error, not an empty simulation")
	}
}
