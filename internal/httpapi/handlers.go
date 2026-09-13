package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/service"
)

func (a *API) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Svc.Activity(r.Context(), limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"activity": rows})
}

// handleEvents is the local event stream that keeps the dashboard live.
// Closing the dashboard simply drops a subscriber; routing is unaffected.
func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("streaming is not supported here"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	ch, cancel := a.Svc.Registry.Subscribe()
	defer cancel()

	fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "event: state\ndata: {\"version\":%d}\n\n", a.Svc.Registry.Current().Version)
			flusher.Flush()
		case <-ticker.C:
			// A comment frame keeps the connection alive through idle proxies.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

type ruleRequest struct {
	policy.Rule
}

func (a *API) handleSaveRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the rule: %w", err))
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	if err := a.Svc.SaveRule(r.Context(), req.Rule); err != nil {
		// Validation and conflict messages are written for a person and are shown verbatim.
		writeErr(w, 400, err)
		return
	}
	// Only report success after the backend has persisted and republished.
	st := a.Svc.Registry.Current()
	names := func(id string) string {
		if ws := st.Workspaces[id]; ws != nil {
			return ws.Name
		}
		return id
	}
	writeJSON(w, 200, map[string]any{
		"saved":         true,
		"id":            req.ID,
		"state_version": st.Version,
		"sentence":      req.Rule.Sentence(names),
	})
}

func (a *API) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	if err := a.Svc.DeleteRule(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "state_version": a.Svc.Registry.Current().Version})
}

type previewRequest struct {
	Rules     []policy.Rule      `json:"rules"`
	Model     string             `json:"model"`
	Scenarios []service.Scenario `json:"scenarios"`
}

func (a *API) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the preview request: %w", err))
		return
	}
	for _, rule := range req.Rules {
		if err := rule.Validate(); err != nil {
			writeErr(w, 400, err)
			return
		}
	}
	d, err := a.Svc.Preview(r.Context(), req.Rules, req.Model, req.Scenarios)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"decision": d,
		// Stated explicitly so the dashboard cannot present a preview as live state.
		"simulated":     d.Simulated,
		"state_version": d.StateVersion,
		"evaluated_at":  d.EvaluatedAt,
	})
}

func (a *API) handleConnect(w http.ResponseWriter, r *http.Request) {
	if a.Connector == nil {
		writeErr(w, 501, fmt.Errorf("connecting accounts is not available in this build"))
		return
	}
	url, flowID, err := a.Connector.BeginConnect()
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]string{"auth_url": url, "flow_id": flowID})
}

func (a *API) handleConnectComplete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FlowID string `json:"flow_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	ids, err := a.Connector.CompleteConnect(body.FlowID)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"workspace_ids": ids})
}

// handleConnectCancel abandons a sign-in that is still waiting on the browser. The dashboard
// offers a Cancel button, so the service has to be able to honour it.
func (a *API) handleConnectCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FlowID string `json:"flow_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.Connector.CancelConnect(body.FlowID)
	writeJSON(w, 200, map[string]any{"cancelled": true})
}

func (a *API) handlePause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := a.Connector.SetPaused(r.PathValue("id"), body.Paused); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"paused": body.Paused})
}

func (a *API) handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, 400, fmt.Errorf("a name is required"))
		return
	}
	if err := a.Connector.Rename(r.PathValue("id"), body.Name); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"renamed": true})
}

// handleRemovalEffect explains what removal will do BEFORE the user confirms it.
func (a *API) handleRemovalEffect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	threads, err := a.Svc.DB.ThreadsForWorkspace(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	msg := "Its stored sign-in is deleted from your OS credential store. Quota readings and history for it are removed."
	if len(threads) > 0 {
		msg = fmt.Sprintf(
			"%d conversation(s) are still bound to this workspace and will not be able to continue. %s",
			len(threads), msg)
	}
	writeJSON(w, 200, service.RemovalEffect{
		WorkspaceID: id, BoundThreads: threads, CredentialsGone: true, Explanation: msg,
	})
}

func (a *API) handleRemove(w http.ResponseWriter, r *http.Request) {
	effect, err := a.Connector.Remove(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, effect)
}

func (a *API) handleRefreshWorkspaces(w http.ResponseWriter, r *http.Request) {
	if err := a.Connector.RefreshWorkspaces(); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"refreshed": true})
}

