package index_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/index/languages"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
	"github.com/stretchr/testify/require"
)

// A caller whose own bytes did not change still loses its edge when the callee
// is edited: the callee's symbols are deleted, ON DELETE CASCADE takes the
// incoming edges with them, and the caller is skipped by hash so nothing
// re-resolves it.
//
// TestIndexer_ExtractsEdgesToUnchangedFileSymbols covers this same shape
// against mockStore, whose DeleteSymbolsByFile is a no-op — the mock cannot
// model the cascade, so the case looked covered.
func TestIndexerKeepsEdgeFromUnchangedCallerAfterCalleeEdit(t *testing.T) {
	dir := t.TempDir()
	caller := filepath.Join(dir, "a_caller.go")
	callee := filepath.Join(dir, "z_callee.go")
	require.NoError(t, os.WriteFile(caller, []byte("package demo\n\nfunc Caller() int {\n\treturn Callee()\n}\n"), 0o644))
	require.NoError(t, os.WriteFile(callee, []byte("package demo\n\nfunc Callee() int {\n\treturn 1\n}\n"), 0o644))

	st, err := sqlite.New(filepath.Join(dir, "index.db"), "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	defer st.Close()

	parser, err := index.NewTreeSitterParser()
	require.NoError(t, err)
	defer parser.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	idx := index.NewIndexer(st, parser, nil, index.NewWalker(log), languages.New, log)

	ctx := context.Background()
	first, err := idx.Index(ctx, dir)
	require.NoError(t, err)
	require.Equal(t, 1, first.Edges, "the first pass resolves Caller -> Callee")

	edges, err := st.ListAllEdges(ctx)
	require.NoError(t, err)
	require.Len(t, edges, 1)

	// Only the callee's body changes. Its name and the call site are untouched.
	require.NoError(t, os.WriteFile(callee, []byte("package demo\n\nfunc Callee() int {\n\treturn 12345\n}\n"), 0o644))

	second, err := idx.Index(ctx, dir)
	require.NoError(t, err)
	require.Equal(t, 1, second.FilesIndexed, "only the callee is re-read")
	require.Equal(t, 1, second.FilesSkipped, "the caller is skipped by hash")

	edges, err = st.ListAllEdges(ctx)
	require.NoError(t, err)
	require.Len(t, edges, 1,
		"editing a callee must not drop the call graph edge pointing at it")
}
