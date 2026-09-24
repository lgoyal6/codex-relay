// Package httpapi serves the dashboard and its JSON API on loopback.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/service"
)

type API struct {
	Svc   *service.Service
	Guard *Guard
	// ProxyAddr is where Codex should point, shown in setup and diagnostics.
	ProxyAddr string
	Version   string
	Connector Connector
}

// Connector performs account sign-in and workspace management. It is an interface so the
// API can be tested without a browser.
type Connector interface {
	BeginConnect() (authURL string, flowID string, err error)
	CompleteConnect(flowID string) (workspaceIDs []string, err error)
	CancelConnect(flowID string)
	SetPaused(workspaceID string, paused bool) error
	Rename(workspaceID, name string) error
	Remove(workspaceID string) (service.RemovalEffect, error)
	RefreshWorkspaces() error
}

func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/activity", a.handleActivity)
	mux.HandleFunc("GET /api/activity.csv", a.handleActivityCSV)
	mux.HandleFunc("GET /api/models", a.handleModels)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("GET /api/diagnostics", a.handleDiagnostics)

	mux.HandleFunc("POST /api/rules", a.handleSaveRule)
	mux.HandleFunc("POST /api/rules/preview", a.handlePreview)
	mux.HandleFunc("POST /api/routing/simulate", a.handleSimulate)
	mux.HandleFunc("DELETE /api/rules/{id}", a.handleDeleteRule)
	mux.HandleFunc("GET /api/profiles", a.handleListProfiles)
	mux.HandleFunc("POST /api/profiles", a.handleSaveProfile)
	mux.HandleFunc("DELETE /api/profiles/{id}", a.handleDeleteProfile)
	mux.HandleFunc("POST /api/profiles/{command}/activate", a.handleActivateProfile)

	mux.HandleFunc("POST /api/workspaces/connect", a.handleConnect)
	mux.HandleFunc("POST /api/workspaces/connect/complete", a.handleConnectComplete)
	mux.HandleFunc("POST /api/workspaces/connect/cancel", a.handleConnectCancel)
	mux.HandleFunc("POST /api/workspaces/{id}/pause", a.handlePause)
	mux.HandleFunc("POST /api/workspaces/{id}/rename", a.handleRename)
	mux.HandleFunc("GET /api/workspaces/{id}/removal-effect", a.handleRemovalEffect)
	mux.HandleFunc("DELETE /api/workspaces/{id}", a.handleRemove)
	mux.HandleFunc("POST /api/workspaces/refresh", a.handleRefreshWorkspaces)
	mux.HandleFunc("POST /api/settings/default-workspace", a.handleDefaultWorkspace)
	mux.HandleFunc("POST /api/settings/pricing", a.handlePricing)

	mux.HandleFunc("GET /api/keys", a.handleListKeys)
	mux.HandleFunc("POST /api/keys", a.handleCreateKey)
	mux.HandleFunc("DELETE /api/keys/{id}", a.handleRevokeKey)
	mux.HandleFunc("POST /api/settings/require-key", a.handleRequireKey)

	mux.HandleFunc("GET /api/automations", a.handleListAutomations)
	mux.HandleFunc("POST /api/automations", a.handleCreateAutomation)
	mux.HandleFunc("DELETE /api/automations/{id}", a.handleDeleteAutomation)
	mux.HandleFunc("POST /api/automations/{id}/enabled", a.handleEnableAutomation)

	mux.HandleFunc("GET /api/rollups", a.handleRollups)
	mux.HandleFunc("GET /api/network", a.handleNetwork)
	mux.HandleFunc("POST /api/network/proxy", a.handleSetProxy)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// StateResponse is everything Overview needs in one read.
