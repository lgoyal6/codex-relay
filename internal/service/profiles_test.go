package service

import (
	"context"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/secrets"
)

func TestRoutingProfilesPersistAndActivateByAlias(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	ctx := context.Background()
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, created_at) VALUES ('a','u','2026-09-20T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces
		(id, account_id, chatgpt_account_id, display_name, credential_ref, credential_ok, created_at, updated_at)
		VALUES ('free','a','cg','Free','ref',1,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`)
	if err := svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	saved, err := svc.SaveRoutingProfile(ctx, policy.RoutingProfile{
		Name: "Free first", Command: "free", Aliases: []string{"his"}, Mode: policy.ProfilePriority,
		PriorityWorkspaceIDs: []string{"free"}, DefaultWorkspaceID: "free", HandoffBelowPercent: 4,
		SubagentHelperEnabled: true, SubagentHelperWorkspaceID: "free", SubagentHelperModel: "gpt-5.6-luna",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || saved.Command != "free" || len(saved.Aliases) != 1 {
		t.Fatalf("saved profile = %+v", saved)
	}
	if !saved.SubagentHelperEnabled || saved.SubagentHelperWorkspaceID != "free" || saved.SubagentHelperModel != "gpt-5.6-luna" {
		t.Fatalf("helper settings were not persisted: %+v", saved)
	}
	activated, err := svc.ActivateRoutingProfile(ctx, "his")
	if err != nil {
		t.Fatal(err)
	}
	if activated.ID != saved.ID {
		t.Fatalf("activated %q, want %q", activated.ID, saved.ID)
	}
	st := svc.Registry.Current()
	if st.ActiveProfile == nil || st.ActiveProfile.ID != saved.ID {
		t.Fatalf("active profile = %+v", st.ActiveProfile)
	}
	if st.DefaultWorkspaceID != "free" || st.HandoffBelowPercent != 4 {
		t.Fatalf("profile settings not published: default=%q handoff=%g", st.DefaultWorkspaceID, st.HandoffBelowPercent)
	}
	if d := policy.Evaluate(st, policy.Request{}, time.Now().UTC()); d.WorkspaceID != "free" {
		t.Fatalf("active profile selected %q", d.WorkspaceID)
	}
	if err := svc.DeleteRoutingProfile(ctx, saved.ID); err == nil {
		t.Fatal("active profile was deleted")
	}
}

func TestRoutingProfileCommandsAndAliasesAreGloballyUnique(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	ctx := context.Background()
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, created_at) VALUES ('a','u','2026-09-20T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces
		(id, account_id, chatgpt_account_id, display_name, credential_ref, credential_ok, created_at, updated_at)
		VALUES ('ws','a','cg','WS','ref',1,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`)
	base := policy.RoutingProfile{Name: "One", Command: "one", Aliases: []string{"first"}, Mode: policy.ProfilePriority, PriorityWorkspaceIDs: []string{"ws"}}
	if _, err := svc.SaveRoutingProfile(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.ID, base.Name, base.Command, base.Aliases = "", "Two", "two", []string{"first"}
	if _, err := svc.SaveRoutingProfile(ctx, base); err == nil {
		t.Fatal("duplicate alias was accepted")
	}
}
