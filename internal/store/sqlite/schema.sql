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

-- Full bodies are stored only for symbols whose searchable excerpt is lossy.
-- Keeping them out of symbols prevents vector/FTS/query scans from hydrating
-- large source blobs during normal retrieval.
CREATE TABLE IF NOT EXISTS symbol_bodies (
  symbol_id INTEGER PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,
  body TEXT NOT NULL,
  sha256 TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS edges (
  src INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  dst INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  weight REAL NOT NULL DEFAULT 1.0,
  call_line INTEGER NOT NULL DEFAULT 0,
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

-- Full bodies indexed for keyword search. External content over symbol_bodies:
-- the tail of a capped symbol is otherwise unreachable by every channel, and
-- external content means the text is indexed without a second copy.
CREATE VIRTUAL TABLE IF NOT EXISTS symbol_body_fts USING fts5(
  body, content='symbol_bodies', content_rowid='symbol_id'
);
