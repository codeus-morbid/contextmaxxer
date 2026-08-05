package index

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type fakeBackfillStore struct {
	missing  []store.SymbolToEmbed
	upserted []store.Embedding
	failOn   int // fail the upsert containing this SymbolID (0 = never)
}

func (f *fakeBackfillStore) ListSymbolsMissingEmbedding(context.Context) ([]store.SymbolToEmbed, error) {
	return f.missing, nil
}

func (f *fakeBackfillStore) UpsertEmbeddingBatch(_ context.Context, embeddings []store.Embedding) error {
	for _, e := range embeddings {
		if f.failOn != 0 && e.SymbolID == int64(f.failOn) {
			return fmt.Errorf("boom on %d", e.SymbolID)
		}
	}
	f.upserted = append(f.upserted, embeddings...)
	return nil
}

type fakeEmbedder struct{ dim int }

func (f *fakeEmbedder) Dimension() int { return f.dim }
func (f *fakeEmbedder) Close() error   { return nil }
func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, f.dim)
		out[i][0] = float32(len(texts[i])) // deterministic, text-dependent
	}
	return out, nil
}

func missingSyms(n int) []store.SymbolToEmbed {
	out := make([]store.SymbolToEmbed, n)
	for i := range out {
		out[i] = store.SymbolToEmbed{
			Symbol:   store.Symbol{ID: int64(i + 1), Name: fmt.Sprintf("f%d", i+1), Kind: "function", QualifiedName: fmt.Sprintf("pkg.f%d", i+1)},
			Path:     "pkg/file.go",
			Language: "go",
		}
	}
	return out
}

func TestBackfillEmbeddings_EmbedsAllMissing(t *testing.T) {
	st := &fakeBackfillStore{missing: missingSyms(backfillChunk + 3)} // spans 2 chunks
	n, err := BackfillEmbeddings(context.Background(), st, &fakeEmbedder{dim: 4}, slog.Default())
	require.NoError(t, err)
	require.Equal(t, backfillChunk+3, n)
	require.Len(t, st.upserted, backfillChunk+3)

	seen := map[int64]bool{}
	for _, e := range st.upserted {
		require.Len(t, e.Vector, 4)
		seen[e.SymbolID] = true
	}
	for i := 1; i <= backfillChunk+3; i++ {
		require.True(t, seen[int64(i)], "symbol %d not embedded", i)
	}
}

func TestBackfillEmbeddings_NothingMissingIsNoop(t *testing.T) {
	st := &fakeBackfillStore{}
	n, err := BackfillEmbeddings(context.Background(), st, &fakeEmbedder{dim: 4}, slog.Default())
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, st.upserted)
}

func TestBackfillEmbeddings_PartialFailureReportsDone(t *testing.T) {
	// First chunk (1..128) succeeds, second chunk contains the failing id:
	// done must reflect only the committed chunk so a retry resumes there.
	st := &fakeBackfillStore{missing: missingSyms(backfillChunk + 10), failOn: backfillChunk + 5}
	n, err := BackfillEmbeddings(context.Background(), st, &fakeEmbedder{dim: 4}, slog.Default())
	require.Error(t, err)
	require.Equal(t, backfillChunk, n)
	require.Len(t, st.upserted, backfillChunk)
}
