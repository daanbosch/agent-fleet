-- Optional scope groups a tenant administrator has confirmed their OAuth app grants, as a
-- comma-separated list (empty: none). The authorize URL asks for a group's scopes only when it is
-- listed here, because an authorize request naming a scope the app was never given fails for every
-- member until the administrator adds it in the provider's console.
--
-- Known groups: jira_software (provider jira) - the Jira Software scopes for boards, sprints and
-- epics, which do not accept Jira's classic scopes.
ALTER TABLE tenant_git_oauth ADD COLUMN scope_groups TEXT NOT NULL DEFAULT ''
