-- Postgres mirror of migrations/0092_member_inbox.sql (ADR 0111, P1-A).
-- See the SQLite file for what the tables mean and why none of them declares a foreign key
-- to membership or tenant.
CREATE TABLE inbox_item (
    id                     TEXT PRIMARY KEY,
    tenant_id              TEXT NOT NULL,
    sender_membership_id   TEXT NOT NULL,
    kind                   TEXT NOT NULL CHECK (kind IN ('dm','request')),
    state                  TEXT NOT NULL DEFAULT ''
                           CHECK (state IN ('','open','claimed','done','declined','withdrawn')),
    assignee_membership_id TEXT NOT NULL DEFAULT '',
    ciphertext             TEXT NOT NULL DEFAULT '',
    key_ref                TEXT NOT NULL DEFAULT '',
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL,
    updated_by             TEXT NOT NULL DEFAULT '',
    final_at               TEXT NOT NULL DEFAULT '',
    session_membership_id  TEXT NOT NULL DEFAULT '',
    session_name           TEXT NOT NULL DEFAULT '',
    session_dir            TEXT NOT NULL DEFAULT '',
    session_branch         TEXT NOT NULL DEFAULT '',
    launched_at            TEXT NOT NULL DEFAULT '',
    launch_pending_until   TEXT NOT NULL DEFAULT '',
    pr_url                 TEXT NOT NULL DEFAULT '',
    pr_branch              TEXT NOT NULL DEFAULT '',
    CHECK ((kind = 'dm' AND state = '') OR (kind = 'request' AND state <> ''))
);
CREATE INDEX idx_inbox_item_sender ON inbox_item(sender_membership_id, created_at);
CREATE INDEX idx_inbox_item_tenant_state ON inbox_item(tenant_id, state);

CREATE TABLE inbox_participant (
    item_id       TEXT NOT NULL REFERENCES inbox_item(id) ON DELETE CASCADE,
    membership_id TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('sender','recipient')),
    delivery      TEXT NOT NULL DEFAULT 'unread' CHECK (delivery IN ('unread','read','declined')),
    seq           BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, membership_id)
);
CREATE INDEX idx_inbox_participant_member ON inbox_participant(membership_id, seq);

CREATE TABLE inbox_mailbox (
    membership_id TEXT PRIMARY KEY,
    last_seq      BIGINT NOT NULL DEFAULT 0,
    read_seq      BIGINT NOT NULL DEFAULT 0,
    accept_from   TEXT NOT NULL DEFAULT 'tenant' CHECK (accept_from IN ('tenant','chosen')),
    dnd           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE inbox_sender_rule (
    membership_id        TEXT NOT NULL,
    sender_membership_id TEXT NOT NULL,
    rule                 TEXT NOT NULL CHECK (rule IN ('allow','block')),
    PRIMARY KEY (membership_id, sender_membership_id)
);
