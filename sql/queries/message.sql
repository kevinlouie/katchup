-- name: UpsertBlob :one
-- Insert a new blob for (account_id, sha256), or bump the refcount of the
-- existing one. Returns the blob id in both cases. path/size are only used on
-- the initial insert; a conflict keeps the original file's path and size.
INSERT INTO blobs (account_id, sha256, path, size)
VALUES (?1, ?2, ?3, ?4)
ON CONFLICT(account_id, sha256) DO UPDATE SET refcount = refcount + 1
RETURNING id;

-- name: GetBlobBySha :one
SELECT id, account_id, sha256, path, size, refcount, created_at
FROM blobs
WHERE account_id = ?1 AND sha256 = ?2;

-- name: InsertMessage :exec
-- Idempotent per (account, folder, uid): re-processing the same message (e.g.
-- an aborted batch retried next run) does not create a duplicate row.
INSERT INTO messages (
    account_id, folder, uid, blob_id, message_id_hdr, fuzzy_fp,
    from_addr, to_addr, subject, internal_date, size
)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)
ON CONFLICT(account_id, folder, uid) DO NOTHING;

-- name: ExistsMessage :one
SELECT COUNT(*) FROM messages
WHERE account_id = ?1 AND folder = ?2 AND uid = ?3;

-- name: ListMessages :many
SELECT
    m.id, m.account_id, m.folder, m.uid, m.blob_id,
    m.message_id_hdr, m.fuzzy_fp, m.from_addr, m.to_addr,
    m.subject, m.internal_date, m.size, m.created_at,
    b.path AS blob_path, b.sha256 AS blob_sha256
FROM messages m
JOIN blobs b ON b.id = m.blob_id
WHERE (@account_id = 0 OR m.account_id = @account_id)
  AND (@date = '' OR substr(m.internal_date, 1, 10) = @date)
ORDER BY m.internal_date DESC, m.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: CountMessages :one
SELECT COUNT(*) FROM messages m
WHERE (@account_id = 0 OR m.account_id = @account_id)
  AND (@date = '' OR substr(m.internal_date, 1, 10) = @date);

-- name: CountMessagesByAccount :one
SELECT COUNT(*) FROM messages WHERE account_id = ?1;

-- name: GetMessageWithBlob :one
SELECT
    m.id, m.account_id, m.folder, m.uid, m.blob_id,
    m.message_id_hdr, m.fuzzy_fp, m.from_addr, m.to_addr,
    m.subject, m.internal_date, m.size, m.created_at,
    b.path AS blob_path, b.sha256 AS blob_sha256
FROM messages m
JOIN blobs b ON b.id = m.blob_id
WHERE m.id = ?1;

-- name: GetMessageIDByUID :one
-- Resolve the messages.id for a (account, folder, uid) triple. Used after an
-- idempotent InsertMessage to obtain the row id for header-only search indexing.
SELECT id FROM messages
WHERE account_id = ?1 AND folder = ?2 AND uid = ?3;

-- name: SearchMessagesLike :many
-- SQLite LIKE fallback for header-only search when Meilisearch is unconfigured.
-- Matches subject/from only (headers), never the encrypted body. @q must already
-- be wrapped in % wildcards by the caller.
SELECT
    m.id, m.account_id, m.folder, m.from_addr, m.to_addr,
    m.subject, m.internal_date, m.message_id_hdr
FROM messages m
WHERE (@account_id = 0 OR m.account_id = @account_id)
  AND (m.subject LIKE @q OR m.from_addr LIKE @q)
ORDER BY m.internal_date DESC, m.id DESC
LIMIT @row_limit;

-- name: GetArchivedByMessageID :one
-- Archived-lookup (Hermes read side): resolve a message by its RFC5322
-- Message-ID header across all accounts/folders. Returns the katchup row id,
-- when it was archived (created_at), and the blob's content hash. Newest match
-- wins when the same Message-ID exists in multiple folders/accounts.
SELECT m.id, m.created_at, b.sha256 AS blob_sha256
FROM messages m
JOIN blobs b ON b.id = m.blob_id
WHERE m.message_id_hdr = ?1
ORDER BY m.id DESC
LIMIT 1;

-- name: GetArchivedByFuzzyFp :one
-- Fuzzy fallback for archived-lookup when a Message-ID match misses: match on
-- the fuzzy fingerprint (sha256 of normalized from|date|subject).
SELECT m.id, m.created_at, b.sha256 AS blob_sha256
FROM messages m
JOIN blobs b ON b.id = m.blob_id
WHERE m.fuzzy_fp = ?1
ORDER BY m.id DESC
LIMIT 1;
