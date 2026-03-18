CREATE TABLE IF NOT EXISTS nodes (
  node_id    TEXT    PRIMARY KEY,
  node_name  TEXT    NOT NULL,
  user_id    TEXT    NOT NULL,
  public_key TEXT    NOT NULL UNIQUE,
  mesh_ip    TEXT    NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS endpoints (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id    TEXT    NOT NULL REFERENCES nodes(node_id) ON DELETE CASCADE,
  endpoint   TEXT    NOT NULL,
  updated_at INTEGER NOT NULL
);
