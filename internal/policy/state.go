package policy

import "time"

// Evidence describes how much we trust a quota reading. Missing and stale evidence is
// explicit and is never rounded down to "fine".
type Evidence string

const (
	// EvidenceFresh means the reading came from a real upstream response recently.
	EvidenceFresh Evidence = "fresh"
	// EvidenceStale means we have a reading but it is older than the staleness budget.
	EvidenceStale Evidence = "stale"
	// EvidenceMissing means we have never observed this window for this workspace.
	EvidenceMissing Evidence = "missing"
)

// Window is one observed quota window for one workspace.
//
// Codex reports used_percent; there is no remaining field on the wire. Remaining is derived
// here in exactly one place so the whole product agrees on it.
type Window struct {
	Minutes     int64      `json:"minutes"`
	UsedPercent float64    `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at"`
	ObservedAt  time.Time  `json:"observed_at"`
}

// RemainingPercent is 100 - used_percent, clamped to [0,100].
func (w Window) RemainingPercent() float64 {
	r := 100 - w.UsedPercent
	if r < 0 {
		return 0
	}
	if r > 100 {
		return 100
	}
	return r
}

// WorkspaceState is everything the evaluator knows about one workspace.
type WorkspaceState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// AccountID is the identity that owns this workspace. An account and a workspace are
	// distinct identities and neither is keyed by email.
	AccountID string `json:"account_id"`

	// Paused stops admitting new conversations without deleting credentials.
	Paused bool `json:"paused"`
	// CredentialOK is false when the stored credential is missing, expired or rejected.
	CredentialOK   bool   `json:"credential_ok"`
	CredentialNote string `json:"credential_note,omitempty"`

	// Windows is keyed by window_minutes.
	Windows map[int64]Window `json:"windows"`

	// EligibleModels lists model slugs this workspace reported. An empty map means we have
	// not observed a model list, which is Unknown, not "supports nothing".
	EligibleModels   map[string]bool `json:"eligible_models"`
	ModelsObservedAt *time.Time      `json:"models_observed_at"`
}

// State is the immutable snapshot the evaluator reads. Snapshots are built once and
// published atomically; the evaluator never reaches into a database.
type State struct {
	Version    int64                      `json:"version"`
	Workspaces map[string]*WorkspaceState `json:"workspaces"`
	// Order is the deterministic workspace ordering used to break ties.
	Order []string `json:"order"`
	// DefaultWorkspaceID is the workspace used when nothing else orders the choice.
	DefaultWorkspaceID string          `json:"default_workspace_id"`
	Rules              []Rule          `json:"rules"`
	ActiveProfile      *RoutingProfile `json:"active_profile,omitempty"`
	// StaleAfter is how old a reading may be before it is labelled stale.
	StaleAfter time.Duration `json:"stale_after"`
	// HandoffBelowPercent moves an owner-bound conversation to another workspace once the
	// owner has this little quota left, instead of letting the next turn fail. Zero disables
	// handoff, and the conversation blocks on its owner as before.
	HandoffBelowPercent float64 `json:"handoff_below_percent"`
}

// EvidenceFor classifies a workspace window at time now.
func (s *State) EvidenceFor(ws *WorkspaceState, minutes int64, now time.Time) (Window, Evidence) {
	w, ok := ws.Windows[minutes]
	if !ok {
		return Window{}, EvidenceMissing
	}
	// A reading describes one specific window. Once that window's reset time has passed the
	// window no longer exists, and the percentage in hand says nothing about the new one, no
	// matter how recently it was observed. Age alone would call a two-minute-old reading
	// fresh at 95% used one minute after the counter reset to zero.
	if w.ResetsAt != nil && !now.Before(*w.ResetsAt) {
		return Window{}, EvidenceMissing
	}
	if s.StaleAfter > 0 && now.Sub(w.ObservedAt) > s.StaleAfter {
		return w, EvidenceStale
	}
	return w, EvidenceFresh
}
