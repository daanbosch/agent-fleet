-- The development OIDC IdP (ADR 0116): signing keys, one client registration per workspace, and
-- the short-lived authorization codes / access tokens it hands out.
--
-- private_enc and secret_enc are custodian-sealed (key_ref says under which key, "" = stored
-- as is on a deployment with no master key), and both columns are listed in sealedColumns so
-- `rewrap-keys` walks them.
--
-- No foreign key to workspace: deleteWorkspaceTx lists what it removes, the same as
-- preview_temp_port. Grants carry their own expiry and are purged on the next insert.
CREATE TABLE IF NOT EXISTS dev_idp_key(
  kid         TEXT PRIMARY KEY,
  private_enc TEXT NOT NULL,
  key_ref     TEXT NOT NULL DEFAULT '',
  public_key  TEXT NOT NULL,
  state       TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  retired_at  BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS dev_idp_client(
  workspace_id TEXT PRIMARY KEY,
  membership_id TEXT NOT NULL,
  client_id    TEXT NOT NULL UNIQUE,
  secret_enc   TEXT NOT NULL,
  key_ref      TEXT NOT NULL DEFAULT '',
  created_at   BIGINT NOT NULL
);

-- hash is the SHA-256 of the code / access token (the value itself is never stored). kind is
-- 'code' or 'access'. An access token names the code it came from in parent, so a replayed
-- code can take the tokens it already produced down with it.
CREATE TABLE IF NOT EXISTS dev_idp_grant(
  hash          TEXT PRIMARY KEY,
  kind          TEXT NOT NULL,
  parent        TEXT NOT NULL DEFAULT '',
  client_id     TEXT NOT NULL,
  workspace_id  TEXT NOT NULL,
  membership_id TEXT NOT NULL,
  identity_id   TEXT NOT NULL DEFAULT '',
  redirect_uri  TEXT NOT NULL DEFAULT '',
  challenge     TEXT NOT NULL DEFAULT '',
  nonce         TEXT NOT NULL DEFAULT '',
  scope         TEXT NOT NULL DEFAULT '',
  claims        TEXT NOT NULL DEFAULT '',
  expires_at    BIGINT NOT NULL,
  used_at       BIGINT NOT NULL DEFAULT 0,
  revoked       BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS dev_idp_grant_parent ON dev_idp_grant(parent);
CREATE INDEX IF NOT EXISTS dev_idp_grant_expires ON dev_idp_grant(expires_at);
