-- name: CreateAccount :one
INSERT INTO accounts (name, host, port, username, encrypted_password, use_ssl, folders)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
RETURNING id, name, host, port, username, encrypted_password, use_ssl, folders, created_at, updated_at;

-- name: GetAccount :one
SELECT id, name, host, port, username, encrypted_password, use_ssl, folders, created_at, updated_at
FROM accounts
WHERE id = ?1;

-- name: ListAccounts :many
SELECT *,
    (SELECT COUNT(*) FROM sync_runs WHERE account_id = accounts.id AND status = 'running')
    AS is_syncing
FROM accounts
ORDER BY name ASC;

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
WHERE id = ?8
RETURNING id, name, host, port, username, encrypted_password, use_ssl, folders, created_at, updated_at;

-- name: DeleteAccount :exec
DELETE FROM accounts WHERE id = ?1;
