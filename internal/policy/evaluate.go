package policy

import (
	"fmt"
	"sort"
	"time"
)

// ReasonCode is the shared vocabulary used by live decisions, previews and Activity rows.
// Preview and execution emit the same codes because they run the same function.
type ReasonCode string

const (
	ReasonDefault            ReasonCode = "default_workspace"
	ReasonPreferred          ReasonCode = "preferred_by_rule"
	ReasonProtected          ReasonCode = "protected_by_reserve"
	ReasonProtectionUnknown  ReasonCode = "protection_evidence_unknown"
	ReasonPaused             ReasonCode = "paused"
	ReasonCredential         ReasonCode = "credential_unavailable"
	ReasonModelIneligible    ReasonCode = "model_not_eligible"
	ReasonModelUnknown       ReasonCode = "model_eligibility_unknown"
	ReasonOwnerBound         ReasonCode = "owner_bound_conversation"
	ReasonOwnerBlocked       ReasonCode = "owner_bound_but_blocked"
	ReasonNoEligible         ReasonCode = "no_eligible_workspace"
	ReasonReserveNoAlternate ReasonCode = "reserve_has_no_alternative"
	ReasonQuotaExhausted     ReasonCode = "quota_exhausted"
	ReasonHandoff            ReasonCode = "handed_off"
	ReasonProfilePreferred   ReasonCode = "preferred_by_profile"
	ReasonProfileDisabled    ReasonCode = "disabled_by_profile"
	ReasonWeeklyPace         ReasonCode = "weekly_pace"
	ReasonPaceOverflow       ReasonCode = "pace_overflow"
	ReasonSubagentHelper     ReasonCode = "subagent_helper"
)

// Outcome is what the evaluator decided.
type Outcome string

const (
	// OutcomeSelected means a workspace was chosen and the request may proceed.
	OutcomeSelected Outcome = "selected"
	// OutcomeBlocked means nothing may be spent and the caller must be told why.
	OutcomeBlocked Outcome = "blocked"
)

// Note is one human-readable line of the explanation, tied to a machine reason code.
type Note struct {
	Code        ReasonCode `json:"code"`
	WorkspaceID string     `json:"workspace_id,omitempty"`
	RuleID      string     `json:"rule_id,omitempty"`
	Message     string     `json:"message"`
	// Comparison is the exact arithmetic that produced the note, shown in the rule editor
	// so the user can check our work rather than trust it.
	Comparison string `json:"comparison,omitempty"`
}

// Candidate is one workspace's standing in a decision.
type Candidate struct {
	WorkspaceID string     `json:"workspace_id"`
	Eligible    bool       `json:"eligible"`
	Rank        int        `json:"rank"`
	Reason      ReasonCode `json:"reason"`
	Detail      string     `json:"detail,omitempty"`
}

// Decision is the full, explainable result of one evaluation.
type Decision struct {
	Outcome      Outcome     `json:"outcome"`
	WorkspaceID  string      `json:"workspace_id,omitempty"`
	Primary      ReasonCode  `json:"primary_reason"`
	Summary      string      `json:"summary"`
	Notes        []Note      `json:"notes"`
	Candidates   []Candidate `json:"candidates"`
	StateVersion int64       `json:"state_version"`
	EvaluatedAt  time.Time   `json:"evaluated_at"`
	// Simulated marks a result produced from hypothetical values. The dashboard must label
	// any simulated result and must never present one as live state.
	Simulated bool `json:"simulated"`
	// UnknownEvidence is true when a protection-sensitive input could not be established.
	// An unknown protection is never treated as false.
	UnknownEvidence bool `json:"unknown_evidence"`
}

// Request describes what is being admitted.
type Request struct {
	Model string
	// IsSubagent is true only when Codex marks this as delegated work. It is never inferred
	// from the model name, so an ordinary Luna parent task keeps the normal profile routing.
	IsSubagent bool
	// OwnerWorkspaceID is set when this conversation is already bound to a workspace.
	// Ownership is retained unless an explicit preference selects another eligible workspace
	// or the owner reaches an automatic handoff condition.
	OwnerWorkspaceID string
	ThreadID         string
	// Exclude names workspaces this attempt must not pick, because they already refused this
	// exact turn. A 429 is the case: the poll-based reading said there was room and upstream
	// disagreed, so the reading is wrong and only the refusal is trustworthy.
	Exclude []string
}

