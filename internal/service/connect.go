package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lgoyal6/codex-relay/internal/oauth"
	"github.com/lgoyal6/codex-relay/internal/secrets"
)

// RemovalEffect explains what removing a workspace will do, before it is done.
type RemovalEffect struct {
	WorkspaceID     string   `json:"workspace_id"`
	BoundThreads    []string `json:"bound_threads"`
	CredentialsGone bool     `json:"credentials_gone"`
	Explanation     string   `json:"explanation"`
}

// Connector implements account sign-in and workspace management for the dashboard.
type Connector struct {
	Svc          *Service
	Issuer       string
	UpstreamBase string

	mu    sync.Mutex
	flows map[string]*oauth.Flow
}

func NewConnector(s *Service, issuer, upstreamBase string) *Connector {
	return &Connector{Svc: s, Issuer: issuer, UpstreamBase: upstreamBase, flows: map[string]*oauth.Flow{}}
}

func (c *Connector) BeginConnect() (string, string, error) {
	// Refuse to start if the credential store is not usable. Connecting an account we
	// cannot store securely would mean either losing it or writing it in plaintext.
	if h := c.Svc.Secrets.Probe(); !h.OK {
		return "", "", fmt.Errorf("%s is not available, so sign-in cannot be stored securely. %s", h.Kind, h.Remedy)
	}
	f, err := oauth.Start(c.Issuer)
	if err != nil {
		return "", "", err
	}
	id := uuid.NewString()
	c.mu.Lock()
	c.flows[id] = f
	c.mu.Unlock()
	return f.AuthURL, id, nil
}

func (c *Connector) CancelConnect(flowID string) {
	c.mu.Lock()
	f := c.flows[flowID]
	delete(c.flows, flowID)
	c.mu.Unlock()
	if f != nil {
		f.Cancel()
	}
}

