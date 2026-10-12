-- Postgres mirror of migrations/0095_handoff_take.sql (issue #2020). See the SQLite file for
-- what the columns mean.
ALTER TABLE session_handoff_offer ADD COLUMN take_token TEXT NOT NULL DEFAULT '';
ALTER TABLE session_handoff_offer ADD COLUMN taken_at TEXT NOT NULL DEFAULT '';
ALTER TABLE session_handoff_offer ADD COLUMN take_confirmed_at TEXT NOT NULL DEFAULT '';
