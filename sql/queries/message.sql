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
-- Idempotent per (account, folder, uidvalidity, uid): re-processing the same
-- message (e.g. an aborted batch retried next run) does not create a duplicate row.
INSERT INTO messages (
    account_id, folder, uid, blob_id, message_id_hdr, fuzzy_fp,
    from_addr, to_addr, subject, internal_date, size, uidvalidity
)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12)
ON CONFLICT(account_id, folder, uidvalidity, uid) DO NOTHING;

-- name: ExistsMessage :one
SELECT COUNT(*) FROM messages
WHERE account_id = ?1 AND folder = ?2 AND uidvalidity = ?3 AND uid = ?4;

-- name: AdoptUIDValidity :exec
-- Stamp a folder's pre-UIDVALIDITY-tracking rows (uidvalidity = 0) with the
-- server's current value the first time it is seen, so they keep matching
-- ExistsMessage. OR IGNORE skips a row that would collide with one already
-- recorded under that generation.
UPDATE OR IGNORE messages SET uidvalidity = ?1
WHERE account_id = ?2 AND folder = ?3 AND uidvalidity = 0;

-- name: ListMessages :many
-- internal_date is RFC3339 UTC, so @since (inclusive) / @before (exclusive) are
-- plain string comparisons; pass 'YYYY-MM-DD' or a full timestamp, '' = unbounded.
-- Rows with an unknown ('') date never match a bound.
SELECT
    m.id, m.account_id, m.folder, m.uid, m.blob_id,
    m.message_id_hdr, m.fuzzy_fp, m.from_addr, m.to_addr,
    m.subject, m.internal_date, m.size, m.created_at,
    b.path AS blob_path, b.sha256 AS blob_sha256
FROM messages m
JOIN blobs b ON b.id = m.blob_id
WHERE (@account_id = 0 OR m.account_id = @account_id)
  AND (@date = '' OR substr(m.internal_date, 1, 10) = @date)
  AND (@since = '' OR m.internal_date >= @since)
  AND (@before = '' OR (m.internal_date <> '' AND m.internal_date < @before))
ORDER BY m.internal_date DESC, m.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: CountMessages :one
SELECT COUNT(*) FROM messages m
WHERE (@account_id = 0 OR m.account_id = @account_id)
  AND (@date = '' OR substr(m.internal_date, 1, 10) = @date)
  AND (@since = '' OR m.internal_date >= @since)
  AND (@before = '' OR (m.internal_date <> '' AND m.internal_date < @before));

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
-- Resolve the messages.id for a (account, folder, uidvalidity, uid) key. Used
-- after an idempotent InsertMessage to obtain the row id for search indexing.
SELECT id FROM messages
WHERE account_id = ?1 AND folder = ?2 AND uidvalidity = ?3 AND uid = ?4;

-- name: SearchMessagesLike :many
-- SQLite LIKE fallback for header-only search when Meilisearch is unconfigured.
-- Matches subject/from only (headers), never the encrypted body. @q must already
-- be wrapped in % wildcards by the caller. @since/@before bound internal_date
-- as in ListMessages.
SELECT
    m.id, m.account_id, m.folder, m.from_addr, m.to_addr,
    m.subject, m.internal_date, m.message_id_hdr
FROM messages m
WHERE (@account_id = 0 OR m.account_id = @account_id)
  AND (m.subject LIKE @q OR m.from_addr LIKE @q)
  AND (@since = '' OR m.internal_date >= @since)
  AND (@before = '' OR (m.internal_date <> '' AND m.internal_date < @before))
ORDER BY m.internal_date DESC, m.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: GetArchivedByMessageID :one
-- Archived-lookup (API read side): resolve a message by its RFC5322
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

-- name: ListIndexedUIDs :many
-- UIDs already archived in a folder generation above a watermark, so the sync
-- can skip them before downloading any body.
SELECT uid FROM messages
WHERE account_id = ?1 AND folder = ?2 AND uidvalidity = ?3 AND uid > ?4;

-- name: CountBlobsByFormat :one
-- How many archived blobs are encrypted (.eml.enc) vs plaintext (.eml); used
-- at startup to spot a KATCHUP_MASTER_KEY that doesn't match the data.
SELECT
    CAST(COALESCE(SUM(path LIKE '%.enc'), 0) AS INTEGER) AS encrypted,
    CAST(COALESCE(SUM(path NOT LIKE '%.enc'), 0) AS INTEGER) AS plaintext
FROM blobs;

-- name: LatestEncryptedBlobPath :one
SELECT path FROM blobs WHERE path LIKE '%.enc' ORDER BY id DESC LIMIT 1;
