-- Recipes, take-in and session attachments for the member inbox (ADR 0111 decisions 3-5,
-- P1-A second half).
--
-- inbox_attachment holds the typed attachments of an item. A recipe keeps its prompt,
-- suggested kind, model and effort and an optional canonical repository identity, sealed
-- together in ciphertext like the body. A session attachment keeps only the catalog id of the
-- shared session: what a reader sees of it is decided on every read from the share ACL, so
-- nothing about the session is copied here. granted is 1 when the sender created the share
-- in the same transaction as the item.
--
-- Like the inbox tables, it declares no foreign key, so an older CP's hard delete is not
-- stopped by it, and the purge deletes the attachments of items that are gone.
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

-- inbox_session_origin is the provenance badge of a session (decision 3), written by the CP
-- when a create it stamped succeeds: which member's session, by name, came from which item and
-- sender. The badge is drawn from this row alone, never from what the workspace reports, so an
-- agent cannot give a session a provenance the CP did not record.
CREATE TABLE inbox_session_origin (
    membership_id TEXT NOT NULL,
    session_name  TEXT NOT NULL,
    item_id       TEXT NOT NULL,
    sender_key    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    PRIMARY KEY (membership_id, session_name)
);

-- A memo taken in from the inbox carries its origin, set by the CP and never by a client:
-- the item and its sender (membership id, and the user key at take-in time for display).
-- Editing the memo leaves these columns alone. One memo per (item, member): the unique
-- index is what makes a double take-in yield one memo.
ALTER TABLE memo ADD COLUMN origin_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE memo ADD COLUMN origin_sender_membership_id TEXT NOT NULL DEFAULT '';
ALTER TABLE memo ADD COLUMN origin_sender_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_memo_origin_once ON memo(origin_item_id, membership_id) WHERE origin_item_id <> ''
