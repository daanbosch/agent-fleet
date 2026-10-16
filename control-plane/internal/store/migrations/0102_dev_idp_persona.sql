-- Test personas of the development OIDC IdP (ADR 0116, #2223): identities a member can choose
-- on the consent page to exercise role checks. Forward compatible, nothing existing changes.
--
-- handle is the only thing the issued address and subject derive from, validated to
-- [a-z0-9._-] before it is stored, so the pair (workspace_id, handle) is also the identity.
-- No foreign key to workspace: deleteWorkspaceTx lists what it removes.
CREATE TABLE IF NOT EXISTS dev_idp_persona(
  id           TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  handle       TEXT NOT NULL,
  name         TEXT NOT NULL DEFAULT '',
  created_at   BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS dev_idp_persona_handle ON dev_idp_persona(workspace_id, handle);
