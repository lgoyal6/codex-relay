package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO accounts (id, chatgpt_user_id, created_at) VALUES ('a','u1',?)`, now); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"personal", "work"} {
		if _, err := db.SQL().ExecContext(ctx, `
			INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, credential_ref, created_at, updated_at)
			VALUES (?, 'a', ?, ?, ?, ?, ?)`, w, "acct_"+w, w, "ref_"+w, now, now); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestClaimIsIdempotentForSameWorkspace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first, err := db.ClaimThread(ctx, "t1", "personal", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.ClaimThread(ctx, "t1", "personal", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-claiming the same binding must succeed: %v", err)
	}
	if second.WorkspaceID != "personal" {
		t.Fatalf("owner changed: %q", second.WorkspaceID)
	}
	if !second.LastSeenAt.After(first.LastSeenAt) {
		t.Fatal("last_seen_at should advance on re-claim")
	}
	if !second.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Fatal("first_seen_at must not move")
	}
}

func TestClaimRefusesToStealAnExistingBinding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := db.ClaimThread(ctx, "t1", "personal", now); err != nil {
		t.Fatal(err)
	}
	got, err := db.ClaimThread(ctx, "t1", "work", now)
	if !errors.Is(err, ErrOwnedByOther) {
		t.Fatalf("expected ErrOwnedByOther, got %v", err)
	}
	if got.WorkspaceID != "personal" {
		t.Fatalf("must report the real owner, got %q", got.WorkspaceID)
	}
	owner, ok, err := db.OwnerOf(ctx, "t1")
	if err != nil || !ok || owner != "personal" {
		t.Fatalf("binding must be unchanged after a refused steal: %q ok=%v err=%v", owner, ok, err)
	}
}

// TestConcurrentClaimsElectExactlyOneOwner is the simultaneous-turns case from the contract.
// Without the atomic upsert, two turns starting the same thread could each believe they own it.
func TestConcurrentClaimsElectExactlyOneOwner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const n = 40
	var wg sync.WaitGroup
	winners := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws := "personal"
			if i%2 == 1 {
				ws = "work"
			}
			<-start
			got, err := db.ClaimThread(ctx, "race", ws, now)
			winners[i], errs[i] = got.WorkspaceID, err
		}(i)
	}
	close(start)
	wg.Wait()

	owner, ok, err := db.OwnerOf(ctx, "race")
	if err != nil || !ok {
		t.Fatalf("a winner must exist: %v ok=%v", err, ok)
	}
	for i := range winners {
		if winners[i] != owner {
			t.Fatalf("claim %d reported owner %q but the stored owner is %q", i, winners[i], owner)
		}
		if errs[i] != nil && !errors.Is(errs[i], ErrOwnedByOther) {
			t.Fatalf("claim %d unexpected error: %v", i, errs[i])
		}
		// A loser must be told it lost.
		if winners[i] == owner && errs[i] != nil && !errors.Is(errs[i], ErrOwnedByOther) {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
	}
}

func TestOwnershipSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.db")
	ctx := context.Background()
	now := time.Now().UTC()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := now.Format(time.RFC3339Nano)
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO accounts (id, chatgpt_user_id, created_at) VALUES ('a','u1',?)`, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, credential_ref, created_at, updated_at) VALUES ('personal','a','acct','Personal','ref',?,?)`, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimThread(ctx, "t1", "personal", now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	owner, ok, err := reopened.OwnerOf(ctx, "t1")
	if err != nil || !ok || owner != "personal" {
		t.Fatalf("ownership must survive restart: %q ok=%v err=%v", owner, ok, err)
	}
}

func TestReleaseRequiresAReason(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := db.ClaimThread(ctx, "t1", "personal", now); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseThread(ctx, "t1", "", now); err == nil {
		t.Fatal("releasing ownership without a recorded reason must be refused")
	}
	if err := db.ReleaseThread(ctx, "t1", "user started a fresh conversation", now); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.OwnerOf(ctx, "t1"); ok {
		t.Fatal("a released thread must no longer report an owner")
	}
}

func TestResourceOwnershipIsKindScoped(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.RecordResource(ctx, "response", "id_1", "personal", "t1", now); err != nil {
		t.Fatal(err)
	}
	// The same literal id under a different kind is a different resource.
	if err := db.RecordResource(ctx, "file", "id_1", "work", "t2", now); err != nil {
		t.Fatal(err)
	}
	if ws, _, _ := db.ResourceOwner(ctx, "response", "id_1"); ws != "personal" {
		t.Fatalf("response id_1 owner = %q", ws)
	}
	if ws, _, _ := db.ResourceOwner(ctx, "file", "id_1"); ws != "work" {
		t.Fatalf("file id_1 owner = %q", ws)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	for i := 0; i < 3; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		v, err := db.SchemaVersion(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if v != len(migrations) {
			t.Fatalf("schema version %d, want %d", v, len(migrations))
		}
		_ = db.Close()
	}
}

// TestSchemaHasEveryExpectedColumn guards the append-only migration rule.
//
// Inserting a migration in the MIDDLE of the list renumbers every later one, so a database
// that already recorded those versions re-runs the wrong statements. That happened: four
// ALTERs were added mid-list and an existing install failed with "table integration_changes
// already exists" while silently missing a column. This test fails if a fresh database does
// not end up with the full expected schema.
func TestSchemaHasEveryExpectedColumn(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	want := map[string][]string{
		"decisions": {
			"at", "thread_id", "model", "outcome", "workspace_id", "primary_reason", "summary",
			"detail_json", "state_version", "attempt", "status_code", "first_token_ms",
			"total_ms", "error_class",
			"input_tokens", "cached_input_tokens", "output_tokens", "total_tokens",
			"quota_snapshot_json",
		},
		"workspaces":          {"id", "account_id", "chatgpt_account_id", "display_name", "credential_ref", "credential_ok", "paused"},
		"quota_windows":       {"workspace_id", "limit_id", "window_minutes", "used_percent", "resets_at", "observed_at"},
		"thread_ownership":    {"thread_id", "workspace_id", "first_seen_at", "last_seen_at", "released_at"},
		"integration_changes": {"target_path", "backup_path", "before_sha256", "after_sha256", "rolled_back_at"},
		"routing_profiles":    {"id", "name", "command", "aliases_json", "mode", "priority_workspace_ids_json", "pace_workspace_ids_json", "overflow_workspace_id", "target_remaining_percent", "default_workspace_id", "handoff_below_percent", "disabled_workspace_ids_json"},
	}

	for table, cols := range want {
		have := map[string]bool{}
		rows, err := db.SQL().QueryContext(ctx,
			`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			have[n] = true
		}
		rows.Close()
		if len(have) == 0 {
			t.Errorf("table %q does not exist in a fresh database", table)
			continue
		}
		for _, c := range cols {
			if !have[c] {
				t.Errorf("table %q is missing column %q in a fresh database", table, c)
			}
		}
	}

	v, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Errorf("a fresh database applied %d migrations, expected all %d", v, len(migrations))
	}
}