// Evaluate is the single decision function. Live admission and dashboard preview both call
// it, with the same State and the same clock source, so a preview cannot promise something
// admission would not do.
//
// Ordering is deterministic and intentionally small: apply hard eligibility first
// (ownership, pause, credentials, model, reserve protection), then order what survives by
// preference, then by the configured default and stable workspace order. There is no
// general workflow engine here, by design.
func Evaluate(st *State, req Request, now time.Time) Decision {
	d := Decision{
		StateVersion: st.Version,
		EvaluatedAt:  now,
		Notes:        []Note{},
		Candidates:   []Candidate{},
	}

	ids := append([]string(nil), st.Order...)
	sort.Strings(ids)
	if len(st.Order) > 0 {
		ids = st.Order
	}

	// Hard eligibility pass.
	eligible := map[string]bool{}
	detail := map[string]ReasonCode{}
	details := map[string]string{}
	profileDisabled := map[string]bool{}
	if st.ActiveProfile != nil {
		for _, id := range st.ActiveProfile.DisabledWorkspaceIDs {
			profileDisabled[id] = true
		}
	}

	for _, id := range ids {
		ws := st.Workspaces[id]
		if ws == nil {
			continue
		}
		switch {
		case ws.Paused:
			detail[id], details[id] = ReasonPaused, "Paused, so it is not admitting new conversations."
		case profileDisabled[id]:
			detail[id], details[id] = ReasonProfileDisabled, "Disabled by the active routing profile."
		case !ws.CredentialOK:
			msg := "Its credential needs attention."
			if ws.CredentialNote != "" {
				msg = ws.CredentialNote
			}
			detail[id], details[id] = ReasonCredential, msg
		default:
			ok, code, why := modelEligible(ws, req.Model)
			if !ok {
				detail[id], details[id] = code, why
				break
			}
			eligible[id] = true
			detail[id] = ReasonDefault
		}
	}

	// Quota exhaustion, applied only when somewhere else can actually serve.
	//
	// Until this existed, eligibility considered only pause, credential and model, so a
	// preferred workspace sitting at 100% used kept taking new conversations and every one
	// failed upstream. Observed live.
	//
	// It is a second pass, not another case above, because a workspace that is empty AND
	// alone must still be tried: blocking pre-emptively gives the user nothing, while trying
	// at least surfaces upstream's own answer. The same reason a reserve rule can fall back
	// to the workspace it protects.
	empty := map[string]bool{}
	haveAlternative := false
	for _, id := range ids {
		ws := st.Workspaces[id]
		if ws == nil || !eligible[id] {
			continue
		}
		if exhausted(st, ws, now) {
			empty[id] = true
		} else {
			haveAlternative = true
		}
	}
	if haveAlternative {
		for id := range empty {
			rem, _ := lowestRemaining(st, st.Workspaces[id], now)
			eligible[id] = false
			detail[id] = ReasonQuotaExhausted
			details[id] = fmt.Sprintf("Out of quota: %s remaining on its tightest window.", formatPct(rem))
		}
	}

	// A workspace that just refused this turn is out, whatever the stored reading claims.
	for _, id := range req.Exclude {
		if _, known := st.Workspaces[id]; known {
			eligible[id] = false
			detail[id] = ReasonQuotaExhausted
			details[id] = "It refused this turn, so it is not retried for it."
		}
	}

	// Reserve rules are hard restrictions applied on top of eligibility.
	protectedBy := map[string]Rule{}
	preferOrder := []string{}
	explicitPreferOrder := []string{}

	for _, rule := range sortedRules(st.Rules) {
		if !rule.Enabled {
			continue
		}
		switch rule.Kind {
		case KindPrefer:
			preferOrder = append(preferOrder, rule.SourceWorkspaceID)
			explicitPreferOrder = append(explicitPreferOrder, rule.SourceWorkspaceID)
		case KindReserve:
			match, unknown, note := reserveMatches(st, rule, now)
			if note != nil {
				d.Notes = append(d.Notes, *note)
			}
			if unknown {
				// A protection-sensitive input we cannot establish is not "false".
				d.UnknownEvidence = true
				protectedBy[rule.SourceWorkspaceID] = rule
				eligible[rule.SourceWorkspaceID] = false
				detail[rule.SourceWorkspaceID] = ReasonProtectionUnknown
				details[rule.SourceWorkspaceID] = "Protected: we could not confirm its quota, so the reserve is held rather than assumed clear."
				preferOrder = append(preferOrder, rule.PreferredWorkspaceID)
				continue
			}
			if match {
				protectedBy[rule.SourceWorkspaceID] = rule
				eligible[rule.SourceWorkspaceID] = false
				detail[rule.SourceWorkspaceID] = ReasonProtected
				details[rule.SourceWorkspaceID] = fmt.Sprintf("Protected by your reserve rule; %s is preferred instead.", nameOf(st, rule.PreferredWorkspaceID))
				preferOrder = append(preferOrder, rule.PreferredWorkspaceID)
			}
		}
	}

	// Profiles are reusable user-named strategies. They order only workspaces that survived
	// hard eligibility and reserve protection. A profile therefore cannot override a pause,
	// broken credential, exhausted quota, unsupported model, or reserve rule.
	profileOrder := []string{}
	profileReason := ""
	profileCode := ReasonProfilePreferred
	if p := st.ActiveProfile; p != nil {
		switch p.Mode {
		case ProfilePace:
			profileOrder, _, profileReason = paceOrder(st, eligible, now)
			if len(profileOrder) > 0 && profileOrder[0] == p.OverflowWorkspaceID {
				profileCode = ReasonPaceOverflow
			} else {
				profileCode = ReasonWeeklyPace
			}
		case ProfilePriority:
			for _, id := range p.PriorityWorkspaceIDs {
				if eligible[id] {
					profileOrder = append(profileOrder, id)
				}
			}
			if len(profileOrder) > 0 {
				profileReason = fmt.Sprintf("The active %s profile prefers %s.", p.Name, nameOf(st, profileOrder[0]))
			}
		}
	}
	if len(profileOrder) > 0 {
		preferOrder = append(profileOrder, preferOrder...)
		explicitPreferOrder = append([]string{profileOrder[0]}, explicitPreferOrder...)
		d.Notes = append(d.Notes, Note{
			Code: profileCode, WorkspaceID: profileOrder[0], Message: profileReason,
		})
	}

	// A helper preference is narrower than the profile's main ordering: it applies only to
	// an actual Codex subagent using the configured helper model. If the helper is paused,
	// out of quota, unsupported, protected, or otherwise unavailable, the ordinary profile
	// order remains intact and becomes the fallback.
	if p := st.ActiveProfile; p != nil && p.SubagentHelperEnabled && req.IsSubagent &&
		(req.Model == "" || req.Model == p.SubagentHelperModel) {
		helperID := p.SubagentHelperWorkspaceID
		if eligible[helperID] {
			preferOrder = append([]string{helperID}, withoutWorkspace(preferOrder, helperID)...)
			explicitPreferOrder = append([]string{helperID}, withoutWorkspace(explicitPreferOrder, helperID)...)
			profileCode = ReasonSubagentHelper
			profileReason = fmt.Sprintf("This is a delegated %s subagent, so the active %s profile prefers %s as its helper.",
				p.SubagentHelperModel, p.Name, nameOf(st, helperID))
			d.Notes = append(d.Notes, Note{Code: ReasonSubagentHelper, WorkspaceID: helperID, Message: profileReason})
			if len(profileOrder) == 0 || profileOrder[0] != helperID {
				profileOrder = append([]string{helperID}, withoutWorkspace(profileOrder, helperID)...)
			}
		} else {
			d.Notes = append(d.Notes, Note{Code: ReasonSubagentHelper, WorkspaceID: helperID,
				Message: fmt.Sprintf("The configured Luna helper %s is unavailable: %s The normal profile order is the fallback.",
					nameOf(st, helperID), details[helperID])})
		}
	}

	// An explicit preference is also the user's account switch. It applies between turns to
	// existing conversations, not only when a conversation is first created. Find the first
	// eligible preference separately from the general rank so an unavailable preference does
	// not move a healthy owner merely because the default workspace or a reserve fallback
	// ranked next. Reserve-driven handoff keeps its separate safety gate below.
	preferred := ""
	for _, id := range explicitPreferOrder {
		if eligible[id] {
			preferred = id
			break
		}
	}

	if req.OwnerWorkspaceID != "" {
		owner := st.Workspaces[req.OwnerWorkspaceID]
		preferenceMove := preferred != "" && preferred != req.OwnerWorkspaceID

		// An owner that is still eligible but nearly empty is handed off BEFORE the turn that
		// would fail on it. Waiting for the hard refusal costs the user a failed turn first.
		nearlyOut := false
		if owner != nil && eligible[req.OwnerWorkspaceID] && st.HandoffBelowPercent > 0 {
			if rem, known := lowestRemaining(st, owner, now); known && rem <= st.HandoffBelowPercent {
				// Only worth moving if somewhere better exists; otherwise stay and spend what is left.
				if alt := withoutWorkspace(rank(st, ids, eligible, preferOrder), req.OwnerWorkspaceID); len(alt) > 0 {
					nearlyOut = true
				}
			}
		}

		if owner != nil && eligible[req.OwnerWorkspaceID] && !nearlyOut && !preferenceMove {
			d.Outcome = OutcomeSelected
			d.WorkspaceID = req.OwnerWorkspaceID
			d.Primary = ReasonOwnerBound
			d.Summary = fmt.Sprintf("Continuing on %s, which already owns this conversation.", owner.Name)
			d.Notes = append(d.Notes, Note{Code: ReasonOwnerBound, WorkspaceID: owner.ID,
				Message: "This conversation is bound to this workspace, so it does not move."})
			d.Candidates = buildCandidates(st, ids, eligible, detail, details, []string{req.OwnerWorkspaceID})
			return d
		}
		// The owner cannot serve, or is about to stop being able to. Moving the conversation
		// is safe here in a way it would not be against a stateful API: this backend rejects
		// store=true, so Codex resends the whole conversation on every turn and nothing lives
		// server-side that is bound to the old account. Verified against the live endpoint.
		//
		// It is still gated, and it still obeys the rules: the replacement is drawn from the
		// same eligibility set as any other pick, so a reserve rule cannot be side-stepped by
		// a handoff.
		if st.HandoffBelowPercent > 0 || preferenceMove {
			alt := rank(st, ids, eligible, preferOrder)
			alt = withoutWorkspace(alt, req.OwnerWorkspaceID)
			if len(alt) > 0 {
				pick := alt[0]
				to := st.Workspaces[pick]
				d.Outcome = OutcomeSelected
				d.WorkspaceID = pick
				d.Primary = ReasonHandoff
				if preferenceMove && pick == preferred {
					d.Summary = fmt.Sprintf("%s owned this conversation, but your rule now prefers %s, so it moved between turns.",
						nameOf(st, req.OwnerWorkspaceID), to.Name)
				} else {
					d.Summary = fmt.Sprintf("%s can no longer serve this conversation, so it moved to %s. The full conversation is resent each turn, so nothing was lost.",
						nameOf(st, req.OwnerWorkspaceID), to.Name)
				}
				d.Notes = append(d.Notes, Note{Code: ReasonHandoff, WorkspaceID: pick, Message: d.Summary})
				d.Candidates = buildCandidates(st, ids, eligible, detail, details, alt)
				return d
			}
		}

		// Blocked owner with nowhere to go: explain the constraint rather than failing blankly.
		d.Outcome = OutcomeBlocked
		d.Primary = ReasonOwnerBlocked
		name := req.OwnerWorkspaceID
		if owner != nil {
			name = owner.Name
		}
		why := details[req.OwnerWorkspaceID]
		if why == "" {
			why = "It is not currently available."
		}
		if st.HandoffBelowPercent <= 0 {
			d.Summary = fmt.Sprintf("This conversation is bound to %s. %s Conversation handoff is disabled, so this turn was blocked. Restore the owner, enable handoff, or start a new conversation.", name, why)
		} else {
			d.Summary = fmt.Sprintf("This conversation is bound to %s. %s No eligible alternative is currently available, so this turn was blocked. Restore the owner, change the policy, or start a new conversation.", name, why)
		}
		d.Notes = append(d.Notes, Note{Code: ReasonOwnerBlocked, WorkspaceID: req.OwnerWorkspaceID, Message: d.Summary})
		d.Candidates = buildCandidates(st, ids, eligible, detail, details, nil)
		return d
	}

	ranked := rank(st, ids, eligible, preferOrder)
	d.Candidates = buildCandidates(st, ids, eligible, detail, details, ranked)

	if len(ranked) == 0 {
		d.Outcome = OutcomeBlocked
		d.Primary = ReasonNoEligible
		if len(protectedBy) > 0 {
			d.Primary = ReasonReserveNoAlternate
			d.Summary = "No workspace is eligible. A reserve rule is protecting quota and no alternative can serve this request, so nothing was spent."
		} else {
			d.Summary = "No workspace is eligible for this request, so nothing was spent."
		}
		d.Notes = append(d.Notes, Note{Code: d.Primary, Message: d.Summary})
		return d
	}

	pick := ranked[0]
	ws := st.Workspaces[pick]
	d.Outcome = OutcomeSelected
	d.WorkspaceID = pick
	d.Primary = ReasonDefault
	d.Summary = fmt.Sprintf("New conversations will use %s.", ws.Name)
	if len(profileOrder) > 0 && pick == profileOrder[0] {
		d.Primary = profileCode
		d.Summary = profileReason
		return d
	}
	for _, p := range preferOrder {
		if p == pick {
			d.Primary = ReasonPreferred
			d.Summary = fmt.Sprintf("New conversations will use %s because a rule prefers it.", ws.Name)
			break
		}
	}
	return d
}

