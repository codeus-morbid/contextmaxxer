package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"), "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_ModelMismatchError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mismatch.db")

	s, err := New(dbPath, "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	s.Close()

	_, err = New(dbPath, "jina-embeddings-v2-base-code", 768)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bge-small-en-v1.5")
	require.Contains(t, err.Error(), "jina-embeddings-v2-base-code")
}

func TestStore_SaveAndGetFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/a/b.go", Language: "go", Hash: "abc123", Mtime: 1000}
	id, err := s.SaveFile(ctx, f)
	require.NoError(t, err)
	require.Positive(t, id)

	got, err := s.GetFileByPath(ctx, "/a/b.go")
	require.NoError(t, err)
	require.Equal(t, id, got.ID)
	require.Equal(t, "go", got.Language)
	require.Equal(t, "abc123", got.Hash)
	require.Equal(t, int64(1000), got.Mtime)

	_, err = s.GetFileByPath(ctx, "/nonexistent")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestStore_SaveFile_UpdateExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/a/b.go", Language: "go", Hash: "old", Mtime: 1}
	id1, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	f2 := &store.File{Path: "/a/b.go", Language: "go", Hash: "new", Mtime: 2}
	id2, err := s.SaveFile(ctx, f2)
	require.NoError(t, err)
	require.Equal(t, id1, id2)

	got, err := s.GetFileByPath(ctx, "/a/b.go")
	require.NoError(t, err)
	require.Equal(t, "new", got.Hash)
}

func TestStore_DeleteFileCascades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/x.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	s1 := &store.Symbol{FileID: fid, Name: "A", Kind: "func", QualifiedName: "pkg.A", BodyExcerpt: "x"}
	id1, err := s.SaveSymbol(ctx, s1)
	require.NoError(t, err)

	s2 := &store.Symbol{FileID: fid, Name: "B", Kind: "func", QualifiedName: "pkg.B", BodyExcerpt: "y"}
	id2, err := s.SaveSymbol(ctx, s2)
	require.NoError(t, err)

	err = s.SaveEdge(ctx, store.Edge{Src: id1, Dst: id2, Kind: "calls", Weight: 1.0})
	require.NoError(t, err)

	err = s.DeleteFile(ctx, fid)
	require.NoError(t, err)

	var symCount int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbols WHERE file_id=?`, fid).Scan(&symCount)
	require.NoError(t, err)
	require.Zero(t, symCount)

	var edgeCount int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE src=? OR dst=?`, id1, id2).Scan(&edgeCount)
	require.NoError(t, err)
	require.Zero(t, edgeCount)
}

func TestStore_SaveSymbolBatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/batch.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	syms := make([]store.Symbol, 10)
	for i := range syms {
		syms[i] = store.Symbol{
			FileID: fid, Name: "Sym", Kind: "func",
			QualifiedName: "pkg.Sym", BodyExcerpt: "x",
		}
	}

	ids, err := s.SaveSymbolBatch(ctx, syms)
	require.NoError(t, err)
	require.Len(t, ids, 10)

	seen := make(map[int64]bool)
	for _, id := range ids {
		require.Positive(t, id)
		require.False(t, seen[id], "duplicate ID %d", id)
		seen[id] = true
	}
}

func TestStore_GetSymbolBodyIsLosslessAndRejectsLegacyTruncation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fid, err := s.SaveFile(ctx, &store.File{Path: "/body.go", Language: "go", Hash: "h"})
	require.NoError(t, err)

	full := "func Huge() {\n" + strings.Repeat("use()\n", 1000) + "}\n"
	ids, err := s.SaveSymbolBatch(ctx, []store.Symbol{{
		FileID: fid, Name: "Huge", Kind: store.KindFunction, QualifiedName: "pkg.Huge",
		BodyExcerpt: full[:2000] + "\n// ... [truncated]", FullBody: full,
	}})
	require.NoError(t, err)
	body, err := s.GetSymbolBody(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, full, body.Body)
	require.Len(t, body.SHA256, 64)

	legacyID, err := s.SaveSymbol(ctx, &store.Symbol{
		FileID: fid, Name: "Legacy", Kind: store.KindFunction, QualifiedName: "pkg.Legacy",
		BodyExcerpt: "func Legacy() {\n// ... [truncated]",
	})
	require.NoError(t, err)
	_, err = s.GetSymbolBody(ctx, legacyID)
	require.ErrorIs(t, err, store.ErrReindexRequired)
}

