-- name: UpsertAccountEncryption :one
INSERT INTO account_encryption (account_id, yubikey_slot_id, slot_fingerprint, encrypted_content_key_prefix)
VALUES (?1, ?2, ?3, ?4)
ON CONFLICT(account_id) DO UPDATE SET
    yubikey_slot_id = excluded.yubikey_slot_id,
    slot_fingerprint = excluded.slot_fingerprint,
    encrypted_content_key_prefix = excluded.encrypted_content_key_prefix,
    created_at = datetime('now')
RETURNING id, account_id, yubikey_slot_id, slot_fingerprint, encrypted_content_key_prefix, created_at;

-- name: GetAccountEncryption :one
SELECT id, account_id, yubikey_slot_id, slot_fingerprint, encrypted_content_key_prefix, created_at
FROM account_encryption
WHERE account_id = ?1;

-- name: GetAccountEncryptionByPrefix :one
SELECT id, account_id, yubikey_slot_id, slot_fingerprint, encrypted_content_key_prefix, created_at
FROM account_encryption
WHERE account_id = ?1 AND encrypted_content_key_prefix = ?2;

-- name: ListAccountEncryption :many
SELECT id, account_id, yubikey_slot_id, slot_fingerprint, encrypted_content_key_prefix, created_at
FROM account_encryption
ORDER BY account_id ASC;

-- name: DeleteAccountEncryption :exec
DELETE FROM account_encryption WHERE account_id = ?1;