// reserveMatches evaluates a reserve condition. Both conditions must hold. It compares
// remaining percent (not used percent) and compares reset time as an exact duration, with
// no calendar-day rounding and no hidden margin.
func reserveMatches(st *State, rule Rule, now time.Time) (match bool, unknown bool, note *Note) {
	ws := st.Workspaces[rule.SourceWorkspaceID]
	if ws == nil {
		return false, false, nil
	}
	w, ev := st.EvidenceFor(ws, rule.Window.Minutes, now)
	if ev == EvidenceMissing {
		return false, true, &Note{
			Code: ReasonProtectionUnknown, WorkspaceID: ws.ID, RuleID: rule.ID,
			Message: fmt.Sprintf("No %s quota reading for %s, so its reserve is held rather than assumed clear.", rule.Window.String(), ws.Name),
		}
	}

	remaining := w.RemainingPercent()
	remainingHit := remaining <= rule.RemainingPercent
	if rule.Comparison == Below {
		remainingHit = remaining < rule.RemainingPercent
	}

	if w.ResetsAt == nil {
		return false, true, &Note{
			Code: ReasonProtectionUnknown, WorkspaceID: ws.ID, RuleID: rule.ID,
			Message: fmt.Sprintf("%s reported %s remaining but no reset time, so the reserve is held rather than assumed clear.", ws.Name, formatPct(remaining)),
		}
	}

	until := w.ResetsAt.Sub(now)
	want := time.Duration(rule.ResetHours * float64(time.Hour))
	resetHit := until >= want
	if rule.ResetComparison == AtMost {
		resetHit = until <= want
	}

	cmp := fmt.Sprintf("remaining %s %s %g%% (%v) AND reset in %s %s %s (%v)",
		formatPct(remaining), rule.Comparison.Symbol(), rule.RemainingPercent, remainingHit,
		humanDuration(until), rule.ResetComparison.Symbol(), humanHours(rule.ResetHours), resetHit)

	n := &Note{Code: ReasonProtected, WorkspaceID: ws.ID, RuleID: rule.ID, Comparison: cmp}
	if remainingHit && resetHit {
		n.Message = fmt.Sprintf("Reserve rule matched for %s.", ws.Name)
		if ev == EvidenceStale {
			n.Message += " The quota reading is stale."
		}
		return true, false, n
	}
	n.Code = ReasonDefault
	n.Message = fmt.Sprintf("Reserve rule did not match for %s.", ws.Name)
	return false, false, n
}

