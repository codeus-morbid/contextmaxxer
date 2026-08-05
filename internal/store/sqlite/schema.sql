CREATE TABLE IF NOT EXISTS _meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
  id INTEGER PRIMARY KEY,
  path TEXT UNIQUE NOT NULL,
  language TEXT NOT NULL,
  hash TEXT NOT NULL,
  mtime INTEGER NOT NULL,
  size INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS symbols (
  id INTEGER PRIMARY KEY,
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  qualified_name TEXT NOT NULL,
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  signature TEXT,
  docstring TEXT,
  body_excerpt TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_symbols_file ON symbols(file_id);
CREATE INDEX IF NOT EXISTS idx_symbols_qname ON symbols(qualified_name);

CREATE TABLE IF NOT EXISTS edges (
  src INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  dst INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  weight REAL NOT NULL DEFAULT 1.0,
  PRIMARY KEY (src, dst, kind)
);

CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst, kind);

-- symbol_vec is created dynamically in sqlite.go with the correct dimension for the active model.

CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(
  qualified_name, signature, docstring, body_excerpt,
  content='symbols', content_rowid='id'
);

CREATE TABLE IF NOT EXISTS annotations (
  id INTEGER PRIMARY KEY,
  symbol_id INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  content TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS feedback (
  id INTEGER PRIMARY KEY,
  query_hash TEXT NOT NULL,
  symbol_id INTEGER NOT NULL,
  signal TEXT NOT NULL,
  ts INTEGER NOT NULL
);
