-- +goose Up

-- UIDs are only meaningful together with the mailbox's UIDVALIDITY: when a
-- server renumbers a folder (migration, restore, rebuild) it bumps
-- UIDVALIDITY and every UID may now name a different message. Track it per
-- folder so the watermark can be reset instead of silently skipping new mail
-- that landed below it. 0 = not yet known (rows from before this migration).
ALTER TABLE folder_sync_state ADD COLUMN uidvalidity INTEGER NOT NULL DEFAULT 0;

-- messages must key on (folder, uidvalidity, uid) too, or a renumbered UID
-- would collide with an archived message and be skipped as "already indexed".
-- SQLite can't alter a UNIQUE constraint, so rebuild the table (ids are kept,
-- so search docs and links stay valid).
CREATE TABLE messages_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder         TEXT NOT NULL,
    uidvalidity    INTEGER NOT NULL DEFAULT 0,
    uid            INTEGER NOT NULL,
    blob_id        INTEGER NOT NULL REFERENCES blobs(id) ON DELETE CASCADE,
    message_id_hdr TEXT,
    fuzzy_fp       TEXT,
    from_addr      TEXT,
    to_addr        TEXT,
    subject        TEXT,
    internal_date  TEXT,
    size           INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id, folder, uidvalidity, uid)
);

INSERT INTO messages_new (
    id, account_id, folder, uidvalidity, uid, blob_id, message_id_hdr, fuzzy_fp,
    from_addr, to_addr, subject, internal_date, size, created_at
)
SELECT
    id, account_id, folder, 0, uid, blob_id, message_id_hdr, fuzzy_fp,
    from_addr, to_addr, subject, internal_date, size, created_at
FROM messages;

DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;

CREATE INDEX idx_messages_msgid  ON messages(account_id, message_id_hdr);
CREATE INDEX idx_messages_fuzzy  ON messages(account_id, fuzzy_fp);
CREATE INDEX idx_messages_date   ON messages(account_id, internal_date DESC);

-- +goose Down

CREATE TABLE messages_old (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder         TEXT NOT NULL,
    uid            INTEGER NOT NULL,
    blob_id        INTEGER NOT NULL REFERENCES blobs(id) ON DELETE CASCADE,
    message_id_hdr TEXT,
    fuzzy_fp       TEXT,
    from_addr      TEXT,
    to_addr        TEXT,
    subject        TEXT,
    internal_date  TEXT,
    size           INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id, folder, uid)
);

-- Rows from an older UIDVALIDITY generation that collide on (folder, uid)
-- can't be represented in the old schema; keep the first.
INSERT OR IGNORE INTO messages_old (
    id, account_id, folder, uid, blob_id, message_id_hdr, fuzzy_fp,
    from_addr, to_addr, subject, internal_date, size, created_at
)
SELECT
    id, account_id, folder, uid, blob_id, message_id_hdr, fuzzy_fp,
    from_addr, to_addr, subject, internal_date, size, created_at
FROM messages ORDER BY id;

DROP TABLE messages;
ALTER TABLE messages_old RENAME TO messages;

CREATE INDEX idx_messages_msgid  ON messages(account_id, message_id_hdr);
CREATE INDEX idx_messages_fuzzy  ON messages(account_id, fuzzy_fp);
CREATE INDEX idx_messages_date   ON messages(account_id, internal_date DESC);

ALTER TABLE folder_sync_state DROP COLUMN uidvalidity;
