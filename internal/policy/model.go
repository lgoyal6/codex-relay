// Package policy holds the rule model and the single deterministic evaluator used for
// both live admission and dashboard preview. There is exactly one evaluator so a preview
// can never disagree with the decision that follows it.
package policy

import (
	"fmt"
	"time"
)

// WindowRef names a quota window by its reported duration in minutes.
//
// Codex reports windows as an anonymous "primary" and "secondary" pair, each carrying a
// window_minutes value (observed live: 300 and 10080). It does not report window names, and
// a plan need not expose both. Rules therefore bind to a duration, not to the words
// "five-hour" or "weekly", and a rule whose window is absent evaluates to Unknown rather
// than to false.
type WindowRef struct {
	Minutes int64 `json:"minutes"`
}

func (w WindowRef) String() string { return HumanWindow(w.Minutes) }

// HumanWindow renders a window duration the way the dashboard labels it. It describes the
// reported duration and never invents a plan-specific name.
func HumanWindow(minutes int64) string {
	switch {
	case minutes <= 0:
		return "unknown window"
	case minutes%10080 == 0 && minutes/10080 == 1:
		return "weekly"
	case minutes%1440 == 0:
		d := minutes / 1440
		if d == 1 {
			return "daily"
		}
		return fmt.Sprintf("%d-day", d)
	case minutes%60 == 0:
		return fmt.Sprintf("%d-hour", minutes/60)
	default:
		return fmt.Sprintf("%d-minute", minutes)
	}
}

// Comparison is the operator used by a threshold condition.
type Comparison string

const (
	AtOrBelow Comparison = "at_or_below" // remaining_percent <= value
	Below     Comparison = "below"       // remaining_percent <  value
)

func (c Comparison) Symbol() string {
	if c == Below {
		return "<"
	}
	return "<="
}

// ResetComparison is the operator used by the reset-time condition.
type ResetComparison string

const (
	AtLeast ResetComparison = "at_least" // reset_at - now >= value
	AtMost  ResetComparison = "at_most"  // reset_at - now <= value
)

func (c ResetComparison) Symbol() string {
	if c == AtMost {
		return "<="
	}
	return ">="
}

// RuleKind enumerates the two first-release templates.
type RuleKind string

const (
	// KindPrefer orders otherwise eligible choices. It never removes a choice.
	KindPrefer RuleKind = "prefer"
	// KindReserve is a hard eligibility restriction on its source workspace while its
	// condition matches.
	KindReserve RuleKind = "reserve"
)

// NoAlternativeBehavior is what a reserve rule does when no alternative can serve.
//
// The contract fixes the default: stop and explain. Spending the protected quota anyway is
// available but must be chosen deliberately, because it silently defeats the reserve.
type NoAlternativeBehavior string

const (
	StopAndExplain NoAlternativeBehavior = "stop_and_explain"
	UseProtected   NoAlternativeBehavior = "use_protected"
)

// Rule is one structured policy row. Rules are never free text: the English sentence shown
// in the dashboard is generated from these fields, and only these fields are evaluated.
type Rule struct {
	ID       string   `json:"id"`
	Kind     RuleKind `json:"kind"`
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"` // lower runs first; ties broken by ID for determinism

	// SourceWorkspaceID is the workspace being protected (reserve) or preferred (prefer).
	SourceWorkspaceID string `json:"source_workspace_id"`

	// Reserve-only condition fields.
	Window               WindowRef             `json:"window"`
	Comparison           Comparison            `json:"comparison"`
	RemainingPercent     float64               `json:"remaining_percent"`
	ResetComparison      ResetComparison       `json:"reset_comparison"`
	ResetHours           float64               `json:"reset_hours"`
	PreferredWorkspaceID string                `json:"preferred_workspace_id"`
	NoAlternative        NoAlternativeBehavior `json:"no_alternative"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate rejects configurations that cannot be evaluated coherently. The dashboard shows
// these messages directly, so they are written for a person.
func (r Rule) Validate() error {
	if r.SourceWorkspaceID == "" {
		return fmt.Errorf("choose a workspace this rule applies to")
	}
	switch r.Kind {
	case KindPrefer:
		return nil
	case KindReserve:
		if r.Window.Minutes <= 0 {
			return fmt.Errorf("choose which quota window to watch")
		}
		if r.RemainingPercent < 0 || r.RemainingPercent > 100 {
			return fmt.Errorf("remaining percent must be between 0 and 100")
		}
		if r.ResetHours < 0 {
			return fmt.Errorf("the reset condition cannot use a negative number of hours")
		}
		if r.PreferredWorkspaceID == "" {
			return fmt.Errorf("choose which workspace to prefer while this one is protected")
		}
		if r.PreferredWorkspaceID == r.SourceWorkspaceID {
			return fmt.Errorf("a workspace cannot be the alternative to itself")
		}
		switch r.NoAlternative {
		case StopAndExplain, UseProtected:
		default:
			return fmt.Errorf("choose what should happen when no alternative is eligible")
		}
		return nil
	default:
		return fmt.Errorf("unknown rule type %q", r.Kind)
	}
}

// Sentence renders the rule as the plain-English line shown in the dashboard. It is
// generated from the structured fields and is never parsed back.
func (r Rule) Sentence(name func(string) string) string {
	switch r.Kind {
	case KindPrefer:
		return fmt.Sprintf("Prefer %s for new conversations.", name(r.SourceWorkspaceID))
	case KindReserve:
		tail := "stop and explain"
		if r.NoAlternative == UseProtected {
			tail = fmt.Sprintf("use %s anyway", name(r.SourceWorkspaceID))
		}
		return fmt.Sprintf(
			"Protect %s and prefer %s when its %s quota remaining is %s %g%% and its reset is %s %s away. If %s cannot serve the request, %s.",
			name(r.SourceWorkspaceID),
			name(r.PreferredWorkspaceID),
			r.Window.String(),
			r.Comparison.Symbol(),
			r.RemainingPercent,
			r.ResetComparison.Symbol(),
			humanHours(r.ResetHours),
			name(r.PreferredWorkspaceID),
			tail,
		)
	}
	return ""
}

func humanHours(h float64) string {
	if h >= 24 && h == float64(int64(h)) && int64(h)%24 == 0 {
		d := int64(h) / 24
		if d == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", d)
	}
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%g hours", h)
}
