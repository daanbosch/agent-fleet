-- A tenant's defaults for its members' Console (ADR 0114 decision 8, P2): today the agent a new
-- member is pointed at first. One row per tenant holding the closed-schema JSON the CP
-- validated, because the admin saves it as a whole and the CP never queries by its keys.
-- No row means no defaults, so an upgraded deployment changes nothing.
CREATE TABLE IF NOT EXISTS tenant_member_defaults(
  tenant_id  TEXT PRIMARY KEY,
  defaults   TEXT NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
