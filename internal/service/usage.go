package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lgoyal6/codex-relay/internal/upstream"
)

// UsagePollInterval is how often each connected workspace's quota is refreshed.
//
// Quota is otherwise only learned from turns this service forwards, so a workspace that is
// paused, or simply idle, keeps whatever reading it last happened to produce. That reading
// goes stale within minutes and is void the moment its window resets, which is exactly when
// a paused workspace is most likely to be reconsidered.
//
// Five minutes sits inside the ten minute staleness budget, so a healthy poller keeps every
// reading fresh, and one missed cycle degrades to "stale" rather than to nothing.
const UsagePollInterval = 5 * time.Minute

// StartUsagePolling refreshes quota for every connected workspace until ctx is done.
//
// The endpoint reports totals and does not run a model turn, so polling costs no quota.
func (s *Service) StartUsagePolling(ctx context.Context) {
	go func() {
		// Poll once at startup so a freshly launched service does not serve a dashboard full
		// of readings from whenever it was last running.
		s.PollUsage(ctx)
		t := time.NewTicker(UsagePollInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.PollUsage(ctx)
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}
	}()
}

// PollUsage refreshes every connected workspace, including paused ones.
//
// Paused means "do not route here", not "do not observe this". A paused workspace with a
// stale reading is precisely the case that made this necessary.
func (s *Service) PollUsage(ctx context.Context) {
	if s.UpstreamBase == "" {
		return
	}
	rows, err := s.DB.SQL().QueryContext(ctx, `SELECT id FROM workspaces ORDER BY sort_order, id`)
	if err != nil {
		s.Log.Warn("could not list workspaces for usage poll", "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	for _, id := range ids {
		if err := s.pollWorkspaceUsage(ctx, id); err != nil {
			// One workspace failing must not stop the others: a single expired credential
			// would otherwise freeze quota for every workspace in the pool.
			s.Log.Warn("usage poll failed", "workspace", id, "error", err)
		}
	}
}

func (s *Service) pollWorkspaceUsage(ctx context.Context, workspaceID string) error {
	ident, err := s.Identity(ctx, workspaceID)
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(s.UpstreamBase, "/") + "/wham/usage"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ident.AccessToken)
	if ident.ChatGPTAccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", ident.ChatGPTAccountID)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("usage request returned %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	snap, err := upstream.ParseUsage(raw, s.Clock.Now())
	if err != nil {
		return err
	}
	s.observeQuota(ctx, workspaceID, []upstream.Snapshot{snap}, "usage_endpoint")
	return nil
}
