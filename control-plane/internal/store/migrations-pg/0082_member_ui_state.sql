-- Postgres mirror of migrations/0097_member_ui_state.sql (ADR 0114). See the SQLite file for
-- what the table holds, why it declares no foreign key to membership, and why it was
-- renumbered from 0080 (IF NOT EXISTS must stay).
CREATE TABLE IF NOT EXISTS member_ui_state(
  membership_id TEXT PRIMARY KEY,
  state         TEXT NOT NULL,
  version       BIGINT NOT NULL,
  updated_at    TEXT NOT NULL
);
