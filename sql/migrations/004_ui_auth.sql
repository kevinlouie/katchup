-- +goose Up

-- Small key/value settings table; currently holds the hashed web-UI access
-- key (key = 'ui_key_hash') when it was created via the first-run setup page
-- rather than the KATCHUP_UI_KEY environment variable.
CREATE TABLE IF NOT EXISTS app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Older releases derived the stored key fingerprint from the leading bytes of
-- the actual encryption key, so existing rows leak 4 bytes of key material.
-- The column is metadata only (never on the decrypt path) — scrub it.
UPDATE account_encryption
SET slot_fingerprint = 'rotated', encrypted_content_key_prefix = NULL;

-- +goose Down

DROP TABLE IF EXISTS app_settings;
