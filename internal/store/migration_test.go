package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Every existing install is sitting at some version, and every one of them has to be able to
// upgrade. This walks all of them: build a database with only the first k migrations applied,
// then open it normally and require the rest to apply cleanly and completely.
//
// What this does NOT catch, verified by trying it: a migration inserted into the middle of
// the list. This test seeds the old database from the CURRENT list, so a mutated list stays
// self-consistent with itself, while a real old install was built from the previous list.
// TestExistingMigrationsAreFrozen is the guard for insertion; this one is the guard for a
// sequence that cannot be replayed from some starting point.
func TestEveryOlderVersionCanUpgrade(t *testing.T) {
	for k := 1; k <= len(migrations); k++ {
		t.Run(fmt.Sprintf("from_v%d", k), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")

			// Stand up a database frozen at version k, the way a user's install would be.
			old, err := openRaw(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			for i := 0; i < k; i++ {
				if _, err := old.ExecContext(ctx, migrations[i]); err != nil {
					t.Fatalf("seeding v%d: %v", i+1, err)
				}
				if _, err := old.ExecContext(ctx,
					`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
					i+1, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
					t.Fatalf("recording v%d: %v", i+1, err)
				}
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}

			// Now upgrade it the way the binary would.
			db, err := Open(path)
			if err != nil {
				t.Fatalf("an install at v%d cannot upgrade: %v", k, err)
			}
			defer db.Close()

			var applied int
			if err := db.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
				t.Fatal(err)
			}
			if applied != len(migrations) {
				t.Fatalf("after upgrading from v%d, %d migrations are recorded, want %d",
					k, applied, len(migrations))
			}
			// And the upgraded schema must be usable, not merely present.
			if _, err := db.ClaimThread(ctx, "t", "ws", time.Now()); err == nil {
				t.Fatalf("claim against a workspace that does not exist should fail the foreign key")
			}
		})
	}
}

// Migrations are positional: index in the list IS the version. Adding to the end is safe;
// editing or inserting silently renumbers every migration after it, and only breaks for
// people who already have a database. Appending leaves this hash untouched, so a failure here
// means something in the existing sequence moved.
func TestExistingMigrationsAreFrozen(t *testing.T) {
	const frozenCount = 16
	const want = "f5f746a4bc89a32d5368f0d21d70c7bd8b6f7d33631693a967d34902c25b7886"

	if len(migrations) < frozenCount {
		t.Fatalf("migrations were removed: %d left, expected at least %d", len(migrations), frozenCount)
	}
	h := sha256.New()
	for i := 0; i < frozenCount; i++ {
		fmt.Fprintf(h, "%d:%s\n", i, migrations[i])
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want == "" {
		t.Logf("FINGERPRINT=%s", got)
		return
	}
	if got != want {
		t.Fatalf("the first %d migrations changed.\n  got  %s\n  want %s\n"+
			"Adding a migration to the END is fine and does not change this. Editing or "+
			"inserting one renumbers the rest and breaks every existing install.", frozenCount, got, want)
	}
}
