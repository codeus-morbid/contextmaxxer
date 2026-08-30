package retrieve

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
)

// The Store interface pins method signatures. It does not pin the behaviour
// this package actually depends on, and the gap has cost us once already: the
// graph memo decides whether its derived maps are still fresh by comparing the
// IDENTITY of the slices the store hands back (graphmemo.go, sameSliceIdentity),
// which only works because the sqlite store serves both from a RAM cache and
// returns the same backing array every call. The mock rebuilt its slice per
// call, so in tests the memo was rediscovered as new on every query, nothing
// was ever shared, and a data race on that shared state passed its own test.
//
// So the contract gets tested against every implementation instead of being
// described in a comment on one of them. A fake that drifts fails here rather
// than quietly disabling the thing it was standing in for.

// storeUnderTest pairs an implementation with a name for subtest output.
type storeUnderTest struct {
	name  string
	store Store
}

func contractImplementations(t *testing.T) []storeUnderTest {
	t.Helper()
	return []storeUnderTest{
		{name: "sqlite", store: contractSQLiteStore(t)},
		{name: "mock", store: contractMockStore()},
	}
}

// contractSQLiteStore builds a real store holding the same tiny graph the mock
// serves: three symbols in one file, two call edges.
func contractSQLiteStore(t *testing.T) Store {
	t.Helper()
	s, err := sqlite.New(filepath.Join(t.TempDir(), "contract.db"), "bge-small-en-v1.5", 384)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	fileID, err := s.SaveFile(ctx, &store.File{Path: "pkg/alpha.go", Language: "go", Hash: "h", Mtime: 1})
	require.NoError(t, err)

	ids, err := s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: fileID, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 10},
		{FileID: fileID, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 11, EndLine: 20},
		{FileID: fileID, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 21, EndLine: 30},
	})
	require.NoError(t, err)
	require.Len(t, ids, 3)

	require.NoError(t, s.SaveEdgeBatch(ctx, []store.Edge{
		{Src: ids[0], Dst: ids[1], Kind: store.EdgeCalls, Weight: 1, CallLine: 3},
		{Src: ids[1], Dst: ids[2], Kind: store.EdgeCalls, Weight: 1, CallLine: 13},
	}))
	return s
}

func contractMockStore() Store {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 10},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 11, EndLine: 20},
		{ID: 3, FileID: 10, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 21, EndLine: 30},
	}
	return &mockStore{
		symbols: symbols,
		edges: []store.Edge{
			{Src: 1, Dst: 2, Kind: store.EdgeCalls, Weight: 1, CallLine: 3},
			{Src: 2, Dst: 3, Kind: store.EdgeCalls, Weight: 1, CallLine: 13},
		},
		ids:       []int64{1, 2, 3},
		filePaths: map[int64]string{10: "pkg/alpha.go"},
		metaCache: symbols,
	}
}

func TestStoreContract_SymbolMetaIsStableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	for _, impl := range contractImplementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			first, err := impl.store.ListSymbolMeta(ctx)
			require.NoError(t, err)
			require.NotEmpty(t, first, "the contract is untestable on an empty result")

			second, err := impl.store.ListSymbolMeta(ctx)
			require.NoError(t, err)
			require.Len(t, second, len(first))

			require.Same(t, &first[0], &second[0],
				"ListSymbolMeta must serve one backing array; the graph memo treats a "+
					"new one as a new index generation and rebuilds every derived map")
		})
	}
}

func TestStoreContract_EdgeListIsStableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	for _, impl := range contractImplementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			first, err := impl.store.ListAllEdges(ctx)
			require.NoError(t, err)
			require.NotEmpty(t, first, "the contract is untestable on an empty result")

			second, err := impl.store.ListAllEdges(ctx)
			require.NoError(t, err)
			require.Len(t, second, len(first))

			require.Same(t, &first[0], &second[0],
				"ListAllEdges must serve one backing array, for the same reason as "+
					"ListSymbolMeta")
		})
	}
}

func TestStoreContract_GraphSlicesAreSharedNotCopied(t *testing.T) {
	// The flip side of stability, and the reason enrichGraphContext must never
	// sort what it is handed: these slices are shared state, so a caller that
	// writes to one writes into the store's cache and into every later query.
	// This test documents that hazard rather than guarding against it — the
	// guard is that callers copy first (see rankRefs).
	ctx := context.Background()
	for _, impl := range contractImplementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			edges, err := impl.store.ListAllEdges(ctx)
			require.NoError(t, err)
			require.NotEmpty(t, edges)

			original := edges[0]
			edges[0] = store.Edge{Src: -1, Dst: -1, Kind: store.EdgeCalls}
			after, err := impl.store.ListAllEdges(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(-1), after[0].Src,
				"a write through the returned slice must be visible to the next "+
					"caller — if it is not, this implementation copies and the "+
					"stability test above is passing for the wrong reason")

			edges[0] = original // leave the fixture as found
		})
	}
}
