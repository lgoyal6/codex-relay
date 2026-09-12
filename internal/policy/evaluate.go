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
	// OwnerWorkspaceID is set when this conversation is already bound to a workspace.
	// Owner-bound conversations do not migrate silently.
	OwnerWorkspaceID string
	ThreadID         string
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

	for _, id := range ids {
		ws := st.Workspaces[id]
		if ws == nil {
			continue
		}
		switch {
		case ws.Paused:
			detail[id], details[id] = ReasonPaused, "Paused, so it is not admitting new conversations."
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

	// Reserve rules are hard restrictions applied on top of eligibility.
	protectedBy := map[string]Rule{}
	preferOrder := []string{}

	for _, rule := range sortedRules(st.Rules) {
		if !rule.Enabled {
			continue
		}
		switch rule.Kind {
		case KindPrefer:
			preferOrder = append(preferOrder, rule.SourceWorkspaceID)
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

	// Ownership beats preference. An owner-bound conversation stays where it is.
	if req.OwnerWorkspaceID != "" {
		owner := st.Workspaces[req.OwnerWorkspaceID]
		if owner != nil && eligible[req.OwnerWorkspaceID] {
			d.Outcome = OutcomeSelected
			d.WorkspaceID = req.OwnerWorkspaceID
			d.Primary = ReasonOwnerBound
			d.Summary = fmt.Sprintf("Continuing on %s, which already owns this conversation.", owner.Name)
			d.Notes = append(d.Notes, Note{Code: ReasonOwnerBound, WorkspaceID: owner.ID,
				Message: "This conversation is bound to this workspace, so it does not move."})
			d.Candidates = buildCandidates(st, ids, eligible, detail, details, []string{req.OwnerWorkspaceID})
			return d
		}
		// Blocked owner: explain the constraint rather than migrating silently.
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
		d.Summary = fmt.Sprintf("This conversation is bound to %s. %s Switching workspaces mid-conversation is not safe, so start a new conversation or change the policy.", name, why)
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
