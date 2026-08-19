// DECISION: using modernc.org/sqlite (pure-Go, no CGo) + modernc.org/sqlite/vec subpackage.
// Probe on Day 0 confirmed that modernc.org/sqlite v1.50.0 ships sqlite-vec as a bundled
// subpackage (modernc.org/sqlite/vec) that self-registers via sqlite3_auto_extension on import.
// No load_extension() call needed. Zero CGo, full cross-compile support maintained.
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// bodyFTSVersion gates the one-time build of symbol_body_fts on an index that
// predates it. Bump it to force every index to rebuild that table.
const bodyFTSVersion = "1"

type Store struct {
	db    *sql.DB
	path  string
	dim   int
	vec   vecCache
	graph graphCache
}

// DECISION: New requires modelName+dim so it can (a) create symbol_vec with the correct
// dimension on first init, and (b) refuse to open an existing index built with a different
// model — preventing silent dimension mismatches that would corrupt vec0 knn results.
func New(path, modelName string, dim int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("sqlite mkdirall: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON;",
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite pragma: %w", err)
		}
	}

	var version string
	_ = db.QueryRow(`SELECT value FROM _meta WHERE key='schema_version'`).Scan(&version)
	if version == "" {
		if _, err := db.Exec(schema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite migrate: %w", err)
		}
		vecDDL := fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS symbol_vec USING vec0(symbol_id INTEGER PRIMARY KEY, embedding FLOAT[%d])`, dim)
		if _, err := db.Exec(vecDDL); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite create symbol_vec: %w", err)
		}
		for _, kv := range [][2]string{
			{"schema_version", "2"},
			{"index_content_version", "0"},
			{"model_name", modelName},
			{"embedding_dim", fmt.Sprintf("%d", dim)},
		} {
			if _, err := db.Exec(`INSERT OR REPLACE INTO _meta(key,value) VALUES(?,?)`, kv[0], kv[1]); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("sqlite meta: %w", err)
			}
		}
	} else {
		if _, err := db.Exec(`ALTER TABLE files ADD COLUMN size INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite migrate files size: %w", err)
		}
		var storedModel string
		_ = db.QueryRow(`SELECT value FROM _meta WHERE key='model_name'`).Scan(&storedModel)
		if storedModel != "" && storedModel != modelName {
			_ = db.Close()
			return nil, fmt.Errorf("index was built with model %q, current model is %q — please reindex with: contextmaxxer index <path>", storedModel, modelName)
		}
	}
	// DECISION(2026-08): exact bodies live in a lazy side table. The searchable
	// symbol row remains compact, while expand_context can hydrate the exact
	// indexed snapshot. ASSUMES: generated indexes are rebuildable caches.
	// REVISIT IF: side-table storage becomes material relative to vectors.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS symbol_bodies (
		symbol_id INTEGER PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,
		body TEXT NOT NULL,
		sha256 TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate symbol bodies: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE edges ADD COLUMN call_line INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate edge call line: %w", err)
	}
	// DECISION(2026-08): keyword search reads body_excerpt, so the tail of a
	// capped symbol is invisible to every channel (measured: ~5% of symbols,
	// ~53% of their code). Index the lossless bodies separately, as external
	// content so the text is not stored twice. ASSUMES: symbol_bodies holds a
	// row for exactly the capped symbols. REVISIT IF: the cap goes away.
	if _, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS symbol_body_fts USING fts5(
		body, content='symbol_bodies', content_rowid='symbol_id'
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate body fts: %w", err)
	}
	// Existing indexes already carry the bodies, so this costs a rebuild pass
	// and no re-embedding. The flag makes it run once per index.
	var bodyFTSBuilt string
	_ = db.QueryRow(`SELECT value FROM _meta WHERE key='body_fts_version'`).Scan(&bodyFTSBuilt)
	if bodyFTSBuilt != bodyFTSVersion {
		if _, err := db.Exec(`INSERT INTO symbol_body_fts(symbol_body_fts) VALUES('rebuild')`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite build body fts: %w", err)
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO _meta(key,value) VALUES('body_fts_version',?)`, bodyFTSVersion); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite mark body fts: %w", err)
		}
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO _meta(key,value) VALUES('schema_version','2')`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite update schema version: %w", err)
	}
	var contentVersion string
	err = db.QueryRow(`SELECT value FROM _meta WHERE key='index_content_version'`).Scan(&contentVersion)
	if errors.Is(err, sql.ErrNoRows) || contentVersion == "" {
		var fileCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&fileCount); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite inspect content version: %w", err)
		}
		initialVersion := "0"
		if fileCount > 0 {
			initialVersion = "1"
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO _meta(key,value) VALUES('index_content_version',?)`, initialVersion); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite initialize content version: %w", err)
		}
	} else if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite read content version: %w", err)
	}
	return &Store{db: db, path: path, dim: dim}, nil
}

func (s *Store) IndexContentVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM _meta WHERE key='index_content_version'`).Scan(&version); err != nil {
		return 0, fmt.Errorf("get index content version: %w", err)
	}
	return version, nil
}

