package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/store"
)

// AutomationTick is how often schedules are evaluated. A minute is the resolution the
// schedules themselves use, so checking more often would do nothing.
const AutomationTick = time.Minute

func (s *Service) ListAutomations(ctx context.Context) ([]store.Automation, error) {
	return s.DB.ListAutomations(ctx)
}

func (s *Service) DeleteAutomation(ctx context.Context, id string) error {
	return s.DB.DeleteAutomation(ctx, id)
}

func (s *Service) SetAutomationEnabled(ctx context.Context, id string, enabled bool) error {
	return s.DB.SetAutomationEnabled(ctx, id, enabled)
}

// CreateAutomation validates before storing, because a schedule that can never fire is worse
// than none: it looks configured and does nothing.
func (s *Service) CreateAutomation(ctx context.Context, name, kind, workspaceID string, atMinute *int64) (store.Automation, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Automation{}, fmt.Errorf("an automation needs a name")
	}
	switch kind {
	case store.AutomationPauseDaily, store.AutomationResumeDaily:
		if atMinute == nil {
			return store.Automation{}, fmt.Errorf("a daily automation needs a time of day")
		}
		if *atMinute < 0 || *atMinute > 1439 {
			return store.Automation{}, fmt.Errorf("time of day must be between 00:00 and 23:59")
		}
	case store.AutomationResumeOnReset:
		atMinute = nil
	default:
		return store.Automation{}, fmt.Errorf("unknown automation kind %q", kind)
	}
	if _, err := s.DB.SQL().ExecContext(ctx, `SELECT 1 FROM workspaces WHERE id = ?`, workspaceID); err != nil {
		return store.Automation{}, err
	}
	a := store.Automation{
		ID: uuid.NewString(), Name: name, Kind: kind,
		WorkspaceID: workspaceID, AtMinute: atMinute, Enabled: true,
		CreatedAt: s.Clock.Now().UTC(),
	}
	if err := s.DB.CreateAutomation(ctx, a, s.Clock.Now()); err != nil {
		return store.Automation{}, err
	}
	return a, nil
}

// StartAutomations evaluates schedules until ctx is done.
func (s *Service) StartAutomations(ctx context.Context) {
	go func() {
		t := time.NewTicker(AutomationTick)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.RunAutomations(ctx)
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}
	}()
}

// RunAutomations applies every automation that is due.
//
// Each one is idempotent: setting a pause that is already set changes nothing, so a tick that
// fires twice in the same minute cannot do damage.
func (s *Service) RunAutomations(ctx context.Context) {
	list, err := s.DB.ListAutomations(ctx)
	if err != nil {
		s.Log.Warn("could not read automations", "error", err)
		return
	}
	now := s.Clock.Now().UTC()
	for _, a := range list {
		if !a.Enabled {
			continue
		}
		due, why := s.automationDue(a, now)
		if !due {
			continue
		}
		var result string
		switch a.Kind {
		case store.AutomationPauseDaily:
			result = s.applyPause(ctx, a.WorkspaceID, true)
		case store.AutomationResumeDaily, store.AutomationResumeOnReset:
			result = s.applyPause(ctx, a.WorkspaceID, false)
		}
		s.DB.MarkAutomationRun(ctx, a.ID, why+": "+result, now)
		s.Log.Info("automation ran", "name", a.Name, "kind", a.Kind, "result", result)
	}
}

func (s *Service) applyPause(ctx context.Context, workspaceID string, paused bool) string {
	_, err := s.DB.SQL().ExecContext(ctx,
		`UPDATE workspaces SET paused = ?, updated_at = ? WHERE id = ?`,
		boolToInt(paused), s.Clock.Now().UTC().Format(time.RFC3339Nano), workspaceID)
	if err != nil {
		return "failed: " + err.Error()
	}
	if err := s.Refresh(ctx); err != nil {
		return "applied, but the routing snapshot did not refresh: " + err.Error()
	}
	if paused {
		return "paused"
	}
	return "resumed"
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// automationDue decides whether an automation should fire now.
//
// Daily schedules fire in their minute and not again, using last_run_at rather than a
// countdown, so a service that was not running at the scheduled minute does not fire late and
// surprise someone hours afterwards.
func (s *Service) automationDue(a store.Automation, now time.Time) (bool, string) {
	switch a.Kind {
	case store.AutomationPauseDaily, store.AutomationResumeDaily:
		if a.AtMinute == nil {
			return false, ""
		}
		minuteOfDay := int64(now.Hour()*60 + now.Minute())
		if minuteOfDay != *a.AtMinute {
			return false, ""
		}
		if a.LastRunAt != nil && now.Sub(a.LastRunAt.UTC()) < 2*time.Minute {
			return false, ""
		}
		return true, fmt.Sprintf("scheduled for %02d:%02d UTC", *a.AtMinute/60, *a.AtMinute%60)

	case store.AutomationResumeOnReset:
		st := s.Registry.Current()
		ws := st.Workspaces[a.WorkspaceID]
		if ws == nil || !ws.Paused {
			return false, ""
		}
		// Resume once every window this workspace reports has rolled past its reset.
		if len(ws.Windows) == 0 {
			return false, ""
		}
		for minutes := range ws.Windows {
			w, ev := st.EvidenceFor(ws, minutes, now)
			// A window whose reading is gone because the window itself expired is exactly
			// the signal to resume; a still-live window means not yet.
			if ev != policy.EvidenceMissing && w.ResetsAt != nil && now.Before(*w.ResetsAt) {
				return false, ""
			}
		}
		return true, "quota window reset"
	}
	return false, ""
}
