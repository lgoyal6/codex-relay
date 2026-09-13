package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Two turns of the same conversation can arrive at once: Codex pipelines, and a retry can
// overlap the turn it is retrying. If both were allowed to bind, one conversation would be
// split across two accounts, billing both and breaking continuity. Exactly one writer must
// win, and every loser must be told, not silently ignored.
func TestConcurrentClaimsOnOneThreadElectExactlyOneWinner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now()

	const racers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := map[string]int{}
	conflicts := 0
	other := 0

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		ws := "personal"
		if i%2 == 1 {
			ws = "work"
		}
		wg.Add(1)
		go func(ws string) {
			defer wg.Done()
			<-start
			got, err := db.ClaimThread(ctx, "shared-thread", ws, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners[got.WorkspaceID]++
			case errors.Is(err, ErrOwnedByOther):
				conflicts++
			default:
				other++
			}
		}(ws)
	}
	close(start)
	wg.Wait()

	if other != 0 {
		t.Fatalf("%d claims failed for an unexpected reason", other)
	}
	if len(winners) != 1 {
		t.Fatalf("the conversation bound to %d different workspaces: %v", len(winners), winners)
	}
	if winners["personal"]+winners["work"]+conflicts != racers {
		t.Fatalf("claims lost: %v winners, %d conflicts, want %d total", winners, conflicts, racers)
	}

	// And the binding that survived is the one the database reports afterwards.
	owner, ok, err := db.OwnerOf(ctx, "shared-thread")
	if err != nil || !ok {
		t.Fatalf("ownership lookup after the race: ok=%v err=%v", ok, err)
	}
	if winners[owner] == 0 {
		t.Fatalf("stored owner %q was never reported as a winner: %v", owner, winners)
	}
}

// A handoff racing the ordinary bind must still leave exactly one owner. Reassign is allowed
// to move a conversation where Claim is not, so the two together are the pairing most likely
// to produce a split binding.
func TestReassignRacingClaimLeavesOneOwner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := db.ClaimThread(ctx, "t", "personal", now); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_, _ = db.ReassignThread(ctx, "t", "work", now)
			} else {
				_, _ = db.ClaimThread(ctx, "t", "personal", now)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	var rows int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM thread_ownership WHERE thread_id='t'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("thread has %d ownership rows, want exactly 1", rows)
	}
	owner, ok, err := db.OwnerOf(ctx, "t")
	if err != nil || !ok {
		t.Fatalf("no owner after the race: %v", err)
	}
	if owner != "personal" && owner != "work" {
		t.Fatalf("owner is %q, which is neither racer", owner)
	}
}
