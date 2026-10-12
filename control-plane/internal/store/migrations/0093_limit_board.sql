-- The opt-in limit board (ADR 0111 decisions 6 and 7, issue #1962).
--
-- Every table is keyed to a tenant membership, never to an identity: the publication setting
-- is per tenant membership, and what is published to one tenant must never reach another.
--
-- limit_observation holds the last real observation per (membership, kind, unit). observed_at
-- is the provider's own observation time and received_at the CP's, kept apart. An upsert only
-- ever replaces an older observation with a newer one, so the reaper and the browser cannot
-- walk a value backwards, and an identical observation from both is stored once.
CREATE TABLE limit_observation (
    membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    unit          TEXT NOT NULL,
    epoch         TEXT NOT NULL DEFAULT '',
    used_pct      REAL NOT NULL DEFAULT 0,
    resets_at     TEXT NOT NULL DEFAULT '',
    window_mins   INTEGER NOT NULL DEFAULT 0,
    observed_at   TEXT NOT NULL,
    received_at   TEXT NOT NULL,
    source        TEXT NOT NULL,
    PRIMARY KEY (membership_id, kind, unit)
);
-- The current login epoch per kind. It is an opaque value from the member's own Agent and is
-- never returned by any route. When it changes, the kind's observations under the old epoch
-- are deleted. epoch_since is the Agent's time for that login (RFC3339 UTC): only a strictly
-- later one may replace the epoch, so a report sent before a re-login and delivered after it
-- is refused instead of bringing the previous account back.
CREATE TABLE limit_epoch (
    membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    epoch         TEXT NOT NULL,
    epoch_since   TEXT NOT NULL DEFAULT '',
    changed_at    TEXT NOT NULL,
    PRIMARY KEY (membership_id, kind)
);
-- The session limit states the reaper last read from the member's workspace (states is a
-- JSON list of kind, state and optional resume time). One row per membership.
CREATE TABLE limit_session_state (
    membership_id TEXT PRIMARY KEY REFERENCES membership(id) ON DELETE CASCADE,
    states        TEXT NOT NULL DEFAULT '[]',
    observed_at   TEXT NOT NULL
);
-- What a member publishes, and to whom. No row means hidden, the default.
CREATE TABLE limit_publication (
    membership_id TEXT PRIMARY KEY REFERENCES membership(id) ON DELETE CASCADE,
    audience      TEXT NOT NULL DEFAULT 'none' CHECK (audience IN ('none','tenant','members')),
    level         TEXT NOT NULL DEFAULT 'state' CHECK (level IN ('state','band','exact')),
    updated_at    TEXT NOT NULL
);
-- The named members of an audience of 'members'. Both ends are memberships of one tenant,
-- which the route checks before writing.
CREATE TABLE limit_publication_member (
    membership_id        TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    viewer_membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    PRIMARY KEY (membership_id, viewer_membership_id)
);
CREATE INDEX idx_limit_publication_member_viewer ON limit_publication_member(viewer_membership_id);
-- The usage-reset notification's own login generation per (membership, source), apart from
-- the board's so it keeps working with the board switched off. A post from an older login must
-- neither arm, disarm nor fire it, and a new login clears every window's armed state of that
-- source. Empty values: nothing generation-stamped has been posted yet.
CREATE TABLE notification_usage_epoch (
    membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    source        TEXT NOT NULL,
    epoch         TEXT NOT NULL DEFAULT '',
    epoch_since   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (membership_id, source)
);
-- The tenant switch (ADR 0111 addendum item 4). No row means on.
CREATE TABLE limit_board_tenant (
    tenant_id  TEXT PRIMARY KEY REFERENCES tenant(id),
    enabled    INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL
)
