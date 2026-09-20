package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ProfileMode string

const (
	ProfilePriority ProfileMode = "priority"
	ProfilePace     ProfileMode = "pace"
)

const WeeklyWindowMinutes int64 = 10080

var profileCommandPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// RoutingProfile is one named, reusable routing configuration. Its command and aliases are
// data, not hardcoded vocabulary, so the dashboard and relaypool resolve the same profiles.
type RoutingProfile struct {
	ID                     string      `json:"id"`
	Name                   string      `json:"name"`
	Command                string      `json:"command"`
	Aliases                []string    `json:"aliases"`
	Mode                   ProfileMode `json:"mode"`
	PriorityWorkspaceIDs   []string    `json:"priority_workspace_ids"`
	PaceWorkspaceIDs       []string    `json:"pace_workspace_ids"`
	OverflowWorkspaceID    string      `json:"overflow_workspace_id,omitempty"`
	TargetRemainingPercent float64     `json:"target_remaining_percent"`
	DefaultWorkspaceID     string      `json:"default_workspace_id,omitempty"`
	HandoffBelowPercent    float64     `json:"handoff_below_percent"`
	DisabledWorkspaceIDs   []string    `json:"disabled_workspace_ids"`
	CreatedAt              time.Time   `json:"created_at,omitempty"`
	UpdatedAt              time.Time   `json:"updated_at,omitempty"`
}

