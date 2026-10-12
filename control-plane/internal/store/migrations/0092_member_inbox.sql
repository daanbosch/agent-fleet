-- The member request inbox (ADR 0111 decisions 2, 3, 8-10 and the 2026-10-10 addendum, P1-A).
--
-- One inbox_item is a DM (kind=dm, no shared state) or a request (kind=request, with the
-- shared state of decision 2: open, claimed, done, declined, withdrawn). Its participants are
-- the sender and the recipients frozen at send time, one inbox_participant row each, and
-- each row carries that person's delivery state and their place in their own sequence.
-- inbox_mailbox holds the per-membership counter, the read cursor and the receive controls.
--
-- No column here declares a foreign key to membership or tenant. A CP version that predates
-- these tables still hard-deletes members with its own cascade list, and a foreign key it
-- does not know about would make that delete fail. Rows left behind that way are orphans,
-- and the CP-side purge deletes them (decision 9).
--
-- A launch from a request is recorded in two steps: reserved (the assignee pressed launch,
-- nothing has been asked of the Agent yet) and creating (the CP confirmed the claim and is
-- asking the Agent, so a session may exist). launch_token is the Agent idempotency key of
-- that one create, and a create that may have happened is never issued again under a new one.
--
-- The body is sealed by the tenant key custodian when one exists (ciphertext, key_ref).
-- Without one it is stored base64-encoded, which is not encryption.
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
    launch_token           TEXT NOT NULL DEFAULT '',
    launch_by              TEXT NOT NULL DEFAULT '',
    launch_state           TEXT NOT NULL DEFAULT '' CHECK (launch_state IN ('','reserved','creating')),
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
    seq           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, membership_id)
);
CREATE INDEX idx_inbox_participant_member ON inbox_participant(membership_id, seq);

CREATE TABLE inbox_mailbox (
    membership_id TEXT PRIMARY KEY,
    last_seq      INTEGER NOT NULL DEFAULT 0,
    read_seq      INTEGER NOT NULL DEFAULT 0,
    accept_from   TEXT NOT NULL DEFAULT 'tenant' CHECK (accept_from IN ('tenant','chosen')),
    dnd           INTEGER NOT NULL DEFAULT 0
);

-- One rule per (recipient, sender). A block replaces an allow, so "blocked" always wins.
CREATE TABLE inbox_sender_rule (
    membership_id        TEXT NOT NULL,
    sender_membership_id TEXT NOT NULL,
    rule                 TEXT NOT NULL CHECK (rule IN ('allow','block')),
    PRIMARY KEY (membership_id, sender_membership_id)
);