func TestStore_UpsertEmbedding_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/emb.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	sym := &store.Symbol{FileID: fid, Name: "Emb", Kind: "func", QualifiedName: "pkg.Emb", BodyExcerpt: "x"}
	sid, err := s.SaveSymbol(ctx, sym)
	require.NoError(t, err)

	vec := make([]float32, 384)
	vec[0] = 0.5
	vec[1] = -0.25

	err = s.UpsertEmbedding(ctx, sid, vec)
	require.NoError(t, err)

	var symID int64
	var rawEmb []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT symbol_id, embedding FROM symbol_vec WHERE symbol_id=?`, sid).
		Scan(&symID, &rawEmb)
	require.NoError(t, err)
	require.Equal(t, sid, symID)
	// 384 float32 values * 4 bytes each
	require.Equal(t, 384*4, len(rawEmb), "embedding should be 384*4 bytes (binary blob)")

	// idempotent upsert
	err = s.UpsertEmbedding(ctx, sid, vec)
	require.NoError(t, err)

	var cnt int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_vec WHERE symbol_id=?`, sid).Scan(&cnt)
	require.NoError(t, err)
	require.Equal(t, 1, cnt)
}

func TestStore_UpsertEmbeddingBatchAndGetEmbedding(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	fid, err := s.SaveFile(ctx, &store.File{Path: "/emb-batch.go", Language: "go", Hash: "h", Mtime: 0})
	require.NoError(t, err)
	ids, err := s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: fid, Name: "A", Kind: store.KindFunction, QualifiedName: "pkg.A", BodyExcerpt: "func A() {}"},
		{FileID: fid, Name: "B", Kind: store.KindFunction, QualifiedName: "pkg.B", BodyExcerpt: "func B() {}"},
	})
	require.NoError(t, err)

	vecA := make([]float32, 384)
	vecB := make([]float32, 384)
	vecA[0] = 0.25
	vecB[0] = 0.75

	err = s.UpsertEmbeddingBatch(ctx, []store.Embedding{
		{SymbolID: ids[0], Vector: vecA},
		{SymbolID: ids[1], Vector: vecB},
	})
	require.NoError(t, err)

	gotA, ok, err := s.GetEmbedding(ctx, ids[0])
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, gotA, 384)
	require.InEpsilon(t, 0.25, gotA[0], 0.0001)

	_, ok, err = s.GetEmbedding(ctx, 999999)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestStore_ListSymbolsByFileAndLanguage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	goFile, err := s.SaveFile(ctx, &store.File{Path: "/pkg/a.go", Language: "go", Hash: "a", Mtime: 1})
	require.NoError(t, err)
	tsFile, err := s.SaveFile(ctx, &store.File{Path: "/web/a.ts", Language: "typescript", Hash: "b", Mtime: 1})
	require.NoError(t, err)
	_, err = s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: goFile, Name: "Run", Kind: store.KindFunction, QualifiedName: "pkg.Run", BodyExcerpt: "func Run() {}"},
		{FileID: tsFile, Name: "render", Kind: store.KindFunction, QualifiedName: "render", BodyExcerpt: "function render() {}"},
	})
	require.NoError(t, err)

	fileSyms, err := s.ListSymbolsByFile(ctx, goFile)
	require.NoError(t, err)
	require.Len(t, fileSyms, 1)
	require.Equal(t, "pkg.Run", fileSyms[0].QualifiedName)

	goSyms, err := s.ListSymbolsByLanguage(ctx, "go")
	require.NoError(t, err)
	require.Len(t, goSyms, 1)
	require.Equal(t, "pkg.Run", goSyms[0].QualifiedName)
}