func NormalizeProfileCommand(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func (p *RoutingProfile) Normalize() {
	p.Name = strings.TrimSpace(p.Name)
	p.Command = NormalizeProfileCommand(p.Command)
	p.Aliases = normalizeCommands(p.Aliases)
	p.PriorityWorkspaceIDs = uniqueStrings(p.PriorityWorkspaceIDs)
	p.PaceWorkspaceIDs = uniqueStrings(p.PaceWorkspaceIDs)
	p.DisabledWorkspaceIDs = uniqueStrings(p.DisabledWorkspaceIDs)
}

func normalizeCommands(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, item := range in {
		item = NormalizeProfileCommand(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, item := range in {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func (p RoutingProfile) Validate(known map[string]bool) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("a profile name is required")
	}
	if !profileCommandPattern.MatchString(p.Command) {
		return fmt.Errorf("command must start with a letter and use only lowercase letters, numbers, hyphens, or underscores")
	}
	reserved := map[string]bool{"status": true, "who": true, "profiles": true, "help": true}
	if reserved[p.Command] {
		return fmt.Errorf("%q is reserved by relaypool", p.Command)
	}
	seenCommands := map[string]bool{p.Command: true}
	for _, alias := range p.Aliases {
		if !profileCommandPattern.MatchString(alias) {
			return fmt.Errorf("alias %q is not a valid command", alias)
		}
		if reserved[alias] {
			return fmt.Errorf("alias %q is reserved by relaypool", alias)
		}
		if seenCommands[alias] {
			return fmt.Errorf("command and aliases must be unique")
		}
		seenCommands[alias] = true
	}
	if p.Mode != ProfilePriority && p.Mode != ProfilePace {
		return fmt.Errorf("profile mode must be priority or pace")
	}
	if p.HandoffBelowPercent < 0 || p.HandoffBelowPercent > 100 {
		return fmt.Errorf("handoff floor must be between 0 and 100 percent")
	}
	allIDs := append([]string{}, p.PriorityWorkspaceIDs...)
	allIDs = append(allIDs, p.PaceWorkspaceIDs...)
	allIDs = append(allIDs, p.OverflowWorkspaceID, p.DefaultWorkspaceID)
	allIDs = append(allIDs, p.DisabledWorkspaceIDs...)
	for _, id := range allIDs {
		if id != "" && !known[id] {
			return fmt.Errorf("workspace %q is not connected", id)
		}
	}
	if p.Mode == ProfilePriority && len(p.PriorityWorkspaceIDs) == 0 {
		return fmt.Errorf("a priority profile needs at least one workspace")
	}
	if p.Mode == ProfilePace {
		if len(p.PaceWorkspaceIDs) == 0 {
			return fmt.Errorf("a pace profile needs at least one paced workspace")
		}
		if p.OverflowWorkspaceID == "" {
			return fmt.Errorf("a pace profile needs an overflow workspace")
		}
		for _, id := range p.PaceWorkspaceIDs {
			if id == p.OverflowWorkspaceID {
				return fmt.Errorf("the overflow workspace cannot also be paced")
			}
		}
		if p.TargetRemainingPercent < 0 || p.TargetRemainingPercent > 100 {
			return fmt.Errorf("pace target must be between 0 and 100 percent")
		}
	}
	disabled := map[string]bool{}
	for _, id := range p.DisabledWorkspaceIDs {
		disabled[id] = true
	}
	available := 0
	for id := range known {
		if !disabled[id] {
			available++
		}
	}
	if available == 0 {
		return fmt.Errorf("a profile cannot disable every workspace")
	}
	return nil
}

func (p RoutingProfile) Sentence(nameOf func(string) string) string {
	if p.Mode == ProfilePace {
		names := make([]string, 0, len(p.PaceWorkspaceIDs))
		for _, id := range p.PaceWorkspaceIDs {
			names = append(names, nameOf(id))
		}
		return fmt.Sprintf("Pace %s toward %.4g%% remaining at each weekly reset. Use %s as overflow while they are on schedule.",
			strings.Join(names, ", "), p.TargetRemainingPercent, nameOf(p.OverflowWorkspaceID))
	}
	names := make([]string, 0, len(p.PriorityWorkspaceIDs))
	for _, id := range p.PriorityWorkspaceIDs {
		names = append(names, nameOf(id))
	}
	return "Try workspaces in this order: " + strings.Join(names, " -> ") + "."
}

type PaceStanding struct {
	WorkspaceID      string  `json:"workspace_id"`
	RemainingPercent float64 `json:"remaining_percent"`
	ExpectedPercent  float64 `json:"expected_percent"`
	DeltaPercent     float64 `json:"delta_percent"`
	Known            bool    `json:"known"`
	Status           string  `json:"status"`
}

func paceOrder(st *State, eligible map[string]bool, now time.Time) ([]string, []PaceStanding, string) {
	p := st.ActiveProfile
	if p == nil || p.Mode != ProfilePace {
		return nil, nil, ""
	}
	standing := make([]PaceStanding, 0, len(p.PaceWorkspaceIDs))
	for _, id := range p.PaceWorkspaceIDs {
		x := PaceStanding{WorkspaceID: id, Status: "quota evidence unavailable"}
		ws := st.Workspaces[id]
		if ws != nil {
			window, evidence := st.EvidenceFor(ws, WeeklyWindowMinutes, now)
			if evidence != EvidenceMissing && window.ResetsAt != nil {
				remainingTime := window.ResetsAt.Sub(now)
				windowDuration := time.Duration(WeeklyWindowMinutes) * time.Minute
				if remainingTime < 0 {
					remainingTime = 0
				}
				if remainingTime > windowDuration {
					remainingTime = windowDuration
				}
				x.Known = true
				x.RemainingPercent = window.RemainingPercent()
				x.ExpectedPercent = p.TargetRemainingPercent +
					(100-p.TargetRemainingPercent)*(remainingTime.Hours()/windowDuration.Hours())
				x.DeltaPercent = x.RemainingPercent - x.ExpectedPercent
				if x.DeltaPercent > 0 {
					x.Status = "needs usage"
				} else {
					x.Status = "on or ahead of pace"
				}
			}
		}
		standing = append(standing, x)
	}

	candidates := append([]PaceStanding(nil), standing...)
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].DeltaPercent > candidates[j].DeltaPercent
	})
	order := []string{}
	reason := ""
	for _, x := range candidates {
		if x.Known && x.DeltaPercent > 0 && eligible[x.WorkspaceID] {
			order = append(order, x.WorkspaceID)
			if reason == "" {
				reason = fmt.Sprintf("%s is %.1f percentage points above its weekly pace target, so it needs usage now.",
					nameOf(st, x.WorkspaceID), x.DeltaPercent)
			}
		}
	}
	if len(order) == 0 && eligible[p.OverflowWorkspaceID] {
		order = append(order, p.OverflowWorkspaceID)
		reason = fmt.Sprintf("The paced workspaces are on schedule, so overflow is using %s.", nameOf(st, p.OverflowWorkspaceID))
	}
	for _, id := range p.PriorityWorkspaceIDs {
		if eligible[id] && !containsID(order, id) {
			order = append(order, id)
		}
	}
	for _, x := range candidates {
		if eligible[x.WorkspaceID] && !containsID(order, x.WorkspaceID) {
			order = append(order, x.WorkspaceID)
		}
	}
	if eligible[p.OverflowWorkspaceID] && !containsID(order, p.OverflowWorkspaceID) {
		order = append(order, p.OverflowWorkspaceID)
	}
	if reason == "" && len(order) > 0 {
		reason = fmt.Sprintf("Weekly pace evidence could not choose a workspace, so the profile fell back to %s.", nameOf(st, order[0]))
	}
	return order, standing, reason
}

// PaceStandings exposes the same target curve used by live routing for the dashboard. It
// does not make a second routing decision or invent data when a weekly reading is absent.
func PaceStandings(st *State, now time.Time) []PaceStanding {
	eligible := map[string]bool{}
	for id := range st.Workspaces {
		eligible[id] = true
	}
	_, standings, _ := paceOrder(st, eligible, now)
	return standings
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
