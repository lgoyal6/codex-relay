package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyPrefix marks a codex-relay key so one is recognisable in a log or a config file, and so
// a leaked string can be searched for.
const KeyPrefix = "crl_"

// ErrKeyNotFound is returned when no live key matches.
var ErrKeyNotFound = errors.New("no matching API key")

// APIKey is the stored record. The secret itself is never part of it.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	DailyLimit *int64     `json:"daily_limit"`
}

// Revoked reports whether the key has been withdrawn.
func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

// NewAPIKeySecret mints a key. The caller shows it once and then cannot recover it, because
// only its hash is kept.
func NewAPIKeySecret() (secret string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashAPIKey is the one-way function used both when storing and when checking. Storing the
// key itself would mean a readable database file hands someone the pool.
func HashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// keyDisplayPrefix is the identifiable, non-secret head of a key.
func keyDisplayPrefix(secret string) string {
	body := strings.TrimPrefix(secret, KeyPrefix)
	if len(body) > 6 {
		body = body[:6]
	}
	return KeyPrefix + body
}

// CreateAPIKey stores a new key and returns the record plus the secret, which is the only
// time the secret exists outside the caller's hands.
func (d *DB) CreateAPIKey(ctx context.Context, id, name string, dailyLimit *int64, now time.Time) (APIKey, string, error) {
	secret, err := NewAPIKeySecret()
	if err != nil {
		return APIKey{}, "", err
	}
	rec := APIKey{
		ID: id, Name: name, Prefix: keyDisplayPrefix(secret),
		CreatedAt: now.UTC(), DailyLimit: dailyLimit,
	}
	_, err = d.sql.ExecContext(ctx, `
		INSERT INTO api_keys (id, name, prefix, key_hash, created_at, daily_limit)
		VALUES (?,?,?,?,?,?)`,
		rec.ID, rec.Name, rec.Prefix, HashAPIKey(secret),
		rec.CreatedAt.Format(time.RFC3339Nano), dailyLimit)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("create api key: %w", err)
	}
	return rec, secret, nil
}

// LookupAPIKey resolves a presented secret to a live key.
//
// The lookup is by hash, so a revoked or unknown key is indistinguishable to the caller and
// no comparison is done against the secret itself.
func (d *DB) LookupAPIKey(ctx context.Context, secret string) (APIKey, error) {
	if !strings.HasPrefix(secret, KeyPrefix) {
		return APIKey{}, ErrKeyNotFound
	}
	row := d.sql.QueryRowContext(ctx, `
		SELECT id, name, prefix, created_at, last_used_at, revoked_at, daily_limit
		FROM api_keys WHERE key_hash = ?`, HashAPIKey(secret))
	k, err := scanAPIKey(row)
	if err != nil {
		return APIKey{}, err
	}
	if k.Revoked() {
		return APIKey{}, ErrKeyNotFound
	}
	return k, nil
}

type rowScanner interface{ Scan(...any) error }

func scanAPIKey(row rowScanner) (APIKey, error) {
	var k APIKey
	var created string
	var lastUsed, revoked sql.NullString
	var limit sql.NullInt64
	if err := row.Scan(&k.ID, &k.Name, &k.Prefix, &created, &lastUsed, &revoked, &limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrKeyNotFound
		}
		return APIKey{}, err
	}
	k.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if lastUsed.Valid {
		if t, err := time.Parse(time.RFC3339Nano, lastUsed.String); err == nil {
			k.LastUsedAt = &t
		}
	}
	if revoked.Valid && revoked.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, revoked.String); err == nil {
			k.RevokedAt = &t
		}
	}
	if limit.Valid {
		v := limit.Int64
		k.DailyLimit = &v
	}
	return k, nil
}

// ListAPIKeys returns every key, revoked ones included, newest first.
func (d *DB) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, name, prefix, created_at, last_used_at, revoked_at, daily_limit
		FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey withdraws a key without deleting it, so history that references it still
// resolves to a name rather than to a dangling id.
func (d *DB) RevokeAPIKey(ctx context.Context, id string, now time.Time) error {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// TouchAPIKey records use. Best effort: a failure here must never fail a turn.
func (d *DB) TouchAPIKey(ctx context.Context, id string, now time.Time) {
	_, _ = d.sql.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		now.UTC().Format(time.RFC3339Nano), id)
}

// CountKeyUsageSince counts turns served for a key, for the daily cap.
func (d *DB) CountKeyUsageSince(ctx context.Context, id string, since time.Time) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM decisions WHERE api_key_id = ? AND at >= ?`,
		id, since.UTC().Format(time.RFC3339Nano)).Scan(&n)
	return n, err
}
