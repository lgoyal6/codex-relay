package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lgoyal6/codex-relay/internal/secrets"
)

func gateService(t *testing.T) *Service {
	t.Helper()
	return connectService(t, secrets.NewMemory())
}

func req(key string) *http.Request {
	r := httptest.NewRequest("POST", "/backend-api/codex/responses", nil)
	if key != "" {
		r.Header.Set(KeyHeader, key)
	}
	return r
}

// Locking the relay is the whole point of the feature: without a key, nothing gets served.
func TestLockedRelayRefusesWithoutAKey(t *testing.T) {
	svc := gateService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateAPIKey(ctx, "laptop", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSetting(ctx, RequireKeySetting, "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, req("")); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("no key on a locked relay returned %v, want ErrKeyRequired", err)
	}
	if _, err := svc.Authenticate(ctx, req("crl_wrong")); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("a bogus key on a locked relay returned %v", err)
	}
}

// Unlocked is the default, so an upgrade cannot silently break a working setup.
func TestUnlockedByDefault(t *testing.T) {
	svc := gateService(t)
	id, err := svc.Authenticate(context.Background(), req(""))
	if err != nil {
		t.Fatalf("a fresh install refused an anonymous caller: %v", err)
	}
	if id != "" {
		t.Errorf("anonymous caller attributed to %q", id)
	}
}

// A daily limit has to actually stop the key, or it is decoration.
func TestDailyLimitStopsAKeyOnceReached(t *testing.T) {
	svc := gateService(t)
	ctx := context.Background()
	limit := int64(2)
	key, secret, err := svc.CreateAPIKey(ctx, "capped", &limit)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, svc, `INSERT INTO accounts (id, chatgpt_user_id, email, plan_type, created_at)
		VALUES ('a','u','a@e.com','plus','2026-09-12T00:00:00Z')`)
	mustExec(t, svc, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, paused, credential_ref, credential_ok, sort_order, created_at, updated_at)
		VALUES ('ws','a','cg','WS',0,'r',1,0,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)

	// Under the cap.
	if _, err := svc.Authenticate(ctx, req(secret)); err != nil {
		t.Fatalf("first use refused: %v", err)
	}
	now := svc.Clock.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	for i := 0; i < 2; i++ {
		if _, err := svc.DB.SQL().ExecContext(ctx,
			`INSERT INTO decisions (at, outcome, primary_reason, summary, detail_json, state_version, attempt, api_key_id, workspace_id)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			now, "selected", "default_workspace", "s", "{}", 1, 1, key.ID, "ws"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Authenticate(ctx, req(secret)); !errors.Is(err, ErrKeyOverLimit) {
		t.Fatalf("a key at its daily limit returned %v, want ErrKeyOverLimit", err)
	}
}

// Revoking has to take effect for the proxy, not just the list view.
func TestRevokedKeyStopsAuthenticating(t *testing.T) {
	svc := gateService(t)
	ctx := context.Background()
	key, secret, err := svc.CreateAPIKey(ctx, "temp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSetting(ctx, RequireKeySetting, "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, req(secret)); err != nil {
		t.Fatalf("a live key was refused: %v", err)
	}
	if err := svc.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, req(secret)); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("a revoked key still authenticates: %v", err)
	}
}