type StateResponse struct {
	Version            int64                 `json:"version"`
	Workspaces         []WorkspaceView       `json:"workspaces"`
	Rules              []RuleView            `json:"rules"`
	Proposed           policy.Decision       `json:"proposed"`
	Problems           []Problem             `json:"problems"`
	ProxyAddr          string                `json:"proxy_addr"`
	AppVersion         string                `json:"app_version"`
	Now                time.Time             `json:"now"`
	Credential         secrets.Health        `json:"credential_storage"`
	DefaultWorkspaceID string                `json:"default_workspace_id"`
	Summary            Summary               `json:"summary"`
	Projections        []Projection          `json:"projections"`
	PoolTotals         []PoolTotal           `json:"pool_totals"`
	Profiles           []ProfileView         `json:"profiles"`
	ActiveProfileID    string                `json:"active_profile_id,omitempty"`
	PaceStandings      []policy.PaceStanding `json:"pace_standings"`
}

// Summary holds the headline counters the Overview tiles render.
//
// Everything here is COUNTED from what actually happened. There is deliberately no estimated
// API cost and no burn projection: the contract forbids implying an API-equivalent estimate is
// real subscription spend, and defers prediction-driven scheduling. A tile that cannot be
// filled honestly is absent rather than guessed.
type Summary struct {
	WindowHours int `json:"window_hours"`

	Turns         int `json:"turns"`
	TurnsServed   int `json:"turns_served"`
	TurnsBlocked  int `json:"turns_blocked"`
	Conversations int `json:"conversations"`
	Workspaces    int `json:"workspaces"`
	RulesActive   int `json:"rules_active"`

	// MedianFirstTokenMS is -1 when nothing was measured in the window.
	MedianFirstTokenMS int64   `json:"median_first_token_ms"`
	ErrorRatePercent   float64 `json:"error_rate_percent"`
	TopErrorClass      string  `json:"top_error_class,omitempty"`

	// Tokens are the models' OWN reported counts, never estimated from text.
	InputTokens  int64 `json:"input_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	// TurnsWithUsage is how many turns actually reported tokens. When it is lower than
	// Turns, every token figure covers only part of the window and the UI says so.
	TurnsWithUsage int `json:"turns_with_usage"`

	// EstimatedCost is API-EQUIVALENT, not subscription spend. See Pricing.
	EstimatedCost float64         `json:"estimated_cost"`
	Pricing       service.Pricing `json:"pricing"`

	// Series are observed history for the sparklines, oldest first.
	TurnsSeries   []float64 `json:"turns_series"`
	ErrorSeries   []float64 `json:"error_series"`
	LatencySeries []float64 `json:"latency_series"`
	TokenSeries   []float64 `json:"token_series"`
	CostSeries    []float64 `json:"cost_series"`
}

// Projection is a forward estimate for one quota window of one workspace.
//
// It is a straight-line extrapolation of the rate ALREADY OBSERVED in this window, and
// nothing more. It does not model your habits, the time of day, or anything else. Whether it
// is worth trusting depends entirely on whether the next few hours look like the last few,
// which is why Basis and Confidence travel with it.
type Projection struct {
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	WindowMinutes int64  `json:"window_minutes"`
	WindowLabel   string `json:"window_label"`

	RemainingPercent float64 `json:"remaining_percent"`
	// BurnPercentPerHour is the observed rate of consumption in this window.
	BurnPercentPerHour float64 `json:"burn_percent_per_hour"`
	// HoursToEmpty is -1 when nothing is being consumed, so nothing runs out.
	HoursToEmpty float64 `json:"hours_to_empty"`
	HoursToReset float64 `json:"hours_to_reset"`
	// WillRunOutBeforeReset is only meaningful when Confidence is not "none".
	WillRunOutBeforeReset bool `json:"will_run_out_before_reset"`
	// Confidence is none, low, or moderate. It is never "high": this is a straight line.
	Confidence string `json:"confidence"`
	Basis      string `json:"basis"`
}

// PoolTotal is the combined view across workspaces for one window duration.
//
// Adding unlike plans together is not arithmetic anyone should act on blindly, so the total
// travels with the plan mix it came from and a note saying what it does and does not mean.
type PoolTotal struct {
	WindowMinutes    int64   `json:"window_minutes"`
	WindowLabel      string  `json:"window_label"`
	Workspaces       int     `json:"workspaces"`
	AverageRemaining float64 `json:"average_remaining_percent"`
	// AccountEquivalents expresses the pool as "how many full accounts worth is left".
	AccountEquivalents float64  `json:"account_equivalents"`
	Plans              []string `json:"plans"`
	MixedPlans         bool     `json:"mixed_plans"`
}

// WindowView is one quota window as the dashboard renders it. Every field the UI shows is
// derived here so the frontend never invents a number.
type WindowView struct {
	Minutes          int64      `json:"minutes"`
	Label            string     `json:"label"`
	RemainingPercent float64    `json:"remaining_percent"`
	ResetsAt         *time.Time `json:"resets_at"`
	ObservedAt       time.Time  `json:"observed_at"`
	Evidence         string     `json:"evidence"`
	// ReserveMarkerPercent is where a reserve rule's threshold sits on this bar, if any.
	ReserveMarkerPercent *float64 `json:"reserve_marker_percent,omitempty"`
}

type WorkspaceView struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	AccountID      string       `json:"account_id"`
	Paused         bool         `json:"paused"`
	CredentialOK   bool         `json:"credential_ok"`
	CredentialNote string       `json:"credential_note,omitempty"`
	Protected      bool         `json:"protected"`
	ProtectedBy    string       `json:"protected_by,omitempty"`
	Windows        []WindowView `json:"windows"`
	ModelsKnown    bool         `json:"models_known"`
}

type RuleView struct {
	policy.Rule
	Sentence string `json:"sentence"`
}

type ProfileView struct {
	policy.RoutingProfile
	Active   bool   `json:"active"`
	Sentence string `json:"sentence"`
}

type Problem struct {
	Kind        string `json:"kind"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Message     string `json:"message"`
	ActionLabel string `json:"action_label"`
	Action      string `json:"action"`
}

