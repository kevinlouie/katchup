# Data Model Specification

## Database: SQLite (WAL mode)

All tables use INTEGER PRIMARY KEY (auto-increment). Timestamps stored as TEXT in RFC3339/SQLite format, always in UTC.

## Tables

### accounts
Stores IMAP account configuration. Password is encrypted at rest using a symmetric key derived from the host (or a master env var).

```sql
CREATE TABLE accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,                    -- display name, e.g. "Personal Gmail"
    host TEXT NOT NULL,                    -- imap.gmail.com, mail.example.com
    port INTEGER NOT NULL DEFAULT 993,     -- 993 (IMAPS) or 143 (STARTTLS)
    username TEXT NOT NULL,                -- email address or IMAP username
    encrypted_password TEXT NOT NULL,      -- AES-256-GCM encrypted password
    use_ssl INTEGER NOT NULL DEFAULT 1,    -- 1 = IMAPS (port 993), 0 = STARTTLS (port 143)
    folders TEXT NOT NULL DEFAULT 'INBOX', -- comma-separated folder names to sync
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

- `folders` — comma-separated list of IMAP folders to sync (default: `INBOX`)
- `encrypted_password` — encrypted with a host-derived key or `KATCHUP_MASTER_KEY` env var
- Password is only decrypted in-memory during IMAP connection

### account_encryption
Stores YubiKey PIV slot metadata per account. Enables per-account key rotation.

```sql
CREATE TABLE account_encryption (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    yubikey_slot_id TEXT NOT NULL,         -- e.g. "9a" (authentication slot)
    slot_fingerprint TEXT NOT NULL,        -- SHA256 of YubiKey public key (for display)
    encrypted_content_key_prefix TEXT,     -- first 16 chars of the encrypted content key (lookup)
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id)
);
```

- `yubikey_slot_id` — PIV slot used for encryption (default "9a", other slots available: "9b", "9c", "9d")
- `slot_fingerprint` — stored for UI display so user can identify which YubiKey was used
- `encrypted_content_key_prefix` — prefix used to look up the right encrypted content key when decrypting

### sync_runs
Tracks each sync run per account for monitoring and debugging.

```sql
CREATE TABLE sync_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    emails_backed_up INTEGER NOT NULL DEFAULT 0,
    errors TEXT,                           -- comma-separated error messages
    status TEXT NOT NULL DEFAULT 'running', -- running, completed, failed
    last_uid INTEGER,                      -- highest UID seen (for dedup)
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

- `status` — `running` during sync, `completed` or `failed` after
- `last_uid` — highest UID seen from the server (used for incremental sync)
- `errors` — limited to 1000 chars; first 5 errors stored comma-separated

## Key Queries

### CreateAccount
```sql
INSERT INTO accounts (name, host, port, username, encrypted_password, use_ssl, folders)
VALUES (?, ?, ?, ?, ?, ?, ?);
```

### GetAccount
```sql
SELECT * FROM accounts WHERE id = ?;
```

### ListAccounts
```sql
SELECT *,
    (SELECT COUNT(*) FROM sync_runs WHERE account_id = accounts.id AND status = 'running')
    AS is_syncing
FROM accounts
ORDER BY name ASC;
```

### UpdateAccount
```sql
UPDATE accounts SET
    name = ?,
    host = ?,
    port = ?,
    username = ?,
    encrypted_password = ?,
    use_ssl = ?,
    folders = ?,
    updated_at = datetime('now')
WHERE id = ?;
```

### DeleteAccount
```sql
DELETE FROM accounts WHERE id = ?;
-- CASCADE deletes sync_runs
```

### CreateSyncRun
```sql
INSERT INTO sync_runs (account_id, started_at, status)
VALUES (?, datetime('now'), 'running');
```

### UpdateSyncRunStatus
```sql
UPDATE sync_runs SET
    finished_at = datetime('now'),
    emails_backed_up = ?,
    errors = ?,
    status = ?,
    last_uid = ?
WHERE id = ?;
```

### ListRecentRuns
```sql
SELECT * FROM sync_runs
WHERE account_id = ?
ORDER BY started_at DESC
LIMIT 20;
```

### GetAccountByLastUIDPrefix
```sql
SELECT * FROM account_encryption
WHERE account_id = ? AND encrypted_content_key_prefix = ?;
```

## Migration 001

```sql
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
```
