-- +goose Up

CREATE TABLE accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    host TEXT NOT NULL,
    port INTEGER NOT NULL DEFAULT 993,
    username TEXT NOT NULL,
    encrypted_password TEXT NOT NULL,
    use_ssl INTEGER NOT NULL DEFAULT 1,
    folders TEXT NOT NULL DEFAULT 'INBOX',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE account_encryption (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    yubikey_slot_id TEXT NOT NULL,
    slot_fingerprint TEXT NOT NULL,
    encrypted_content_key_prefix TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id)
);

CREATE TABLE sync_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    emails_backed_up INTEGER NOT NULL DEFAULT 0,
    errors TEXT,
    status TEXT NOT NULL DEFAULT 'running',
    last_uid INTEGER,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_sync_runs_account ON sync_runs(account_id, started_at DESC);

-- +goose Down
DROP TABLE IF EXISTS sync_runs;
DROP TABLE IF EXISTS account_encryption;
DROP TABLE IF EXISTS accounts;
