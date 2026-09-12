package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/require"
)

// symbols.id is INTEGER PRIMARY KEY, which is the rowid: SQLite hands the same
// id to a later row once the highest one is deleted. Anything that caches a
// symbol id across a reindex — expand_context does, for the life of the MCP
// process — is caching a pointer that can come to rest on a different symbol,
// which is why the server checks identity before serving a cached rank
// (TestExpansionRefusesWhenCachedIDNamesAnotherSymbol).
//
// This test states the premise rather than a requirement: if ids ever stop
// being reused, that check can be reconsidered, and this failing is how anyone
// would find out.
func TestSymbolIDIsReusedAfterDelete(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "reuse.db"), "bge-small-en-v1.5", 384)
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
	require.Len(t, ids, 1)
	oldID := ids[0]

	require.NoError(t, st.DeleteSymbolsByFile(ctx, fileID))

	ids, err = st.SaveSymbolBatch(ctx, []store.Symbol{{
		FileID: fileID, Name: "New", QualifiedName: "demo.New", Kind: "function",
		StartLine: 2, EndLine: 3, BodyExcerpt: "func New() {}",
	}})
	require.NoError(t, err)
	require.Len(t, ids, 1)

	require.Equal(t, oldID, ids[0],
		"the freed rowid is handed to the next symbol written")
}
