package service

import (
	"testing"

	"github.com/lgoyal6/codex-relay/internal/secrets"
)

// A workspace floor set through the connector reaches the routing snapshot, and clearing it
// falls back to the profile floor.
func TestSetHandoffFloorReachesSnapshot(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('a','u','a@e.com','plus','2026-09-12T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws','a','cg','WS',0,'r',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)
	c := &Connector{Svc: svc}

	zero := 0.0
	if err := c.SetHandoffFloor("ws", &zero); err != nil {
		t.Fatal(err)
	}
	got := svc.Registry.Current().Workspaces["ws"].HandoffFloorPercent
	if got == nil || *got != 0 {
		t.Fatalf("floor = %v, want 0", got)
	}

	if err := c.SetHandoffFloor("ws", nil); err != nil {
		t.Fatal(err)
	}
	if got := svc.Registry.Current().Workspaces["ws"].HandoffFloorPercent; got != nil {
		t.Fatalf("floor = %v after clear, want nil", *got)
	}

	bad := 101.0
	if err := c.SetHandoffFloor("ws", &bad); err == nil {
		t.Fatal("accepted a floor above 100")
	}
	if err := c.SetHandoffFloor("nope", &zero); err == nil {
		t.Fatal("accepted an unknown workspace")
	}
}
