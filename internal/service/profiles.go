package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lgoyal6/codex-relay/internal/policy"
)

const ActiveProfileSetting = "active_profile_id"

func (s *Service) RoutingProfiles(ctx context.Context) ([]policy.RoutingProfile, error) {
	rows, err := s.DB.SQL().QueryContext(ctx, `
		SELECT id, name, command, aliases_json, mode, priority_workspace_ids_json,
		       pace_workspace_ids_json, overflow_workspace_id, target_remaining_percent,
		       default_workspace_id, handoff_below_percent, disabled_workspace_ids_json,
		       subagent_helper_enabled, subagent_helper_workspace_id, subagent_helper_model,
		       created_at, updated_at
		FROM routing_profiles ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []policy.RoutingProfile{}
	for rows.Next() {
		profile, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, profile)
	}
	return out, rows.Err()
}

type profileScanner interface {
	Scan(...any) error
}

func scanProfile(row profileScanner) (policy.RoutingProfile, error) {
	var p policy.RoutingProfile
	var aliases, priority, paced, disabled, mode, created, updated string
	var helperEnabled bool
	err := row.Scan(&p.ID, &p.Name, &p.Command, &aliases, &mode, &priority, &paced,
		&p.OverflowWorkspaceID, &p.TargetRemainingPercent, &p.DefaultWorkspaceID,
		&p.HandoffBelowPercent, &disabled, &helperEnabled, &p.SubagentHelperWorkspaceID,
		&p.SubagentHelperModel, &created, &updated)
	if err != nil {
		return p, err
	}
	p.Mode = policy.ProfileMode(mode)
	p.SubagentHelperEnabled = helperEnabled
	for _, pair := range []struct {
		raw    string
		target *[]string
	}{{aliases, &p.Aliases}, {priority, &p.PriorityWorkspaceIDs}, {paced, &p.PaceWorkspaceIDs}, {disabled, &p.DisabledWorkspaceIDs}} {
		if err := json.Unmarshal([]byte(pair.raw), pair.target); err != nil {
			return p, fmt.Errorf("profile %s contains invalid JSON: %w", p.ID, err)
		}
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func (s *Service) SaveRoutingProfile(ctx context.Context, profile policy.RoutingProfile) (policy.RoutingProfile, error) {
	profile.Normalize()
	known, err := s.knownWorkspaceIDs(ctx)
	if err != nil {
		return profile, err
	}
	if err := profile.Validate(known); err != nil {
		return profile, err
	}
	if profile.ID == "" {
		profile.ID = uuid.NewString()
	}
	profiles, err := s.RoutingProfiles(ctx)
	if err != nil {
		return profile, err
	}
	wanted := append([]string{profile.Command}, profile.Aliases...)
	for _, existing := range profiles {
		if existing.ID == profile.ID {
			continue
		}
		have := append([]string{existing.Command}, existing.Aliases...)
		for _, a := range wanted {
			for _, b := range have {
				if a == b {
					return profile, fmt.Errorf("command or alias %q is already used by %s", a, existing.Name)
				}
			}
		}
	}
	aliases, _ := json.Marshal(profile.Aliases)
	priority, _ := json.Marshal(profile.PriorityWorkspaceIDs)
	paced, _ := json.Marshal(profile.PaceWorkspaceIDs)
	disabled, _ := json.Marshal(profile.DisabledWorkspaceIDs)
	now := s.Clock.Now().Format(time.RFC3339Nano)
	created := now
	if !profile.CreatedAt.IsZero() {
		created = profile.CreatedAt.Format(time.RFC3339Nano)
	}
	_, err = s.DB.SQL().ExecContext(ctx, `
		INSERT INTO routing_profiles
			(id, name, command, aliases_json, mode, priority_workspace_ids_json,
			 pace_workspace_ids_json, overflow_workspace_id, target_remaining_percent,
			 default_workspace_id, handoff_below_percent, disabled_workspace_ids_json,
			 subagent_helper_enabled, subagent_helper_workspace_id, subagent_helper_model,
			 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, command=excluded.command, aliases_json=excluded.aliases_json,
			mode=excluded.mode, priority_workspace_ids_json=excluded.priority_workspace_ids_json,
			pace_workspace_ids_json=excluded.pace_workspace_ids_json,
			overflow_workspace_id=excluded.overflow_workspace_id,
			target_remaining_percent=excluded.target_remaining_percent,
			default_workspace_id=excluded.default_workspace_id,
			handoff_below_percent=excluded.handoff_below_percent,
			disabled_workspace_ids_json=excluded.disabled_workspace_ids_json,
			subagent_helper_enabled=excluded.subagent_helper_enabled,
			subagent_helper_workspace_id=excluded.subagent_helper_workspace_id,
			subagent_helper_model=excluded.subagent_helper_model,
			updated_at=excluded.updated_at`,
		profile.ID, profile.Name, profile.Command, string(aliases), string(profile.Mode), string(priority),
		string(paced), profile.OverflowWorkspaceID, profile.TargetRemainingPercent,
		profile.DefaultWorkspaceID, profile.HandoffBelowPercent, string(disabled),
		profile.SubagentHelperEnabled, profile.SubagentHelperWorkspaceID, profile.SubagentHelperModel,
		created, now)
	if err != nil {
		return profile, err
	}
	profile.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	profile.UpdatedAt, _ = time.Parse(time.RFC3339Nano, now)
	if active, _ := s.GetSetting(ctx, ActiveProfileSetting); active == profile.ID {
		if err := s.Refresh(ctx); err != nil {
			return profile, err
		}
	}
	return profile, nil
}

func (s *Service) ActivateRoutingProfile(ctx context.Context, commandOrAlias string) (policy.RoutingProfile, error) {
	commandOrAlias = policy.NormalizeProfileCommand(commandOrAlias)
	profiles, err := s.RoutingProfiles(ctx)
	if err != nil {
		return policy.RoutingProfile{}, err
	}
	for _, profile := range profiles {
		if profile.Command == commandOrAlias || containsString(profile.Aliases, commandOrAlias) {
			if err := s.SetSetting(ctx, ActiveProfileSetting, profile.ID); err != nil {
				return profile, err
			}
			if err := s.Refresh(ctx); err != nil {
				return profile, err
			}
			return profile, nil
		}
	}
	return policy.RoutingProfile{}, fmt.Errorf("no routing profile uses command or alias %q", commandOrAlias)
}

func (s *Service) DeleteRoutingProfile(ctx context.Context, id string) error {
	active, _ := s.GetSetting(ctx, ActiveProfileSetting)
	if active == id {
		return fmt.Errorf("activate another profile before deleting the active one")
	}
	result, err := s.DB.SQL().ExecContext(ctx, `DELETE FROM routing_profiles WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Service) ActiveRoutingProfile(ctx context.Context) (*policy.RoutingProfile, error) {
	id, err := s.GetSetting(ctx, ActiveProfileSetting)
	if err != nil || id == "" {
		return nil, err
	}
	row := s.DB.SQL().QueryRowContext(ctx, `
		SELECT id, name, command, aliases_json, mode, priority_workspace_ids_json,
		       pace_workspace_ids_json, overflow_workspace_id, target_remaining_percent,
		       default_workspace_id, handoff_below_percent, disabled_workspace_ids_json,
		       subagent_helper_enabled, subagent_helper_workspace_id, subagent_helper_model,
		       created_at, updated_at
		FROM routing_profiles WHERE id = ?`, id)
	profile, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

func (s *Service) knownWorkspaceIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.SQL().QueryContext(ctx, `SELECT id FROM workspaces`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func containsString(items []string, wanted string) bool {
	for _, item := range items {
		if strings.EqualFold(item, wanted) {
			return true
		}
	}
	return false
}