func (s *Store) SetIndexContentVersion(ctx context.Context, version int) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO _meta(key,value) VALUES('index_content_version',?)`, version); err != nil {
		return fmt.Errorf("set index content version: %w", err)
	}
	return nil
}

func (s *Store) SaveFile(ctx context.Context, f *store.File) (int64, error) {
	var existing int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM files WHERE path=?`, f.Path).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("save file lookup: %w", err)
	}
	if existing != 0 {
		_, err = s.db.ExecContext(ctx,
			`UPDATE files SET hash=?,mtime=?,size=?,language=? WHERE id=?`,
			f.Hash, f.Mtime, f.Size, f.Language, existing)
		if err != nil {
			return 0, fmt.Errorf("save file update: %w", err)
		}
		f.ID = existing
		return existing, nil
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO files(path,language,hash,mtime,size) VALUES(?,?,?,?,?)`,
		f.Path, f.Language, f.Hash, f.Mtime, f.Size)
	if err != nil {
		return 0, fmt.Errorf("save file insert: %w", err)
	}
	id, _ := res.LastInsertId()
	f.ID = id
	return id, nil
}

func (s *Store) GetFileByPath(ctx context.Context, path string) (*store.File, error) {
	f := &store.File{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id,path,language,hash,mtime,size FROM files WHERE path=?`, path).
		Scan(&f.ID, &f.Path, &f.Language, &f.Hash, &f.Mtime, &f.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file by path: %w", err)
	}
	return f, nil
}

