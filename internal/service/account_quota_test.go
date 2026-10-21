package service

import (
	"context"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

func TestUsageSnapshotRemovesWindowsNoLongerReported(t *testing.T) {
	for _, source := range []string{"usage_endpoint", "response_headers"} {
		t.Run(source, func(t *testing.T) {
			svc := connectService(t, secrets.NewMemory())
			mustExec(t, svc, `INSERT INTO accounts (id,chatgpt_user_id,plan_type,created_at) VALUES ('acct','user','prolite','2026-10-19T00:00:00Z')`)
			mustExec(t, svc, `INSERT INTO workspaces (id,account_id,chatgpt_account_id,display_name,credential_ref,credential_ok,created_at,updated_at)
				VALUES ('ws','acct','cg','Pro','ref',1,'2026-10-19T00:00:00Z','2026-10-19T00:00:00Z')`)
			ctx := context.Background()
			reset := svc.Clock.Now().Add(7 * 24 * time.Hour)
			svc.observeQuota(ctx, "ws", []upstream.Snapshot{{LimitID: "codex",
				Primary:   &upstream.Window{Minutes: 1440, UsedPercent: 0, ResetsAt: &reset},
				Secondary: &upstream.Window{Minutes: 10080, UsedPercent: 10, ResetsAt: &reset},
			}}, "usage_endpoint")
			svc.observeQuota(ctx, "ws", []upstream.Snapshot{{LimitID: "codex",
				Primary: &upstream.Window{Minutes: 10080, UsedPercent: 13, ResetsAt: &reset},
			}}, source)
			ws := svc.Registry.Current().Workspaces["ws"]
			_, hasDaily := ws.Windows[1440]
			if hasDaily != (source == "response_headers") {
				t.Fatalf("daily window present = %v after %s; only a complete usage snapshot may remove it", hasDaily, source)
			}
			if got := ws.Windows[10080].RemainingPercent(); got != 87 {
				t.Fatalf("weekly remaining = %v, want 87", got)
			}
		})
	}
}
