-- name: CreateAccount :one
INSERT INTO accounts (name, host, port, username, encrypted_password, use_ssl, folders)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
RETURNING *;

-- name: GetAccount :one
-- Deleted accounts are invisible to everything but the archive itself.
SELECT *
FROM accounts
WHERE id = ?1 AND deleted_at IS NULL;

-- name: ListAccounts :many
SELECT *,
    (SELECT COUNT(*) FROM sync_runs WHERE account_id = accounts.id AND status = 'running')
    AS is_syncing
FROM accounts
WHERE deleted_at IS NULL
ORDER BY name ASC;

-- name: ListAllAccountIDs :many
-- Every account that owns archived mail, deleted ones included.
SELECT id FROM accounts ORDER BY id;

-- name: UpdateAccount :one
UPDATE accounts SET
    name = ?1,
    host = ?2,
    port = ?3,
    username = ?4,
    encrypted_password = ?5,
    use_ssl = ?6,
    folders = ?7,
    updated_at = datetime('now')
WHERE id = ?8 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteAccount :exec
-- Stop syncing an account and wipe its stored credential, keeping the row so
-- its archived mail (blobs/messages reference it) survives.
UPDATE accounts SET
    deleted_at = datetime('now'),
    encrypted_password = '',
    updated_at = datetime('now')
WHERE id = ?1 AND deleted_at IS NULL;
