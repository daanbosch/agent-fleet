-- A temporary preview grant (ADR 0062, note on decision 6): the workspace owner opened a port
-- that is not on the preview list, and the CP bound that open to the listening sockets it saw
-- then (inodes, comma separated: one server often holds a tcp and a tcp6 socket for the port).
-- The row is only ever honoured for the slug it was made under, for the owner, and while the
-- Agent still sees one of those sockets, and slug rotation and stop delete the workspace's rows.
--
-- No foreign key to workspace: deleteWorkspaceTx lists what it removes, and a row whose slug no
-- longer matches the workspace's is inert anyway.
CREATE TABLE IF NOT EXISTS preview_temp_port(
  workspace_id TEXT NOT NULL,
  port         INTEGER NOT NULL,
  slug         TEXT NOT NULL,
  inodes       TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  PRIMARY KEY (workspace_id, port)
);