// CompleteConnect waits for the browser flow, stores the credential, then discovers which
// workspaces this account can use.
func (c *Connector) CompleteConnect(flowID string) ([]string, error) {
	c.mu.Lock()
	f := c.flows[flowID]
	delete(c.flows, flowID)
	c.mu.Unlock()
	if f == nil {
		return nil, fmt.Errorf("that sign-in is no longer in progress")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tok, err := f.Wait(ctx)
	if err != nil {
		return nil, err
	}
	ident, err := oauth.ParseIdentity(tok.IDToken)
	if err != nil {
		return nil, err
	}

	now := c.Svc.Clock.Now()
	ts := now.Format(time.RFC3339Nano)

	// An account is keyed by chatgpt_user_id, never by email.
	accountID := "acct_" + ident.ChatGPTUserID
	if _, err := c.Svc.DB.SQL().ExecContext(ctx, `
		INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(chatgpt_user_id) DO UPDATE SET email=excluded.email, plan_type=excluded.plan_type`,
		accountID, ident.ChatGPTUserID, ident.Email, ident.PlanType, ts); err != nil {
		return nil, fmt.Errorf("could not save the account: %w", err)
	}

	workspaces, err := c.discoverWorkspaces(ctx, tok.AccessToken, ident)
	if err != nil {
		c.Svc.Log.Warn("workspace discovery failed; falling back to the signed-in workspace", "error", err)
		workspaces = nil
	}

	// Discovery returning an EMPTY list is not success. A personal account with no
	// additional workspaces is a real and common case, and the endpoint can answer with no
	// usable entries. Treat that exactly like a discovery failure: the identity we just
	// signed in as is itself a workspace, and it is the one the token is already scoped to.
	//
	// This was a live defect: a real sign-in created the account row, discovered zero
	// workspaces without erroring, skipped the registration loop entirely, and still
	// reported success. The user saw a completed sign-in and no connected workspace.
	workspaces = keepUsable(workspaces)
	if len(workspaces) == 0 {
		c.Svc.Log.Warn("workspace discovery returned no usable entries; using the signed-in identity",
			"has_account_id", ident.ChatGPTAccountID != "")
		if ident.ChatGPTAccountID == "" {
			// Nothing to register under. Say so plainly instead of returning success.
			return nil, fmt.Errorf(
				"sign-in completed for %s, but no ChatGPT workspace could be identified: the account "+
					"list was empty and the sign-in token carried no workspace id. Nothing was connected",
				fallbackName(ident))
		}
		workspaces = []workspaceInfo{{
			ID:        ident.ChatGPTAccountID,
			Name:      fallbackName(ident),
			Structure: "personal",
		}}
	}

	var ids []string
	for i, ws := range workspaces {
		if ws.ID == "" {
			continue
		}
		wsID := accountID + ":" + ws.ID
		ref := "workspace/" + wsID

		// Each connected identity gets its own credential entry. We never share or copy a
		// refresh token between identities or with Codex.
		if err := c.Svc.Secrets.Set(ref, secrets.Credential{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			IDToken:      tok.IDToken,
			AccountID:    ws.ID,
			ExpiresAt:    tok.ExpiresAt,
			ObtainedAt:   now,
		}); err != nil {
			return nil, fmt.Errorf("could not store the sign-in securely: %w", err)
		}
		// A reconnect writes a new credential straight to the OS store, so any cached copy
		// for this ref is now stale.
		c.Svc.Creds.Forget(ref)

		if _, err := c.Svc.DB.SQL().ExecContext(ctx, `
			INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, upstream_name,
				structure, credential_ref, credential_ok, sort_order, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,1,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
				upstream_name=excluded.upstream_name, structure=excluded.structure,
				credential_ref=excluded.credential_ref, credential_ok=1, credential_note='',
				updated_at=excluded.updated_at`,
			wsID, accountID, ws.ID, ws.Name, ws.Name, ws.Structure, ref, i, ts, ts); err != nil {
			return nil, fmt.Errorf("could not save the workspace: %w", err)
		}
		ids = append(ids, wsID)
	}

	// A connect that registered nothing is a failure, whatever happened on the way here.
	if len(ids) == 0 {
		return nil, fmt.Errorf(
			"sign-in completed but no workspace could be connected. Nothing was stored. " +
				"Check Diagnostics, then try connecting again")
	}

	if def, _ := c.Svc.GetSetting(ctx, "default_workspace_id"); def == "" && len(ids) > 0 {
		_ = c.Svc.SetSetting(ctx, "default_workspace_id", ids[0])
	}
	return ids, c.Svc.Refresh(ctx)
}

// keepUsable drops entries with no workspace id, which cannot be registered.
func keepUsable(in []workspaceInfo) []workspaceInfo {
	out := in[:0]
	for _, w := range in {
		if strings.TrimSpace(w.ID) != "" {
			out = append(out, w)
		}
	}
	return out
}

func fallbackName(i oauth.Identity) string {
	if i.Email != "" {
		return i.Email
	}
	return "Connected workspace"
}

type workspaceInfo struct {
	ID        string
	Name      string
	Structure string
}

// discoverWorkspaces reads human-readable workspace names.
//
// Codex requests id_token_add_organizations but does not parse an organizations claim; the
// names come from the accounts/check endpoint (see docs/compatibility.md).
func (c *Connector) discoverWorkspaces(ctx context.Context, accessToken string, ident oauth.Identity) ([]workspaceInfo, error) {
	url := strings.TrimSuffix(c.UpstreamBase, "/") + "/wham/accounts/check"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if ident.ChatGPTAccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", ident.ChatGPTAccountID)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("workspace list request returned %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	// The endpoint has been observed in two shapes; accept both, as the client does.
	var body struct {
		Accounts json.RawMessage `json:"accounts"`
		Ordering []string        `json:"account_ordering"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}

	// Log the SHAPE of the answer, never its contents. Workspace names and ids are personal
	// data, but "which top-level keys came back, and was accounts an object or an array"
	// is what makes an empty result diagnosable instead of a mystery. This exists because a
	// real sign-in once discovered nothing and left no trace of why.
	c.Svc.Log.Debug("accounts/check answered",
		"status", resp.StatusCode,
		"top_level_keys", jsonKeys(raw),
		"accounts_kind", jsonKind(body.Accounts),
		"accounts_len", jsonLen(body.Accounts),
		"ordering_len", len(body.Ordering))

	var list []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Structure string `json:"structure"`
	}
	if err := json.Unmarshal(body.Accounts, &list); err == nil && len(list) > 0 {
		out := make([]workspaceInfo, 0, len(list))
		for _, a := range list {
			out = append(out, workspaceInfo{ID: a.ID, Name: displayName(a.Name, a.ID, ident.Email), Structure: a.Structure})
		}
		return out, nil
	}

	var m map[string]struct {
		Account struct {
			AccountID string `json:"account_id"`
			Name      string `json:"name"`
			Structure string `json:"structure"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body.Accounts, &m); err != nil {
		return nil, fmt.Errorf("workspace list was not in a recognised shape")
	}
	order := body.Ordering
	if len(order) == 0 {
		for k := range m {
			order = append(order, k)
		}
	}
	var out []workspaceInfo
	for _, id := range order {
		if a, ok := m[id]; ok && a.Account.AccountID != "" {
			out = append(out, workspaceInfo{ID: a.Account.AccountID, Name: displayName(a.Account.Name, a.Account.AccountID, ident.Email), Structure: a.Account.Structure})
		}
	}
	return out, nil
}

// jsonKind reports whether a value is an object, array, or something else, without
// revealing any of its contents.
func jsonKind(v json.RawMessage) string {
	t := strings.TrimSpace(string(v))
	switch {
	case t == "" || t == "null":
		return "absent"
	case strings.HasPrefix(t, "{"):
		return "object"
	case strings.HasPrefix(t, "["):
		return "array"
	default:
		return "scalar"
	}
}

// jsonLen counts entries in an object or array, without revealing them.
func jsonLen(v json.RawMessage) int {
	switch jsonKind(v) {
	case "object":
		var m map[string]json.RawMessage
		if json.Unmarshal(v, &m) == nil {
			return len(m)
		}
	case "array":
		var a []json.RawMessage
		if json.Unmarshal(v, &a) == nil {
			return len(a)
		}
	}
	return 0
}

// jsonKeys lists an object's key names. Top-level response keys are API structure, not user
// data; the values they hold are never logged.
func jsonKeys(v json.RawMessage) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// displayName picks the most recognisable TRUE label available, in order: the name the
// backend gave, then the signed-in email, then a stable identifier.
//
// The email is used because a real personal workspace comes back from accounts/check with no
// name at all, and "Workspace bcd0bebd-3" tells a person nothing about which account it is.
// The email is not invented: it is the identity that signed in. It is still never used as a
// KEY, only as a label.
func displayName(name, id, email string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	if e := strings.TrimSpace(email); e != "" {
		return e
	}
	if len(id) > 10 {
		return "Workspace " + id[:10]
	}
	return "Workspace " + id
}

func (c *Connector) SetPaused(workspaceID string, paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	ctx := context.Background()
	if _, err := c.Svc.DB.SQL().ExecContext(ctx,
		`UPDATE workspaces SET paused = ?, updated_at = ? WHERE id = ?`,
		v, c.Svc.Clock.Now().Format(time.RFC3339Nano), workspaceID); err != nil {
		return err
	}
	return c.Svc.Refresh(ctx)
}

func (c *Connector) Rename(workspaceID, name string) error {
	ctx := context.Background()
	if _, err := c.Svc.DB.SQL().ExecContext(ctx,
		`UPDATE workspaces SET display_name = ?, updated_at = ? WHERE id = ?`,
		name, c.Svc.Clock.Now().Format(time.RFC3339Nano), workspaceID); err != nil {
		return err
	}
	return c.Svc.Refresh(ctx)
}

// Remove deletes a workspace and its stored credential, and says what that affected.
func (c *Connector) Remove(workspaceID string) (RemovalEffect, error) {
	ctx := context.Background()
	threads, err := c.Svc.DB.ThreadsForWorkspace(ctx, workspaceID)
	if err != nil {
		return RemovalEffect{}, err
	}
	var ref string
	_ = c.Svc.DB.SQL().QueryRowContext(ctx, `SELECT credential_ref FROM workspaces WHERE id = ?`, workspaceID).Scan(&ref)

	// Ownership rows reference the workspace; release them explicitly with a reason so the
	// history says why, rather than letting a cascade erase the record silently.
	for _, t := range threads {
		_ = c.Svc.DB.ReleaseThread(ctx, t, "workspace removed by the user", c.Svc.Clock.Now())
	}
	if _, err := c.Svc.DB.SQL().ExecContext(ctx, `DELETE FROM thread_ownership WHERE workspace_id = ?`, workspaceID); err != nil {
		return RemovalEffect{}, err
	}
	if _, err := c.Svc.DB.SQL().ExecContext(ctx, `DELETE FROM resource_ownership WHERE workspace_id = ?`, workspaceID); err != nil {
		return RemovalEffect{}, err
	}
	if _, err := c.Svc.DB.SQL().ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, workspaceID); err != nil {
		return RemovalEffect{}, err
	}
	credGone := true
	if ref != "" {
		// Drop the in-memory copy first, so a removal can never keep serving from cache
		// even if deleting from the OS store fails below.
		c.Svc.Creds.Forget(ref)
		if err := c.Svc.Secrets.Delete(ref); err != nil {
			credGone = false
			c.Svc.Log.Warn("could not delete stored credential", "error", err)
		}
	}
	msg := "The workspace was removed and its stored sign-in was deleted."
	if !credGone {
		msg = "The workspace was removed, but its stored sign-in could not be deleted from your OS credential store. Remove it manually."
	}
	if len(threads) > 0 {
		msg += fmt.Sprintf(" %d conversation(s) were bound to it and can no longer continue; start new conversations for them.", len(threads))
	}
	return RemovalEffect{
		WorkspaceID: workspaceID, BoundThreads: threads, CredentialsGone: credGone, Explanation: msg,
	}, c.Svc.Refresh(ctx)
}

// RefreshWorkspaces re-reads workspace names from upstream for every connected account.
func (c *Connector) RefreshWorkspaces() error {
	ctx := context.Background()
	rows, err := c.Svc.DB.SQL().QueryContext(ctx, `SELECT id, credential_ref, chatgpt_account_id FROM workspaces`)
	if err != nil {
		return err
	}
	type row struct{ id, ref, chatgpt string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ref, &r.chatgpt); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()

	for _, r := range all {
		cred, err := c.Svc.Creds.Access(ctx, r.ref)
		if err != nil {
			continue
		}
		infos, err := c.discoverWorkspaces(ctx, cred.AccessToken, oauth.Identity{ChatGPTAccountID: r.chatgpt})
		if err != nil {
			continue
		}
		for _, info := range infos {
			if info.ID == r.chatgpt {
				_, _ = c.Svc.DB.SQL().ExecContext(ctx,
					`UPDATE workspaces SET upstream_name = ?, structure = ?, updated_at = ? WHERE id = ?`,
					info.Name, info.Structure, c.Svc.Clock.Now().Format(time.RFC3339Nano), r.id)
			}
		}
	}
	return c.Svc.Refresh(ctx)
}
