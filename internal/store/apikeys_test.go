package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The secret must never be recoverable from the database. If it were, a readable file would
// hand someone the pool, which is the whole reason keys are hashed rather than stored.
func TestTheSecretIsNeverStored(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, secret, err := db.CreateAPIKey(ctx, "k1", "laptop", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.SQL().QueryContext(ctx, `SELECT id, name, prefix, key_hash FROM api_keys`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	body := strings.TrimPrefix(secret, KeyPrefix)
	for rows.Next() {
		var id, name, prefix, hash string
		if err := rows.Scan(&id, &name, &prefix, &hash); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{id, name, hash} {
			if strings.Contains(field, body) {
				t.Fatalf("the raw secret appears in the database: %q", field)
			}
		}
		// The prefix is deliberately a fragment, for identification only.
		if len(prefix) >= len(secret) {
			t.Fatalf("prefix %q is the whole key", prefix)
		}
	}
}

// A key must authenticate, and only the exact key.
func TestLookupAcceptsOnlyTheExactKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rec, secret, err := db.CreateAPIKey(ctx, "k1", "laptop", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.LookupAPIKey(ctx, secret)
	if err != nil {
		t.Fatalf("the real key was rejected: %v", err)
	}
	if got.ID != rec.ID {
		t.Fatalf("resolved to %q, want %q", got.ID, rec.ID)
	}
	for _, wrong := range []string{
		secret + "x",
		secret[:len(secret)-1],
		strings.ToUpper(secret),
		strings.TrimPrefix(secret, KeyPrefix),
		"crl_completelymadeup",
		"",
	} {
		if _, err := db.LookupAPIKey(ctx, wrong); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("a near-miss key %.14q was accepted", wrong)
		}
	}
}

// A revoked key stops working immediately and is indistinguishable from an unknown one, so a
// caller cannot probe which keys once existed.
func TestRevokedKeyIsRejectedLikeAnUnknownOne(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, secret, err := db.CreateAPIKey(ctx, "k1", "laptop", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeAPIKey(ctx, "k1", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, revokedErr := db.LookupAPIKey(ctx, secret)
	_, unknownErr := db.LookupAPIKey(ctx, "crl_neverexisted")
	if !errors.Is(revokedErr, ErrKeyNotFound) {
		t.Fatalf("a revoked key still authenticates: %v", revokedErr)
	}
	if revokedErr.Error() != unknownErr.Error() {
		t.Errorf("revoked and unknown keys give different errors, which leaks which keys existed:\n  %v\n  %v",
			revokedErr, unknownErr)
	}
	// Revoking twice is not an error the caller should have to handle differently.
	if err := db.RevokeAPIKey(ctx, "k1", time.Now()); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("second revoke returned %v", err)
	}
	// The record survives, so history referencing it still resolves to a name.
	keys, err := db.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("revoked key was deleted rather than withdrawn: %d keys, %v", len(keys), err)
	}
	if !keys[0].Revoked() {
		t.Error("key is not marked revoked")
	}
}

// Two keys must never collide, and every key must carry the searchable prefix.
func TestKeysAreUniqueAndPrefixed(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		_, secret, err := db.CreateAPIKey(ctx, "k"+time.Now().Format("150405.000000000")+string(rune(i)), "n", nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(secret, KeyPrefix) {
			t.Fatalf("key %q lacks the %q prefix that makes a leak searchable", secret, KeyPrefix)
		}
		if seen[secret] {
			t.Fatalf("duplicate key generated: %q", secret)
		}
		seen[secret] = true
	}
}