func TestStore_DeleteSymbolsByFileRemovesEmbeddings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	fid, err := s.SaveFile(ctx, &store.File{Path: "/cleanup.go", Language: "go", Hash: "h", Mtime: 0})
	require.NoError(t, err)
	sid, err := s.SaveSymbol(ctx, &store.Symbol{FileID: fid, Name: "Cleanup", Kind: store.KindFunction, QualifiedName: "pkg.Cleanup", BodyExcerpt: "func Cleanup() {}"})
	require.NoError(t, err)
	err = s.UpsertEmbedding(ctx, sid, make([]float32, 384))
	require.NoError(t, err)

	err = s.DeleteSymbolsByFile(ctx, fid)
	require.NoError(t, err)

	_, ok, err := s.GetEmbedding(ctx, sid)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestStore_ListFiles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, path := range []string{"/a.go", "/b.go", "/c.go"} {
		_, err := s.SaveFile(ctx, &store.File{Path: path, Language: "go", Hash: "h", Mtime: 0})
		require.NoError(t, err)
	}

	files, err := s.ListFiles(ctx)
	require.NoError(t, err)
	require.Len(t, files, 3)
}

func TestStore_DeleteSymbolsByFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/d.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		_, err := s.SaveSymbol(ctx, &store.Symbol{
			FileID: fid, Name: "X", Kind: "func", QualifiedName: "pkg.X", BodyExcerpt: "x",
		})
		require.NoError(t, err)
	}

	err = s.DeleteSymbolsByFile(ctx, fid)
	require.NoError(t, err)

	var cnt int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbols WHERE file_id=?`, fid).Scan(&cnt)
	require.NoError(t, err)
	require.Zero(t, cnt)
}

func TestStore_SaveEdgeBatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/e.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	var symIDs []int64
	for i := 0; i < 3; i++ {
		id, err := s.SaveSymbol(ctx, &store.Symbol{
			FileID: fid, Name: "S", Kind: "func", QualifiedName: "pkg.S", BodyExcerpt: "x",
		})
		require.NoError(t, err)
		symIDs = append(symIDs, id)
	}

	edges := []store.Edge{
		{Src: symIDs[0], Dst: symIDs[1], Kind: "calls", Weight: 1.0, CallLine: 17},
		{Src: symIDs[1], Dst: symIDs[2], Kind: "calls", Weight: 1.0, CallLine: 29},
	}
	err = s.SaveEdgeBatch(ctx, edges)
	require.NoError(t, err)

	var cnt int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges`).Scan(&cnt)
	require.NoError(t, err)
	require.Equal(t, 2, cnt)
	got, err := s.GetEdges(ctx, symIDs[0])
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 17, got[0].CallLine)
}

