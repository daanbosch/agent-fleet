-- Postgres mirror of migrations/0094_inbox_recipes.sql (ADR 0111, P1-A second half).
-- See the SQLite file for what the table and the columns mean.
CREATE TABLE inbox_attachment (
    id         TEXT PRIMARY KEY,
    item_id    TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('recipe','session')),
    ciphertext TEXT NOT NULL DEFAULT '',
    key_ref    TEXT NOT NULL DEFAULT '',
    catalog_id TEXT NOT NULL DEFAULT '',
    granted    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_inbox_attachment_item ON inbox_attachment(item_id);

ALTER TABLE memo ADD COLUMN origin_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE memo ADD COLUMN origin_sender_membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE memo ADD COLUMN origin_sender_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_memo_origin_once ON memo(origin_item_id, membership_id) WHERE origin_item_id <> ''
