// Package store owns the SQLite database: versioned configuration, identities, quota
// readings, durable ownership, and bounded history.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the connection with the settings a single-process local service wants:
// WAL for concurrent reads during a streamed turn, foreign keys on, and a busy timeout so
// a slow audit write never hard-fails a routed turn.
type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// modernc/sqlite is safe for concurrent use, but a single writer avoids lock churn on
	// the ownership table, which is the one place we must not lose a write.
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db := &DB{sql: sqlDB}
	if err := db.migrate(context.Background()); err != nil {
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) SQL() *sql.DB { return d.sql }

// migrate applies pending migrations inside a transaction each, and is safe to call on
// every start.
func (d *DB) migrate(ctx context.Context) error {
	var have int
	row := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`)
	if err := row.Scan(&have); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	applied := map[int]bool{}
	if have > 0 {
		rows, err := d.sql.QueryContext(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return fmt.Errorf("read migrations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			if err := rows.Scan(&v); err != nil {
				return err
			}
			applied[v] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for i, stmt := range migrations {
		version := i + 1
		if applied[version] {
			continue
		}
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}

// SchemaVersion reports the highest applied migration.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.sql.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}
