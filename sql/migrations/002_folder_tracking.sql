-- +goose Up

CREATE TABLE IF NOT EXISTS folder_sync_state (
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder     TEXT NOT NULL,
    last_uid   INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (account_id, folder)
);

-- +goose Down

DROP TABLE IF EXISTS folder_sync_state;
