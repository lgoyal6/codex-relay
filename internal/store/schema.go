package store

// migrations are applied in order and recorded in schema_migrations. Each entry is
// immutable once shipped; changes go in a new migration.
//
// The split that matters here: ownership and configuration are required, transactional
// state. Activity history is bounded, best-effort audit. They are separate tables with
// separate write paths so an audit write can never block or fail a routed turn.
var migrations = []string{
	`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	);`,

	`CREATE TABLE accounts (
		id TEXT PRIMARY KEY,
		chatgpt_user_id TEXT NOT NULL,
		email TEXT,
		plan_type TEXT,
		created_at TEXT NOT NULL,
		UNIQUE (chatgpt_user_id)
	);`,

	// A workspace is distinct from its account, and neither is keyed by email.
	`CREATE TABLE workspaces (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		chatgpt_account_id TEXT NOT NULL,
		display_name TEXT NOT NULL,
		upstream_name TEXT,
		structure TEXT NOT NULL DEFAULT '',
		paused INTEGER NOT NULL DEFAULT 0,
		credential_ref TEXT NOT NULL,
		credential_ok INTEGER NOT NULL DEFAULT 0,
		credential_note TEXT NOT NULL DEFAULT '',
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE (account_id, chatgpt_account_id)
	);`,

	`CREATE TABLE quota_windows (
		workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		limit_id TEXT NOT NULL DEFAULT 'codex',
		window_minutes INTEGER NOT NULL,
		used_percent REAL NOT NULL,
		resets_at INTEGER,
		observed_at TEXT NOT NULL,
		source TEXT NOT NULL,
		PRIMARY KEY (workspace_id, limit_id, window_minutes)
	);`,

	`CREATE TABLE model_eligibility (
		workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		model_slug TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		PRIMARY KEY (workspace_id, model_slug)
	);`,

	`CREATE TABLE rules (
		id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		priority INTEGER NOT NULL DEFAULT 100,
		source_workspace_id TEXT NOT NULL,
		window_minutes INTEGER NOT NULL DEFAULT 0,
		comparison TEXT NOT NULL DEFAULT '',
		remaining_percent REAL NOT NULL DEFAULT 0,
		reset_comparison TEXT NOT NULL DEFAULT '',
		reset_hours REAL NOT NULL DEFAULT 0,
		preferred_workspace_id TEXT NOT NULL DEFAULT '',
		no_alternative TEXT NOT NULL DEFAULT 'stop_and_explain',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`,

	// Required, transactional. A conversation's owning workspace is written before any
	// account-bound state is exposed, and is never rewritten silently.
	`CREATE TABLE thread_ownership (
		thread_id TEXT PRIMARY KEY,
		workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
		first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		released_at TEXT,
		release_reason TEXT NOT NULL DEFAULT ''
	);`,

	// Account-bound resources observed inside a thread (response ids, file ids, ...).
	// Separate from thread ownership because the scopes differ.
	`CREATE TABLE resource_ownership (
		kind TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
		thread_id TEXT,
		created_at TEXT NOT NULL,
		PRIMARY KEY (kind, resource_id)
	);`,

	// Bounded, best-effort audit. Never holds request bodies or credentials.
	`CREATE TABLE decisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		thread_id TEXT,
		model TEXT,
		outcome TEXT NOT NULL,
		workspace_id TEXT,
		primary_reason TEXT NOT NULL,
		summary TEXT NOT NULL,
		detail_json TEXT NOT NULL,
		state_version INTEGER NOT NULL,
		attempt INTEGER NOT NULL DEFAULT 1,
		status_code INTEGER,
		first_token_ms INTEGER,
		total_ms INTEGER,
		error_class TEXT
	);`,
	`CREATE INDEX idx_decisions_at ON decisions(at DESC);`,

	`CREATE TABLE settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`,

	// Every change we make to the user's Codex config, so rollback restores only our edits
	// and can detect later third-party edits instead of overwriting them.
	`CREATE TABLE integration_changes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		target_path TEXT NOT NULL,
		backup_path TEXT NOT NULL,
		before_sha256 TEXT NOT NULL,
		after_sha256 TEXT NOT NULL,
		description TEXT NOT NULL,
		rolled_back_at TEXT
	);`,

	// APPEND ONLY. Migrations are identified by their position in this slice, so inserting
	// one in the middle renumbers every migration after it and a database that has already
	// applied them will try to re-run the wrong statements. That happened once: these four
	// were added mid-list and an existing install failed with
	// "migration 16: table integration_changes already exists".
	`ALTER TABLE decisions ADD COLUMN input_tokens INTEGER;`,
	`ALTER TABLE decisions ADD COLUMN cached_input_tokens INTEGER;`,
	`ALTER TABLE decisions ADD COLUMN output_tokens INTEGER;`,
	`ALTER TABLE decisions ADD COLUMN total_tokens INTEGER;`,

	// Diagnostic detail. Until these existed a failed turn recorded only a bucket
	// ("quota", "upstream"), which tells a user that something broke but never what.
	`ALTER TABLE decisions ADD COLUMN error_message TEXT;`,
	`ALTER TABLE decisions ADD COLUMN failure_phase TEXT;`,
	`ALTER TABLE decisions ADD COLUMN upstream_status INTEGER;`,
	`ALTER TABLE decisions ADD COLUMN transport TEXT;`,
	`ALTER TABLE decisions ADD COLUMN upstream_ms INTEGER;`,

	// API keys let a client other than Codex use the pool, and let the proxy be locked so it
	// is not open to every process on the machine. The key itself is never stored: only a
	// SHA-256 of it, plus a short prefix so a key can be identified in a list.
	`CREATE TABLE api_keys (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		prefix       TEXT NOT NULL,
		key_hash     TEXT NOT NULL UNIQUE,
		created_at   TEXT NOT NULL,
		last_used_at TEXT,
		revoked_at   TEXT,
		daily_limit  INTEGER
	);`,
	`CREATE INDEX idx_api_keys_hash ON api_keys(key_hash);`,
	`ALTER TABLE decisions ADD COLUMN api_key_id TEXT;`,

	// Hourly rollups exist because the decisions table is capped and pruned: past the cap,
	// every older turn is deleted and with it any record of what was spent. These are
	// aggregates only, they are never pruned, and they are what makes long-term cost and
	// usage answerable at all.
	`CREATE TABLE usage_rollups (
		hour_utc      TEXT NOT NULL,
		workspace_id  TEXT NOT NULL DEFAULT '',
		api_key_id    TEXT NOT NULL DEFAULT '',
		turns         INTEGER NOT NULL DEFAULT 0,
		blocked       INTEGER NOT NULL DEFAULT 0,
		errors        INTEGER NOT NULL DEFAULT 0,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (hour_utc, workspace_id, api_key_id)
	);`,
	`CREATE INDEX idx_usage_rollups_hour ON usage_rollups(hour_utc);`,

	// Automations are scheduled changes to workspace availability. Deliberately narrow: the
	// two things a person actually wants on a timer are "stop using this account" and "start
	// again", including "start again once its window resets", which otherwise means watching
	// the dashboard for a reset that happens at an inconvenient hour.
	`CREATE TABLE automations (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		kind          TEXT NOT NULL,
		workspace_id  TEXT NOT NULL,
		at_minute     INTEGER,
		enabled       INTEGER NOT NULL DEFAULT 1,
		last_run_at   TEXT,
		last_result   TEXT,
		created_at    TEXT NOT NULL,
		FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
	);`,

	// The quota visible after a turn may differ from the quota that informed that turn. Keep
	// the immutable point-in-time evidence with the decision instead of joining current state.
	// JSON is intentional because plans expose different window counts and durations.
	`ALTER TABLE decisions ADD COLUMN quota_snapshot_json TEXT;`,

	// Profiles are reusable routing strategies shared by the dashboard and relaypool. JSON
	// arrays keep ordered workspace priority and aliases without adding positional join tables.
	`CREATE TABLE routing_profiles (
		id                           TEXT PRIMARY KEY,
		name                         TEXT NOT NULL,
		command                      TEXT NOT NULL UNIQUE,
		aliases_json                 TEXT NOT NULL DEFAULT '[]',
		mode                         TEXT NOT NULL,
		priority_workspace_ids_json  TEXT NOT NULL DEFAULT '[]',
		pace_workspace_ids_json      TEXT NOT NULL DEFAULT '[]',
		overflow_workspace_id        TEXT NOT NULL DEFAULT '',
		target_remaining_percent     REAL NOT NULL DEFAULT 3,
		default_workspace_id         TEXT NOT NULL DEFAULT '',
		handoff_below_percent        REAL NOT NULL DEFAULT 2,
		disabled_workspace_ids_json  TEXT NOT NULL DEFAULT '[]',
		created_at                   TEXT NOT NULL,
		updated_at                   TEXT NOT NULL
	);`,
}