func TestStore_ForeignKeysEnabled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO symbols(file_id,name,kind,qualified_name,start_line,end_line,body_excerpt)
		 VALUES(9999,'X','func','X',0,0,'x')`)
	require.Error(t, err, "FK constraint should reject invalid file_id")

	var fkOn int
	err = s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fkOn)
	require.NoError(t, err)
	require.Equal(t, 1, fkOn)
}

func TestStore_SchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s1, err := New(path, "bge-small-en-v1.5", 384)
	require.NoError(t, err)

	var ver string
	err = s1.db.QueryRowContext(context.Background(), `SELECT value FROM _meta WHERE key='schema_version'`).Scan(&ver)
	require.NoError(t, err)
	require.Equal(t, "2", ver)
	s1.Close()

	// Second open should not fail (IF NOT EXISTS + version check)
	s2, err := New(path, "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	s2.Close()
}

func TestStore_ContentVersionRequiresExplicitFullReindex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "content-version.db")
	ctx := context.Background()

	s1, err := New(path, "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	version, err := s1.IndexContentVersion(ctx)
	require.NoError(t, err)
	require.Zero(t, version)
	_, err = s1.SaveFile(ctx, &store.File{Path: "legacy.go", Language: "go", Hash: "h"})
	require.NoError(t, err)
	_, err = s1.db.ExecContext(ctx, `DELETE FROM _meta WHERE key='index_content_version'`)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := New(path, "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	defer s2.Close()
	version, err = s2.IndexContentVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, version)
	require.NoError(t, s2.SetIndexContentVersion(ctx, store.CurrentIndexContentVersion))
	version, err = s2.IndexContentVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, store.CurrentIndexContentVersion, version)
}

// Verify that querying a nonexistent file returns ErrNotFound (not sql.ErrNoRows directly).
func TestStore_GetFileByPath_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetFileByPath(context.Background(), "/nope")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NotErrorIs(t, err, sql.ErrNoRows)
}

func TestStore_FTS_InsertAndSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/fts.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	syms := []store.Symbol{
		{FileID: fid, Name: "WalkFiles", Kind: "function", QualifiedName: "index.Walker.Walk", BodyExcerpt: "func Walk() {}"},
		{FileID: fid, Name: "FooBar", Kind: "function", QualifiedName: "pkg.FooBar", BodyExcerpt: "func FooBar() {}"},
		{FileID: fid, Name: "BazQux", Kind: "function", QualifiedName: "pkg.BazQux", BodyExcerpt: "func BazQux() {}"},
	}
	ids, err := s.SaveSymbolBatch(ctx, syms)
	require.NoError(t, err)
	require.Len(t, ids, 3)

	var ftsCount int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_fts`).Scan(&ftsCount)
	require.NoError(t, err)
	require.Equal(t, 3, ftsCount, "all 3 symbols should be in FTS")

	var rowids []int64
	rows, err := s.db.QueryContext(ctx, `SELECT rowid FROM symbol_fts WHERE symbol_fts MATCH 'Walk'`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var rid int64
		require.NoError(t, rows.Scan(&rid))
		rowids = append(rowids, rid)
	}
	require.NoError(t, rows.Err())
	require.Contains(t, rowids, ids[0], "Walk symbol rowid should be found via FTS")
}

func TestStore_FTS_DeleteCleansUp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/del.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	_, err = s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: fid, Name: "Foo", Kind: "function", QualifiedName: "pkg.Foo", BodyExcerpt: "uniquetoken123"},
		{FileID: fid, Name: "Bar", Kind: "function", QualifiedName: "pkg.Bar", BodyExcerpt: "uniquetoken456"},
	})
	require.NoError(t, err)

	err = s.DeleteSymbolsByFile(ctx, fid)
	require.NoError(t, err)

	var cnt int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_fts WHERE symbol_fts MATCH 'uniquetoken123'`).Scan(&cnt)
	require.NoError(t, err)
	require.Zero(t, cnt, "FTS entries should be deleted when symbols are deleted")
}

func TestStore_SearchByText_Basic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/srch.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	_, err = s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: fid, Name: "RunIndex", Kind: "function", QualifiedName: "index.RunIndex", BodyExcerpt: "func RunIndex() { walk files }"},
		{FileID: fid, Name: "NewStore", Kind: "function", QualifiedName: "sqlite.NewStore", BodyExcerpt: "func NewStore() {}"},
		{FileID: fid, Name: "Unrelated", Kind: "function", QualifiedName: "pkg.Unrelated", BodyExcerpt: "completely different content"},
	})
	require.NoError(t, err)

	results, err := s.SearchByText(ctx, "RunIndex walk files", 5)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, "index.RunIndex", results[0].QualifiedName, "RunIndex should be top result")
	for _, r := range results {
		require.Greater(t, r.Score, float32(0), "scores should be positive (inverted bm25)")
	}
}

func TestStore_SearchByText_EmptyQuery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	results, err := s.SearchByText(ctx, "the is a an", 10)
	require.NoError(t, err)
	require.Empty(t, results, "all-stopword query should return empty")
}

func TestPreprocessFTSQuery_SanitizesSpecialChars(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"slash path", "internal/sql/colexec", `"internal" OR "sql" OR "colexec"`},
		{"parens and star", "Scan(*Batch)", `"Scan" OR "Batch"`},
		{"hyphen is a separator", "tree-sitter", `"tree" OR "sitter"`},
		// a bareword colliding with an FTS5 operator is quoted into a literal,
		// not parsed as AND/OR — otherwise a MATCH syntax error.
		{"fts keyword neutralized", "rows or scan", `"rows" OR "or" OR "scan"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, preprocessFTSQuery(tc.query))
		})
	}
}

