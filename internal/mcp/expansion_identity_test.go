package mcp

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
	"github.com/stretchr/testify/require"
)

// expand_context resolves a cached rank to a symbol id and reads the body
// behind it. symbols.id is the SQLite rowid, so a reindex between the search
// and the expansion can put a different symbol on that id. The first expansion
// here is the control: the same call must work before the index changes.
func TestExpansionRefusesWhenCachedIDNamesAnotherSymbol(t *testing.T) {
	dir := t.TempDir()
	st, err := sqlite.New(filepath.Join(dir, "index.db"), "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	defer st.Close()

	ctx := context.Background()
	fileID, err := st.SaveFile(ctx, &store.File{Path: "demo.go", Language: "go", Hash: "h1"})
	require.NoError(t, err)
	ids, err := st.SaveSymbolBatch(ctx, []store.Symbol{{
		FileID: fileID, Name: "Old", QualifiedName: "demo.Old", Kind: "function",
		StartLine: 2, EndLine: 3, BodyExcerpt: "func Old() {}",
	}})
	require.NoError(t, err)
	symbolID := ids[0]

	srv := NewServer(retrieve.NewRetriever(st, nil, slog.Default()), slog.Default())
	srv.cacheExpansion("req-1", []retrieve.ScoredResult{{
		SymbolID: symbolID, File: "demo.go", QualifiedName: "demo.Old",
		Kind: "function", StartLine: 2, EndLine: 3,
	}})

	page, err := srv.startExpansion(ctx, "req-1", 1)
	require.NoError(t, err, "control: the cached rank resolves while the index is unchanged")
	require.Contains(t, page.Body, "func Old() {}")

	// Reindex the file: the old symbol goes, a new one takes the freed rowid.
	require.NoError(t, st.DeleteSymbolsByFile(ctx, fileID))
	reused, err := st.SaveSymbolBatch(ctx, []store.Symbol{{
		FileID: fileID, Name: "New", QualifiedName: "demo.New", Kind: "function",
		StartLine: 2, EndLine: 3, BodyExcerpt: "func New() {}",
	}})
	require.NoError(t, err)
	require.Equal(t, symbolID, reused[0], "precondition: SQLite reused the id")

	_, err = srv.startExpansion(ctx, "req-1", 1)
	require.Error(t, err, "the cached rank no longer names the symbol it was taken from")
	require.Contains(t, err.Error(), "stale")
	require.Contains(t, err.Error(), "demo.Old")
}
