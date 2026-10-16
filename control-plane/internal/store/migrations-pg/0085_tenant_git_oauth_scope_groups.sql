-- Postgres mirror of migrations/0100_tenant_git_oauth_scope_groups.sql.
-- See the SQLite file for what the column means.
ALTER TABLE tenant_git_oauth ADD COLUMN scope_groups TEXT NOT NULL DEFAULT ''
