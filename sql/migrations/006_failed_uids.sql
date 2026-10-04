-- +goose Up

-- Messages whose fetch/store keeps failing. Each failure bumps attempts; once
-- a UID reaches the retry cap the sync stops fetching it and lets the folder
-- watermark move past it, so one poison message can't pin the watermark (and
-- force a re-scan of everything above it) forever. Delete a row to retry it.
CREATE TABLE failed_uids (
    account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder      TEXT NOT NULL,
    uidvalidity INTEGER NOT NULL,
    uid         INTEGER NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 1,
    last_error  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (account_id, folder, uidvalidity, uid)
);

-- +goose Down

DROP TABLE IF EXISTS failed_uids;
