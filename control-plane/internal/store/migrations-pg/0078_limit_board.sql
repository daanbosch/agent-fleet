-- Postgres mirror of migrations/0093_limit_board.sql (issue #1962, ADR 0111 decision 6).
-- See the SQLite file for what each table holds.
CREATE TABLE limit_observation (
    membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    unit          TEXT NOT NULL,
    epoch         TEXT NOT NULL DEFAULT '',
    used_pct      DOUBLE PRECISION NOT NULL DEFAULT 0,
    resets_at     TEXT NOT NULL DEFAULT '',
    window_mins   INTEGER NOT NULL DEFAULT 0,
    observed_at   TEXT NOT NULL,
    received_at   TEXT NOT NULL,
    source        TEXT NOT NULL,
    PRIMARY KEY (membership_id, kind, unit)
);
CREATE TABLE limit_epoch (
    membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    epoch         TEXT NOT NULL,
    epoch_since   TEXT NOT NULL DEFAULT '',
    changed_at    TEXT NOT NULL,
    PRIMARY KEY (membership_id, kind)
);
CREATE TABLE limit_session_state (
    membership_id TEXT PRIMARY KEY REFERENCES membership(id) ON DELETE CASCADE,
    states        TEXT NOT NULL DEFAULT '[]',
    observed_at   TEXT NOT NULL
);
CREATE TABLE limit_publication (
    membership_id TEXT PRIMARY KEY REFERENCES membership(id) ON DELETE CASCADE,
    audience      TEXT NOT NULL DEFAULT 'none' CHECK (audience IN ('none','tenant','members')),
    level         TEXT NOT NULL DEFAULT 'state' CHECK (level IN ('state','band','exact')),
    updated_at    TEXT NOT NULL
);
CREATE TABLE limit_publication_member (
    membership_id        TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    viewer_membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    PRIMARY KEY (membership_id, viewer_membership_id)
);
CREATE INDEX idx_limit_publication_member_viewer ON limit_publication_member(viewer_membership_id);
CREATE TABLE notification_usage_epoch (
    membership_id TEXT NOT NULL REFERENCES membership(id) ON DELETE CASCADE,
    source        TEXT NOT NULL,
    epoch         TEXT NOT NULL DEFAULT '',
    epoch_since   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (membership_id, source)
);
CREATE TABLE limit_board_tenant (
    tenant_id  TEXT PRIMARY KEY REFERENCES tenant(id),
    enabled    INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL
)
