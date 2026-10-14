-- A per-workspace preview epoch: a random value replaced on every start, reissue and stop. The
-- slug alone cannot tell a restart from the previous start when the workspace pins a fixed slug
-- (it comes back as the same string), so a temporary preview grant is saved only under the epoch
-- it was probed in. Absent row means no epoch yet. Replaced in the same transaction that clears
-- the grants. No foreign key (deleteWorkspaceTx lists what it removes).
CREATE TABLE IF NOT EXISTS preview_epoch(
  workspace_id TEXT PRIMARY KEY,
  epoch        TEXT NOT NULL
);