func (a *API) handleDefaultWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkspaceID string `json:"workspace_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := a.Svc.SetSetting(r.Context(), "default_workspace_id", body.WorkspaceID); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := a.Svc.Refresh(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"default_workspace_id": body.WorkspaceID})
}

// handlePricing sets the rates behind the API-equivalent cost estimate. They are editable
// precisely because a hardcoded price presented as fact is the thing this build avoids.
func (a *API) handlePricing(w http.ResponseWriter, r *http.Request) {
	var p service.Pricing
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the rates: %w", err))
		return
	}
	if p.InputPerMTok < 0 || p.CachedPerMTok < 0 || p.OutputPerMTok < 0 {
		writeErr(w, 400, fmt.Errorf("rates cannot be negative"))
		return
	}
	if err := a.Svc.SetPricing(r.Context(), p); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, a.Svc.Pricing(r.Context()))
}

// handleDiagnostics returns a redacted report safe to share.
func (a *API) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	schema, _ := a.Svc.DB.SchemaVersion(r.Context())
	st := a.Svc.Registry.Current()

	type wsDiag struct {
		ID           string `json:"id"`
		Paused       bool   `json:"paused"`
		CredentialOK bool   `json:"credential_ok"`
		WindowCount  int    `json:"window_count"`
		ModelsKnown  bool   `json:"models_known"`
	}
	var ws []wsDiag
	for _, id := range st.Order {
		x := st.Workspaces[id]
		if x == nil {
			continue
		}
		// Identifiers only: no names, no emails, no tokens.
		ws = append(ws, wsDiag{
			ID: redactID(x.ID), Paused: x.Paused, CredentialOK: x.CredentialOK,
			WindowCount: len(x.Windows), ModelsKnown: len(x.EligibleModels) > 0,
		})
	}
	// Diagnostics forces a fresh probe: the whole point of this report is to test the
	// credential store right now, not to render quickly.
	health := a.Svc.Secrets.Probe()
	if fresh, ok := a.Svc.Secrets.(interface{ ProbeNow() secrets.Health }); ok {
		health = fresh.ProbeNow()
	}
	writeJSON(w, 200, map[string]any{
		"app_version":        a.Version,
		"os":                 runtime.GOOS,
		"arch":               runtime.GOARCH,
		"go_version":         runtime.Version(),
		"schema_version":     schema,
		"state_version":      st.Version,
		"proxy_addr":         a.ProxyAddr,
		"credential_storage": health,
		"workspaces":         ws,
		"rule_count":         len(st.Rules),
		"generated_at":       a.Svc.Clock.Now(),
		"note":               "Redacted by design: no account names, emails, tokens, conversation ids or prompt text are included.",
	})
}

// redactID keeps enough of an id to correlate rows without identifying the account.
func redactID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "..."
}

// handleListKeys returns every key, revoked included. The secret is not among them: it exists
// only in the response to the request that created it.
func (a *API) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.Svc.ListAPIKeys(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"keys":        keys,
		"require_key": a.Svc.RequireAPIKey(r.Context()),
		"header":      service.KeyHeader,
	})
}

func (a *API) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		DailyLimit *int64 `json:"daily_limit"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the request: %w", err))
		return
	}
	key, secret, err := a.Svc.CreateAPIKey(r.Context(), body.Name, body.DailyLimit)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	// The only time the secret is ever returned. It cannot be recovered afterwards.
	writeJSON(w, 200, map[string]any{"key": key, "secret": secret})
}

func (a *API) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if err := a.Svc.RevokeAPIKey(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"revoked": true})
}

// handleRequireKey locks or unlocks the proxy. Locking with no usable key would make the
// relay unreachable, so that combination is refused rather than accepted and regretted.
func (a *API) handleRequireKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Require bool `json:"require"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the request: %w", err))
		return
	}
	if body.Require {
		keys, err := a.Svc.ListAPIKeys(r.Context())
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		live := 0
		for _, k := range keys {
			if !k.Revoked() {
				live++
			}
		}
		if live == 0 {
			writeErr(w, 400, fmt.Errorf("create a key first: locking the relay with no live key would make it unreachable"))
			return
		}
	}
	v := "false"
	if body.Require {
		v = "true"
	}
	if err := a.Svc.SetSetting(r.Context(), service.RequireKeySetting, v); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"require_key": body.Require})
}

func (a *API) handleListAutomations(w http.ResponseWriter, r *http.Request) {
	list, err := a.Svc.ListAutomations(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"automations": list})
}

func (a *API) handleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		WorkspaceID string `json:"workspace_id"`
		AtMinute    *int64 `json:"at_minute"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the request: %w", err))
		return
	}
	a2, err := a.Svc.CreateAutomation(r.Context(), body.Name, body.Kind, body.WorkspaceID, body.AtMinute)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"automation": a2})
}

func (a *API) handleDeleteAutomation(w http.ResponseWriter, r *http.Request) {
	if err := a.Svc.DeleteAutomation(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true})
}

func (a *API) handleEnableAutomation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the request: %w", err))
		return
	}
	if err := a.Svc.SetAutomationEnabled(r.Context(), r.PathValue("id"), body.Enabled); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": body.Enabled})
}

// handleRollups serves long-term usage. These outlive the pruned decision rows, so this is
// the only place a question about last month can be answered.
func (a *API) handleRollups(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 365 {
		days = 30
	}
	since := a.Svc.Clock.Now().Add(-time.Duration(days) * 24 * time.Hour)
	buckets, err := a.Svc.DB.Rollups(r.Context(), since)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"days": days, "buckets": buckets})
}

func (a *API) handleNetwork(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"upstream_base":  a.Svc.UpstreamBase,
		"upstream_proxy": a.Svc.UpstreamProxyValue(r.Context()),
	})
}

func (a *API) handleSetProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, 400, fmt.Errorf("could not read the request: %w", err))
		return
	}
	if err := a.Svc.SetUpstreamProxy(r.Context(), body.Proxy); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"upstream_proxy": body.Proxy})
}
