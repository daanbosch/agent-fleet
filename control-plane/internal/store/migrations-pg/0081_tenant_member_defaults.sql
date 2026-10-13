-- Postgres mirror of migrations/0096_tenant_member_defaults.sql (ADR 0114, P2).
CREATE TABLE IF NOT EXISTS tenant_member_defaults(
  tenant_id  TEXT PRIMARY KEY,
  defaults   TEXT NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