func TestStore_SearchByText_SpecialCharsNoSyntaxError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/internal/sql/colexec.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)
	_, err = s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: fid, Name: "ColExec", Kind: "function", QualifiedName: "colexec.ColExec", BodyExcerpt: "func ColExec() { scan batch rows }"},
	})
	require.NoError(t, err)

	// Each of these previously raised an FTS5 MATCH syntax error (slash, parens/star,
	// FTS operator keyword, hyphen-NOT). They must now run cleanly.
	for _, q := range []string{"internal/sql/colexec", "Scan(*Batch)", "rows AND scan", "tree-sitter colexec"} {
		_, err := s.SearchByText(ctx, q, 5)
		require.NoErrorf(t, err, "query %q must not raise an FTS syntax error", q)
	}

	// The slash-path query should also actually match via its tokenized components.
	results, err := s.SearchByText(ctx, "internal/sql/colexec", 5)
	require.NoError(t, err)
	require.NotEmpty(t, results, "slash-path query should match via tokenized components")
}

func TestStore_GetByIDs_BatchesOverSQLVarLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/big.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	// Exceed one IN(...) chunk so the query must be batched, otherwise SQLite
	// raises "too many SQL variables".
	const n = sqlVarLimit + 250
	syms := make([]store.Symbol, n)
	for i := range syms {
		syms[i] = store.Symbol{
			FileID: fid, Name: fmt.Sprintf("F%d", i), Kind: "function",
			QualifiedName: fmt.Sprintf("pkg.F%d", i), BodyExcerpt: "x",
		}
	}
	ids, err := s.SaveSymbolBatch(ctx, syms)
	require.NoError(t, err)
	require.Len(t, ids, n)

	got, err := s.GetSymbolsByIDs(ctx, ids)
	require.NoError(t, err, "GetSymbolsByIDs must batch IN(...) under the SQLite variable limit")
	require.Len(t, got, n, "all symbols returned across chunks")

	files, err := s.GetFilesByIDs(ctx, ids)
	require.NoError(t, err)
	require.Contains(t, files, fid)
}

func TestStore_GetEmbeddingsByIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := &store.File{Path: "/embs.go", Language: "go", Hash: "h", Mtime: 0}
	fid, err := s.SaveFile(ctx, f)
	require.NoError(t, err)

	dim := 384
	makeVec := func(v float32) []float32 {
		vec := make([]float32, dim)
		for i := range vec {
			vec[i] = v + float32(i)*0.001
		}
		return vec
	}

	ids, err := s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: fid, Name: "A", Kind: "func", QualifiedName: "pkg.A", BodyExcerpt: "a"},
		{FileID: fid, Name: "B", Kind: "func", QualifiedName: "pkg.B", BodyExcerpt: "b"},
		{FileID: fid, Name: "C", Kind: "func", QualifiedName: "pkg.C", BodyExcerpt: "c"},
	})
	require.NoError(t, err)

	vecs := [][]float32{makeVec(0.1), makeVec(0.5), makeVec(0.9)}
	for i, id := range ids {
		require.NoError(t, s.UpsertEmbedding(ctx, id, vecs[i]))
	}

	got, err := s.GetEmbeddingsByIDs(ctx, ids)
	require.NoError(t, err)
	require.Len(t, got, 3)

	for i, id := range ids {
		vec, ok := got[id]
		require.True(t, ok, "missing embedding for symbol %d", id)
		require.Len(t, vec, dim)
		require.InDelta(t, vecs[i][0], vec[0], 1e-5, "first element mismatch for symbol %d", id)
		require.InDelta(t, vecs[i][dim-1], vec[dim-1], 1e-5, "last element mismatch for symbol %d", id)
	}

	// empty input
	empty, err := s.GetEmbeddingsByIDs(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	// unknown id returns empty result (no error)
	partial, err := s.GetEmbeddingsByIDs(ctx, []int64{ids[0], 999999})
	require.NoError(t, err)
	require.Len(t, partial, 1)
	require.Contains(t, partial, ids[0])
}

