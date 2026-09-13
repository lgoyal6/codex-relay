package service

import (
	"context"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/store"
)

func autoService(t *testing.T) *Service {
	t.Helper()
	svc := connectService(t, secrets.NewMemory())
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('a','u','a@e.com','plus','2026-09-12T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws','a','cg','WS',0,'r',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)
	return svc
}

func minutePtr(h, m int) *int64 { v := int64(h*60 + m); return &v }

// A daily schedule fires in its minute and not again, so a tick landing twice in the same
// minute cannot pause something twice or fight a person who just resumed it.
func TestDailyAutomationFiresOnceInItsMinute(t *testing.T) {
	svc := autoService(t)
	ctx := context.Background()
	a, err := svc.CreateAutomation(ctx, "night pause", store.AutomationPauseDaily, "ws", minutePtr(22, 30))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 13, 22, 30, 0, 0, time.UTC)

	if due, _ := svc.automationDue(a, at); !due {
		t.Fatal("did not fire in its scheduled minute")
	}
	if due, _ := svc.automationDue(a, at.Add(time.Minute)); due {
		t.Error("fired outside its scheduled minute")
	}
	// Once it has run, it must not run again in the same minute.
	ran := at
	a.LastRunAt = &ran
	if due, _ := svc.automationDue(a, at); due {
		t.Error("fired twice in the same minute")
	}
	// A service that was down at the scheduled minute must not fire late hours afterwards.
	if due, _ := svc.automationDue(a, at.Add(3*time.Hour)); due {
		t.Error("fired hours late, which would surprise someone")
	}
}

// The whole point of resume-on-reset is that it waits for the window to actually roll over.
func TestResumeOnResetWaitsForTheWindowToRollOver(t *testing.T) {
	svc := autoService(t)
	ctx := context.Background()
	mustExec(t, svc, `UPDATE workspaces SET paused = 1 WHERE id = 'ws'`)

	future := time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	observed := future.Add(-time.Hour)
	if _, err := svc.DB.SQL().ExecContext(ctx, `
		INSERT INTO quota_windows (workspace_id, limit_id, window_minutes, used_percent, resets_at, observed_at, source)
		VALUES ('ws','codex',300,95,?,?,'response_headers')`,
		future.Unix(), observed.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := svc.CreateAutomation(ctx, "auto resume", store.AutomationResumeOnReset, "ws", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Before the reset: must not resume, the quota is still spent.
	if due, _ := svc.automationDue(a, future.Add(-30*time.Minute)); due {
		t.Fatal("resumed before the window reset, while the account was still exhausted")
	}
	// After the reset: resume.
	if due, why := svc.automationDue(a, future.Add(time.Minute)); !due {
		t.Fatalf("did not resume after the window reset (why=%q)", why)
	}
}

// A workspace that is not paused is not resumed, so an automation cannot undo a deliberate
// state or churn the routing snapshot every minute.
func TestResumeOnResetIgnoresAnActiveWorkspace(t *testing.T) {
	svc := autoService(t)
	ctx := context.Background()
	a, err := svc.CreateAutomation(ctx, "auto resume", store.AutomationResumeOnReset, "ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if due, _ := svc.automationDue(a, time.Now().UTC()); due {
		t.Fatal("tried to resume a workspace that was never paused")
	}
}

// Running an automation must actually change the workspace and be visible to routing.
func TestRunningAnAutomationPausesAndRefreshesRouting(t *testing.T) {
	svc := autoService(t)
	ctx := context.Background()
	if _, err := svc.CreateAutomation(ctx, "pause now", store.AutomationPauseDaily, "ws",
		minutePtr(svc.Clock.Now().UTC().Hour(), svc.Clock.Now().UTC().Minute())); err != nil {
		t.Fatal(err)
	}
	svc.RunAutomations(ctx)

	var paused int
	if err := svc.DB.SQL().QueryRowContext(ctx, `SELECT paused FROM workspaces WHERE id='ws'`).Scan(&paused); err != nil {
		t.Fatal(err)
	}
	if paused != 1 {
		t.Fatal("the automation did not pause the workspace")
	}
	if ws := svc.Registry.Current().Workspaces["ws"]; ws == nil || !ws.Paused {
		t.Fatal("routing still sees the workspace as active, so the pause would not take effect")
	}
}

// Invalid schedules are refused at creation. A schedule that can never fire is worse than
// none, because it looks configured.
func TestInvalidAutomationsAreRefused(t *testing.T) {
	svc := autoService(t)
	ctx := context.Background()
	bad := []struct {
		name, kind string
		minute     *int64
	}{
		{"", store.AutomationPauseDaily, minutePtr(1, 0)},
		{"no time", store.AutomationPauseDaily, nil},
		{"out of range", store.AutomationResumeDaily, func() *int64 { v := int64(1500); return &v }()},
		{"unknown kind", "reboot_the_planet", nil},
	}
	for _, c := range bad {
		if _, err := svc.CreateAutomation(ctx, c.name, c.kind, "ws", c.minute); err == nil {
			t.Errorf("accepted an automation that can never fire: %+v", c)
		}
	}
}
