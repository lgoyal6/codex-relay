package store

import (
	"context"
	"testing"
	"time"
)

func BenchmarkClaimThreadNew(b *testing.B) {
	db := testDBB(b)
	ctx := context.Background()
	now := time.Now().UTC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = db.ClaimThread(ctx, "t"+itoaB(i), "personal", now)
	}
}

func BenchmarkClaimThreadRepeat(b *testing.B) {
	db := testDBB(b)
	ctx := context.Background()
	now := time.Now().UTC()
	_, _ = db.ClaimThread(ctx, "same", "personal", now)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = db.ClaimThread(ctx, "same", "personal", now)
	}
}

func BenchmarkOwnerOf(b *testing.B) {
	db := testDBB(b)
	ctx := context.Background()
	_, _ = db.ClaimThread(ctx, "same", "personal", time.Now().UTC())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = db.OwnerOf(ctx, "same")
	}
}

func testDBB(b *testing.B) *DB {
	b.Helper()
	db, err := Open(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO accounts (id, chatgpt_user_id, created_at) VALUES ('a','u1',?)`, now); err != nil {
		b.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO workspaces (id, account_id, chatgpt_account_id, display_name, credential_ref, created_at, updated_at) VALUES ('personal','a','acct','Personal','ref',?,?)`, now, now); err != nil {
		b.Fatal(err)
	}
	return db
}

func itoaB(i int) string {
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}