func (a *API) handleState(w http.ResponseWriter, r *http.Request) {
	st := a.Svc.Registry.Current()
	now := a.Svc.Clock.Now()

	// The headline answer: which workspace a new conversation would use, and why.
	proposed := policy.Evaluate(st, policy.Request{}, now)

	protectedBy := map[string]policy.Rule{}
	markers := map[string]map[int64]float64{}
	for _, rule := range st.Rules {
		if !rule.Enabled || rule.Kind != policy.KindReserve {
			continue
		}
		if markers[rule.SourceWorkspaceID] == nil {
			markers[rule.SourceWorkspaceID] = map[int64]float64{}
		}
		markers[rule.SourceWorkspaceID][rule.Window.Minutes] = rule.RemainingPercent
	}
	for _, c := range proposed.Candidates {
		if c.Reason == policy.ReasonProtected || c.Reason == policy.ReasonProtectionUnknown {
			for _, rule := range st.Rules {
				if rule.Kind == policy.KindReserve && rule.SourceWorkspaceID == c.WorkspaceID {
					protectedBy[c.WorkspaceID] = rule
				}
			}
		}
	}

	resp := StateResponse{
		Version:            st.Version,
		Proposed:           proposed,
		ProxyAddr:          a.ProxyAddr,
		AppVersion:         a.Version,
		Now:                now,
		DefaultWorkspaceID: st.DefaultWorkspaceID,
		Credential:         a.Svc.Secrets.Probe(),
		Workspaces:         []WorkspaceView{},
		Rules:              []RuleView{},
		Profiles:           []ProfileView{},
		PaceStandings:      []policy.PaceStanding{},
		Problems:           []Problem{},
	}

	names := func(id string) string {
		if ws := st.Workspaces[id]; ws != nil {
			return ws.Name
		}
		return id
	}

	for _, id := range st.Order {
		ws := st.Workspaces[id]
		if ws == nil {
			continue
		}
		v := WorkspaceView{
			ID: ws.ID, Name: ws.Name, AccountID: ws.AccountID,
			Paused: ws.Paused, CredentialOK: ws.CredentialOK, CredentialNote: ws.CredentialNote,
			ModelsKnown: len(ws.EligibleModels) > 0,
			Windows:     []WindowView{},
		}
		if rule, ok := protectedBy[id]; ok {
			v.Protected = true
			v.ProtectedBy = rule.ID
		}
		for minutes, win := range ws.Windows {
			_, ev := st.EvidenceFor(ws, minutes, now)
			wv := WindowView{
				Minutes:          minutes,
				Label:            policy.HumanWindow(minutes),
				RemainingPercent: win.RemainingPercent(),
				ResetsAt:         win.ResetsAt,
				ObservedAt:       win.ObservedAt,
				Evidence:         string(ev),
			}
			if m, ok := markers[id][minutes]; ok {
				mm := m
				wv.ReserveMarkerPercent = &mm
			}
			v.Windows = append(v.Windows, wv)
		}
		sortWindows(v.Windows)
		resp.Workspaces = append(resp.Workspaces, v)

		if !ws.CredentialOK {
			resp.Problems = append(resp.Problems, Problem{
				Kind: "credential", WorkspaceID: ws.ID,
				Message:     fmt.Sprintf("%s needs to be reconnected. %s", ws.Name, ws.CredentialNote),
				ActionLabel: "Reconnect", Action: "reconnect",
			})
		}
		if len(ws.Windows) == 0 && ws.CredentialOK && !ws.Paused {
			resp.Problems = append(resp.Problems, Problem{
				Kind: "no_quota_evidence", WorkspaceID: ws.ID,
				Message:     fmt.Sprintf("No quota reading for %s yet. Quota appears after its first routed turn.", ws.Name),
				ActionLabel: "How quota is read", Action: "explain_quota",
			})
		}
	}

	for _, rule := range st.Rules {
		resp.Rules = append(resp.Rules, RuleView{Rule: rule, Sentence: rule.Sentence(names)})
	}
	profiles, err := a.Svc.RoutingProfiles(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if st.ActiveProfile != nil {
		resp.ActiveProfileID = st.ActiveProfile.ID
		resp.PaceStandings = policy.PaceStandings(st, now)
	}
	for _, profile := range profiles {
		resp.Profiles = append(resp.Profiles, ProfileView{
			RoutingProfile: profile,
			Active:         st.ActiveProfile != nil && st.ActiveProfile.ID == profile.ID,
			Sentence:       profile.Sentence(names),
		})
	}

	if proposed.Outcome != policy.OutcomeSelected {
		resp.Problems = append(resp.Problems, Problem{
			Kind: "blocked", Message: proposed.Summary,
			ActionLabel: "Review rules", Action: "open_rules",
		})
	}
	if h := resp.Credential; !h.OK {
		resp.Problems = append(resp.Problems, Problem{
			Kind: "credential_storage", Message: h.Detail + " " + h.Remedy,
			ActionLabel: "Diagnostics", Action: "open_diagnostics",
		})
	}

	activeRules := 0
	for _, r := range st.Rules {
		if r.Enabled {
			activeRules++
		}
	}
	resp.Summary = a.buildSummary(r.Context(), now, len(resp.Workspaces), activeRules)
	resp.Projections = buildProjections(st, now)

	plans := map[string]string{}
	if rows, err := a.Svc.DB.SQL().QueryContext(r.Context(), `SELECT id, plan_type FROM accounts`); err == nil {
		for rows.Next() {
			var id, plan string
			if rows.Scan(&id, &plan) == nil {
				plans[id] = plan
			}
		}
		rows.Close()
	}
	resp.PoolTotals = buildPoolTotals(st, now, plans)

	writeJSON(w, 200, resp)
}

func sortWindows(ws []WindowView) {
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && ws[j].Minutes < ws[j-1].Minutes; j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}
