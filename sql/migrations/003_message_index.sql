-- +goose Up

CREATE TABLE blobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    sha256      TEXT NOT NULL,             -- hex of SHA-256 over raw RFC822 bytes
    path        TEXT NOT NULL,             -- relative path to the .eml.enc file
    size        INTEGER NOT NULL,
    refcount    INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id, sha256)             -- dedup key is PER-ACCOUNT
);

CREATE TABLE messages (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder         TEXT NOT NULL,
    uid            INTEGER NOT NULL,
    blob_id        INTEGER NOT NULL REFERENCES blobs(id) ON DELETE CASCADE,
    message_id_hdr TEXT,                   -- RFC5322 Message-ID header (indexed)
    fuzzy_fp       TEXT,                   -- sha256(normalized from|date|subject)
    from_addr      TEXT,
    to_addr        TEXT,
    subject        TEXT,
    internal_date  TEXT,
    size           INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id, folder, uid)
);

CREATE INDEX idx_messages_msgid  ON messages(account_id, message_id_hdr);
CREATE INDEX idx_messages_fuzzy  ON messages(account_id, fuzzy_fp);
CREATE INDEX idx_messages_date   ON messages(account_id, internal_date DESC);

-- +goose Down
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS blobs;