// The head FTS index stops at body_excerpt, so a term living past the cap was
// unreachable by keyword search — the tail of a capped symbol is invisible to
// every channel (measured: ~5% of symbols, ~53% of their code).
func TestStore_SearchByTextReachesPastTheBodyCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fid, err := s.SaveFile(ctx, &store.File{Path: "/deep.go", Language: "go", Hash: "h"})
	require.NoError(t, err)

	full := "func Huge() {\n" + strings.Repeat("filler()\n", 400) + "quorumRecalibration()\n}\n"
	require.Greater(t, len(full), 2000)
	excerpt := full[:2000] + "\n// ... [truncated]"
	require.NotContains(t, excerpt, "quorumRecalibration")

	_, err = s.SaveSymbolBatch(ctx, []store.Symbol{{
		FileID: fid, Name: "Huge", Kind: store.KindFunction, QualifiedName: "pkg.Huge",
		BodyExcerpt: excerpt, FullBody: full,
	}})
	require.NoError(t, err)

	head, err := s.SearchByText(ctx, "quorumRecalibration", 10)
	require.NoError(t, err)
	require.Empty(t, head, "the head index cannot see past the cap")

	deep, err := s.SearchByBodyText(ctx, "quorumRecalibration", 10)
	require.NoError(t, err)
	require.Len(t, deep, 1)
	require.Equal(t, "pkg.Huge", deep[0].QualifiedName)
}

// Reindexing a file drops its symbols; the body index must drop with them or a
// stale rowid keeps answering for code that no longer exists.
func TestStore_DeleteSymbolsByFileClearsBodyIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fid, err := s.SaveFile(ctx, &store.File{Path: "/gone.go", Language: "go", Hash: "h"})
	require.NoError(t, err)

	full := "func Huge() {\n" + strings.Repeat("filler()\n", 400) + "quorumRecalibration()\n}\n"
	_, err = s.SaveSymbolBatch(ctx, []store.Symbol{{
		FileID: fid, Name: "Huge", Kind: store.KindFunction, QualifiedName: "pkg.Huge",
		BodyExcerpt: full[:2000] + "\n// ... [truncated]", FullBody: full,
	}})
	require.NoError(t, err)
	require.NoError(t, s.DeleteSymbolsByFile(ctx, fid))

	hits, err := s.SearchByBodyText(ctx, "quorumRecalibration", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
}

// DeleteFile cleaned symbol_vec by hand but left both FTS indexes pointing at
// cascaded-away symbols, which then matched queries and disappeared at the
// JOIN — invisible in the results, but still spending the result limit.
func TestStore_DeleteFileClearsSearchIndexes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fid, err := s.SaveFile(ctx, &store.File{Path: "/dropped.go", Language: "go", Hash: "h"})
	require.NoError(t, err)

	full := "func Huge() {\n" + strings.Repeat("filler()\n", 400) + "quorumRecalibration()\n}\n"
	_, err = s.SaveSymbolBatch(ctx, []store.Symbol{{
		FileID: fid, Name: "Huge", Kind: store.KindFunction, QualifiedName: "pkg.HugeDropped",
		Docstring: "planStabilization of the write path", BodyExcerpt: full[:2000] + "\n// ... [truncated]", FullBody: full,
	}})
	require.NoError(t, err)
	require.NoError(t, s.DeleteFile(ctx, fid))

	// Asserted against the FTS tables directly: SearchByText joins symbols, and
	// that join hides stale entries instead of reporting them.
	var headTerms, bodyTerms int
	require.NoError(t, s.db.QueryRow(
		`SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, "planStabilization").Scan(&headTerms))
	require.Zero(t, headTerms, "head index still holds terms of a deleted file")
	require.NoError(t, s.db.QueryRow(
		`SELECT count(*) FROM symbol_body_fts WHERE symbol_body_fts MATCH ?`, "quorumRecalibration").Scan(&bodyTerms))
	require.Zero(t, bodyTerms, "body index still holds terms of a deleted file")
}
