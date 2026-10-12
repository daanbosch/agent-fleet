-- Postgres mirror of migrations/0095_member_ui_state.sql (ADR 0114). See the SQLite file for
-- what the table holds and why it declares no foreign key to membership.
CREATE TABLE IF NOT EXISTS member_ui_state(
  membership_id TEXT PRIMARY KEY,
  state         TEXT NOT NULL,
  version       BIGINT NOT NULL,
  updated_at    TEXT NOT NULL
);