func (s *Store) DeleteFile(ctx context.Context, id int64) error {
	// symbol_vec is a vec0 virtual table: FK cascade on symbols does not reach
	// it, so orphaned vectors must be removed explicitly.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM symbol_vec WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id=?)`, id); err != nil {
		return fmt.Errorf("delete file vectors: %w", err)
	}
	// Neither does the cascade reach the FTS indexes, and they must be cleared
	// while their content rows still exist: an external-content table reads the
	// row to remove its terms. Deleting the file first leaves both indexes
	// matching symbols that no longer exist, which then vanish at the JOIN
	// while still consuming the result limit.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM symbol_fts WHERE rowid IN (SELECT id FROM symbols WHERE file_id=?)`, id); err != nil {
		return fmt.Errorf("delete file fts: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM symbol_body_fts WHERE rowid IN (SELECT symbol_id FROM symbol_bodies WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id=?))`, id); err != nil {
		return fmt.Errorf("delete file body fts: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	s.invalidateVecCache()
	return nil
}

func (s *Store) ListFiles(ctx context.Context) ([]store.File, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,path,language,hash,mtime,size FROM files`)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	var files []store.File
	for rows.Next() {
		var f store.File
		if err := rows.Scan(&f.ID, &f.Path, &f.Language, &f.Hash, &f.Mtime, &f.Size); err != nil {
			return nil, fmt.Errorf("list files scan: %w", err)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Store) SaveSymbol(ctx context.Context, sym *store.Symbol) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("save symbol begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO symbols(file_id,name,kind,qualified_name,start_line,end_line,signature,docstring,body_excerpt)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		sym.FileID, sym.Name, sym.Kind, sym.QualifiedName,
		sym.StartLine, sym.EndLine, sym.Signature, sym.Docstring, sym.BodyExcerpt)
	if err != nil {
		return 0, fmt.Errorf("save symbol insert: %w", err)
	}
	id, _ := res.LastInsertId()
	sym.ID = id

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO symbol_fts(rowid, qualified_name, signature, docstring, body_excerpt)
		 VALUES(?,?,?,?,?)`,
		id, sym.QualifiedName, sym.Signature, sym.Docstring, sym.BodyExcerpt); err != nil {
		return 0, fmt.Errorf("save symbol fts: %w", err)
	}
	if sym.FullBody != "" {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO symbol_bodies(symbol_id,body,sha256) VALUES(?,?,?)`,
			id, sym.FullBody, bodySHA256(sym.FullBody)); err != nil {
			return 0, fmt.Errorf("save symbol full body: %w", err)
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO symbol_body_fts(rowid, body) VALUES(?,?)`,
			id, sym.FullBody); err != nil {
			return 0, fmt.Errorf("save symbol body fts: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("save symbol commit: %w", err)
	}
	return id, nil
}

func (s *Store) SaveSymbolBatch(ctx context.Context, symbols []store.Symbol) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("save symbol batch begin: %w", err)
	}
	defer tx.Rollback()

	symStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO symbols(file_id,name,kind,qualified_name,start_line,end_line,signature,docstring,body_excerpt)
		 VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, fmt.Errorf("save symbol batch prepare: %w", err)
	}
	defer symStmt.Close()

	ftsStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO symbol_fts(rowid, qualified_name, signature, docstring, body_excerpt)
		 VALUES(?,?,?,?,?)`)
	if err != nil {
		return nil, fmt.Errorf("save symbol batch fts prepare: %w", err)
	}
	defer ftsStmt.Close()

	bodyStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO symbol_bodies(symbol_id,body,sha256) VALUES(?,?,?)`)
	if err != nil {
		return nil, fmt.Errorf("save symbol batch body prepare: %w", err)
	}
	defer bodyStmt.Close()

	bodyFTSStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO symbol_body_fts(rowid, body) VALUES(?,?)`)
	if err != nil {
		return nil, fmt.Errorf("save symbol batch body fts prepare: %w", err)
	}
	defer bodyFTSStmt.Close()

	ids := make([]int64, len(symbols))
	for i, sym := range symbols {
		res, err := symStmt.ExecContext(ctx,
			sym.FileID, sym.Name, sym.Kind, sym.QualifiedName,
			sym.StartLine, sym.EndLine, sym.Signature, sym.Docstring, sym.BodyExcerpt)
		if err != nil {
			return nil, fmt.Errorf("save symbol batch exec: %w", err)
		}
		id, _ := res.LastInsertId()
		ids[i] = id
		if _, err = ftsStmt.ExecContext(ctx, id, sym.QualifiedName, sym.Signature, sym.Docstring, sym.BodyExcerpt); err != nil {
			return nil, fmt.Errorf("save symbol batch fts exec: %w", err)
		}
		if sym.FullBody != "" {
			if _, err = bodyStmt.ExecContext(ctx, id, sym.FullBody, bodySHA256(sym.FullBody)); err != nil {
				return nil, fmt.Errorf("save symbol batch body exec: %w", err)
			}
			if _, err = bodyFTSStmt.ExecContext(ctx, id, sym.FullBody); err != nil {
				return nil, fmt.Errorf("save symbol batch body fts exec: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("save symbol batch commit: %w", err)
	}
	return ids, nil
}

func (s *Store) DeleteSymbolsByFile(ctx context.Context, fileID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete symbols by file begin: %w", err)
	}
	defer tx.Rollback()

	if _, err = tx.ExecContext(ctx,
		`DELETE FROM symbol_fts WHERE rowid IN (SELECT id FROM symbols WHERE file_id=?)`, fileID); err != nil {
		return fmt.Errorf("delete symbols fts: %w", err)
	}
	// Must run while symbol_bodies still holds the rows: an external-content
	// FTS5 table reads the content row to remove its terms, and a cascade
	// delete would leave the index pointing at bodies that no longer exist.
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM symbol_body_fts WHERE rowid IN (SELECT symbol_id FROM symbol_bodies WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id=?))`, fileID); err != nil {
		return fmt.Errorf("delete symbols body fts: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM symbol_vec WHERE symbol_id IN (SELECT id FROM symbols WHERE file_id=?)`, fileID); err != nil {
		return fmt.Errorf("delete symbol embeddings: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM symbols WHERE file_id=?`, fileID); err != nil {
		return fmt.Errorf("delete symbols by file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateVecCache()
	return nil
}

func (s *Store) ListSymbolsByFile(ctx context.Context, fileID int64) ([]store.Symbol, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,file_id,name,kind,qualified_name,start_line,end_line,signature,docstring,body_excerpt
		 FROM symbols WHERE file_id=? ORDER BY id`, fileID)
	if err != nil {
		return nil, fmt.Errorf("list symbols by file: %w", err)
	}
	defer rows.Close()
	return scanSymbols(rows, "list symbols by file")
}

func (s *Store) ListSymbolsByLanguage(ctx context.Context, language string) ([]store.Symbol, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id,s.file_id,s.name,s.kind,s.qualified_name,s.start_line,s.end_line,s.signature,s.docstring,s.body_excerpt
		 FROM symbols s JOIN files f ON f.id=s.file_id
		 WHERE f.language=? ORDER BY s.id`, language)
	if err != nil {
		return nil, fmt.Errorf("list symbols by language: %w", err)
	}
	defer rows.Close()
	return scanSymbols(rows, "list symbols by language")
}

// ListSymbolsMissingEmbedding returns symbols with no symbol_vec row, joined
// with their file's path/language (EmbeddingText inputs). These exist after a
// structure-only (--fast) index; the backfill embeds them later.
func (s *Store) ListSymbolsMissingEmbedding(ctx context.Context) ([]store.SymbolToEmbed, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id,s.file_id,s.name,s.kind,s.qualified_name,s.start_line,s.end_line,s.signature,s.docstring,s.body_excerpt,
		        f.path,f.language
		 FROM symbols s
		 JOIN files f ON f.id=s.file_id
		 LEFT JOIN symbol_vec v ON v.symbol_id=s.id
		 WHERE v.symbol_id IS NULL ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("list symbols missing embedding: %w", err)
	}
	defer rows.Close()

	var out []store.SymbolToEmbed
	for rows.Next() {
		var it store.SymbolToEmbed
		if err := rows.Scan(&it.ID, &it.FileID, &it.Name, &it.Kind, &it.QualifiedName,
			&it.StartLine, &it.EndLine, &it.Signature, &it.Docstring, &it.BodyExcerpt,
			&it.Path, &it.Language); err != nil {
			return nil, fmt.Errorf("list symbols missing embedding scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list symbols missing embedding rows: %w", err)
	}
	return out, nil
}

func (s *Store) SaveEdge(ctx context.Context, e store.Edge) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO edges(src,dst,kind,weight,call_line) VALUES(?,?,?,?,?)`,
		e.Src, e.Dst, e.Kind, e.Weight, e.CallLine)
	if err != nil {
		return fmt.Errorf("save edge: %w", err)
	}
	return nil
}

func (s *Store) SaveEdgeBatch(ctx context.Context, edges []store.Edge) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save edge batch begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO edges(src,dst,kind,weight,call_line) VALUES(?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("save edge batch prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range edges {
		if _, err := stmt.ExecContext(ctx, e.Src, e.Dst, e.Kind, e.Weight, e.CallLine); err != nil {
			return fmt.Errorf("save edge batch exec: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save edge batch commit: %w", err)
	}
	return nil
}

// DECISION: vec0 virtual tables don't support INSERT OR REPLACE; use DELETE+INSERT in a tx.
// Vector accepted as JSON array string — confirmed by modernc.org/sqlite vec_test.go.
func (s *Store) UpsertEmbedding(ctx context.Context, symbolID int64, vec []float32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert embedding begin: %w", err)
	}
	defer tx.Rollback()

	if _, err = tx.ExecContext(ctx, `DELETE FROM symbol_vec WHERE symbol_id=?`, symbolID); err != nil {
		return fmt.Errorf("upsert embedding delete: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO symbol_vec(symbol_id, embedding) VALUES(?,?)`, symbolID, vectorJSON(vec)); err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("upsert embedding commit: %w", err)
	}
	s.invalidateVecCache()
	return nil
}