func modelEligible(ws *WorkspaceState, model string) (bool, ReasonCode, string) {
	if model == "" || len(ws.EligibleModels) == 0 {
		// We have not observed a model list. That is unknown, not ineligible, and it does
		// not on its own remove the workspace.
		return true, ReasonModelUnknown, ""
	}
	if ws.EligibleModels[model] {
		return true, ReasonDefault, ""
	}
	return false, ReasonModelIneligible, fmt.Sprintf("It did not report %s as an available model.", model)
}

func rank(st *State, ids []string, eligible map[string]bool, preferOrder []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range preferOrder {
		if eligible[p] && !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	if st.DefaultWorkspaceID != "" && eligible[st.DefaultWorkspaceID] && !seen[st.DefaultWorkspaceID] {
		out = append(out, st.DefaultWorkspaceID)
		seen[st.DefaultWorkspaceID] = true
	}
	for _, id := range ids {
		if eligible[id] && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}

func buildCandidates(st *State, ids []string, eligible map[string]bool, detail map[string]ReasonCode, details map[string]string, ranked []string) []Candidate {
	pos := map[string]int{}
	for i, id := range ranked {
		pos[id] = i + 1
	}
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		if st.Workspaces[id] == nil {
			continue
		}
		out = append(out, Candidate{
			WorkspaceID: id,
			Eligible:    eligible[id],
			Rank:        pos[id],
			Reason:      detail[id],
			Detail:      details[id],
		})
	}
	return out
}

func sortedRules(rules []Rule) []Rule {
	out := append([]Rule(nil), rules...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func nameOf(st *State, id string) string {
	if ws := st.Workspaces[id]; ws != nil {
		return ws.Name
	}
	return id
}

func formatPct(v float64) string { return fmt.Sprintf("%.4g%%", v) }

func humanDuration(d time.Duration) string {
	if d < 0 {
		return "already past"
	}
	h := d.Hours()
	if h >= 24 {
		return fmt.Sprintf("%.1f days", h/24)
	}
	return fmt.Sprintf("%.1f hours", h)
}

// withoutWorkspace removes one id, so a handoff cannot pick the workspace it is leaving.
func withoutWorkspace(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// lowestRemaining is the tightest window the workspace reports, and whether it is knowable.
//
// The tightest window is the one that stops the next turn, so a workspace with 40% left on
// the week and 1% left on the hour is at 1%. Evidence that is missing returns false: a
// handoff fired on an unknown reading would move conversations for no reason.
func lowestRemaining(st *State, ws *WorkspaceState, now time.Time) (float64, bool) {
	lowest, found := 0.0, false
	for minutes := range ws.Windows {
		w, ev := st.EvidenceFor(ws, minutes, now)
		if ev == EvidenceMissing {
			continue
		}
		if r := w.RemainingPercent(); !found || r < lowest {
			lowest, found = r, true
		}
	}
	return lowest, found
}

// exhausted reports whether a workspace has run its tightest window down to the handoff
// floor, using only evidence we actually have.
//
// The same threshold governs handoff, so a conversation is moved off a workspace at exactly
// the point new conversations stop being sent to it. Two different numbers here would mean a
// workspace could be too empty to start on but not empty enough to leave.
func exhausted(st *State, ws *WorkspaceState, now time.Time) bool {
	if st.HandoffBelowPercent <= 0 {
		return false
	}
	rem, known := lowestRemaining(st, ws, now)
	return known && rem <= st.HandoffBelowPercent
}
