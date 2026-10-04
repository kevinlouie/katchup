-- name: RecordFailedUID :one
-- Count one more failed attempt for a message; returns the new total.
INSERT INTO failed_uids (account_id, folder, uidvalidity, uid, last_error)
VALUES (?1, ?2, ?3, ?4, ?5)
ON CONFLICT(account_id, folder, uidvalidity, uid) DO UPDATE SET
    attempts = attempts + 1,
    last_error = excluded.last_error,
    updated_at = datetime('now')
RETURNING attempts;

-- name: ListFailedUIDs :many
SELECT uid, attempts FROM failed_uids
WHERE account_id = ?1 AND folder = ?2 AND uidvalidity = ?3;

-- name: ClearFailedUID :exec
DELETE FROM failed_uids
WHERE account_id = ?1 AND folder = ?2 AND uidvalidity = ?3 AND uid = ?4;

-- name: ClearStaleFailedUIDs :exec
-- Drop failure records from other UIDVALIDITY generations of a folder; their
-- UIDs no longer name anything on the server.
DELETE FROM failed_uids
WHERE account_id = ?1 AND folder = ?2 AND uidvalidity <> ?3;