func (s *Store) UpsertEmbeddingBatch(ctx context.Context, embeddings []store.Embedding) error {
	if len(embeddings) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert embedding batch begin: %w", err)
	}
	defer tx.Rollback()

	delStmt, err := tx.PrepareContext(ctx, `DELETE FROM symbol_vec WHERE symbol_id=?`)
	if err != nil {
		return fmt.Errorf("upsert embedding batch delete prepare: %w", err)
	}
	defer delStmt.Close()
	insStmt, err := tx.PrepareContext(ctx, `INSERT INTO symbol_vec(symbol_id, embedding) VALUES(?,?)`)
	if err != nil {
		return fmt.Errorf("upsert embedding batch insert prepare: %w", err)
	}
	defer insStmt.Close()

	for _, emb := range embeddings {
		if _, err := delStmt.ExecContext(ctx, emb.SymbolID); err != nil {
			return fmt.Errorf("upsert embedding batch delete: %w", err)
		}
		if _, err := insStmt.ExecContext(ctx, emb.SymbolID, vectorJSON(emb.Vector)); err != nil {
			return fmt.Errorf("upsert embedding batch insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upsert embedding batch commit: %w", err)
	}
	s.invalidateVecCache()
	return nil
}

func (s *Store) GetEmbedding(ctx context.Context, symbolID int64) ([]float32, bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT embedding FROM symbol_vec WHERE symbol_id=?`, symbolID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get embedding: %w", err)
	}
	if len(raw)%4 != 0 {
		return nil, false, fmt.Errorf("get embedding: invalid raw length %d", len(raw))
	}
	vec := make([]float32, len(raw)/4)
	for i := range vec {
		bits := binary.LittleEndian.Uint32(raw[i*4 : i*4+4])
		vec[i] = math.Float32frombits(bits)
	}
	return vec, true, nil
}

// SearchByVector returns the topK nearest symbols. Served from the in-memory
// vector cache; see SearchByVectorScored.
func (s *Store) SearchByVector(ctx context.Context, embedding []float32, topK int) ([]store.Symbol, error) {
	scored, err := s.SearchByVectorScored(ctx, embedding, topK)
	if err != nil {
		return nil, err
	}
	syms := make([]store.Symbol, len(scored))
	for i, ss := range scored {
		syms[i] = ss.Symbol
	}
	return syms, nil
}

func (s *Store) GetEdges(ctx context.Context, symbolID int64) ([]store.Edge, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT src,dst,kind,weight,call_line FROM edges WHERE src=? OR dst=?`, symbolID, symbolID)
	if err != nil {
		return nil, fmt.Errorf("get edges: %w", err)
	}
	defer rows.Close()
	var edges []store.Edge
	for rows.Next() {
		var e store.Edge
		if err := rows.Scan(&e.Src, &e.Dst, &e.Kind, &e.Weight, &e.CallLine); err != nil {
			return nil, fmt.Errorf("get edges scan: %w", err)
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// DECISION: score = 1/(1+distance) where distance is L2. For L2-normalized vectors,
// cosine similarity = 1 - d²/2, but 1/(1+d) is simpler and monotonically equivalent for ranking.
// Served from the in-memory vector cache (see veccache.go); falls back to the
// vec0 KNN query if the cache cannot answer (load failure, dim mismatch).
func (s *Store) SearchByVectorScored(ctx context.Context, vec []float32, k int) ([]store.ScoredSymbol, error) {
	if err := s.ensureVecCache(ctx); err == nil {
		if hits, ok := s.searchVecCache(vec, k); ok {
			return s.hydrateScoredHits(ctx, hits)
		}
	}
	return s.searchByVectorScoredVec0(ctx, vec, k)
}

// hydrateScoredHits loads symbol rows for cache hits, preserving score order.
func (s *Store) hydrateScoredHits(ctx context.Context, hits []vecHit) ([]store.ScoredSymbol, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	syms, err := s.GetSymbolsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("hydrate vec hits: %w", err)
	}
	byID := make(map[int64]store.Symbol, len(syms))
	for _, sym := range syms {
		byID[sym.ID] = sym
	}
	result := make([]store.ScoredSymbol, 0, len(hits))
	for _, h := range hits {
		sym, ok := byID[h.id]
		if !ok {
			// Orphaned vector (symbol deleted); skip.
			continue
		}
		result = append(result, store.ScoredSymbol{Symbol: sym, Score: h.score})
	}
	return result, nil
}

func (s *Store) searchByVectorScoredVec0(ctx context.Context, vec []float32, k int) ([]store.ScoredSymbol, error) {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	json := "[" + strings.Join(parts, ",") + "]"
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id,s.file_id,s.name,s.kind,s.qualified_name,s.start_line,s.end_line,
		       s.signature,s.docstring,s.body_excerpt,knn.distance
		FROM (
			SELECT symbol_id, distance FROM symbol_vec WHERE embedding MATCH ? AND k = ?
			ORDER BY distance
		) AS knn
		JOIN symbols s ON s.id=knn.symbol_id`, json, k)
	if err != nil {
		return nil, fmt.Errorf("search by vector scored: %w", err)
	}
	defer rows.Close()
	var result []store.ScoredSymbol
	for rows.Next() {
		var ss store.ScoredSymbol
		var dist float32
		if err := rows.Scan(&ss.ID, &ss.FileID, &ss.Name, &ss.Kind, &ss.QualifiedName,
			&ss.StartLine, &ss.EndLine, &ss.Signature, &ss.Docstring, &ss.BodyExcerpt, &dist); err != nil {
			return nil, fmt.Errorf("search by vector scored scan: %w", err)
		}
		ss.Score = 1.0 / (1.0 + dist)
		result = append(result, ss)
	}
	return result, rows.Err()
}

// sqlVarLimit caps how many bound parameters go into one IN(...) clause. SQLite's
// SQLITE_MAX_VARIABLE_NUMBER is 999 on older builds; we stay under it and batch
// larger id sets. Without this, graph enrichment on large dense graphs (e.g.
// CockroachDB after type-aware resolution) hits "too many SQL variables" when a
// neighbor id set exceeds the limit.
const sqlVarLimit = 900

func chunkInt64(ids []int64, size int) [][]int64 {
	if size <= 0 || len(ids) <= size {
		return [][]int64{ids}
	}
	chunks := make([][]int64, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

// queryIDChunks runs `sqlFmt` (which must contain a single %s for the placeholder
// list) once per chunk of ids, invoking scan for every row. It batches to stay
// under sqlVarLimit bound parameters per statement.
func (s *Store) queryIDChunks(ctx context.Context, ids []int64, sqlFmt string, scan func(*sql.Rows) error) error {
	for _, chunk := range chunkInt64(ids, sqlVarLimit) {
		if len(chunk) == 0 {
			continue
		}
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(sqlFmt, strings.Join(placeholders, ",")), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := scan(rows); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func (s *Store) GetSymbolsByIDs(ctx context.Context, ids []int64) ([]store.Symbol, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var syms []store.Symbol
	err := s.queryIDChunks(ctx, ids,
		`SELECT id,file_id,name,kind,qualified_name,start_line,end_line,signature,docstring,body_excerpt
		 FROM symbols WHERE id IN (%s)`,
		func(rows *sql.Rows) error {
			var sym store.Symbol
			if err := rows.Scan(&sym.ID, &sym.FileID, &sym.Name, &sym.Kind, &sym.QualifiedName,
				&sym.StartLine, &sym.EndLine, &sym.Signature, &sym.Docstring, &sym.BodyExcerpt); err != nil {
				return err
			}
			syms = append(syms, sym)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("get symbols by ids: %w", err)
	}
	return syms, nil
}

// GetSymbolBody returns the exact body captured by the index. A legacy
// truncated excerpt without its lossless side-table row is never passed off as
// complete: callers get a reindex-required error instead.
func (s *Store) GetSymbolBody(ctx context.Context, symbolID int64) (store.SymbolBody, error) {
	var excerpt string
	var fullBody, digest sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT s.body_excerpt,b.body,b.sha256
		FROM symbols s
		LEFT JOIN symbol_bodies b ON b.symbol_id=s.id
		WHERE s.id=?`, symbolID).Scan(&excerpt, &fullBody, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return store.SymbolBody{}, store.ErrNotFound
	}
	if err != nil {
		return store.SymbolBody{}, fmt.Errorf("get symbol body: %w", err)
	}
	if fullBody.Valid {
		return store.SymbolBody{SymbolID: symbolID, Body: fullBody.String, SHA256: digest.String}, nil
	}
	if strings.HasSuffix(excerpt, "\n// ... [truncated]") {
		return store.SymbolBody{}, fmt.Errorf("%w: symbol %d has a lossy legacy excerpt; run contextmaxxer index --force <repo>", store.ErrReindexRequired, symbolID)
	}
	return store.SymbolBody{SymbolID: symbolID, Body: excerpt, SHA256: bodySHA256(excerpt)}, nil
}

func bodySHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func scanSymbols(rows *sql.Rows, op string) ([]store.Symbol, error) {
	var syms []store.Symbol
	for rows.Next() {
		var sym store.Symbol
		if err := rows.Scan(&sym.ID, &sym.FileID, &sym.Name, &sym.Kind, &sym.QualifiedName,
			&sym.StartLine, &sym.EndLine, &sym.Signature, &sym.Docstring, &sym.BodyExcerpt); err != nil {
			return nil, fmt.Errorf("%s scan: %w", op, err)
		}
		syms = append(syms, sym)
	}
	return syms, rows.Err()
}

func vectorJSON(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (s *Store) GetFilesByIDs(ctx context.Context, ids []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(ids))
	err := s.queryIDChunks(ctx, ids, `SELECT id,path FROM files WHERE id IN (%s)`,
		func(rows *sql.Rows) error {
			var id int64
			var path string
			if err := rows.Scan(&id, &path); err != nil {
				return err
			}
			result[id] = path
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("get files by ids: %w", err)
	}
	return result, nil
}

func (s *Store) GetEmbeddingsByIDs(ctx context.Context, ids []int64) (map[int64][]float32, error) {
	result := make(map[int64][]float32, len(ids))
	err := s.queryIDChunks(ctx, ids, `SELECT symbol_id, embedding FROM symbol_vec WHERE symbol_id IN (%s)`,
		func(rows *sql.Rows) error {
			var symbolID int64
			var raw []byte
			if err := rows.Scan(&symbolID, &raw); err != nil {
				return err
			}
			if len(raw)%4 != 0 {
				return fmt.Errorf("invalid raw length %d for symbol %d", len(raw), symbolID)
			}
			vec := make([]float32, len(raw)/4)
			for i := range vec {
				bits := binary.LittleEndian.Uint32(raw[i*4 : i*4+4])
				vec[i] = math.Float32frombits(bits)
			}
			result[symbolID] = vec
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("get embeddings by ids: %w", err)
	}
	return result, nil
}

func (s *Store) ListAllSymbolIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM symbols`)
	if err != nil {
		return nil, fmt.Errorf("list all symbol ids: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list all symbol ids scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DECISION: bm25() in FTS5 returns negative values (lower = more relevant).
// We invert to Score = -bm25_value so higher Score = better match, consistent with
// SearchByVectorScored where Score = 1/(1+distance). ORDER BY rank uses the raw
// bm25 implicitly (SQLite FTS5 sets rank = bm25 by default).
func (s *Store) SearchByText(ctx context.Context, query string, k int) ([]store.ScoredSymbol, error) {
	ftsQuery := preprocessFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	return s.ftsSearch(ctx, headFTSSQL, ftsQuery, k)
}

// SearchByBodyText searches the lossless bodies. It is a separate ranking, not
// extra rows on SearchByText's: seeding fuses channels by rank, so appending
// these to the head list would hand them the worst ranks and a vote near zero
// however well they matched. Callers fuse it as its own channel.
func (s *Store) SearchByBodyText(ctx context.Context, query string, k int) ([]store.ScoredSymbol, error) {
	ftsQuery := preprocessFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	return s.ftsSearch(ctx, bodyFTSSQL, ftsQuery, k)
}

const headFTSSQL = `
	SELECT f.rowid, bm25(symbol_fts),
	       s.id, s.file_id, s.name, s.kind, s.qualified_name,
	       s.start_line, s.end_line, s.signature, s.docstring, s.body_excerpt
	FROM symbol_fts f
	JOIN symbols s ON s.id = f.rowid
	WHERE symbol_fts MATCH ?
	ORDER BY rank
	LIMIT ?`

const bodyFTSSQL = `
	SELECT f.rowid, bm25(symbol_body_fts),
	       s.id, s.file_id, s.name, s.kind, s.qualified_name,
	       s.start_line, s.end_line, s.signature, s.docstring, s.body_excerpt
	FROM symbol_body_fts f
	JOIN symbols s ON s.id = f.rowid
	WHERE symbol_body_fts MATCH ?
	ORDER BY rank
	LIMIT ?`

func (s *Store) ftsSearch(ctx context.Context, sqlText, ftsQuery string, k int) ([]store.ScoredSymbol, error) {
	rows, err := s.db.QueryContext(ctx, sqlText, ftsQuery, k)
	if err != nil {
		return nil, fmt.Errorf("search by text: %w", err)
	}
	defer rows.Close()
	var result []store.ScoredSymbol
	for rows.Next() {
		var ss store.ScoredSymbol
		var ftsRowID int64
		var bm25val float64
		if err := rows.Scan(&ftsRowID, &bm25val,
			&ss.ID, &ss.FileID, &ss.Name, &ss.Kind, &ss.QualifiedName,
			&ss.StartLine, &ss.EndLine, &ss.Signature, &ss.Docstring, &ss.BodyExcerpt); err != nil {
			return nil, fmt.Errorf("search by text scan: %w", err)
		}
		ss.Score = float32(-bm25val)
		result = append(result, ss)
	}
	return result, rows.Err()
}

var ftsStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "do": true, "does": true,
	"in": true, "on": true, "at": true, "to": true, "of": true,
	"for": true, "with": true, "where": true, "how": true, "what": true,
	"why": true, "when": true, "we": true, "our": true, "my": true,
	"this": true, "that": true, "these": true, "those": true,
	"it": true, "its": true,
}

func preprocessFTSQuery(query string) string {
	var b strings.Builder
	terms := tokenizeFTS(query)
	first := true
	for _, t := range terms {
		if !first {
			b.WriteString(" OR ")
		}
		first = false
		// DECISION(2026-06): wrap each term as an FTS5 string literal. Terms are
		// already [\p{L}\p{N}_]+ (no embedded quote can break out), and quoting
		// neutralizes any term that collides with an FTS5 keyword (AND/OR/NOT/NEAR),
		// which would otherwise be a MATCH syntax error.
		b.WriteByte('"')
		b.WriteString(t)
		b.WriteByte('"')
	}
	return b.String()
}

func tokenizeFTS(query string) []string {
	var buf strings.Builder
	var words []string
	for _, r := range query + " " {
		// DECISION(2026-06): allowlist tokenization — keep only letters, digits and
		// underscore; every other rune is a separator. FTS5 query barewords are
		// [\p{L}\p{N}_]+, so any other char left inside a term is a MATCH syntax error
		// (e.g. "internal/sql/colexec" on the '/', or "tree-sitter" where '-' is the
		// NOT operator). Splitting on the full non-word set fixes the whole class.
		// REVISIT IF: we want phrase/path matching that needs to preserve a delimiter.
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			buf.WriteRune(r)
			continue
		}
		w := buf.String()
		buf.Reset()
		if w != "" {
			words = append(words, w)
		}
	}
	seen := make(map[string]bool)
	var terms []string
	for _, w := range words {
		lower := strings.ToLower(w)
		if ftsStopwords[lower] || len(w) < 2 {
			continue
		}
		expanded := expandIdentifier(w)
		for _, t := range expanded {
			if !seen[t] {
				seen[t] = true
				terms = append(terms, t)
			}
		}
	}
	return terms
}

func expandIdentifier(word string) []string {
	parts := splitCamelSnake(word)
	if len(parts) <= 1 {
		return []string{word}
	}
	result := []string{word}
	for _, p := range parts {
		if len(p) >= 2 && !ftsStopwords[strings.ToLower(p)] {
			result = append(result, p)
		}
	}
	return result
}

func splitCamelSnake(s string) []string {
	if strings.Contains(s, "_") {
		return strings.Split(s, "_")
	}
	var parts []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			parts = append(parts, s[start:i])
			start = i
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func (s *Store) GetCallerEdges(ctx context.Context, dstIDs []int64, limitPerSymbol int) (map[int64][]int64, error) {
	result := make(map[int64][]int64, len(dstIDs))
	counts := make(map[int64]int, len(dstIDs))
	// Each dst lands in exactly one chunk (ids are partitioned), so per-symbol
	// counts are correct across chunks.
	err := s.queryIDChunks(ctx, dstIDs, `SELECT src, dst FROM edges WHERE dst IN (%s)`,
		func(rows *sql.Rows) error {
			var src, dst int64
			if err := rows.Scan(&src, &dst); err != nil {
				return err
			}
			if limitPerSymbol <= 0 || counts[dst] < limitPerSymbol {
				result[dst] = append(result[dst], src)
				counts[dst]++
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("get caller edges: %w", err)
	}
	return result, nil
}

func (s *Store) GetCalleeEdges(ctx context.Context, srcIDs []int64, limitPerSymbol int) (map[int64][]int64, error) {
	result := make(map[int64][]int64, len(srcIDs))
	counts := make(map[int64]int, len(srcIDs))
	// Each src lands in exactly one chunk, so per-symbol counts stay correct.
	err := s.queryIDChunks(ctx, srcIDs, `SELECT src, dst FROM edges WHERE src IN (%s)`,
		func(rows *sql.Rows) error {
			var src, dst int64
			if err := rows.Scan(&src, &dst); err != nil {
				return err
			}
			if limitPerSymbol <= 0 || counts[src] < limitPerSymbol {
				result[src] = append(result[src], dst)
				counts[src]++
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("get callee edges: %w", err)
	}
	return result, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}
