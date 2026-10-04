-- +goose Up

-- Deleting an account used to cascade to its blobs/messages index, leaving
-- the encrypted files orphaned on disk (and backfill skips unknown account
-- ids). An archive should outlive the mailbox it came from, so a delete now
-- only marks the account: syncing stops and the stored password is wiped, but
-- its archived mail stays indexed, browsable, and searchable.
ALTER TABLE accounts ADD COLUMN deleted_at TEXT;

-- +goose Down

DELETE FROM accounts WHERE deleted_at IS NOT NULL;
ALTER TABLE accounts DROP COLUMN deleted_at;
