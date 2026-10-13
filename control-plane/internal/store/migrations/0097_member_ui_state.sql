-- A member's Console-only UI state (ADR 0114): what the Console must read while the workspace is
-- stopped and what has to follow the member across devices, such as the first-run state that
-- feeds the Home view. Settings the Agent acts on stay in the workspace's ui-prefs (decision 1).
--
-- One closed-schema JSON document per membership. The CP validates it and merges PATCHes by
-- top-level key under an optimistic version, so two tabs writing different keys never undo
-- each other (decision 4).
--
-- Renumbered from 0095, which 0095_handoff_take.sql also took when both reached develop. A
-- deployment that ran the old number replays this file, so IF NOT EXISTS must stay
-- (TestRenumberedMigrationsStayReplayable).
--
-- No foreign key to membership: an older CP hard-deletes members with its own cascade list,
-- and a key it does not know about would make that delete fail (decision 7).
CREATE TABLE IF NOT EXISTS member_ui_state(
  membership_id TEXT PRIMARY KEY,
  state         TEXT NOT NULL,
  version       INTEGER NOT NULL,
  updated_at    TEXT NOT NULL
);
